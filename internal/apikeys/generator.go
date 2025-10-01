// internal/apikeys/generator.go
package apikeys

import (
	"crypto/rand"
	"encoding/base32"
	"encoding/base64"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

type APIKey struct {
	ID          string            `json:"id" db:"id"`
	AccessKey   string            `json:"access_key" db:"access_key"`
	SecretKey   string            `json:"secret_key,omitempty" db:"secret_key"`
	UserID      uuid.UUID         `json:"user_id" db:"user_id"`
	Name        string            `json:"name" db:"name"`
	Permissions []string          `json:"permissions" db:"permissions"`
	ExpiresAt   *time.Time        `json:"expires_at,omitempty" db:"expires_at"`
	LastUsedAt  *time.Time        `json:"last_used_at,omitempty" db:"last_used_at"`
	CreatedAt   time.Time         `json:"created_at" db:"created_at"`
	RevokedAt   *time.Time        `json:"revoked_at,omitempty" db:"revoked_at"`
	Metadata    map[string]string `json:"metadata" db:"metadata"`
}

type KeyGenerator struct {
	prefix string
}

func NewKeyGenerator() *KeyGenerator {
	return &KeyGenerator{
		prefix: "VLT", // Vaultaire
	}
}

func (kg *KeyGenerator) Generate() (*APIKey, error) {
	// Generate access key (public)
	accessKey, err := kg.generateAccessKey()
	if err != nil {
		return nil, fmt.Errorf("generate access key: %w", err)
	}

	// Generate secret key (private)
	secretKey, err := kg.generateSecretKey()
	if err != nil {
		return nil, fmt.Errorf("generate secret key: %w", err)
	}

	return &APIKey{
		ID:        uuid.New().String(),
		AccessKey: accessKey,
		SecretKey: secretKey,
		CreatedAt: time.Now(),
		Metadata:  make(map[string]string),
	}, nil
}

func (kg *KeyGenerator) generateAccessKey() (string, error) {
	// Generate 15 random bytes (will encode to 20 base32 chars)
	b := make([]byte, 15)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}

	// Encode to base32 (uppercase, no padding)
	encoded := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b)
	encoded = strings.ToUpper(encoded)

	// Format: VLT_XXXXXXXXXXXXXXXXXXXX
	return fmt.Sprintf("%s_%s", kg.prefix, encoded[:20]), nil
}

func (kg *KeyGenerator) generateSecretKey() (string, error) {
	// Generate 30 random bytes (will encode to 40 base64url chars)
	b := make([]byte, 30)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}

	// Encode to base64url (no padding)
	encoded := base64.RawURLEncoding.EncodeToString(b)

	return encoded, nil
}
