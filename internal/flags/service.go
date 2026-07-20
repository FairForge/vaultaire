// Package flags provides runtime feature flags backed by the feature_flags
// table (migration 059): global kill-switches and per-tenant enablement,
// flippable via the admin API / dashboard with no deploy or restart.
//
// Resolution precedence: tenant row → global row ('*') → registered in-code
// default. Unregistered keys with no row are disabled. The whole table is
// cached in memory and refreshed every ~15s (it is tiny), so the hot path
// never touches the database; Set/Unset write through and reload the cache
// immediately so an admin flip is visible on the next request.
package flags

import (
	"context"
	"database/sql"
	"errors"
	"sort"
	"sync"
	"time"

	"go.uber.org/zap"
)

// GlobalTenant is the tenant_id sentinel for a flag's global row. It is a
// literal '*' rather than NULL because NULL cannot be part of a primary key.
const GlobalTenant = "*"

// ErrNoDatabase is returned by Set/Unset when the service runs without a
// database (dev/CI): defaults still resolve, but there is nothing to write.
var ErrNoDatabase = errors.New("flags: no database configured")

// Override is a per-tenant row in the resolved view of a flag.
type Override struct {
	TenantID  string    `json:"tenant_id"`
	Enabled   bool      `json:"enabled"`
	UpdatedBy string    `json:"updated_by,omitempty"`
	UpdatedAt time.Time `json:"updated_at,omitempty"`
}

// Flag is the resolved view of one flag: its in-code default, the global
// row (if any), the effective global state, and every tenant override.
type Flag struct {
	Key        string     `json:"key"`
	Registered bool       `json:"registered"`
	Default    bool       `json:"default"`
	HasGlobal  bool       `json:"has_global_row"`
	Global     bool       `json:"global,omitempty"`
	Enabled    bool       `json:"enabled"`
	Overrides  []Override `json:"overrides,omitempty"`
}

// cachedRow is one feature_flags row held in the in-memory snapshot.
type cachedRow struct {
	enabled   bool
	updatedBy string
	updatedAt time.Time
}

// snapshot is the immutable cache swapped atomically on refresh:
// flag_key → tenant_id → row.
type snapshot map[string]map[string]cachedRow

// Service resolves and mutates feature flags. Safe for concurrent use.
type Service struct {
	db     *sql.DB
	logger *zap.Logger

	mu       sync.RWMutex
	defaults map[string]bool
	cache    snapshot

	refreshInterval time.Duration
}

// New creates a Service. db may be nil (defaults only). Call Register for
// each known flag, then Refresh once and Start for the background loop.
func New(db *sql.DB, logger *zap.Logger) *Service {
	if logger == nil {
		logger = zap.NewNop()
	}
	return &Service{
		db:              db,
		logger:          logger,
		defaults:        make(map[string]bool),
		cache:           snapshot{},
		refreshInterval: 15 * time.Second,
	}
}

// Register declares an in-code flag and its default, used when no DB row
// exists. Adding a flag is: Register(key, default) + a call site.
func (s *Service) Register(key string, def bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.defaults[key] = def
}

// Enabled reports whether the flag is on for the given tenant.
// tenantID may be empty for global-only flags (e.g. signups).
func (s *Service) Enabled(key, tenantID string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if rows, ok := s.cache[key]; ok {
		if tenantID != "" && tenantID != GlobalTenant {
			if row, ok := rows[tenantID]; ok {
				return row.enabled
			}
		}
		if row, ok := rows[GlobalTenant]; ok {
			return row.enabled
		}
	}
	return s.defaults[key]
}

