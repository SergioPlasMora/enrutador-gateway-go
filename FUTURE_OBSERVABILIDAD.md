# 🔭 Plan de Observabilidad: Data Platform

## Visión General

Este documento describe la estrategia de observabilidad unificada para todos los agentes que corren en máquinas de clientes (Data Connector, Data Agent) y cómo centralizar sus métricas en luzzi-core-im.

---

## 🎯 Objetivo

Crear un sistema de métricas centralizado donde:
- **Data Connector** (streaming de datos) envíe métricas
- **Data Agent** (ETL con Prefect) envíe métricas
- **luzzi-core-im** reciba, almacene y exponga métricas para Prometheus
- **Grafana** visualice todo en dashboards unificados

```
┌─────────────────────────────────────────────────────────────────────────────┐
│                    MÁQUINA DEL CLIENTE                                       │
├─────────────────────────────────────────────────────────────────────────────┤
│                                                                              │
│  ┌─────────────────┐         ┌─────────────────┐                            │
│  │  Data Connector │         │   Data Agent    │                            │
│  │   (Streaming)   │         │   (ETL/Prefect) │                            │
│  │                 │         │                 │                            │
│  │  Métricas:      │         │  Métricas:      │                            │
│  │  • bytes_sent   │         │  • flows_run    │                            │
│  │  • queries      │         │  • tasks_ok     │                            │
│  │  • errors       │         │  • tasks_failed │                            │
│  └────────┬────────┘         └────────┬────────┘                            │
│           │                           │                                      │
│           └─────────┬─────────────────┘                                      │
│                     │  HTTP POST                                             │
│                     ▼                                                        │
└─────────────────────┼────────────────────────────────────────────────────────┘
                      │
                      │  INTERNET
                      ▼
┌─────────────────────────────────────────────────────────────────────────────┐
│                    SERVIDOR EN LA NUBE                                       │
├─────────────────────────────────────────────────────────────────────────────┤
│                                                                              │
│  ┌─────────────────────────────────────────┐                                │
│  │            luzzi-core-im                 │                                │
│  │                                          │                                │
│  │  POST /api/metrics/agent/{tenant_id}     │ ◄── Recibe de todos           │
│  │                                          │                                │
│  │  GET /metrics/agents (Prometheus format) │ ◄── Expone para Prometheus    │
│  │                                          │                                │
│  │  Dashboard UI: Estado de todos los       │                                │
│  │  connectors y agents por tenant          │                                │
│  └─────────────────┬───────────────────────┘                                │
│                    │                                                         │
│                    ▼                                                         │
│  ┌─────────────────────────────────────────┐                                │
│  │            Prometheus                    │   ──▶  Grafana                │
│  │  scrape: /metrics/agents                 │                                │
│  └─────────────────────────────────────────┘                                │
│                                                                              │
└─────────────────────────────────────────────────────────────────────────────┘
```

---

## 📊 Métricas por Tipo de Agente

### Data Connector (Streaming)

| Métrica | Tipo | Descripción |
|---------|------|-------------|
| `connector_bytes_sent_total` | Counter | Total de bytes enviados |
| `connector_records_sent_total` | Counter | Total de registros enviados |
| `connector_queries_processed_total` | Counter | Queries procesados |
| `connector_errors_total` | Counter | Errores totales |
| `connector_uptime_seconds` | Gauge | Tiempo activo |
| `connector_connected` | Gauge | 1=conectado, 0=desconectado |
| `connector_last_seen_seconds` | Gauge | Segundos desde última actualización |
| `connector_certificate_expiry_days` | Gauge | Días hasta expiración del cert |

### Data Agent (ETL/Prefect)

| Métrica | Tipo | Descripción |
|---------|------|-------------|
| `agent_flows_executed_total` | Counter | Flows ejecutados |
| `agent_flows_success_total` | Counter | Flows exitosos |
| `agent_flows_failed_total` | Counter | Flows fallidos |
| `agent_tasks_executed_total` | Counter | Tasks ejecutados |
| `agent_last_flow_duration_seconds` | Gauge | Duración del último flow |
| `agent_uptime_seconds` | Gauge | Tiempo activo |
| `agent_connected` | Gauge | 1=conectado, 0=desconectado |

---

## 🛠️ Plan de Implementación

### Fase 1: Endpoint en luzzi-core-im

