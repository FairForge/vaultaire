package crypto

import (
	"testing"
)

func TestPipelineConfigValidate(t *testing.T) {
	tests := []struct {
		name    string
		config  PipelineConfig
		wantErr bool
	}{
		{
			name:    "passthrough mode valid",
			config:  ConfigPassthrough,
			wantErr: false,
		},
		{
			name:    "smart storage preset valid",
			config:  ConfigSmartStorage,
			wantErr: false,
		},
		{
			name:    "archive preset valid",
			config:  ConfigArchive,
			wantErr: false,
		},
		{
			name:    "hpc preset valid",
			config:  ConfigHPC,
			wantErr: false,
		},
		{
			name:    "enterprise preset valid",
			config:  ConfigEnterprise,
			wantErr: false,
		},
		{
			name: "chunking without algo",
			config: PipelineConfig{
				ChunkingEnabled: true,
				ChunkMinSize:    1024,
				ChunkAvgSize:    4096,
				ChunkMaxSize:    8192,
				ChunkingAlgo:    ChunkingNone,
			},
			wantErr: true,
		},
		{
			name: "invalid chunk sizes",
			config: PipelineConfig{
				ChunkingEnabled: true,
				ChunkingAlgo:    ChunkingFastCDC,
				ChunkMinSize:    8192, // min > avg
				ChunkAvgSize:    4096,
				ChunkMaxSize:    16384,
			},
			wantErr: true,
		},
		{
			name: "dedup without chunking",
			config: PipelineConfig{
				DedupEnabled: true,
			},
			wantErr: true,
		},
		{
			name: "compression without algo",
			config: PipelineConfig{
				CompressionEnabled: true,
				CompressionAlgo:    CompressionNone,
			},
			wantErr: true,
		},
		{
			name: "invalid zstd level",
			config: PipelineConfig{
				CompressionEnabled: true,
				CompressionAlgo:    CompressionZstd,
				CompressionLevel:   25, // Invalid
			},
			wantErr: true,
		},
		{
			name: "encryption without algo",
			config: PipelineConfig{
				EncryptionEnabled: true,
				EncryptionMode:    EncryptionModeRandom,
			},
			wantErr: true,
		},
		{
			name: "encryption without mode",
			config: PipelineConfig{
				EncryptionEnabled: true,
				EncryptionAlgo:    EncryptionAESGCM,
			},
			wantErr: true,
		},
		{
			name: "convergent encryption without chunking",
			config: PipelineConfig{
				DedupCrossTenant:  true,
				EncryptionEnabled: true,
				EncryptionAlgo:    EncryptionAESGCM,
				EncryptionMode:    EncryptionModeRandom, // Should be convergent
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.config.Validate()
			if (err != nil) != tt.wantErr {
				t.Errorf("Validate() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestGetPreset(t *testing.T) {
	tests := []struct {
		name    string
		wantErr bool
	}{
		{"smart", false},
		{"default", false},
		{"archive", false},
		{"cold", false},
		{"hpc", false},
		{"performance", false},
		{"fast", false},
		{"passthrough", false},
		{"none", false},
		{"enterprise", false},
		{"compliance", false},
		{"pq", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			config, err := GetPreset(tt.name)
			if (err != nil) != tt.wantErr {
				t.Errorf("GetPreset(%s) error = %v, wantErr %v", tt.name, err, tt.wantErr)
			}
			if err == nil {
				if err := config.Validate(); err != nil {
					t.Errorf("GetPreset(%s) returned invalid config: %v", tt.name, err)
				}
			}
		})
	}
}

func TestConfigPresetValues(t *testing.T) {
	// Verify smart preset has expected values
	if ConfigSmartStorage.ChunkAvgSize != 4*1024*1024 {
		t.Errorf("ConfigSmartStorage.ChunkAvgSize = %d, want %d",
			ConfigSmartStorage.ChunkAvgSize, 4*1024*1024)
	}

	// Verify archive has cross-tenant dedup
	if !ConfigArchive.DedupCrossTenant {
		t.Error("ConfigArchive should have cross-tenant dedup enabled")
	}

	// Verify enterprise has post-quantum flag
	if !ConfigEnterprise.PostQuantumReady {
		t.Error("ConfigEnterprise should have PostQuantumReady flag")
	}

	// Verify passthrough has everything disabled
	if ConfigPassthrough.ChunkingEnabled || ConfigPassthrough.CompressionEnabled ||
		ConfigPassthrough.EncryptionEnabled || ConfigPassthrough.DedupEnabled {
		t.Error("ConfigPassthrough should have all features disabled")
	}
}
