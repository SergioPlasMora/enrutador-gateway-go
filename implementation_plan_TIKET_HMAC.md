# Diseño de Integración: Servicio Tableros + luzzi-core-im

## Resumen Ejecutivo

Integrar el servicio de **Tableros** (visualización de datos) con luzzi-core-im, donde:
- **luzzi-core-im** maneja toda la autenticación/autorización
- **enrutador-gateway-go** solo valida un ticket temporal y rutea datos
- Los datos regresan **directo al navegador** sin pasar por luzzi-core-im

---

## Patrón Arquitectónico: Control Plane / Data Plane

### ¿Qué es este patrón?

El patrón **Control Plane / Data Plane** es una arquitectura que separa las responsabilidades de **toma de decisiones** (control) del **movimiento de datos** (data). Es ampliamente utilizado en sistemas como Kubernetes, Istio, Envoy, y ahora en nuestra plataforma.

| Plano | Rol | Características |
|-------|-----|-----------------|
| **Control Plane** | "El cerebro" | Toma decisiones, define políticas, gestiona configuración, autentica usuarios |
| **Data Plane** | "Los músculos" | Ejecuta las decisiones, mueve datos, rutea tráfico, no toma decisiones de negocio |

### Aplicación en nuestra arquitectura

```
┌─────────────────────────────────────────────────────────────────────────────────┐
│                    PATRÓN CONTROL PLANE / DATA PLANE                            │
├─────────────────────────────────────────────────────────────────────────────────┤
│                                                                                  │
│   ┌─────────────────────────────────────┐                                       │
│   │         CONTROL PLANE               │                                       │
│   │        (luzzi-core-im)              │                                       │
│   │           FastAPI                   │                                       │
│   │                                     │                                       │
│   │  Responsabilidades:                 │                                       │
│   │  ✅ Autenticación (JWT)              │                                       │
│   │  ✅ Autorización (permisos, roles)   │                                       │
│   │  ✅ Gestión de sesiones              │                                       │
│   │  ✅ Políticas (qué tenant puede ver) │                                       │
│   │  ✅ Configuración del sistema        │                                       │
│   │  ✅ Emite tickets firmados (HMAC)    │                                       │
│   └──────────────────┬──────────────────┘                                       │
│                      │                                                          │
│                      │ Ticket firmado = "autorización pre-validada"             │
│                      ▼                                                          │
│   ┌─────────────────────────────────────┐     ┌──────────────────────┐          │
│   │          DATA PLANE                 │     │                      │          │
│   │     (enrutador-gateway-go)          │◀───▶│   Data Connectors    │          │
│   │            Go + gRPC                │     │     (Python)         │          │
│   │                                     │     │                      │          │
│   │  Responsabilidades:                 │     └──────────────────────┘          │
│   │  ✅ Ruteo de tráfico (multi-tenant)  │                                       │
│   │  ✅ WebSocket → gRPC translation     │                                       │
│   │  ✅ Streaming Arrow IPC              │                                       │
│   │  ✅ Valida tickets (sin consultar)   │                                       │
│   │  ❌ NO toma decisiones de negocio    │                                       │
│   │  ❌ NO gestiona usuarios             │                                       │
│   └─────────────────────────────────────┘                                       │
│                                                                                  │
└─────────────────────────────────────────────────────────────────────────────────┘
```

### Beneficios de esta separación

| Beneficio | Descripción |
|-----------|-------------|
| **Escalado independiente** | Gateway escala con el tráfico de datos; luzzi escala con usuarios/sesiones |
| **Resiliencia** | Si luzzi falla, el gateway sigue sirviendo streams con tickets ya emitidos |
| **Sin cuello de botella** | Los datos NO pasan por el Control Plane durante el streaming |
| **Simplicidad** | Cada componente tiene una responsabilidad clara |
| **Stateless Data Plane** | El gateway no mantiene estado de usuarios, solo valida firma HMAC |

### Comparación con sistemas de la industria

