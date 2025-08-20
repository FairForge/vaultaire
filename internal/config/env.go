package config

import (
	"os"
	"strconv"
)

// LoadFromEnv loads configuration from environment variables
func LoadFromEnv(cfg *Config) {
	if port := os.Getenv("VAULTAIRE_PORT"); port != "" {
		if p, err := strconv.Atoi(port); err == nil {
			cfg.Server.Port = p
		}
	}

	if logLevel := os.Getenv("VAULTAIRE_LOG_LEVEL"); logLevel != "" {
		cfg.Server.LogLevel = logLevel
	}

	// Cache settings
	if cacheSize := os.Getenv("VAULTAIRE_CACHE_SIZE"); cacheSize != "" {
		if size, err := strconv.ParseInt(cacheSize, 10, 64); err == nil {
			cfg.Cache.MemorySize = size
		}
	}

	// Add more as needed for production
}

// GetEnvOrDefault returns environment variable or default value
func GetEnvOrDefault(key, defaultValue string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return defaultValue
}
