package crypto

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sync"
	"time"

	"golang.org/x/crypto/hkdf"
)

// KeyManager handles tenant key derivation and management
type KeyManager struct {
	masterKey   []byte
	keyCache    map[string]*CachedKey
	cacheMu     sync.RWMutex
	cacheMaxAge time.Duration
	keyVersions map[string]int // tenant_id -> current key version
	versionsMu  sync.RWMutex
}

// CachedKey holds a derived key with metadata
type CachedKey struct {
	Key       []byte
	Version   int
	DerivedAt time.Time
	TenantID  string
}

// KeyManagerConfig configures the key manager
type KeyManagerConfig struct {
	MasterKey     []byte        // 32-byte master key
	MasterKeyHex  string        // Alternative: hex-encoded master key
	CacheMaxAge   time.Duration // How long to cache derived keys
	EnableCaching bool          // Whether to cache derived keys
}

// DefaultKeyManagerConfig returns sensible defaults
func DefaultKeyManagerConfig() *KeyManagerConfig {
	return &KeyManagerConfig{
		CacheMaxAge:   1 * time.Hour,
		EnableCaching: true,
	}
}

// NewKeyManager creates a new key manager
func NewKeyManager(config *KeyManagerConfig) (*KeyManager, error) {
	var masterKey []byte

	if config.MasterKey != nil {
		masterKey = config.MasterKey
	} else if config.MasterKeyHex != "" {
		var err error
		masterKey, err = hex.DecodeString(config.MasterKeyHex)
		if err != nil {
			return nil, fmt.Errorf("invalid master key hex: %w", err)
		}
	} else {
		return nil, fmt.Errorf("master key required: set MasterKey or MasterKeyHex")
	}

	if len(masterKey) != 32 {
		return nil, fmt.Errorf("master key must be 32 bytes, got %d", len(masterKey))
	}

	km := &KeyManager{
		masterKey:   masterKey,
		keyVersions: make(map[string]int),
		cacheMaxAge: config.CacheMaxAge,
	}

	if config.EnableCaching {
		km.keyCache = make(map[string]*CachedKey)
	}

	return km, nil
}

// GenerateMasterKey creates a new random master key
func GenerateMasterKey() ([]byte, error) {
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, fmt.Errorf("failed to generate master key: %w", err)
	}
	return key, nil
}

// GenerateMasterKeyHex creates a new random master key as hex string
func GenerateMasterKeyHex() (string, error) {
	key, err := GenerateMasterKey()
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(key), nil
}

// DeriveTenantKey derives a tenant-specific encryption key using HKDF
func (km *KeyManager) DeriveTenantKey(tenantID string, version int) ([]byte, error) {
	if tenantID == "" {
		return nil, fmt.Errorf("tenant ID required")
	}

	// Check cache first
	if km.keyCache != nil {
		cacheKey := fmt.Sprintf("%s:v%d", tenantID, version)
		km.cacheMu.RLock()
		cached, ok := km.keyCache[cacheKey]
		km.cacheMu.RUnlock()

		if ok && time.Since(cached.DerivedAt) < km.cacheMaxAge {
			return cached.Key, nil
		}
	}

	// Derive key using HKDF
	info := []byte(fmt.Sprintf("vaultaire-tenant-key:v%d:%s", version, tenantID))
	salt := sha256.Sum256([]byte("vaultaire-salt-v1"))

	hkdfReader := hkdf.New(sha256.New, km.masterKey, salt[:], info)
	derivedKey := make([]byte, 32)
	if _, err := hkdfReader.Read(derivedKey); err != nil {
		return nil, fmt.Errorf("HKDF derivation failed: %w", err)
	}

	// Cache the derived key
	if km.keyCache != nil {
		cacheKey := fmt.Sprintf("%s:v%d", tenantID, version)
		km.cacheMu.Lock()
		km.keyCache[cacheKey] = &CachedKey{
			Key:       derivedKey,
			Version:   version,
			DerivedAt: time.Now(),
			TenantID:  tenantID,
		}
		km.cacheMu.Unlock()
	}

	return derivedKey, nil
}

