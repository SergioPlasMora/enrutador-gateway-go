# Build stage
FROM golang:1.21-alpine AS builder

WORKDIR /app

# Install build dependencies
RUN apk add --no-cache git

# Copy go mod files
COPY go.mod go.sum ./
RUN go mod download

# Copy source code
COPY *.go ./
COPY templates/ ./templates/

# Build the binary
RUN CGO_ENABLED=0 GOOS=linux go build -o gateway .

# Runtime stage
FROM alpine:3.19

WORKDIR /app

# Install runtime dependencies
RUN apk add --no-cache ca-certificates wget

# Copy binary from builder
COPY --from=builder /app/gateway .
COPY --from=builder /app/templates ./templates/

# Copy config if exists
COPY config.yaml ./

# Expose ports
EXPOSE 8081 8815

# Health check
HEALTHCHECK --interval=30s --timeout=3s --start-period=5s --retries=3 \
    CMD wget -q --spider http://localhost:8081/health || exit 1

# Run the gateway
CMD ["./gateway"]
