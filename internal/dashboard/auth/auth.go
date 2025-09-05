package auth

import (
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"sync"
	"time"
)

type BasicAuth struct {
	username string
	password string
}

func NewBasicAuth(username, password string) *BasicAuth {
	return &BasicAuth{
		username: username,
		password: password,
	}
}

func (ba *BasicAuth) Validate(r *http.Request) bool {
	user, pass, ok := r.BasicAuth()
	if !ok {
		return false
	}
	return user == ba.username && pass == ba.password
}

type Session struct {
	UserID    string
	ExpiresAt time.Time
}

type SessionManager struct {
	mu       sync.RWMutex
	sessions map[string]*Session
	secret   string
	ttl      time.Duration
}

func NewSessionManager(secret string, ttl time.Duration) *SessionManager {
	return &SessionManager{
		sessions: make(map[string]*Session),
		secret:   secret,
		ttl:      ttl,
	}
}

func (sm *SessionManager) CreateSession(userID string) string {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	// Generate random session ID
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	sessionID := hex.EncodeToString(b)

	sm.sessions[sessionID] = &Session{
		UserID:    userID,
		ExpiresAt: time.Now().Add(sm.ttl),
	}

	return sessionID
}

func (sm *SessionManager) GetSession(sessionID string) string {
	sm.mu.RLock()
	defer sm.mu.RUnlock()

	session, exists := sm.sessions[sessionID]
	if !exists {
		return ""
	}

	if time.Now().After(session.ExpiresAt) {
		delete(sm.sessions, sessionID)
		return ""
	}

	return session.UserID
}

func AuthMiddleware(sm *SessionManager) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Check for session cookie
			cookie, err := r.Cookie("session")
			if err != nil || sm.GetSession(cookie.Value) == "" {
				http.Redirect(w, r, "/login", http.StatusSeeOther)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
