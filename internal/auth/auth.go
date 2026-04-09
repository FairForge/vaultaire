package auth

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"golang.org/x/crypto/bcrypt"
)

// User represents a stored.ge customer
type User struct {
	ID           string
	Email        string
	PasswordHash string
	Company      string
	TenantID     string // Link to their storage tenant
	CreatedAt    time.Time
	UpdatedAt    time.Time
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
	db              Database
	sqlDB           *sql.DB // for persistent writes; nil in test mode
	jwtSecret       []byte
	users           map[string]*User          // email -> user
	tenants         map[string]*Tenant        // tenantID -> tenant
	apiKeys         map[string]*APIKey        // key -> apikey
	userIndex       map[string]*User          // userID -> user
	keyIndex        map[string]*Tenant        // accessKey -> tenant (for S3 auth)
	profiles        map[string]*ProfileUpdate // user profiles
	preferences     map[string]*UserPreferences
	activityTracker *ActivityTracker
	auditLogger     *AuditLogger
}

// Database interface for auth operations
type Database interface {
	// Will be implemented with PostgreSQL
}

// NewAuthService creates a new auth service.
// sqlDB may be nil (e.g. in tests); persistence is skipped when it is.
func NewAuthService(db Database, sqlDB *sql.DB) *AuthService {
	return &AuthService{
		db:              db,
		sqlDB:           sqlDB,
		jwtSecret:       []byte("change-me-in-production"), // TODO: Use env var
		users:           make(map[string]*User),
		tenants:         make(map[string]*Tenant),
		apiKeys:         make(map[string]*APIKey),
		userIndex:       make(map[string]*User),
		keyIndex:        make(map[string]*Tenant),
		profiles:        make(map[string]*ProfileUpdate),
		preferences:     make(map[string]*UserPreferences),
		activityTracker: nil,
		auditLogger:     nil,
	}
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

	// Load users
	rows, err := a.sqlDB.QueryContext(ctx, `
		SELECT id, email, password_hash, company, created_at, updated_at
		FROM users
	`)
	if err != nil {
		return fmt.Errorf("load users: %w", err)
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		u := &User{}
		if err := rows.Scan(&u.ID, &u.Email, &u.PasswordHash, &u.Company,
			&u.CreatedAt, &u.UpdatedAt); err != nil {
			return fmt.Errorf("scan user: %w", err)
		}
		a.users[u.Email] = u
		a.userIndex[u.ID] = u
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate users: %w", err)
	}

	// Load tenants and link to users
	trows, err := a.sqlDB.QueryContext(ctx, `
		SELECT id, name, email, access_key, secret_key, created_at
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

		a.tenants[t.ID] = &t
		if t.AccessKey != "" {
			a.keyIndex[t.AccessKey] = &t
		}
	}
	if err := trows.Err(); err != nil {
		return fmt.Errorf("iterate tenants: %w", err)
	}

	return nil
}

// SetAuditLogger sets the audit logger for the auth service
func (a *AuthService) SetAuditLogger(logger *AuditLogger) {
	a.auditLogger = logger
}

// CreateUser creates a new user account WITH tenant
func (a *AuthService) CreateUser(ctx context.Context, email, password string) (*User, error) {
	user, _, _, err := a.CreateUserWithTenant(ctx, email, password, "")
	return user, err
}

// CreateUserWithTenant creates both user and their storage tenant.
//
// Credentials are written to the in-memory maps first (so the caller
// gets them back immediately), then persisted to PostgreSQL via three
// sequential INSERTs: users → tenants → api_keys. Each INSERT is
// wrapped with ON CONFLICT DO NOTHING for idempotency. If a DB write
// fails, the error is returned — the in-memory maps are already
// populated so the current process can serve the new tenant, but the
// caller should surface the error so the operator knows persistence
// failed.
func (a *AuthService) CreateUserWithTenant(ctx context.Context, email, password, company string) (*User, *Tenant, *APIKey, error) {
	// Validate email
	email = strings.ToLower(strings.TrimSpace(email))
	if !strings.Contains(email, "@") {
		return nil, nil, nil, fmt.Errorf("invalid email address")
	}

	// Check if user exists
	if _, exists := a.users[email]; exists {
		return nil, nil, nil, fmt.Errorf("user already exists")
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

	// Create user
	user := &User{
		ID:           uuid.New().String(),
		Email:        email,
		PasswordHash: hashStr,
		Company:      company,
		CreatedAt:    time.Now(),
		UpdatedAt:    time.Now(),
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

	// Create primary API key
	apiKey, err := a.GenerateAPIKey(ctx, user.ID, "primary")
	if err != nil {
		apiKey = &APIKey{
			ID:        uuid.New().String(),
			UserID:    user.ID,
			TenantID:  tenant.ID,
			Name:      "primary",
			Key:       tenant.AccessKey,
			Secret:    tenant.SecretKey,
			CreatedAt: time.Now(),
		}
	}

	// Write to in-memory maps for current-process lookups.
	a.users[email] = user
	a.userIndex[user.ID] = user
	a.tenants[tenant.ID] = tenant
	a.apiKeys[apiKey.Key] = apiKey
	a.keyIndex[tenant.AccessKey] = tenant

	// Persist to PostgreSQL so credentials survive restarts.
	if a.sqlDB != nil {
		_, err = a.sqlDB.ExecContext(ctx, `
			INSERT INTO users (id, email, password_hash, company, created_at, updated_at)
			VALUES ($1, $2, $3, $4, $5, $6)
			ON CONFLICT (email) DO NOTHING
		`, user.ID, user.Email, user.PasswordHash, user.Company,
			user.CreatedAt, user.UpdatedAt)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("persist user: %w", err)
		}

		// tenants.name = company name; tenants.email = owner email.
		// Both are NOT NULL in the schema so must always be provided.
		_, err = a.sqlDB.ExecContext(ctx, `
			INSERT INTO tenants (id, name, email, access_key, secret_key, created_at)
			VALUES ($1, $2, $3, $4, $5, $6)
			ON CONFLICT (id) DO NOTHING
		`, tenant.ID, company, email, tenant.AccessKey, tenant.SecretKey, tenant.CreatedAt)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("persist tenant: %w", err)
		}

		// api_keys.secret_hash stores a bcrypt hash — the raw secret is
		// returned to the user once at registration and never stored plaintext.
		secretHash, err := bcrypt.GenerateFromPassword([]byte(apiKey.Secret), bcrypt.DefaultCost)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("hash api key secret: %w", err)
		}
		_, err = a.sqlDB.ExecContext(ctx, `
			INSERT INTO api_keys (id, user_id, name, key_id, secret_hash, created_at)
			VALUES ($1, $2, $3, $4, $5, $6)
			ON CONFLICT (key_id) DO NOTHING
		`, apiKey.ID, user.ID, apiKey.Name, apiKey.Key, string(secretHash), apiKey.CreatedAt)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("persist api key: %w", err)
		}

		// Provision a default quota row so HandlePut never sees
		// "no rows in result set" on the tenant_quotas SELECT.
		_, err = a.sqlDB.ExecContext(ctx, `
			INSERT INTO tenant_quotas (tenant_id)
			VALUES ($1)
			ON CONFLICT (tenant_id) DO NOTHING
		`, tenant.ID)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("provision tenant quota: %w", err)
		}
	}

	return user, tenant, apiKey, nil
}

// ValidatePassword checks if password is correct
func (a *AuthService) ValidatePassword(ctx context.Context, email, password string) (bool, error) {
	email = strings.ToLower(strings.TrimSpace(email))

	user, exists := a.users[email]
	if !exists {
		return false, nil
	}

	// OAuth-only users have no password — reject login via password form.
	if user.PasswordHash == "" {
		return false, nil
	}

	err := bcrypt.CompareHashAndPassword([]byte(user.PasswordHash), []byte(password))
	if err == bcrypt.ErrMismatchedHashAndPassword {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("compare password: %w", err)
	}

	return true, nil
}

// ChangePassword validates the current password and updates to a new one.
// Updates both the in-memory map and PostgreSQL (if available).
func (a *AuthService) ChangePassword(ctx context.Context, userID, currentPassword, newPassword string) error {
	user, exists := a.userIndex[userID]
	if !exists {
		return fmt.Errorf("user not found")
	}

	if err := bcrypt.CompareHashAndPassword([]byte(user.PasswordHash), []byte(currentPassword)); err != nil {
		return fmt.Errorf("current password is incorrect")
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(newPassword), bcrypt.DefaultCost)
	if err != nil {
		return fmt.Errorf("hash password: %w", err)
	}

	user.PasswordHash = string(hash)

	if a.sqlDB != nil {
		_, err = a.sqlDB.ExecContext(ctx,
			`UPDATE users SET password_hash = $1, updated_at = NOW() WHERE id = $2`,
			string(hash), userID)
		if err != nil {
			return fmt.Errorf("update password in db: %w", err)
		}
	}

	return nil
}

// GetUserByEmail retrieves a user by email
func (a *AuthService) GetUserByEmail(ctx context.Context, email string) (*User, error) {
	email = strings.ToLower(strings.TrimSpace(email))
	user, exists := a.users[email]
	if !exists {
		return nil, fmt.Errorf("user not found")
	}
	return user, nil
}

// GetUserByID retrieves a user by ID
func (a *AuthService) GetUserByID(ctx context.Context, userID string) (*User, error) {
	user, exists := a.userIndex[userID]
	if !exists {
		return nil, fmt.Errorf("user not found")
	}
	return user, nil
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
	user, exists := a.userIndex[userID]
	if !exists {
		return nil, nil
	}
	return user, nil
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
// Also links the OAuth account.
func (a *AuthService) CreateUserFromOAuth(ctx context.Context, email, company, provider, providerID string) (*User, *Tenant, error) {
	user, tenant, _, err := a.CreateUserWithTenant(ctx, email, "", company)
	if err != nil {
		return nil, nil, fmt.Errorf("create oauth user: %w", err)
	}

	if err := a.LinkOAuthAccount(ctx, user.ID, provider, providerID, email, company); err != nil {
		return nil, nil, fmt.Errorf("link oauth on create: %w", err)
	}

	return user, tenant, nil
}

// ValidateS3Request validates S3 API requests and returns tenant
func (a *AuthService) ValidateS3Request(ctx context.Context, accessKey string) (*Tenant, error) {
	tenant, exists := a.keyIndex[accessKey]
	if !exists {
		return nil, fmt.Errorf("invalid access key")
	}

	if apiKey, ok := a.apiKeys[accessKey]; ok {
		now := time.Now()
		apiKey.LastUsed = &now
		apiKey.UsageCount++

		if a.auditLogger != nil {
			event := APIKeyAuditEvent{
				UserID:  tenant.UserID,
				KeyID:   apiKey.ID,
				Action:  AuditKeyUsed,
				Success: true,
			}
			_ = a.auditLogger.LogKeyEvent(ctx, event)
		}
	}

	return tenant, nil
}

// GenerateJWT creates a JWT token for web access
func (a *AuthService) GenerateJWT(user *User) (string, error) {
	claims := JWTClaims{
		UserID:   user.ID,
		Email:    user.Email,
		TenantID: user.TenantID,
		RegisteredClaims: jwt.RegisteredClaims{
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(24 * time.Hour)),
			IssuedAt:  jwt.NewNumericDate(time.Now()),
			Issuer:    "vaultaire",
		},
	}

	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	return token.SignedString(a.jwtSecret)
}

// ValidateJWT validates a JWT token
func (a *AuthService) ValidateJWT(tokenString string) (*JWTClaims, error) {
	token, err := jwt.ParseWithClaims(tokenString, &JWTClaims{}, func(token *jwt.Token) (interface{}, error) {
		if _, ok := token.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("unexpected signing method")
		}
		return a.jwtSecret, nil
	})

	if err != nil {
		return nil, fmt.Errorf("parse token: %w", err)
	}

	claims, ok := token.Claims.(*JWTClaims)
	if !ok || !token.Valid {
		return nil, fmt.Errorf("invalid token")
	}

	return claims, nil
}

// GenerateID generates a random 8-byte hex string
func GenerateID() string {
	bytes := make([]byte, 8)
	_, _ = rand.Read(bytes)
	return hex.EncodeToString(bytes)
}

// RequestPasswordReset generates a reset token for the user
func (a *AuthService) RequestPasswordReset(ctx context.Context, email string) (string, error) {
	_, err := a.GetUserByEmail(ctx, email)
	if err != nil {
		return "", fmt.Errorf("user not found")
	}

	token := GenerateID() + GenerateID()
	return token, nil
}

// CompletePasswordReset updates the password using a valid token
func (a *AuthService) CompletePasswordReset(ctx context.Context, token, newPassword string) error {
	// TODO: Validate token and update password
	return nil
}

// TrackActivity tracks user activity
func (a *AuthService) TrackActivity(userID, action, resource, ip, userAgent string) {
	if a.activityTracker != nil {
		event := &ActivityEvent{
			UserID:    userID,
			Action:    action,
			Resource:  resource,
			IP:        ip,
			UserAgent: userAgent,
		}
		go func() {
			_ = a.activityTracker.Track(context.Background(), event)
		}()
	}
}
