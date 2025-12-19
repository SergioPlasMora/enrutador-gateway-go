# Arquitectura de Producción: Data Streaming Platform

## Visión General

Esta plataforma permite el streaming de datos en tiempo real desde múltiples fuentes de datos distribuidas hasta clientes web, utilizando **Apache Arrow IPC** para transferencia binaria de alta eficiencia.

**Característica clave:** Los Data Connectors se conectan al Gateway mediante **túnel reverso** - el Connector inicia la conexión hacia el Gateway, eliminando la necesidad de puertos abiertos en el firewall del cliente.

```
┌─────────────────────────────────────────────────────────────────────────────┐
│                         ARQUITECTURA (Túnel Reverso)                        │
│                                                                             │
│  ┌─────────────┐    gRPC Bidireccional    ┌──────────────┐    WebSocket    │
│  │             │        (Producción)       │              │    Arrow IPC   │
│  │   Data      │◄────────────────────────▶│   Gateway    │◄──────────────▶│
│  │  Connector  │     :50051 (Protobuf)     │     (Go)     │  /stream/...   │
│  │  (Python)   │                           │              │    :8081       │
│  │             ├ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─┤              │                │
│  │             │    WebSocket (Alternativo)│              │                │
│  └──────┬──────┘       /ws/connect :8081   └──────┬───────┘                │
│         │                                         │                        │
│         │  Arrow IPC                              │  HTTP                  │
│         │  (datos binarios)                       │  (validación)          │
│         │                                         ▼                        │
│         │                                  ┌─────────────┐                 │
│         │                                  │Control Plane│                 │
│         │                                  │(luzzi-core) │                 │
│         │                                  └─────────────┘                 │
│         ▼                                                                  │
│  ┌─────────────────────────────────────────────────────────────────────┐  │
│  │      Datos locales: SQL Server, PostgreSQL, archivos CSV/JSON       │  │
│  └─────────────────────────────────────────────────────────────────────┘  │
└─────────────────────────────────────────────────────────────────────────────┘
```

---

## Modos de Transporte (Connector ↔ Gateway)

El túnel reverso entre Data Connector y Gateway soporta **dos modos de transporte**:

| Característica | gRPC (Producción) | WebSocket (Alternativo) |
|----------------|-------------------|-------------------------|
| **Puerto** | 50051 | 8081 |
| **Serialización** | Protobuf nativo | JSON + binario |
| **Rendimiento** | ⭐⭐⭐ Óptimo | ⭐⭐ Bueno |
| **HTTP/2** | ✅ Multiplexing | ❌ HTTP/1.1 |
| **Overhead** | Mínimo (binario) | Mayor (text + binary) |
| **Complejidad** | Requiere protoc | Simple |

### Recomendación

- **Producción:** gRPC con protobuf nativo
- **Desarrollo/Testing:** WebSocket (más simple de debuggear)

---

## Transporte gRPC (Producción)

### Arquitectura gRPC Bidireccional

```
┌─────────────────────────────────────────────────────────────────────────────┐
│                         gRPC BIDIRECCIONAL (Producción)                     │
│                                                                             │
│  ┌─────────────────┐                        ┌─────────────────┐            │
│  │  Data Connector │                        │     Gateway     │            │
│  │    (Python)     │                        │      (Go)       │            │
│  ├─────────────────┤                        ├─────────────────┤            │
│  │                 │                        │                 │            │
│  │  gRPC Client    │◀═══════════════════════▶  gRPC Server   │            │
│  │  (stub)         │   Bidirectional Stream │  (service)      │            │
│  │                 │        :50051          │                 │            │
│  │  connector_pb2  │                        │  proto/*.pb.go  │            │
│  │  connector_pb2  │                        │                 │            │
│  │  _grpc          │                        │                 │            │
│  │                 │                        │                 │            │
│  └─────────────────┘                        └─────────────────┘            │
│         │                                           │                      │
│         │ INICIA conexión                           │ ESCUCHA              │
│         ▼                                           │                      │
│  ┌─────────────────┐                                │                      │
│  │ NO requiere     │                                │                      │
│  │ puertos abiertos│                                │                      │
│  │ en firewall     │                                │                      │
│  └─────────────────┘                                │                      │
│                                                     │                      │
└─────────────────────────────────────────────────────────────────────────────┘
```

### Definición del Servicio (connector.proto)

```protobuf
syntax = "proto3";
package connector;

service ConnectorService {
  // Stream bidireccional: Connector envía respuestas, Gateway envía comandos
  rpc Connect(stream ConnectorMessage) returns (stream GatewayCommand) {}
}

message ConnectorMessage {
  string request_id = 1;
  oneof payload {
    RegisterRequest register = 2;
    FlightInfoResponse flight_info = 3;
    ArrowChunk arrow_chunk = 4;      // Bytes directos, NO base64
    StreamStatus stream_status = 5;
    HeartbeatResponse heartbeat = 6;
  }
}

message GatewayCommand {
  string request_id = 1;
  oneof command {
    RegisterResponse register_response = 2;
    GetFlightInfoRequest get_flight_info = 3;
    DoGetRequest do_get = 4;
    Heartbeat heartbeat = 5;
  }
}

message ArrowChunk {
  bytes data = 1;       // Arrow IPC bytes (binario directo)
  int32 partition = 2;
}
```

