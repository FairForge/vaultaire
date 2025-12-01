package crypto

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"testing"
)

func TestAESGCMEncryptor_Basic(t *testing.T) {
	e := NewAESGCMEncryptor()

	if e.Algorithm() != EncryptionAESGCM {
		t.Errorf("Algorithm() = %v, want %v", e.Algorithm(), EncryptionAESGCM)
	}
	if e.KeySize() != 32 {
		t.Errorf("KeySize() = %d, want 32", e.KeySize())
	}
	if e.NonceSize() != 12 {
		t.Errorf("NonceSize() = %d, want 12", e.NonceSize())
	}
}

func TestAESGCMEncryptor_RoundTrip(t *testing.T) {
	e := NewAESGCMEncryptor()
	key, _ := GenerateRandomKey(32)
	plaintext := []byte("Hello, World! This is a test of AES-GCM encryption.")

	ciphertext, nonce, err := e.Encrypt(key, plaintext)
	if err != nil {
		t.Fatalf("Encrypt failed: %v", err)
	}

	if bytes.Equal(plaintext, ciphertext) {
		t.Error("Ciphertext should not equal plaintext")
	}

	decrypted, err := e.Decrypt(key, nonce, ciphertext)
	if err != nil {
		t.Fatalf("Decrypt failed: %v", err)
	}

	if !bytes.Equal(plaintext, decrypted) {
		t.Error("Decrypted data doesn't match original")
	}
}

func TestAESGCMEncryptor_EmptyData(t *testing.T) {
	e := NewAESGCMEncryptor()
	key, _ := GenerateRandomKey(32)

	ciphertext, nonce, err := e.Encrypt(key, []byte{})
	if err != nil {
		t.Fatalf("Encrypt empty failed: %v", err)
	}

	decrypted, err := e.Decrypt(key, nonce, ciphertext)
	if err != nil {
		t.Fatalf("Decrypt empty failed: %v", err)
	}

	if len(decrypted) != 0 {
		t.Errorf("Expected empty result, got %d bytes", len(decrypted))
	}
}

func TestAESGCMEncryptor_LargeData(t *testing.T) {
	e := NewAESGCMEncryptor()
	key, _ := GenerateRandomKey(32)
	plaintext := make([]byte, 4*1024*1024) // 4MB
	_, _ = rand.Read(plaintext)

	ciphertext, nonce, err := e.Encrypt(key, plaintext)
	if err != nil {
		t.Fatalf("Encrypt failed: %v", err)
	}

	// Ciphertext should be slightly larger (auth tag)
	expectedSize := len(plaintext) + 16 // GCM auth tag
	if len(ciphertext) != expectedSize {
		t.Errorf("Ciphertext size = %d, want %d", len(ciphertext), expectedSize)
	}

	decrypted, err := e.Decrypt(key, nonce, ciphertext)
	if err != nil {
		t.Fatalf("Decrypt failed: %v", err)
	}

	if !bytes.Equal(plaintext, decrypted) {
		t.Error("Decrypted data doesn't match original")
	}
}

func TestAESGCMEncryptor_InvalidKey(t *testing.T) {
	e := NewAESGCMEncryptor()
	shortKey := make([]byte, 16) // Too short

	_, _, err := e.Encrypt(shortKey, []byte("test"))
	if err == nil {
		t.Error("Expected error for short key")
	}
}

func TestAESGCMEncryptor_TamperedCiphertext(t *testing.T) {
	e := NewAESGCMEncryptor()
	key, _ := GenerateRandomKey(32)
	plaintext := []byte("Secret message")

	ciphertext, nonce, _ := e.Encrypt(key, plaintext)

	// Tamper with ciphertext
	ciphertext[0] ^= 0xFF

	_, err := e.Decrypt(key, nonce, ciphertext)
	if err == nil {
		t.Error("Expected error for tampered ciphertext")
	}
}

func TestAESGCMEncryptor_WrongKey(t *testing.T) {
	e := NewAESGCMEncryptor()
	key1, _ := GenerateRandomKey(32)
	key2, _ := GenerateRandomKey(32)
	plaintext := []byte("Secret message")

	ciphertext, nonce, _ := e.Encrypt(key1, plaintext)

	_, err := e.Decrypt(key2, nonce, ciphertext)
	if err == nil {
		t.Error("Expected error for wrong key")
	}
}

func TestChaCha20Poly1305Encryptor_Basic(t *testing.T) {
	e := NewChaCha20Poly1305Encryptor()

	if e.Algorithm() != EncryptionChaCha {
		t.Errorf("Algorithm() = %v, want %v", e.Algorithm(), EncryptionChaCha)
	}
	if e.KeySize() != 32 {
		t.Errorf("KeySize() = %d, want 32", e.KeySize())
	}
	if e.NonceSize() != 24 {
		t.Errorf("NonceSize() = %d, want 24", e.NonceSize())
	}
}

