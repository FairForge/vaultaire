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
	Key         string            `json:"key" db:"key_id"`
	Secret      string            `json:"secret,omitempty"`
	Hash        string            `json:"-" db:"secret_hash"`
	Permissions []string          `json:"permissions" db:"permissions"`
	ExpiresAt   *time.Time        `json:"expires_at,omitempty" db:"expires_at"`
	LastUsed    *time.Time        `json:"last_used,omitempty" db:"last_used"`
	CreatedAt   time.Time         `json:"created_at" db:"created_at"`
	RevokedAt   *time.Time        `json:"revoked_at,omitempty" db:"revoked_at"`
	Metadata    map[string]string `json:"metadata" db:"metadata"`
	UsageCount  int64             `json:"usage_count" db:"usage_count"`
	LastIP      string            `json:"last_ip,omitempty" db:"last_ip"`
}

// GenerateAPIKey creates a new API key for a user
func (a *AuthService) GenerateAPIKey(ctx context.Context, userID, name string) (*APIKey, error) {
	user, exists := a.userIndex[userID]
	if !exists {
		return nil, fmt.Errorf("user not found")
	}

	accessKey, err := generateAccessKey()
	if err != nil {
		return nil, fmt.Errorf("generate access key: %w", err)
	}

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
		Secret:      secretKey,
		Hash:        hash,
		Permissions: []string{"s3:*"},
		CreatedAt:   time.Now(),
		Metadata:    make(map[string]string),
	}

	a.apiKeys[accessKey] = apiKey
	return apiKey, nil
}

// ValidateAPIKey checks if API key is valid
func (a *AuthService) ValidateAPIKey(ctx context.Context, key, secret string) (*User, error) {
	apiKey, exists := a.apiKeys[key]
	if !exists {
		return nil, fmt.Errorf("invalid API key")
	}

	if apiKey.RevokedAt != nil {
		return nil, fmt.Errorf("API key has been revoked")
	}

	if apiKey.ExpiresAt != nil && time.Now().After(*apiKey.ExpiresAt) {
		return nil, fmt.Errorf("API key has expired")
	}

	hash := sha256.Sum256([]byte(secret))
	if hex.EncodeToString(hash[:]) != apiKey.Hash {
		return nil, fmt.Errorf("invalid API secret")
	}

	user, exists := a.userIndex[apiKey.UserID]
	if !exists {
		return nil, fmt.Errorf("user not found")
	}

	now := time.Now()
	apiKey.LastUsed = &now
	apiKey.UsageCount++

	return user, nil
}

// RotateAPIKey rotates an existing API key
func (a *AuthService) RotateAPIKey(ctx context.Context, userID, keyID string) (*APIKey, error) {
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

	accessKey, err := generateAccessKey()
	if err != nil {
		return nil, fmt.Errorf("generate access key: %w", err)
	}

	secretKey, hash, err := generateSecretKey()
	if err != nil {
		return nil, fmt.Errorf("generate secret key: %w", err)
	}

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

	now := time.Now()
	oldKey.RevokedAt = &now
	a.apiKeys[accessKey] = newKey

	return newKey, nil
}

// RevokeAPIKey revokes an API key
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

// SetAPIKeyExpiration sets expiration for an API key
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
			keyCopy := *key
			keyCopy.Secret = ""
			keys = append(keys, &keyCopy)
		}
	}
	return keys, nil
}

// GenerateAPIKeyWithAudit creates a new API key with audit logging
func (a *AuthService) GenerateAPIKeyWithAudit(ctx context.Context, userID, name, ip, userAgent string) (*APIKey, error) {
	key, err := a.GenerateAPIKey(ctx, userID, name)

	if a.auditLogger != nil {
		event := APIKeyAuditEvent{
			UserID:    userID,
			KeyID:     key.ID,
			Action:    AuditKeyCreated,
			IP:        ip,
			UserAgent: userAgent,
			Success:   err == nil,
			Metadata: map[string]interface{}{
				"key_name": name,
			},
		}
		if err != nil {
			event.Error = err.Error()
		}
		_ = a.auditLogger.LogKeyEvent(ctx, event)
	}

	return key, err
}

