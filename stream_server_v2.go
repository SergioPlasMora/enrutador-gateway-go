package main

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// StreamServerV2 handles WebSocket connections validated via Control Plane
type StreamServerV2 struct {
	registry       *ConnectorRegistry
	sessionManager *SessionManager
	upgrader       websocket.Upgrader
}

// NewStreamServerV2 creates a new v2 stream server with Control Plane validation
func NewStreamServerV2(registry *ConnectorRegistry, sessionManager *SessionManager, enableCompression bool) *StreamServerV2 {
	return &StreamServerV2{
		registry:       registry,
		sessionManager: sessionManager,
		upgrader: websocket.Upgrader{
			ReadBufferSize:    1024 * 64,
			WriteBufferSize:   1024 * 1024,
			EnableCompression: enableCompression, // configurable permessage-deflate
			CheckOrigin: func(r *http.Request) bool {
				return true
			},
		},
	}
}

// StreamResponseV2 represents responses to the client
type StreamResponseV2 struct {
	Status      string `json:"status"`
	Message     string `json:"message,omitempty"`
	Error       string `json:"error,omitempty"`
	TotalBytes  int64  `json:"total_bytes,omitempty"`
	Chunks      int    `json:"chunks,omitempty"`
	UserID      string `json:"user_id,omitempty"`
	CuentaID    string `json:"cuenta_id,omitempty"`
	SessionID   string `json:"session_id,omitempty"`
	Compression string `json:"compression,omitempty"` // 'zstd' o 'none' - indica al browser cómo descomprimir
}

// HandleStream handles WebSocket connections at /stream/{session_id}
// URL format: /stream/{session_id}
func (s *StreamServerV2) HandleStream(w http.ResponseWriter, r *http.Request) {
	// 1. Extract session_id from URL path
	path := strings.TrimPrefix(r.URL.Path, "/stream/")
	sessionID := strings.Split(path, "/")[0]

	if sessionID == "" {
		http.Error(w, `{"error":"missing session_id in URL"}`, http.StatusBadRequest)
		return
	}

	log.Printf("[StreamV2] Connection request for session %s from %s", sessionID, r.RemoteAddr)

	// 2. Validate session with Control Plane (or get from cache)
	session, err := s.sessionManager.GetOrCreateSession(sessionID)
	if err != nil {
		log.Printf("[StreamV2] Session validation error for %s: %v", sessionID, err)
		http.Error(w, `{"error":"session validation failed"}`, http.StatusInternalServerError)
		return
	}

	if session == nil {
		// DEBUG MODE: Try to create a debug session if tenant is connected
		log.Printf("[StreamV2] Session nil, attempting DEBUG mode for: %s", sessionID)

		// Log all connected tenants for debugging
		connectedTenants := s.registry.GetConnectedTenants()
		log.Printf("[StreamV2] DEBUG: Connected tenants: %v", connectedTenants)

		if s.registry.IsConnected(sessionID) {
			log.Printf("[StreamV2] DEBUG: Tenant %s is connected, creating debug session", sessionID)
			// Create a minimal debug session
			session = &Session{
				ID:        sessionID,
				UserID:    "debug-user",
				CuentaID:  sessionID,
				ExpiresAt: time.Now().Add(1 * time.Hour),
				CreatedAt: time.Now(),
				done:      make(chan struct{}),
				isActive:  true,
			}
		} else {
			log.Printf("[StreamV2] Invalid or expired session: %s (not in connected list)", sessionID)
			http.Error(w, `{"error":"session not found or expired"}`, http.StatusUnauthorized)
			return
		}
	}

	// 3. Verify that the edge/tenant is connected
	if !s.registry.IsConnected(session.CuentaID) {
		http.Error(w, `{"error":"tenant not connected: `+session.CuentaID+`"}`, http.StatusBadRequest)
		return
	}

	// 4. Upgrade to WebSocket
	ws, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("[StreamV2] WebSocket upgrade error: %v", err)
		return
	}
	defer ws.Close()

	// Store browser connection in session
	session.SetBrowserConn(ws)

	log.Printf("[StreamV2] Browser connected: session=%s user=%s cuenta=%s",
		session.ID, session.UserID, session.CuentaID)

	var writeMu sync.Mutex

	// Send authentication confirmation
	s.sendResponse(ws, &writeMu, StreamResponseV2{
		Status:    "authenticated",
		Message:   "session validated successfully",
		UserID:    session.UserID,
		CuentaID:  session.CuentaID,
		SessionID: session.ID,
	})

	// 5. Message loop
	for {
		select {
		case <-session.Done():
			// Session was revoked externally
			s.sendResponse(ws, &writeMu, StreamResponseV2{
				Status:  "disconnected",
				Message: "session revoked",
			})
			return

		default:
			// Read message with timeout
			_, data, err := ws.ReadMessage()
			if err != nil {
				// Only log unexpected errors, not normal disconnects
				if !websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.CloseNoStatusReceived) {
					log.Printf("[StreamV2] Read error: %v", err)
				}
				return
			}

			// Check if session is still valid
			if session.IsExpired() {
				s.sendResponse(ws, &writeMu, StreamResponseV2{
					Status:  "disconnected",
					Message: "session expired",
				})
				return
			}

			var req struct {
				Action  string `json:"action"`
				Dataset string `json:"dataset"`
			}
			if err := json.Unmarshal(data, &req); err != nil {
				s.sendResponse(ws, &writeMu, StreamResponseV2{
					Status: "error",
					Error:  "invalid JSON",
				})
				continue
			}

			switch req.Action {
			case "query":
				// Use the dataset from the session if not specified
				dataset := req.Dataset
				if dataset == "" {
					dataset = session.Dataset
				}
				go s.handleQuery(ws, &writeMu, session, dataset)

			case "ping":
				s.sendResponse(ws, &writeMu, StreamResponseV2{
					Status:  "ok",
					Message: "pong",
				})

			default:
				s.sendResponse(ws, &writeMu, StreamResponseV2{
					Status: "error",
					Error:  "unknown action: " + req.Action,
				})
			}
		}
	}
}

