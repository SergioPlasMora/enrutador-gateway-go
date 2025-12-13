package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"

	"github.com/apache/arrow/go/v14/arrow/flight"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// FlightServer implementa el servidor Arrow Flight
type FlightServer struct {
	flight.BaseFlightServer
	manager *ConnectionManager
	port    int
}

// NewFlightServer crea un nuevo servidor Flight
func NewFlightServer(manager *ConnectionManager, port int) *FlightServer {
	return &FlightServer{
		manager: manager,
		port:    port,
	}
}

// GetFlightInfo obtiene metadata de un dataset
func (s *FlightServer) GetFlightInfo(ctx context.Context, desc *flight.FlightDescriptor) (*flight.FlightInfo, error) {
	// Extraer tenant_id y dataset del path
	if len(desc.Path) < 2 {
		return nil, status.Error(codes.InvalidArgument, "invalid descriptor path")
	}

	tenantID := string(desc.Path[0])
	dataset := string(desc.Path[1])

	var rows int
	if len(desc.Path) > 2 {
		fmt.Sscanf(string(desc.Path[2]), "%d", &rows)
	}

	if !s.manager.IsConnected(tenantID) {
		return nil, status.Errorf(codes.Unavailable, "tenant %s not connected", tenantID)
	}

	// Preparar request para el Data Connector
	reqData := map[string]interface{}{
		"descriptor": map[string]interface{}{
			"type": "PATH",
			"path": []string{dataset},
		},
	}
	if rows > 0 {
		reqData["descriptor"].(map[string]interface{})["rows"] = rows
	}

	// Enviar request al Data Connector
	resp, err := s.manager.SendRequest(tenantID, "get_flight_info", reqData)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "connector error: %v", err)
	}

	if resp.Status != "ok" {
		return nil, status.Errorf(codes.Internal, "connector error: %s", resp.Error)
	}

	// Construir ticket
	ticketData := map[string]interface{}{
		"tenant_id": tenantID,
		"dataset":   dataset,
	}
	if rows > 0 {
		ticketData["rows"] = rows
	}
	ticketBytes, _ := json.Marshal(ticketData)

	// Extraer metadata
	totalRecords := int64(-1)
	totalBytes := int64(-1)
	if data := resp.Data; data != nil {
		if tr, ok := data["total_records"].(float64); ok {
			totalRecords = int64(tr)
		}
		if tb, ok := data["total_bytes"].(float64); ok {
			totalBytes = int64(tb)
		}
	}

	// Construir FlightInfo
	info := &flight.FlightInfo{
		FlightDescriptor: desc,
		Endpoint: []*flight.FlightEndpoint{{
			Ticket: &flight.Ticket{Ticket: ticketBytes},
		}},
		TotalRecords: totalRecords,
		TotalBytes:   totalBytes,
	}

	return info, nil
}

// DoGet retorna un stream de datos Arrow
func (s *FlightServer) DoGet(ticket *flight.Ticket, stream flight.FlightService_DoGetServer) error {
	// Parsear ticket
	var ticketData map[string]interface{}
	if err := json.Unmarshal(ticket.Ticket, &ticketData); err != nil {
		return status.Error(codes.InvalidArgument, "invalid ticket")
	}

	tenantID, _ := ticketData["tenant_id"].(string)
	dataset, _ := ticketData["dataset"].(string)
	rows := 0
	if r, ok := ticketData["rows"].(float64); ok {
		rows = int(r)
	}

	if !s.manager.IsConnected(tenantID) {
		return status.Errorf(codes.Unavailable, "tenant %s not connected", tenantID)
	}

	// Preparar request para stream - enviar ticket en base64 como Node.js
	// El Data Connector espera "ticket" no "descriptor" para do_get
	ticketForConnector := map[string]interface{}{
		"tenant_id": tenantID,
		"dataset":   dataset,
	}
	if rows > 0 {
		ticketForConnector["rows"] = rows
	}

	// Codificar ticket en base64
	ticketJSON, _ := json.Marshal(ticketForConnector)
	ticketB64 := base64.StdEncoding.EncodeToString(ticketJSON)

	reqData := map[string]interface{}{
		"ticket": ticketB64,
	}

	// Solicitar stream al Data Connector
	requestID, chunks, done, err := s.manager.SendStreamRequest(tenantID, "do_get", reqData)
	if err != nil {
		return status.Errorf(codes.Internal, "connector error: %v", err)
	}

	log.Printf("[Flight] DoGet started: %s/%s (request: %s)", tenantID, dataset, requestID)

	// Procesar chunks - enviar directamente como Node.js (sin parsear)
	var totalBytes int64
	chunkCount := 0

	for {
		select {
		case chunk, ok := <-chunks:
			if !ok {
				// Stream terminado
				log.Printf("[Flight] DoGet completed: %s/%s (%d bytes, %d chunks)", tenantID, dataset, totalBytes, chunkCount)
				return nil
			}

			totalBytes += int64(len(chunk))
			chunkCount++

			// Enviar chunk directamente como data_body (igual que Node.js)
			// Arrow Flight protocol: data_header vacío + data_body con los bytes IPC
			flightData := &flight.FlightData{
				DataBody: chunk,
			}
			if err := stream.Send(flightData); err != nil {
				log.Printf("[Flight] Send error: %v", err)
				return err
			}

		case <-done:
			log.Printf("[Flight] DoGet done signal: %s/%s", tenantID, dataset)
			return nil

		case <-stream.Context().Done():
			log.Printf("[Flight] DoGet cancelled: %s/%s", tenantID, dataset)
			return stream.Context().Err()
		}
	}
}

// Start inicia el servidor Flight
func (s *FlightServer) Start() error {
	addr := fmt.Sprintf("0.0.0.0:%d", s.port)
	log.Printf("[Flight] Server starting on %s", addr)

	server := flight.NewServerWithMiddleware(nil)
	server.RegisterFlightService(s)

	if err := server.Init(addr); err != nil {
		return err
	}

	return server.Serve()
}
