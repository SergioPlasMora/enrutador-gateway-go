package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/structpb"
)

// ConnectorGRPCServer handles gRPC connections from Data Connectors (reverse tunnel mode)
type ConnectorGRPCServer struct {
	registry   *ConnectorRegistry
	grpcServer *grpc.Server
	port       int
}

// NewConnectorGRPCServer creates a new gRPC server for connector connections
func NewConnectorGRPCServer(registry *ConnectorRegistry, port int) *ConnectorGRPCServer {
	return &ConnectorGRPCServer{
		registry: registry,
		port:     port,
	}
}

// Start starts the gRPC server
func (s *ConnectorGRPCServer) Start() error {
	lis, err := net.Listen("tcp", fmt.Sprintf(":%d", s.port))
	if err != nil {
		return fmt.Errorf("failed to listen: %w", err)
	}

	s.grpcServer = grpc.NewServer()
	RegisterConnectorBidiServiceServer(s.grpcServer, s)

	log.Printf("[ConnectorGRPC] Starting gRPC server on :%d", s.port)
	return s.grpcServer.Serve(lis)
}

// Stop stops the gRPC server gracefully
func (s *ConnectorGRPCServer) Stop() {
	if s.grpcServer != nil {
		s.grpcServer.GracefulStop()
	}
}

// ============== gRPC Service Interface ==============

// ConnectorBidiServiceServer is the interface for the gRPC bidirectional service
type ConnectorBidiServiceServer interface {
	Connect(stream ConnectorBidiService_ConnectServer) error
}

// ConnectorBidiService_ConnectServer is the bidirectional stream interface
type ConnectorBidiService_ConnectServer interface {
	Send(*structpb.Struct) error
	Recv() (*structpb.Struct, error)
	grpc.ServerStream
}

// RegisterConnectorBidiServiceServer registers the service
func RegisterConnectorBidiServiceServer(s *grpc.Server, srv ConnectorBidiServiceServer) {
	s.RegisterService(&_ConnectorBidiService_serviceDesc, srv)
}

var _ConnectorBidiService_serviceDesc = grpc.ServiceDesc{
	ServiceName: "connector.ConnectorService",
	HandlerType: (*ConnectorBidiServiceServer)(nil),
	Methods:     []grpc.MethodDesc{},
	Streams: []grpc.StreamDesc{
		{
			StreamName:    "Connect",
			Handler:       _ConnectorBidiService_Connect_Handler,
			ServerStreams: true,
			ClientStreams: true,
		},
	},
	Metadata: "proto/connector.proto",
}

func _ConnectorBidiService_Connect_Handler(srv interface{}, stream grpc.ServerStream) error {
	return srv.(ConnectorBidiServiceServer).Connect(&connectorBidiServiceConnectServer{stream})
}

type connectorBidiServiceConnectServer struct {
	grpc.ServerStream
}

func (x *connectorBidiServiceConnectServer) Send(m *structpb.Struct) error {
	return x.ServerStream.SendMsg(m)
}

func (x *connectorBidiServiceConnectServer) Recv() (*structpb.Struct, error) {
	m := new(structpb.Struct)
	if err := x.ServerStream.RecvMsg(m); err != nil {
		return nil, err
	}
	return m, nil
}

// ============== Helper Functions for structpb conversion ==============

func structToMap(s *structpb.Struct) map[string]interface{} {
	if s == nil {
		return nil
	}
	return s.AsMap()
}

func mapToStruct(m map[string]interface{}) (*structpb.Struct, error) {
	return structpb.NewStruct(m)
}

// ============== Message Types ==============

type GRPCConnectorMsg struct {
	RequestID  string                 `json:"request_id"`
	Type       string                 `json:"type"`
	Register   *GRPCRegisterRequest   `json:"register,omitempty"`
	FlightInfo *GRPCFlightInfoResp    `json:"flight_info,omitempty"`
	ArrowChunk []byte                 `json:"arrow_chunk,omitempty"`
	Status     *GRPCStreamStatus      `json:"status,omitempty"`
	Heartbeat  *GRPCHeartbeatResp     `json:"heartbeat,omitempty"`
	Data       map[string]interface{} `json:"data,omitempty"`
}

type GRPCGatewayCommand struct {
	RequestID     string                `json:"request_id"`
	Type          string                `json:"type"`
	RegisterResp  *GRPCRegisterResponse `json:"register_response,omitempty"`
	GetFlightInfo *GRPCGetFlightInfoReq `json:"get_flight_info,omitempty"`
	DoGet         *GRPCDoGetRequest     `json:"do_get,omitempty"`
	Heartbeat     *GRPCHeartbeat        `json:"heartbeat,omitempty"`
}

