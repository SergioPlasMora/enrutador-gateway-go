# Seguridad: mTLS y Autenticación de Sesiones

Este documento describe en detalle los mecanismos de seguridad de la plataforma de streaming de datos.

---

## Visión General: Dos Capas de Seguridad

La plataforma utiliza **dos sistemas de autenticación independientes** que protegen diferentes aspectos:

```
┌─────────────────────────────────────────────────────────────────────────────────┐
│                    DOS CAPAS DE SEGURIDAD INDEPENDIENTES                        │
├─────────────────────────────────────────────────────────────────────────────────┤
│                                                                                 │
│  CAPA 1: mTLS (Túnel de Transporte)                                            │
│  ═══════════════════════════════════                                            │
│  Propósito: Establecer el "carril" seguro para transportar datos               │
│  Cuándo: Al iniciar el servicio (ANTES de cualquier solicitud de datos)        │
│                                                                                 │
│  ┌─────────────────┐         mTLS Certificados         ┌─────────────────┐     │
│  │  Data Connector │◄═══════════════════════════════▶│     Gateway     │       │
│  │                 │                                   │                 │      │
│  │  🔐 client.crt  │  "Soy el tenant bf935f05..."     │  🔐 Valida cert │      │
│  │  🔐 client.key  │  "Te identifico, túnel listo"    │  🔐 Extrae CN   │      │
│  │                 │                                   │                 │      │
│  └─────────────────┘                                   └────────┬────────┘      │
│         │                                                       │               │
│         │  ← Conexión PERMANENTE, esperando comandos            │               │
│         │     (reverse tunnel listo 24/7)                       │               │
│                                                                                 │
├─────────────────────────────────────────────────────────────────────────────────┤
│                                                                                 │
│  CAPA 2: session_id (Autorización de Solicitud)                                │
│  ════════════════════════════════════════════════                               │
│  Propósito: Validar que el usuario tiene permiso para ver estos datos          │
│  Cuándo: Al solicitar datos desde el tablero (CADA solicitud)                  │
│                                                                                 │
│  ┌─────────────┐    ┌─────────────┐    ┌─────────────┐    ┌─────────────┐      │
│  │   Usuario   │    │ luzzi-core  │    │   Gateway   │    │  Connector  │      │
│  │  (Browser)  │    │     -im     │    │    (Go)     │    │  (Python)   │      │
│  └──────┬──────┘    └──────┬──────┘    └──────┬──────┘    └──────┬──────┘      │
│         │                  │                  │                  │              │
│    1.   │──── Login ──────▶│                  │                  │              │
│         │                  │                  │                  │              │
│    2.   │◀── JWT ──────────│                  │                  │              │
│         │                  │                  │                  │              │
│    3.   │── Ir a Tableros ▶│                  │                  │              │
│         │                  │                  │                  │              │
│    4.   │◀─ session_id ────│ (firmado)        │                  │              │
│         │   + URL WebSocket │                  │                  │              │
│         │                  │                  │                  │              │
│    5.   │───────── WebSocket /stream/{session_id} ──────────────▶│              │
│         │                  │                  │                  │              │
│    6.   │                  │◀── Validar ──────│                  │              │
│         │                  │    session_id    │                  │              │
│         │                  │────── OK ───────▶│                  │              │
│         │                  │                  │                  │              │
│    7.   │                  │                  │═══ Usa el túnel ═▶│              │
│         │                  │                  │   mTLS ya listo   │              │
│         │                  │                  │                  │              │
│    8.   │                  │                  │◀════ Datos ══════│              │
│         │                  │                  │   Arrow IPC       │              │
│         │                  │                  │                  │              │
│    9.   │◀───────── Datos via WebSocket ──────│                  │              │
│         │                                                                       │
└─────────────────────────────────────────────────────────────────────────────────┘
```

---

## Comparativa de Mecanismos

