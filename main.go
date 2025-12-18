package main

import (
	"bufio"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
)

func main() {
	log.Println("=== Arrow Flight Gateway (Go) ===")

	// Leer configuración
	config := loadConfig()

	// Configurar puerto HTTP/WebSocket
	httpPort := 8081

	if p := os.Getenv("HTTP_PORT"); p != "" {
		fmt.Sscanf(p, "%d", &httpPort)
	}

	// Crear ConnectorRegistry para conexiones gRPC a Data Connectors
	registry := NewConnectorRegistry()

	// Registrar conectores desde configuración
	if connectors, ok := config["connectors"].(map[string]string); ok {
		for tenantID, address := range connectors {
			if err := registry.RegisterConnector(tenantID, address); err != nil {
				log.Printf("[Main] Warning: failed to register %s: %v", tenantID, err)
			}
		}
	}

	// Determinar directorio de templates
	execPath, _ := os.Executable()
	templatesDir := filepath.Join(filepath.Dir(execPath), "templates")
	if _, err := os.Stat(templatesDir); os.IsNotExist(err) {
		templatesDir = "templates"
	}
	staticServer := NewStaticServer(templatesDir)

	// === CDP Edge Architecture Components ===

	// Control Plane client for session validation
	controlPlane := NewControlPlaneClient()
	log.Printf("[Main] Control Plane URL: %s", controlPlane.baseURL)

	// Session manager for tracking active sessions
	sessionManager := NewSessionManager(controlPlane)

	// Redis subscriber for real-time revocation events
	redisSubscriber := NewRedisSubscriber(sessionManager)
	if err := redisSubscriber.Start(); err != nil {
		log.Printf("[Main] Warning: Redis subscriber not started (revocation events delayed): %v", err)
	}

	// StreamServerV2 with Control Plane validation
	streamServerV2 := NewStreamServerV2(registry, sessionManager)

	// === End CDP Edge Components ===

	// WebSocket endpoint for Data Connectors (reverse tunnel mode)
	connectorWS := NewConnectorWSServer(registry)
	http.HandleFunc("/ws/connect", connectorWS.HandleConnection)
	log.Printf("[Main] WebSocket /ws/connect endpoint enabled for connector reverse tunnels")

	http.Handle("/dashboard/", http.StripPrefix("/dashboard/", staticServer))
	http.Handle("/dashboard", http.RedirectHandler("/dashboard/", http.StatusMovedPermanently))

	// CDP Edge endpoint with Control Plane validation
	// URL format: /stream/{session_id}
	http.HandleFunc("/stream/", streamServerV2.HandleStream)
	log.Printf("[Main] CDP Edge /stream/{session_id} endpoint enabled")

	// Health check endpoint
	http.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		activeSessions, activeUsers := sessionManager.Stats()
		redisConnected := redisSubscriber.IsConnected()

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, `{"status":"ok","sessions":%d,"users":%d,"redis":%t}`,
			activeSessions, activeUsers, redisConnected)
	})

	// Iniciar servidor HTTP (Dashboard + WebSocket para browsers)
	go func() {
		addr := fmt.Sprintf(":%d", httpPort)
		log.Printf("[Main] HTTP server starting on %s", addr)
		if err := http.ListenAndServe(addr, nil); err != nil {
			log.Fatalf("[Main] HTTP server error: %v", err)
		}
	}()

	log.Printf("[Main] Gateway started")
	log.Printf("[Main]   HTTP/WebSocket: :%d", httpPort)
	log.Printf("[Main]   Dashboard:      http://localhost:%d/dashboard/", httpPort)
	log.Printf("[Main]   Stream (CDP):   ws://localhost:%d/stream/{session_id}", httpPort)

	// Esperar señal de terminación
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	<-sigChan

	log.Println("[Main] Shutting down...")

	// Graceful shutdown
	redisSubscriber.Stop()
	sessionManager.Stop()
}

// loadConfig carga configuración desde config.yaml
func loadConfig() map[string]interface{} {
	config := make(map[string]interface{})

	file, err := os.Open("config.yaml")
	if err != nil {
		log.Printf("[Config] No config.yaml found, using defaults")
		return config
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	var currentSection string
	connectors := make(map[string]string)

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())

		// Skip comments and empty lines
		if strings.HasPrefix(line, "#") || line == "" {
			continue
		}

		// Check for section
		if strings.HasSuffix(line, ":") && !strings.Contains(line, " ") {
			currentSection = strings.TrimSuffix(line, ":")
			continue
		}

		// Parse key-value
		parts := strings.SplitN(line, ":", 2)
		if len(parts) != 2 {
			continue
		}

		key := strings.TrimSpace(parts[0])
		value := strings.TrimSpace(parts[1])
		value = strings.Trim(value, "\"'")

		if currentSection == "connectors" {
			connectors[key] = value
		} else if currentSection == "" {
			config[key] = value
		}
	}

	if len(connectors) > 0 {
		config["connectors"] = connectors
	}

	return config
}