| Sistema | Control Plane | Data Plane |
|---------|---------------|------------|
| **Kubernetes** | API Server, etcd, Scheduler | Kubelet, Container Runtime |
| **Istio** | istiod (Pilot, Citadel) | Envoy proxies |
| **Kong Gateway** | Kong Manager | Kong Gateway proxies |
| **Nuestra Plataforma** | luzzi-core-im | enrutador-gateway-go |

---

## Arquitectura Propuesta

```
┌─────────────────────────────────────────────────────────────────────────────────────────┐
│                         FLUJO COMPLETO DE TABLEROS                                       │
├─────────────────────────────────────────────────────────────────────────────────────────┤
│                                                                                          │
│  ┌───────────┐  1. GET /api/tableros/datasets                ┌──────────────────┐       │
│  │           │ ────────────────────────────────────────────▶│                  │       │
│  │  Browser  │    Authorization: Bearer <jwt>                │  luzzi-core-im   │       │
│  │           │                                               │    (FastAPI)     │       │
│  │           │                                               │                  │       │
│  │           │  2. Valida JWT + Permisos + Cuenta activa     │   ┌──────────┐   │       │
│  │           │     - Token no blacklisted ✓                  │   │  Redis   │   │       │
│  │           │     - Usuario tiene acceso a workspace ✓       │   └──────────┘   │       │
│  │           │     - Servicio "tableros" habilitado ✓        │                  │       │
│  │           │                                               └────────┬─────────┘       │
│  │           │                                                        │                  │
│  │           │  3. Respuesta con ticket firmado                       │                  │
│  │           │ ◀──────────────────────────────────────────────────────┘                  │
│  │           │    {                                                                      │
│  │           │      "ticket": "eyJhbGciOiJIUzI1NiJ9...",                                │
│  │           │      "gateway_url": "wss://gateway.ejemplo.com/stream",                  │
│  │           │      "expires_in": 30                                                    │
│  │           │    }                                                                      │
│  │           │                                                                           │
│  │           │  4. WebSocket DIRECTO al Gateway               ┌──────────────────┐      │
│  │           │ ─────────────────────────────────────────────▶│                  │      │
│  │           │    wss://gateway/stream?ticket=<ticket>        │ enrutador-gateway│      │
│  │           │                                                │      (Go)        │      │
│  │           │                                                │                  │      │
│  │           │  5. Gateway valida ticket (solo firma HMAC)    │   No consulta    │      │
│  │           │     Si válido → conecta a data-conector        │   a luzzi-core   │      │
│  │           │                                                │                  │      │
│  │           │                                                └────────┬─────────┘      │
│  │           │                                                         │                 │
│  │           │                                                         ▼                 │
│  │           │                                                ┌──────────────────┐      │
│  │           │                                                │  data-conector   │      │
│  │           │                                                │    (Python)      │      │
│  │           │                                                └────────┬─────────┘      │
│  │           │                                                         │                 │
│  │           │  6. Stream de datos DIRECTO al browser                  │                 │
│  │           │ ◀───────────────────────────────────────────────────────┘                 │
│  │           │    Arrow IPC via WebSocket                                               │
│  └───────────┘    (NO pasa por luzzi-core-im)                                           │
│                                                                                          │
└─────────────────────────────────────────────────────────────────────────────────────────┘
```

---

## Flujo Detallado Paso a Paso

### FASE 1: Obtención del Ticket (Control Plane)

#### Paso 1: Usuario solicita acceso al dashboard

```
┌──────────────────────────────────────────────────────────────────────────────┐
│  Usuario quiere ver un dashboard                                              │
├──────────────────────────────────────────────────────────────────────────────┤
│                                                                               │
│   👤 Usuario en el Browser                                                    │
│      │                                                                        │
│      │  Hace clic en "Ver Dashboard de Ventas"                               │
│      │  El frontend tiene guardado el JWT de sesión                          │
│      ▼                                                                        │
│   ┌─────────────────────────────────────────────┐                            │
│   │  POST /api/v2/tableros/stream-ticket        │                            │
│   │  Headers:                                    │                            │
│   │    Authorization: Bearer eyJhbGci...        │                            │
│   │  Body:                                       │                            │
│   │    { "dataset": "ventas" }                   │                            │
│   └─────────────────────────────────────────────┘                            │
└──────────────────────────────────────────────────────────────────────────────┘
```