| Aspecto | mTLS (Túnel) | session_id (Solicitud) |
|---------|--------------|------------------------|
| **¿Qué protege?** | La conexión Connector ↔ Gateway | La solicitud Browser → Gateway |
| **¿Cuándo ocurre?** | Al iniciar el servicio (una vez) | Cada vez que solicitas datos |
| **¿Quién se autentica?** | El Data Connector (máquina) | El Usuario (persona) |
| **¿Cómo?** | Certificados X.509 | Token firmado (JWT/HMAC) |
| **¿Por qué?** | Solo connectors autorizados pueden enviar datos | Solo usuarios autorizados pueden ver datos |

---

## Parte 1: mTLS (Mutual TLS)

### ¿Qué es mTLS?

**mTLS (Mutual Transport Layer Security)** es una extensión de TLS donde **ambas partes** se autentican mutuamente con certificados.

```
┌──────────────────────────────────────────────────────────────────────────────┐
│                         TLS Normal vs mTLS                                   │
├──────────────────────────────────────────────────────────────────────────────┤
│                                                                              │
│   TLS Normal (HTTPS):          mTLS (Este proyecto):                         │
│   ────────────────────         ─────────────────────                         │
│   Cliente → Servidor           Cliente → Servidor                            │
│   "¿Quién eres?"               "¿Quién eres?"                                │
│   Servidor: 📜 cert            Servidor: 📜 server.crt                       │
│   Cliente: ✓ Válido            Cliente: ✓ Válido                             │
│                                                                              │
│   ⚠️ Servidor NO sabe          Servidor → Cliente                            │
│      quién es el cliente       "¿Y tú quién eres?"                           │
│                                Cliente: 📜 client.crt (CN=tenant_id)         │
│                                Servidor: ✓ Válido, extracto tenant_id        │
│                                                                              │
└──────────────────────────────────────────────────────────────────────────────┘
```

### Handshake mTLS Detallado

```
┌─────────────────────────────────────────────────────────────────────────────┐
│                    HANDSHAKE mTLS PASO A PASO                               │
├─────────────────────────────────────────────────────────────────────────────┤
│                                                                             │
│  Data Connector (Python)              Gateway (Go)                          │
│  ──────────────────────               ────────────                          │
│         │                                     │                             │
│  PASO 1 │──── ClientHello ───────────────────▶│                             │
│         │     "Quiero conectar vía TLS"       │                             │
│         │                                     │                             │
│  PASO 2 │◀──── ServerHello + server.crt ──────│                             │
│         │      + CertificateRequest           │ (Go pide cert al cliente)   │
│         │                                     │                             │
│  PASO 3 │     Validar server.crt:             │                             │
│         │     ✓ ¿Firmado por ca.crt?          │                             │
│         │     ✓ ¿No expirado?                 │                             │
│         │     ✓ ¿Hostname válido?             │                             │
│         │                                     │                             │
│  PASO 4 │──── client.crt + Prueba firma ─────▶│                             │
│         │     (CN = tenant_id)                │                             │
│         │                                     │                             │
│  PASO 5 │                                     │ Validar client.crt:         │
│         │                                     │ ✓ ¿Firmado por ca.crt?      │
│         │                                     │ ✓ ¿No expirado?             │
│         │                                     │ ✓ Extraer CN → tenant_id    │
│         │                                     │                             │
│  PASO 6 │◀════════ Conexión segura ══════════▶│                             │
│         │     (mTLS establecido)              │                             │
│                                                                             │
└─────────────────────────────────────────────────────────────────────────────┘
```

---

## Parte 2: Generación y Firma de Certificados

### Estructura de Archivos

```
certs/
├── ca.crt              # Certificado raíz (la "autoridad")
├── ca.key              # Clave privada del CA (¡PROTEGER!)
├── server.crt          # Certificado del Gateway
├── server.key          # Clave privada del Gateway
└── clients/
    └── {tenant_id}/
        ├── client.crt  # Certificado del Connector (CN = tenant_id)
        └── client.key  # Clave privada del Connector
```

### ¿Qué es una Cadena de Confianza?

