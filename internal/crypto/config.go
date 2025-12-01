// Package crypto provides the unified data processing pipeline for Vaultaire.
// It handles chunking, deduplication, compression, and encryption in a
// configurable, streaming manner that works with any protocol and backend.
package crypto

import (
	"fmt"
)

// ChunkingAlgorithm represents supported chunking algorithms
type ChunkingAlgorithm string

const (
	ChunkingNone    ChunkingAlgorithm = "none"
	ChunkingFixed   ChunkingAlgorithm = "fixed"
	ChunkingFastCDC ChunkingAlgorithm = "fastcdc"
)

// CompressionAlgorithm represents supported compression algorithms
type CompressionAlgorithm string

const (
	CompressionNone CompressionAlgorithm = "none"
	CompressionZstd CompressionAlgorithm = "zstd"
	CompressionLZ4  CompressionAlgorithm = "lz4"  // Future
	CompressionGzip CompressionAlgorithm = "gzip" // Future (S3 compat)
)

// EncryptionAlgorithm represents supported encryption algorithms
type EncryptionAlgorithm string

const (
	EncryptionNone     EncryptionAlgorithm = "none"
	EncryptionAESGCM   EncryptionAlgorithm = "aes-256-gcm"
	EncryptionChaCha   EncryptionAlgorithm = "chacha20-poly1305"
	EncryptionAESGCMPQ EncryptionAlgorithm = "aes-256-gcm-pq" // Post-quantum hybrid (future)
)

// EncryptionMode represents the encryption key derivation mode
type EncryptionMode string

const (
	EncryptionModeNone       EncryptionMode = "none"
	EncryptionModeConvergent EncryptionMode = "convergent" // Key derived from content hash (enables dedup)
	EncryptionModeRandom     EncryptionMode = "random"     // Random key per chunk (no cross-tenant dedup)
)

// PipelineConfig defines the processing pipeline for a storage class or bucket
type PipelineConfig struct {
	// Chunking settings
	ChunkingEnabled bool              `json:"chunking_enabled"`
	ChunkingAlgo    ChunkingAlgorithm `json:"chunking_algo,omitempty"`
	ChunkMinSize    int               `json:"chunk_min_size,omitempty"` // bytes
	ChunkAvgSize    int               `json:"chunk_avg_size,omitempty"` // bytes
	ChunkMaxSize    int               `json:"chunk_max_size,omitempty"` // bytes

	// Deduplication settings
	DedupEnabled     bool `json:"dedup_enabled"`
	DedupCrossTenant bool `json:"dedup_cross_tenant"` // Allow cross-tenant dedup (requires convergent encryption)

	// Compression settings
	CompressionEnabled bool                 `json:"compression_enabled"`
	CompressionAlgo    CompressionAlgorithm `json:"compression_algo,omitempty"`
	CompressionLevel   int                  `json:"compression_level,omitempty"` // Algorithm-specific level

	// Encryption settings
	EncryptionEnabled bool                `json:"encryption_enabled"`
	EncryptionAlgo    EncryptionAlgorithm `json:"encryption_algo,omitempty"`
	EncryptionMode    EncryptionMode      `json:"encryption_mode,omitempty"`

	// Future: Post-quantum readiness flag
	PostQuantumReady bool `json:"post_quantum_ready,omitempty"`
}

// Validate checks if the pipeline config is valid
func (c PipelineConfig) Validate() error {
	// Validate chunking
	if c.ChunkingEnabled {
		if c.ChunkingAlgo == "" || c.ChunkingAlgo == ChunkingNone {
			return fmt.Errorf("chunking enabled but no algorithm specified")
		}
		if c.ChunkingAlgo == ChunkingFastCDC || c.ChunkingAlgo == ChunkingFixed {
			if c.ChunkMinSize <= 0 || c.ChunkAvgSize <= 0 || c.ChunkMaxSize <= 0 {
				return fmt.Errorf("chunk sizes must be positive")
			}
			if c.ChunkMinSize > c.ChunkAvgSize || c.ChunkAvgSize > c.ChunkMaxSize {
				return fmt.Errorf("chunk sizes must be: min <= avg <= max")
			}
		}
	}

	// Validate dedup
	if c.DedupEnabled && !c.ChunkingEnabled {
		return fmt.Errorf("deduplication requires chunking to be enabled")
	}

	// Validate compression
	if c.CompressionEnabled {
		if c.CompressionAlgo == "" || c.CompressionAlgo == CompressionNone {
			return fmt.Errorf("compression enabled but no algorithm specified")
		}
		if c.CompressionAlgo == CompressionZstd {
			if c.CompressionLevel < 1 || c.CompressionLevel > 19 {
				return fmt.Errorf("zstd compression level must be 1-19")
			}
		}
	}

	// Validate encryption
	if c.EncryptionEnabled {
		if c.EncryptionAlgo == "" || c.EncryptionAlgo == EncryptionNone {
			return fmt.Errorf("encryption enabled but no algorithm specified")
		}
		if c.EncryptionMode == "" || c.EncryptionMode == EncryptionModeNone {
			return fmt.Errorf("encryption enabled but no mode specified")
		}
	}

	// Validate cross-tenant dedup requirements
	if c.DedupCrossTenant {
		if !c.EncryptionEnabled || c.EncryptionMode != EncryptionModeConvergent {
			return fmt.Errorf("cross-tenant dedup requires convergent encryption")
		}
	}

	return nil
}

