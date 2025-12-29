# Arquitectura de Producción: Data Streaming Platform

## Visión General

Esta plataforma permite el streaming de datos en tiempo real desde múltiples fuentes de datos distribuidas hasta clientes web, utilizando **Apache Arrow IPC** para transferencia binaria de alta eficiencia, **gRPC** con **mTLS** para comunicación segura entre connectors y gateway, y **WebSocket** para browsers.

```
┌─────────────────────────────────────────────────────────────────────────────┐
│                    ARQUITECTURA ACTUAL (gRPC + mTLS)                        │
│                                                                             │
│   ┌─────────────┐     gRPC + mTLS      ┌──────────────┐      WebSocket     │
│   │             │   Bidirectional      │              │     Arrow IPC      │
│   │   Data      │◄────────────────────▶│   Gateway    │◄──────────────────▶│
│   │  Connector  │   Arrow IPC          │     (Go)     │  /stream/{session} │
│   │  (Python)   │      :50051          │              │       :8081        │
│   │  🔐 cert    │   🔐 mTLS auth       │  🔐 CA       │                    │
│   └─────────────┘                      └──────┬───────┘                    │
│         │                                     │                           │
│         │  Arrow IPC                          │  HTTP                      │
│         │  (datos binarios)                   │  (validación)              │
│         │                                     ▼                            │
│         │                              ┌─────────────┐                     │
│         │                              │Control Plane│                     │
│         │                              │(luzzi-core) │                     │
│         │                              └─────────────┘                     │
│         │                                                                  │
│         ▼                                                                  │
│  ┌─────────────────────────────────────────────────────────────┐          │
│  │  Datos locales: SQL Server, PostgreSQL, archivos CSV/JSON   │          │
│  └─────────────────────────────────────────────────────────────┘          │
└─────────────────────────────────────────────────────────────────────────────┘
```

---

## Conceptos Clave: Apache Arrow

### ¿Qué es Apache Arrow?

Apache Arrow es un proyecto de la Apache Software Foundation que define:
1. **Formato de datos columnar en memoria** - Cómo organizar datos de manera eficiente
2. **Arrow IPC** - Formato binario para serializar/deserializar datos Arrow
3. **Arrow Flight** - Protocolo de transporte basado en gRPC

### Arrow IPC vs Arrow Flight

```
┌─────────────────────────────────────────────────────────────────────────────┐
│                          APACHE ARROW (Proyecto)                            │
├─────────────────────────────────────────────────────────────────────────────┤
│                                                                             │
│  ┌─────────────────────────────┐    ┌─────────────────────────────────────┐│
│  │     ARROW IPC               │    │     ARROW FLIGHT                    ││
│  │     (Solo Formato)          │    │     (Protocolo Completo)            ││
│  ├─────────────────────────────┤    ├─────────────────────────────────────┤│
│  │                             │    │                                     ││
│  │  • Serialización binaria    │    │  • Transporte: gRPC (HTTP/2)        ││
│  │  • Formato columnar         │    │  • Datos: Arrow IPC                 ││
│  │  • Cross-language           │    │  • APIs: GetFlightInfo, DoGet       ││
│  │  • NO define transporte     │    │  • Requiere servidor gRPC           ││
│  │                             │    │                                     ││
│  │  TÚ ELIGES CÓMO             │    │  TODO INCLUIDO                      ││
│  │  TRANSPORTARLO              │    │  (pero menos flexible)              ││
│  │                             │    │                                     ││
│  └─────────────────────────────┘    └─────────────────────────────────────┘│
│           │                                      │                         │
│           ▼                                      ▼                         │
│   ┌───────────────────┐                 ┌───────────────────┐             │
│   │  Bytes binarios   │                 │  gRPC + Bytes     │             │
│   │  (tú transportas) │                 │  (acoplado)       │             │
│   └───────────────────┘                 └───────────────────┘             │
└─────────────────────────────────────────────────────────────────────────────┘
```

### Nuestra Decisión: Arrow IPC + gRPC + mTLS