```
┌─────────────────────────────────────────────────────────────────────────────┐
│                    CADENA DE CONFIANZA (Trust Chain)                        │
├─────────────────────────────────────────────────────────────────────────────┤
│                                                                             │
│                         ┌─────────────────┐                                 │
│                         │   CA (Raíz)     │                                 │
│                         │   🔐 ca.key     │  ← La "autoridad máxima"        │
│                         │   📜 ca.crt     │     (auto-firmado)              │
│                         └────────┬────────┘                                 │
│                                  │                                          │
│                    ┌─────────────┴─────────────┐                            │
│                    │ FIRMA con ca.key          │                            │
│                    ▼                           ▼                            │
│           ┌─────────────────┐         ┌─────────────────┐                   │
│           │   server.crt    │         │   client.crt    │                   │
│           │   (Gateway)     │         │   (Connector)   │                   │
│           │                 │         │                 │                   │
│           │   CN=gateway    │         │   CN=tenant_id  │                   │
│           │   .luzzi.com    │         │   (identidad)   │                   │
│           └─────────────────┘         └─────────────────┘                   │
│                                                                             │
│   VALIDACIÓN: Cualquiera con ca.crt puede verificar que server.crt         │
│               y client.crt fueron firmados por la CA legítima.              │
│                                                                             │
└─────────────────────────────────────────────────────────────────────────────┘
```

### Proceso de Generación Paso a Paso

#### 1. Generar la CA (Certificate Authority)

```bash
# 1. Generar clave privada RSA de 4096 bits
openssl genrsa -out ca.key 4096

# 2. Crear certificado auto-firmado (válido 10 años)
openssl req -new -x509 -days 3650 -key ca.key -out ca.crt \
    -subj "/C=MX/ST=CDMX/O=Luzzi/CN=Luzzi Root CA"
```

**¿Qué contiene `ca.crt`?**

```
┌─────────────────────────────────────────────────────────────┐
│                    CERTIFICADO CA (ca.crt)                  │
├─────────────────────────────────────────────────────────────┤
│ Version: 3                                                  │
│ Serial Number: (único, generado por OpenSSL)                │
│ Signature Algorithm: sha256WithRSAEncryption                │
│                                                             │
│ Issuer (Emisor):                                            │
│   C  = MX                                                   │
│   ST = CDMX                                                 │
│   O  = Luzzi                                                │
│   CN = Luzzi Root CA  ← Mismo que Subject (auto-firmado)    │
│                                                             │
│ Subject (Sujeto):                                           │
│   C  = MX                                                   │
│   ST = CDMX                                                 │
│   O  = Luzzi                                                │
│   CN = Luzzi Root CA                                        │
│                                                             │
│ Validity (Validez):                                         │
│   Not Before: Dec 23 2024                                   │
│   Not After:  Dec 21 2034 (10 años)                         │
│                                                             │
│ Public Key: RSA 4096 bits                                   │
│   [clave pública derivada de ca.key]                        │
│                                                             │
│ Extensions:                                                 │
│   CA: TRUE  ← Indica que puede firmar otros certificados    │
│                                                             │
│ Signature: [firma digital creada con ca.key]                │
│   - Hash SHA-256 del contenido                              │
│   - Cifrado con la clave privada ca.key                     │
└─────────────────────────────────────────────────────────────┘
```

#### 2. Generar Certificado del Servidor (Gateway)

```bash
# 1. Generar clave privada del servidor
openssl genrsa -out server.key 2048

# 2. Crear CSR (Certificate Signing Request)
openssl req -new -key server.key -out server.csr \
    -subj "/C=MX/ST=CDMX/O=Luzzi/CN=gateway.luzzi.com"

# 3. Firmar con la CA (válido 1 año)
openssl x509 -req -days 365 \
    -in server.csr \
    -CA ca.crt -CAkey ca.key -CAcreateserial \
    -out server.crt \
    -extensions v3_req -extfile server.cnf
```

**Archivo `server.cnf` (extensiones importantes):**

