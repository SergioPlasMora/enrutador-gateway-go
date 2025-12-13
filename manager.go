package main

import (
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
)

// ConnectionManager gestiona las conexiones WebSocket con pool y cola
type ConnectionManager struct {
	// tenant_id -> pool de conexiones disponibles
	connectionPools map[string]chan *websocket.Conn
	// tenant_id -> todas las conexiones (para cleanup)
	allConnections map[string][]*websocket.Conn
	// ws -> active stream collector (para rutear binarios por websocket)
	activeStreamsByWS map[*websocket.Conn]*ChunkCollector
	// request_id -> chan Response (para requests JSON)
	pendingRequests map[string]chan Response
	// ws -> mutex para escritura serializada
	writeLocks map[*websocket.Conn]*sync.Mutex
	mu         sync.RWMutex
}

// Response representa una respuesta JSON del Data Connector
type Response struct {
	Status    string                 `json:"status"`
	RequestID string                 `json:"request_id"`
	Type      string                 `json:"type,omitempty"` // stream_start, stream_end
	Data      map[string]interface{} `json:"data,omitempty"`
	Error     string                 `json:"error,omitempty"`
}

// ChunkCollector acumula chunks binarios para un request
type ChunkCollector struct {
	chunks    chan []byte
	done      chan struct{}
	requestID string
	ws        *websocket.Conn
}

// NewConnectionManager crea un nuevo connection manager
func NewConnectionManager() *ConnectionManager {
	return &ConnectionManager{
		connectionPools:   make(map[string]chan *websocket.Conn),
		allConnections:    make(map[string][]*websocket.Conn),
		activeStreamsByWS: make(map[*websocket.Conn]*ChunkCollector),
		pendingRequests:   make(map[string]chan Response),
		writeLocks:        make(map[*websocket.Conn]*sync.Mutex),
	}
}

// Register registra un nuevo Data Connector y lo agrega al pool
func (m *ConnectionManager) Register(ws *websocket.Conn, tenantID string) string {
	m.mu.Lock()
	defer m.mu.Unlock()

	sessionID := uuid.New().String()

	// Inicializar pool si no existe (buffer grande para muchas conexiones)
	if _, exists := m.connectionPools[tenantID]; !exists {
		m.connectionPools[tenantID] = make(chan *websocket.Conn, 10000) // buffer grande
		m.allConnections[tenantID] = make([]*websocket.Conn, 0)
	}

	// Agregar al pool (disponible inmediatamente)
	m.connectionPools[tenantID] <- ws
	m.allConnections[tenantID] = append(m.allConnections[tenantID], ws)
	m.writeLocks[ws] = &sync.Mutex{}

	return sessionID
}

// Unregister desregistra un Data Connector
func (m *ConnectionManager) Unregister(ws *websocket.Conn, tenantID string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Remover de allConnections
	conns := m.allConnections[tenantID]
	for i, conn := range conns {
		if conn == ws {
			m.allConnections[tenantID] = append(conns[:i], conns[i+1:]...)
			break
		}
	}

	if len(m.allConnections[tenantID]) == 0 {
		delete(m.allConnections, tenantID)
		delete(m.connectionPools, tenantID)
	}

	delete(m.writeLocks, ws)
	delete(m.activeStreamsByWS, ws)
}

// IsConnected verifica si un tenant está conectado
func (m *ConnectionManager) IsConnected(tenantID string) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	conns, exists := m.allConnections[tenantID]
	return exists && len(conns) > 0
}

// acquireConnection obtiene una conexión del pool (espera si no hay disponibles)
func (m *ConnectionManager) acquireConnection(tenantID string, timeout time.Duration) (*websocket.Conn, error) {
	m.mu.RLock()
	pool, exists := m.connectionPools[tenantID]
	m.mu.RUnlock()

	if !exists {
		return nil, fmt.Errorf("tenant %s not connected", tenantID)
	}

	select {
	case ws := <-pool:
		return ws, nil
	case <-time.After(timeout):
		return nil, fmt.Errorf("timeout waiting for available connection")
	}
}

// releaseConnection devuelve una conexión al pool
func (m *ConnectionManager) releaseConnection(tenantID string, ws *websocket.Conn) {
	m.mu.RLock()
	pool, exists := m.connectionPools[tenantID]
	m.mu.RUnlock()

	if exists {
		select {
		case pool <- ws:
			// Devuelto al pool
		default:
			// Pool lleno (no debería pasar)
		}
	}
}

