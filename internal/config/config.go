package config

import (
    "time"
)

type Config struct {
    Server   ServerConfig   `yaml:"server"`
    Engine   EngineConfig   `yaml:"engine"`
    Pipeline PipelineConfig `yaml:"pipeline"`
    Events   EventConfig    `yaml:"events"`
    Cache    CacheConfig    `yaml:"cache"`
    Backends map[string]BackendConfig `yaml:"backends"`
}

type ServerConfig struct {
    Port        int    `yaml:"port" default:"8080"`
    MetricsPort int    `yaml:"metrics_port" default:"9090"`
    LogLevel    string `yaml:"log_level" default:"info"`
}

type EngineConfig struct {
    MaxOperations int  `yaml:"max_operations" default:"1000"`
    EnableQuery   bool `yaml:"enable_query" default:"false"`
    EnableCompute bool `yaml:"enable_compute" default:"false"`
}

type PipelineConfig struct {
    Stages []string `yaml:"stages"` // ["compress", "encrypt"] for MVP
}

type EventConfig struct {
    Enabled       bool          `yaml:"enabled" default:"true"`
    BufferSize    int           `yaml:"buffer_size" default:"10000"`
    FlushInterval time.Duration `yaml:"flush_interval" default:"1m"`
}

type CacheConfig struct {
    MemorySize int64  `yaml:"memory_size"` // 256GB RAM on your hub
    SSDPath    string `yaml:"ssd_path"`    // "/mnt/nvme/cache"
    SSDSize    int64  `yaml:"ssd_size"`    // 8TB NVMe on your hub
    Algorithm  string `yaml:"algorithm" default:"lru"`
}

type BackendConfig struct {
    Type     string                 `yaml:"type"`
    Endpoint string                 `yaml:"endpoint"`
    Options  map[string]interface{} `yaml:"options"`
}
