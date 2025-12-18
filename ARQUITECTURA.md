# Arquitectura de Producción: Data Streaming Platform

## Visión General

Esta plataforma permite el streaming de datos en tiempo real desde múltiples fuentes de datos distribuidas hasta clientes web, utilizando **Apache Arrow IPC** para transferencia binaria de alta eficiencia y **WebSocket** como transporte.

```
┌─────────────────────────────────────────────────────────────────────────────┐
│                    ARQUITECTURA ACTUAL (WebSocket)                          │
│                                                                             │
│   ┌─────────────┐      WebSocket       ┌──────────────┐      WebSocket     │
│   │             │    Reverse Tunnel    │              │     Arrow IPC      │
│   │   Data      │◄────────────────────▶│   Gateway    │◄──────────────────▶│
│   │  Connector  │    /ws/connect       │     (Go)     │  /stream/{session} │
│   │  (Python)   │      :8081           │              │       :8081        │
│   └─────────────┘                      └──────┬───────┘                    │
│         │                                     │                            │
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

### Nuestra Decisión: Arrow IPC + WebSocket

| Aspecto | Arrow Flight (gRPC) | Nuestra Solución (WebSocket + Arrow IPC) |
|---------|---------------------|------------------------------------------|
| **Transporte** | gRPC (HTTP/2) | WebSocket |
| **Datos** | Arrow IPC | Arrow IPC ✅ |
| **Puertos** | Servidor escucha (50051) | Reverse tunnel (cliente inicia) |
| **Browser support** | ❌ Requiere proxy | ✅ Nativo |
| **Firewall** | ⚠️ Puertos abiertos | ✅ Solo salientes |

---

## Evolución de la Arquitectura

### ANTES: Arquitectura gRPC (Arrow Flight)

```
┌─────────────────────────────────────────────────────────────────────────────┐
│                    ARQUITECTURA ANTERIOR (gRPC)                             │
│                                                                             │
│  ┌─────────────┐       gRPC           ┌──────────────┐      WebSocket      │
│  │             │    Arrow Flight      │              │      Arrow IPC      │
│  │   Data      │──────────────────────▶   Gateway    │────────────────────▶│
│  │  Connector  │      :50051          │     (Go)     │       :8080         │
│  │  (Python)   │                      │              │                     │
│  │             │   El Gateway         └──────────────┘                     │
│  │  SERVIDOR   │   INICIA la                                               │
│  │  (escucha)  │   conexión                                                │
│  └─────────────┘                                                           │
│                                                                            │
│  ❌ PROBLEMAS:                                                             │
│  • Requiere puerto 50051 abierto en firewall del cliente                   │
│  • El connector debe ser accesible desde internet                          │
│  • Configuración estática de IPs en el gateway                             │
│                                                                            │
└─────────────────────────────────────────────────────────────────────────────┘
```

**Flujo anterior:**
1. Gateway lee `config.yaml` con IP:Puerto de cada conector
2. Gateway abre conexión gRPC hacia el conector
3. Conector debe tener puerto expuesto públicamente
4. Browser → Gateway via WebSocket

### AHORA: Arquitectura WebSocket (Reverse Tunnel)

```
┌─────────────────────────────────────────────────────────────────────────────┐
│                    ARQUITECTURA ACTUAL (WebSocket)                          │
│                                                                             │
│  ┌─────────────┐      WebSocket        ┌──────────────┐      WebSocket     │
│  │             │    Reverse Tunnel     │              │      Arrow IPC     │
│  │   Data      │◄─────────────────────▶│   Gateway    │◄──────────────────▶│
│  │  Connector  │    /ws/connect        │     (Go)     │  /stream/{session} │
│  │  (Python)   │      :8081            │              │       :8081        │
│  │             │                       └──────────────┘                    │
│  │  CLIENTE    │   El Connector                                            │
│  │  (inicia)   │   INICIA la                                               │
│  └─────────────┘   conexión                                                │
│                                                                            │
│  ✅ VENTAJAS:                                                              │
│  • NO requiere puertos abiertos en el cliente                              │
│  • Connector puede estar detrás de NAT/Firewall                            │
│  • Registro dinámico (sin config.yaml)                                     │
│  • Browser-native (WebSocket nativo)                                       │
│                                                                            │
└─────────────────────────────────────────────────────────────────────────────┘
```

**Flujo actual:**
1. Connector inicia conexión WebSocket hacia el Gateway (`/ws/connect`)
2. Connector se registra con su `tenant_id`
3. Gateway almacena la conexión en `ConnectorRegistry`
4. Browser solicita datos → Gateway reenvía por el WebSocket existente
5. Connector responde con Arrow IPC bytes

---

## Componentes

### 1. Data Connector (Python)

**Rol:** Fuente de datos distribuida. Se conecta al Gateway via WebSocket.

**Características:**
- Cliente WebSocket hacia Gateway (`/ws/connect`)
- Lee archivos CSV, JSON, Parquet, bases de datos
- Convierte datos a Apache Arrow Tables
- Envía RecordBatches como bytes Arrow IPC

**Archivos principales:**
| Archivo | Función |
|---------|---------|
| `service.py` | Punto de entrada, Windows Service |
| `connector.py` | Lógica WebSocket, protocolo de mensajes |
| `data_loader.py` | Carga y serializa datos a Arrow IPC |
| `config.yml` | Configuración del conector |

**Protocolo de mensajes:**
```json
// Registro (connector → gateway)
{"action": "register", "tenant_id": "abc-123", "version": "1.0.0", "datasets": ["ventas"]}

