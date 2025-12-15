# Arquitectura de Producción: Data Streaming Platform

## Visión General

Esta plataforma permite el streaming de datos en tiempo real desde múltiples fuentes de datos distribuidas hasta clientes web y CLI, utilizando Apache Arrow para transferencia binaria de alta eficiencia.

```
┌─────────────────────────────────────────────────────────────────────────────┐
│                           ARQUITECTURA                                      │
│                                                                             │
│   ┌─────────────┐       gRPC        ┌──────────────┐      WebSocket        │
│   │             │    Arrow Flight   │              │      Arrow IPC        │
│   │   Data      │───────────────────▶   Gateway    │─────────────────────▶ │
│   │  Connector  │      :50051       │     (Go)     │       :8080           │
│   │  (Python)   │                   │              │                       │
│   └─────────────┘                   └──────┬───────┘                       │
│                                            │                               │
│                                            │ gRPC                          │
│                                            │ Arrow Flight                  │
│                                            │ :8815                         │
│                                            ▼                               │
│                                     ┌─────────────┐                        │
│                                     │ CLI Clients │                        │
│                                     │  (Python)   │                        │
│                                     └─────────────┘                        │
└─────────────────────────────────────────────────────────────────────────────┘
```

---

## Componentes

### 1. Data Connector (Python)

**Rol:** Fuente de datos distribuida que lee archivos locales y los expone via Arrow Flight.

**Características:**
- Servidor Arrow Flight (gRPC) en puerto **50051**
- Lee archivos CSV, JSON, Parquet
- Convierte datos a Apache Arrow Tables
- Envía RecordBatches en streaming

**Archivo principal:** `flight_server.py`

```python
class DataConnectorFlightServer(flight.FlightServerBase):
    def get_flight_info(self, context, descriptor):
        # Carga el dataset y retorna metadata
        
    def do_get(self, context, ticket):
        # Retorna stream de RecordBatches
```

**Endpoints:**
| Método | Descripción |
|--------|-------------|
| `GetFlightInfo` | Carga dataset, retorna schema y metadata |
| `DoGet` | Retorna stream de Arrow RecordBatches |

---

### 2. Gateway (Go)

**Rol:** Router central que conecta Data Connectors con clientes. No procesa datos, solo los reenvía.

**Características:**
- Cliente gRPC hacia Data Connectors (puerto **50051**)
- Servidor WebSocket para navegadores (puerto **8080**)
- Servidor Arrow Flight para CLI (puerto **8815**)
- Multi-tenant: soporta múltiples conectores simultáneos

**Archivos principales:**

| Archivo | Función |
|---------|---------|
| `main.go` | Punto de entrada, configuración de modos |
| `connector_registry.go` | Gestiona conexiones gRPC a conectores |
| `browser_ws_grpc.go` | WebSocket para browsers (modo gRPC) |
| `flight_grpc.go` | Flight Server para CLI clients |
| `config.yaml` | Configuración de conectores |

**Modos de operación:**

```yaml
# config.yaml
connector_mode: grpc  # o "websocket"

connectors:
  tenant_sergio: "localhost:50051"
  tenant_empresa_a: "10.0.0.5:50051"
```

---

### 3. Browser Client (JavaScript)

**Rol:** Dashboard web que visualiza datos en tiempo real.

**Características:**
- Conexión WebSocket al Gateway
- Parsea Arrow IPC con `apache-arrow` (JS)
- Visualización con Chart.js
- Selector de datasets

**Flujo:**
1. Usuario selecciona tenant y dataset
2. Browser envía JSON: `{"action": "query", "tenant": "...", "dataset": "..."}`
3. Gateway obtiene datos del conector via gRPC
4. Gateway reenvía chunks binarios via WebSocket
5. Browser parsea Arrow IPC y renderiza charts

---

### 4. CLI Clients (Python)

**Rol:** Clientes programáticos para testing, integración, y análisis.

**Ejemplo:** `unified-evaluator`

```python
import pyarrow.flight as flight

client = flight.connect("grpc://localhost:8815")
info = client.get_flight_info(flight.FlightDescriptor.for_path("tenant", "dataset"))
reader = client.do_get(info.endpoints[0].ticket)

for batch in reader:
    process(batch.data)
```

---

