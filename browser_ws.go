package main

import (
	"encoding/json"
	"log"
	"net/http"
	"sync"

	"github.com/gorilla/websocket"
)

// BrowserWSServer maneja conexiones WebSocket desde navegadores
type BrowserWSServer struct {
	manager  *ConnectionManager
	upgrader websocket.Upgrader
}

// BrowserRequest representa un request del navegador
type BrowserRequest struct {
	Action   string `json:"action"`
	TenantID string `json:"tenant"`
	Dataset  string `json:"dataset"`
	Rows     int    `json:"rows,omitempty"`
}

// BrowserResponse representa una respuesta de control al navegador
type BrowserResponse struct {
	Status     string `json:"status"`
	Message    string `json:"message,omitempty"`
	TotalBytes int64  `json:"total_bytes,omitempty"`
	Chunks     int    `json:"chunks,omitempty"`
	Error      string `json:"error,omitempty"`
}

// NewBrowserWSServer crea un nuevo servidor WebSocket para navegadores
func NewBrowserWSServer(manager *ConnectionManager) *BrowserWSServer {
	return &BrowserWSServer{
		manager: manager,
		upgrader: websocket.Upgrader{
			ReadBufferSize:  1024 * 64,
			WriteBufferSize: 1024 * 1024, // 1MB para chunks Arrow
			CheckOrigin: func(r *http.Request) bool {
				return true // Permitir cualquier origen (desarrollo)
			},
		},
	}
}

// HandleConnection maneja una conexión WebSocket del navegador
func (s *BrowserWSServer) HandleConnection(w http.ResponseWriter, r *http.Request) {
	ws, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("[BrowserWS] Upgrade error: %v", err)
		return
	}
	defer ws.Close()

	log.Printf("[BrowserWS] Browser connected from %s", r.RemoteAddr)

	// Mutex para escritura serializada
	var writeMu sync.Mutex

	for {
		_, data, err := ws.ReadMessage()
		if err != nil {
			log.Printf("[BrowserWS] Read error: %v", err)
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
			go s.handleQuery(ws, &writeMu, req)
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

	log.Printf("[BrowserWS] Browser disconnected")
}

// handleQuery procesa una query y envía Arrow IPC al navegador
func (s *BrowserWSServer) handleQuery(ws *websocket.Conn, writeMu *sync.Mutex, req BrowserRequest) {
	// Verificar que el tenant esté conectado
	if !s.manager.IsConnected(req.TenantID) {
		s.sendResponse(ws, writeMu, BrowserResponse{
			Status: "error",
			Error:  "tenant not connected: " + req.TenantID,
		})
		return
	}

	// Enviar mensaje de inicio
	s.sendResponse(ws, writeMu, BrowserResponse{
		Status:  "loading",
		Message: "loading dataset: " + req.Dataset,
	})

	// PASO 1: Llamar a get_flight_info para que el conector cargue el dataset
	flightInfoReq := map[string]interface{}{
		"descriptor": map[string]interface{}{
			"type": "PATH",
			"path": []string{req.Dataset},
		},
	}

	log.Printf("[BrowserWS] Loading dataset: %s", req.Dataset)
	_, err := s.manager.SendRequest(req.TenantID, "get_flight_info", flightInfoReq)
	if err != nil {
		s.sendResponse(ws, writeMu, BrowserResponse{
			Status: "error",
			Error:  "failed to load dataset: " + err.Error(),
		})
		return
	}

	// Enviar mensaje de streaming
	s.sendResponse(ws, writeMu, BrowserResponse{
		Status:  "streaming",
		Message: "starting data transfer",
	})

	// PASO 2: Preparar ticket para do_get
	ticketData := map[string]interface{}{
		"tenant_id": req.TenantID,
		"dataset":   req.Dataset,
	}
	if req.Rows > 0 {
		ticketData["rows"] = req.Rows
	}

	// Codificar ticket
	ticketJSON, _ := json.Marshal(ticketData)
	ticketB64 := base64Encode(ticketJSON)

	reqData := map[string]interface{}{
		"ticket": ticketB64,
	}

	// PASO 3: Solicitar stream al Data Connector
	requestID, chunks, done, err := s.manager.SendStreamRequest(req.TenantID, "do_get", reqData)
	if err != nil {
		s.sendResponse(ws, writeMu, BrowserResponse{
			Status: "error",
			Error:  err.Error(),
		})
		return
	}

	log.Printf("[BrowserWS] Query started: %s/%s (request: %s)", req.TenantID, req.Dataset, requestID)

	// Procesar chunks y reenviar al navegador
	var totalBytes int64
	chunkCount := 0

	for {
		select {
		case chunk, ok := <-chunks:
			if !ok {
				// Stream terminado
				s.sendResponse(ws, writeMu, BrowserResponse{
					Status:     "complete",
					TotalBytes: totalBytes,
					Chunks:     chunkCount,
				})
				log.Printf("[BrowserWS] Query complete: %d bytes, %d chunks", totalBytes, chunkCount)
				return
			}

			// Enviar chunk binario al navegador
			writeMu.Lock()
			err := ws.WriteMessage(websocket.BinaryMessage, chunk)
			writeMu.Unlock()

			if err != nil {
				log.Printf("[BrowserWS] Write error: %v", err)
				return
			}

			totalBytes += int64(len(chunk))
			chunkCount++

		case <-done:
			s.sendResponse(ws, writeMu, BrowserResponse{
				Status:     "complete",
				TotalBytes: totalBytes,
				Chunks:     chunkCount,
			})
			log.Printf("[BrowserWS] Query done signal: %d bytes", totalBytes)
			return
		}
	}
}

// sendResponse envía una respuesta JSON al navegador
func (s *BrowserWSServer) sendResponse(ws *websocket.Conn, writeMu *sync.Mutex, resp BrowserResponse) {
	data, err := json.Marshal(resp)
	if err != nil {
		return
	}

	writeMu.Lock()
	ws.WriteMessage(websocket.TextMessage, data)
	writeMu.Unlock()
}

// base64Encode codifica bytes a base64
func base64Encode(data []byte) string {
	const base64Chars = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"
	result := make([]byte, ((len(data)+2)/3)*4)

	for i, j := 0, 0; i < len(data); i += 3 {
		var val uint32
		remaining := len(data) - i

		val = uint32(data[i]) << 16
		if remaining > 1 {
			val |= uint32(data[i+1]) << 8
		}
		if remaining > 2 {
			val |= uint32(data[i+2])
		}

		result[j] = base64Chars[(val>>18)&0x3F]
		result[j+1] = base64Chars[(val>>12)&0x3F]

		if remaining > 1 {
			result[j+2] = base64Chars[(val>>6)&0x3F]
		} else {
			result[j+2] = '='
		}

		if remaining > 2 {
			result[j+3] = base64Chars[val&0x3F]
		} else {
			result[j+3] = '='
		}

		j += 4
	}

	return string(result)
}
