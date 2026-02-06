package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/gorilla/websocket"
	"github.com/stefano/minitunnel/internal/protocol"
	"gopkg.in/yaml.v3"
)

// Config holds the configuration from ~/.minitunnel.yaml
type Config struct {
	Server string `yaml:"server"`
	Token  string `yaml:"token"`
}

func loadConfig() (*Config, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}

	configPath := filepath.Join(home, ".minitunnel.yaml")
	data, err := os.ReadFile(configPath)
	if err != nil {
		if os.IsNotExist(err) {
			return &Config{}, nil // No config file, return empty config
		}
		return nil, err
	}

	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("failed to parse config: %w", err)
	}

	return &cfg, nil
}

func usage() {
	fmt.Fprintf(os.Stderr, `minitunnel - expose local services through a tunnel server

Usage:
  minitunnel [protocol] <port> [flags]
  minitunnel http 8000 --name myservice
  minitunnel 8000 --name myservice

Flags:
`)
	flag.PrintDefaults()
	fmt.Fprintf(os.Stderr, `
Environment Variables (fallback):
  SERVER_URL    - Tunnel server URL
  AUTH_TOKEN    - Authentication token
  SERVICE_NAME  - Service name for routing
  LOCAL_TARGET  - Local target (e.g., localhost:8000)

Config File (~/.minitunnel.yaml):
  server: http://example.com:8888
  token: your-token

Priority: CLI flags > Environment variables > Config file
`)
}

func main() {
	// Define flags
	var (
		name   = flag.String("name", "", "Service name for routing (required)")
		server = flag.String("server", "", "Tunnel server URL")
		token  = flag.String("token", "", "Authentication token")
		host   = flag.String("host", "localhost", "Local host to forward to")
	)

	flag.Usage = usage

	// Extract positional args (before parsing flags) to allow flags anywhere
	// e.g., "http 8000 --name mmg" or "--name mmg http 8000"
	positional, flagArgs := separateArgs(os.Args[1:])

	// Parse only the flag arguments
	if err := flag.CommandLine.Parse(flagArgs); err != nil {
		os.Exit(1)
	}

	// Load config file
	cfg, err := loadConfig()
	if err != nil {
		log.Printf("Warning: failed to load config: %v", err)
		cfg = &Config{}
	}

	// Parse positional arguments: [protocol] <port>
	var protocol, port string

	switch len(positional) {
	case 0:
		// No positional args - check for LOCAL_TARGET env var (legacy mode)
		if os.Getenv("LOCAL_TARGET") == "" {
			fmt.Fprintln(os.Stderr, "Error: port is required")
			usage()
			os.Exit(1)
		}
	case 1:
		// Just port: "8000"
		protocol = "http"
		port = positional[0]
	case 2:
		// Protocol and port: "http 8000"
		protocol = positional[0]
		port = positional[1]
	default:
		fmt.Fprintln(os.Stderr, "Error: too many arguments")
		usage()
		os.Exit(1)
	}

	// Build local target from positional args or env var
	var localTarget string
	if port != "" {
		localTarget = fmt.Sprintf("%s://%s:%s", protocol, *host, port)
	}

	// Resolve configuration with priority: CLI flags > env vars > config file
	serverURL := resolve(*server, os.Getenv("SERVER_URL"), cfg.Server)
	authToken := resolve(*token, os.Getenv("AUTH_TOKEN"), cfg.Token)
	serviceName := resolve(*name, os.Getenv("SERVICE_NAME"), "")
	if localTarget == "" {
		localTarget = os.Getenv("LOCAL_TARGET")
	}

	// Validate required fields
	var missing []string
	if serverURL == "" {
		missing = append(missing, "server")
	}
	if authToken == "" {
		missing = append(missing, "token")
	}
	if serviceName == "" {
		missing = append(missing, "name")
	}
	if localTarget == "" {
		missing = append(missing, "port/target")
	}

	if len(missing) > 0 {
		fmt.Fprintf(os.Stderr, "Error: missing required configuration: %s\n", strings.Join(missing, ", "))
		usage()
		os.Exit(1)
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

// separateArgs separates positional arguments from flag arguments
// This allows flags to appear anywhere (e.g., "http 8000 --name mmg")
func separateArgs(args []string) (positional, flags []string) {
	knownFlags := map[string]bool{
		"-name": true, "--name": true,
		"-server": true, "--server": true,
		"-token": true, "--token": true,
		"-host": true, "--host": true,
		"-h": true, "--help": true, "-help": true,
	}

	i := 0
	for i < len(args) {
		arg := args[i]
		if strings.HasPrefix(arg, "-") {
			// It's a flag
			flags = append(flags, arg)
			// Check if this flag takes a value (not a boolean)
			if arg == "-h" || arg == "--help" || arg == "-help" {
				i++
				continue
			}
			// Check if value is in same arg (e.g., --name=value)
			if strings.Contains(arg, "=") {
				i++
				continue
			}
			// Check if it's a known flag that takes a value
			base := strings.TrimPrefix(strings.TrimPrefix(arg, "-"), "-")
			if knownFlags["-"+base] && i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
				i++
				flags = append(flags, args[i])
			}
		} else {
			// It's a positional argument
			positional = append(positional, arg)
		}
		i++
	}
	return
}

// resolve returns the first non-empty value (priority order)
func resolve(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
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
