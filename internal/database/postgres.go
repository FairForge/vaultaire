package database

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	_ "github.com/lib/pq" // PostgreSQL driver
)

// Config holds database configuration
type Config struct {
	Host     string
	Port     int
	Database string
	User     string
	Password string
	SSLMode  string
}

// Postgres represents a PostgreSQL connection
type Postgres struct {
	db *sql.DB
}

// Tenant represents a tenant in the system
type Tenant struct {
	ID        string
	Name      string
	CreatedAt time.Time
	UpdatedAt time.Time
}

// NewPostgres creates a new PostgreSQL connection
func NewPostgres(cfg Config) (*Postgres, error) {
	if cfg.SSLMode == "" {
		cfg.SSLMode = "disable"
	}

	dsn := fmt.Sprintf("host=%s port=%d user=%s password=%s dbname=%s sslmode=%s",
		cfg.Host, cfg.Port, cfg.User, cfg.Password, cfg.Database, cfg.SSLMode)

	db, err := sql.Open("postgres", dsn)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}

	// Set connection pool settings
	db.SetMaxOpenConns(25)
	db.SetMaxIdleConns(5)
	db.SetConnMaxLifetime(5 * time.Minute)

	return &Postgres{db: db}, nil
}

// Close closes the database connection
func (p *Postgres) Close() error {
	return p.db.Close()
}

// Ping verifies the database connection
func (p *Postgres) Ping(ctx context.Context) error {
	return p.db.PingContext(ctx)
}

// CreateTables creates the necessary database tables
func (p *Postgres) CreateTables(ctx context.Context) error {
	queries := []string{
		`CREATE TABLE IF NOT EXISTS tenants (
			id VARCHAR(255) PRIMARY KEY,
			name VARCHAR(255) NOT NULL,
			created_at TIMESTAMP NOT NULL DEFAULT NOW(),
			updated_at TIMESTAMP NOT NULL DEFAULT NOW()
		)`,
		`CREATE TABLE IF NOT EXISTS artifacts (
			id SERIAL PRIMARY KEY,
			tenant_id VARCHAR(255) NOT NULL,
			container VARCHAR(255) NOT NULL,
			name VARCHAR(255) NOT NULL,
			size BIGINT NOT NULL,
			content_type VARCHAR(255),
			etag VARCHAR(255),
			created_at TIMESTAMP NOT NULL DEFAULT NOW(),
			updated_at TIMESTAMP NOT NULL DEFAULT NOW(),
			FOREIGN KEY (tenant_id) REFERENCES tenants(id),
			UNIQUE(tenant_id, container, name)
		)`,
	}

	for _, query := range queries {
		if _, err := p.db.ExecContext(ctx, query); err != nil {
			return fmt.Errorf("create table: %w", err)
		}
	}

	return nil
}

// CreateTenant creates a new tenant
func (p *Postgres) CreateTenant(ctx context.Context, tenant *Tenant) error {
	query := `INSERT INTO tenants (id, name, created_at) VALUES ($1, $2, $3)`
	_, err := p.db.ExecContext(ctx, query, tenant.ID, tenant.Name, tenant.CreatedAt)
	if err != nil {
		return fmt.Errorf("insert tenant: %w", err)
	}
	return nil
}

// GetTenant retrieves a tenant by ID
func (p *Postgres) GetTenant(ctx context.Context, id string) (*Tenant, error) {
	query := `SELECT id, name, created_at, updated_at FROM tenants WHERE id = $1`

	var tenant Tenant
	err := p.db.QueryRowContext(ctx, query, id).Scan(
		&tenant.ID,
		&tenant.Name,
		&tenant.CreatedAt,
		&tenant.UpdatedAt,
	)

	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("tenant not found")
	}
	if err != nil {
		return nil, fmt.Errorf("query tenant: %w", err)
	}

	return &tenant, nil
}

// Artifact represents stored artifact metadata
type Artifact struct {
	ID          int64
	TenantID    string
	Container   string
	Name        string
	Size        int64
	ContentType string
	ETag        string
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// CreateArtifact stores artifact metadata
func (p *Postgres) CreateArtifact(ctx context.Context, artifact *Artifact) error {
	query := `
		INSERT INTO artifacts (tenant_id, container, name, size, content_type, etag)
		VALUES ($1, $2, $3, $4, $5, $6)
		RETURNING id, created_at, updated_at`

	err := p.db.QueryRowContext(ctx, query,
		artifact.TenantID,
		artifact.Container,
		artifact.Name,
		artifact.Size,
		artifact.ContentType,
		artifact.ETag,
	).Scan(&artifact.ID, &artifact.CreatedAt, &artifact.UpdatedAt)

	if err != nil {
		return fmt.Errorf("insert artifact: %w", err)
	}
	return nil
}

// GetArtifact retrieves artifact metadata
func (p *Postgres) GetArtifact(ctx context.Context, tenantID, container, name string) (*Artifact, error) {
	query := `
		SELECT id, tenant_id, container, name, size, content_type, etag, created_at, updated_at
		FROM artifacts
		WHERE tenant_id = $1 AND container = $2 AND name = $3`

	var artifact Artifact
	err := p.db.QueryRowContext(ctx, query, tenantID, container, name).Scan(
		&artifact.ID,
		&artifact.TenantID,
		&artifact.Container,
		&artifact.Name,
		&artifact.Size,
		&artifact.ContentType,
		&artifact.ETag,
		&artifact.CreatedAt,
		&artifact.UpdatedAt,
	)

	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("artifact not found")
	}
	if err != nil {
		return nil, fmt.Errorf("query artifact: %w", err)
	}

	return &artifact, nil
}

// ListArtifacts lists artifacts in a container
func (p *Postgres) ListArtifacts(ctx context.Context, tenantID, container string, limit int) ([]*Artifact, error) {
	query := `
		SELECT id, tenant_id, container, name, size, content_type, etag, created_at, updated_at
		FROM artifacts
		WHERE tenant_id = $1 AND container = $2
		ORDER BY name
		LIMIT $3`

	rows, err := p.db.QueryContext(ctx, query, tenantID, container, limit)
	if err != nil {
		return nil, fmt.Errorf("query artifacts: %w", err)
	}
	defer rows.Close()

	var artifacts []*Artifact
	for rows.Next() {
		var a Artifact
		err := rows.Scan(
			&a.ID,
			&a.TenantID,
			&a.Container,
			&a.Name,
			&a.Size,
			&a.ContentType,
			&a.ETag,
			&a.CreatedAt,
			&a.UpdatedAt,
		)
		if err != nil {
			return nil, fmt.Errorf("scan artifact: %w", err)
		}
		artifacts = append(artifacts, &a)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("rows error: %w", err)
	}

	return artifacts, nil
}

// DeleteArtifact removes artifact metadata
func (p *Postgres) DeleteArtifact(ctx context.Context, tenantID, container, name string) error {
	query := `DELETE FROM artifacts WHERE tenant_id = $1 AND container = $2 AND name = $3`

	result, err := p.db.ExecContext(ctx, query, tenantID, container, name)
	if err != nil {
		return fmt.Errorf("delete artifact: %w", err)
	}

	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("rows affected: %w", err)
	}

	if rows == 0 {
		return fmt.Errorf("artifact not found")
	}

	return nil
}