#### Paso 2: luzzi-core-im valida TODO

```
┌──────────────────────────────────────────────────────────────────────────────┐
│  Control Plane realiza todas las validaciones                                 │
├──────────────────────────────────────────────────────────────────────────────┤
│                                                                               │
│   📍 luzzi-core-im (FastAPI) - CONTROL PLANE                                 │
│      │                                                                        │
│      ├─ ✅ Verifica JWT válido y no expirado                                  │
│      ├─ ✅ Verifica JWT no está en blacklist (Redis)                          │
│      ├─ ✅ Extrae user_id del JWT                                             │
│      ├─ ✅ Verifica que el usuario tiene sesión activa                        │
│      ├─ ✅ Obtiene active_account_id (cuenta/workspace activo)               │
│      ├─ ✅ Verifica que el usuario pertenece a esa cuenta (UsuarioCuentaRol)  │
│      ├─ ✅ Verifica que el usuario tiene permiso para "tableros"              │
│      │                                                                        │
│      │  Si CUALQUIERA falla → 401 Unauthorized                               │
│      │                                                                        │
│      ▼  Si TODO OK → Genera el ticket (Paso 3)                               │
└──────────────────────────────────────────────────────────────────────────────┘
```

#### Paso 3: Generación del Ticket HMAC

```
┌──────────────────────────────────────────────────────────────────────────────┐
│  luzzi-core-im genera el Ticket firmado                                       │
├──────────────────────────────────────────────────────────────────────────────┤
│                                                                               │
│   📍 luzzi-core-im crea el ticket:                                           │
│      │                                                                        │
│      │  1. Construye el payload:                                             │
│      │     {                                                                  │
│      │       "user_id": "550e8400-e29b-41d4-a716...",                        │
│      │       "cuenta_id": "660e8400-e29b-41d4-a716...",                      │
│      │       "datasets": ["ventas"],                                          │
│      │       "exp": 1702656630,  ← Unix timestamp (ahora + 30 segundos)      │
│      │       "iat": 1702656600   ← Unix timestamp (ahora)                    │
│      │     }                                                                  │
│      │                                                                        │
│      │  2. Codifica en base64:                                               │
│      │     payload_b64 = "eyJ1c2VyX2lkIjoiNTUw..."                           │
│      │                                                                        │
│      │  3. Firma con HMAC-SHA256 usando TABLEROS_SECRET_KEY:                 │
│      │     signature = HMAC(payload_b64, secret_key)                         │
│      │     signature_b64 = "dGhpcyBpcyBhIHNpZ25hdHVyZQ..."                   │
│      │                                                                        │
│      │  4. Combina:                                                           │
│      │     ticket = "eyJ1c2VyX2lkIjoiNTUw....dGhpcyBpcyBhIHNpZ25hdHVyZQ"     │
│      │              └─────payload─────┘ └──────────signature───────────┘     │
│      ▼                                                                        │
│   Respuesta al Browser:                                                       │
│   {                                                                           │
│     "ticket": "eyJ1c2VyX...signature",                                       │
│     "gateway_url": "wss://gateway.ejemplo.com/stream",                       │
│     "expires_in": 30                                                          │
│   }                                                                           │
└──────────────────────────────────────────────────────────────────────────────┘
```

---

### FASE 2: Conexión Directa al Gateway (Data Plane)

#### Paso 4: Browser conecta DIRECTO al Gateway

