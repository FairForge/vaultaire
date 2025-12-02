package crypto

import (
	"bytes"
	"context"
	"crypto/rand"
	"fmt"
	"testing"
	"time"
)

// Benchmarks for crypto pipeline performance

func BenchmarkPipeline_SmallFile(b *testing.B) {
	benchmarkPipeline(b, 1024, "smart") // 1KB
}

func BenchmarkPipeline_MediumFile(b *testing.B) {
	benchmarkPipeline(b, 1024*1024, "smart") // 1MB
}

func BenchmarkPipeline_LargeFile(b *testing.B) {
	benchmarkPipeline(b, 10*1024*1024, "smart") // 10MB
}

func BenchmarkPipeline_Archive(b *testing.B) {
	benchmarkPipeline(b, 1024*1024, "archive") // 1MB with archive preset
}

func BenchmarkPipeline_HPC(b *testing.B) {
	benchmarkPipeline(b, 1024*1024, "hpc") // 1MB with HPC preset
}

func benchmarkPipeline(b *testing.B, size int, preset string) {
	masterKey, _ := GenerateMasterKey()
	keyManager, _ := NewKeyManager(&KeyManagerConfig{MasterKey: masterKey})
	pipeline, _ := NewPipelineFromPreset(preset)

	pb, _ := NewProcessingBackend(&ProcessingBackendConfig{
		Pipeline:   pipeline,
		KeyManager: keyManager,
	})

	ctx := context.Background()
	data := make([]byte, size)
	_, _ = rand.Read(data)

	b.ResetTimer()
	b.SetBytes(int64(size))

	for i := 0; i < b.N; i++ {
		_, _ = pb.ProcessForUpload(ctx, "bench-tenant", "file.bin", bytes.NewReader(data))
	}
}

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

func BenchmarkRoundTrip_1MB(b *testing.B) {
	masterKey, _ := GenerateMasterKey()
	keyManager, _ := NewKeyManager(&KeyManagerConfig{MasterKey: masterKey})
	pipeline, _ := NewPipelineFromPreset("smart")
	fetcher := NewSimpleChunkFetcher()

	pb, _ := NewProcessingBackend(&ProcessingBackendConfig{
		Pipeline:   pipeline,
		KeyManager: keyManager,
	})

	ctx := context.Background()
	data := make([]byte, 1024*1024) // 1MB
	_, _ = rand.Read(data)

	b.ResetTimer()
	b.SetBytes(int64(len(data)) * 2) // Count both upload and download

	for i := 0; i < b.N; i++ {
		// Upload
		result, _ := pb.ProcessForUpload(ctx, "bench", "file", bytes.NewReader(data))
		for j, chunk := range result.Chunks {
			if chunk.IsNew {
				fetcher.Store(result.Metadata.ChunkRefs[j].Location, chunk.Data)
			}
		}

		// Download
		_, _ = pb.ProcessForDownload(ctx, "bench", &result.Metadata, fetcher)
	}
}

// Throughput test to estimate 10Gbps capability
func TestThroughput_Target10Gbps(t *testing.T) {
	masterKey, _ := GenerateMasterKey()
	keyManager, _ := NewKeyManager(&KeyManagerConfig{MasterKey: masterKey})
	pipeline, _ := NewPipelineFromPreset("hpc") // HPC preset for max throughput

	pb, _ := NewProcessingBackend(&ProcessingBackendConfig{
		Pipeline:   pipeline,
		KeyManager: keyManager,
	})

	ctx := context.Background()

	// 100MB test file
	size := 100 * 1024 * 1024
	data := make([]byte, size)
	_, _ = rand.Read(data)

	// Measure upload throughput
	iterations := 3
	var totalDuration time.Duration

	for i := 0; i < iterations; i++ {
		start := time.Now()
		_, err := pb.ProcessForUpload(ctx, "throughput-test", "file.bin", bytes.NewReader(data))
		if err != nil {
			t.Fatalf("Upload failed: %v", err)
		}
		totalDuration += time.Since(start)
	}

	avgDuration := totalDuration / time.Duration(iterations)
	throughputMBps := float64(size) / avgDuration.Seconds() / (1024 * 1024)
	throughputGbps := throughputMBps * 8 / 1000

	t.Logf("Throughput: %.2f MB/s = %.2f Gbps", throughputMBps, throughputGbps)

	if throughputGbps < 1.0 {
		t.Logf("Warning: Throughput below 1 Gbps target")
	}
}
