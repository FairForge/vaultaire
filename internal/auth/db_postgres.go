package auth

import (
	"context"
	"database/sql"
	"time"
)

// PostgresDB implements the Database interface
type PostgresDB struct {
	db *sql.DB
}

// NewPostgresDB creates a new PostgreSQL database adapter
func NewPostgresDB(db *sql.DB) Database {
	return &PostgresDB{db: db}
}

// Implement the Database interface methods
func (p *PostgresDB) SaveUser(ctx context.Context, user *User) error {
	query := `
		INSERT INTO users (id, email, password_hash, company, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (email) DO UPDATE SET
			password_hash = EXCLUDED.password_hash,
			updated_at = EXCLUDED.updated_at
	`
	_, err := p.db.ExecContext(ctx, query,
		user.ID, user.Email, user.PasswordHash, user.Company,
		user.CreatedAt, user.UpdatedAt,
	)
	return err
}

func (p *PostgresDB) SaveTenant(ctx context.Context, tenantID, userEmail string, createdAt time.Time) error {
	query := `
		INSERT INTO tenants (id, name, created_at, email)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (id) DO NOTHING
	`
	_, err := p.db.ExecContext(ctx, query, tenantID, userEmail, createdAt, userEmail)
	return err
}

func (p *PostgresDB) SaveAPIKey(ctx context.Context, key *APIKey) error {
	query := `
		INSERT INTO api_keys (id, user_id, tenant_id, name, key_value, secret_hash, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		ON CONFLICT (key_value) DO NOTHING
	`
	_, err := p.db.ExecContext(ctx, query,
		key.ID, key.UserID, key.TenantID, key.Name,
		key.Key, key.Hash, key.CreatedAt,
	)
	return err
}
