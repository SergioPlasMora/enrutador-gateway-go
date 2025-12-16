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

	// Registrar rutas HTTP
	browserWS := NewBrowserWSServerGRPC(registry)
	http.HandleFunc("/ws/browser", browserWS.HandleConnection)
	http.Handle("/dashboard/", http.StripPrefix("/dashboard/", staticServer))
	http.Handle("/dashboard", http.RedirectHandler("/dashboard/", http.StatusMovedPermanently))

	// Health check endpoint
	http.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"status":"ok"}`))
	})

	// Iniciar servidor HTTP (Dashboard + WebSocket para browsers)
	go func() {
		addr := fmt.Sprintf(":%d", httpPort)
		log.Printf("[Main] HTTP server starting on %s", addr)
		if err := http.ListenAndServe(addr, nil); err != nil {
			log.Fatalf("[Main] HTTP server error: %v", err)
		}
	}()

	// Iniciar Flight Server (para CLI clients como unified-evaluator)
	flightServer := NewFlightServerGRPC(registry, flightPort)
	go func() {
		if err := flightServer.Start(); err != nil {
			log.Fatalf("[Main] Flight server error: %v", err)
		}
	}()

	log.Printf("[Main] Gateway started")
	log.Printf("[Main]   HTTP/WebSocket: :%d", httpPort)
	log.Printf("[Main]   Arrow Flight:   :%d", flightPort)
	log.Printf("[Main]   Dashboard:      http://localhost:%d/dashboard/", httpPort)

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
