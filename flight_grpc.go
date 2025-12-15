package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"

	"github.com/apache/arrow/go/v14/arrow/flight"
	"github.com/apache/arrow/go/v14/arrow/memory"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// FlightServerGRPC implementa el servidor Arrow Flight usando gRPC para conectores
type FlightServerGRPC struct {
	flight.BaseFlightServer
	registry *ConnectorRegistry
	port     int
}

// NewFlightServerGRPC crea un nuevo servidor Flight que usa gRPC para conectores
func NewFlightServerGRPC(registry *ConnectorRegistry, port int) *FlightServerGRPC {
	return &FlightServerGRPC{
		registry: registry,
		port:     port,
	}
}

// GetFlightInfo obtiene metadata de un dataset
func (s *FlightServerGRPC) GetFlightInfo(ctx context.Context, desc *flight.FlightDescriptor) (*flight.FlightInfo, error) {
	// Extraer tenant_id y dataset del path
	if len(desc.Path) < 2 {
		return nil, status.Error(codes.InvalidArgument, "invalid descriptor path: need [tenant_id, dataset]")
	}

	tenantID := string(desc.Path[0])
	dataset := string(desc.Path[1])

	if !s.registry.IsConnected(tenantID) {
		return nil, status.Errorf(codes.Unavailable, "tenant %s not connected", tenantID)
	}

	// Obtener cliente Flight del conector
	client, _ := s.registry.GetClient(tenantID)

	// Crear descriptor para el conector
	connectorDesc := &flight.FlightDescriptor{
		Type: flight.DescriptorPATH,
		Path: []string{dataset},
	}

	// Llamar al conector para obtener info
	info, err := client.GetFlightInfo(ctx, connectorDesc)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "connector error: %v", err)
	}

	// Modificar ticket para incluir tenant_id
	ticketData := map[string]interface{}{
		"tenant_id": tenantID,
		"dataset":   dataset,
	}
	ticketBytes, _ := json.Marshal(ticketData)

	// Construir FlightInfo con nuestro ticket
	resultInfo := &flight.FlightInfo{
		FlightDescriptor: desc,
		Endpoint: []*flight.FlightEndpoint{{
			Ticket: &flight.Ticket{Ticket: ticketBytes},
		}},
		TotalRecords: info.TotalRecords,
		TotalBytes:   info.TotalBytes,
	}

	return resultInfo, nil
}

// DoGet retorna un stream de datos Arrow
func (s *FlightServerGRPC) DoGet(ticket *flight.Ticket, stream flight.FlightService_DoGetServer) error {
	// Parsear ticket
	var ticketData map[string]interface{}
	if err := json.Unmarshal(ticket.Ticket, &ticketData); err != nil {
		return status.Error(codes.InvalidArgument, "invalid ticket")
	}

	tenantID, _ := ticketData["tenant_id"].(string)
	dataset, _ := ticketData["dataset"].(string)

	if !s.registry.IsConnected(tenantID) {
		return status.Errorf(codes.Unavailable, "tenant %s not connected", tenantID)
	}

	log.Printf("[Flight-gRPC] DoGet: %s/%s", tenantID, dataset)

	// Obtener cliente Flight del conector
	client, _ := s.registry.GetClient(tenantID)

	// Crear descriptor para el dataset
	descriptor := &flight.FlightDescriptor{
		Type: flight.DescriptorPATH,
		Path: []string{dataset},
	}

	// Obtener FlightInfo
	ctx := stream.Context()
	info, err := client.GetFlightInfo(ctx, descriptor)
	if err != nil {
		return status.Errorf(codes.Internal, "get_flight_info failed: %v", err)
	}

	if len(info.Endpoint) == 0 {
		return status.Error(codes.NotFound, "no endpoints available")
	}

	// Obtener stream del conector
	connectorStream, err := client.DoGet(ctx, info.Endpoint[0].Ticket)
	if err != nil {
		return status.Errorf(codes.Internal, "do_get failed: %v", err)
	}

	// Crear reader
	reader, err := flight.NewRecordReader(connectorStream)
	if err != nil {
		return status.Errorf(codes.Internal, "record reader failed: %v", err)
	}
	defer reader.Release()

	// Reenviar datos al cliente (unified-evaluator)
	var totalBytes int64
	chunkCount := 0
	alloc := memory.NewGoAllocator()

	for reader.Next() {
		record := reader.Record()
		if record == nil {
			continue
		}

		// Serializar el record
		buf, err := serializeArrowRecord(record, alloc)
		if err != nil {
			log.Printf("[Flight-gRPC] Serialize error: %v", err)
			continue
		}

		// Enviar como FlightData
		flightData := &flight.FlightData{
			DataBody: buf,
		}

		if err := stream.Send(flightData); err != nil {
			log.Printf("[Flight-gRPC] Send error: %v", err)
			return err
		}

		totalBytes += int64(len(buf))
		chunkCount++
	}

	if err := reader.Err(); err != nil {
		log.Printf("[Flight-gRPC] Reader error: %v", err)
	}

	log.Printf("[Flight-gRPC] DoGet complete: %d bytes, %d chunks", totalBytes, chunkCount)
	return nil
}

// Start inicia el servidor Flight
func (s *FlightServerGRPC) Start() error {
	addr := fmt.Sprintf("0.0.0.0:%d", s.port)
	log.Printf("[Flight-gRPC] Server starting on %s", addr)

	server := flight.NewServerWithMiddleware(nil)
	server.RegisterFlightService(s)

	if err := server.Init(addr); err != nil {
		return err
	}

	return server.Serve()
}
