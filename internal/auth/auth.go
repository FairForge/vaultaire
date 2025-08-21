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

// User represents a system user
type User struct {
	ID           string
	Email        string
	PasswordHash string
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// APIKey represents an API key for S3 access
type APIKey struct {
	ID        string
	UserID    string
	Name      string
	Key       string // Public key (like AWS Access Key)
	Secret    string // Secret key (like AWS Secret Key)
	Hash      string // Hash of secret for storage
	CreatedAt time.Time
	LastUsed  time.Time
}

// JWTClaims represents JWT token claims
type JWTClaims struct {
	UserID string `json:"user_id"`
	Email  string `json:"email"`
	jwt.RegisteredClaims
}

// AuthService handles authentication
type AuthService struct {
	db        Database
	jwtSecret []byte
	users     map[string]*User   // In-memory for now
	apiKeys   map[string]*APIKey // In-memory for now
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
		apiKeys:   make(map[string]*APIKey),
	}
}

// CreateUser creates a new user account
func (a *AuthService) CreateUser(ctx context.Context, email, password string) (*User, error) {
	// Validate email
	email = strings.ToLower(strings.TrimSpace(email))
	if !strings.Contains(email, "@") {
		return nil, fmt.Errorf("invalid email address")
	}

	// Check if user exists
	if _, exists := a.users[email]; exists {
		return nil, fmt.Errorf("user already exists")
	}

	// Hash password
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return nil, fmt.Errorf("hash password: %w", err)
	}

	user := &User{
		ID:           uuid.New().String(),
		Email:        email,
		PasswordHash: string(hash),
		CreatedAt:    time.Now(),
		UpdatedAt:    time.Now(),
	}

	// Store in memory (TODO: Use database)
	a.users[email] = user

	return user, nil
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

// GenerateAPIKey creates a new API key
func (a *AuthService) GenerateAPIKey(ctx context.Context, userID, name string) (*APIKey, error) {
	// Generate random key and secret
	keyBytes := make([]byte, 20)
	if _, err := rand.Read(keyBytes); err != nil {
		return nil, fmt.Errorf("generate key: %w", err)
	}

	secretBytes := make([]byte, 40)
	if _, err := rand.Read(secretBytes); err != nil {
		return nil, fmt.Errorf("generate secret: %w", err)
	}

	key := "VK" + strings.ToUpper(hex.EncodeToString(keyBytes)) // VK prefix for Vaultaire Key
	secret := hex.EncodeToString(secretBytes)

	// Hash secret for storage
	hash := sha256.Sum256([]byte(secret))

	apiKey := &APIKey{
		ID:        uuid.New().String(),
		UserID:    userID,
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

// ValidateAPIKey checks if API key is valid
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
	for _, user := range a.users {
		if user.ID == apiKey.UserID {
			// Update last used
			apiKey.LastUsed = time.Now()
			return user, nil
		}
	}

	return nil, fmt.Errorf("user not found")
}

// GenerateJWT creates a JWT token for web access
func (a *AuthService) GenerateJWT(user *User) (string, error) {
	claims := JWTClaims{
		UserID: user.ID,
		Email:  user.Email,
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
