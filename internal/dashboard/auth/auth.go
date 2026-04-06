package auth

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"net/http"
	"sync"
	"time"
)

// contextKey is an unexported type for context keys in this package.
type contextKey int

const (
	// SessionKey is the context key for the current session data.
	SessionKey contextKey = iota
)

// SessionData is the payload stored in request context after authentication.
type SessionData struct {
	UserID   string
	TenantID string
	Email    string
	Role     string
}

// GetSession extracts session data from a request context.
// Returns nil if no session is present (unauthenticated request).
func GetSession(ctx context.Context) *SessionData {
	sd, _ := ctx.Value(SessionKey).(*SessionData)
	return sd
}

// --- SessionStore interface ---

// SessionStore abstracts session persistence so that tests can use an
// in-memory implementation while production uses PostgreSQL.
type SessionStore interface {
	Create(ctx context.Context, sd SessionData, ttl time.Duration) (token string, err error)
	Get(ctx context.Context, token string) (*SessionData, error)
	Delete(ctx context.Context, token string) error
}

// --- In-memory implementation (tests, local dev without DB) ---

type memSession struct {
	data      SessionData
	expiresAt time.Time
}

// MemoryStore is an in-memory session store.
type MemoryStore struct {
	mu       sync.RWMutex
	sessions map[string]*memSession
}

// NewMemoryStore creates an in-memory session store.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{sessions: make(map[string]*memSession)}
}

func (m *MemoryStore) Create(_ context.Context, sd SessionData, ttl time.Duration) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	token, err := generateToken()
	if err != nil {
		return "", err
	}
	m.sessions[token] = &memSession{data: sd, expiresAt: time.Now().Add(ttl)}
	return token, nil
}

func (m *MemoryStore) Get(_ context.Context, token string) (*SessionData, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	s, ok := m.sessions[token]
	if !ok || time.Now().After(s.expiresAt) {
		return nil, nil
	}
	cp := s.data
	return &cp, nil
}

func (m *MemoryStore) Delete(_ context.Context, token string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.sessions, token)
	return nil
}

// --- PostgreSQL implementation ---

// DBStore persists sessions in the dashboard_sessions table.
type DBStore struct {
	db *sql.DB
}

// NewDBStore creates a PostgreSQL-backed session store.
func NewDBStore(db *sql.DB) *DBStore {
	return &DBStore{db: db}
}

func (d *DBStore) Create(ctx context.Context, sd SessionData, ttl time.Duration) (string, error) {
	token, err := generateToken()
	if err != nil {
		return "", err
	}
	_, err = d.db.ExecContext(ctx, `
		INSERT INTO dashboard_sessions (id, user_id, tenant_id, email, role, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6)
	`, token, sd.UserID, sd.TenantID, sd.Email, sd.Role, time.Now().Add(ttl))
	if err != nil {
		return "", fmt.Errorf("insert session: %w", err)
	}
	return token, nil
}

func (d *DBStore) Get(ctx context.Context, token string) (*SessionData, error) {
	sd := &SessionData{}
	err := d.db.QueryRowContext(ctx, `
		SELECT user_id, tenant_id, email, role
		FROM dashboard_sessions
		WHERE id = $1 AND expires_at > NOW()
	`, token).Scan(&sd.UserID, &sd.TenantID, &sd.Email, &sd.Role)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("query session: %w", err)
	}
	return sd, nil
}

func (d *DBStore) Delete(ctx context.Context, token string) error {
	_, err := d.db.ExecContext(ctx, `DELETE FROM dashboard_sessions WHERE id = $1`, token)
	if err != nil {
		return fmt.Errorf("delete session: %w", err)
	}
	return nil
}

// StartCleanup launches a goroutine that deletes expired sessions every hour.
// It stops when ctx is cancelled.
func (d *DBStore) StartCleanup(ctx context.Context) {
	go func() {
		ticker := time.NewTicker(1 * time.Hour)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				_, _ = d.db.ExecContext(ctx, `DELETE FROM dashboard_sessions WHERE expires_at < NOW()`)
			}
		}
	}()
}

// --- Middleware ---

const cookieName = "vaultaire_session"

// RequireSession is HTTP middleware that checks for a valid session cookie.
// If valid, it injects SessionData into the request context.
// If invalid or missing, it redirects to /login.
func RequireSession(store SessionStore) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			cookie, err := r.Cookie(cookieName)
			if err != nil {
				http.Redirect(w, r, "/login", http.StatusSeeOther)
				return
			}
			sd, err := store.Get(r.Context(), cookie.Value)
			if err != nil || sd == nil {
				http.Redirect(w, r, "/login", http.StatusSeeOther)
				return
			}
			ctx := context.WithValue(r.Context(), SessionKey, sd)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// RequireAdmin wraps RequireSession and additionally checks role == "admin".
func RequireAdmin(store SessionStore) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return RequireSession(store)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			sd := GetSession(r.Context())
			if sd == nil || sd.Role != "admin" {
				http.Error(w, "Forbidden", http.StatusForbidden)
				return
			}
			next.ServeHTTP(w, r)
		}))
	}
}

// SetSessionCookie writes the session cookie on the response.
func SetSessionCookie(w http.ResponseWriter, token string, ttl time.Duration) {
	http.SetCookie(w, &http.Cookie{
		Name:     cookieName,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   true,
		MaxAge:   int(ttl.Seconds()),
	})
}

// ClearSessionCookie removes the session cookie.
func ClearSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     cookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		MaxAge:   -1,
	})
}

// --- BasicAuth (kept for admin API endpoints) ---

// BasicAuth validates HTTP Basic Authentication credentials.
type BasicAuth struct {
	username string
	password string
}

// NewBasicAuth creates a BasicAuth validator.
func NewBasicAuth(username, password string) *BasicAuth {
	return &BasicAuth{username: username, password: password}
}

// Validate checks the request's basic auth credentials.
func (ba *BasicAuth) Validate(r *http.Request) bool {
	user, pass, ok := r.BasicAuth()
	if !ok {
		return false
	}
	return user == ba.username && pass == ba.password
}

// --- helpers ---

func generateToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate session token: %w", err)
	}
	return hex.EncodeToString(b), nil
}
