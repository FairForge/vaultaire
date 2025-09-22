package database

import (
	"os"
)

// GetTestConfig returns database config for testing
func GetTestConfig() Config {
	return Config{
		Host:     getEnv("TEST_DB_HOST", "localhost"),
		Port:     5432,
		Database: getEnv("TEST_DB_NAME", "vaultaire"),
		User:     getEnv("TEST_DB_USER", "viera"),
		Password: getEnv("TEST_DB_PASSWORD", ""),
		SSLMode:  "disable",
	}
}

func getEnv(key, defaultVal string) string {
	if val := os.Getenv(key); val != "" {
		return val
	}
	return defaultVal
}
