// internal/auth/auth.go
package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
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

// APIKey represents an API key for S3 access
type APIKey struct {
	ID        string
	UserID    string
	TenantID  string
	Name      string
	Key       string // Public key (like AWS Access Key)
	Secret    string // Secret key (shown once)
	Hash      string // Hash of secret for storage
	CreatedAt time.Time
	LastUsed  *time.Time
}

// JWTClaims represents JWT token claims
type JWTClaims struct {
	UserID   string `json:"user_id"`
	Email    string `json:"email"`
	TenantID string `json:"tenant_id"`
	jwt.RegisteredClaims
}

// AuthService handles authentication
type AuthService struct {
	db        Database
	jwtSecret []byte
	users     map[string]*User   // email -> user
	tenants   map[string]*Tenant // tenantID -> tenant
	apiKeys   map[string]*APIKey // key -> apikey
	userIndex map[string]*User   // userID -> user
	keyIndex  map[string]*Tenant // accessKey -> tenant (for S3 auth)
}

// Database interface for auth operations
type Database interface {
	// Will be implemented with PostgreSQL
}

// NewAuthService creates a new auth service
func NewAuthService(db Database) *AuthService {
	return &AuthService{
		db:        db,
		jwtSecret: []byte("change-me-in-production"), // TODO: Use env var
		users:     make(map[string]*User),
		tenants:   make(map[string]*Tenant),
		apiKeys:   make(map[string]*APIKey),
		userIndex: make(map[string]*User),
		keyIndex:  make(map[string]*Tenant),
	}
}

// CreateUser creates a new user account WITH tenant
func (a *AuthService) CreateUser(ctx context.Context, email, password string) (*User, error) {
	// This is the original method - calls the new one with empty company
	user, _, _, err := a.CreateUserWithTenant(ctx, email, password, "")
	return user, err
}

// CreateUserWithTenant creates both user and their storage tenant
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
		ID:        "tenant-" + generateID(),
		UserID:    user.ID,
		AccessKey: "VK" + generateID(), // VK = Vaultaire Key
		SecretKey: "SK" + generateID() + generateID(),
		CreatedAt: time.Now(),
	}

	// Link tenant to user
	user.TenantID = tenant.ID

	// Create primary API key
	secretHash := sha256.Sum256([]byte(tenant.SecretKey))
	apiKey := &APIKey{
		ID:        uuid.New().String(),
		UserID:    user.ID,
		TenantID:  tenant.ID,
		Name:      "primary",
		Key:       tenant.AccessKey,
		Secret:    tenant.SecretKey, // Only returned on creation
		Hash:      hex.EncodeToString(secretHash[:]),
		CreatedAt: time.Now(),
	}

	// Store everything in memory (TODO: Use database)
	a.users[email] = user
	a.userIndex[user.ID] = user
	a.tenants[tenant.ID] = tenant
	a.apiKeys[apiKey.Key] = apiKey
	a.keyIndex[tenant.AccessKey] = tenant // For S3 auth lookup

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

// GenerateAPIKey creates a new API key for a user
func (a *AuthService) GenerateAPIKey(ctx context.Context, userID, name string) (*APIKey, error) {
	user, exists := a.userIndex[userID]
	if !exists {
		return nil, fmt.Errorf("user not found")
	}

	// Generate random key and secret
	keyBytes := make([]byte, 20)
	if _, err := rand.Read(keyBytes); err != nil {
		return nil, fmt.Errorf("generate key: %w", err)
	}

	secretBytes := make([]byte, 40)
	if _, err := rand.Read(secretBytes); err != nil {
		return nil, fmt.Errorf("generate secret: %w", err)
	}

	key := "VK" + strings.ToUpper(hex.EncodeToString(keyBytes))[:20]
	secret := hex.EncodeToString(secretBytes)

	// Hash secret for storage
	hash := sha256.Sum256([]byte(secret))

	apiKey := &APIKey{
		ID:        uuid.New().String(),
		UserID:    userID,
		TenantID:  user.TenantID,
		Name:      name,
		Key:       key,
		Secret:    secret,
		Hash:      hex.EncodeToString(hash[:]),
		CreatedAt: time.Now(),
	}

	// Store in memory (TODO: Use database)
	a.apiKeys[key] = apiKey

	return apiKey, nil
}

// ValidateAPIKey checks if API key is valid (for S3 auth)
func (a *AuthService) ValidateAPIKey(ctx context.Context, key, secret string) (*User, error) {
	apiKey, exists := a.apiKeys[key]
	if !exists {
		return nil, fmt.Errorf("invalid API key")
	}

	// Verify secret
	hash := sha256.Sum256([]byte(secret))
	if hex.EncodeToString(hash[:]) != apiKey.Hash {
		return nil, fmt.Errorf("invalid API secret")
	}

	// Find user
	user, exists := a.userIndex[apiKey.UserID]
	if !exists {
		return nil, fmt.Errorf("user not found")
	}

	// Update last used
	now := time.Now()
	apiKey.LastUsed = &now

	return user, nil
}

// ValidateS3Request validates S3 API requests and returns tenant
func (a *AuthService) ValidateS3Request(ctx context.Context, accessKey string) (*Tenant, error) {
	// Quick lookup by access key
	tenant, exists := a.keyIndex[accessKey]
	if !exists {
		return nil, fmt.Errorf("invalid access key")
	}

	// Update last used
	if apiKey, ok := a.apiKeys[accessKey]; ok {
		now := time.Now()
		apiKey.LastUsed = &now
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

// Helper function to generate IDs
func generateID() string {
	bytes := make([]byte, 8)
	_, _ = rand.Read(bytes)
	return hex.EncodeToString(bytes)
}

// RequestPasswordReset generates a reset token for the user
func (a *AuthService) RequestPasswordReset(ctx context.Context, email string) (string, error) {
	// Check user exists
	_, err := a.GetUserByEmail(ctx, email)
	if err != nil {
		return "", fmt.Errorf("user not found")
	}

	// Generate secure reset token
	token := generateID() + generateID()

	// Store token with expiry (TODO: use Redis or DB)
	// For now, just return the token

	return token, nil
}

// CompletePasswordReset updates the password using a valid token
func (a *AuthService) CompletePasswordReset(ctx context.Context, token, newPassword string) error {
	// TODO: Validate token and update password
	return nil
}
