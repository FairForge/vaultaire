// Package testutil holds cross-package test configuration. It is imported only
// from _test.go files and is never linked into cmd/vaultaire.
package testutil

import (
	"fmt"
	"os"

	"github.com/FairForge/vaultaire/internal/database"
)

// DBConfig returns the PostgreSQL configuration used by tests that need a real
// database. Overridable with TEST_DB_HOST / TEST_DB_NAME / TEST_DB_USER /
// TEST_DB_PASSWORD; tests skip when the server is unreachable.
func DBConfig() database.Config {
	return database.Config{
		Host:     getEnv("TEST_DB_HOST", "localhost"),
		Port:     5432,
		Database: getEnv("TEST_DB_NAME", "vaultaire"),
		User:     getEnv("TEST_DB_USER", "viera"),
		Password: getEnv("TEST_DB_PASSWORD", ""),
		SSLMode:  "disable",
	}
}

// DSN renders DBConfig as a lib/pq connection string. The password field is
// omitted entirely when empty (an empty `password=` breaks some pq setups).
func DSN() string {
	cfg := DBConfig()
	if cfg.Password != "" {
		return fmt.Sprintf("host=%s port=%d user=%s password=%s dbname=%s sslmode=%s",
			cfg.Host, cfg.Port, cfg.User, cfg.Password, cfg.Database, cfg.SSLMode)
	}
	return fmt.Sprintf("host=%s port=%d user=%s dbname=%s sslmode=%s",
		cfg.Host, cfg.Port, cfg.User, cfg.Database, cfg.SSLMode)
}

func getEnv(key, defaultVal string) string {
	if val := os.Getenv(key); val != "" {
		return val
	}
	return defaultVal
}
