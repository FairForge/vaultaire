//go:build !prod
// +build !prod

package database

// TestConfig returns config for local testing
func TestConfig() Config {
	return Config{
		Host:     "localhost",
		Port:     5432,
		Database: "vaultaire",
		User:     "viera", // Your actual PostgreSQL user
		Password: "",
		SSLMode:  "disable",
	}
}
