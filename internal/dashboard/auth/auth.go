package auth

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"net/http"
	"sort"
	"sync"
	"time"

	"github.com/FairForge/vaultaire/internal/audit"
)

// contextKey is an unexported type for context keys in this package.
type contextKey int

const (
	// SessionKey is the context key for the current session data.
	SessionKey contextKey = iota
)

// SessionData is the payload stored in request context after authentication.
// IPAddress and UserAgent are populated at Create time from the originating
// request. They are read-only after creation and shown on the "active
// sessions" list on the settings page.
type SessionData struct {
	UserID    string
	TenantID  string
	Email     string
	Role      string
	IPAddress string
	UserAgent string
}

// GetSession extracts session data from a request context.
// Returns nil if no session is present (unauthenticated request).
func GetSession(ctx context.Context) *SessionData {
	sd, _ := ctx.Value(SessionKey).(*SessionData)
	return sd
}

// SessionInfo is the display-oriented view of a persisted session used by
// the "active sessions" settings page. It wraps SessionData with the token
// ID and timestamps so the UI can render a table with a "this device" badge.
type SessionInfo struct {
	ID           string // Session token (also the primary key).
	UserID       string
	IPAddress    string
	UserAgent    string
	CreatedAt    time.Time
	LastActiveAt time.Time
	ExpiresAt    time.Time
}

// --- SessionStore interface ---

// SessionStore abstracts session persistence so that tests can use an
// in-memory implementation while production uses PostgreSQL.
type SessionStore interface {
	Create(ctx context.Context, sd SessionData, ttl time.Duration) (token string, err error)
	Get(ctx context.Context, token string) (*SessionData, error)
	Delete(ctx context.Context, token string) error
	DeleteByUserID(ctx context.Context, userID string) error
	// ListByUserID returns all non-expired sessions for a user, newest first.
	ListByUserID(ctx context.Context, userID string) ([]SessionInfo, error)
	// DeleteByUserIDExcept deletes all sessions for a user except the given
	// token. Used to implement "sign out all other devices" without
	// logging out the device that issued the request.
	DeleteByUserIDExcept(ctx context.Context, userID, exceptToken string) error
}

// --- In-memory implementation (tests, local dev without DB) ---

type memSession struct {
	data         SessionData
	createdAt    time.Time
	lastActiveAt time.Time
	expiresAt    time.Time
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
	now := time.Now()
	m.sessions[token] = &memSession{
		data:         sd,
		createdAt:    now,
		lastActiveAt: now,
		expiresAt:    now.Add(ttl),
	}
	return token, nil
}

func (m *MemoryStore) Get(_ context.Context, token string) (*SessionData, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	s, ok := m.sessions[token]
	if !ok || time.Now().After(s.expiresAt) {
		return nil, nil
	}
	// Touch last-active on every Get so the settings page can show a
	// meaningful "last active" timestamp per device.
	s.lastActiveAt = time.Now()
	cp := s.data
	return &cp, nil
}

func (m *MemoryStore) Delete(_ context.Context, token string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.sessions, token)
	return nil
}

func (m *MemoryStore) DeleteByUserID(_ context.Context, userID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for token, s := range m.sessions {
		if s.data.UserID == userID {
			delete(m.sessions, token)
		}
	}
	return nil
}

func (m *MemoryStore) DeleteByUserIDExcept(_ context.Context, userID, exceptToken string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for token, s := range m.sessions {
		if s.data.UserID == userID && token != exceptToken {
			delete(m.sessions, token)
		}
	}
	return nil
}

