package auth

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/FairForge/vaultaire/internal/audit"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/lib/pq"
	"golang.org/x/crypto/bcrypt"
)

// User represents a stored.ge customer
type User struct {
	ID            string
	Email         string
	PasswordHash  string
	Company       string
	TenantID      string // Link to their storage tenant
	EmailVerified bool
	CreatedAt     time.Time
	UpdatedAt     time.Time
	// PasswordChangedAt is stamped by a password change or reset; a JWT
	// issued before it is refused (WP-R5-10). Nil = never changed.
	PasswordChangedAt *time.Time
}

// Tenant represents an isolated storage namespace
type Tenant struct {
	ID        string
	UserID    string // Owner user
	AccessKey string // S3 access key
	SecretKey string // S3 secret key
	CreatedAt time.Time
}

// JWTClaims represents JWT token claims
type JWTClaims struct {
	UserID   string `json:"user_id"`
	Email    string `json:"email"`
	TenantID string `json:"tenant_id"`
	jwt.RegisteredClaims
}

// AuthService handles authentication.
//
// Runtime state (users, tenants, apiKeys, etc.) is kept in memory for
// fast O(1) lookups during request handling. sqlDB is used to persist
// new registrations so they survive process restarts.
type AuthService struct {
	db          Database
	sqlDB       *sql.DB // for persistent writes; nil in test mode
	jwtSecret   []byte
	users       map[string]*User          // email -> user
	tenants     map[string]*Tenant        // tenantID -> tenant
	apiKeys     map[string]*APIKey        // key -> apikey
	userIndex   map[string]*User          // userID -> user
	keyIndex    map[string]*Tenant        // accessKey -> tenant (for S3 auth)
	profiles    map[string]*ProfileUpdate // user profiles
	preferences map[string]*UserPreferences
	mfaSettings map[string]*MFASettings // userID -> MFA config
	// cacheMu guards the maps above AND the fields of the *User / *APIKey /
	// *Tenant values they hold: every reader takes RLock, every writer Lock,
	// and what leaves the service is a copy (WP-R5-6, proven by
	// TestAuthService_CredentialCacheIsRaceFree under -race). It began as the
	// guard against Evict (WP-R10-3) with most readers unlocked.
	cacheMu        sync.RWMutex
	totpUsed       map[string]map[string]time.Time // userID -> accepted TOTP codes inside the replay window (R5-15c, R12-38)
	mfaMu          sync.RWMutex
	verifySecret   []byte // HMAC key for email verification tokens
	resetRates     map[string][]time.Time
	resetMu        sync.Mutex
	signupsEnabled bool // when false, all account creation is rejected
	// signupsEnabledFn, when set, overrides signupsEnabled — 1.13 wires the
	// feature-flag service here so the `signups` flag (env default + DB
	// override) decides, flippable at runtime with no deploy.
	signupsEnabledFn func() bool
}

// ErrSignupsDisabled is returned by CreateUserWithTenant — the single chokepoint
// for the web form (/register), the JSON API (/auth/register), AND OAuth signup
// (CreateUserFromOAuth calls CreateUserWithTenant) — when public signups are
// turned off via SetSignupsEnabled(false). Gating this one function blocks every
// account-creation path at the source.
var ErrSignupsDisabled = errors.New("signups are currently disabled")

// ErrUserExists: the e-mail address already has an account.
var ErrUserExists = errors.New("user already exists")

// MinPasswordLength is the shortest password CreateUserWithTenant accepts.
const MinPasswordLength = 8

// ErrPasswordTooShort is returned by CreateUserWithTenant for a password
// shorter than MinPasswordLength (every signup entry point: web form,
// /auth/register API; OAuth passes no password).
var ErrPasswordTooShort = errors.New("password must be at least 8 characters")

// Database interface for auth operations
type Database interface {
	// Will be implemented with PostgreSQL
}

func generateRandomSecret() []byte {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return []byte(hex.EncodeToString([]byte("fallback-jwt-secret")))
	}
	return []byte(hex.EncodeToString(b))
}

