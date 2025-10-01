package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// APIKey represents an API key for S3 access
type APIKey struct {
	ID          string            `json:"id" db:"id"`
	UserID      string            `json:"user_id" db:"user_id"`
	TenantID    string            `json:"tenant_id" db:"tenant_id"`
	Name        string            `json:"name" db:"name"`
	Key         string            `json:"key" db:"key_id"`    // Public key (like AWS Access Key)
	Secret      string            `json:"secret,omitempty"`   // Secret key (shown once)
	Hash        string            `json:"-" db:"secret_hash"` // Hash of secret for storage
	Permissions []string          `json:"permissions" db:"permissions"`
	ExpiresAt   *time.Time        `json:"expires_at,omitempty" db:"expires_at"`
	LastUsed    *time.Time        `json:"last_used,omitempty" db:"last_used"`
	CreatedAt   time.Time         `json:"created_at" db:"created_at"`
	RevokedAt   *time.Time        `json:"revoked_at,omitempty" db:"revoked_at"`
	Metadata    map[string]string `json:"metadata" db:"metadata"`

	// Usage tracking (Step 324)
	UsageCount int64  `json:"usage_count" db:"usage_count"`
	LastIP     string `json:"last_ip,omitempty" db:"last_ip"`
}

// GenerateAPIKey creates a new API key for a user (Step 321 - enhanced)
func (a *AuthService) GenerateAPIKey(ctx context.Context, userID, name string) (*APIKey, error) {
	user, exists := a.userIndex[userID]
	if !exists {
		return nil, fmt.Errorf("user not found")
	}

	// Generate access key with better format: VLT_XXXXXXXXXXXXXXXXXXXX
	accessKey, err := generateAccessKey()
	if err != nil {
		return nil, fmt.Errorf("generate access key: %w", err)
	}

	// Generate secret key (40 chars hex)
	secretKey, hash, err := generateSecretKey()
	if err != nil {
		return nil, fmt.Errorf("generate secret key: %w", err)
	}

	apiKey := &APIKey{
		ID:          uuid.New().String(),
		UserID:      userID,
		TenantID:    user.TenantID,
		Name:        name,
		Key:         accessKey,
		Secret:      secretKey, // Only returned on creation
		Hash:        hash,
		Permissions: []string{"s3:*"}, // Default permissions (Step 323)
		CreatedAt:   time.Now(),
		Metadata:    make(map[string]string),
	}

	// Store in memory (TODO: Use database)
	a.apiKeys[accessKey] = apiKey

	return apiKey, nil
}

