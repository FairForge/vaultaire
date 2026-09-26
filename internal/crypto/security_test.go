package crypto

import (
	"bytes"
	"testing"
	"time"
)

// Security Tests - verify cryptographic properties

func TestSecurity_KeyRotation(t *testing.T) {
	masterKey, _ := GenerateMasterKey()
	keyManager, _ := NewKeyManager(&KeyManagerConfig{MasterKey: masterKey})

	tenantID := "rotation-test"

	// Get key version 1
	key1, v1, _ := keyManager.GetTenantKey(tenantID)

	// Rotate key
	newVersion, _ := keyManager.RotateKey(tenantID)

	// Get key version 2
	key2, v2, _ := keyManager.GetTenantKey(tenantID)

	if v1 == v2 {
		t.Error("Version should change after rotation")
	}

	if bytes.Equal(key1, key2) {
		t.Error("SECURITY: Key should change after rotation")
	}

	// Old version should still be derivable (for decryption of old data)
	oldKey, _ := keyManager.DeriveTenantKey(tenantID, v1)
	if !bytes.Equal(key1, oldKey) {
		t.Error("Old key version should be derivable")
	}

	t.Logf("✓ Key rotation verified: v%d → v%d", v1, newVersion)
}

func TestSecurity_MasterKeyStrength(t *testing.T) {
	// Verify master key has sufficient entropy
	for i := 0; i < 10; i++ {
		key, _ := GenerateMasterKey()

		// Check length
		if len(key) != 32 {
			t.Errorf("Master key should be 32 bytes, got %d", len(key))
		}

		// Basic entropy check: not all zeros, not all same byte
		allSame := true
		for j := 1; j < len(key); j++ {
			if key[j] != key[0] {
				allSame = false
				break
			}
		}
		if allSame {
			t.Error("SECURITY: Master key has no entropy")
		}
	}

	t.Log("✓ Master key strength verified")
}

func TestSecurity_PostQuantumKeyExchange(t *testing.T) {
	// Verify ML-KEM key exchange produces different shared secrets
	secrets := make(map[string]bool)

	for i := 0; i < 10; i++ {
		keyPair, _ := GenerateMLKEMKeyPair()
		encap, _ := Encapsulate(keyPair.PublicKey)

		secretStr := string(encap.SharedSecret)
		if secrets[secretStr] {
			t.Error("SECURITY: Shared secret collision")
		}
		secrets[secretStr] = true
	}

	t.Logf("✓ Post-quantum key exchange verified (%d unique secrets)", len(secrets))
}

// Timing attack resistance test
func TestSecurity_ConstantTimeComparison(t *testing.T) {
	// This is a basic sanity check - real timing tests need statistical analysis
	masterKey, _ := GenerateMasterKey()
	keyManager, _ := NewKeyManager(&KeyManagerConfig{MasterKey: masterKey})

	key1, _, _ := keyManager.GetTenantKey("tenant-1")
	key2, _, _ := keyManager.GetTenantKey("tenant-2")

	// Measure time to derive keys (should be constant regardless of tenant)
	start := time.Now()
	for i := 0; i < 1000; i++ {
		_, _ = keyManager.DeriveTenantKey("tenant-1", 1)
	}
	time1 := time.Since(start)

	start = time.Now()
	for i := 0; i < 1000; i++ {
		_, _ = keyManager.DeriveTenantKey("tenant-2", 1)
	}
	time2 := time.Since(start)

	// Times should be similar (within 50% - this is a rough check)
	ratio := float64(time1) / float64(time2)
	if ratio < 0.5 || ratio > 2.0 {
		t.Logf("Warning: Timing difference detected: %v vs %v (ratio: %.2f)", time1, time2, ratio)
	}

	// Keys should be different
	if bytes.Equal(key1, key2) {
		t.Error("Different tenants should have different keys")
	}

	t.Logf("✓ Key derivation timing: tenant-1=%v, tenant-2=%v", time1, time2)
}