```
┌──────────────────────────────────────────────────────────────────────────────┐
│  Conexión directa sin pasar por luzzi-core-im                                │
├──────────────────────────────────────────────────────────────────────────────┤
│                                                                               │
│   👤 Browser                                                                  │
│      │                                                                        │
│      │  JavaScript:                                                           │
│      │  const ws = new WebSocket(                                            │
│      │    "wss://gateway.ejemplo.com/stream?ticket=eyJ1c2VyX..."             │
│      │  );                                                                    │
│      │                                                                        │
│      ▼  Conexión WebSocket directa al Gateway                                │
│   ┌─────────────────────────────────────────────┐                            │
│   │         enrutador-gateway-go                 │                            │
│   │              DATA PLANE                      │                            │
│   └─────────────────────────────────────────────┘                            │
│                                                                               │
│   ⚠️ NOTA: Esta conexión NO pasa por luzzi-core-im                           │
│      El browser habla DIRECTAMENTE con el Gateway                            │
└──────────────────────────────────────────────────────────────────────────────┘
```

#### Paso 5: Gateway valida el Ticket (sin consultar a luzzi)

```
┌──────────────────────────────────────────────────────────────────────────────┐
│  Validación auto-contenida usando HMAC                                       │
├──────────────────────────────────────────────────────────────────────────────┤
│                                                                               │
│   📍 enrutador-gateway-go                                                    │
│      │                                                                        │
│      │  1. Extrae ticket del query param                                     │
│      │     ticketStr = "eyJ1c2VyX...signature"                               │
│      │                                                                        │
│      │  2. Separa payload y signature                                        │
│      │     parts = split(ticketStr, ".")                                     │
│      │     payload_b64 = parts[0]                                            │
│      │     signature_received = parts[1]                                     │
│      │                                                                        │
│      │  3. Recalcula la firma con SU copia del secret:                       │
│      │     signature_expected = HMAC(payload_b64, TABLEROS_SECRET_KEY)       │
│      │                                                                        │
│      │  4. Compara firmas:                                                   │
│      │     if signature_received != signature_expected:                      │
│      │         → 401 "invalid ticket signature"  ❌                          │
│      │                                                                        │
│      │  5. Decodifica el payload                                             │
│      │     payload = base64_decode(payload_b64)                              │
│      │     { "user_id": "...", "cuenta_id": "...", "exp": ... }              │
│      │                                                                        │
│      │  6. Verifica expiración:                                              │
│      │     if now() > exp:                                                   │
│      │         → 401 "ticket expired"  ❌                                    │
│      │                                                                        │
│      ▼  Si todo OK → Ticket válido ✅                                        │
└──────────────────────────────────────────────────────────────────────────────┘
```

---

### FASE 3: Streaming de Datos

#### Paso 6: Gateway conecta al Data Connector correcto

```
┌──────────────────────────────────────────────────────────────────────────────┐
│  Ruteo multi-tenant basado en cuenta_id del ticket                           │
├──────────────────────────────────────────────────────────────────────────────┤
│                                                                               │
│   📍 enrutador-gateway-go                                                    │
│      │                                                                        │
│      │  Ticket dice: cuenta_id = "660e8400..."                               │
│      │                                                                        │
│      │  Busca en config.yaml:                                                │
│      │  connectors:                                                          │
│      │    "660e8400...": "192.168.1.10:50051"  ← ¡Este!                      │
│      │                                                                        │
│      │  Conexión gRPC Arrow Flight:                                          │
│      ▼                                                                        │
│   ┌─────────────────────────────────────────────┐                            │
│   │           data-conector (Python)             │                            │
│   │           192.168.1.10:50051                 │                            │
│   │                                              │                            │
│   │  1. GetFlightInfo(descriptor="ventas")       │                            │
│   │     → Carga el dataset, retorna schema       │                            │
│   │                                              │                            │
│   │  2. DoGet(ticket)                            │                            │
│   │     → Stream de RecordBatches en Arrow IPC   │                            │
│   └─────────────────────────────────────────────┘                            │
└──────────────────────────────────────────────────────────────────────────────┘
```

#### Paso 7: Datos fluyen DIRECTO al Browser