// NewAuthService creates a new auth service.
// sqlDB may be nil (e.g. in tests); persistence is skipped when it is.
func NewAuthService(db Database, sqlDB *sql.DB) *AuthService {
	return &AuthService{
		db:             db,
		sqlDB:          sqlDB,
		jwtSecret:      generateRandomSecret(),
		users:          make(map[string]*User),
		tenants:        make(map[string]*Tenant),
		apiKeys:        make(map[string]*APIKey),
		userIndex:      make(map[string]*User),
		keyIndex:       make(map[string]*Tenant),
		profiles:       make(map[string]*ProfileUpdate),
		preferences:    make(map[string]*UserPreferences),
		mfaSettings:    make(map[string]*MFASettings),
		resetRates:     make(map[string][]time.Time),
		signupsEnabled: true, // default: signups allowed (prod sets SIGNUPS_ENABLED=false to close)
	}
}

// SetSignupsEnabled toggles public account creation. When disabled,
// CreateUserWithTenant rejects new accounts — which closes the web form, the
// /auth/register API, and OAuth signup, since all three funnel through it.
// Existing-user login (password or OAuth) is unaffected.
func (a *AuthService) SetSignupsEnabled(enabled bool) { a.signupsEnabled = enabled }

// SetSignupsEnabledFunc wires a dynamic source (the feature-flag service) as
// the authority on signups. Once set it overrides the static bool everywhere:
// the CreateUserWithTenant gate and the SignupsEnabled read path. Set during
// server construction, before any request is served.
func (a *AuthService) SetSignupsEnabledFunc(fn func() bool) { a.signupsEnabledFn = fn }

// SignupsEnabled reports whether public account creation is currently allowed.
func (a *AuthService) SignupsEnabled() bool {
	if a.signupsEnabledFn != nil {
		return a.signupsEnabledFn()
	}
	return a.signupsEnabled
}

// SetJWTSecret overrides the default JWT signing key.
// Call this from main.go with the value from the JWT_SECRET env var.
func (a *AuthService) SetJWTSecret(secret string) {
	if secret != "" {
		a.jwtSecret = []byte(secret)
	}
}