// ValidateAPIKeyWithAudit validates an API key with audit logging
func (a *AuthService) ValidateAPIKeyWithAudit(ctx context.Context, key, secret, ip string) (*User, error) {
	user, err := a.ValidateAPIKey(ctx, key, secret)

	if a.auditLogger != nil && err == nil {
		if apiKey, exists := a.apiKeys[key]; exists {
			event := APIKeyAuditEvent{
				UserID:  user.ID,
				KeyID:   apiKey.ID,
				Action:  AuditKeyUsed,
				IP:      ip,
				Success: true,
			}
			_ = a.auditLogger.LogKeyEvent(ctx, event)
		}
	}

	return user, err
}

// Helper functions
func generateAccessKey() (string, error) {
	b := make([]byte, 15)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}

	encoded := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b)
	encoded = strings.ToUpper(encoded)

	if len(encoded) < 20 {
		encoded = encoded + strings.Repeat("0", 20-len(encoded))
	}

	return fmt.Sprintf("VLT_%s", encoded[:20]), nil
}

func generateSecretKey() (string, string, error) {
	b := make([]byte, 30)
	if _, err := rand.Read(b); err != nil {
		return "", "", err
	}

	secret := hex.EncodeToString(b)
	if len(secret) > 40 {
		secret = secret[:40]
	}

	hash := sha256.Sum256([]byte(secret))
	hashStr := hex.EncodeToString(hash[:])

	return secret, hashStr, nil
}

// KeyRotationPolicy defines when keys should be rotated
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
		return false
	}

	if policy.MaxAge > 0 {
		age := time.Since(key.CreatedAt)
		if age > policy.MaxAge {
			return true
		}
	}

	if policy.MaxUsageCount > 0 && key.UsageCount > policy.MaxUsageCount {
		return true
	}

	return false
}

// Audit logging types and functions

// APIKeyAuditEvent represents an audit log entry
type APIKeyAuditEvent struct {
	ID        string                 `json:"id" db:"id"`
	Timestamp time.Time              `json:"timestamp" db:"timestamp"`
	UserID    string                 `json:"user_id" db:"user_id"`
	KeyID     string                 `json:"key_id" db:"key_id"`
	Action    string                 `json:"action" db:"action"`
	IP        string                 `json:"ip,omitempty" db:"ip"`
	UserAgent string                 `json:"user_agent,omitempty" db:"user_agent"`
	Success   bool                   `json:"success" db:"success"`
	Error     string                 `json:"error,omitempty" db:"error"`
	Metadata  map[string]interface{} `json:"metadata,omitempty" db:"metadata"`
}

// Audit action types
const (
	AuditKeyCreated       = "key.created"
	AuditKeyRotated       = "key.rotated"
	AuditKeyRevoked       = "key.revoked"
	AuditKeyExpired       = "key.expired"
	AuditKeyUsed          = "key.used"
	AuditKeyValidated     = "key.validated"
	AuditKeyListed        = "key.listed"
	AuditKeyRateLimited   = "key.rate_limited"
	AuditKeyExpireSet     = "key.expire_set"
	AuditKeyPermissionSet = "key.permission_set"
)

// AuditLogger handles API key audit logging
type AuditLogger struct {
	events []APIKeyAuditEvent
}

// NewAuditLogger creates a new audit logger
func NewAuditLogger() *AuditLogger {
	return &AuditLogger{
		events: make([]APIKeyAuditEvent, 0),
	}
}

// LogKeyEvent logs an API key event
func (al *AuditLogger) LogKeyEvent(ctx context.Context, event APIKeyAuditEvent) error {
	event.ID = GenerateID()
	if event.Timestamp.IsZero() {
		event.Timestamp = time.Now()
	}
	al.events = append(al.events, event)
	return nil
}

// GetAuditLogs retrieves audit logs
func (al *AuditLogger) GetAuditLogs(ctx context.Context, filters AuditFilters) ([]APIKeyAuditEvent, error) {
	var results []APIKeyAuditEvent
	for _, event := range al.events {
		if filters.UserID != "" && event.UserID != filters.UserID {
			continue
		}
		results = append(results, event)
	}
	return results, nil
}

// AuditFilters for querying audit logs
type AuditFilters struct {
	UserID    string
	KeyID     string
	Action    string
	StartTime time.Time
	EndTime   time.Time
	Limit     int
}