```
┌──────────────────────────────────────────────────────────────────────────────┐
│  Stream sin pasar por el Control Plane                                       │
├──────────────────────────────────────────────────────────────────────────────┤
│                                                                               │
│   data-conector                                                               │
│      │                                                                        │
│      │  RecordBatch 1 (Arrow IPC binario)                                    │
│      │  RecordBatch 2                                                         │
│      │  RecordBatch 3...                                                     │
│      ▼                                                                        │
│   enrutador-gateway-go                                                       │
│      │                                                                        │
│      │  Recibe gRPC stream → Reenvía por WebSocket                           │
│      │  (NO modifica datos, solo los pasa)                                   │
│      │                                                                        │
│      │  ⚠️ NOTA: Aquí NO se consulta a luzzi-core-im                         │
│      │     Los datos van DIRECTO al browser                                  │
│      ▼                                                                        │
│   👤 Browser                                                                  │
│      │                                                                        │
│      │  ws.onmessage = (event) => {                                          │
│      │    const table = tableFromIPC(event.data);  // Apache Arrow JS        │
│      │    renderChart(table);  // Chart.js                                   │
│      │  }                                                                     │
│      │                                                                        │
│      ▼  🎉 ¡Usuario ve su dashboard en tiempo real!                          │
└──────────────────────────────────────────────────────────────────────────────┘
```

---

### Resumen: ¿Qué componente participa en cada paso?

| Paso | Acción | luzzi-core-im | Gateway | Data Connector |
|------|--------|:-------------:|:-------:|:--------------:|
| 1 | Usuario solicita ticket | ✅ | ❌ | ❌ |
| 2 | Validación JWT, permisos | ✅ | ❌ | ❌ |
| 3 | Generación ticket HMAC | ✅ | ❌ | ❌ |
| 4 | Browser conecta a Gateway | ❌ | ✅ | ❌ |
| 5 | Validación del ticket | ❌ | ✅ | ❌ |
| 6 | Conexión a Data Connector | ❌ | ✅ | ✅ |
| 7 | Streaming de datos | ❌ | ✅ | ✅ |

> [!TIP]
> **El Control Plane (luzzi-core-im) solo participa en los pasos 1-3.** Los datos (potencialmente millones de filas) fluyen directamente por el Data Plane, eliminando el cuello de botella.

---

## Hallazgos del Análisis de luzzi-core-im

### Sistema de Autenticación Actual

