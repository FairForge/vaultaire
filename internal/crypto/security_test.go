package crypto

import (
	"testing"
)

// Security Tests - verify cryptographic properties

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