### Ventajas de Protobuf Nativo

| Aspecto | JSON (anterior) | Protobuf (actual) |
|---------|-----------------|-------------------|
| **Serialización Arrow** | base64 encode/decode | bytes directos |
| **Overhead** | ~33% extra por base64 | 0% |
| **Tipado** | Dinámico | Estático |
| **Parsing** | JSON.parse() | Código generado |
| **Tamaño mensaje** | Mayor | Menor |

---

## Transporte WebSocket (Alternativo)

Para desarrollo o entornos donde gRPC no es viable:

```
┌─────────────────────────────────────────────────────────────────────────────┐
│                         WEBSOCKET (Alternativo)                             │
│                                                                             │
│  ┌─────────────┐      WebSocket       ┌──────────────┐                     │
│  │   Data      │   Reverse Tunnel     │   Gateway    │                     │
│  │  Connector  │◄────────────────────▶│     (Go)     │                     │
│  │  (Python)   │    /ws/connect       │              │                     │
│  │             │      :8081           │              │                     │
│  └─────────────┘                      └──────────────┘                     │
│                                                                             │
│  Protocolo: JSON para control, Binary (TextMessage/BinaryMessage) para     │
│  datos Arrow IPC                                                           │
│                                                                             │
└─────────────────────────────────────────────────────────────────────────────┘
```

---

## Configuración de Modo de Transporte

### Gateway (config.yaml)

```yaml
# Modo de transporte para túnel reverso Connector↔Gateway
# Opciones: "grpc" (producción), "websocket" (alternativo), "both"
transport_mode: "grpc"

# Puerto gRPC (solo si transport_mode es "grpc" o "both")
grpc_port: 50051

# Puerto HTTP/WebSocket
http_port: 8081

# Timeouts
timeouts:
  query_seconds: 60
  connect_seconds: 10

# Clave secreta para validar tickets
tableros_secret_key: "your-secret-key"
```

### Data Connector (config.yml)

```yaml
# Conexión al Gateway
gateway:
  # Modo de transporte: "grpc" (producción) o "websocket" (alternativo)
  transport_mode: "grpc"
  
  # URI gRPC (para modo gRPC)
  grpc_uri: "gateway.example.com:50051"
  
  # URI WebSocket (para modo websocket)
  uri: "ws://gateway.example.com:8081/ws/connect"

# Identificación del tenant
tenant:
  id: "bf935f05-bf2e-4138-bfec-f4baaf99fecc"

# Rendimiento
performance:
  parallel_connections: 3
  parallel_partitions: true
  max_chunk_size: 65536
  reconnect_delay: 5
```

---

## Flujo de Datos (gRPC)

```
┌──────────────────────────────────────────────────────────────────────────────┐
│                         FLUJO DE DATOS (gRPC)                                │
├──────────────────────────────────────────────────────────────────────────────┤
│                                                                              │
│  REGISTRO: Connector establece túnel                                         │
│  ═════════════════════════════════════                                       │
│  1. Connector → Gateway: gRPC Connect() stream bidireccional (:50051)        │
│  2. Connector → Gateway: ConnectorMessage{register: {tenant_id, version}}    │
│  3. Gateway → Connector: GatewayCommand{register_response: {ok, session}}    │
│     └── Conexión permanece abierta, Connector espera comandos               │
│                                                                              │
│  QUERY: Browser solicita datos                                               │
│  ═════════════════════════════                                               │
│  4. Browser → Gateway: WebSocket connect /stream/{session_id} (:8081)        │
│  5. Gateway → Control Plane: Validar session_id                              │
│  6. Control Plane → Gateway: {user_id, cuenta_id, permisos}                  │
│  7. Browser → Gateway: {"action":"query","dataset":"ventas"}                 │
│  8. Gateway → Connector: GatewayCommand{get_flight_info: {path: ["ventas"]}} │
│  9. Connector → Gateway: ConnectorMessage{flight_info: {schema, records}}    │
│ 10. Gateway → Connector: GatewayCommand{do_get: {ticket: "..."}}             │
│ 11. Connector → Gateway: ConnectorMessage{arrow_chunk: {data: bytes}}        │
│     └── Múltiples chunks, datos Arrow IPC binarios directos                 │
│ 12. Connector → Gateway: ConnectorMessage{stream_status: {type: "end"}}      │
│ 13. Gateway → Browser: [Forward Arrow IPC chunks via WebSocket]              │
│                                                                              │
│  HEARTBEAT: Mantener conexión viva                                           │
│  ════════════════════════════════                                            │
│  14. Gateway → Connector: GatewayCommand{heartbeat: {timestamp}}             │
│  15. Connector → Gateway: ConnectorMessage{heartbeat: {tenant_id, ts}}       │
│                                                                              │
└──────────────────────────────────────────────────────────────────────────────┘
```

---

## Protocolos y Puertos