// Respuesta (gateway → connector)
{"status": "ok", "data": {"session_id": "xyz"}}

// Query (gateway → connector)
{"action": "get_flight_info", "request_id": "req-1", "descriptor": {"path": ["ventas"]}}
{"action": "do_get", "request_id": "req-2", "ticket": "ventas"}

// Respuesta de datos (connector → gateway)
{"request_id": "req-2", "type": "stream_start", "schema": "base64..."}
[BINARY: Arrow IPC bytes]
[BINARY: Arrow IPC bytes]
{"request_id": "req-2", "type": "stream_end", "total_bytes": 1234567}
```

---

### 2. Gateway (Go)

**Rol:** Router central. Conecta browsers con connectors. Valida sesiones.

**Características:**
- Servidor WebSocket para connectors (`/ws/connect`)
- Servidor WebSocket para browsers (`/stream/{session_id}`)
- Validación de sesiones con Control Plane
- Multi-tenant: soporta múltiples connectors simultáneos

**Archivos principales:**
| Archivo | Función |
|---------|---------|
| `main.go` | Punto de entrada, configuración |
| `connector_registry.go` | Gestiona conexiones de connectors |
| `connector_ws.go` | WebSocket handler para connectors |
| `stream_server_v2.go` | WebSocket handler para browsers |
| `session_manager.go` | Gestión de sesiones con Control Plane |
| `redis_subscriber.go` | Revocación en tiempo real |
| `control_plane_client.go` | Validación con luzzi-core-im |

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
5. Browser solicita datos, Gateway reenvía al Connector
6. Browser recibe Arrow IPC, parsea y visualiza

---

## Flujo de Datos Completo

```
┌──────────────────────────────────────────────────────────────────────────────┐
│                         FLUJO DE DATOS (WebSocket)                           │
├──────────────────────────────────────────────────────────────────────────────┤
│                                                                              │
│  INICIO: Connector se registra                                               │
│  ═══════════════════════════════                                             │
│  1. Connector → Gateway: WebSocket connect /ws/connect                       │
│  2. Connector → Gateway: {"action":"register","tenant_id":"abc-123"}         │
│  3. Gateway → Connector: {"status":"ok","data":{"session_id":"xyz"}}         │
│     └── Connector queda registrado y esperando comandos                      │
│                                                                              │
│  QUERY: Browser solicita datos                                               │
│  ═════════════════════════════                                               │
│  4. Browser → Gateway: WebSocket connect /stream/{session_id}                │
│  5. Gateway → Control Plane: Validar session_id                              │
│  6. Control Plane → Gateway: {user_id, cuenta_id, permisos}                  │
│  7. Browser → Gateway: {"action":"query","dataset":"ventas"}                 │
│  8. Gateway → Connector: {"action":"get_flight_info","request_id":"r1",...}  │
│  9. Connector → Gateway: {"request_id":"r1","data":{schema,records}}         │
│ 10. Gateway → Connector: {"action":"do_get","request_id":"r2",...}           │
│ 11. Connector → Gateway: [Arrow IPC binary chunks]                           │
│ 12. Gateway → Browser: [Forward Arrow IPC chunks]                            │
│                                                                              │
│  FIN: Browser renderiza                                                      │
│  ═══════════════════════                                                     │
│ 13. Browser: tableFromIPC(bytes) → JavaScript Array → Chart.js               │
│                                                                              │
└──────────────────────────────────────────────────────────────────────────────┘
```

---

## Protocolos y Puertos

| Puerto | Protocolo | Dirección | Descripción |
|--------|-----------|-----------|-------------|
| **8081** | WebSocket | Connector → Gateway | Reverse tunnel, registro dinámico |
| **8081** | WebSocket | Browser → Gateway | Stream de datos (`/stream/{session_id}`) |
| **8081** | HTTP | Gateway | Dashboard, health check |

> **Nota:** Ya no se usa el puerto 50051 (gRPC). Todo pasa por WebSocket en puerto 8081.

---

## Formato de Datos: Apache Arrow IPC

**¿Por qué Arrow IPC?**
- Formato binario columnar eficiente
- Zero-copy cuando es posible
- Cross-language (Python → Go → JavaScript)
- Streaming nativo con RecordBatches
- Sin overhead de gRPC

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

Transferido como: websocket.BinaryMessage
```

---

## Seguridad

### Validación de Sesiones (CDP Edge Architecture)

```
┌──────────────────────────────────────────────────────────────────────────────┐
│                         FLUJO DE SEGURIDAD                                   │
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
# Conexión al Gateway
gateway:
  uri: "ws://gateway.example.com:8081/ws/connect"

# Identificación del tenant
tenant:
  id: "bf935f05-bf2e-4138-bfec-f4baaf99fecc"

# Rendimiento
performance:
  parallel_connections: 3  # Workers WebSocket simultáneos
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

# Ejecutar
./enrutador-gateway-go.exe
```

---

## Resumen

| Componente | Tecnología | Función |
|------------|------------|---------|
| Data Connector | Python + PyArrow + WebSocket | Fuente de datos, reverse tunnel |
| Gateway | Go + Gorilla WebSocket | Router central, validación |
| Control Plane | luzzi-core-im (Django) | Autenticación, sesiones |
| Browser | HTML + JS + Apache Arrow JS | Visualización |
| Datos | Arrow IPC | Serialización binaria eficiente |
| Transporte | WebSocket | Bidireccional, browser-native |