// GetCurrentKeyVersion returns the current key version for a tenant
func (km *KeyManager) GetCurrentKeyVersion(tenantID string) int {
	km.versionsMu.RLock()
	defer km.versionsMu.RUnlock()

	version, ok := km.keyVersions[tenantID]
	if !ok {
		return 1 // Default to version 1
	}
	return version
}

// SetKeyVersion sets the key version for a tenant
func (km *KeyManager) SetKeyVersion(tenantID string, version int) error {
	if version < 1 {
		return fmt.Errorf("key version must be >= 1")
	}

	km.versionsMu.Lock()
	km.keyVersions[tenantID] = version
	km.versionsMu.Unlock()

	return nil
}

// RotateKey increments the key version for a tenant
func (km *KeyManager) RotateKey(tenantID string) (int, error) {
	km.versionsMu.Lock()
	defer km.versionsMu.Unlock()

	current := km.keyVersions[tenantID]
	if current == 0 {
		current = 1
	}
	newVersion := current + 1
	km.keyVersions[tenantID] = newVersion

	return newVersion, nil
}

// GetTenantKey is a convenience method that gets the current version's key
func (km *KeyManager) GetTenantKey(tenantID string) ([]byte, int, error) {
	version := km.GetCurrentKeyVersion(tenantID)
	key, err := km.DeriveTenantKey(tenantID, version)
	if err != nil {
		return nil, 0, err
	}
	return key, version, nil
}

// DeriveChunkKey derives a content-specific key for convergent encryption
// This enables cross-tenant deduplication while maintaining per-tenant isolation
func (km *KeyManager) DeriveChunkKey(tenantID string, contentHash []byte) ([]byte, error) {
	tenantKey, _, err := km.GetTenantKey(tenantID)
	if err != nil {
		return nil, fmt.Errorf("failed to get tenant key: %w", err)
	}

	// Convergent key = HMAC-SHA256(tenantKey, contentHash)
	return DeriveConvergentKey(tenantKey, contentHash), nil
}

// ClearCache removes all cached keys
func (km *KeyManager) ClearCache() {
	if km.keyCache == nil {
		return
	}
	km.cacheMu.Lock()
	km.keyCache = make(map[string]*CachedKey)
	km.cacheMu.Unlock()
}

// ClearTenantCache removes cached keys for a specific tenant
func (km *KeyManager) ClearTenantCache(tenantID string) {
	if km.keyCache == nil {
		return
	}
	km.cacheMu.Lock()
	for key := range km.keyCache {
		if len(key) > len(tenantID) && key[:len(tenantID)] == tenantID {
			delete(km.keyCache, key)
		}
	}
	km.cacheMu.Unlock()
}

// CacheStats returns cache statistics
type CacheStats struct {
	Size      int
	HitCount  int64
	MissCount int64
}

// GetCacheStats returns current cache statistics
func (km *KeyManager) GetCacheStats() CacheStats {
	if km.keyCache == nil {
		return CacheStats{}
	}
	km.cacheMu.RLock()
	size := len(km.keyCache)
	km.cacheMu.RUnlock()
	return CacheStats{Size: size}
}

// KeyInfo contains metadata about a key (without the key itself)
type KeyInfo struct {
	TenantID  string    `json:"tenant_id"`
	Version   int       `json:"version"`
	Algorithm string    `json:"algorithm"`
	CreatedAt time.Time `json:"created_at,omitempty"`
}

// GetKeyInfo returns metadata about a tenant's current key
func (km *KeyManager) GetKeyInfo(tenantID string) KeyInfo {
	return KeyInfo{
		TenantID:  tenantID,
		Version:   km.GetCurrentKeyVersion(tenantID),
		Algorithm: "HKDF-SHA256",
	}
}

// ValidateMasterKey checks if the master key matches expected properties
func (km *KeyManager) ValidateMasterKey() error {
	if len(km.masterKey) != 32 {
		return fmt.Errorf("master key is %d bytes, expected 32", len(km.masterKey))
	}

	// Check for weak key (all zeros)
	allZero := true
	for _, b := range km.masterKey {
		if b != 0 {
			allZero = false
			break
		}
	}
	if allZero {
		return fmt.Errorf("master key is all zeros (weak key)")
	}

	return nil
}

// SecureZeroKey overwrites a key in memory (for cleanup)
func SecureZeroKey(key []byte) {
	for i := range key {
		key[i] = 0
	}
}
