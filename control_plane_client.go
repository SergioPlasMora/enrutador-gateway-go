package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"time"
)

// ControlPlaneClient handles communication with the Control Plane (FastAPI)
type ControlPlaneClient struct {
	baseURL    string
	apiKey     string
	httpClient *http.Client
}

// SessionInfo contains validated session information from Control Plane
type SessionInfo struct {
	Valid     bool   `json:"valid"`
	SessionID string `json:"session_id,omitempty"`
	UserID    string `json:"user_id,omitempty"`
	CuentaID  string `json:"cuenta_id,omitempty"`
	EdgeID    string `json:"edge_id,omitempty"`
	Dataset   string `json:"dataset,omitempty"`
	ExpiresAt string `json:"expires_at,omitempty"`
}

// NewControlPlaneClient creates a new Control Plane client
func NewControlPlaneClient() *ControlPlaneClient {
	baseURL := os.Getenv("CONTROL_PLANE_URL")
	if baseURL == "" {
		baseURL = "http://localhost:8000/api/v2/control"
	}

	apiKey := os.Getenv("INTERNAL_API_KEY")
	if apiKey == "" {
		apiKey = "dev-internal-key-change-in-production"
	}

	return &ControlPlaneClient{
		baseURL: baseURL,
		apiKey:  apiKey,
		httpClient: &http.Client{
			Timeout: 5 * time.Second,
		},
	}
}

// ValidateSession validates a session with the Control Plane
// Returns session info if valid, nil if invalid
func (c *ControlPlaneClient) ValidateSession(sessionID string) (*SessionInfo, error) {
	url := fmt.Sprintf("%s/validate/%s", c.baseURL, sessionID)

	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	// Add internal API key header
	req.Header.Set("X-Internal-API-Key", c.apiKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return nil, fmt.Errorf("internal API key rejected")
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected status: %d", resp.StatusCode)
	}

	var info SessionInfo
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
		return nil, fmt.Errorf("failed to decode response: %w", err)
	}

	if !info.Valid {
		return nil, nil // Session not valid (expired, revoked, or not found)
	}

	log.Printf("[ControlPlane] Session %s validated: user=%s cuenta=%s edge=%s",
		sessionID, info.UserID, info.CuentaID, info.EdgeID)

	return &info, nil
}

// EdgeHeartbeat sends a heartbeat to register this edge as online
func (c *ControlPlaneClient) EdgeHeartbeat(edgeID string) error {
	url := fmt.Sprintf("%s/edge/%s/heartbeat", c.baseURL, edgeID)

	req, err := http.NewRequest("POST", url, nil)
	if err != nil {
		return fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("X-Internal-API-Key", c.apiKey)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("heartbeat request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("heartbeat failed with status: %d", resp.StatusCode)
	}

	return nil
}
