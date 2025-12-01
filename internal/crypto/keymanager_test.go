package crypto

import (
	"bytes"
	"encoding/hex"
	"testing"
	"time"
)

func TestNewKeyManager(t *testing.T) {
	masterKey, _ := GenerateMasterKey()
	config := &KeyManagerConfig{
		MasterKey:     masterKey,
		CacheMaxAge:   time.Hour,
		EnableCaching: true,
	}

	km, err := NewKeyManager(config)
	if err != nil {
		t.Fatalf("NewKeyManager failed: %v", err)
	}
	if km == nil {
		t.Fatal("KeyManager is nil")
	}
}

func TestNewKeyManager_HexKey(t *testing.T) {
	masterKeyHex, _ := GenerateMasterKeyHex()
	config := &KeyManagerConfig{
		MasterKeyHex:  masterKeyHex,
		EnableCaching: true,
	}

	km, err := NewKeyManager(config)
	if err != nil {
		t.Fatalf("NewKeyManager with hex key failed: %v", err)
	}
	if km == nil {
		t.Fatal("KeyManager is nil")
	}
}

func TestNewKeyManager_InvalidKey(t *testing.T) {
	tests := []struct {
		name   string
		config *KeyManagerConfig
	}{
		{"no key", &KeyManagerConfig{}},
		{"short key", &KeyManagerConfig{MasterKey: make([]byte, 16)}},
		{"long key", &KeyManagerConfig{MasterKey: make([]byte, 64)}},
		{"invalid hex", &KeyManagerConfig{MasterKeyHex: "not-hex"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := NewKeyManager(tt.config)
			if err == nil {
				t.Error("Expected error for invalid config")
			}
		})
	}
}

func TestDeriveTenantKey(t *testing.T) {
	masterKey, _ := GenerateMasterKey()
	km, _ := NewKeyManager(&KeyManagerConfig{
		MasterKey:     masterKey,
		EnableCaching: true,
	})

	key1, err := km.DeriveTenantKey("tenant-1", 1)
	if err != nil {
		t.Fatalf("DeriveTenantKey failed: %v", err)
	}

	if len(key1) != 32 {
		t.Errorf("Key length = %d, want 32", len(key1))
	}

	// Same tenant + version should produce same key
	key2, _ := km.DeriveTenantKey("tenant-1", 1)
	if !bytes.Equal(key1, key2) {
		t.Error("Same tenant/version should produce same key")
	}

	// Different tenant should produce different key
	key3, _ := km.DeriveTenantKey("tenant-2", 1)
	if bytes.Equal(key1, key3) {
		t.Error("Different tenants should have different keys")
	}

	// Different version should produce different key
	key4, _ := km.DeriveTenantKey("tenant-1", 2)
	if bytes.Equal(key1, key4) {
		t.Error("Different versions should have different keys")
	}
}

func TestDeriveTenantKey_Deterministic(t *testing.T) {
	// Same master key should always derive same tenant keys
	masterKey := make([]byte, 32)
	for i := range masterKey {
		masterKey[i] = byte(i)
	}

	km1, _ := NewKeyManager(&KeyManagerConfig{MasterKey: masterKey})
	km2, _ := NewKeyManager(&KeyManagerConfig{MasterKey: masterKey})

	key1, _ := km1.DeriveTenantKey("test-tenant", 1)
	key2, _ := km2.DeriveTenantKey("test-tenant", 1)

	if !bytes.Equal(key1, key2) {
		t.Error("Same master key should derive same tenant keys")
	}
}

func TestKeyVersioning(t *testing.T) {
	masterKey, _ := GenerateMasterKey()
	km, _ := NewKeyManager(&KeyManagerConfig{MasterKey: masterKey})

	tenantID := "version-test-tenant"

	// Default version should be 1
	v := km.GetCurrentKeyVersion(tenantID)
	if v != 1 {
		t.Errorf("Default version = %d, want 1", v)
	}

	// Set version
	_ = km.SetKeyVersion(tenantID, 5)
	v = km.GetCurrentKeyVersion(tenantID)
	if v != 5 {
		t.Errorf("Version after set = %d, want 5", v)
	}

	// Rotate key
	newV, _ := km.RotateKey(tenantID)
	if newV != 6 {
		t.Errorf("Version after rotate = %d, want 6", newV)
	}

	// Invalid version
	err := km.SetKeyVersion(tenantID, 0)
	if err == nil {
		t.Error("Should reject version 0")
	}
}

func TestGetTenantKey(t *testing.T) {
	masterKey, _ := GenerateMasterKey()
	km, _ := NewKeyManager(&KeyManagerConfig{MasterKey: masterKey})

	tenantID := "convenience-test"

	key, version, err := km.GetTenantKey(tenantID)
	if err != nil {
		t.Fatalf("GetTenantKey failed: %v", err)
	}

	if len(key) != 32 {
		t.Errorf("Key length = %d, want 32", len(key))
	}
	if version != 1 {
		t.Errorf("Version = %d, want 1", version)
	}

	// After rotation, should get new key
	_, _ = km.RotateKey(tenantID)
	key2, version2, _ := km.GetTenantKey(tenantID)

	if version2 != 2 {
		t.Errorf("Version after rotate = %d, want 2", version2)
	}
	if bytes.Equal(key, key2) {
		t.Error("Key should change after rotation")
	}
}