```ini
[v3_req]
basicConstraints = CA:FALSE              # NO es una CA
keyUsage = digitalSignature, keyEncipherment
subjectAltName = @alt_names

[alt_names]
DNS.1 = localhost
DNS.2 = gateway
DNS.3 = gateway.luzzi.com
IP.1 = 127.0.0.1
```

**¿Qué contiene `server.crt`?**

```
┌─────────────────────────────────────────────────────────────┐
│               CERTIFICADO SERVIDOR (server.crt)             │
├─────────────────────────────────────────────────────────────┤
│ Version: 3                                                  │
│ Serial Number: (asignado por CA)                            │
│ Signature Algorithm: sha256WithRSAEncryption                │
│                                                             │
│ Issuer (Emisor): ← DIFERENTE al Subject                     │
│   CN = Luzzi Root CA  ← Firmado por la CA                   │
│                                                             │
│ Subject (Sujeto):                                           │
│   C  = MX                                                   │
│   ST = CDMX                                                 │
│   O  = Luzzi                                                │
│   CN = gateway.luzzi.com  ← Identidad del servidor          │
│                                                             │
│ Validity: 365 días                                          │
│                                                             │
│ Subject Alternative Names (SAN):                            │
│   DNS: localhost, gateway, gateway.luzzi.com                │
│   IP: 127.0.0.1                                             │
│   ↑ Hostnames válidos para este certificado                 │
│                                                             │
│ Signature: [firma de la CA]                                 │
│   - La CA usó ca.key para firmar este certificado           │
│   - Cualquiera con ca.crt puede verificar la firma          │
└─────────────────────────────────────────────────────────────┘
```

#### 3. Generar Certificado del Cliente (Connector)

```bash
TENANT_ID="bf935f05-bf2e-4138-bfec-f4baaf99fecc"

# 1. Generar clave privada del cliente
openssl genrsa -out "clients/$TENANT_ID/client.key" 2048

# 2. Crear CSR con tenant_id como CN
openssl req -new -key "clients/$TENANT_ID/client.key" \
    -out "clients/$TENANT_ID/client.csr" \
    -subj "/C=MX/ST=CDMX/O=Luzzi/CN=$TENANT_ID"
#                                    ↑ IMPORTANTE: CN = tenant_id

# 3. Firmar con la CA (válido 90 días - renovación frecuente)
openssl x509 -req -days 90 \
    -in "clients/$TENANT_ID/client.csr" \
    -CA ca.crt -CAkey ca.key -CAcreateserial \
    -out "clients/$TENANT_ID/client.crt"
```

**¿Qué contiene `client.crt`?**

```
┌─────────────────────────────────────────────────────────────┐
│               CERTIFICADO CLIENTE (client.crt)              │
├─────────────────────────────────────────────────────────────┤
│ Version: 3                                                  │
│ Serial Number: (único, asignado por CA)                     │
│                                                             │
│ Issuer:                                                     │
│   CN = Luzzi Root CA  ← Firmado por nuestra CA              │
│                                                             │
│ Subject:                                                    │
│   C  = MX                                                   │
│   ST = CDMX                                                 │
│   O  = Luzzi                                                │
│   CN = bf935f05-bf2e-4138-bfec-f4baaf99fecc                │
│        ↑ tenant_id EMBEBIDO en el certificado               │
│                                                             │
│ Validity: 90 días (renovación frecuente = más seguro)       │
│                                                             │
│ Signature: [firma de la CA]                                 │
│   - Gateway valida esta firma contra ca.crt                 │
│   - Si pasa: el tenant_id es CRIPTOGRÁFICAMENTE VERIFICADO  │
└─────────────────────────────────────────────────────────────┘
```

---

## Parte 3: ¿Cómo Funciona la Firma Digital?

### El Proceso de Firma