// Preset configurations for common use cases

// ConfigPassthrough is a no-op pipeline (data stored as-is)
var ConfigPassthrough = PipelineConfig{
	ChunkingEnabled:    false,
	DedupEnabled:       false,
	CompressionEnabled: false,
	EncryptionEnabled:  false,
}

// ConfigSmartStorage is the default "smart" pipeline for general storage
var ConfigSmartStorage = PipelineConfig{
	ChunkingEnabled:    true,
	ChunkingAlgo:       ChunkingFastCDC,
	ChunkMinSize:       1 * 1024 * 1024,  // 1MB
	ChunkAvgSize:       4 * 1024 * 1024,  // 4MB
	ChunkMaxSize:       16 * 1024 * 1024, // 16MB
	DedupEnabled:       true,
	DedupCrossTenant:   true,
	CompressionEnabled: true,
	CompressionAlgo:    CompressionZstd,
	CompressionLevel:   3,
	EncryptionEnabled:  true,
	EncryptionAlgo:     EncryptionAESGCM,
	EncryptionMode:     EncryptionModeConvergent,
}

// ConfigArchive is optimized for cold storage (max compression)
var ConfigArchive = PipelineConfig{
	ChunkingEnabled:    true,
	ChunkingAlgo:       ChunkingFastCDC,
	ChunkMinSize:       1 * 1024 * 1024,  // 1MB
	ChunkAvgSize:       8 * 1024 * 1024,  // 8MB (larger for archive)
	ChunkMaxSize:       32 * 1024 * 1024, // 32MB
	DedupEnabled:       true,
	DedupCrossTenant:   true,
	CompressionEnabled: true,
	CompressionAlgo:    CompressionZstd,
	CompressionLevel:   9, // Higher compression for archive
	EncryptionEnabled:  true,
	EncryptionAlgo:     EncryptionAESGCM,
	EncryptionMode:     EncryptionModeConvergent,
}

// ConfigHPC is optimized for high-performance computing (minimal overhead)
var ConfigHPC = PipelineConfig{
	ChunkingEnabled:    true,
	ChunkingAlgo:       ChunkingFixed, // Fixed is faster than CDC
	ChunkMinSize:       16 * 1024 * 1024,
	ChunkAvgSize:       16 * 1024 * 1024,
	ChunkMaxSize:       16 * 1024 * 1024,
	DedupEnabled:       false, // Skip dedup for speed
	CompressionEnabled: false, // Skip compression for speed
	EncryptionEnabled:  true,
	EncryptionAlgo:     EncryptionChaCha, // Faster on CPUs without AES-NI
	EncryptionMode:     EncryptionModeRandom,
}

// ConfigEnterprise is for compliance-focused deployments
var ConfigEnterprise = PipelineConfig{
	ChunkingEnabled:    true,
	ChunkingAlgo:       ChunkingFastCDC,
	ChunkMinSize:       1 * 1024 * 1024,
	ChunkAvgSize:       4 * 1024 * 1024,
	ChunkMaxSize:       16 * 1024 * 1024,
	DedupEnabled:       true,
	DedupCrossTenant:   false, // No cross-tenant for compliance
	CompressionEnabled: true,
	CompressionAlgo:    CompressionZstd,
	CompressionLevel:   3,
	EncryptionEnabled:  true,
	EncryptionAlgo:     EncryptionAESGCM,
	EncryptionMode:     EncryptionModeRandom, // Unique keys per tenant
	PostQuantumReady:   true,                 // Flag for future PQ migration
}

// GetPreset returns a preset configuration by name
func GetPreset(name string) (PipelineConfig, error) {
	switch name {
	case "smart", "default":
		return ConfigSmartStorage, nil
	case "archive", "cold":
		return ConfigArchive, nil
	case "hpc", "performance", "fast":
		return ConfigHPC, nil
	case "passthrough", "none":
		return ConfigPassthrough, nil
	case "enterprise", "compliance":
		return ConfigEnterprise, nil
	case "pq":
		config := ConfigEnterprise
		config.PostQuantumReady = true
		return config, nil
	default:
		return PipelineConfig{}, fmt.Errorf("unknown preset: %s", name)
	}
}
