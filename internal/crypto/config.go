// Package crypto provides the unified data processing pipeline for Vaultaire.
// It handles chunking, deduplication, compression, and encryption in a
// configurable, streaming manner that works with any protocol and backend.
package crypto

import (
	"fmt"
)

// ChunkingAlgorithm defines the chunking strategy
type ChunkingAlgorithm string

const (
	ChunkingNone    ChunkingAlgorithm = "none"
	ChunkingFixed   ChunkingAlgorithm = "fixed"
	ChunkingFastCDC ChunkingAlgorithm = "fastcdc"
)

// DedupScope defines deduplication boundaries
type DedupScope string

const (
	DedupScopeNone   DedupScope = "none"
	DedupScopeBucket DedupScope = "bucket"
	DedupScopeTenant DedupScope = "tenant"
	DedupScopeGlobal DedupScope = "global"
)

// CompressionAlgo defines compression algorithms
type CompressionAlgo string

const (
	CompressionNone   CompressionAlgo = "none"
	CompressionZstd   CompressionAlgo = "zstd"
	CompressionLZ4    CompressionAlgo = "lz4"
	CompressionSnappy CompressionAlgo = "snappy"
)

// EncryptionAlgo defines encryption algorithms
type EncryptionAlgo string

const (
	EncryptionNone      EncryptionAlgo = "none"
	EncryptionAES256GCM EncryptionAlgo = "aes256gcm"
	EncryptionChaCha20  EncryptionAlgo = "chacha20poly1305"
)

// EncryptionMode defines how encryption keys are derived
type EncryptionMode string

const (
	EncryptionModeStandard   EncryptionMode = "standard"   // Random key per object
	EncryptionModeConvergent EncryptionMode = "convergent" // Key derived from content hash
)

// PipelineConfig controls data processing per bucket
type PipelineConfig struct {
	// Chunking configuration
	ChunkingEnabled bool              `json:"chunking_enabled"`
	ChunkingAlgo    ChunkingAlgorithm `json:"chunking_algo"`
	MinChunkSize    int               `json:"min_chunk_size"` // bytes
	AvgChunkSize    int               `json:"avg_chunk_size"` // bytes
	MaxChunkSize    int               `json:"max_chunk_size"` // bytes

	// Deduplication configuration
	DedupEnabled bool       `json:"dedup_enabled"`
	DedupScope   DedupScope `json:"dedup_scope"`

	// Compression configuration
	CompressionEnabled bool            `json:"compression_enabled"`
	CompressionAlgo    CompressionAlgo `json:"compression_algo"`
	CompressionLevel   int             `json:"compression_level"` // 1-19 for zstd

	// Encryption configuration
	EncryptionEnabled  bool           `json:"encryption_enabled"`
	EncryptionAlgo     EncryptionAlgo `json:"encryption_algo"`
	EncryptionMode     EncryptionMode `json:"encryption_mode"`
	PostQuantumEnabled bool           `json:"post_quantum_enabled"`

	// Performance options
	PassthroughMode bool `json:"passthrough_mode"` // Skip ALL processing
}

// Validate checks if the configuration is valid
func (c *PipelineConfig) Validate() error {
	if c.PassthroughMode {
		return nil // No validation needed for passthrough
	}

	if c.ChunkingEnabled {
		if c.MinChunkSize <= 0 {
			return fmt.Errorf("min_chunk_size must be positive")
		}
		if c.AvgChunkSize < c.MinChunkSize {
			return fmt.Errorf("avg_chunk_size must be >= min_chunk_size")
		}
		if c.MaxChunkSize < c.AvgChunkSize {
			return fmt.Errorf("max_chunk_size must be >= avg_chunk_size")
		}
		if c.ChunkingAlgo == "" || c.ChunkingAlgo == ChunkingNone {
			return fmt.Errorf("chunking_algo required when chunking is enabled")
		}
	}

	if c.DedupEnabled && !c.ChunkingEnabled {
		return fmt.Errorf("dedup requires chunking to be enabled")
	}

	if c.CompressionEnabled {
		if c.CompressionAlgo == "" || c.CompressionAlgo == CompressionNone {
			return fmt.Errorf("compression_algo required when compression is enabled")
		}
		if c.CompressionAlgo == CompressionZstd {
			if c.CompressionLevel < 1 || c.CompressionLevel > 19 {
				return fmt.Errorf("zstd compression_level must be 1-19")
			}
		}
	}

	if c.EncryptionEnabled {
		if c.EncryptionAlgo == "" || c.EncryptionAlgo == EncryptionNone {
			return fmt.Errorf("encryption_algo required when encryption is enabled")
		}
		if c.EncryptionMode == "" {
			return fmt.Errorf("encryption_mode required when encryption is enabled")
		}
	}

	if c.EncryptionMode == EncryptionModeConvergent && !c.ChunkingEnabled {
		return fmt.Errorf("convergent encryption requires chunking for content-based key derivation")
	}

	return nil
}