// SendRequest envía un request y espera respuesta JSON
func (m *ConnectionManager) SendRequest(tenantID, action string, data map[string]interface{}) (*Response, error) {
	// Adquirir conexión del pool (espera hasta 30s)
	ws, err := m.acquireConnection(tenantID, 30*time.Second)
	if err != nil {
		return nil, err
	}
	defer m.releaseConnection(tenantID, ws)

	requestID := uuid.New().String()

	// Crear canal para la respuesta
	respChan := make(chan Response, 1)
	m.mu.Lock()
	m.pendingRequests[requestID] = respChan
	m.mu.Unlock()

	defer func() {
		m.mu.Lock()
		delete(m.pendingRequests, requestID)
		m.mu.Unlock()
	}()

	// Construir mensaje - aplanar campos de data
	msg := map[string]interface{}{
		"action":     action,
		"request_id": requestID,
	}
	for k, v := range data {
		msg[k] = v
	}

	msgBytes, err := json.Marshal(msg)
	if err != nil {
		return nil, err
	}

	// Enviar mensaje (con lock de escritura)
	m.mu.RLock()
	wsMutex := m.writeLocks[ws]
	m.mu.RUnlock()

	wsMutex.Lock()
	err = ws.WriteMessage(websocket.TextMessage, msgBytes)
	wsMutex.Unlock()

	if err != nil {
		return nil, err
	}

	// Esperar respuesta
	select {
	case resp := <-respChan:
		return &resp, nil
	case <-time.After(30 * time.Second):
		return nil, fmt.Errorf("request timeout")
	}
}

// SendStreamRequest envía un request de stream (NO libera la conexión hasta que termine)
func (m *ConnectionManager) SendStreamRequest(tenantID, action string, data map[string]interface{}) (string, chan []byte, chan struct{}, error) {
	// Adquirir conexión del pool (espera hasta 60s para streams)
	ws, err := m.acquireConnection(tenantID, 60*time.Second)
	if err != nil {
		return "", nil, nil, err
	}

	requestID := uuid.New().String()

	// Crear collector para chunks
	collector := &ChunkCollector{
		chunks:    make(chan []byte, 1000),
		done:      make(chan struct{}),
		requestID: requestID,
		ws:        ws,
	}

	m.mu.Lock()
	m.activeStreamsByWS[ws] = collector
	m.mu.Unlock()

	// Construir mensaje - aplanar campos de data
	msg := map[string]interface{}{
		"action":     action,
		"request_id": requestID,
	}
	for k, v := range data {
		msg[k] = v
	}

	msgBytes, err := json.Marshal(msg)
	if err != nil {
		m.mu.Lock()
		delete(m.activeStreamsByWS, ws)
		m.mu.Unlock()
		m.releaseConnection(tenantID, ws)
		return "", nil, nil, err
	}

	// Enviar mensaje
	m.mu.RLock()
	wsMutex := m.writeLocks[ws]
	m.mu.RUnlock()

	wsMutex.Lock()
	err = ws.WriteMessage(websocket.TextMessage, msgBytes)
	wsMutex.Unlock()

	if err != nil {
		m.mu.Lock()
		delete(m.activeStreamsByWS, ws)
		m.mu.Unlock()
		m.releaseConnection(tenantID, ws)
		return "", nil, nil, err
	}

	// La conexión se liberará cuando se reciba stream_end (en HandleMessage)
	return requestID, collector.chunks, collector.done, nil
}

// HandleMessage procesa un mensaje entrante
func (m *ConnectionManager) HandleMessage(ws *websocket.Conn, messageType int, data []byte) {
	if messageType == websocket.TextMessage {
		// Mensaje JSON
		var resp Response
		if err := json.Unmarshal(data, &resp); err != nil {
			return
		}

		// Si es mensaje de control de stream (stream_end)
		if resp.Type == "stream_end" {
			m.mu.Lock()
			collector, exists := m.activeStreamsByWS[ws]
			if exists {
				close(collector.chunks)
				close(collector.done)
				delete(m.activeStreamsByWS, ws)
			}
			m.mu.Unlock()

			// IMPORTANTE: liberar conexión al pool cuando termina el stream
			// Encontrar tenant para este websocket
			m.mu.RLock()
			var foundTenant string
			for tenant, conns := range m.allConnections {
				for _, conn := range conns {
					if conn == ws {
						foundTenant = tenant
						break
					}
				}
				if foundTenant != "" {
					break
				}
			}
			m.mu.RUnlock()

			if foundTenant != "" {
				m.releaseConnection(foundTenant, ws)
			}
			return
		}

		// Si es respuesta a un request pendiente
		m.mu.RLock()
		respChan, exists := m.pendingRequests[resp.RequestID]
		m.mu.RUnlock()

		if exists {
			respChan <- resp
		}
	} else if messageType == websocket.BinaryMessage {
		// Chunk binario - buscar stream activo para este WebSocket
		m.mu.RLock()
		collector, exists := m.activeStreamsByWS[ws]
		m.mu.RUnlock()

		if exists && len(data) > 0 {
			collector.chunks <- data
		}
	}
}

// GetConnectionCount retorna el número de conexiones para un tenant
func (m *ConnectionManager) GetConnectionCount(tenantID string) int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.allConnections[tenantID])
}

// GetAvailableConnections retorna conexiones disponibles en el pool
func (m *ConnectionManager) GetAvailableConnections(tenantID string) int {
	m.mu.RLock()
	pool, exists := m.connectionPools[tenantID]
	m.mu.RUnlock()

	if !exists {
		return 0
	}
	return len(pool)
}