| Componente | Tecnología | Detalles |
|------------|------------|----------|
| JWT | HS256 | `JWT_SECRET_KEY` compartida |
| Access Token | 15 min | Configurable via `ACCESS_TOKEN_EXPIRE_MINUTES` |
| Refresh Token | 30 días | Almacenado en sesión |
| Blacklist | Redis | `jwt_blacklist:<token>` y `jwt_blacklist_user:<user_id>` |
| Payload JWT | [sub](file://wsl.localhost/Ubuntu/home/sergio/luzzi-core-im/backend/src/views/view_routes/subscription_views.py#135-285) = user_id | También puede incluir `email`, [session_id](file://wsl.localhost/Ubuntu/home/sergio/luzzi-core-im/backend/src/middleware/jwt_auth.py#136-141) |

### Sistema Multi-Tenant

| Modelo | Descripción |
|--------|-------------|
| [Cuenta](file://wsl.localhost/Ubuntu/home/sergio/luzzi-core-im/backend/src/models/cuenta.py#45-76) | = Workspace/Tenant (tiene `propietario_id`) |
| [UsuarioCuentaRol](file://wsl.localhost/Ubuntu/home/sergio/luzzi-core-im/backend/src/models/usuario_cuenta_rol.py#31-50) | Relación Usuario ↔ Cuenta ↔ Rol |
| [UsuarioServicio](file://wsl.localhost/Ubuntu/home/sergio/luzzi-core-im/backend/src/models/usuario_servicio.py#14-34) | Acceso a servicios específicos por usuario+cuenta |
| Sesión | `active_account_id` = cuenta/workspace activo |

### Validación de Permisos

```python
# Archivo: services/permission_service.py
async def check_user_permission(db_session, usuario, permission, account_id):
    # Verifica que el usuario tenga el permiso EN ESA CUENTA
    
async def check_user_role(db_session, usuario, role_name, account_id):
    # Verifica que el usuario tenga el rol EN ESA CUENTA
```

---

## Propuesta de Implementación

### Opción Recomendada: **Signed Ticket con HMAC**

> [!IMPORTANT]
> Esta opción NO requiere que el Gateway consulte a luzzi-core-im. El ticket es auto-validable usando una clave secreta compartida.

#### Formato del Ticket

```json
{
  "user_id": "550e8400-e29b-41d4-a716-446655440000",
  "cuenta_id": "660e8400-e29b-41d4-a716-446655440001",
  "datasets": ["ventas", "clientes"],  // opcional: limitar datasets
  "exp": 1702656630,                    // Unix timestamp (30 seg desde ahora)
  "iat": 1702656600                      // issued at
}
```

**Estructura final:** `base64(payload).base64(hmac_sha256(payload, secret))`

#### Por qué Ticket y no JWT Directo

| Aspecto | JWT del Usuario | Ticket Firmado |
|---------|----------------|----------------|
| Duración | 15 minutos | 30 segundos |
| Información | ID usuario | ID usuario + cuenta + datasets |
| Revocación | Requiere Redis | No necesaria (expira rápido) |
| Si se filtra | Atacante tiene 15 min | Atacante tiene 30 seg |

---

## Cambios Requeridos

### En luzzi-core-im (Python/FastAPI)

#### [NEW] [tableros_service.py](file:///\\wsl.localhost\Ubuntu\home\sergio\luzzi-core-im\backend\src\services\tableros_service.py)

Servicio para generación de tickets:

```python
import hmac
import hashlib
import base64
import json
import time
from typing import List, Optional

class TablerosService:
    def __init__(self, secret_key: str):
        self.secret_key = secret_key.encode()
    
    def generate_ticket(
        self, 
        user_id: str, 
        cuenta_id: str,
        datasets: Optional[List[str]] = None,
        expires_in_seconds: int = 30
    ) -> str:
        """Genera un ticket firmado para acceso al Gateway"""
        payload = {
            "user_id": user_id,
            "cuenta_id": cuenta_id,
            "datasets": datasets or [],
            "exp": int(time.time()) + expires_in_seconds,
            "iat": int(time.time())
        }
        
        payload_b64 = base64.urlsafe_b64encode(
            json.dumps(payload).encode()
        ).decode().rstrip("=")
        
        signature = hmac.new(
            self.secret_key,
            payload_b64.encode(),
            hashlib.sha256
        ).digest()
        
        signature_b64 = base64.urlsafe_b64encode(signature).decode().rstrip("=")
        
        return f"{payload_b64}.{signature_b64}"
```

---

#### [NEW] [tableros_api.py](file:///\\wsl.localhost\Ubuntu\home\sergio\luzzi-core-im\backend\src\api\v2\tableros_api.py)

Endpoints para el servicio Tableros:

```python
from fastapi import APIRouter, Depends, HTTPException, status
from sqlalchemy.ext.asyncio import AsyncSession
from src.api.v2.security import get_current_usuario
from src.config.database import get_db_session
from src.services.tableros_service import TablerosService
from src.services.permission_service import PermissionService
from src.models.usuario import Usuario
from pydantic import BaseModel
from typing import List, Optional
import os

router = APIRouter(prefix="/tableros", tags=["Tableros"])

TABLEROS_SECRET = os.getenv("TABLEROS_SECRET_KEY", "change-me-in-production")
GATEWAY_URL = os.getenv("TABLEROS_GATEWAY_URL", "ws://localhost:8080/stream")

tableros_service = TablerosService(TABLEROS_SECRET)

class StreamTicketResponse(BaseModel):
    ticket: str
    gateway_url: str
    expires_in: int

class DatasetQueryRequest(BaseModel):
    dataset: str
    tenant_id: Optional[str] = None  # Si no se especifica, usa active_account_id

@router.post("/stream-ticket", response_model=StreamTicketResponse)
async def get_stream_ticket(
    request: DatasetQueryRequest,
    current_user: Usuario = Depends(get_current_usuario),
    db_session: AsyncSession = Depends(get_db_session)
):
    """
    Genera un ticket temporal para conectarse al Gateway de streaming.
    Valida que el usuario tenga acceso al workspace y al servicio Tableros.
    """
    # 1. Determinar cuenta/tenant
    cuenta_id = request.tenant_id or str(current_user.active_account_id)
    
    if not cuenta_id:
        raise HTTPException(
            status_code=status.HTTP_400_BAD_REQUEST,
            detail="No hay workspace activo. Seleccione uno primero."
        )
    
    # 2. Verificar que usuario tiene acceso a esa cuenta
    has_access = await PermissionService.check_user_role(
        db_session=db_session,
        usuario=current_user,
        role_name="Propietario",  # o cualquier rol válido
        account_id=cuenta_id
    )
    
    if not has_access:
        # También verificar si es miembro con cualquier rol
        from src.services.usuario_cuenta_rol_service import UsuarioCuentaRolService
        user_roles = await UsuarioCuentaRolService.get_usuario_cuentas_roles_by_usuario(
            db_session, str(current_user.id)
        )
        has_access = any(str(ucr.cuenta_id) == cuenta_id for ucr in user_roles)
    
    if not has_access:
        raise HTTPException(
            status_code=status.HTTP_401_UNAUTHORIZED,
            detail="No tiene acceso a este workspace"
        )
    
    # 3. Verificar que el workspace tiene el servicio Tableros habilitado
    # (por ahora lo dejamos comentado, activar cuando el servicio exista)
    # from src.services.subscription_service import SubscriptionService
    # has_tableros = await SubscriptionService.check_service_access(
    #     db_session, cuenta_id, "tableros"
    # )
    # if not has_tableros:
    #     raise HTTPException(
    #         status_code=status.HTTP_403_FORBIDDEN,
    #         detail="El workspace no tiene el servicio Tableros activo"
    #     )
    
    # 4. Generar ticket firmado
    ticket = tableros_service.generate_ticket(
        user_id=str(current_user.id),
        cuenta_id=cuenta_id,
        datasets=[request.dataset],
        expires_in_seconds=30
    )
    
    return StreamTicketResponse(
        ticket=ticket,
        gateway_url=GATEWAY_URL,
        expires_in=30
    )
```

---

### En enrutador-gateway-go (Go)

#### [NEW] [ticket_validator.go](file:///c:\Users\sergi\OneDrive\Documentos\GitHub\enrutador-gateway-go\ticket_validator.go)

Validación de tickets sin consultar a luzzi-core-im:

```go
package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

type Ticket struct {
	UserID   string   `json:"user_id"`
	CuentaID string   `json:"cuenta_id"`
	Datasets []string `json:"datasets"`
	Exp      int64    `json:"exp"`
	Iat      int64    `json:"iat"`
}

type TicketValidator struct {
	secretKey []byte
}

func NewTicketValidator(secretKey string) *TicketValidator {
	return &TicketValidator{
		secretKey: []byte(secretKey),
	}
}

func (tv *TicketValidator) ValidateTicket(ticketStr string) (*Ticket, error) {
	// Split ticket into payload and signature
	parts := strings.Split(ticketStr, ".")
	if len(parts) != 2 {
		return nil, errors.New("invalid ticket format")
	}
	
	payloadB64 := parts[0]
	signatureB64 := parts[1]
	
	// Verify signature
	expectedSig := tv.computeHMAC(payloadB64)
	expectedSigB64 := base64.RawURLEncoding.EncodeToString(expectedSig)
	
	if !hmac.Equal([]byte(signatureB64), []byte(expectedSigB64)) {
		return nil, errors.New("invalid ticket signature")
	}
	
	// Decode payload
	payloadBytes, err := base64.RawURLEncoding.DecodeString(payloadB64)
	if err != nil {
		return nil, errors.New("invalid ticket encoding")
	}
	
	var ticket Ticket
	if err := json.Unmarshal(payloadBytes, &ticket); err != nil {
		return nil, errors.New("invalid ticket payload")
	}
	
	// Check expiration
	if time.Now().Unix() > ticket.Exp {
		return nil, errors.New("ticket expired")
	}
	
	return &ticket, nil
}

func (tv *TicketValidator) computeHMAC(message string) []byte {
	h := hmac.New(sha256.New, tv.secretKey)
	h.Write([]byte(message))
	return h.Sum(nil)
}
```

---

#### [MODIFY] [browser_ws_grpc.go](file:///c:\Users\sergi\OneDrive\Documentos\GitHub\enrutador-gateway-go\browser_ws_grpc.go)

Agregar validación de ticket en la conexión WebSocket:

```go
// En la función que maneja conexiones WebSocket
func handleWebSocketConnection(w http.ResponseWriter, r *http.Request) {
    // Obtener ticket de query param
    ticketStr := r.URL.Query().Get("ticket")
    if ticketStr == "" {
        http.Error(w, "Missing ticket", http.StatusUnauthorized)
        return
    }
    
    // Validar ticket
    validator := NewTicketValidator(os.Getenv("TABLEROS_SECRET_KEY"))
    ticket, err := validator.ValidateTicket(ticketStr)
    if err != nil {
        http.Error(w, err.Error(), http.StatusUnauthorized)
        return
    }
    
    // Usar ticket.CuentaID como tenant_id para rutear al conector correcto
    connectorAddr := registry.GetConnector(ticket.CuentaID)
    // ... resto del código existente
}
```

---

## Configuración de Entorno

### Variables Nuevas Requeridas

```bash
# En luzzi-core-im (.env)
TABLEROS_SECRET_KEY=una-clave-secreta-de-32-caracteres-minimo
TABLEROS_GATEWAY_URL=wss://gateway.tudominio.com/stream

# En enrutador-gateway-go (config.yaml o env)
TABLEROS_SECRET_KEY=una-clave-secreta-de-32-caracteres-minimo  # MISMA clave
```

> [!CAUTION]
> La `TABLEROS_SECRET_KEY` debe ser idéntica en ambos servicios y nunca exponerse públicamente.

---

## Flujo de Revocación de Sesión

¿Qué pasa si un usuario cierra sesión mientras tiene un stream activo?

### Opciones:

1. **No hacer nada** (recomendado inicialmente)
   - El ticket expira en 30 segundos
   - Si el usuario ya tiene stream abierto, podrá seguir hasta que lo cierre
   - Próximas conexiones requerirán nuevo ticket (que no podrá obtener)

2. **Revocación activa** (futuro)
   - Gateway mantiene conexión Redis para escuchar "revocaciones"
   - Cuando luzzi-core-im blacklistea un user, publica evento
   - Gateway cierra streams activos de ese user_id

---

## Plan de Verificación

### Pruebas Automatizadas

```bash
# 1. Test de generación de ticket
pytest tests/api/test_tableros_api.py

# 2. Test de validación en Gateway
go test ./... -run TestTicketValidator

# 3. Test end-to-end
# - Login en luzzi-core-im
# - Obtener ticket via API
# - Conectar WebSocket al Gateway con ticket
# - Verificar recepción de datos
```

### Verificación Manual

1. Usuario sin acceso a workspace → esperar 401 en `/tableros/stream-ticket`
2. Usuario con acceso → recibir ticket válido
3. Ticket expirado (esperar 35 seg) → Gateway rechaza con "ticket expired"
4. Ticket manipulado → Gateway rechaza con "invalid signature"

---

## Próximos Pasos

1. [ ] Revisar y aprobar este diseño
2. [ ] Implementar `TablerosService` y API en luzzi-core-im
3. [ ] Implementar `TicketValidator` en enrutador-gateway-go
4. [ ] Configurar variables de entorno compartidas
5. [ ] Pruebas de integración
6. [ ] Documentar para el equipo
