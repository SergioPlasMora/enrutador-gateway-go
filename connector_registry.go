package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/apache/arrow/go/v14/arrow"
	"github.com/apache/arrow/go/v14/arrow/flight"
	"github.com/apache/arrow/go/v14/arrow/ipc"
	"github.com/apache/arrow/go/v14/arrow/memory"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// ConnectorRegistry mantiene el registro de conectores disponibles (gRPC y WebSocket)
// Uses connector_id as primary key to support multiple connectors per tenant
type ConnectorRegistry struct {
	mu           sync.RWMutex
	connectors   map[string]*ConnectorInfo     // connector_id → info
	clients      map[string]flight.Client      // connector_id → flight client (legacy)
	wsClients    map[string]*WSConnectorClient // connector_id → WebSocket client
	grpcClients  map[string]interface{}        // connector_id → gRPC bidirectional client
	tenantLookup map[string]string             // connector_id → tenant_id (for auth)
}

// ConnectorInfo contiene información de un conector
type ConnectorInfo struct {
	ConnectorID string `json:"connector_id"`
	TenantID    string `json:"tenant_id"`
	Address     string `json:"address"` // host:port or "websocket" or "grpc"
	Status      string `json:"status"`
	Mode        string `json:"mode"` // "grpc", "websocket", or "grpc-bidi"
}

// NewConnectorRegistry crea un nuevo registro de conectores
func NewConnectorRegistry() *ConnectorRegistry {
	return &ConnectorRegistry{
		connectors:   make(map[string]*ConnectorInfo),
		clients:      make(map[string]flight.Client),
		wsClients:    make(map[string]*WSConnectorClient),
		grpcClients:  make(map[string]interface{}),
		tenantLookup: make(map[string]string),
	}
}

// RegisterConnector registra un conector por su tenant_id
func (r *ConnectorRegistry) RegisterConnector(tenantID, address string) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	// Fix for Docker environment: map localhost to host.docker.internal
	// This allows the local connector to register as "localhost" but be reachable from the container
	if os.Getenv("RUNNING_IN_DOCKER") == "true" {
		originalAddress := address
		if strings.Contains(address, "localhost") {
			address = strings.Replace(address, "localhost", "host.docker.internal", 1)
		} else if strings.Contains(address, "127.0.0.1") {
			address = strings.Replace(address, "127.0.0.1", "host.docker.internal", 1)
		} else if strings.Contains(address, "[::1]") {
			address = strings.Replace(address, "[::1]", "host.docker.internal", 1)
		} else if strings.Contains(address, "0.0.0.0") {
			address = strings.Replace(address, "0.0.0.0", "host.docker.internal", 1)
		}

		if address != originalAddress {
			log.Printf("[ConnectorRegistry] Docker fix: mapped %s -> %s", originalAddress, address)
		}
	}

	// Crear cliente Flight
	client, err := flight.NewClientWithMiddleware(
		address,
		nil,
		nil,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		return fmt.Errorf("failed to connect to connector %s: %w", address, err)
	}

	r.connectors[tenantID] = &ConnectorInfo{
		TenantID: tenantID,
		Address:  address,
		Status:   "connected",
		Mode:     "grpc",
	}
	r.clients[tenantID] = client

	log.Printf("[ConnectorRegistry] Registered gRPC: %s at %s", tenantID, address)
	return nil
}

// RegisterWSConnector registra un conector WebSocket
func (r *ConnectorRegistry) RegisterWSConnector(tenantID string, client *WSConnectorClient) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.connectors[tenantID] = &ConnectorInfo{
		TenantID: tenantID,
		Address:  "websocket",
		Status:   "connected",
		Mode:     "websocket",
	}
	r.wsClients[tenantID] = client

	log.Printf("[ConnectorRegistry] Registered WebSocket: %s", tenantID)
}

// UnregisterWSConnector elimina un conector WebSocket del registro
func (r *ConnectorRegistry) UnregisterWSConnector(tenantID string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	delete(r.wsClients, tenantID)
	delete(r.connectors, tenantID)

	log.Printf("[ConnectorRegistry] Unregistered WebSocket: %s", tenantID)
}