// ValidateAPIKey checks if API key is valid (moved from auth.go)
func (a *AuthService) ValidateAPIKey(ctx context.Context, key, secret string) (*User, error) {
	apiKey, exists := a.apiKeys[key]
	if !exists {
		return nil, fmt.Errorf("invalid API key")
	}

	// Check if key is revoked (Step 327)
	if apiKey.RevokedAt != nil {
		return nil, fmt.Errorf("API key has been revoked")
	}

	// Check if key is expired (Step 326)
	if apiKey.ExpiresAt != nil && time.Now().After(*apiKey.ExpiresAt) {
		return nil, fmt.Errorf("API key has expired")
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

	// Update usage tracking (Step 324)
	now := time.Now()
	apiKey.LastUsed = &now
	apiKey.UsageCount++

	return user, nil
}

// RotateAPIKey rotates an existing API key (Step 322)
func (a *AuthService) RotateAPIKey(ctx context.Context, userID, keyID string) (*APIKey, error) {
	// Find existing key
	var oldKey *APIKey
	for _, key := range a.apiKeys {
		if key.ID == keyID && key.UserID == userID {
			oldKey = key
			break
		}
	}

	if oldKey == nil {
		return nil, fmt.Errorf("API key not found")
	}

	// Generate new credentials
	accessKey, err := generateAccessKey()
	if err != nil {
		return nil, fmt.Errorf("generate access key: %w", err)
	}

	secretKey, hash, err := generateSecretKey()
	if err != nil {
		return nil, fmt.Errorf("generate secret key: %w", err)
	}

	// Create new key with same permissions
	newKey := &APIKey{
		ID:          uuid.New().String(),
		UserID:      oldKey.UserID,
		TenantID:    oldKey.TenantID,
		Name:        oldKey.Name + " (rotated)",
		Key:         accessKey,
		Secret:      secretKey,
		Hash:        hash,
		Permissions: oldKey.Permissions,
		CreatedAt:   time.Now(),
		Metadata:    oldKey.Metadata,
	}

	// Mark old key as revoked
	now := time.Now()
	oldKey.RevokedAt = &now

	// Store new key
	a.apiKeys[accessKey] = newKey

	return newKey, nil
}

// RevokeAPIKey revokes an API key (Step 327)
func (a *AuthService) RevokeAPIKey(ctx context.Context, userID, keyID string) error {
	for _, key := range a.apiKeys {
		if key.ID == keyID && key.UserID == userID {
			if key.RevokedAt != nil {
				return fmt.Errorf("API key already revoked")
			}
			now := time.Now()
			key.RevokedAt = &now
			return nil
		}
	}
	return fmt.Errorf("API key not found")
}

// SetAPIKeyExpiration sets expiration for an API key (Step 326)
func (a *AuthService) SetAPIKeyExpiration(ctx context.Context, userID, keyID string, expiresAt time.Time) error {
	for _, key := range a.apiKeys {
		if key.ID == keyID && key.UserID == userID {
			key.ExpiresAt = &expiresAt
			return nil
		}
	}
	return fmt.Errorf("API key not found")
}

// ListAPIKeys lists all API keys for a user
func (a *AuthService) ListAPIKeys(ctx context.Context, userID string) ([]*APIKey, error) {
	var keys []*APIKey
	for _, key := range a.apiKeys {
		if key.UserID == userID {
			// Don't include secret in list
			keyCopy := *key
			keyCopy.Secret = ""
			keys = append(keys, &keyCopy)
		}
	}
	return keys, nil
}

// Helper functions

func generateAccessKey() (string, error) {
	// Generate 15 random bytes (encodes to 24 base32 chars)
	b := make([]byte, 15)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}

	// Encode to base32, remove padding, take first 20 chars
	encoded := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b)
	encoded = strings.ToUpper(encoded)

	// Ensure we have at least 20 chars
	if len(encoded) < 20 {
		encoded = encoded + strings.Repeat("0", 20-len(encoded))
	}

	return fmt.Sprintf("VLT_%s", encoded[:20]), nil
}

func generateSecretKey() (string, string, error) {
	// Generate 30 random bytes
	b := make([]byte, 30)
	if _, err := rand.Read(b); err != nil {
		return "", "", err
	}

	// Create secret key (40 chars hex)
	secret := hex.EncodeToString(b)
	if len(secret) > 40 {
		secret = secret[:40]
	}

	// Hash for storage
	hash := sha256.Sum256([]byte(secret))
	hashStr := hex.EncodeToString(hash[:])

	return secret, hashStr, nil
}

// KeyRotationPolicy defines when keys should be rotated (Step 322)
type KeyRotationPolicy struct {
	MaxAge           time.Duration `json:"max_age"`
	MaxUsageCount    int64         `json:"max_usage_count"`
	RequireRotation  bool          `json:"require_rotation"`
	AutoRotate       bool          `json:"auto_rotate"`
	NotificationDays int           `json:"notification_days"`
}

// CheckRotationNeeded checks if a key needs rotation
func (a *AuthService) CheckRotationNeeded(key *APIKey, policy *KeyRotationPolicy) bool {
	if key.RevokedAt != nil {
		return false // Already revoked
	}

	// Check age
	if policy.MaxAge > 0 {
		age := time.Since(key.CreatedAt)
		if age > policy.MaxAge {
			return true
		}
	}

	// Check usage count
	if policy.MaxUsageCount > 0 && key.UsageCount > policy.MaxUsageCount {
		return true
	}

	return false
}
