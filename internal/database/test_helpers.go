package database

import "fmt"

// GetTestDSN returns the PostgreSQL connection string for testing
func GetTestDSN() string {
	// Get config with correct defaults
	config := GetTestConfig()

	// Build DSN - NEVER include empty password field
	if config.Password != "" {
		dsn := fmt.Sprintf("host=%s port=%d user=%s password=%s dbname=%s sslmode=%s",
			config.Host, config.Port, config.User, config.Password, config.Database, config.SSLMode)
		return dsn
	}

	// No password - omit the field entirely
	dsn := fmt.Sprintf("host=%s port=%d user=%s dbname=%s sslmode=%s",
		config.Host, config.Port, config.User, config.Database, config.SSLMode)
	return dsn
}