type GRPCRegisterRequest struct {
	TenantID string   `json:"tenant_id"`
	Version  string   `json:"version"`
	Datasets []string `json:"datasets"`
}

type GRPCRegisterResponse struct {
	Status    string `json:"status"`
	SessionID string `json:"session_id"`
	Error     string `json:"error,omitempty"`
}

type GRPCGetFlightInfoReq struct {
	Path []string `json:"path"`
	Rows int64    `json:"rows,omitempty"`
}

type GRPCFlightInfoResp struct {
	Status       string `json:"status"`
	Schema       []byte `json:"schema"`
	TotalRecords int64  `json:"total_records"`
	TotalBytes   int64  `json:"total_bytes"`
	Dataset      string `json:"dataset"`
	Partitions   int    `json:"partitions"`
	Error        string `json:"error,omitempty"`
}

type GRPCDoGetRequest struct {
	Ticket string `json:"ticket"`
}

type GRPCStreamStatus struct {
	Type            string `json:"type"`
	Schema          []byte `json:"schema,omitempty"`
	Partition       int    `json:"partition"`
	TotalPartitions int    `json:"total_partitions"`
	TotalBytes      int64  `json:"total_bytes"`
	Error           string `json:"error,omitempty"`
}

type GRPCHeartbeat struct {
	Timestamp int64 `json:"timestamp"`
}

type GRPCHeartbeatResp struct {
	TenantID  string `json:"tenant_id"`
	Timestamp int64  `json:"timestamp"`
}

// ============== gRPC Client Wrapper ==============

// GRPCConnectorClient wraps a gRPC stream to act as a connector client
type GRPCConnectorClient struct {
	stream    ConnectorBidiService_ConnectServer
	tenantID  string
	sessionID string
	pending   map[string]chan map[string]interface{}
	pendingMu sync.RWMutex
	chunks    map[string]chan []byte
	chunksMu  sync.RWMutex
	writeMu   sync.Mutex
}

// NewGRPCConnectorClient creates a new gRPC-based connector client
func NewGRPCConnectorClient(stream ConnectorBidiService_ConnectServer, tenantID, sessionID string) *GRPCConnectorClient {
	return &GRPCConnectorClient{
		stream:    stream,
		tenantID:  tenantID,
		sessionID: sessionID,
		pending:   make(map[string]chan map[string]interface{}),
		chunks:    make(map[string]chan []byte),
	}
}

// ============== Connect Handler ==============

// Connect handles the bidirectional stream using structpb.Struct
func (s *ConnectorGRPCServer) Connect(stream ConnectorBidiService_ConnectServer) error {
	// Wait for registration message
	msgStruct, err := stream.Recv()
	if err != nil {
		log.Printf("[ConnectorGRPC] Recv error: %v", err)
		return err
	}

	msg := structToMap(msgStruct)
	msgType, _ := msg["type"].(string)
	register, _ := msg["register"].(map[string]interface{})
	tenantID := ""
	if register != nil {
		tenantID, _ = register["tenant_id"].(string)
	}

	if msgType != "register" || tenantID == "" {
		log.Printf("[ConnectorGRPC] Invalid registration: %+v", msg)
		resp, _ := mapToStruct(map[string]interface{}{
			"type": "register_response",
			"register_response": map[string]interface{}{
				"status": "error",
				"error":  "invalid registration",
			},
		})
		stream.Send(resp)
		return fmt.Errorf("invalid registration")
	}

	sessionID := base64.RawURLEncoding.EncodeToString([]byte(time.Now().Format("20060102150405")))

	// Create client wrapper
	client := NewGRPCConnectorClient(stream, tenantID, sessionID)

	// Register with registry
	s.registry.RegisterGRPCConnector(tenantID, client)
	defer s.registry.UnregisterGRPCConnector(tenantID)

	// Send confirmation
	resp, _ := mapToStruct(map[string]interface{}{
		"type": "register_response",
		"register_response": map[string]interface{}{
			"status":     "ok",
			"session_id": sessionID,
		},
	})
	stream.Send(resp)

	version := ""
	if register != nil {
		version, _ = register["version"].(string)
	}
	log.Printf("[ConnectorGRPC] Registered: tenant=%s session=%s version=%s", tenantID, sessionID, version)

	// Start heartbeat goroutine
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go s.keepalive(ctx, stream, tenantID)

	// Main message loop - receive messages from connector
	for {
		msgStruct, err := stream.Recv()
		if err == io.EOF {
			log.Printf("[ConnectorGRPC] Stream closed: tenant=%s", tenantID)
			return nil
		}
		if err != nil {
			log.Printf("[ConnectorGRPC] Recv error: tenant=%s err=%v", tenantID, err)
			return err
		}

		msg := structToMap(msgStruct)
		client.handleMessage(msg)
	}
}

