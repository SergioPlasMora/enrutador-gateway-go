package main

import (
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// StaticServer sirve archivos estáticos para el dashboard
type StaticServer struct {
	dir string
}

// NewStaticServer crea un servidor de archivos estáticos
func NewStaticServer(dir string) *StaticServer {
	return &StaticServer{dir: dir}
}

// ServeHTTP implementa http.Handler
func (s *StaticServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Limpiar path
	path := r.URL.Path
	if path == "/" || path == "" {
		path = "/dashboard.html"
	}

	// Construir path completo
	fullPath := filepath.Join(s.dir, filepath.Clean(path))

	// Verificar que el archivo existe
	info, err := os.Stat(fullPath)
	if err != nil || info.IsDir() {
		// Si es directorio o no existe, intentar dashboard.html
		fullPath = filepath.Join(s.dir, "dashboard.html")
		info, err = os.Stat(fullPath)
		if err != nil {
			http.NotFound(w, r)
			return
		}
	}

	// Establecer Content-Type
	contentType := getContentType(fullPath)
	w.Header().Set("Content-Type", contentType)

	// Servir archivo
	http.ServeFile(w, r, fullPath)
	log.Printf("[Static] Served: %s", path)
}

// getContentType retorna el content type basado en la extensión
func getContentType(path string) string {
	ext := strings.ToLower(filepath.Ext(path))
	switch ext {
	case ".html":
		return "text/html; charset=utf-8"
	case ".css":
		return "text/css; charset=utf-8"
	case ".js":
		return "application/javascript; charset=utf-8"
	case ".json":
		return "application/json; charset=utf-8"
	case ".png":
		return "image/png"
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".svg":
		return "image/svg+xml"
	case ".ico":
		return "image/x-icon"
	default:
		return "application/octet-stream"
	}
}
