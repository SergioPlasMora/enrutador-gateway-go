package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// ConnectorWSServer handles WebSocket connections from Data Connectors (reverse tunnel mode)
type ConnectorWSServer struct {
	registry *ConnectorRegistry
	upgrader websocket.Upgrader
}

// NewConnectorWSServer creates a new WebSocket server for connector connections
func NewConnectorWSServer(registry *ConnectorRegistry) *ConnectorWSServer {
	return &ConnectorWSServer{
		registry: registry,
		upgrader: websocket.Upgrader{
			ReadBufferSize:  1024 * 64,
			WriteBufferSize: 1024 * 1024,
			CheckOrigin: func(r *http.Request) bool {
				return true
			},
		},
	}
}

// ConnectorMessage represents messages between Gateway and Connector
type ConnectorMessage struct {
	Action    string                 `json:"action"`
	RequestID string                 `json:"request_id,omitempty"`
	TenantID  string                 `json:"tenant_id,omitempty"`
	Status    string                 `json:"status,omitempty"`
	Error     string                 `json:"error,omitempty"`
	Data      map[string]interface{} `json:"data,omitempty"`

	// For registration
	Version  string   `json:"version,omitempty"`
	Datasets []string `json:"datasets,omitempty"`

	// For FlightInfo response
	Descriptor map[string]interface{} `json:"descriptor,omitempty"`
	Ticket     string                 `json:"ticket,omitempty"`

	// For stream_end
	Type       string `json:"type,omitempty"`
	Partition  int    `json:"partition,omitempty"`
	TotalBytes int64  `json:"total_bytes,omitempty"`

	// For heartbeat
	Timestamp int64 `json:"timestamp,omitempty"`
}

// WSConnectorClient wraps a WebSocket connection to act as a connector client
type WSConnectorClient struct {
	ws        *websocket.Conn
	writeMu   sync.Mutex
	tenantID  string
	sessionID string
	pending   map[string]chan *ConnectorMessage // request_id -> response channel
	pendingMu sync.RWMutex
	chunks    map[string]chan []byte // request_id -> binary chunks channel
	chunksMu  sync.RWMutex
}

// NewWSConnectorClient creates a new WebSocket-based connector client
func NewWSConnectorClient(ws *websocket.Conn, tenantID, sessionID string) *WSConnectorClient {
	return &WSConnectorClient{
		ws:        ws,
		tenantID:  tenantID,
		sessionID: sessionID,
		pending:   make(map[string]chan *ConnectorMessage),
		chunks:    make(map[string]chan []byte),
	}
}

// HandleConnection handles incoming WebSocket connections from connectors
func (s *ConnectorWSServer) HandleConnection(w http.ResponseWriter, r *http.Request) {
	ws, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("[ConnectorWS] Upgrade error: %v", err)
		return
	}
	defer ws.Close()

	log.Printf("[ConnectorWS] New connection from %s", r.RemoteAddr)

	// Wait for registration message
	_, data, err := ws.ReadMessage()
	if err != nil {
		log.Printf("[ConnectorWS] Read error: %v", err)
		return
	}

	var regMsg ConnectorMessage
	if err := json.Unmarshal(data, &regMsg); err != nil {
		log.Printf("[ConnectorWS] Invalid registration message: %v", err)
		return
	}

	if regMsg.Action != "register" || regMsg.TenantID == "" {
		log.Printf("[ConnectorWS] Invalid registration: action=%s, tenant=%s", regMsg.Action, regMsg.TenantID)
		sendJSON(ws, ConnectorMessage{Status: "error", Error: "invalid registration"})
		return
	}

	tenantID := regMsg.TenantID
	sessionID := generateSessionID()

	// Create client wrapper
	client := NewWSConnectorClient(ws, tenantID, sessionID)

	// Register with the registry
	s.registry.RegisterWSConnector(tenantID, client)
	defer s.registry.UnregisterWSConnector(tenantID)

	// Send confirmation
	sendJSON(ws, ConnectorMessage{
		Status:   "ok",
		Action:   "registered",
		TenantID: tenantID,
		Data:     map[string]interface{}{"session_id": sessionID},
	})

	log.Printf("[ConnectorWS] Registered: tenant=%s session=%s version=%s datasets=%v",
		tenantID, sessionID, regMsg.Version, regMsg.Datasets)

	// Start keepalive goroutine
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go s.keepalive(ctx, ws, tenantID)

	// Main message loop - handle responses from connector
	for {
		messageType, data, err := ws.ReadMessage()
		if err != nil {
			log.Printf("[ConnectorWS] Connection closed: tenant=%s err=%v", tenantID, err)
			return
		}

		if messageType == websocket.BinaryMessage {
			// Binary Arrow IPC data - route to appropriate request channel
			client.handleBinaryChunk(data)
		} else {
			// JSON message
			var msg ConnectorMessage
			if err := json.Unmarshal(data, &msg); err != nil {
				log.Printf("[ConnectorWS] Invalid message: %v", err)
				continue
			}

			client.handleResponse(&msg)
		}
	}
}