// RegisterGRPCConnector registra un conector gRPC bidireccional
// connectorID is the unique identifier for this connector instance
// tenantID is the workspace/account the connector belongs to
func (r *ConnectorRegistry) RegisterGRPCConnector(connectorID, tenantID string, client interface{}) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.connectors[connectorID] = &ConnectorInfo{
		ConnectorID: connectorID,
		TenantID:    tenantID,
		Address:     "grpc-bidi",
		Status:      "connected",
		Mode:        "grpc-bidi",
	}
	r.grpcClients[connectorID] = client
	r.tenantLookup[connectorID] = tenantID

	log.Printf("[ConnectorRegistry] Registered gRPC-Bidi: connector=%s tenant=%s", connectorID, tenantID)
}

// UnregisterGRPCConnector elimina un conector gRPC del registro
func (r *ConnectorRegistry) UnregisterGRPCConnector(connectorID string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	delete(r.grpcClients, connectorID)
	delete(r.connectors, connectorID)
	delete(r.tenantLookup, connectorID)

	log.Printf("[ConnectorRegistry] Unregistered gRPC-Bidi: %s", connectorID)
}

// GetGRPCClient obtiene el cliente gRPC para un tenant
func (r *ConnectorRegistry) GetGRPCClient(tenantID string) (interface{}, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	client, exists := r.grpcClients[tenantID]
	return client, exists
}

// UnregisterConnector elimina un conector del registro (gRPC)
func (r *ConnectorRegistry) UnregisterConnector(tenantID string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if client, exists := r.clients[tenantID]; exists {
		client.Close()
		delete(r.clients, tenantID)
	}
	delete(r.connectors, tenantID)

	log.Printf("[ConnectorRegistry] Unregistered gRPC: %s", tenantID)
}

// GetClient obtiene el cliente Flight gRPC para un tenant
func (r *ConnectorRegistry) GetClient(tenantID string) (flight.Client, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	client, exists := r.clients[tenantID]
	return client, exists
}

// GetWSClient obtiene el cliente WebSocket para un tenant
func (r *ConnectorRegistry) GetWSClient(tenantID string) (*WSConnectorClient, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	client, exists := r.wsClients[tenantID]
	return client, exists
}

// GetConnectorMode retorna el modo de conexión del tenant ("grpc", "websocket", o "" si no existe)
func (r *ConnectorRegistry) GetConnectorMode(tenantID string) string {
	r.mu.RLock()
	defer r.mu.RUnlock()

	if info, exists := r.connectors[tenantID]; exists {
		return info.Mode
	}
	return ""
}

// IsConnected verifica si un tenant está conectado (gRPC, WebSocket o gRPC-bidi)
func (r *ConnectorRegistry) IsConnected(tenantID string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()

	_, grpcExists := r.clients[tenantID]
	_, wsExists := r.wsClients[tenantID]
	_, grpcBidiExists := r.grpcClients[tenantID]
	return grpcExists || wsExists || grpcBidiExists
}

// GetConnectedTenants returns a list of all connected tenant IDs (for debugging)
func (r *ConnectorRegistry) GetConnectedTenants() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()

	tenants := make([]string, 0)
	for id := range r.clients {
		tenants = append(tenants, id+" (grpc)")
	}
	for id := range r.wsClients {
		tenants = append(tenants, id+" (websocket)")
	}
	for id := range r.grpcClients {
		tenants = append(tenants, id+" (grpc-bidi)")
	}
	return tenants
}

// ListConnectors retorna la lista de conectores registrados
func (r *ConnectorRegistry) ListConnectors() []*ConnectorInfo {
	r.mu.RLock()
	defer r.mu.RUnlock()

	list := make([]*ConnectorInfo, 0, len(r.connectors))
	for _, info := range r.connectors {
		list = append(list, info)
	}
	return list
}

// LoadConfig carga configuración de conectores desde un mapa
func (r *ConnectorRegistry) LoadConfig(config map[string]string) error {
	for tenantID, address := range config {
		if err := r.RegisterConnector(tenantID, address); err != nil {
			log.Printf("[ConnectorRegistry] Warning: failed to register %s: %v", tenantID, err)
		}
	}
	return nil
}

// RegisterFromJSON registra conectores desde JSON
func (r *ConnectorRegistry) RegisterFromJSON(jsonData []byte) error {
	var config map[string]string
	if err := json.Unmarshal(jsonData, &config); err != nil {
		return err
	}
	return r.LoadConfig(config)
}

