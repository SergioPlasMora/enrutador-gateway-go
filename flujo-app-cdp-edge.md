# 🔁 Flujo Completo: Edge → Cliente con Control Plane + Data Plane

A continuación tienes el **diagrama y secuencia paso a paso** que separa **autorización (control)** de **movimiento de bytes (datos)**. El cliente **nunca habla directamente con el edge**; ambos se conectan al **Data Plane Router** que solo **enruta** después de que el **Control Plane** (FastAPI) ha dicho «sí tiene permiso».

---

## 📊 Diagrama de Flujo (Autorización → Streaming)

```mermaid
sequenceDiagram
    participant C as Cliente (Navegador)
    participant CP as Control Plane<br/>FastAPI
    participant DP as Data Plane<br/>Go Router
    participant E as Edge Agent<br/>Cliente

    Note over C,DP: CONTROL PLANE – solo metadatos
    Note over C,DP: DATA PLANE – solo bytes

    rect rgb(200 230 255)
    Note over C,CP: 1. Autorización y registro de sesión
    C->>+CP: POST /control/access<br/>{client_id, dataset_id}
    CP->>CP: valida JWT, permisos, cuotas
    CP->>CP: genera session_token & data_plane_url
    CP-->>-C: 200 OK<br/>{session_token, wss://dp.../stream/<sid>}
    end

    rect rgb(220 255 220)
    Note over C,DP: 2. Cliente se engancha al Data Plane
    C->>+DP: wss://dp/stream/<sid><br/>Header: Authorization: <session_token>
    DP->>DP: extrae sid del path
    DP->>+CP: GET /control/validate/<sid> (HTTP rápido)
    CP-->>-DP: 200 {client_id, edge_id, expires}
    DP->>DP: marca sesión como «cliente conectado»
    DP-->>C: 101 Switching Protocols
    end

    rect rgb(255 255 220)
    Note over E,DP: 3. Edge se engancha (inicia él)
    E->>+DP: wss://dp/edge/<edge_id><br/>Header: Edge-Key: <pre-shared>
    DP->>DP: lookup sesiones pendientes de este edge
    DP->>DP: asocia edge → cliente(s)
    DP-->>E: 101 Switching Protocols
    end

    rect rgb(255 220 255)
    Note over C,E: 4. Streaming de datos (sin Control Plane)
    E->>DP: binary frame {data}
    DP->>DP: re-escribe frame (sin parse)
    DP->>C: mismo binary frame
    end
```

---

## 🔎 Paso a Paso Detallado

### 1️⃣ Control Plane – Autorización (FastAPI)

```http
POST /control/access
Authorization: Bearer <JWT>
Content-Type: application/json

{
  "client_id": "acme-corp",
  "dataset_id": "factory-01-metrics",
  "requested_ops": ["read"]
}
```

**FastAPI responde:**

```json
200 OK
{
  "session_id": "s-9f3b2e1a",
  "data_plane_url": "wss://dp.saas.com/stream/s-9f3b2e1a",
  "expires_at": "2025-07-21T18:25:43Z",
  "edge_id": "edge-acme-f01"   // qué edge debe conectarse
}
```

**Validaciones internas (Control Plane)**:
- JWT válido
- Cliente tiene plan «Analytics Pro»
- Dataset pertenece al cliente
- Cuota de streams simultáneos no excedida
- Edge `edge-acme-f01` está on-line (heartbeat reciente)

---

### 2️⃣ Data Plane – Cliente se conecta (Go Router)

```javascript
const ws = new WebSocket("wss://dp.saas.com/stream/s-9f3b2e1a");
ws.onopen = () => {
  // Header no se puede poner en browser→WS, enviamos primer frame
  ws.send(JSON.stringify({auth: "s-9f3b2e1a"}));
};
```

**Go Router**:

```go
func (r *DataRouter) HandleBrowser(ws *websocket.Conn, sid string) {
    // 1. Pregunta a Control Plane (HTTP corto, <5 ms)
    info, err := r.cp.Validate(sid) // GET /control/validate/s-9f3b2e1a
    if err != nil {
        ws.Close() // 401
        return
    }
    // 2. Guarda sesión
    r.sessions.Store(sid, &Session{
        clientID:  info.ClientID,
        edgeID:    info.EdgeID,
        browser:   ws,
        edge:      nil, // aún no
    })
}
```

---

### 3️⃣ Data Plane – Edge se conecta

Edge (en infra del cliente) **abre conexión saliente** (cumple firewall):

```bash
wss://dp.saas.com/edge/edge-acme-f01
Header: Edge-Key: <pre-shared-secret>
```

Go Router:

```go
func (r *DataRouter) HandleEdge(ws *websocket.Conn, edgeID string) {
    // 1. Autentica edge (pre-shared key)
    if !r.validateEdge(edgeID, ws.Request().Header.Get("Edge-Key")) {
        ws.Close()
        return
    }
    // 2. Busca sesiones de ese edge
    r.sessions.Range(func(key, value interface{}) bool {
        sess := value.(*Session)
        if sess.edgeID == edgeID && sess.browser != nil {
            sess.edge = ws
            go r.pipe(sess) // ¡enruta!
        }
        return true
    })
}
```

---

### 4️⃣ Data Plane – Streaming puro

```go
func (r *DataRouter) pipe(s *Session) {
    // Copia bytes sin parsear: edge → navegador
    io.Copy(s.browser, s.edge) // latencia ~0
}
```

- **Sin JSON decode/encode** en el router.
- **Sin consultar base de datos**.
- **Sin llamar a Control Plane** mientras fluye.
- Si el frame es > 64 KB, **se fragmenta automáticamente** (WebSockets RFC).

---

## 🛡️ Seguridad y Aislamiento

| Punto | Implementación |
|-------|----------------|
| **Token de sesión** | JWT corto (15 min) firmado por Control Plane; Data Plane solo verifica firma (sin DB). |
| **Edge autenticación** | Pre-shared key por cliente + TLS mutual opcional. |
| **Rate-limit** | Control Plane entrega cuota (ej. 500 MB/h) y Data Plane cuenta bytes; al límite, cierra. |
| **Revocación en caliente** | FastAPI publica en Redis `revoke:s-9f3b2e1a`; Go Router suscribe y cierra inmediato. |

---

## 🧪 Flujo de Error – Acceso Denegado

```mermaid
sequenceDiagram
    C->>CP: POST /control/access<br/>dataset=competitor-data
    CP->>CP: JWT OK, pero dataset NO pertenece al cliente
    CP-->>C: 403 Forbidden
    Note over C: Cliente no recibe URL del Data Plane
```

---

## 🧩 Resumen de Responsabilidades

| **Control Plane (FastAPI)** | **Data Plane (Go Router)** |
|-----------------------------|----------------------------|
| Validar JWT y permisos | Mover bytes edge ↔ cliente |
| Generar session_token | Validar token vs Redis |
| Decidir qué edge usar | Asociar edge con cliente |
| Aplicar cuotas y billing | Cortar stream si se excede cuota |
| Logs de auditoría | Métricas de throughput |
| NO toca datos | NO toca lógica de negocio |

Con esta separación puedes:
- **Escalar Data Plane** (más instancias Go) sin tocar FastAPI.
- **Desplear fixes de autorización** sin downtime de streaming.
- **Cambiar el motor de datos** (Go → Rust → Kafka) sin reescribir tu SaaS.

¿quieres el código de referencia completo del Go Data Router con este flujo?