func (m *MemoryStore) ListByUserID(_ context.Context, userID string) ([]SessionInfo, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	now := time.Now()
	out := make([]SessionInfo, 0)
	for token, s := range m.sessions {
		if s.data.UserID != userID || now.After(s.expiresAt) {
			continue
		}
		out = append(out, SessionInfo{
			ID:           token,
			UserID:       s.data.UserID,
			IPAddress:    s.data.IPAddress,
			UserAgent:    s.data.UserAgent,
			CreatedAt:    s.createdAt,
			LastActiveAt: s.lastActiveAt,
			ExpiresAt:    s.expiresAt,
		})
	}
	// Newest first by LastActiveAt so the "current" device sorts to top.
	sort.Slice(out, func(i, j int) bool {
		return out[i].LastActiveAt.After(out[j].LastActiveAt)
	})
	return out, nil
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
		INSERT INTO dashboard_sessions
		    (id, user_id, tenant_id, email, role, ip_address, user_agent, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
	`, token, sd.UserID, sd.TenantID, sd.Email, sd.Role,
		sd.IPAddress, sd.UserAgent, time.Now().Add(ttl))
	if err != nil {
		return "", fmt.Errorf("insert session: %w", err)
	}
	return token, nil
}

// Get returns the session for a token and atomically refreshes its
// last_active_at timestamp. The single UPDATE ... RETURNING round-trip keeps
// the "last active" column in sync without a second query per request.
func (d *DBStore) Get(ctx context.Context, token string) (*SessionData, error) {
	sd := &SessionData{}
	var ip, ua sql.NullString
	err := d.db.QueryRowContext(ctx, `
		UPDATE dashboard_sessions
		SET last_active_at = NOW()
		WHERE id = $1 AND expires_at > NOW()
		RETURNING user_id, tenant_id, email, role, ip_address, user_agent
	`, token).Scan(&sd.UserID, &sd.TenantID, &sd.Email, &sd.Role, &ip, &ua)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("query session: %w", err)
	}
	if ip.Valid {
		sd.IPAddress = ip.String
	}
	if ua.Valid {
		sd.UserAgent = ua.String
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

func (d *DBStore) DeleteByUserID(ctx context.Context, userID string) error {
	_, err := d.db.ExecContext(ctx, `DELETE FROM dashboard_sessions WHERE user_id = $1`, userID)
	if err != nil {
		return fmt.Errorf("delete sessions by user: %w", err)
	}
	return nil
}

func (d *DBStore) DeleteByUserIDExcept(ctx context.Context, userID, exceptToken string) error {
	_, err := d.db.ExecContext(ctx,
		`DELETE FROM dashboard_sessions WHERE user_id = $1 AND id <> $2`,
		userID, exceptToken)
	if err != nil {
		return fmt.Errorf("delete sessions by user except: %w", err)
	}
	return nil
}

func (d *DBStore) ListByUserID(ctx context.Context, userID string) ([]SessionInfo, error) {
	rows, err := d.db.QueryContext(ctx, `
		SELECT id, user_id, ip_address, user_agent, created_at, last_active_at, expires_at
		FROM dashboard_sessions
		WHERE user_id = $1 AND expires_at > NOW()
		ORDER BY last_active_at DESC
	`, userID)
	if err != nil {
		return nil, fmt.Errorf("list sessions: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := make([]SessionInfo, 0)
	for rows.Next() {
		var si SessionInfo
		var ip, ua sql.NullString
		if err := rows.Scan(&si.ID, &si.UserID, &ip, &ua,
			&si.CreatedAt, &si.LastActiveAt, &si.ExpiresAt); err != nil {
			return nil, fmt.Errorf("scan session: %w", err)
		}
		if ip.Valid {
			si.IPAddress = ip.String
		}
		if ua.Valid {
			si.UserAgent = ua.String
		}
		out = append(out, si)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate sessions: %w", err)
	}
	return out, nil
}

// CleanupExpired deletes expired sessions and returns how many. The API
// server runs it hourly as the `session_cleanup` job; the error used to be
// dropped.
func (d *DBStore) CleanupExpired(ctx context.Context) (int64, error) {
	res, err := d.db.ExecContext(ctx, `DELETE FROM dashboard_sessions WHERE expires_at < NOW()`)
	if err != nil {
		return 0, fmt.Errorf("delete expired sessions: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// --- Middleware ---

// SessionCookieName is the name of the cookie that holds the dashboard
// session token. Exported so handlers outside this package can read the
// current session token (e.g., password change keeping the current device).
//
// The `__Host-` prefix (WP-R12-14, RFC 6265bis §4.1.3.2): a browser accepts
// a cookie with this name only from a secure origin, only with `Secure`,
// only with `Path=/` and never with a `Domain` attribute — so it is
// host-only by construction and a page on a sibling subdomain
// (cdn.stored.ge serves customer content) can no longer plant a session
// cookie for the dashboard, the login-CSRF residue R12 accepted. The CSRF
// token is HMAC(key, this cookie's value), so it follows the rename.
const SessionCookieName = "__Host-vaultaire_session"

// LegacySessionCookieName is what the session cookie was called before the
// prefix. Nothing reads it; a browser that still sends one is told to drop
// it the next time a session cookie is written or cleared, so a user whose
// session predates the change signs in once more and carries one cookie.
const LegacySessionCookieName = "vaultaire_session"

// cookieName is kept as an internal alias for compatibility with the older
// middleware/cookie helpers below.
const cookieName = SessionCookieName

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
			ctx = audit.WithActor(ctx, sd.UserID)
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

// SetSessionCookie writes the session cookie on the response: Secure,
// HttpOnly, SameSite=Lax, Path=/ and no Domain — the attributes the
// `__Host-` prefix requires, or the browser discards it.
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
	expireLegacySessionCookie(w)
}

// ClearSessionCookie removes the session cookie. The clearing cookie carries
// the same attributes as the one it clears: a `__Host-` cookie without
// `Secure` and `Path=/` is refused by the browser and the session cookie
// would stay.
func ClearSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     cookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   true,
		MaxAge:   -1,
	})
	expireLegacySessionCookie(w)
}

func expireLegacySessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     LegacySessionCookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   true,
		MaxAge:   -1,
	})
}

// --- helpers ---

// MaxUserAgentLen bounds the User-Agent string stored per session to the
// column width in `dashboard_sessions` (VARCHAR 512). UAs longer than this
// are extremely rare but worth guarding against so INSERT never fails.
const MaxUserAgentLen = 512

// TruncateUserAgent clips an overly long User-Agent header so it fits in
// `dashboard_sessions.user_agent`.
func TruncateUserAgent(ua string) string {
	if len(ua) > MaxUserAgentLen {
		return ua[:MaxUserAgentLen]
	}
	return ua
}

func generateToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate session token: %w", err)
	}
	return hex.EncodeToString(b), nil
}