```
┌─────────────────────────────────────────────────────────────────────────────┐
│                         PROCESO DE FIRMA DIGITAL                            │
├─────────────────────────────────────────────────────────────────────────────┤
│                                                                             │
│  GENERAR CERTIFICADO:                                                       │
│  ═══════════════════                                                        │
│                                                                             │
│  1. Crear contenido del certificado (Subject, validez, clave pública...)   │
│                                                                             │
│  2. Calcular hash SHA-256 del contenido                                     │
│     Contenido ──▶ [SHA-256] ──▶ Hash (32 bytes)                            │
│                                                                             │
│  3. Cifrar el hash con ca.key (clave privada de la CA)                     │
│     Hash ──▶ [RSA encrypt con ca.key] ──▶ Firma Digital                    │
│                                                                             │
│  4. Adjuntar firma al certificado                                           │
│     Certificado = Contenido + Firma                                         │
│                                                                             │
├─────────────────────────────────────────────────────────────────────────────┤
│                                                                             │
│  VERIFICAR CERTIFICADO:                                                     │
│  ══════════════════════                                                     │
│                                                                             │
│  1. Extraer la firma del certificado                                        │
│                                                                             │
│  2. Descifrar la firma con ca.crt (clave pública de la CA)                 │
│     Firma ──▶ [RSA decrypt con ca.crt] ──▶ Hash Original                   │
│                                                                             │
│  3. Calcular hash del contenido actual                                      │
│     Contenido ──▶ [SHA-256] ──▶ Hash Calculado                             │
│                                                                             │
│  4. Comparar hashes                                                         │
│     Hash Original == Hash Calculado ? ✓ VÁLIDO : ✗ INVÁLIDO                │
│                                                                             │
└─────────────────────────────────────────────────────────────────────────────┘
```

### ¿Por qué es Seguro?

```
┌─────────────────────────────────────────────────────────────────────────────┐
│                     GARANTÍAS DE SEGURIDAD                                  │
├─────────────────────────────────────────────────────────────────────────────┤
│                                                                             │
│  1. AUTENTICIDAD                                                            │
│     ─────────────                                                           │
│     Solo quien tiene ca.key puede crear firmas válidas.                     │
│     Si la firma es válida, el certificado vino de nuestra CA.               │
│                                                                             │
│  2. INTEGRIDAD                                                              │
│     ───────────                                                             │
│     Si alguien modifica el contenido (ej: cambiar el CN/tenant_id),         │
│     el hash no coincidirá y la validación fallará.                          │
│                                                                             │
│  3. NO REPUDIO                                                              │
│     ───────────                                                             │
│     No se puede negar haber firmado un certificado.                         │
│     La firma es prueba matemática de origen.                                │
│                                                                             │
│  ATAQUE IMPOSIBLE:                                                          │
│  ─────────────────                                                          │
│  ✗ Crear certificado con tenant_id falso → No tienes ca.key                │
│  ✗ Modificar tenant_id existente → Hash no coincide                         │
│  ✗ Copiar certificado de otro tenant → Necesitas client.key                 │
│                                                                             │
└─────────────────────────────────────────────────────────────────────────────┘
```

---

## Parte 4: Extracción del tenant_id en el Gateway

### Código Go que Extrae el CN

```go
// connector_grpc.go líneas 128-134
func (s *ConnectorGRPCServer) Connect(stream pb.ConnectorService_ConnectServer) error {
    p, ok := peer.FromContext(stream.Context())
    if ok && p.AuthInfo != nil {
        if tlsInfo, ok := p.AuthInfo.(credentials.TLSInfo); ok {
            // VerifiedChains contiene los certificados ya validados
            if len(tlsInfo.State.VerifiedChains) > 0 {
                clientCert := tlsInfo.State.VerifiedChains[0][0]
                
                // Extraer CN (Common Name) = tenant_id
                certTenantID = clientCert.Subject.CommonName
            }
        }
    }
}
```

### Doble Validación

```go
// connector_grpc.go líneas 171-184
// El connector también envía tenant_id en el mensaje RegisterRequest
// Gateway compara ambos valores

if s.mtlsEnabled && certTenantID != tenantID {
    // El tenant_id del certificado NO coincide con el del registro
    // RECHAZAR - posible intento de suplantación
    return fmt.Errorf("tenant_id mismatch: cert=%s, register=%s", 
                      certTenantID, tenantID)
}
```

