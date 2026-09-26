package crypto

import (
	"bytes"
	"crypto/rand"
	"fmt"
	"testing"
)

// Benchmarks for the crypto primitives (compression, encryption, chunking, KDF, ML-KEM)

func BenchmarkCompression_Zstd(b *testing.B) {
	compressor, _ := NewZstdCompressor(3)
	data := bytes.Repeat([]byte("benchmark compression data "), 10000) // ~270KB compressible

	b.ResetTimer()
	b.SetBytes(int64(len(data)))

	for i := 0; i < b.N; i++ {
		_, _ = compressor.Compress(data)
	}
}

func BenchmarkDecompression_Zstd(b *testing.B) {
	compressor, _ := NewZstdCompressor(3)
	data := bytes.Repeat([]byte("benchmark compression data "), 10000)
	compressed, _ := compressor.Compress(data)

	b.ResetTimer()
	b.SetBytes(int64(len(data)))

	for i := 0; i < b.N; i++ {
		_, _ = compressor.Decompress(compressed)
	}
}

func BenchmarkEncryption_AES256GCM(b *testing.B) {
	encryptor := NewAESGCMEncryptor()
	key := make([]byte, 32)
	_, _ = rand.Read(key)
	data := make([]byte, 1024*1024) // 1MB
	_, _ = rand.Read(data)

	b.ResetTimer()
	b.SetBytes(int64(len(data)))

	for i := 0; i < b.N; i++ {
		_, _, _ = encryptor.Encrypt(key, data)
	}
}

func BenchmarkDecryption_AES256GCM(b *testing.B) {
	encryptor := NewAESGCMEncryptor()
	key := make([]byte, 32)
	_, _ = rand.Read(key)
	data := make([]byte, 1024*1024) // 1MB
	_, _ = rand.Read(data)
	ciphertext, nonce, _ := encryptor.Encrypt(key, data)

	b.ResetTimer()
	b.SetBytes(int64(len(data)))

	for i := 0; i < b.N; i++ {
		_, _ = encryptor.Decrypt(key, nonce, ciphertext)
	}
}

func BenchmarkChunking_FastCDC(b *testing.B) {
	config, _ := GetPreset("smart")
	chunker, _ := NewChunkerFromConfig(config)

	data := make([]byte, 10*1024*1024) // 10MB
	_, _ = rand.Read(data)

	b.ResetTimer()
	b.SetBytes(int64(len(data)))

	for i := 0; i < b.N; i++ {
		_, _ = chunker.ChunkBytes(data)
	}
}

func BenchmarkKeyDerivation_HKDF(b *testing.B) {
	masterKey, _ := GenerateMasterKey()
	keyManager, _ := NewKeyManager(&KeyManagerConfig{
		MasterKey:     masterKey,
		EnableCaching: false, // Disable cache to measure actual derivation
	})

	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		_, _ = keyManager.DeriveTenantKey(fmt.Sprintf("tenant-%d", i%100), 1)
	}
}

func BenchmarkKeyDerivation_Cached(b *testing.B) {
	masterKey, _ := GenerateMasterKey()
	keyManager, _ := NewKeyManager(&KeyManagerConfig{
		MasterKey:     masterKey,
		EnableCaching: true,
	})

	// Warm cache
	for i := 0; i < 100; i++ {
		_, _ = keyManager.DeriveTenantKey(fmt.Sprintf("tenant-%d", i), 1)
	}

	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		_, _ = keyManager.DeriveTenantKey(fmt.Sprintf("tenant-%d", i%100), 1)
	}
}

func BenchmarkMLKEM_KeyGen(b *testing.B) {
	for i := 0; i < b.N; i++ {
		_, _ = GenerateMLKEMKeyPair()
	}
}

func BenchmarkMLKEM_Encapsulate(b *testing.B) {
	keyPair, _ := GenerateMLKEMKeyPair()

	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		_, _ = Encapsulate(keyPair.PublicKey)
	}
}

func BenchmarkMLKEM_Decapsulate(b *testing.B) {
	keyPair, _ := GenerateMLKEMKeyPair()
	encap, _ := Encapsulate(keyPair.PublicKey)

	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		_, _ = Decapsulate(keyPair.PrivateKey, encap.Ciphertext)
	}
}