| Aspecto | Arrow Flight (gRPC) | Nuestra Solución (gRPC + mTLS + Arrow IPC) |
|---------|---------------------|------------------------------------------|
| **Transporte** | gRPC (HTTP/2) | gRPC + mTLS (HTTP/2) |
| **Datos** | Arrow IPC | Arrow IPC ✅ |
| **Autenticación** | Requiere implementación | mTLS con certificados ✅ |
| **Browser support** | ❌ Requiere proxy | ✅ WebSocket para browsers |
| **Seguridad** | ⚠️ Opcional | ✅ mTLS obligatorio |

---

## Componentes

### 1. Data Connector (Python)

**Rol:** Fuente de datos distribuida. Se conecta al Gateway via gRPC con mTLS.

**Características:**
- Cliente gRPC bidireccional hacia Gateway (`:50051`)
- Autenticación mutua con certificados (mTLS)
- Lee archivos CSV, JSON, Parquet, bases de datos
- Convierte datos a Apache Arrow Tables
- Envía RecordBatches como bytes Arrow IPC via Protobuf

**Archivos principales:**

| Archivo | Función |
|---------|---------|
| `service.py` | Punto de entrada, Windows Service |
| `connector_grpc.py` | Cliente gRPC con mTLS, protocolo protobuf |
| `data_loader.py` | Carga y serializa datos a Arrow IPC |
| `config.yml` | Configuración del conector |
| `certs/` | Certificados mTLS (client.crt, client.key, ca.crt) |

**Protocolo de mensajes (Protobuf):**

```protobuf
// Registro (connector → gateway)
message RegisterRequest {
  string tenant_id = 1;
  string version = 2;
  repeated string datasets = 3;
}

// Query (gateway → connector)
message GetFlightInfoRequest {
  repeated string path = 1;
}

message DoGetRequest {
  string ticket = 1;
}

// Respuesta de datos (connector → gateway)
message ArrowChunk {
  bytes data = 1;  // Arrow IPC bytes
}
```

---

### 2. Gateway (Go)

**Rol:** Router central. Conecta browsers con connectors. Valida sesiones.

**Características:**
- Servidor gRPC con mTLS para connectors (`:50051`)
- Servidor WebSocket para browsers (`/stream/{session_id}`)
- Extrae `tenant_id` del certificado CN
- Validación de sesiones con Control Plane
- Multi-tenant: soporta múltiples connectors simultáneos

**Archivos principales:**

| Archivo | Función |
|---------|---------|
| `main.go` | Punto de entrada, configuración |
| `connector_registry.go` | Gestiona conexiones de connectors |
| `connector_grpc.go` | Servidor gRPC con mTLS para connectors |
| `stream_server_v2.go` | WebSocket handler para browsers |
| `session_manager.go` | Gestión de sesiones con Control Plane |
| `redis_subscriber.go` | Revocación en tiempo real |
| `certs/` | Certificados mTLS (server.crt, server.key, ca.crt) |

---

### 3. Browser Client (JavaScript)

**Rol:** Dashboard web que visualiza datos en tiempo real.

**Características:**
- Conexión WebSocket al Gateway (`/stream/{session_id}`)
- Parsea Arrow IPC con `apache-arrow` (JS)
- Visualización con Chart.js
- Session_id otorgado por luzzi-core-im

**Flujo:**
1. Usuario accede a Tableros desde luzzi-core-im
2. luzzi-core-im genera `session_id` firmado
3. Browser conecta a Gateway con `session_id`
4. Gateway valida con Control Plane
5. Browser solicita datos, Gateway reenvía al Connector via gRPC
6. Browser recibe Arrow IPC, parsea y visualiza

---

## Flujo de Datos Completo