// LoadFromDB populates the in-memory maps from PostgreSQL so that
// authentication works immediately after a restart/deploy without
// requiring every user to re-register.
//
// It loads users, tenants, and the keyIndex (accessKey → tenant) which
// is the map ValidateS3Request uses to authorize every S3 call.
// If sqlDB is nil (tests), this is a no-op.
func (a *AuthService) LoadFromDB(ctx context.Context) error {
	if a.sqlDB == nil {
		return nil
	}

	// Load users. company is nullable (R10-44): a single NULL row used to
	// abort the whole load — and with it every login after a restart.
	rows, err := a.sqlDB.QueryContext(ctx, `
		SELECT id, email, password_hash, COALESCE(company, ''), created_at, updated_at,
		       COALESCE(email_verified, FALSE), password_changed_at
		FROM users
	`)
	if err != nil {
		return fmt.Errorf("load users: %w", err)
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		u := &User{}
		var changed sql.NullTime
		if err := rows.Scan(&u.ID, &u.Email, &u.PasswordHash, &u.Company,
			&u.CreatedAt, &u.UpdatedAt, &u.EmailVerified, &changed); err != nil {
			return fmt.Errorf("scan user: %w", err)
		}
		if changed.Valid {
			u.PasswordChangedAt = &changed.Time
		}
		a.cacheMu.Lock()
		a.users[u.Email] = u
		a.userIndex[u.ID] = u
		a.cacheMu.Unlock()
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate users: %w", err)
	}

	// Load tenants and link to users. COALESCE matches the api_keys load
	// below: one malformed row (NULL text column) must not abort the whole
	// credential load — that would leave auth empty after a restart.
	// tenants.access_key/secret_key are the MIRROR of the live primary pair
	// (WP-R5-14): they are loaded as the tenant's pair, but the hot index
	// (keyIndex) is built from api_keys rows only, below.
	trows, err := a.sqlDB.QueryContext(ctx, `
		SELECT id, COALESCE(name, ''), COALESCE(email, ''),
		       COALESCE(access_key, ''), COALESCE(secret_key, ''), created_at
		FROM tenants
	`)
	if err != nil {
		return fmt.Errorf("load tenants: %w", err)
	}
	defer func() { _ = trows.Close() }()

	for trows.Next() {
		var (
			t     Tenant
			name  string
			email string
		)
		if err := trows.Scan(&t.ID, &name, &email, &t.AccessKey, &t.SecretKey,
			&t.CreatedAt); err != nil {
			return fmt.Errorf("scan tenant: %w", err)
		}

		// Find the owning user by email and link them
		if u, ok := a.users[email]; ok {
			t.UserID = u.ID
			u.TenantID = t.ID
		}

		a.cacheMu.Lock()
		a.tenants[t.ID] = &t
		a.cacheMu.Unlock()
	}
	if err := trows.Err(); err != nil {
		return fmt.Errorf("iterate tenants: %w", err)
	}

	// Load API keys with scope data. Every key goes to apiKeys (listings);
	// only a LIVE key goes to keyIndex (the hot index), and the live primary
	// becomes the tenant's in-memory pair. The tenant comes from the row
	// (WP-R5-9); a row without one is listed under the user's tenant but
	// never indexed.
	akRows, err := a.sqlDB.QueryContext(ctx, `
		SELECT id, user_id, COALESCE(tenant_id, ''), is_primary, name, key_id, secret_hash,
		       COALESCE(secret_key, ''),
		       COALESCE(permissions, '["*"]'::jsonb),
		       COALESCE(bucket_scope, '{}'),
		       COALESCE(ip_allowlist, '{}'),
		       expires_at, last_used, created_at, revoked_at
		FROM api_keys
	`)
	if err != nil {
		return fmt.Errorf("load api keys: %w", err)
	}
	defer func() { _ = akRows.Close() }()

	for akRows.Next() {
		var (
			k           APIKey
			permJSON    []byte
			bucketScope pq.StringArray
			ipAllowlist pq.StringArray
			expiresAt   sql.NullTime
			lastUsed    sql.NullTime
			revokedAt   sql.NullTime
		)
		if err := akRows.Scan(&k.ID, &k.UserID, &k.TenantID, &k.IsPrimary, &k.Name, &k.Key, &k.Hash,
			&k.Secret, &permJSON, &bucketScope, &ipAllowlist,
			&expiresAt, &lastUsed, &k.CreatedAt, &revokedAt); err != nil {
			return fmt.Errorf("scan api key: %w", err)
		}

		// Corrupt permissions fail closed (no permissions), never open (R5-14).
		if err := json.Unmarshal(permJSON, &k.Permissions); err != nil {
			k.Permissions = nil
		}
		if revokedAt.Valid {
			k.RevokedAt = &revokedAt.Time
		}
		k.BucketScope = []string(bucketScope)
		k.IPAllowlist = []string(ipAllowlist)
		if expiresAt.Valid {
			k.ExpiresAt = &expiresAt.Time
		}
		if lastUsed.Valid {
			k.LastUsed = &lastUsed.Time
		}
		k.Metadata = make(map[string]string)

		a.cacheMu.Lock()
		rowTenant := k.TenantID != ""
		if !rowTenant {
			if u, ok := a.userIndex[k.UserID]; ok {
				k.TenantID = u.TenantID
			}
		}
		a.apiKeys[k.Key] = &k
		if rowTenant && k.RevokedAt == nil {
			a.indexAPIKeyLocked(&k)
		}
		a.cacheMu.Unlock()
	}
	if err := akRows.Err(); err != nil {
		return fmt.Errorf("iterate api keys: %w", err)
	}

	return nil
}

