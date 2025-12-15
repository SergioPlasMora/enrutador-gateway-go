# Build stage
FROM golang:1.21-alpine AS builder

WORKDIR /app

# Instalar dependencias de compilación
RUN apk add --no-cache gcc musl-dev

# Copiar go.mod primero para cache de dependencias
COPY go.mod go.sum* ./
RUN go mod download

# Copiar código fuente
COPY *.go ./

# Compilar binario estático
RUN CGO_ENABLED=0 GOOS=linux go build -a -installsuffix cgo -o gateway .

# Runtime stage - imagen mínima
FROM alpine:3.19

WORKDIR /app

# Copiar solo el binario
COPY --from=builder /app/gateway .

# Copiar templates para el dashboard
COPY templates/ ./templates/

# Exponer puertos
EXPOSE 8080 8815

# Ejecutar
CMD ["./gateway"]