func TestChaCha20Poly1305Encryptor_RoundTrip(t *testing.T) {
	e := NewChaCha20Poly1305Encryptor()
	key, _ := GenerateRandomKey(32)
	plaintext := []byte("Hello, World! This is a test of ChaCha20-Poly1305 encryption.")

	ciphertext, nonce, err := e.Encrypt(key, plaintext)
	if err != nil {
		t.Fatalf("Encrypt failed: %v", err)
	}

	decrypted, err := e.Decrypt(key, nonce, ciphertext)
	if err != nil {
		t.Fatalf("Decrypt failed: %v", err)
	}

	if !bytes.Equal(plaintext, decrypted) {
		t.Error("Decrypted data doesn't match original")
	}
}

func TestChaCha20Poly1305Encryptor_TamperedCiphertext(t *testing.T) {
	e := NewChaCha20Poly1305Encryptor()
	key, _ := GenerateRandomKey(32)
	plaintext := []byte("Secret message")

	ciphertext, nonce, _ := e.Encrypt(key, plaintext)
	ciphertext[0] ^= 0xFF

	_, err := e.Decrypt(key, nonce, ciphertext)
	if err == nil {
		t.Error("Expected error for tampered ciphertext")
	}
}

func TestNoopEncryptor(t *testing.T) {
	e := NewNoopEncryptor()

	if e.Algorithm() != EncryptionNone {
		t.Errorf("Algorithm() = %v, want %v", e.Algorithm(), EncryptionNone)
	}

	plaintext := []byte("Test data")
	ciphertext, nonce, err := e.Encrypt(nil, plaintext)
	if err != nil {
		t.Fatalf("Encrypt failed: %v", err)
	}

	if !bytes.Equal(plaintext, ciphertext) {
		t.Error("NoopEncryptor should return unchanged data")
	}
	if nonce != nil {
		t.Error("NoopEncryptor should return nil nonce")
	}

	decrypted, err := e.Decrypt(nil, nil, ciphertext)
	if err != nil {
		t.Fatalf("Decrypt failed: %v", err)
	}
	if !bytes.Equal(plaintext, decrypted) {
		t.Error("NoopEncryptor decrypt should return unchanged data")
	}
}

func TestNewEncryptorFromConfig(t *testing.T) {
	tests := []struct {
		name     string
		config   PipelineConfig
		wantAlgo EncryptionAlgorithm
	}{
		{"disabled", PipelineConfig{EncryptionEnabled: false}, EncryptionNone},
		{"aesgcm", PipelineConfig{EncryptionEnabled: true, EncryptionAlgo: EncryptionAESGCM}, EncryptionAESGCM},
		{"chacha", PipelineConfig{EncryptionEnabled: true, EncryptionAlgo: EncryptionChaCha}, EncryptionChaCha},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e, err := NewEncryptorFromConfig(tt.config)
			if err != nil {
				t.Fatalf("NewEncryptorFromConfig() error = %v", err)
			}
			if e.Algorithm() != tt.wantAlgo {
				t.Errorf("Algorithm() = %v, want %v", e.Algorithm(), tt.wantAlgo)
			}
		})
	}
}

func TestDeriveConvergentKey(t *testing.T) {
	tenantKey := make([]byte, 32)
	_, _ = rand.Read(tenantKey)

	contentHash1 := sha256.Sum256([]byte("content1"))
	contentHash2 := sha256.Sum256([]byte("content2"))

	key1a := DeriveConvergentKey(tenantKey, contentHash1[:])
	key1b := DeriveConvergentKey(tenantKey, contentHash1[:])
	key2 := DeriveConvergentKey(tenantKey, contentHash2[:])

	// Same content should produce same key
	if !bytes.Equal(key1a, key1b) {
		t.Error("Same content should produce same key")
	}

	// Different content should produce different key
	if bytes.Equal(key1a, key2) {
		t.Error("Different content should produce different key")
	}

	// Key should be 32 bytes
	if len(key1a) != 32 {
		t.Errorf("Key size = %d, want 32", len(key1a))
	}
}

func TestDeriveConvergentKey_CrossTenant(t *testing.T) {
	tenant1Key := make([]byte, 32)
	tenant2Key := make([]byte, 32)
	_, _ = rand.Read(tenant1Key)
	_, _ = rand.Read(tenant2Key)

	// Same content, different tenants
	contentHash := sha256.Sum256([]byte("shared content"))

	key1 := DeriveConvergentKey(tenant1Key, contentHash[:])
	key2 := DeriveConvergentKey(tenant2Key, contentHash[:])

	// Different tenants should have different keys (even for same content)
	if bytes.Equal(key1, key2) {
		t.Error("Different tenants should have different keys")
	}
}

func TestGenerateRandomKey(t *testing.T) {
	key1, err := GenerateRandomKey(32)
	if err != nil {
		t.Fatalf("GenerateRandomKey failed: %v", err)
	}

	key2, _ := GenerateRandomKey(32)

	if len(key1) != 32 {
		t.Errorf("Key size = %d, want 32", len(key1))
	}

	// Two random keys should be different
	if bytes.Equal(key1, key2) {
		t.Error("Random keys should be different")
	}
}