// record writes an operator audit row (internal/audit) for a mutation on
// this service. One site covers the dashboard, the user API and the
// management API (Review R11-09). tenantID may be empty.
func (a *AuthService) record(ctx context.Context, e audit.Entry) {
	if e.TenantID == "" && e.UserID != "" {
		a.cacheMu.RLock()
		if u, ok := a.userIndex[e.UserID]; ok {
			e.TenantID = u.TenantID
		}
		a.cacheMu.RUnlock()
	}
	audit.Record(ctx, a.sqlDB, e)
}

// CreateUser creates a new user account WITH tenant
func (a *AuthService) CreateUser(ctx context.Context, email, password string) (*User, error) {
	user, _, _, err := a.CreateUserWithTenant(ctx, email, password, "")
	return user, err
}

// CreateUserWithTenant creates both user and their storage tenant.
//
// The four rows — users → tenants → api_keys → tenant_quotas, in that order
// — are written in ONE transaction; the in-memory maps are populated only
// after it commits (Review R10-10 / R5-18). A failed INSERT therefore leaves
// neither a partial account in the database nor a phantom account in this
// process. The primary API key is the tenant's own S3 key pair.
func (a *AuthService) CreateUserWithTenant(ctx context.Context, email, password, company string) (*User, *Tenant, *APIKey, error) {
	// Single chokepoint: when signups are disabled, reject before any work so
	// the web form, /auth/register API, and OAuth signup are all blocked here.
	if !a.SignupsEnabled() {
		return nil, nil, nil, ErrSignupsDisabled
	}

	// Validate email
	email = strings.ToLower(strings.TrimSpace(email))
	if !strings.Contains(email, "@") {
		return nil, nil, nil, fmt.Errorf("invalid email address")
	}

	// Check if user exists — in the database when there is one (another
	// instance may have registered the address since this one booted). The
	// users.email unique constraint stays the final arbiter (persistNewAccount).
	existing, err := a.userByEmail(ctx, email)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("check existing user: %w", err)
	}
	if existing != nil {
		return nil, nil, nil, ErrUserExists
	}

	// Password policy lives here, not in the forms: the web form enforced
	// eight characters while /auth/register accepted one (Review R12). OAuth
	// accounts pass "" and get no password hash at all.
	if password != "" && len(password) < MinPasswordLength {
		return nil, nil, nil, ErrPasswordTooShort
	}

	// Hash password (empty for OAuth-only users).
	var hashStr string
	if password != "" {
		hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("hash password: %w", err)
		}
		hashStr = string(hash)
	}

	// Create user. OAuth users (empty password) are auto-verified.
	user := &User{
		ID:            uuid.New().String(),
		Email:         email,
		PasswordHash:  hashStr,
		Company:       company,
		EmailVerified: password == "",
		CreatedAt:     time.Now(),
		UpdatedAt:     time.Now(),
	}

	// Create tenant for this user
	tenant := &Tenant{
		ID:        "tenant-" + GenerateID(),
		UserID:    user.ID,
		AccessKey: "VK" + GenerateID(),
		SecretKey: "SK" + GenerateID() + GenerateID(),
		CreatedAt: time.Now(),
	}

	// Link tenant to user
	user.TenantID = tenant.ID

	// The primary API key IS the credential (is_primary row, WP-R5-14); the
	// tenant's access_key/secret_key columns mirror it.
	apiKey := &APIKey{
		ID:          uuid.New().String(),
		UserID:      user.ID,
		TenantID:    tenant.ID,
		IsPrimary:   true,
		Name:        "primary",
		Key:         tenant.AccessKey,
		Secret:      tenant.SecretKey,
		Permissions: []string{"*"},
		BucketScope: []string{},
		IPAllowlist: []string{},
		CreatedAt:   time.Now(),
	}

	// Persist to PostgreSQL so credentials survive restarts — all four rows
	// or none.
	if a.sqlDB != nil {
		if err := a.persistNewAccount(ctx, user, tenant, apiKey, company); err != nil {
			return nil, nil, nil, err
		}
	}

	// Write to in-memory maps for current-process lookups — after the
	// commit, so a failed persist never leaves an account that authenticates
	// until the next restart.
	a.cacheMu.Lock()
	a.users[email] = user
	a.userIndex[user.ID] = user
	a.tenants[tenant.ID] = tenant
	a.apiKeys[apiKey.Key] = apiKey
	a.keyIndex[tenant.AccessKey] = tenant
	a.cacheMu.Unlock()

	a.record(ctx, audit.Entry{UserID: user.ID, TenantID: tenant.ID, Action: "account.created",
		Resource: "user:" + user.ID, Metadata: map[string]any{"email": email}})

	return user, tenant, apiKey, nil
}

