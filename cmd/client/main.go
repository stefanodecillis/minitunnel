package main

import (
	"bytes"
	"encoding/json"
	"io"
	"log"
	"math"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/gorilla/websocket"
	"github.com/stefano/minitunnel/internal/protocol"
)

func main() {
	serverURL := os.Getenv("SERVER_URL")
	authToken := os.Getenv("AUTH_TOKEN")
	serviceName := os.Getenv("SERVICE_NAME")
	localTarget := os.Getenv("LOCAL_TARGET")

	if serverURL == "" || authToken == "" || serviceName == "" || localTarget == "" {
		log.Fatal("SERVER_URL, AUTH_TOKEN, SERVICE_NAME, and LOCAL_TARGET are required")
	}

	// Ensure localTarget has scheme
	if !strings.HasPrefix(localTarget, "http") {
		localTarget = "http://" + localTarget
	}

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)

	done := make(chan struct{})
	go func() {
		defer close(done)
		runClient(serverURL, authToken, serviceName, localTarget, stop)
	}()

	<-done
	log.Println("client shut down")
}

func runClient(serverURL, authToken, serviceName, localTarget string, stop chan os.Signal) {
	attempt := 0
	for {
		err := connect(serverURL, authToken, serviceName, localTarget, stop)
		if err == nil {
			return // clean shutdown
		}

		attempt++
		delay := time.Duration(math.Min(float64(time.Second)*math.Pow(2, float64(attempt)), float64(30*time.Second)))
		log.Printf("connection lost (%v), reconnecting in %s...", err, delay)

		select {
		case <-stop:
			return
		case <-time.After(delay):
		}
	}
}

func connect(serverURL, authToken, serviceName, localTarget string, stop chan os.Signal) error {
	u, err := url.Parse(serverURL)
	if err != nil {
		return err
	}
	// Convert http(s) to ws(s)
	switch u.Scheme {
	case "https":
		u.Scheme = "wss"
	default:
		u.Scheme = "ws"
	}
	u.Path = "/ws"
	q := u.Query()
	q.Set("service", serviceName)
	q.Set("token", authToken)
	u.RawQuery = q.Encode()

	log.Printf("connecting to %s as service '%s'", serverURL, serviceName)

	conn, _, err := websocket.DefaultDialer.Dial(u.String(), nil)
	if err != nil {
		return err
	}
	defer conn.Close()

	log.Printf("connected, forwarding to %s", localTarget)

	// Handle shutdown signal
	go func() {
		<-stop
		conn.WriteMessage(websocket.CloseMessage,
			websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""))
		conn.Close()
	}()

	client := &http.Client{Timeout: 30 * time.Second}

	for {
		_, msg, err := conn.ReadMessage()
		if err != nil {
			// Check if this was a clean shutdown
			if websocket.IsCloseError(err, websocket.CloseNormalClosure) {
				return nil
			}
			return err
		}

		var req protocol.Request
		if err := json.Unmarshal(msg, &req); err != nil {
			log.Printf("unmarshal error: %v", err)
			continue
		}

		go handleRequest(conn, client, localTarget, req)
	}
}

func handleRequest(conn *websocket.Conn, client *http.Client, localTarget string, req protocol.Request) {
	targetURL := localTarget + req.Path

	httpReq, err := http.NewRequest(req.Method, targetURL, bytes.NewBufferString(req.Body))
	if err != nil {
		sendError(conn, req.ID, http.StatusBadGateway, "failed to create request: "+err.Error())
		return
	}

	for k, v := range req.Headers {
		httpReq.Header.Set(k, v)
	}

	resp, err := client.Do(httpReq)
	if err != nil {
		sendError(conn, req.ID, http.StatusBadGateway, "local request failed: "+err.Error())
		return
	}
	defer resp.Body.Close()

	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		sendError(conn, req.ID, http.StatusBadGateway, "failed to read response: "+err.Error())
		return
	}

	headers := make(map[string]string)
	for k, v := range resp.Header {
		headers[k] = strings.Join(v, ", ")
	}

	tunnelResp := protocol.Response{
		ID:      req.ID,
		Status:  resp.StatusCode,
		Headers: headers,
		Body:    string(bodyBytes),
	}

	data, _ := json.Marshal(tunnelResp)
	writeConn(conn, data)
}

var connMu sync.Mutex

func writeConn(conn *websocket.Conn, data []byte) {
	connMu.Lock()
	defer connMu.Unlock()
	if err := conn.WriteMessage(websocket.TextMessage, data); err != nil {
		log.Printf("write error: %v", err)
	}
}

func sendError(conn *websocket.Conn, id string, status int, msg string) {
	resp := protocol.Response{
		ID:     id,
		Status: status,
		Body:   msg,
	}
	data, _ := json.Marshal(resp)
	writeConn(conn, data)
}