// handleMessage routes messages from the connector
func (c *GRPCConnectorClient) handleMessage(msg map[string]interface{}) {
	msgType, _ := msg["type"].(string)
	requestID, _ := msg["request_id"].(string)

	switch msgType {
	case "flight_info":
		c.handleFlightInfo(requestID, msg)
	case "arrow_chunk":
		c.handleArrowChunk(requestID, msg)
	case "stream_status":
		c.handleStreamStatus(requestID, msg)
	case "heartbeat":
		// Ignore heartbeat responses
	default:
		log.Printf("[ConnectorGRPC] Unknown message type: %s", msgType)
	}
}

func (c *GRPCConnectorClient) handleFlightInfo(requestID string, msg map[string]interface{}) {
	c.pendingMu.RLock()
	ch, exists := c.pending[requestID]
	c.pendingMu.RUnlock()

	if exists {
		select {
		case ch <- msg:
		default:
		}
	}
}

func (c *GRPCConnectorClient) handleArrowChunk(requestID string, msg map[string]interface{}) {
	c.chunksMu.RLock()
	ch, exists := c.chunks[requestID]
	c.chunksMu.RUnlock()

	if exists {
		// Decode base64 arrow chunk
		chunkB64, _ := msg["arrow_chunk"].(string)
		chunk, err := base64.StdEncoding.DecodeString(chunkB64)
		if err == nil {
			select {
			case ch <- chunk:
			default:
				log.Printf("[ConnectorGRPC] Channel full for request %s", requestID)
			}
		}
	}
}

func (c *GRPCConnectorClient) handleStreamStatus(requestID string, msg map[string]interface{}) {
	status, _ := msg["status"].(map[string]interface{})
	if status == nil {
		return
	}

	statusType, _ := status["type"].(string)
	if statusType == "stream_end" {
		c.chunksMu.Lock()
		if ch, exists := c.chunks[requestID]; exists {
			close(ch)
			delete(c.chunks, requestID)
		}
		c.chunksMu.Unlock()
	}

	// Also route to pending
	c.pendingMu.RLock()
	ch, exists := c.pending[requestID]
	c.pendingMu.RUnlock()

	if exists {
		select {
		case ch <- msg:
		default:
		}
	}
}

// SendCommand sends a command to the connector
func (c *GRPCConnectorClient) SendCommand(ctx context.Context, cmd map[string]interface{}) (map[string]interface{}, error) {
	requestID, _ := cmd["request_id"].(string)

	respCh := make(chan map[string]interface{}, 1)
	c.pendingMu.Lock()
	c.pending[requestID] = respCh
	c.pendingMu.Unlock()

	defer func() {
		c.pendingMu.Lock()
		delete(c.pending, requestID)
		c.pendingMu.Unlock()
	}()

	cmdStruct, err := mapToStruct(cmd)
	if err != nil {
		return nil, err
	}

	c.writeMu.Lock()
	err = c.stream.Send(cmdStruct)
	c.writeMu.Unlock()

	if err != nil {
		return nil, err
	}

	select {
	case resp := <-respCh:
		return resp, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// keepalive sends periodic heartbeats
func (s *ConnectorGRPCServer) keepalive(ctx context.Context, stream ConnectorBidiService_ConnectServer, tenantID string) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			hb, _ := mapToStruct(map[string]interface{}{
				"type": "heartbeat",
				"heartbeat": map[string]interface{}{
					"timestamp": time.Now().Unix(),
				},
			})
			if err := stream.Send(hb); err != nil {
				log.Printf("[ConnectorGRPC] Heartbeat failed for %s: %v", tenantID, err)
				return
			}
		}
	}
}

// Dummy types for JSON marshaling (not used with structpb approach)
func (m *GRPCConnectorMsg) Marshal() ([]byte, error)   { return json.Marshal(m) }
func (m *GRPCConnectorMsg) Unmarshal(d []byte) error   { return json.Unmarshal(d, m) }
func (m *GRPCGatewayCommand) Marshal() ([]byte, error) { return json.Marshal(m) }
func (m *GRPCGatewayCommand) Unmarshal(d []byte) error { return json.Unmarshal(d, m) }