// QueryDataToChannel consulta datos y envía chunks serializados a un canal
// Soporta tanto conectores gRPC como WebSocket
func (r *ConnectorRegistry) QueryDataToChannel(ctx context.Context, tenantID, dataset string, chunks chan []byte) error {
	defer close(chunks) // IMPORTANTE: cerrar el canal al terminar

	// Verificar modo de conexión
	mode := r.GetConnectorMode(tenantID)
	if mode == "" {
		return fmt.Errorf("tenant not connected: %s", tenantID)
	}

	// Dispatch según el modo
	if mode == "websocket" {
		return r.queryDataViaWebSocket(ctx, tenantID, dataset, chunks)
	}

	// gRPC-bidi mode (nuevo túnel reverso gRPC)
	if mode == "grpc-bidi" {
		return r.queryDataViaGRPCBidi(ctx, tenantID, dataset, chunks)
	}

	// gRPC mode (legacy - conexión directa)
	client, exists := r.GetClient(tenantID)
	if !exists {
		return fmt.Errorf("gRPC client not found for tenant: %s", tenantID)
	}

	// Crear descriptor para el dataset
	descriptor := &flight.FlightDescriptor{
		Type: flight.DescriptorPATH,
		Path: []string{dataset},
	}

	// Timeout para la operación completa
	ctx, cancel := context.WithTimeout(ctx, 120*time.Second)
	defer cancel()

	// Paso 1: GetFlightInfo para cargar el dataset
	log.Printf("[ConnectorRegistry] GetFlightInfo for %s", dataset)
	info, err := client.GetFlightInfo(ctx, descriptor)
	if err != nil {
		return fmt.Errorf("get_flight_info failed: %w", err)
	}

	if len(info.Endpoint) == 0 {
		return fmt.Errorf("no endpoints returned")
	}

	// Paso 2: DoGet para obtener el stream
	ticket := info.Endpoint[0].Ticket
	log.Printf("[ConnectorRegistry] DoGet with ticket")

	stream, err := client.DoGet(ctx, ticket)
	if err != nil {
		return fmt.Errorf("do_get failed: %w", err)
	}

	// Crear un RecordReader desde el stream
	reader, err := flight.NewRecordReader(stream)
	if err != nil {
		return fmt.Errorf("failed to create record reader: %w", err)
	}
	defer reader.Release()

	alloc := memory.NewGoAllocator()

	// Leer records y serializarlos
	for reader.Next() {
		record := reader.Record()
		if record == nil {
			continue
		}

		// Serializar a Arrow IPC
		buf, err := serializeArrowRecord(record, alloc)
		if err != nil {
			log.Printf("[ConnectorRegistry] Serialize error: %v", err)
			continue
		}

		select {
		case chunks <- buf:
		case <-ctx.Done():
			return ctx.Err()
		}
	}

	if err := reader.Err(); err != nil && err != io.EOF {
		return err
	}

	return nil
}

// serializeArrowRecord serializa un arrow.Record a bytes IPC
func serializeArrowRecord(record arrow.Record, alloc memory.Allocator) ([]byte, error) {
	var buf []byte
	writer := &arrowBytesWriter{buf: &buf}

	ipcWriter := ipc.NewWriter(writer, ipc.WithSchema(record.Schema()), ipc.WithAllocator(alloc))

	if err := ipcWriter.Write(record); err != nil {
		ipcWriter.Close()
		return nil, err
	}

	if err := ipcWriter.Close(); err != nil {
		return nil, err
	}

	return buf, nil
}

// arrowBytesWriter implementa io.Writer para escribir a un slice de bytes
type arrowBytesWriter struct {
	buf *[]byte
}

func (w *arrowBytesWriter) Write(p []byte) (n int, err error) {
	*w.buf = append(*w.buf, p...)
	return len(p), nil
}

