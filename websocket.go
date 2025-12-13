package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"

	"github.com/gorilla/websocket"
)

var upgrader = websocket.Upgrader{
	ReadBufferSize:  1024 * 1024, // 1MB
	WriteBufferSize: 1024 * 1024,
	CheckOrigin: func(r *http.Request) bool {
		return true // Permitir todas las conexiones
	},
}

// WebSocketServer maneja las conexiones WebSocket de los Data Connectors
type WebSocketServer struct {
	manager *ConnectionManager
	port    int
}

// NewWebSocketServer crea un nuevo servidor WebSocket
func NewWebSocketServer(manager *ConnectionManager, port int) *WebSocketServer {
	return &WebSocketServer{
		manager: manager,
		port:    port,
	}
}

// handleConnection maneja una conexión WebSocket
func (s *WebSocketServer) handleConnection(w http.ResponseWriter, r *http.Request) {
	ws, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("[WebSocket] Upgrade error: %v", err)
		return
	}
	defer ws.Close()

	var tenantID string
	var registered bool

	// Configurar límites
	ws.SetReadLimit(200 * 1024 * 1024) // 200MB max message size

	for {
		messageType, data, err := ws.ReadMessage()
		if err != nil {
			if registered && tenantID != "" {
				s.manager.Unregister(ws, tenantID)
				log.Printf("[WebSocket] Disconnected: %s", tenantID)
			}
			break
		}

		if messageType == websocket.TextMessage {
			// Mensaje JSON
			var msg map[string]interface{}
			if err := json.Unmarshal(data, &msg); err != nil {
				log.Printf("[WebSocket] Invalid JSON: %v", err)
				continue
			}

			action, _ := msg["action"].(string)

			switch action {
			case "register":
				// Registro de Data Connector
				tenantID, _ = msg["tenant_id"].(string)
				if tenantID == "" {
					s.sendError(ws, "register", "tenant_id required")
					continue
				}

				sessionID := s.manager.Register(ws, tenantID)
				registered = true

				response := map[string]interface{}{
					"action":     "register",
					"status":     "ok",
					"session_id": sessionID,
				}
				s.sendJSON(ws, response)

				log.Printf("[WebSocket] Registered: %s (%d connections)",
					tenantID, s.manager.GetConnectionCount(tenantID))

			default:
				// Respuesta a un request pendiente
				s.manager.HandleMessage(ws, messageType, data)
			}
		} else if messageType == websocket.BinaryMessage {
			// Chunk binario - pasar al manager
			s.manager.HandleMessage(ws, messageType, data)
		}
	}
}

// sendJSON envía una respuesta JSON
func (s *WebSocketServer) sendJSON(ws *websocket.Conn, data interface{}) {
	msg, err := json.Marshal(data)
	if err != nil {
		log.Printf("[WebSocket] Marshal error: %v", err)
		return
	}
	ws.WriteMessage(websocket.TextMessage, msg)
}

// sendError envía un mensaje de error
func (s *WebSocketServer) sendError(ws *websocket.Conn, action, errMsg string) {
	response := map[string]interface{}{
		"action": action,
		"status": "error",
		"error":  errMsg,
	}
	s.sendJSON(ws, response)
}

// Start inicia el servidor WebSocket
func (s *WebSocketServer) Start() error {
	http.HandleFunc("/ws/connect", s.handleConnection)

	addr := fmt.Sprintf(":%d", s.port)
	log.Printf("[WebSocket] Server starting on %s", addr)

	return http.ListenAndServe(addr, nil)
}