// handleBinaryChunk routes binary data to the appropriate request channel
func (c *WSConnectorClient) handleBinaryChunk(data []byte) {
	c.chunksMu.RLock()
	defer c.chunksMu.RUnlock()

	// Binary chunks come with request_id in the first 36 bytes (UUID format)
	// But our protocol sends them without prefix, so we route to the single active request
	// For now, broadcast to all pending chunk channels
	for _, ch := range c.chunks {
		select {
		case ch <- data:
		default:
			// Channel full, skip
		}
	}
}

// handleResponse routes JSON responses to waiting callers
func (c *WSConnectorClient) handleResponse(msg *ConnectorMessage) {
	if msg.RequestID == "" {
		if msg.Action == "heartbeat" {
			// Heartbeat response, ignore
			return
		}
		log.Printf("[ConnectorWS] Response without request_id: %+v", msg)
		return
	}

	// Check if this is a stream_end message
	if msg.Type == "stream_end" {
		c.chunksMu.Lock()
		if ch, exists := c.chunks[msg.RequestID]; exists {
			close(ch)
			delete(c.chunks, msg.RequestID)
		}
		c.chunksMu.Unlock()
	}

	// Route to pending request
	c.pendingMu.RLock()
	ch, exists := c.pending[msg.RequestID]
	c.pendingMu.RUnlock()

	if exists {
		select {
		case ch <- msg:
		default:
		}
	}
}

// SendCommand sends a command to the connector and waits for response
func (c *WSConnectorClient) SendCommand(ctx context.Context, msg *ConnectorMessage) (*ConnectorMessage, error) {
	// Create response channel
	respCh := make(chan *ConnectorMessage, 1)
	c.pendingMu.Lock()
	c.pending[msg.RequestID] = respCh
	c.pendingMu.Unlock()

	defer func() {
		c.pendingMu.Lock()
		delete(c.pending, msg.RequestID)
		c.pendingMu.Unlock()
	}()

	// Send message
	c.writeMu.Lock()
	err := c.ws.WriteJSON(msg)
	c.writeMu.Unlock()

	if err != nil {
		return nil, err
	}

	// Wait for response with timeout
	select {
	case resp := <-respCh:
		return resp, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// StreamData streams data from the connector for a given request
func (c *WSConnectorClient) StreamData(ctx context.Context, requestID string, chunks chan []byte) error {
	// Register chunks channel
	c.chunksMu.Lock()
	c.chunks[requestID] = chunks
	c.chunksMu.Unlock()

	// Wait for stream to complete (channel closed by handleResponse when stream_end received)
	<-ctx.Done()

	// Cleanup
	c.chunksMu.Lock()
	if ch, exists := c.chunks[requestID]; exists {
		close(ch)
		delete(c.chunks, requestID)
	}
	c.chunksMu.Unlock()

	return ctx.Err()
}

// keepalive sends periodic heartbeats
func (s *ConnectorWSServer) keepalive(ctx context.Context, ws *websocket.Conn, tenantID string) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			msg := ConnectorMessage{
				Action:    "heartbeat",
				Timestamp: time.Now().Unix(),
			}
			if err := sendJSON(ws, msg); err != nil {
				log.Printf("[ConnectorWS] Heartbeat failed for %s: %v", tenantID, err)
				return
			}
		}
	}
}

// ConnectorMessage with timestamp for heartbeat
type heartbeatMessage struct {
	Action    string `json:"action"`
	Timestamp int64  `json:"timestamp"`
}

// Helper to add timestamp field
func (m *ConnectorMessage) MarshalJSON() ([]byte, error) {
	type Alias ConnectorMessage
	return json.Marshal(&struct {
		*Alias
		Timestamp int64 `json:"timestamp,omitempty"`
	}{
		Alias:     (*Alias)(m),
		Timestamp: time.Now().Unix(),
	})
}

func sendJSON(ws *websocket.Conn, msg interface{}) error {
	return ws.WriteJSON(msg)
}

func generateSessionID() string {
	return base64.RawURLEncoding.EncodeToString([]byte(time.Now().Format("20060102150405")))
}
