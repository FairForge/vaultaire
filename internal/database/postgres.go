package database

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	_ "github.com/lib/pq" // PostgreSQL driver
	"go.uber.org/zap"
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
	db     *sql.DB
	logger *zap.Logger
}

// Tenant represents a tenant in the system
type Tenant struct {
	ID        string
	Name      string
	CreatedAt time.Time
	Email     string
	AccessKey string
	SecretKey string
}

// NewPostgres creates a new PostgreSQL connection
func NewPostgres(cfg Config, logger *zap.Logger) (*Postgres, error) {
	// Never an empty password field; always a connect timeout (Config.DSN).
	dsn := cfg.DSN()

	db, err := sql.Open("postgres", dsn)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}

	db.SetMaxOpenConns(50)
	db.SetMaxIdleConns(25)
	db.SetConnMaxLifetime(5 * time.Minute)
	db.SetConnMaxIdleTime(time.Minute)

	return &Postgres{
		db:     db,
		logger: logger,
	}, nil
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
func (p *Postgres) CreateTenant(ctx context.Context, tenant *Tenant) error {
	query := `INSERT INTO tenants (id, name, email, access_key, secret_key, created_at) VALUES ($1, $2, $3, $4, $5, $6)`
	_, err := p.db.ExecContext(ctx, query, tenant.ID, tenant.Name, tenant.Email, tenant.AccessKey, tenant.SecretKey, tenant.CreatedAt)
	if err != nil {
		return fmt.Errorf("insert tenant: %w", err)
	}
	return nil
}

// GetTenant retrieves a tenant by ID
func (p *Postgres) GetTenant(ctx context.Context, id string) (*Tenant, error) {
	query := `SELECT id, name, email, created_at FROM tenants WHERE id = $1`

	var tenant Tenant
	err := p.db.QueryRowContext(ctx, query, id).Scan(
		&tenant.ID,
		&tenant.Name,
		&tenant.Email,
		&tenant.CreatedAt,
	)

	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("tenant not found")
	}
	if err != nil {
		return nil, fmt.Errorf("query tenant: %w", err)
	}

	return &tenant, nil
}

func (p *Postgres) DB() *sql.DB {
	return p.db
}

// Exec executes a query without returning any rows

// ConnectTimeoutSeconds bounds a new connection's whole establishment (dial,
// TLS, startup, auth). lib/pq ignores the context during the startup
// handshake, so a Postgres that accepts TCP and never answers hung every new
// connection without bound (Prompt 2b 0.4).
const ConnectTimeoutSeconds = 5

// DSN is the lib/pq key=value DSN of cfg: sslmode defaults to disable, the
// password field is omitted when empty, connect_timeout is always set.
func (cfg Config) DSN() string {
	sslMode := cfg.SSLMode
	if sslMode == "" {
		sslMode = "disable"
	}
	if cfg.Password != "" {
		return fmt.Sprintf("host=%s port=%d user=%s password=%s dbname=%s sslmode=%s connect_timeout=%d",
			cfg.Host, cfg.Port, cfg.User, cfg.Password, cfg.Database, sslMode, ConnectTimeoutSeconds)
	}
	return fmt.Sprintf("host=%s port=%d user=%s dbname=%s sslmode=%s connect_timeout=%d",
		cfg.Host, cfg.Port, cfg.User, cfg.Database, sslMode, ConnectTimeoutSeconds)
}
