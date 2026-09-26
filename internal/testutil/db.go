// Package testutil holds cross-package test configuration. It is imported only
// from _test.go files and is never linked into cmd/vaultaire.
//
// Database resolution order (both DSN and DBConfig):
//  1. DATABASE_URL — what CI sets; used verbatim.
//  2. TEST_DB_HOST / TEST_DB_PORT / TEST_DB_NAME / TEST_DB_USER / TEST_DB_PASSWORD.
//  3. Defaults: localhost:5432, user viera, database **vaultaire_test**.
//
// The default is deliberately NOT the shared dev database `vaultaire`: several
// tests drop and recreate tables. Create the test database with `make test-db`.
package testutil

import (
	"fmt"
	"net/url"
	"os"
	"strconv"

	"github.com/FairForge/vaultaire/internal/database"
)

// DefaultTestDB is the database tests use when nothing else is configured.
const DefaultTestDB = "vaultaire_test"

// DBConfig returns the PostgreSQL configuration used by tests that need a real
// database.
func DBConfig() database.Config {
	if raw := os.Getenv("DATABASE_URL"); raw != "" {
		if cfg, err := parseURL(raw); err == nil {
			return cfg
		}
	}
	port, _ := strconv.Atoi(getEnv("TEST_DB_PORT", "5432"))
	if port == 0 {
		port = 5432
	}
	return database.Config{
		Host:     getEnv("TEST_DB_HOST", "localhost"),
		Port:     port,
		Database: getEnv("TEST_DB_NAME", DefaultTestDB),
		User:     getEnv("TEST_DB_USER", "viera"),
		Password: getEnv("TEST_DB_PASSWORD", ""),
		SSLMode:  getEnv("TEST_DB_SSLMODE", "disable"),
	}
}

// DSN renders the test database connection string for lib/pq. DATABASE_URL is
// returned verbatim when set; otherwise a key=value DSN is built from DBConfig.
// The password field is omitted entirely when empty (an empty `password=`
// breaks some pq setups).
func DSN() string {
	if raw := os.Getenv("DATABASE_URL"); raw != "" {
		return raw
	}
	cfg := DBConfig()
	if cfg.Password != "" {
		return fmt.Sprintf("host=%s port=%d user=%s password=%s dbname=%s sslmode=%s",
			cfg.Host, cfg.Port, cfg.User, cfg.Password, cfg.Database, cfg.SSLMode)
	}
	return fmt.Sprintf("host=%s port=%d user=%s dbname=%s sslmode=%s",
		cfg.Host, cfg.Port, cfg.User, cfg.Database, cfg.SSLMode)
}

// parseURL converts postgres://user:pass@host:port/db?sslmode=x into a Config.
func parseURL(raw string) (database.Config, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return database.Config{}, fmt.Errorf("parse DATABASE_URL: %w", err)
	}
	if u.Scheme != "postgres" && u.Scheme != "postgresql" {
		return database.Config{}, fmt.Errorf("parse DATABASE_URL: unsupported scheme %q", u.Scheme)
	}
	port := 5432
	if p := u.Port(); p != "" {
		if port, err = strconv.Atoi(p); err != nil {
			return database.Config{}, fmt.Errorf("parse DATABASE_URL port %q: %w", p, err)
		}
	}
	cfg := database.Config{
		Host:     u.Hostname(),
		Port:     port,
		Database: u.Path[1:], // strip leading '/'
		SSLMode:  u.Query().Get("sslmode"),
	}
	if cfg.Host == "" {
		cfg.Host = "localhost"
	}
	if cfg.SSLMode == "" {
		cfg.SSLMode = "disable"
	}
	if u.User != nil {
		cfg.User = u.User.Username()
		cfg.Password, _ = u.User.Password()
	}
	return cfg, nil
}

func getEnv(key, defaultVal string) string {
	if val := os.Getenv(key); val != "" {
		return val
	}
	return defaultVal
}