```
┌──────────────────────────────────────────────────────────────────────────────┐
│                         FLUJO DE DATOS (gRPC + mTLS)                         │
├──────────────────────────────────────────────────────────────────────────────┤
│                                                                              │
│  INICIO: Connector se registra via gRPC con mTLS                             │
│  ════════════════════════════════════════════════                            │
│  1. Connector → Gateway: gRPC TLS handshake (mTLS)                           │
│  2. Gateway valida certificado, extrae tenant_id del CN                      │
│  3. Connector → Gateway: RegisterRequest{tenant_id, version, datasets}       │
│  4. Gateway → Connector: RegisterResponse{status:"ok", session_id}           │
│     └── Connector queda registrado y esperando comandos                      │
│                                                                              │
│  QUERY: Browser solicita datos                                               │
│  ═════════════════════════════                                               │
│  5. Browser → Gateway: WebSocket connect /stream/{session_id}                │
│  6. Gateway → Control Plane: Validar session_id                              │
│  7. Control Plane → Gateway: {user_id, cuenta_id, permisos}                  │
│  8. Browser → Gateway: {action:"query", dataset:"ventas"}                    │
│  9. Gateway → Connector: GetFlightInfoRequest{path:["ventas"]}               │
│ 10. Connector → Gateway: FlightInfoResponse{partitions, schema}              │
│ 11. Gateway → Connector: DoGetRequest{ticket:"ventas"}                       │
│ 12. Connector → Gateway: [ArrowChunk{data: bytes}...]                        │
│ 13. Gateway → Browser: [Forward Arrow IPC chunks via WebSocket]              │
│                                                                              │
│  FIN: Browser renderiza                                                      │
│  ═══════════════════════                                                     │
│ 14. Browser: tableFromIPC(bytes) → JavaScript Array → Chart.js               │
│                                                                              │
└──────────────────────────────────────────────────────────────────────────────┘
```

---

## Protocolos y Puertos

| Puerto | Protocolo | Dirección | Descripción |
|--------|-----------|-----------|-------------|
| **50051** | gRPC + mTLS | Connector → Gateway | Túnel bidireccional con autenticación mutua |
| **8081** | WebSocket | Browser → Gateway | Stream de datos (`/stream/{session_id}`) |
| **8081** | HTTP | Gateway | Dashboard, health check |

> **Nota:** La comunicación Connector ↔ Gateway usa gRPC con mTLS para máxima seguridad. Los browsers usan WebSocket.

---

## Formato de Datos: Apache Arrow IPC

**¿Por qué Arrow IPC?**
- Formato binario columnar eficiente
- Zero-copy cuando es posible
- Cross-language (Python → Go → JavaScript)
- Streaming nativo con RecordBatches

```
┌────────────────────────────────────────┐
│           Apache Arrow IPC             │
├────────────────────────────────────────┤
│ Schema (metadata en el primer chunk)   │
├────────────────────────────────────────┤
│ RecordBatch 1 (~64KB - 1MB)           │
├────────────────────────────────────────┤
│ RecordBatch 2                          │
├────────────────────────────────────────┤
│ RecordBatch N                          │
└────────────────────────────────────────┘

Transferido como: Protobuf ArrowChunk (bytes)
```

---

## Seguridad

### mTLS (Mutual TLS) - Autenticación de Connectors

La comunicación entre Data Connectors y Gateway está protegida con **mTLS (Mutual Transport Layer Security)**, que proporciona autenticación mutua criptográfica.

```
┌─────────────────────────────────────────────────────────────────────────────┐
│                         mTLS (Mutual TLS)                                   │
├─────────────────────────────────────────────────────────────────────────────┤
│                                                                             │
│  ┌──────────────┐                                    ┌──────────────┐       │
│  │   Data       │                                    │   Gateway    │       │
│  │  Connector   │                                    │    (Go)      │       │
│  │  (Python)    │                                    │              │       │
│  │              │                                    │  Tiene:      │       │
│  │  Tiene:      │                                    │  • server.crt│       │
│  │  • client.crt│                                    │  • server.key│       │
│  │  • client.key│                                    │  • ca.crt    │       │
│  │  • ca.crt    │                                    │              │       │
│  └──────┬───────┘                                    └───────┬──────┘       │
│         │                                                    │              │
│         │  1. TLS Handshake (gRPC secure_channel)            │              │
│         │─────────────────────────────────────────────────────▶             │
│         │                                                    │              │
│         │  2. Gateway presenta su certificado                │              │
│         │◀─────────────────────────────────────────────────────             │
│         │     📜 server.crt                                  │              │
│         │                                                    │              │
│         │  3. Connector valida: ✓ Firmado por CA            │              │
│         │                                                    │              │
│         │  4. Connector presenta su certificado              │              │
│         │─────────────────────────────────────────────────────▶             │
│         │     📜 client.crt (CN=tenant_id)                   │              │
│         │                                                    │              │
│         │  5. Gateway extrae tenant_id del CN               │              │
│         │     y valida que coincida con el registro          │              │
│         │                                                    │              │
│         │  6. Conexión mTLS establecida                      │              │
│         │◀════════════════════════════════════════════════════▶             │
│                                                                             │
└─────────────────────────────────────────────────────────────────────────────┘
```

