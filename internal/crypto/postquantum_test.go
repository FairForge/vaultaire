package crypto

import (
	"bytes"
	"testing"
)

func TestGenerateMLKEMKeyPair(t *testing.T) {
	keyPair, err := GenerateMLKEMKeyPair()
	if err != nil {
		t.Fatalf("GenerateMLKEMKeyPair failed: %v", err)
	}

	if len(keyPair.PublicKey) != PublicKeySize() {
		t.Errorf("Public key size = %d, want %d", len(keyPair.PublicKey), PublicKeySize())
	}

	if len(keyPair.PrivateKey) != PrivateKeySize() {
		t.Errorf("Private key size = %d, want %d", len(keyPair.PrivateKey), PrivateKeySize())
	}

	t.Logf("ML-KEM-768 key sizes: public=%d, private=%d",
		len(keyPair.PublicKey), len(keyPair.PrivateKey))
}

func TestEncapsulateDecapsulate(t *testing.T) {
	keyPair, err := GenerateMLKEMKeyPair()
	if err != nil {
		t.Fatalf("Key generation failed: %v", err)
	}

	// Encapsulate using public key
	encap, err := Encapsulate(keyPair.PublicKey)
	if err != nil {
		t.Fatalf("Encapsulate failed: %v", err)
	}

	if len(encap.Ciphertext) != CiphertextSize() {
		t.Errorf("Ciphertext size = %d, want %d", len(encap.Ciphertext), CiphertextSize())
	}

	if len(encap.SharedSecret) != 32 {
		t.Errorf("Shared secret size = %d, want 32", len(encap.SharedSecret))
	}

	// Decapsulate using private key
	recovered, err := Decapsulate(keyPair.PrivateKey, encap.Ciphertext)
	if err != nil {
		t.Fatalf("Decapsulate failed: %v", err)
	}

	// Shared secrets should match
	if !bytes.Equal(encap.SharedSecret, recovered) {
		t.Error("Shared secrets don't match after decapsulation")
	}

	t.Logf("KEM ciphertext size: %d bytes", len(encap.Ciphertext))
}

func TestPostQuantumEncryptor_RoundTrip(t *testing.T) {
	keyPair, err := GenerateMLKEMKeyPair()
	if err != nil {
		t.Fatalf("Key generation failed: %v", err)
	}

	encryptor := NewPostQuantumEncryptor()
	plaintext := []byte("Hello, Post-Quantum World! This message is secure against quantum computers.")

	// Encrypt
	encrypted, err := encryptor.Encrypt(keyPair.PublicKey, plaintext)
	if err != nil {
		t.Fatalf("Encrypt failed: %v", err)
	}

	if encrypted.Algorithm != "ML-KEM-768+AES-256-GCM" {
		t.Errorf("Algorithm = %s, want ML-KEM-768+AES-256-GCM", encrypted.Algorithm)
	}

	// Decrypt
	decrypted, err := encryptor.Decrypt(keyPair.PrivateKey, encrypted)
	if err != nil {
		t.Fatalf("Decrypt failed: %v", err)
	}

	if !bytes.Equal(plaintext, decrypted) {
		t.Error("Decrypted data doesn't match original")
	}
}

func TestPostQuantumEncryptor_LargeData(t *testing.T) {
	keyPair, _ := GenerateMLKEMKeyPair()
	encryptor := NewPostQuantumEncryptor()

	// 1MB of data
	plaintext := bytes.Repeat([]byte("Post-quantum secure! "), 50000)

	encrypted, err := encryptor.Encrypt(keyPair.PublicKey, plaintext)
	if err != nil {
		t.Fatalf("Encrypt failed: %v", err)
	}

	decrypted, err := encryptor.Decrypt(keyPair.PrivateKey, encrypted)
	if err != nil {
		t.Fatalf("Decrypt failed: %v", err)
	}

	if !bytes.Equal(plaintext, decrypted) {
		t.Error("Large data round-trip failed")
	}

	t.Logf("Encrypted 1MB: overhead = %d bytes (KEM ciphertext + nonce + auth tag)",
		len(encrypted.KEMCiphertext)+len(encrypted.Nonce)+16)
}

func TestPostQuantumEncryptor_EmptyData(t *testing.T) {
	keyPair, _ := GenerateMLKEMKeyPair()
	encryptor := NewPostQuantumEncryptor()

	encrypted, err := encryptor.Encrypt(keyPair.PublicKey, []byte{})
	if err != nil {
		t.Fatalf("Encrypt empty failed: %v", err)
	}

	decrypted, err := encryptor.Decrypt(keyPair.PrivateKey, encrypted)
	if err != nil {
		t.Fatalf("Decrypt empty failed: %v", err)
	}

	if len(decrypted) != 0 {
		t.Errorf("Expected empty result, got %d bytes", len(decrypted))
	}
}