// Refresh loads the entire feature_flags table and swaps the cache.
// Nil-DB is a no-op (defaults keep serving).
func (s *Service) Refresh(ctx context.Context) error {
	if s.db == nil {
		return nil
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT flag_key, tenant_id, enabled, COALESCE(updated_by, ''), COALESCE(updated_at, now())
		FROM feature_flags`)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()

	next := snapshot{}
	for rows.Next() {
		var key, tenantID, updatedBy string
		var enabled bool
		var updatedAt time.Time
		if err := rows.Scan(&key, &tenantID, &enabled, &updatedBy, &updatedAt); err != nil {
			return err
		}
		if next[key] == nil {
			next[key] = make(map[string]cachedRow)
		}
		next[key][tenantID] = cachedRow{enabled: enabled, updatedBy: updatedBy, updatedAt: updatedAt}
	}
	if err := rows.Err(); err != nil {
		return err
	}

	s.mu.Lock()
	s.cache = next
	s.mu.Unlock()
	return nil
}

// Start runs the background full-table refresh loop until ctx is done.
// Nil-DB never starts a goroutine.
func (s *Service) Start(ctx context.Context) {
	if s.db == nil {
		return
	}
	go func() {
		ticker := time.NewTicker(s.refreshInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := s.Refresh(ctx); err != nil {
					s.logger.Warn("feature flag refresh failed", zap.Error(err))
				}
			}
		}
	}()
}

// Set upserts a flag row (tenantID = GlobalTenant for the global row) and
// reloads the cache so the flip is visible on the next request.
func (s *Service) Set(ctx context.Context, key, tenantID string, enabled bool, updatedBy string) error {
	if s.db == nil {
		return ErrNoDatabase
	}
	if tenantID == "" {
		tenantID = GlobalTenant
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO feature_flags (flag_key, tenant_id, enabled, updated_by, updated_at)
		VALUES ($1, $2, $3, $4, now())
		ON CONFLICT (flag_key, tenant_id)
		DO UPDATE SET enabled = EXCLUDED.enabled, updated_by = EXCLUDED.updated_by, updated_at = now()`,
		key, tenantID, enabled, updatedBy)
	if err != nil {
		return err
	}
	s.logger.Info("feature flag set",
		zap.String("flag", key),
		zap.String("tenant", tenantID),
		zap.Bool("enabled", enabled),
		zap.String("updated_by", updatedBy))
	return s.Refresh(ctx)
}

// Unset deletes a flag row, reverting the tenant to the global row (or the
// global row to the registered default), then reloads the cache.
func (s *Service) Unset(ctx context.Context, key, tenantID string) error {
	if s.db == nil {
		return ErrNoDatabase
	}
	if tenantID == "" {
		tenantID = GlobalTenant
	}
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM feature_flags WHERE flag_key = $1 AND tenant_id = $2`, key, tenantID)
	if err != nil {
		return err
	}
	s.logger.Info("feature flag unset",
		zap.String("flag", key),
		zap.String("tenant", tenantID))
	return s.Refresh(ctx)
}

// Registered reports whether the key was declared in code.
func (s *Service) Registered(key string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	_, ok := s.defaults[key]
	return ok
}

// Resolved returns the full admin view: every registered flag plus any DB
// key that is not registered (e.g. a leftover row after a flag is removed
// from code), each with default, global row, effective state, and overrides.
func (s *Service) Resolved() []Flag {
	s.mu.RLock()
	defer s.mu.RUnlock()

	keys := make(map[string]bool, len(s.defaults))
	for k := range s.defaults {
		keys[k] = true
	}
	for k := range s.cache {
		keys[k] = true
	}

	out := make([]Flag, 0, len(keys))
	for key := range keys {
		def, registered := s.defaults[key]
		f := Flag{
			Key:        key,
			Registered: registered,
			Default:    def,
			Enabled:    def,
		}
		for tenantID, row := range s.cache[key] {
			if tenantID == GlobalTenant {
				f.HasGlobal = true
				f.Global = row.enabled
				f.Enabled = row.enabled
			} else {
				f.Overrides = append(f.Overrides, Override{
					TenantID:  tenantID,
					Enabled:   row.enabled,
					UpdatedBy: row.updatedBy,
					UpdatedAt: row.updatedAt,
				})
			}
		}
		sort.Slice(f.Overrides, func(i, j int) bool {
			return f.Overrides[i].TenantID < f.Overrides[j].TenantID
		})
		out = append(out, f)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}