// queryDataViaWebSocket queries data through a WebSocket connector with parallel partitions
func (r *ConnectorRegistry) queryDataViaWebSocket(ctx context.Context, tenantID, dataset string, chunks chan []byte) error {
	client, exists := r.GetWSClient(tenantID)
	if !exists {
		return fmt.Errorf("WebSocket client not found for tenant: %s", tenantID)
	}

	// Timeout para la operación completa
	ctx, cancel := context.WithTimeout(ctx, 120*time.Second)
	defer cancel()

	requestID := fmt.Sprintf("req_%d", time.Now().UnixNano())

	// 1. Enviar GetFlightInfo para obtener número de particiones
	log.Printf("[ConnectorRegistry] WebSocket GetFlightInfo for %s", dataset)
	infoResp, err := client.SendCommand(ctx, &ConnectorMessage{
		Action:    "get_flight_info",
		RequestID: requestID,
		Descriptor: map[string]interface{}{
			"path": []string{dataset},
		},
	})
	if err != nil {
		return fmt.Errorf("get_flight_info failed: %w", err)
	}
	if infoResp.Status != "ok" {
		return fmt.Errorf("get_flight_info error: %s", infoResp.Error)
	}

	// 2. Obtener número de particiones de la respuesta
	partitions := 1
	if infoResp.Data != nil {
		if p, ok := infoResp.Data["partitions"]; ok {
			switch v := p.(type) {
			case float64:
				partitions = int(v)
			case int:
				partitions = v
			}
		}
	}

	log.Printf("[ConnectorRegistry] Starting %d parallel partition(s) for %s", partitions, dataset)

	// 3. Lanzar goroutines para cada partición EN PARALELO
	// Los chunks binarios ahora incluyen prefijo de request_id para routing correcto
	var wg sync.WaitGroup
	errChan := make(chan error, partitions)

	for i := 0; i < partitions; i++ {
		wg.Add(1)
		go func(partition int) {
			defer wg.Done()
			err := r.fetchPartition(ctx, client, dataset, partition, partitions, chunks)
			if err != nil {
				log.Printf("[ConnectorRegistry] Partition %d error: %v", partition, err)
				errChan <- err
			}
		}(i)
	}

	// 4. Esperar a que todas las particiones terminen
	wg.Wait()
	close(errChan)

	// Verificar si hubo errores
	for err := range errChan {
		if err != nil {
			return err
		}
	}

	log.Printf("[ConnectorRegistry] All %d partitions complete", partitions)
	return nil
}

// fetchPartition fetches a single partition from the connector
func (r *ConnectorRegistry) fetchPartition(ctx context.Context, client *WSConnectorClient, dataset string, partition, totalPartitions int, chunks chan []byte) error {
	// Crear ticket con información de partición (base64 encoded JSON)
	ticketData := map[string]interface{}{
		"dataset":          dataset,
		"partition":        partition,
		"total_partitions": totalPartitions,
	}
	ticketJSON, _ := json.Marshal(ticketData)
	ticketB64 := base64.StdEncoding.EncodeToString(ticketJSON)

	doGetID := fmt.Sprintf("get_%d_p%d", time.Now().UnixNano(), partition)
	log.Printf("[ConnectorRegistry] DoGet partition %d/%d for %s", partition, totalPartitions, dataset)

	// Registrar canal para recibir chunks binarios de esta partición
	chunkChan := make(chan []byte, 100)
	client.chunksMu.Lock()
	client.chunks[doGetID] = chunkChan
	client.chunksMu.Unlock()

	defer func() {
		client.chunksMu.Lock()
		delete(client.chunks, doGetID)
		client.chunksMu.Unlock()
	}()

	// Enviar comando DoGet con ticket de partición
	_, err := client.SendCommand(ctx, &ConnectorMessage{
		Action:    "do_get",
		RequestID: doGetID,
		Ticket:    ticketB64,
	})
	if err != nil {
		return fmt.Errorf("do_get partition %d failed: %w", partition, err)
	}

	// Recibir chunks binarios de esta partición
	for {
		select {
		case chunk, ok := <-chunkChan:
			if !ok {
				// Canal cerrado = partición terminada
				log.Printf("[ConnectorRegistry] Partition %d complete", partition)
				return nil
			}
			select {
			case chunks <- chunk:
			case <-ctx.Done():
				return ctx.Err()
			}
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// queryDataViaGRPCBidi handles data queries via gRPC bidirectional tunnel
// This delegates to the native protobuf implementation
func (r *ConnectorRegistry) queryDataViaGRPCBidi(ctx context.Context, tenantID, dataset string, chunks chan []byte) error {
	// Use the native protobuf implementation
	return r.queryDataViaGRPCBidiNative(ctx, tenantID, dataset, chunks)
}