// handleQuery processes a data query
func (s *StreamServerV2) handleQuery(ws *websocket.Conn, writeMu *sync.Mutex, session *Session, dataset string) {
	s.sendResponse(ws, writeMu, StreamResponseV2{
		Status:  "loading",
		Message: "loading dataset: " + dataset,
	})

	ctx := context.Background()
	chunks := make(chan []byte, 100)

	var totalBytes int64
	chunkCount := 0
	compressionDetected := "" // Will be "zstd" or "none"

	errChan := make(chan error, 1)
	go func() {
		errChan <- s.registry.QueryDataToChannel(ctx, session.CuentaID, dataset, chunks)
	}()

	log.Printf("[StreamV2] Query started: session=%s cuenta=%s dataset=%s",
		session.ID, session.CuentaID, dataset)

	// Stream chunks to browser
	firstChunk := true
	for chunk := range chunks {
		// Check if session was revoked
		select {
		case <-session.Done():
			log.Printf("[StreamV2] Session revoked during streaming: %s", session.ID)
			return
		default:
		}

		// Auto-detect compression from first chunk (ZSTD magic: 0x28B52FFD)
		if firstChunk {
			if len(chunk) >= 4 && chunk[0] == 0x28 && chunk[1] == 0xB5 && chunk[2] == 0x2F && chunk[3] == 0xFD {
				compressionDetected = "zstd"
			} else {
				compressionDetected = "none"
			}
			// Send streaming status with detected compression BEFORE first data
			s.sendResponse(ws, writeMu, StreamResponseV2{
				Status:      "streaming",
				Message:     "starting data transfer",
				Compression: compressionDetected,
			})
			log.Printf("[StreamV2] Compression auto-detected: %s", compressionDetected)
			firstChunk = false
		}

		writeMu.Lock()
		err := ws.WriteMessage(websocket.BinaryMessage, chunk)
		writeMu.Unlock()

		if err != nil {
			log.Printf("[StreamV2] Write error: %v", err)
			return
		}

		totalBytes += int64(len(chunk))
		chunkCount++
	}

	if err := <-errChan; err != nil {
		log.Printf("[StreamV2] Query error: %v", err)
	}

	s.sendResponse(ws, writeMu, StreamResponseV2{
		Status:     "complete",
		TotalBytes: totalBytes,
		Chunks:     chunkCount,
	})
	log.Printf("[StreamV2] Query complete: %d bytes, %d chunks", totalBytes, chunkCount)
}

// sendResponse sends a JSON response to the browser
func (s *StreamServerV2) sendResponse(ws *websocket.Conn, writeMu *sync.Mutex, resp StreamResponseV2) {
	data, err := json.Marshal(resp)
	if err != nil {
		return
	}

	writeMu.Lock()
	ws.WriteMessage(websocket.TextMessage, data)
	writeMu.Unlock()
}