## Flujo de Datos

```
┌──────────────────────────────────────────────────────────────────────────────┐
│                            FLUJO DE DATOS                                    │
├──────────────────────────────────────────────────────────────────────────────┤
│                                                                              │
│  1. Browser solicita datos                                                   │
│     ──────────────────────────▶                                              │
│     WebSocket JSON: {"action": "query", "dataset": "ventas"}                 │
│                                                                              │
│  2. Gateway consulta al Conector                                             │
│     ──────────────────────────▶                                              │
│     gRPC: GetFlightInfo(descriptor) → carga dataset                          │
│     gRPC: DoGet(ticket) → stream de RecordBatches                            │
│                                                                              │
│  3. Conector lee y serializa                                                 │
│     CSV/JSON/Parquet → PyArrow Table → Arrow IPC → gRPC stream               │
│                                                                              │
│  4. Gateway reenvía al Browser                                               │
│     ◀──────────────────────────                                              │
│     gRPC RecordBatch → serialize IPC → WebSocket Binary                      │
│                                                                              │
│  5. Browser parsea y visualiza                                               │
│     Arrow IPC → tableFromIPC() → JavaScript Object → Chart.js                │
│                                                                              │
└──────────────────────────────────────────────────────────────────────────────┘
```

---

## Protocolos y Puertos

| Puerto | Protocolo | Desde | Hacia | Descripción |
|--------|-----------|-------|-------|-------------|
| **50051** | gRPC (Arrow Flight) | Gateway | Data Connector | Obtener datos |
| **8080** | WebSocket + HTTP | Browser | Gateway | Dashboard web |
| **8815** | gRPC (Arrow Flight) | CLI | Gateway | Clientes programáticos |

---

## Formato de Datos: Apache Arrow IPC

**¿Por qué Arrow?**
- Formato binario columnar eficiente
- Zero-copy cuando es posible
- Cross-language (Python → Go → JavaScript)
- Streaming nativo con RecordBatches

```
┌────────────────────────────────────────┐
│           Apache Arrow IPC             │
├────────────────────────────────────────┤
│ Schema (metadata)                      │
├────────────────────────────────────────┤
│ RecordBatch 1 (~64KB - 4MB)           │
├────────────────────────────────────────┤
│ RecordBatch 2                          │
├────────────────────────────────────────┤
│ RecordBatch N                          │
└────────────────────────────────────────┘
```

---

## Configuración Multi-Tenant

Cada tenant tiene su propio Data Connector:

```yaml
# Gateway config.yaml
connectors:
  empresa_a: "192.168.1.10:50051"
  empresa_b: "192.168.1.20:50051"
  empresa_c: "10.0.0.5:50051"
```

El Gateway rutea automáticamente las queries al conector correcto basándose en el `tenant_id`.

---

## Rendimiento

| Métrica | Valor |
|---------|-------|
| Throughput | ~113 MB/s |
| Latencia (100MB) | ~0.88s |
| Concurrencia | Soporta 100+ requests simultáneos |
| Memoria Gateway | Mínima (solo pasa bytes) |

---

## Despliegue

### Data Connector (cada ubicación)

```bash
# Instalar dependencias
pip install pyarrow

# Iniciar servidor
python flight_server.py --host 0.0.0.0 --port 50051
```

### Gateway (servidor central)

```bash
# Compilar
go build -o gateway .

# Configurar config.yaml con conectores
# Ejecutar
./gateway
```

### Como servicios de Windows

```bash
# Data Connector
nssm install DataConnector "python" "C:\path\to\flight_server.py --port 50051"

# Gateway
nssm install Gateway "C:\path\to\gateway.exe"
```

---

## Seguridad (Producción)

Para producción, agregar:

1. **TLS/SSL** en gRPC y WebSocket
2. **Autenticación** por tenant
3. **Firewall** - solo permitir conexiones conocidas
4. **Rate limiting** en el Gateway

---

## Resumen

| Componente | Tecnología | Función |
|------------|------------|---------|
| Data Connector | Python + PyArrow | Fuente de datos, Flight Server |
| Gateway | Go + Arrow Flight | Router central, no procesa datos |
| Dashboard | HTML + JS + Chart.js | Visualización web |
| CLI | Python + PyArrow | Testing y automatización |
