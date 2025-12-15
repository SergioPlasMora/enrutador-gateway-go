package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"sync"
	"time"

	"github.com/apache/arrow/go/v14/arrow"
	"github.com/apache/arrow/go/v14/arrow/flight"
	"github.com/apache/arrow/go/v14/arrow/ipc"
	"github.com/apache/arrow/go/v14/arrow/memory"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// ConnectorRegistry mantiene el registro de conectores gRPC disponibles
type ConnectorRegistry struct {
	mu         sync.RWMutex
	connectors map[string]*ConnectorInfo // tenant_id → info
	clients    map[string]flight.Client  // tenant_id → flight client
}

// ConnectorInfo contiene información de un conector
type ConnectorInfo struct {
	TenantID string `json:"tenant_id"`
	Address  string `json:"address"` // host:port
	Status   string `json:"status"`
}

// NewConnectorRegistry crea un nuevo registro de conectores
func NewConnectorRegistry() *ConnectorRegistry {
	return &ConnectorRegistry{
		connectors: make(map[string]*ConnectorInfo),
		clients:    make(map[string]flight.Client),
	}
}

// RegisterConnector registra un conector por su tenant_id
func (r *ConnectorRegistry) RegisterConnector(tenantID, address string) error {
	r.mu.Lock()
	defer r.mu.Unlock()

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
	}
	r.clients[tenantID] = client

	log.Printf("[ConnectorRegistry] Registered: %s at %s", tenantID, address)
	return nil
}

// UnregisterConnector elimina un conector del registro
func (r *ConnectorRegistry) UnregisterConnector(tenantID string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if client, exists := r.clients[tenantID]; exists {
		client.Close()
		delete(r.clients, tenantID)
	}
	delete(r.connectors, tenantID)

	log.Printf("[ConnectorRegistry] Unregistered: %s", tenantID)
}

// GetClient obtiene el cliente Flight para un tenant
func (r *ConnectorRegistry) GetClient(tenantID string) (flight.Client, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	client, exists := r.clients[tenantID]
	return client, exists
}

// IsConnected verifica si un tenant está conectado
func (r *ConnectorRegistry) IsConnected(tenantID string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()

	_, exists := r.clients[tenantID]
	return exists
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
func (r *ConnectorRegistry) QueryDataToChannel(ctx context.Context, tenantID, dataset string, chunks chan []byte) error {
	defer close(chunks) // IMPORTANTE: cerrar el canal al terminar

	client, exists := r.GetClient(tenantID)
	if !exists {
		return fmt.Errorf("tenant not connected: %s", tenantID)
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
