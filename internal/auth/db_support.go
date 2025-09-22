package auth

import (
	"context"
	"database/sql"
	"fmt"
)

// InitWithDB initializes auth service with optional database
func (a *AuthService) InitWithDB(db *sql.DB) {
	if db != nil {
		a.db = NewPostgresDB(db)
	}
}

// PersistToDB saves current in-memory data to database
func (a *AuthService) PersistToDB(ctx context.Context) error {
	pgDB, ok := a.db.(*PostgresDB)
	if !ok || pgDB == nil {
		return nil // No database, skip
	}

	// Save all users
	for _, user := range a.users {
		if err := pgDB.SaveUser(ctx, user); err != nil {
			return fmt.Errorf("save user %s: %w", user.Email, err)
		}
	}

	// Save all tenants
	for _, tenant := range a.tenants {
		if err := pgDB.SaveTenant(ctx, tenant.ID, tenant.AccessKey, tenant.CreatedAt); err != nil {
			return fmt.Errorf("save tenant %s: %w", tenant.ID, err)
		}
	}

	// Save all API keys
	for _, key := range a.apiKeys {
		if err := pgDB.SaveAPIKey(ctx, key); err != nil {
			return fmt.Errorf("save api key %s: %w", key.Key, err)
		}
	}

	return nil
}
