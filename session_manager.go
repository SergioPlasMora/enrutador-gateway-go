package main

import (
	"context"
	"log"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// Session represents an active streaming session between browser and edge
type Session struct {
	ID        string
	UserID    string
	CuentaID  string
	EdgeID    string
	Dataset   string
	ExpiresAt time.Time
	CreatedAt time.Time

	// Active connections
	BrowserConn *websocket.Conn
	EdgeConn    *websocket.Conn

	// Synchronization
	mu       sync.RWMutex
	done     chan struct{}
	isActive bool
}

// NewSession creates a new session from Control Plane info
func NewSession(info *SessionInfo) *Session {
	// Try multiple date formats since Control Plane may send ISO8601 without timezone
	var expiresAt time.Time
	var err error

	// Try RFC3339 first (with timezone)
	expiresAt, err = time.Parse(time.RFC3339, info.ExpiresAt)
	if err != nil {
		// Try ISO8601 without timezone (Python's datetime.isoformat())
		expiresAt, err = time.Parse("2006-01-02T15:04:05.999999", info.ExpiresAt)
		if err != nil {
			// Try without microseconds
			expiresAt, err = time.Parse("2006-01-02T15:04:05", info.ExpiresAt)
			if err != nil {
				log.Printf("[Session] Failed to parse ExpiresAt '%s', using 15 min default", info.ExpiresAt)
				expiresAt = time.Now().Add(15 * time.Minute)
			}
		}
	}

	return &Session{
		ID:        info.SessionID,
		UserID:    info.UserID,
		CuentaID:  info.CuentaID,
		EdgeID:    info.EdgeID,
		Dataset:   info.Dataset,
		ExpiresAt: expiresAt,
		CreatedAt: time.Now(),
		done:      make(chan struct{}),
		isActive:  true,
	}
}

// IsExpired checks if the session has expired
func (s *Session) IsExpired() bool {
	return time.Now().After(s.ExpiresAt)
}

// Close closes the session and all associated connections
func (s *Session) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()

	if !s.isActive {
		return
	}

	s.isActive = false
	close(s.done)

	if s.BrowserConn != nil {
		s.BrowserConn.Close()
	}
	if s.EdgeConn != nil {
		s.EdgeConn.Close()
	}

	log.Printf("[Session] Closed session %s for user %s", s.ID, s.UserID)
}

// Done returns a channel that's closed when the session ends
func (s *Session) Done() <-chan struct{} {
	return s.done
}

// SetBrowserConn sets the browser WebSocket connection
func (s *Session) SetBrowserConn(conn *websocket.Conn) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.BrowserConn = conn
}

// SetEdgeConn sets the edge WebSocket connection
func (s *Session) SetEdgeConn(conn *websocket.Conn) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.EdgeConn = conn
}

// SessionManager manages active streaming sessions
type SessionManager struct {
	sessions      sync.Map // session_id -> *Session
	userSessions  sync.Map // user_id -> []session_id
	controlPlane  *ControlPlaneClient
	cleanupTicker *time.Ticker
	ctx           context.Context
	cancel        context.CancelFunc
}

// NewSessionManager creates a new session manager
func NewSessionManager(controlPlane *ControlPlaneClient) *SessionManager {
	ctx, cancel := context.WithCancel(context.Background())

	sm := &SessionManager{
		controlPlane:  controlPlane,
		cleanupTicker: time.NewTicker(30 * time.Second),
		ctx:           ctx,
		cancel:        cancel,
	}

	// Start background cleanup of expired sessions
	go sm.cleanupLoop()

	return sm
}

// GetOrCreateSession gets an existing session or validates and creates from Control Plane
func (sm *SessionManager) GetOrCreateSession(sessionID string) (*Session, error) {
	// Check cache first
	if cached, ok := sm.sessions.Load(sessionID); ok {
		session := cached.(*Session)
		if !session.IsExpired() && session.isActive {
			return session, nil
		}
		// Expired or inactive, remove from cache
		sm.RemoveSession(sessionID)
	}

	// Validate with Control Plane
	info, err := sm.controlPlane.ValidateSession(sessionID)
	if err != nil {
		return nil, err
	}
	if info == nil {
		return nil, nil // Session not valid
	}

	// Create new session
	session := NewSession(info)
	sm.sessions.Store(sessionID, session)

	// Track by user
	sm.addUserSession(session.UserID, sessionID)

	log.Printf("[SessionManager] Created session %s for user %s", sessionID, session.UserID)
	return session, nil
}

