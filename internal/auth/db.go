package auth

import (
	"context"
	"database/sql"
	"fmt"

	_ "github.com/lib/pq"
)

type DBAuthService struct {
	*AuthService
	db *sql.DB
}

func NewDBAuthService(db *sql.DB) *DBAuthService {
	return &DBAuthService{
		AuthService: NewAuthService(nil),
		db:          db,
	}
}

func (a *DBAuthService) CreateUser(ctx context.Context, email, password string) (*User, error) {
	// Use parent to validate and hash
	user, err := a.AuthService.CreateUser(ctx, email, password)
	if err != nil {
		return nil, err
	}

	// Save to database
	query := `
        INSERT INTO users (id, email, password_hash, created_at, updated_at)
        VALUES ($1, $2, $3, NOW(), NOW())
        ON CONFLICT (email) DO NOTHING
        RETURNING id
    `

	err = a.db.QueryRowContext(ctx, query, user.ID, user.Email, user.PasswordHash).Scan(&user.ID)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("user already exists")
	}
	if err != nil {
		return nil, fmt.Errorf("create user: %w", err)
	}

	return user, nil
}

func (a *DBAuthService) SaveAPIKey(ctx context.Context, apiKey *APIKey) error {
	query := `
        INSERT INTO api_keys (id, user_id, name, key_id, secret_hash, created_at)
        VALUES ($1, $2, $3, $4, $5, NOW())
    `

	_, err := a.db.ExecContext(ctx, query,
		apiKey.ID, apiKey.UserID, apiKey.Name, apiKey.Key, apiKey.Hash)

	if err != nil {
		return fmt.Errorf("save api key: %w", err)
	}

	// Also store in memory for quick access
	a.apiKeys[apiKey.Key] = apiKey

	return nil
}

func (a *DBAuthService) GenerateAPIKey(ctx context.Context, userID, name string) (*APIKey, error) {
	// Generate using parent method
	apiKey, err := a.AuthService.GenerateAPIKey(ctx, userID, name)
	if err != nil {
		return nil, err
	}

	// Save to database
	if err := a.SaveAPIKey(ctx, apiKey); err != nil {
		return nil, err
	}

	return apiKey, nil
}
