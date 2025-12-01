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
				MinChunkSize:    1024,
				AvgChunkSize:    4096,
				MaxChunkSize:    8192,
				ChunkingAlgo:    ChunkingNone,
			},
			wantErr: true,
		},
		{
			name: "invalid chunk sizes",
			config: PipelineConfig{
				ChunkingEnabled: true,
				ChunkingAlgo:    ChunkingFastCDC,
				MinChunkSize:    8192, // min > avg
				AvgChunkSize:    4096,
				MaxChunkSize:    16384,
			},
			wantErr: true,
		},
		{
			name: "dedup without chunking",
			config: PipelineConfig{
				DedupEnabled: true,
				DedupScope:   DedupScopeTenant,
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
				CompressionLevel:   25, // max is 19
			},
			wantErr: true,
		},
		{
			name: "encryption without algo",
			config: PipelineConfig{
				EncryptionEnabled: true,
				EncryptionAlgo:    EncryptionNone,
				EncryptionMode:    EncryptionModeStandard,
			},
			wantErr: true,
		},
		{
			name: "encryption without mode",
			config: PipelineConfig{
				EncryptionEnabled: true,
				EncryptionAlgo:    EncryptionAES256GCM,
			},
			wantErr: true,
		},
		{
			name: "convergent encryption without chunking",
			config: PipelineConfig{
				EncryptionEnabled: true,
				EncryptionAlgo:    EncryptionAES256GCM,
				EncryptionMode:    EncryptionModeConvergent,
				ChunkingEnabled:   false,
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
	presets := []string{"smart", "default", "archive", "cold", "hpc", "performance", "fast", "passthrough", "none", "enterprise", "compliance", "pq"}

	for _, name := range presets {
		t.Run(name, func(t *testing.T) {
			config, err := GetPreset(name)
			if err != nil {
				t.Errorf("GetPreset(%s) error = %v", name, err)
				return
			}
			if err := config.Validate(); err != nil {
				t.Errorf("GetPreset(%s) returned invalid config: %v", name, err)
			}
		})
	}

	// Test unknown preset
	_, err := GetPreset("unknown")
	if err == nil {
		t.Error("GetPreset(unknown) should return error")
	}
}

func TestConfigPresetValues(t *testing.T) {
	// Verify smart storage config
	if !ConfigSmartStorage.ChunkingEnabled {
		t.Error("ConfigSmartStorage should have chunking enabled")
	}
	if ConfigSmartStorage.AvgChunkSize != 4*1024*1024 {
		t.Errorf("ConfigSmartStorage avg chunk size = %d, want 4MB", ConfigSmartStorage.AvgChunkSize)
	}
	if ConfigSmartStorage.EncryptionMode != EncryptionModeConvergent {
		t.Error("ConfigSmartStorage should use convergent encryption")
	}

	// Verify HPC config
	if ConfigHPC.ChunkingEnabled {
		t.Error("ConfigHPC should have chunking disabled")
	}
	if ConfigHPC.CompressionEnabled {
		t.Error("ConfigHPC should have compression disabled")
	}
	if !ConfigHPC.EncryptionEnabled {
		t.Error("ConfigHPC should still have encryption enabled")
	}

	// Verify archive config
	if ConfigArchive.DedupScope != DedupScopeGlobal {
		t.Error("ConfigArchive should use global dedup scope")
	}
	if ConfigArchive.CompressionLevel != 9 {
		t.Errorf("ConfigArchive compression level = %d, want 9", ConfigArchive.CompressionLevel)
	}

	// Verify enterprise config
	if !ConfigEnterprise.PostQuantumEnabled {
		t.Error("ConfigEnterprise should have post-quantum enabled")
	}

	// Verify passthrough config
	if !ConfigPassthrough.PassthroughMode {
		t.Error("ConfigPassthrough should have passthrough mode enabled")
	}
}
