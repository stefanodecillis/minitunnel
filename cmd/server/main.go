package main

import (
	"encoding/json"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/stefano/minitunnel/internal/protocol"
)

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true },
}

type tunnel struct {
	conn     *websocket.Conn
	mu       sync.Mutex
	pending  map[string]chan protocol.Response
	pMu     sync.Mutex
}

func (t *tunnel) send(req protocol.Request) (protocol.Response, error) {
	ch := make(chan protocol.Response, 1)
	t.pMu.Lock()
	t.pending[req.ID] = ch
	t.pMu.Unlock()

	t.mu.Lock()
	err := t.conn.WriteJSON(req)
	t.mu.Unlock()
	if err != nil {
		t.pMu.Lock()
		delete(t.pending, req.ID)
		t.pMu.Unlock()
		return protocol.Response{}, err
	}

	select {
	case resp := <-ch:
		return resp, nil
	case <-time.After(30 * time.Second):
		t.pMu.Lock()
		delete(t.pending, req.ID)
		t.pMu.Unlock()
		return protocol.Response{}, http.ErrHandlerTimeout
	}
}

type registry struct {
	mu      sync.RWMutex
	tunnels map[string]*tunnel
}

func (r *registry) register(name string, t *tunnel) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.tunnels[name] = t
}

func (r *registry) unregister(name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.tunnels, name)
}

func (r *registry) get(name string) *tunnel {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.tunnels[name]
}

func main() {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	authToken := os.Getenv("AUTH_TOKEN")
	if authToken == "" {
		log.Fatal("AUTH_TOKEN environment variable is required")
	}

	reg := &registry{tunnels: make(map[string]*tunnel)}

	mux := http.NewServeMux()

	mux.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		token := r.URL.Query().Get("token")
		if token != authToken {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		service := r.URL.Query().Get("service")
		if service == "" {
			http.Error(w, "service query param required", http.StatusBadRequest)
			return
		}

		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			log.Printf("websocket upgrade error: %v", err)
			return
		}

		t := &tunnel{
			conn:    conn,
			pending: make(map[string]chan protocol.Response),
		}
		reg.register(service, t)
		log.Printf("tunnel registered: %s", service)

		defer func() {
			reg.unregister(service)
			conn.Close()
			log.Printf("tunnel unregistered: %s", service)
		}()

		// Read responses from client
		for {
			_, msg, err := conn.ReadMessage()
			if err != nil {
				log.Printf("tunnel %s read error: %v", service, err)
				return
			}
			var resp protocol.Response
			if err := json.Unmarshal(msg, &resp); err != nil {
				log.Printf("tunnel %s unmarshal error: %v", service, err)
				continue
			}
			t.pMu.Lock()
			ch, ok := t.pending[resp.ID]
			if ok {
				delete(t.pending, resp.ID)
			}
			t.pMu.Unlock()
			if ok {
				ch <- resp
			}
		}
	})

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		// Parse /<service>/rest/of/path
		path := strings.TrimPrefix(r.URL.Path, "/")
		parts := strings.SplitN(path, "/", 2)
		if len(parts) == 0 || parts[0] == "" {
			http.Error(w, "no service specified in path", http.StatusBadRequest)
			return
		}
		service := parts[0]
		remainder := "/"
		if len(parts) == 2 {
			remainder = "/" + parts[1]
		}

		t := reg.get(service)
		if t == nil {
			http.Error(w, "service not found: "+service, http.StatusBadGateway)
			return
		}

		bodyBytes, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "failed to read body", http.StatusInternalServerError)
			return
		}

		headers := make(map[string]string)
		for k, v := range r.Header {
			headers[k] = strings.Join(v, ", ")
		}

		// Preserve query string
		forwardPath := remainder
		if r.URL.RawQuery != "" {
			forwardPath += "?" + r.URL.RawQuery
		}

		req := protocol.Request{
			ID:      uuid.New().String(),
			Method:  r.Method,
			Path:    forwardPath,
			Headers: headers,
			Body:    string(bodyBytes),
		}

		resp, err := t.send(req)
		if err != nil {
			http.Error(w, "tunnel error: "+err.Error(), http.StatusBadGateway)
			return
		}

		for k, v := range resp.Headers {
			w.Header().Set(k, v)
		}
		w.WriteHeader(resp.Status)
		w.Write([]byte(resp.Body))
	})

	log.Printf("minitunnel server listening on :%s", port)
	if err := http.ListenAndServe(":"+port, mux); err != nil {
		log.Fatal(err)
	}
}
