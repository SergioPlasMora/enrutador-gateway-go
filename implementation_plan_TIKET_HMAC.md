# Diseño de Integración: Servicio Tableros + luzzi-core-im

## Resumen Ejecutivo

Integrar el servicio de **Tableros** (visualización de datos tipo Power BI) con luzzi-core-im, donde:
- **luzzi-core-im** maneja toda la autenticación/autorización
- **enrutador-gateway-go** solo valida un ticket temporal y rutea datos
- Los datos regresan **directo al navegador** sin pasar por luzzi-core-im

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