func TestDeriveChunkKey(t *testing.T) {
	masterKey, _ := GenerateMasterKey()
	km, _ := NewKeyManager(&KeyManagerConfig{MasterKey: masterKey})

	contentHash := []byte("0123456789abcdef0123456789abcdef")

	// Same content, same tenant = same key
	key1, _ := km.DeriveChunkKey("tenant-1", contentHash)
	key2, _ := km.DeriveChunkKey("tenant-1", contentHash)
	if !bytes.Equal(key1, key2) {
		t.Error("Same content+tenant should produce same key")
	}

	// Same content, different tenant = different key
	key3, _ := km.DeriveChunkKey("tenant-2", contentHash)
	if bytes.Equal(key1, key3) {
		t.Error("Different tenants should have different chunk keys")
	}

	// Different content, same tenant = different key
	differentHash := []byte("different-content-hash-here!!!!!")
	key4, _ := km.DeriveChunkKey("tenant-1", differentHash)
	if bytes.Equal(key1, key4) {
		t.Error("Different content should have different chunk keys")
	}
}

func TestCaching(t *testing.T) {
	masterKey, _ := GenerateMasterKey()
	km, _ := NewKeyManager(&KeyManagerConfig{
		MasterKey:     masterKey,
		EnableCaching: true,
		CacheMaxAge:   time.Hour,
	})

	// Derive a key (should cache it)
	_, _ = km.DeriveTenantKey("cached-tenant", 1)

	stats := km.GetCacheStats()
	if stats.Size != 1 {
		t.Errorf("Cache size = %d, want 1", stats.Size)
	}

	// Derive more keys
	_, _ = km.DeriveTenantKey("cached-tenant", 2)
	_, _ = km.DeriveTenantKey("other-tenant", 1)

	stats = km.GetCacheStats()
	if stats.Size != 3 {
		t.Errorf("Cache size = %d, want 3", stats.Size)
	}

	// Clear tenant cache
	km.ClearTenantCache("cached-tenant")
	stats = km.GetCacheStats()
	if stats.Size != 1 {
		t.Errorf("Cache size after clear = %d, want 1", stats.Size)
	}

	// Clear all cache
	km.ClearCache()
	stats = km.GetCacheStats()
	if stats.Size != 0 {
		t.Errorf("Cache size after full clear = %d, want 0", stats.Size)
	}
}

func TestNoCaching(t *testing.T) {
	masterKey, _ := GenerateMasterKey()
	km, _ := NewKeyManager(&KeyManagerConfig{
		MasterKey:     masterKey,
		EnableCaching: false,
	})

	_, _ = km.DeriveTenantKey("tenant", 1)
	stats := km.GetCacheStats()
	if stats.Size != 0 {
		t.Errorf("Cache should be empty when disabled, got size %d", stats.Size)
	}
}

func TestGetKeyInfo(t *testing.T) {
	masterKey, _ := GenerateMasterKey()
	km, _ := NewKeyManager(&KeyManagerConfig{MasterKey: masterKey})

	info := km.GetKeyInfo("info-test-tenant")

	if info.TenantID != "info-test-tenant" {
		t.Errorf("TenantID = %s, want info-test-tenant", info.TenantID)
	}
	if info.Version != 1 {
		t.Errorf("Version = %d, want 1", info.Version)
	}
	if info.Algorithm != "HKDF-SHA256" {
		t.Errorf("Algorithm = %s, want HKDF-SHA256", info.Algorithm)
	}
}

func TestValidateMasterKey(t *testing.T) {
	// Valid key
	masterKey, _ := GenerateMasterKey()
	km, _ := NewKeyManager(&KeyManagerConfig{MasterKey: masterKey})
	if err := km.ValidateMasterKey(); err != nil {
		t.Errorf("Valid key should pass validation: %v", err)
	}

	// Weak key (all zeros) - need to bypass constructor
	weakKm := &KeyManager{masterKey: make([]byte, 32)}
	if err := weakKm.ValidateMasterKey(); err == nil {
		t.Error("All-zero key should fail validation")
	}
}

func TestSecureZeroKey(t *testing.T) {
	key := []byte{1, 2, 3, 4, 5, 6, 7, 8}
	SecureZeroKey(key)

	for i, b := range key {
		if b != 0 {
			t.Errorf("Byte %d not zeroed: %d", i, b)
		}
	}
}

func TestGenerateMasterKey(t *testing.T) {
	key1, err := GenerateMasterKey()
	if err != nil {
		t.Fatalf("GenerateMasterKey failed: %v", err)
	}
	if len(key1) != 32 {
		t.Errorf("Key length = %d, want 32", len(key1))
	}

	// Two generated keys should be different
	key2, _ := GenerateMasterKey()
	if bytes.Equal(key1, key2) {
		t.Error("Generated keys should be unique")
	}
}

func TestGenerateMasterKeyHex(t *testing.T) {
	hexKey, err := GenerateMasterKeyHex()
	if err != nil {
		t.Fatalf("GenerateMasterKeyHex failed: %v", err)
	}
	if len(hexKey) != 64 {
		t.Errorf("Hex key length = %d, want 64", len(hexKey))
	}

	// Should be valid hex
	_, err = hex.DecodeString(hexKey)
	if err != nil {
		t.Errorf("Invalid hex: %v", err)
	}
}

// Benchmark

func BenchmarkDeriveTenantKey_Uncached(b *testing.B) {
	masterKey, _ := GenerateMasterKey()
	km, _ := NewKeyManager(&KeyManagerConfig{
		MasterKey:     masterKey,
		EnableCaching: false,
	})

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = km.DeriveTenantKey("benchmark-tenant", 1)
	}
}

func BenchmarkDeriveTenantKey_Cached(b *testing.B) {
	masterKey, _ := GenerateMasterKey()
	km, _ := NewKeyManager(&KeyManagerConfig{
		MasterKey:     masterKey,
		EnableCaching: true,
		CacheMaxAge:   time.Hour,
	})

	// Warm cache
	_, _ = km.DeriveTenantKey("benchmark-tenant", 1)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = km.DeriveTenantKey("benchmark-tenant", 1)
	}
}