// GetSession retrieves a session by ID (from cache only, no validation)
func (sm *SessionManager) GetSession(sessionID string) *Session {
	if cached, ok := sm.sessions.Load(sessionID); ok {
		session := cached.(*Session)
		if !session.IsExpired() && session.isActive {
			return session
		}
	}
	return nil
}

// RemoveSession removes a session from the manager
func (sm *SessionManager) RemoveSession(sessionID string) {
	if cached, ok := sm.sessions.Load(sessionID); ok {
		session := cached.(*Session)
		session.Close()

		// Remove from user sessions
		sm.removeUserSession(session.UserID, sessionID)
	}
	sm.sessions.Delete(sessionID)
}

// GetUserSessions gets all active sessions for a user
func (sm *SessionManager) GetUserSessions(userID string) []*Session {
	var sessions []*Session

	if sessionIDs, ok := sm.userSessions.Load(userID); ok {
		for _, sid := range sessionIDs.([]string) {
			if session := sm.GetSession(sid); session != nil {
				sessions = append(sessions, session)
			}
		}
	}

	return sessions
}

// RevokeUserSessions closes all sessions for a user
func (sm *SessionManager) RevokeUserSessions(userID string) int {
	sessions := sm.GetUserSessions(userID)
	for _, session := range sessions {
		sm.RemoveSession(session.ID)
	}
	return len(sessions)
}

// RevokeSession closes a specific session
func (sm *SessionManager) RevokeSession(sessionID string) bool {
	if session := sm.GetSession(sessionID); session != nil {
		sm.RemoveSession(sessionID)
		return true
	}
	return false
}

// addUserSession adds a session ID to user's session list
func (sm *SessionManager) addUserSession(userID, sessionID string) {
	existing, _ := sm.userSessions.Load(userID)
	var sessions []string
	if existing != nil {
		sessions = existing.([]string)
	}
	sessions = append(sessions, sessionID)
	sm.userSessions.Store(userID, sessions)
}

// removeUserSession removes a session ID from user's session list
func (sm *SessionManager) removeUserSession(userID, sessionID string) {
	existing, ok := sm.userSessions.Load(userID)
	if !ok {
		return
	}

	sessions := existing.([]string)
	var newSessions []string
	for _, sid := range sessions {
		if sid != sessionID {
			newSessions = append(newSessions, sid)
		}
	}

	if len(newSessions) > 0 {
		sm.userSessions.Store(userID, newSessions)
	} else {
		sm.userSessions.Delete(userID)
	}
}

// cleanupLoop periodically removes expired sessions
func (sm *SessionManager) cleanupLoop() {
	for {
		select {
		case <-sm.ctx.Done():
			return
		case <-sm.cleanupTicker.C:
			sm.cleanupExpiredSessions()
		}
	}
}

// cleanupExpiredSessions removes all expired sessions
func (sm *SessionManager) cleanupExpiredSessions() {
	var expired []string

	sm.sessions.Range(func(key, value interface{}) bool {
		session := value.(*Session)
		if session.IsExpired() || !session.isActive {
			expired = append(expired, key.(string))
		}
		return true
	})

	for _, sessionID := range expired {
		sm.RemoveSession(sessionID)
	}

	if len(expired) > 0 {
		log.Printf("[SessionManager] Cleaned up %d expired sessions", len(expired))
	}
}

// Stop stops the session manager
func (sm *SessionManager) Stop() {
	sm.cancel()
	sm.cleanupTicker.Stop()

	// Close all active sessions
	sm.sessions.Range(func(key, value interface{}) bool {
		session := value.(*Session)
		session.Close()
		return true
	})

	log.Println("[SessionManager] Stopped")
}

// Stats returns current session statistics
func (sm *SessionManager) Stats() (activeSessions int, activeUsers int) {
	sm.sessions.Range(func(key, value interface{}) bool {
		session := value.(*Session)
		if session.isActive && !session.IsExpired() {
			activeSessions++
		}
		return true
	})

	sm.userSessions.Range(func(key, value interface{}) bool {
		activeUsers++
		return true
	})

	return
}
