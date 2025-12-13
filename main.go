package main

import (
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
)

func main() {
	log.Println("=== Arrow Flight Gateway (Go) ===")

	// Crear connection manager
	manager := NewConnectionManager()

	// Configurar puertos (desde env o defaults)
	wsPort := 8080
	flightPort := 8815

	if p := os.Getenv("WS_PORT"); p != "" {
		fmt.Sscanf(p, "%d", &wsPort)
	}
	if p := os.Getenv("FLIGHT_PORT"); p != "" {
		fmt.Sscanf(p, "%d", &flightPort)
	}

	// Iniciar WebSocket server en goroutine
	wsServer := NewWebSocketServer(manager, wsPort)
	go func() {
		if err := wsServer.Start(); err != nil {
			log.Fatalf("[Main] WebSocket server error: %v", err)
		}
	}()

	// Iniciar Flight server en goroutine
	flightServer := NewFlightServer(manager, flightPort)
	go func() {
		if err := flightServer.Start(); err != nil {
			log.Fatalf("[Main] Flight server error: %v", err)
		}
	}()

	log.Printf("[Main] Gateway started - WebSocket: %d, Flight: %d", wsPort, flightPort)

	// Esperar señal de terminación
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	<-sigChan

	log.Println("[Main] Shutting down...")
}