**Archivos a crear/modificar:**

#### 1.1 Modelos de Métricas

```python
# backend/src/api/schemas/agent_metrics.py
from pydantic import BaseModel
from typing import Optional, Literal
from enum import Enum

class AgentType(str, Enum):
    CONNECTOR = "connector"
    ETL = "etl"

class AgentMetrics(BaseModel):
    agent_type: AgentType
    version: str
    uptime_seconds: int
    connected: bool
    errors_total: int
    
    # Connector específicas
    bytes_sent_total: Optional[int] = None
    records_sent_total: Optional[int] = None
    queries_processed: Optional[int] = None
    certificate_expiry_days: Optional[int] = None
    
    # Data Agent específicas
    flows_executed_total: Optional[int] = None
    flows_success_total: Optional[int] = None
    flows_failed_total: Optional[int] = None
    tasks_executed_total: Optional[int] = None
    last_flow_duration_seconds: Optional[float] = None
    last_flow_name: Optional[str] = None
```

#### 1.2 API Router

```python
# backend/src/api/agent_metrics.py
from fastapi import APIRouter, Response
from typing import Dict
import time

router = APIRouter(prefix="/api/metrics", tags=["Agent Metrics"])

# Almacenamiento en memoria (Redis en producción)
agent_metrics: Dict[str, dict] = {}

@router.post("/agent/{tenant_id}")
async def receive_agent_metrics(tenant_id: str, metrics: AgentMetrics):
    """Recibe métricas de cualquier agente"""
    key = f"{tenant_id}:{metrics.agent_type}"
    agent_metrics[key] = {
        **metrics.dict(),
        "last_seen": time.time(),
        "tenant_id": tenant_id
    }
    return {"status": "ok"}

@router.get("/prometheus/agents", response_class=Response)
async def prometheus_metrics():
    """Endpoint que Prometheus scrapea"""
    lines = []
    lines.append("# HELP connector_bytes_sent_total Total bytes sent by connector")
    lines.append("# TYPE connector_bytes_sent_total counter")
    
    for key, m in agent_metrics.items():
        tenant_id = m["tenant_id"]
        agent_type = m["agent_type"]
        labels = f'tenant_id="{tenant_id}",agent_type="{agent_type}"'
        
        if agent_type == "connector":
            if m.get("bytes_sent_total"):
                lines.append(f'connector_bytes_sent_total{{{labels}}} {m["bytes_sent_total"]}')
            if m.get("records_sent_total"):
                lines.append(f'connector_records_sent_total{{{labels}}} {m["records_sent_total"]}')
            if m.get("queries_processed"):
                lines.append(f'connector_queries_processed_total{{{labels}}} {m["queries_processed"]}')
        
        elif agent_type == "etl":
            if m.get("flows_executed_total"):
                lines.append(f'agent_flows_executed_total{{{labels}}} {m["flows_executed_total"]}')
            if m.get("flows_success_total"):
                lines.append(f'agent_flows_success_total{{{labels}}} {m["flows_success_total"]}')
            if m.get("flows_failed_total"):
                lines.append(f'agent_flows_failed_total{{{labels}}} {m["flows_failed_total"]}')
        
        # Métricas comunes
        lines.append(f'agent_uptime_seconds{{{labels}}} {m["uptime_seconds"]}')
        lines.append(f'agent_connected{{{labels}}} {1 if m["connected"] else 0}')
        lines.append(f'agent_errors_total{{{labels}}} {m["errors_total"]}')
        
        age = time.time() - m["last_seen"]
        lines.append(f'agent_last_seen_seconds{{{labels}}} {age:.0f}')
    
    return Response(content="\n".join(lines), media_type="text/plain")
```

#### 1.3 Registrar Router en main.py

```python
# En main.py o routes.py
from src.api.agent_metrics import router as agent_metrics_router
app.include_router(agent_metrics_router)
```

---

### Fase 2: Cliente de Métricas en Data Connector

**Archivo:** `data-conector/metrics_reporter.py`