func TestGenerateTenantKey(t *testing.T) {
	key, err := GenerateTenantKey()
	if err != nil {
		t.Fatalf("GenerateTenantKey failed: %v", err)
	}

	if len(key) != 32 {
		t.Errorf("Tenant key size = %d, want 32", len(key))
	}
}

func TestEncryptChunk(t *testing.T) {
	e := NewAESGCMEncryptor()
	key, _ := GenerateRandomKey(32)

	chunkData := []byte("chunk data for encryption test")
	chunk := &Chunk{
		Hash: "abc123def456",
		Data: chunkData,
		Size: len(chunkData),
	}

	encrypted, err := EncryptChunk(e, key, chunk, 1)
	if err != nil {
		t.Fatalf("EncryptChunk failed: %v", err)
	}

	if encrypted.PlaintextHash != chunk.Hash {
		t.Errorf("PlaintextHash = %s, want %s", encrypted.PlaintextHash, chunk.Hash)
	}
	if encrypted.KeyVersion != 1 {
		t.Errorf("KeyVersion = %d, want 1", encrypted.KeyVersion)
	}
	if len(encrypted.CiphertextHash) != 64 { // SHA256 hex
		t.Errorf("CiphertextHash length = %d, want 64", len(encrypted.CiphertextHash))
	}
	if encrypted.Algorithm != EncryptionAESGCM {
		t.Errorf("Algorithm = %v, want %v", encrypted.Algorithm, EncryptionAESGCM)
	}

	// Decrypt and verify
	decrypted, err := DecryptChunk(e, key, encrypted)
	if err != nil {
		t.Fatalf("DecryptChunk failed: %v", err)
	}
	if !bytes.Equal(chunkData, decrypted) {
		t.Error("Decrypted data doesn't match original")
	}
}

func TestEncryptionStats(t *testing.T) {
	stats := &EncryptionStats{}

	stats.AddEncrypted(1000)
	stats.AddEncrypted(2000)
	stats.AddDecrypted(1500)

	if stats.ChunksEncrypted != 2 {
		t.Errorf("ChunksEncrypted = %d, want 2", stats.ChunksEncrypted)
	}
	if stats.BytesEncrypted != 3000 {
		t.Errorf("BytesEncrypted = %d, want 3000", stats.BytesEncrypted)
	}
	if stats.ChunksDecrypted != 1 {
		t.Errorf("ChunksDecrypted = %d, want 1", stats.ChunksDecrypted)
	}
	if stats.BytesDecrypted != 1500 {
		t.Errorf("BytesDecrypted = %d, want 1500", stats.BytesDecrypted)
	}
}

func TestEncryptionOverhead(t *testing.T) {
	tests := []struct {
		algo EncryptionAlgorithm
		want int
	}{
		{EncryptionAESGCM, 28}, // 16 + 12
		{EncryptionChaCha, 40}, // 16 + 24
		{EncryptionNone, 0},
	}

	for _, tt := range tests {
		t.Run(string(tt.algo), func(t *testing.T) {
			if got := EncryptionOverhead(tt.algo); got != tt.want {
				t.Errorf("EncryptionOverhead(%s) = %d, want %d", tt.algo, got, tt.want)
			}
		})
	}
}

// Benchmarks

func BenchmarkAESGCM_Encrypt_4MB(b *testing.B) {
	e := NewAESGCMEncryptor()
	key, _ := GenerateRandomKey(32)
	plaintext := make([]byte, 4*1024*1024)
	_, _ = rand.Read(plaintext)

	b.ResetTimer()
	b.SetBytes(int64(len(plaintext)))

	for i := 0; i < b.N; i++ {
		_, _, _ = e.Encrypt(key, plaintext)
	}
}

func BenchmarkAESGCM_Decrypt_4MB(b *testing.B) {
	e := NewAESGCMEncryptor()
	key, _ := GenerateRandomKey(32)
	plaintext := make([]byte, 4*1024*1024)
	_, _ = rand.Read(plaintext)
	ciphertext, nonce, _ := e.Encrypt(key, plaintext)

	b.ResetTimer()
	b.SetBytes(int64(len(plaintext)))

	for i := 0; i < b.N; i++ {
		_, _ = e.Decrypt(key, nonce, ciphertext)
	}
}

func BenchmarkChaCha20_Encrypt_4MB(b *testing.B) {
	e := NewChaCha20Poly1305Encryptor()
	key, _ := GenerateRandomKey(32)
	plaintext := make([]byte, 4*1024*1024)
	_, _ = rand.Read(plaintext)

	b.ResetTimer()
	b.SetBytes(int64(len(plaintext)))

	for i := 0; i < b.N; i++ {
		_, _, _ = e.Encrypt(key, plaintext)
	}
}

func BenchmarkDeriveConvergentKey(b *testing.B) {
	tenantKey := make([]byte, 32)
	contentHash := make([]byte, 32)
	_, _ = rand.Read(tenantKey)
	_, _ = rand.Read(contentHash)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = DeriveConvergentKey(tenantKey, contentHash)
	}
}
