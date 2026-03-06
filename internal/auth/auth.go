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
// gets them back immediately), then persisted to PostgreSQL. If the
// SQL writes fail the registration response is still returned — the
// process will work until restart. A log.Error is emitted so the
// operator knows persistence failed.
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

	// Hash password
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("hash password: %w", err)
	}

	// Create user
	user := &User{
		ID:           uuid.New().String(),
		Email:        email,
		PasswordHash: string(hash),
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

	// Write to in-memory maps — this is what the rest of the process
	// uses for lookups during the current run.
	a.users[email] = user
	a.userIndex[user.ID] = user
	a.tenants[tenant.ID] = tenant
	a.apiKeys[apiKey.Key] = apiKey
	a.keyIndex[tenant.AccessKey] = tenant

	// Persist to PostgreSQL so credentials survive restarts.
	// Each INSERT is non-fatal: log and continue so the caller still
	// gets their credentials back even if the DB is momentarily down.
	if a.sqlDB != nil {
		_, err = a.sqlDB.ExecContext(ctx, `
			INSERT INTO users (id, email, password_hash, company, created_at, updated_at)
			VALUES ($1, $2, $3, $4, $5, $6)
			ON CONFLICT (email) DO NOTHING
		`, user.ID, user.Email, user.PasswordHash, user.Company,
			user.CreatedAt, user.UpdatedAt)
		if err != nil {
			// Log but do not return — in-memory state is consistent.
			fmt.Printf("ERROR: failed to persist user to PostgreSQL: %v\n", err)
		}

		_, err = a.sqlDB.ExecContext(ctx, `
			INSERT INTO tenants (id, access_key, secret_key, created_at)
			VALUES ($1, $2, $3, $4)
			ON CONFLICT (id) DO NOTHING
		`, tenant.ID, tenant.AccessKey, tenant.SecretKey, tenant.CreatedAt)
		if err != nil {
			fmt.Printf("ERROR: failed to persist tenant to PostgreSQL: %v\n", err)
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

	err := bcrypt.CompareHashAndPassword([]byte(user.PasswordHash), []byte(password))
	if err == bcrypt.ErrMismatchedHashAndPassword {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("compare password: %w", err)
	}

	return true, nil
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