func TestPostQuantumEncryptor_WrongKey(t *testing.T) {
	keyPair1, _ := GenerateMLKEMKeyPair()
	keyPair2, _ := GenerateMLKEMKeyPair()
	encryptor := NewPostQuantumEncryptor()

	plaintext := []byte("Secret message")

	// Encrypt with key pair 1
	encrypted, _ := encryptor.Encrypt(keyPair1.PublicKey, plaintext)

	// Try to decrypt with key pair 2 (should fail)
	_, err := encryptor.Decrypt(keyPair2.PrivateKey, encrypted)
	if err == nil {
		t.Error("Should fail to decrypt with wrong key")
	}
}

func TestPostQuantumEncryptor_TamperedCiphertext(t *testing.T) {
	keyPair, _ := GenerateMLKEMKeyPair()
	encryptor := NewPostQuantumEncryptor()

	plaintext := []byte("Tamper test message")

	encrypted, _ := encryptor.Encrypt(keyPair.PublicKey, plaintext)

	// Tamper with the AES ciphertext
	encrypted.Ciphertext[0] ^= 0xFF

	_, err := encryptor.Decrypt(keyPair.PrivateKey, encrypted)
	if err == nil {
		t.Error("Should fail to decrypt tampered ciphertext")
	}
}

func TestGetPQKeyInfo(t *testing.T) {
	info := GetPQKeyInfo()

	if info.Algorithm != "ML-KEM-768" {
		t.Errorf("Algorithm = %s, want ML-KEM-768", info.Algorithm)
	}

	if info.PublicKeySize <= 0 {
		t.Error("PublicKeySize should be positive")
	}

	if info.SharedKeySize != 32 {
		t.Errorf("SharedKeySize = %d, want 32", info.SharedKeySize)
	}

	t.Logf("PQ Key Info: %+v", info)
}

func TestIsPQEnabled(t *testing.T) {
	enabled := IsPQEnabled()
	if !enabled {
		t.Error("Post-quantum encryption should be enabled")
	}
}

func TestPostQuantumEncryptor_Algorithm(t *testing.T) {
	encryptor := NewPostQuantumEncryptor()

	if encryptor.Algorithm() != EncryptionAESGCMPQ {
		t.Errorf("Algorithm() = %v, want %v", encryptor.Algorithm(), EncryptionAESGCMPQ)
	}

	if encryptor.KeySize() != 32 {
		t.Errorf("KeySize() = %d, want 32", encryptor.KeySize())
	}
}

// Benchmarks

func BenchmarkMLKEMKeyGeneration(b *testing.B) {
	for i := 0; i < b.N; i++ {
		_, _ = GenerateMLKEMKeyPair()
	}
}

func BenchmarkMLKEMEncapsulate(b *testing.B) {
	keyPair, _ := GenerateMLKEMKeyPair()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = Encapsulate(keyPair.PublicKey)
	}
}

func BenchmarkMLKEMDecapsulate(b *testing.B) {
	keyPair, _ := GenerateMLKEMKeyPair()
	encap, _ := Encapsulate(keyPair.PublicKey)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = Decapsulate(keyPair.PrivateKey, encap.Ciphertext)
	}
}

func BenchmarkPQEncrypt_1KB(b *testing.B) {
	keyPair, _ := GenerateMLKEMKeyPair()
	encryptor := NewPostQuantumEncryptor()
	plaintext := bytes.Repeat([]byte("x"), 1024)

	b.ResetTimer()
	b.SetBytes(1024)
	for i := 0; i < b.N; i++ {
		_, _ = encryptor.Encrypt(keyPair.PublicKey, plaintext)
	}
}

func BenchmarkPQDecrypt_1KB(b *testing.B) {
	keyPair, _ := GenerateMLKEMKeyPair()
	encryptor := NewPostQuantumEncryptor()
	plaintext := bytes.Repeat([]byte("x"), 1024)
	encrypted, _ := encryptor.Encrypt(keyPair.PublicKey, plaintext)

	b.ResetTimer()
	b.SetBytes(1024)
	for i := 0; i < b.N; i++ {
		_, _ = encryptor.Decrypt(keyPair.PrivateKey, encrypted)
	}
}