---

## Parte 5: session_id para Autenticación de Usuarios

El `session_id` es un mecanismo separado para validar solicitudes de browsers:

```
┌─────────────────────────────────────────────────────────────────────────────┐
│                         FLUJO DE session_id                                 │
├─────────────────────────────────────────────────────────────────────────────┤
│                                                                             │
│  1. Usuario hace login en luzzi-core-im                                     │
│     └── Recibe JWT de autenticación                                         │
│                                                                             │
│  2. Usuario navega a Tableros                                               │
│                                                                             │
│  3. luzzi-core-im genera session_id firmado                                 │
│     session_id = base64(                                                    │
│       user_id + cuenta_id + edge_id + timestamp + HMAC(secret_key)          │
│     )                                                                       │
│                                                                             │
│  4. Browser recibe URL:                                                     │
│     wss://gateway.luzzi.com/stream/{session_id}                             │
│                                                                             │
│  5. Gateway recibe conexión WebSocket                                       │
│     └── Extrae session_id de la URL                                         │
│     └── Llama a Control Plane: GET /api/v2/control/validate/{session_id}    │
│                                                                             │
│  6. Control Plane valida:                                                   │
│     ✓ Firma HMAC válida                                                     │
│     ✓ No expirado                                                           │
│     ✓ Usuario tiene permisos para este edge_id                              │
│                                                                             │
│  7. Gateway permite/rechaza la conexión                                     │
│                                                                             │
│  8. Si válido: Gateway usa el túnel mTLS para obtener datos del Connector   │
│                                                                             │
└─────────────────────────────────────────────────────────────────────────────┘
```

---

## Resumen: Analogía del Ferrocarril

```
🚂 FERROCARRIL (mTLS)
═══════════════════════════════════════════════════════════════

1. El Data Connector "construye" una vía férrea hacia el Gateway
   usando certificados como "permiso de construcción"
   
2. La vía queda LISTA y esperando trenes (datos)

3. El certificado (client.crt) contiene:
   - Identidad del constructor (CN = tenant_id)
   - Firma del gobierno (CA) que certifica la identidad
   - Fecha de expiración del permiso


🎫 BOLETO DE TREN (session_id)
═══════════════════════════════════════════════════════════════

1. Usuario hace login en luzzi-core-im (compra boleto)
2. Va a Tableros (estación de tren)
3. luzzi-core-im le da un "boleto" (session_id)
4. Usuario presenta el boleto al Gateway (controlador)
5. Gateway valida: "¿Este boleto es válido?"
6. Si sí → envía el tren por la vía mTLS ya construida
7. Datos llegan al browser
```

---

## Comandos Útiles

### Generar todos los certificados

```bash
cd certs/
./generate_certs.sh all mi-tenant-id
```

### Ver contenido de un certificado

```bash
openssl x509 -in client.crt -text -noout
```

### Verificar que un certificado está firmado por la CA

```bash
openssl verify -CAfile ca.crt client.crt
# Debe mostrar: client.crt: OK
```

### Ver el CN de un certificado

```bash
openssl x509 -in client.crt -noout -subject
# Subject: C = MX, ST = CDMX, O = Luzzi, CN = bf935f05-bf2e-4138-bfec-f4baaf99fecc
```

---

## Consideraciones de Seguridad

| Componente | Protección Requerida |
|------------|---------------------|
| `ca.key` | **MÁXIMA** - Nunca compartir, guardar offline si es posible |
| `ca.crt` | Público - Distribuir a todos los componentes |
| `server.key` | Solo en el Gateway |
| `client.key` | Solo en el Connector correspondiente |
| Certificados (*.crt) | Pueden ser públicos (no contienen secretos) |

> ⚠️ **IMPORTANTE**: Si `ca.key` se compromete, un atacante puede crear certificados para cualquier tenant_id. Rota todos los certificados inmediatamente.
