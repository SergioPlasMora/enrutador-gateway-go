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

	// Configurar puertos
	httpPort := 8080
	flightPort := 8815

	if p := os.Getenv("HTTP_PORT"); p != "" {
		fmt.Sscanf(p, "%d", &httpPort)
	}
	if p := os.Getenv("FLIGHT_PORT"); p != "" {
		fmt.Sscanf(p, "%d", &flightPort)
	}

	// Determinar modo de operación
	mode, _ := config["connector_mode"].(string)
	if mode == "" {
		mode = "websocket" // Default para compatibilidad
	}

	log.Printf("[Main] Connector mode: %s", mode)

	// Determinar directorio de templates
	execPath, _ := os.Executable()
	templatesDir := filepath.Join(filepath.Dir(execPath), "templates")
	if _, err := os.Stat(templatesDir); os.IsNotExist(err) {
		templatesDir = "templates"
	}
	staticServer := NewStaticServer(templatesDir)

	// Registrar rutas estáticas
	http.Handle("/dashboard/", http.StripPrefix("/dashboard/", staticServer))
	http.Handle("/dashboard", http.RedirectHandler("/dashboard/", http.StatusMovedPermanently))

	if mode == "grpc" {
		// Modo gRPC: usar ConnectorRegistry
		registry := NewConnectorRegistry()

		// Registrar conectores desde configuración
		if connectors, ok := config["connectors"].(map[string]string); ok {
			for tenantID, address := range connectors {
				if err := registry.RegisterConnector(tenantID, address); err != nil {
					log.Printf("[Main] Warning: failed to register %s: %v", tenantID, err)
				}
			}
		}

		// Crear servidor WebSocket para browsers (versión gRPC)
		browserWS := NewBrowserWSServerGRPC(registry)
		http.HandleFunc("/ws/browser", browserWS.HandleConnection)

		// Iniciar servidor HTTP
		go func() {
			addr := fmt.Sprintf(":%d", httpPort)
			log.Printf("[Main] HTTP server starting on %s", addr)
			if err := http.ListenAndServe(addr, nil); err != nil {
				log.Fatalf("[Main] HTTP server error: %v", err)
			}
		}()

		// Iniciar Flight Server para clientes gRPC externos (unified-evaluator)
		flightServerGRPC := NewFlightServerGRPC(registry, flightPort)
		go func() {
			if err := flightServerGRPC.Start(); err != nil {
				log.Fatalf("[Main] Flight server error: %v", err)
			}
		}()

		log.Printf("[Main] gRPC mode: connectors communicate via Arrow Flight")

	} else {
		// Modo WebSocket: usar ConnectionManager (compatibilidad)
		manager := NewConnectionManager()

		wsServer := NewWebSocketServer(manager, httpPort)
		browserWS := NewBrowserWSServer(manager)

		http.HandleFunc("/ws/browser", browserWS.HandleConnection)

		// Iniciar servidor WebSocket/HTTP
		go func() {
			if err := wsServer.Start(); err != nil {
				log.Fatalf("[Main] HTTP server error: %v", err)
			}
		}()

		// Iniciar Flight server (para clientes externos)
		flightServer := NewFlightServer(manager, flightPort)
		go func() {
			if err := flightServer.Start(); err != nil {
				log.Fatalf("[Main] Flight server error: %v", err)
			}
		}()

		log.Printf("[Main] WebSocket mode: connectors communicate via WebSocket")
	}

	log.Printf("[Main] Gateway started - HTTP: %d, Mode: %s", httpPort, mode)
	log.Printf("[Main] Dashboard available at: http://localhost:%d/dashboard/", httpPort)

	// Esperar señal de terminación
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	<-sigChan

	log.Println("[Main] Shutting down...")
}

// loadConfig carga configuración desde config.yaml
func loadConfig() map[string]interface{} {
	config := make(map[string]interface{})

	file, err := os.Open("config.yaml")
	if err != nil {
		log.Printf("[Config] No config.yaml found, using defaults (websocket mode)")
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