#### Estructura de Certificados

```
certs/
├── ca.crt              # CA root certificate (compartido)
├── ca.key              # CA private key (¡PROTEGER!)
├── server.crt          # Gateway certificate
├── server.key          # Gateway private key
└── clients/
    └── {tenant_id}/
        ├── client.crt  # Connector certificate (CN=tenant_id)
        └── client.key  # Connector private key
```

#### Beneficios de mTLS

| Característica | Descripción |
|----------------|-------------|
| **Autenticación mutua** | Tanto cliente como servidor verifican identidad |
| **Identidad criptográfica** | tenant_id está en el CN del certificado |
| **No hay credenciales en tránsito** | Sin tokens, API keys, o passwords |
| **Revocación** | Revocar certificado = desconexión inmediata |
| **Auto-detección** | mTLS se activa si existen los certificados |

#### Generación de Certificados

```bash
cd certs/
./generate_certs.sh all {tenant_id}
```

---

### Validación de Sesiones (Browser → Gateway)

```
┌──────────────────────────────────────────────────────────────────────────────┐
│                         FLUJO DE SEGURIDAD (Browser)                         │
├──────────────────────────────────────────────────────────────────────────────┤
│                                                                              │
│  1. Usuario accede a Tableros en luzzi-core-im                               │
│  2. luzzi-core-im genera session_id firmado (JWT/HMAC)                       │
│  3. Browser recibe URL: wss://gateway/stream/{session_id}                    │
│  4. Browser conecta al Gateway                                               │
│  5. Gateway valida session_id con Control Plane (HTTP)                       │
│  6. Control Plane retorna: user_id, cuenta_id, edge_id, permisos             │
│  7. Gateway permite/rechaza la conexión                                      │
│  8. Redis pub/sub para revocación en tiempo real                             │
│                                                                              │
└──────────────────────────────────────────────────────────────────────────────┘
```

---

## Configuración

### Data Connector (`config.yml`)

```yaml
# Conexión al Gateway via gRPC
gateway:
  grpc_uri: "gateway.example.com:50051"
  transport_mode: "grpc"

# mTLS Certificates
mtls:
  ca_cert: "certs/ca.crt"
  client_cert: "certs/client.crt"
  client_key: "certs/client.key"

# Identificación del tenant
tenant:
  id: "bf935f05-bf2e-4138-bfec-f4baaf99fecc"

# Rendimiento
performance:
  max_chunk_size: 16384
  reconnect_delay: 5

# Logging
logging:
  level: "INFO"
```

### Gateway (`config.yaml`)

```yaml
# Puertos
http_port: 8081
grpc_port: 50051
transport_mode: "grpc"

# mTLS (auto-detecta si existen los certificados)
# certs/ca.crt, certs/server.crt, certs/server.key

# Timeouts
timeouts:
  query_seconds: 60
  connect_seconds: 10

# Clave secreta para validar tickets (misma que en luzzi-core-im)
tableros_secret_key: "your-secret-key"
```

---

## Despliegue

### Data Connector (cada ubicación de datos)

```bash
# Instalar dependencias
pip install -r requirements.txt

# Copiar certificados
cp /path/to/certs/* certs/

# Ejecutar en modo test
python service.py --test

# Instalar como servicio Windows
python service.py install
python service.py start
```

### Gateway (servidor central)

```bash
# Compilar
go build -o enrutador-gateway-go.exe .

# Asegurar que existen certificados
ls certs/ca.crt certs/server.crt certs/server.key

# Ejecutar
./enrutador-gateway-go.exe
```

---

## Resumen

| Componente | Tecnología | Función |
|------------|------------|---------|
| Data Connector | Python + PyArrow + gRPC + mTLS | Fuente de datos, reverse tunnel |
| Gateway | Go + gRPC + mTLS | Router central, validación, mTLS termination |
| Control Plane | luzzi-core-im (FastAPI + Jinja2) | Autenticación, sesiones |
| Browser | HTML + JS + Apache Arrow JS + WebSocket | Visualización |
| Datos | Arrow IPC | Serialización binaria eficiente |
| Transporte Connector | gRPC + mTLS | Bidireccional, autenticación mutua |
| Transporte Browser | WebSocket | Bidireccional, browser-native |
