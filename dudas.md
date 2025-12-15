# Preguntas Frecuentes (FAQ) - Arquitectura WebSocket + Arrow IPC

## Flujo de Datos

### ¿Cómo funciona el flujo completo de datos?

```
Data-Conector (Python)     Gateway (Go)           Browser (JavaScript)
       │                       │                        │
  PyArrow Table                │                        │
       │                       │                        │
  Serializa a ─────▶  Arrow IPC Bytes ─────▶  Arrow IPC Bytes
  Arrow IPC                    │                        │
  (binario)               (PIPE, no               tableFromIPC()
                          toca el                       │
                          contenido)              JavaScript Table
                                                        │
                                                   Chart.js
```

1. **Dashboard** solicita datos via WebSocket
2. **Gateway-Go** enruta la petición al Data-Conector
3. **Data-Conector** carga el archivo, lo convierte a Arrow IPC, lo chunkea y lo envía
4. **Gateway-Go** reenvía los chunks binarios al navegador (sin tocarlos)
5. **Browser** recibe chunks, los combina, y parsea con `tableFromIPC()`

---

### ¿Quién hace el "chunking" de los datos?

El **Data-Conector (Python)** chunkea los datos. PyArrow divide los datos en **RecordBatches** (típicamente ~4MB cada uno). Cada batch es autocontenido y está serializado en formato Arrow IPC.

---

### ¿El Gateway-Go procesa los datos?

**No.** El Gateway-Go es solo un "tubo" (pipe). No deserializa ni modifica los bytes Arrow. Solo los pasa de un WebSocket a otro.

```go
// Solo pasa bytes directamente
err := ws.WriteMessage(websocket.BinaryMessage, chunk)
```

---

## Arquitectura

### ¿Qué hace cada componente?

| Componente | Rol |
|------------|-----|
| **Data-Conector** | Lee archivos, convierte a Arrow IPC, chunkea, envía bytes |
| **Gateway-Go** | Solo rutea mensajes, no procesa datos |
| **Browser** | Recibe chunks binarios, los combina, parsea, visualiza |

---

### ¿Por qué el Gateway no se satura?

Porque su única función es establecer conexiones y enrutar mensajes. El trabajo pesado (leer archivos, serializar Arrow) lo hacen los **Conectores distribuidos**, no el Gateway central.

---

### ¿Cuántos WebSockets hay?

Hay **2 endpoints WebSocket** en el Gateway:

| Endpoint | Quién se conecta | Propósito |
|----------|------------------|-----------|
| `/ws/connect` | Data-Connectors | Registrar tenant, recibir comandos, enviar datos |
| `/ws/browser` | Navegadores | Solicitar datos, recibir chunks Arrow |

---

## gRPC y Arrow Flight

### ¿Dónde entra gRPC en la arquitectura?

Arrow Flight = Arrow IPC + gRPC. En tu arquitectura:

| Puerto | Protocolo | Uso |
|--------|-----------|-----|
| 8815 | Arrow Flight (gRPC) | Para clientes Python/Java (unified-evaluator) |
| 8080 | WebSocket | Para navegadores + Data Connectors |

---

### ¿Si no necesito unified-evaluator, necesito Arrow Flight/gRPC?

**No.** Si solo usas el Dashboard web, puedes eliminar Arrow Flight y quedarte solo con WebSocket.

---

### ¿Por qué usamos WebSocket en lugar de gRPC para el navegador?

gRPC no funciona bien en navegadores porque:
- Usa HTTP/2 trailers que browsers no soportan bien
- Requiere headers binarios que browsers no permiten controlar

WebSocket es nativo en browsers (`new WebSocket()`) y soporta streaming binario.

---

## Tecnologías

### ¿Qué librerías se usan en cada componente?

| Componente | Librería | Función |
|------------|----------|---------|
| Data-Conector (Python) | `pyarrow` | Convierte CSV → Arrow Table → IPC bytes |
| Gateway (Go) | N/A | Solo pasa bytes |
| Browser (JS) | `apache-arrow` | Parsea IPC bytes → JavaScript Table |

---

### ¿Qué es Arrow IPC?

Es un formato binario columnar definido por Apache Arrow. Permite:
- Serialización eficiente (zero-copy)
- Cross-language (Python puede escribir, JavaScript puede leer)
- Sin necesidad de conversión a JSON/CSV

---

## Resumen Final

```
┌─────────────────────────────────────────────────────────────┐
│                   ARQUITECTURA DASHBOARD                    │
├─────────────────────────────────────────────────────────────┤
│                                                             │
│  TRANSPORTE:  WebSocket (porque gRPC no va en browsers)     │
│  FORMATO:     Arrow IPC (eficiente, cross-language)         │
│  PARSING:     apache-arrow JS (tableFromIPC)                │
│  RENDER:      Chart.js                                      │
│                                                             │
│  RESULTADO:   100 MB en 0.88s 🚀                            │
│                                                             │
└─────────────────────────────────────────────────────────────┘
```