// persistNewAccount writes users → tenants → api_keys → tenant_quotas in one
// transaction. Each INSERT is ON CONFLICT DO NOTHING for idempotency.
func (a *AuthService) persistNewAccount(ctx context.Context, user *User, tenant *Tenant, apiKey *APIKey, company string) error {
	tx, err := a.sqlDB.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin registration: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	res, err := tx.ExecContext(ctx, `
		INSERT INTO users (id, email, password_hash, company, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (email) DO NOTHING
	`, user.ID, user.Email, user.PasswordHash, user.Company,
		user.CreatedAt, user.UpdatedAt)
	if err != nil {
		return fmt.Errorf("persist user: %w", err)
	}
	// A concurrent registration of the same address (on this instance or the
	// other one of a deploy overlap) won the unique constraint.
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		return ErrUserExists
	}

	// tenants.name = company name; tenants.email = owner email.
	// Both are NOT NULL in the schema so must always be provided.
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO tenants (id, name, email, access_key, secret_key, created_at)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (id) DO NOTHING
	`, tenant.ID, company, user.Email, tenant.AccessKey, tenant.SecretKey, tenant.CreatedAt); err != nil {
		return fmt.Errorf("persist tenant: %w", err)
	}

	// secret_key must be stored (not just the bcrypt secret_hash) — SigV4
	// verification recomputes the request signature from the raw secret,
	// so a hash-only row can never authenticate and must be regenerated.
	secretHash, err := bcrypt.GenerateFromPassword([]byte(apiKey.Secret), bcrypt.DefaultCost)
	if err != nil {
		return fmt.Errorf("hash api key secret: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO api_keys (id, user_id, tenant_id, is_primary, name, key_id, secret_hash, secret_key, created_at)
		VALUES ($1, $2, $3, TRUE, $4, $5, $6, $7, $8)
		ON CONFLICT (key_id) DO NOTHING
	`, apiKey.ID, user.ID, tenant.ID, apiKey.Name, apiKey.Key, string(secretHash), apiKey.Secret, apiKey.CreatedAt); err != nil {
		return fmt.Errorf("persist api key: %w", err)
	}

	// Provision a default quota row so HandlePut never sees
	// "no rows in result set" on the tenant_quotas SELECT.
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO tenant_quotas (tenant_id)
		VALUES ($1)
		ON CONFLICT (tenant_id) DO NOTHING
	`, tenant.ID); err != nil {
		return fmt.Errorf("provision tenant quota: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit registration: %w", err)
	}
	return nil
}

// ValidatePassword checks if password is correct. With a database the hash
// is the row's current one, read on every call (user_lookup.go): a password
// changed, or an account registered or erased, on another instance decides
// this sign-in too.
func (a *AuthService) ValidatePassword(ctx context.Context, email, password string) (bool, error) {
	email = strings.ToLower(strings.TrimSpace(email))

	user, err := a.userByEmail(ctx, email)
	if err != nil {
		return false, fmt.Errorf("validate password: %w", err)
	}
	if user == nil {
		return false, nil
	}
	a.cacheMu.RLock()
	hash := user.PasswordHash
	a.cacheMu.RUnlock()

	// OAuth-only users have no password — reject login via password form.
	if hash == "" {
		return false, nil
	}

	err = bcrypt.CompareHashAndPassword([]byte(hash), []byte(password))
	if errors.Is(err, bcrypt.ErrMismatchedHashAndPassword) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("compare password: %w", err)
	}

	return true, nil
}

// ChangePassword validates the current password and updates to a new one.
// Updates both the in-memory map and PostgreSQL (if available). Every JWT
// issued before the change is dead from here on (WP-R5-10); the dashboard
// keeps the session the change was made from and revokes the others.
func (a *AuthService) ChangePassword(ctx context.Context, userID, currentPassword, newPassword string) error {
	user, err := a.userByID(ctx, userID)
	if err != nil {
		return fmt.Errorf("change password: %w", err)
	}
	if user == nil {
		return fmt.Errorf("user not found")
	}
	a.cacheMu.RLock()
	current := user.PasswordHash
	a.cacheMu.RUnlock()

	if err := bcrypt.CompareHashAndPassword([]byte(current), []byte(currentPassword)); err != nil {
		return fmt.Errorf("current password is incorrect")
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(newPassword), bcrypt.DefaultCost)
	if err != nil {
		return fmt.Errorf("hash password: %w", err)
	}

	changedAt, err := a.setPassword(ctx, user, string(hash))
	if err != nil {
		return err
	}

	a.record(ctx, audit.Entry{UserID: userID, Action: "auth.password_changed", Resource: "user:" + userID,
		Metadata: map[string]any{"jwts_invalidated_before": changedAt}})
	return nil
}

// setPassword writes a new password hash — the row first, then the
// in-memory user — and stamps password_changed_at with one timestamp in
// both places. Returns the stamp.
func (a *AuthService) setPassword(ctx context.Context, user *User, hash string) (time.Time, error) {
	changedAt := time.Now().UTC()
	if a.sqlDB != nil {
		if _, err := a.sqlDB.ExecContext(ctx,
			`UPDATE users SET password_hash = $1, updated_at = NOW(), password_changed_at = $3 WHERE id = $2`,
			hash, user.ID, changedAt); err != nil {
			return time.Time{}, fmt.Errorf("update password in db: %w", err)
		}
	}
	a.cacheMu.Lock()
	user.PasswordHash = hash
	user.PasswordChangedAt = &changedAt
	a.cacheMu.Unlock()
	return changedAt, nil
}

// GetUserByEmail retrieves a user by email — the database row when there
// is one (sign-in, password-reset and OAuth flows decide on it).
func (a *AuthService) GetUserByEmail(ctx context.Context, email string) (*User, error) {
	email = strings.ToLower(strings.TrimSpace(email))
	user, err := a.userByEmail(ctx, email)
	if err != nil {
		return nil, fmt.Errorf("get user by email: %w", err)
	}
	if user == nil {
		return nil, fmt.Errorf("user not found")
	}
	a.cacheMu.RLock()
	defer a.cacheMu.RUnlock()
	cp := *user // a copy: the caller reads it outside the lock
	return &cp, nil
}

// GetUserByID retrieves a user by ID — the database row when there is one.
func (a *AuthService) GetUserByID(ctx context.Context, userID string) (*User, error) {
	user, err := a.userByID(ctx, userID)
	if err != nil {
		return nil, fmt.Errorf("get user by id: %w", err)
	}
	if user == nil {
		return nil, fmt.Errorf("user not found")
	}
	a.cacheMu.RLock()
	defer a.cacheMu.RUnlock()
	cp := *user
	return &cp, nil
}

// GetUserIDByTenantID returns the owning user's ID for a given tenant.
// Returns "" if not found.
func (a *AuthService) GetUserIDByTenantID(_ context.Context, tenantID string) string {
	a.cacheMu.RLock()
	defer a.cacheMu.RUnlock()
	t, exists := a.tenants[tenantID]
	if !exists {
		return ""
	}
	return t.UserID
}

// GetUserByOAuth looks up a user by OAuth provider and provider ID.
// Returns nil, nil if no linked account is found.
func (a *AuthService) GetUserByOAuth(ctx context.Context, provider, providerID string) (*User, error) {
	if a.sqlDB == nil {
		return nil, nil
	}
	var userID string
	err := a.sqlDB.QueryRowContext(ctx,
		`SELECT user_id FROM oauth_accounts WHERE provider = $1 AND provider_id = $2`,
		provider, providerID).Scan(&userID)
	if err != nil {
		return nil, nil //nolint:nilerr // not found is not an error
	}
	user, err := a.userByID(ctx, userID)
	if err != nil {
		return nil, fmt.Errorf("get user by oauth: %w", err)
	}
	if user == nil {
		return nil, nil
	}
	a.cacheMu.RLock()
	defer a.cacheMu.RUnlock()
	cp := *user
	return &cp, nil
}

// LinkOAuthAccount associates an OAuth provider account with an existing user.
func (a *AuthService) LinkOAuthAccount(ctx context.Context, userID, provider, providerID, email, name string) error {
	if a.sqlDB == nil {
		return nil
	}
	_, err := a.sqlDB.ExecContext(ctx,
		`INSERT INTO oauth_accounts (user_id, provider, provider_id, email, name)
		 VALUES ($1, $2, $3, $4, $5)
		 ON CONFLICT (provider, provider_id) DO NOTHING`,
		userID, provider, providerID, email, name)
	if err != nil {
		return fmt.Errorf("link oauth account: %w", err)
	}
	return nil
}

// CreateUserFromOAuth creates a new user+tenant via OAuth (no password).
// Also links the OAuth account. The returned APIKey carries the plaintext
// secret — the only chance to show it to the user (B2 reveal-once).
func (a *AuthService) CreateUserFromOAuth(ctx context.Context, email, company, provider, providerID string) (*User, *Tenant, *APIKey, error) {
	user, tenant, apiKey, err := a.CreateUserWithTenant(ctx, email, "", company)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("create oauth user: %w", err)
	}

	if err := a.LinkOAuthAccount(ctx, user.ID, provider, providerID, email, company); err != nil {
		return nil, nil, nil, fmt.Errorf("link oauth on create: %w", err)
	}

	return user, tenant, apiKey, nil
}

// ValidateS3Request validates S3 API requests and returns tenant
func (a *AuthService) ValidateS3Request(ctx context.Context, accessKey string) (*Tenant, error) {
	a.cacheMu.Lock()
	defer a.cacheMu.Unlock()
	tenant, exists := a.keyIndex[accessKey]
	if !exists {
		return nil, fmt.Errorf("invalid access key")
	}
	if apiKey, hasKey := a.apiKeys[accessKey]; hasKey {
		now := time.Now()
		apiKey.LastUsed = &now
		apiKey.UsageCount++
	}
	cp := *tenant
	return &cp, nil
}

// Evict removes an erased account from the in-process credential cache
// (WP-R10-3). S3 auth reads the database, but the dashboard login,
// password checks and API-key validation read these maps, which are loaded
// once at boot: without this an erased user could still sign in until the
// next restart. The tenant is removed with every access key that pointed
// at it and every API key that belonged to the user.
func (a *AuthService) Evict(userID, tenantID string) {
	a.cacheMu.Lock()
	defer a.cacheMu.Unlock()
	if u, ok := a.userIndex[userID]; ok {
		delete(a.users, u.Email)
		delete(a.userIndex, userID)
	}
	if t, ok := a.tenants[tenantID]; ok {
		delete(a.keyIndex, t.AccessKey)
		delete(a.tenants, tenantID)
	}
	for key, k := range a.apiKeys {
		if k.UserID == userID || (tenantID != "" && k.TenantID == tenantID) {
			delete(a.apiKeys, key)
			delete(a.keyIndex, key)
		}
	}
	for key, t := range a.keyIndex {
		if tenantID != "" && t.ID == tenantID {
			delete(a.keyIndex, key)
		}
	}
	a.mfaMu.Lock()
	delete(a.mfaSettings, userID)
	delete(a.totpUsed, userID)
	a.mfaMu.Unlock()
	delete(a.profiles, userID)
	delete(a.preferences, userID)
}

// jwtIssuer is the `iss` every token we mint carries and every token we
// accept must carry (WP-R5-10).
const jwtIssuer = "vaultaire"

// ErrJWTRevoked: the token was issued before the user's last password
// change or reset (WP-R5-10). The holder signs in again.
var ErrJWTRevoked = errors.New("token issued before the last password change")

// GenerateJWT creates a JWT token for web access
func (a *AuthService) GenerateJWT(user *User) (string, error) {
	return a.generateJWTAt(user, time.Now())
}

// generateJWTAt mints a token with an explicit issue time (tests date a
// token before a password change).
func (a *AuthService) generateJWTAt(user *User, issuedAt time.Time) (string, error) {
	claims := JWTClaims{
		UserID:   user.ID,
		Email:    user.Email,
		TenantID: user.TenantID,
		RegisteredClaims: jwt.RegisteredClaims{
			ExpiresAt: jwt.NewNumericDate(issuedAt.Add(24 * time.Hour)),
			IssuedAt:  jwt.NewNumericDate(issuedAt),
			Issuer:    jwtIssuer,
		},
	}

	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	return token.SignedString(a.jwtSecret)
}

// ValidateJWT validates a JWT token: HMAC signature, expiry, issuer, and —
// WP-R5-10 — that it was issued no earlier than the user's last password
// change (`iat` is compared at second precision: a token minted in the same
// second as the change survives, one minted before it does not). A token
// for a user this process does not know (erased, evicted) is refused.
func (a *AuthService) ValidateJWT(tokenString string) (*JWTClaims, error) {
	token, err := jwt.ParseWithClaims(tokenString, &JWTClaims{}, func(token *jwt.Token) (interface{}, error) {
		if _, ok := token.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("unexpected signing method")
		}
		return a.jwtSecret, nil
	}, jwt.WithIssuer(jwtIssuer), jwt.WithIssuedAt())

	if err != nil {
		return nil, fmt.Errorf("parse token: %w", err)
	}

	claims, ok := token.Claims.(*JWTClaims)
	if !ok || !token.Valid {
		return nil, fmt.Errorf("invalid token")
	}
	if claims.IssuedAt == nil {
		return nil, fmt.Errorf("invalid token: no issue time")
	}

	a.cacheMu.RLock()
	user, known := a.userIndex[claims.UserID]
	var changedAt *time.Time
	if known {
		changedAt = user.PasswordChangedAt
	}
	a.cacheMu.RUnlock()
	if !known && a.sqlDB != nil {
		// A user this process has not cached may have registered on the
		// other instance of a deploy overlap: one read, on a miss only (the
		// signature is already verified, so a miss cannot be provoked
		// without the JWT key). A cached user is not re-read per request.
		ctx, cancel := context.WithTimeout(context.Background(), jwtLookupTimeout)
		u, err := a.userByID(ctx, claims.UserID)
		cancel()
		if err != nil {
			return nil, fmt.Errorf("validate token: %w", err)
		}
		if u != nil {
			known = true
			a.cacheMu.RLock()
			changedAt = u.PasswordChangedAt
			a.cacheMu.RUnlock()
		}
	}
	if !known {
		return nil, fmt.Errorf("invalid token: unknown user")
	}
	if changedAt != nil && claims.IssuedAt.Unix() < changedAt.Unix() {
		return nil, ErrJWTRevoked
	}

	return claims, nil
}

// GenerateID generates a random 8-byte hex string
func GenerateID() string {
	bytes := make([]byte, 8)
	_, _ = rand.Read(bytes)
	return hex.EncodeToString(bytes)
}
