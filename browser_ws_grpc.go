package main

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"sync"

	"github.com/gorilla/websocket"
)

// BrowserWSServerGRPC maneja conexiones WebSocket desde navegadores usando gRPC para conectores
type BrowserWSServerGRPC struct {
	registry *ConnectorRegistry
	upgrader websocket.Upgrader
}

// NewBrowserWSServerGRPC crea un nuevo servidor WebSocket para navegadores (versión gRPC)
func NewBrowserWSServerGRPC(registry *ConnectorRegistry) *BrowserWSServerGRPC {
	return &BrowserWSServerGRPC{
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

// HandleConnection maneja una conexión WebSocket del navegador
func (s *BrowserWSServerGRPC) HandleConnection(w http.ResponseWriter, r *http.Request) {
	ws, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("[BrowserWS-gRPC] Upgrade error: %v", err)
		return
	}
	defer ws.Close()

	log.Printf("[BrowserWS-gRPC] Browser connected from %s", r.RemoteAddr)

	var writeMu sync.Mutex

	for {
		_, data, err := ws.ReadMessage()
		if err != nil {
			log.Printf("[BrowserWS-gRPC] Read error: %v", err)
			break
		}

		var req BrowserRequest
		if err := json.Unmarshal(data, &req); err != nil {
			s.sendResponse(ws, &writeMu, BrowserResponse{
				Status: "error",
				Error:  "invalid JSON",
			})
			continue
		}

		switch req.Action {
		case "query":
			go s.handleQueryGRPC(ws, &writeMu, req)
		case "list_connectors":
			s.handleListConnectors(ws, &writeMu)
		case "ping":
			s.sendResponse(ws, &writeMu, BrowserResponse{
				Status:  "ok",
				Message: "pong",
			})
		default:
			s.sendResponse(ws, &writeMu, BrowserResponse{
				Status: "error",
				Error:  "unknown action: " + req.Action,
			})
		}
	}

	log.Printf("[BrowserWS-gRPC] Browser disconnected")
}

// handleQueryGRPC procesa una query usando gRPC/Arrow Flight
func (s *BrowserWSServerGRPC) handleQueryGRPC(ws *websocket.Conn, writeMu *sync.Mutex, req BrowserRequest) {
	// Verificar que el tenant esté registrado
	if !s.registry.IsConnected(req.TenantID) {
		s.sendResponse(ws, writeMu, BrowserResponse{
			Status: "error",
			Error:  "tenant not connected: " + req.TenantID,
		})
		return
	}

	s.sendResponse(ws, writeMu, BrowserResponse{
		Status:  "loading",
		Message: "loading dataset via gRPC: " + req.Dataset,
	})

	ctx := context.Background()

	// Crear canal para chunks
	chunks := make(chan []byte, 100)

	// Variables para estadísticas
	var totalBytes int64
	chunkCount := 0

	// Iniciar query en goroutine
	errChan := make(chan error, 1)
	go func() {
		errChan <- s.registry.QueryDataToChannel(ctx, req.TenantID, req.Dataset, chunks)
	}()

	s.sendResponse(ws, writeMu, BrowserResponse{
		Status:  "streaming",
		Message: "starting data transfer",
	})

	log.Printf("[BrowserWS-gRPC] Query started: %s/%s", req.TenantID, req.Dataset)

	// Recibir y reenviar chunks
	for chunk := range chunks {
		writeMu.Lock()
		err := ws.WriteMessage(websocket.BinaryMessage, chunk)
		writeMu.Unlock()

		if err != nil {
			log.Printf("[BrowserWS-gRPC] Write error: %v", err)
			return
		}

		totalBytes += int64(len(chunk))
		chunkCount++
	}

	// Verificar si hubo error en la query
	if err := <-errChan; err != nil {
		log.Printf("[BrowserWS-gRPC] Query error: %v", err)
	}

	s.sendResponse(ws, writeMu, BrowserResponse{
		Status:     "complete",
		TotalBytes: totalBytes,
		Chunks:     chunkCount,
	})
	log.Printf("[BrowserWS-gRPC] Query complete: %d bytes, %d chunks", totalBytes, chunkCount)
}

// handleListConnectors retorna la lista de conectores disponibles
func (s *BrowserWSServerGRPC) handleListConnectors(ws *websocket.Conn, writeMu *sync.Mutex) {
	connectors := s.registry.ListConnectors()
	data, _ := json.Marshal(map[string]interface{}{
		"status":     "ok",
		"connectors": connectors,
	})

	writeMu.Lock()
	ws.WriteMessage(websocket.TextMessage, data)
	writeMu.Unlock()
}

// sendResponse envía una respuesta JSON al navegador
func (s *BrowserWSServerGRPC) sendResponse(ws *websocket.Conn, writeMu *sync.Mutex, resp BrowserResponse) {
	data, err := json.Marshal(resp)
	if err != nil {
		return
	}

	writeMu.Lock()
	ws.WriteMessage(websocket.TextMessage, data)
	writeMu.Unlock()
}