| Puerto | Protocolo | Dirección | Descripción |
|--------|-----------|-----------|-------------|
| **50051** | gRPC (HTTP/2) | Connector → Gateway | Túnel reverso producción, protobuf nativo |
| **8081** | WebSocket | Connector → Gateway | Túnel reverso alternativo (`/ws/connect`) |
| **8081** | WebSocket | Browser → Gateway | Stream de datos (`/stream/{session_id}`) |
| **8081** | HTTP | Gateway | Dashboard, health check |

> **Nota Importante:** El Connector SIEMPRE inicia la conexión hacia el Gateway (ya sea gRPC o WebSocket). No se requieren puertos abiertos en el firewall del cliente.

---

## Componentes

### 1. Data Connector (Python)

**Rol:** Fuente de datos distribuida. Se conecta al Gateway via gRPC o WebSocket.

**Archivos principales:**
| Archivo | Función |
|---------|---------|
| `service.py` | Punto de entrada, selección de transporte |
| `connector_grpc.py` | Cliente gRPC con protobuf nativo |
| `connector.py` | Cliente WebSocket (alternativo) |
| `data_loader.py` | Carga y serializa datos a Arrow IPC |
| `proto/connector_pb2.py` | Tipos protobuf generados |
| `proto/connector_pb2_grpc.py` | Stub gRPC generado |
| `config.yml` | Configuración |

### 2. Gateway (Go)

**Rol:** Router central. Conecta browsers con connectors. Valida sesiones.

**Archivos principales:**
| Archivo | Función |
|---------|---------|
| `main.go` | Punto de entrada, configuración |
| `connector_grpc.go` | Servidor gRPC para connectors |
| `connector_ws.go` | Servidor WebSocket para connectors |
| `connector_registry.go` | Gestiona conexiones de connectors |
| `stream_server_v2.go` | WebSocket handler para browsers |
| `proto/connector.pb.go` | Tipos protobuf generados |
| `proto/connector_grpc.pb.go` | Servicio gRPC generado |

### 3. Browser Client (JavaScript)

**Rol:** Dashboard web que visualiza datos en tiempo real.

- Conexión WebSocket al Gateway (`/stream/{session_id}`)
- Parsea Arrow IPC con `apache-arrow` (JS)
- Visualización con Chart.js

---

## Generación de Código Protobuf

### Requisitos

```bash
# Go plugins
go install google.golang.org/protobuf/cmd/protoc-gen-go@latest
go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@latest

# Python
pip install grpcio grpcio-tools protobuf
```

### Generar código Go

```bash
cd enrutador-gateway-go
protoc --go_out=. --go_opt=paths=source_relative \
       --go-grpc_out=. --go-grpc_opt=paths=source_relative \
       proto/connector.proto
```

### Generar código Python

```bash
cd data-conector
python -m grpc_tools.protoc -Iproto \
       --python_out=proto --grpc_python_out=proto \
       proto/connector.proto
```

---

## Despliegue

### Data Connector

```bash
# Instalar dependencias
pip install -r requirements.txt

# Ejecutar en modo test
python service.py --test

# Instalar como servicio Windows
python service.py install
python service.py start
```

### Gateway

```bash
# Actualizar dependencias (grpc >= v1.64.0 requerido)
go get google.golang.org/grpc@latest
go mod tidy

# Compilar
go build -o enrutador-gateway-go.exe .

# Ejecutar
./enrutador-gateway-go.exe
```

---

## Seguridad

### Validación de Sesiones

```
┌──────────────────────────────────────────────────────────────────────────────┐
│                         FLUJO DE SEGURIDAD                                   │
├──────────────────────────────────────────────────────────────────────────────┤
│                                                                              │
│  1. Usuario accede a Tableros en luzzi-core-im                               │
│  2. luzzi-core-im genera session_id firmado (JWT/HMAC)                       │
│  3. Browser recibe URL: wss://gateway:8081/stream/{session_id}               │
│  4. Browser conecta al Gateway                                               │
│  5. Gateway valida session_id con Control Plane (HTTP)                       │
│  6. Control Plane retorna: user_id, cuenta_id, edge_id, permisos             │
│  7. Gateway permite/rechaza la conexión                                      │
│  8. Redis pub/sub para revocación en tiempo real                             │
│                                                                              │
└──────────────────────────────────────────────────────────────────────────────┘
```

### Túnel Reverso Seguro

- El Connector **siempre inicia** la conexión
- No requiere puertos abiertos en el cliente
- Funciona detrás de NAT/Firewall corporativo
- Conexión persistente con heartbeat

---

## Resumen

| Componente | Tecnología | Función |
|------------|------------|---------|
| Data Connector | Python + PyArrow + gRPC | Fuente de datos, túnel reverso |
| Gateway | Go + gRPC + Gorilla WS | Router central, validación |
| Control Plane | luzzi-core-im (Django) | Autenticación, sesiones |
| Browser | HTML + JS + Apache Arrow JS | Visualización |
| Datos | Arrow IPC | Serialización binaria eficiente |
| Transporte (Prod) | **gRPC** (Protobuf) | Bidireccional, HTTP/2 |
| Transporte (Alt) | WebSocket | Browser-native |