// Preset configurations for common use cases

// ConfigSmartStorage provides balanced processing for general use
var ConfigSmartStorage = PipelineConfig{
	ChunkingEnabled:    true,
	ChunkingAlgo:       ChunkingFastCDC,
	MinChunkSize:       1 * 1024 * 1024,  // 1MB
	AvgChunkSize:       4 * 1024 * 1024,  // 4MB
	MaxChunkSize:       16 * 1024 * 1024, // 16MB
	DedupEnabled:       true,
	DedupScope:         DedupScopeTenant,
	CompressionEnabled: true,
	CompressionAlgo:    CompressionZstd,
	CompressionLevel:   3,
	EncryptionEnabled:  true,
	EncryptionAlgo:     EncryptionAES256GCM,
	EncryptionMode:     EncryptionModeConvergent,
	PostQuantumEnabled: false,
}

// ConfigArchive provides maximum space savings for archival data
var ConfigArchive = PipelineConfig{
	ChunkingEnabled:    true,
	ChunkingAlgo:       ChunkingFastCDC,
	MinChunkSize:       2 * 1024 * 1024,  // 2MB
	AvgChunkSize:       8 * 1024 * 1024,  // 8MB
	MaxChunkSize:       32 * 1024 * 1024, // 32MB
	DedupEnabled:       true,
	DedupScope:         DedupScopeGlobal, // Cross-tenant dedup OK for cold storage
	CompressionEnabled: true,
	CompressionAlgo:    CompressionZstd,
	CompressionLevel:   9, // Higher compression
	EncryptionEnabled:  true,
	EncryptionAlgo:     EncryptionAES256GCM,
	EncryptionMode:     EncryptionModeConvergent,
	PostQuantumEnabled: false,
}

// ConfigHPC provides maximum throughput for AI/HPC workloads
var ConfigHPC = PipelineConfig{
	ChunkingEnabled:    false, // No chunking overhead
	DedupEnabled:       false, // Skip dedup checks
	CompressionEnabled: false, // Raw speed
	EncryptionEnabled:  true,  // Still encrypted
	EncryptionAlgo:     EncryptionAES256GCM,
	EncryptionMode:     EncryptionModeStandard, // Faster than convergent
	PostQuantumEnabled: false,
}

// ConfigPassthrough skips all processing (for pre-encrypted data)
var ConfigPassthrough = PipelineConfig{
	PassthroughMode: true,
}

// ConfigEnterprise provides post-quantum security for regulated data
var ConfigEnterprise = PipelineConfig{
	ChunkingEnabled:    true,
	ChunkingAlgo:       ChunkingFastCDC,
	MinChunkSize:       1 * 1024 * 1024,
	AvgChunkSize:       4 * 1024 * 1024,
	MaxChunkSize:       16 * 1024 * 1024,
	DedupEnabled:       true,
	DedupScope:         DedupScopeTenant,
	CompressionEnabled: true,
	CompressionAlgo:    CompressionZstd,
	CompressionLevel:   3,
	EncryptionEnabled:  true,
	EncryptionAlgo:     EncryptionAES256GCM,
	EncryptionMode:     EncryptionModeConvergent,
	PostQuantumEnabled: true, // ML-KEM hybrid
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
	case "enterprise", "compliance", "pq":
		return ConfigEnterprise, nil
	default:
		return PipelineConfig{}, fmt.Errorf("unknown preset: %s", name)
	}
}
