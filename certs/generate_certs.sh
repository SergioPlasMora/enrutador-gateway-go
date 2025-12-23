#!/bin/bash
# =============================================================================
# mTLS Certificate Generator for Luzzi Data Platform
# Generates CA, Server (Gateway), and Client (Connector) certificates
# =============================================================================

set -e

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$SCRIPT_DIR"

# Configuration
CA_DAYS=3650          # 10 years for CA
SERVER_DAYS=365       # 1 year for server
CLIENT_DAYS=90        # 90 days for clients (short-lived)

echo "=== Luzzi mTLS Certificate Generator ==="
echo ""

# =============================================================================
# 1. Generate CA (Certificate Authority)
# =============================================================================
generate_ca() {
    echo "[1/3] Generating CA certificate..."
    
    if [ -f "ca.crt" ]; then
        echo "  CA already exists. Skipping. (Delete ca.crt to regenerate)"
        return
    fi
    
    # Generate CA private key
    openssl genrsa -out ca.key 4096
    
    # Generate CA certificate
    openssl req -new -x509 -days $CA_DAYS -key ca.key -out ca.crt \
        -subj "/C=MX/ST=CDMX/O=Luzzi/CN=Luzzi Root CA"
    
    echo "  ✓ CA certificate generated: ca.crt"
    echo "  ⚠ PROTECT ca.key - it can sign any certificate!"
}

# =============================================================================
# 2. Generate Server Certificate (Gateway)
# =============================================================================
generate_server() {
    echo "[2/3] Generating Server certificate (Gateway)..."
    
    # Create server config with SAN
    cat > server.cnf << EOF
[req]
distinguished_name = req_distinguished_name
req_extensions = v3_req
prompt = no

[req_distinguished_name]
C = MX
ST = CDMX
O = Luzzi
CN = gateway.luzzi.com

[v3_req]
basicConstraints = CA:FALSE
keyUsage = nonRepudiation, digitalSignature, keyEncipherment
subjectAltName = @alt_names

[alt_names]
DNS.1 = localhost
DNS.2 = gateway
DNS.3 = gateway.luzzi.com
DNS.4 = *.luzzi.com
IP.1 = 127.0.0.1
IP.2 = ::1
EOF

    # Generate server private key
    openssl genrsa -out server.key 2048
    
    # Generate CSR
    openssl req -new -key server.key -out server.csr -config server.cnf
    
    # Sign with CA
    openssl x509 -req -days $SERVER_DAYS -in server.csr \
        -CA ca.crt -CAkey ca.key -CAcreateserial \
        -out server.crt -extensions v3_req -extfile server.cnf
    
    # Cleanup
    rm -f server.csr server.cnf
    
    echo "  ✓ Server certificate generated: server.crt"
}

# =============================================================================
# 3. Generate Client Certificate (Connector)
# =============================================================================
generate_client() {
    TENANT_ID="${1:-tenant-test}"
    
    echo "[3/3] Generating Client certificate for: $TENANT_ID"
    
    # Create client directory
    mkdir -p "clients/$TENANT_ID"
    
    # Generate client private key
    openssl genrsa -out "clients/$TENANT_ID/client.key" 2048
    
    # Generate CSR with tenant_id as CN
    openssl req -new -key "clients/$TENANT_ID/client.key" \
        -out "clients/$TENANT_ID/client.csr" \
        -subj "/C=MX/ST=CDMX/O=Luzzi/CN=$TENANT_ID"
    
    # Sign with CA
    openssl x509 -req -days $CLIENT_DAYS \
        -in "clients/$TENANT_ID/client.csr" \
        -CA ca.crt -CAkey ca.key -CAcreateserial \
        -out "clients/$TENANT_ID/client.crt"
    
    # Cleanup
    rm -f "clients/$TENANT_ID/client.csr"
    
    echo "  ✓ Client certificate generated: clients/$TENANT_ID/client.crt"
    echo "  ℹ CN (tenant_id): $TENANT_ID"
    echo "  ℹ Expires in: $CLIENT_DAYS days"
}

# =============================================================================
# Main
# =============================================================================
case "${1:-all}" in
    ca)
        generate_ca
        ;;
    server)
        generate_server
        ;;
    client)
        generate_client "${2:-tenant-test}"
        ;;
    all)
        generate_ca
        generate_server
        generate_client "${2:-tenant-test}"
        ;;
    *)
        echo "Usage: $0 {ca|server|client <tenant_id>|all [tenant_id]}"
        echo ""
        echo "Examples:"
        echo "  $0 all                    # Generate CA, server, and default client"
        echo "  $0 all my-tenant-123      # Generate all with custom tenant_id"
        echo "  $0 client another-tenant  # Generate only client cert"
        exit 1
        ;;
esac

echo ""
echo "=== Certificate generation complete ==="
echo ""
echo "Files created:"
ls -la *.crt *.key 2>/dev/null || true
echo ""
echo "Client certificates:"
ls -la clients/*/client.crt 2>/dev/null || true