```python
import aiohttp
import asyncio
import time
import logging

logger = logging.getLogger(__name__)

class MetricsReporter:
    def __init__(self, api_url: str, tenant_id: str):
        self.api_url = f"{api_url}/api/metrics/agent/{tenant_id}"
        self.tenant_id = tenant_id
        self.start_time = time.time()
        
        # Contadores
        self.bytes_sent = 0
        self.records_sent = 0
        self.queries_processed = 0
        self.errors = 0
        
        self._running = False
    
    def record_bytes_sent(self, count: int):
        self.bytes_sent += count
    
    def record_records_sent(self, count: int):
        self.records_sent += count
    
    def record_query_processed(self):
        self.queries_processed += 1
    
    def record_error(self):
        self.errors += 1
    
    async def start(self, interval: int = 30):
        """Inicia el loop de reporte de métricas"""
        self._running = True
        while self._running:
            await self._send_metrics()
            await asyncio.sleep(interval)
    
    def stop(self):
        self._running = False
    
    async def _send_metrics(self):
        metrics = {
            "agent_type": "connector",
            "version": "1.0.0",
            "uptime_seconds": int(time.time() - self.start_time),
            "connected": True,
            "errors_total": self.errors,
            "bytes_sent_total": self.bytes_sent,
            "records_sent_total": self.records_sent,
            "queries_processed": self.queries_processed,
        }
        
        try:
            async with aiohttp.ClientSession() as session:
                async with session.post(self.api_url, json=metrics, timeout=10) as resp:
                    if resp.status != 200:
                        logger.warning(f"Failed to send metrics: {resp.status}")
        except Exception as e:
            logger.warning(f"Metrics send error: {e}")
```

**Integración en `connector_grpc.py`:**

```python
# En GRPCConnector.__init__
self.metrics = MetricsReporter(
    api_url="https://api.luzzi.com",
    tenant_id=self.tenant_id
)

# En run()
asyncio.create_task(self.metrics.start(interval=30))

# Cuando envías datos
self.metrics.record_bytes_sent(len(chunk_data))
self.metrics.record_records_sent(batch.num_rows)
```

---

### Fase 3: Configuración Prometheus

**Archivo:** `prometheus/prometheus.yml`

```yaml
scrape_configs:
  # Métricas de agentes (connectors + data agents)
  - job_name: 'luzzi-agents'
    scrape_interval: 30s
    static_configs:
      - targets: ['app:8000']
    metrics_path: '/api/metrics/prometheus/agents'
```

---

### Fase 4: Dashboard en Grafana

**Paneles sugeridos:**

1. **Estado de Connectors**
   - Tabla: tenant_id, status (🟢/🔴), last_seen, bytes_sent
   - Alertas: si last_seen > 5 minutos

2. **Tráfico de Datos**
   - Gráfica: bytes_sent_total over time por tenant
   - Gráfica: queries_processed_total over time

3. **Estado de Data Agents**
   - Tabla: tenant_id, flows_success, flows_failed
   - Gráfica: flow success rate

4. **Errores**
   - Alertas: errors_total > threshold

---

## 📋 Checklist de Implementación

### Fase 1: Backend luzzi-core-im
- [ ] Crear `src/api/schemas/agent_metrics.py`
- [ ] Crear `src/api/agent_metrics.py`
- [ ] Registrar router en `main.py`
- [ ] Tests unitarios
- [ ] Deploy a staging

### Fase 2: Data Connector
- [ ] Crear `metrics_reporter.py`
- [ ] Integrar en `connector_grpc.py`
- [ ] Configurar URL del API en `config.yml`
- [ ] Tests locales

### Fase 3: Prometheus + Grafana
- [ ] Actualizar `prometheus.yml`
- [ ] Crear dashboard en Grafana
- [ ] Configurar alertas

### Fase 4: Data Agent (ETL)
- [ ] Crear reporter compatible con Prefect
- [ ] Integrar hooks de Prefect con el reporter
- [ ] Tests

---

## 🔒 Consideraciones de Seguridad

1. **Autenticación del endpoint de métricas**
   - Validar que el `tenant_id` en la URL coincida con el usuario autenticado
   - Usar API key o JWT para autenticar requests

2. **Rate limiting**
   - Limitar requests por tenant (ej. 1 request cada 10s)

3. **Datos sensibles**
   - NO incluir datos de negocio en métricas
   - Solo contadores y gauges agregados

---

## 📈 Métricas de Éxito

| KPI | Target |
|-----|--------|
| Connectors con métricas activas | 100% |
| Latencia de métricas (envío → scrape) | < 60s |
| Uptime del endpoint de métricas | 99.9% |
| Alertas en < 5 min de falla | 95% |
