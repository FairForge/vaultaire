// Package flags provides runtime feature flags backed by the feature_flags
// table (migration 059): global kill-switches and per-tenant enablement,
// flippable via the admin API / dashboard with no deploy or restart.
//
// Resolution precedence: tenant row → global row ('*') → registered in-code
// default. Unregistered keys with no row are disabled. A flag registered
// OnWins (Stage 1a Prompt 34a, RP44-2: the transparency log's record of
// admin reads) is on when the tenant row OR the global state is on: a
// tenant row can only turn it on, and a per-tenant OFF row under a global
// ON is refused (ErrNotExemptible). A flag registered with an Inheritance
// (WP-ACCT-1, RP82-4) reads a child tenant's tree: InheritTighten (a
// privacy or safety flag — a sealing default) is on when it is on anywhere
// up the tree, and a child's OFF row under an ancestor's ON is refused
// (ErrOnlyTightens); InheritDefault (a performance flag) takes the nearest
// row up the tree as the child's default. The whole table is
// cached in memory and refreshed every ~15s (it is tiny), so the hot path
// never touches the database; Set/Unset write through and reload the cache
// immediately so an admin flip is visible on the next request.
package flags

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/FairForge/vaultaire/internal/audit"
	"go.uber.org/zap"
)

// GlobalTenant is the tenant_id sentinel for a flag's global row. It is a
// literal '*' rather than NULL because NULL cannot be part of a primary key.
const GlobalTenant = "*"

// ErrNoDatabase is returned by Set/Unset when the service runs without a
// database (dev/CI): defaults still resolve, but there is nothing to write.
var ErrNoDatabase = errors.New("flags: no database configured")

// ErrNotExemptible: a per-tenant OFF row of an OnWins flag while the flag is
// on globally — it would exempt one tenant from what everyone has (RP44-2).
var ErrNotExemptible = errors.New("flags: this flag cannot be turned off for one tenant while it is on globally")

// ErrGlobalOnly: a per-tenant row of a flag that is read only globally.
var ErrGlobalOnly = errors.New("flags: this flag has a global row only")

// ErrOnlyTightens: a child's OFF row of an InheritTighten flag that an
// ancestor (or the global state) turns on — a child may tighten a privacy
// or safety flag, never weaken it (RP82-4).
var ErrOnlyTightens = errors.New("flags: this flag is on up the account tree; a child may tighten it, never turn it off")

// Inheritance is how a flag reads a child tenant's tree (WP-ACCT-1).
type Inheritance int

const (
	// NoInherit: the tenant's own row, else the global state (every flag
	// not registered otherwise).
	NoInherit Inheritance = iota
	// InheritDefault: a performance flag — the nearest row up the tree
	// (the tenant's own first) decides, else the global state.
	InheritDefault
	// InheritTighten: a privacy or safety flag — on when the global state
	// or any row up the tree (the tenant's own included) is on.
	InheritTighten
)

// maxTreeDepth bounds the walk up the tree (depth 2 is the deepest the
// tree allows; a cycle can never loop).
const maxTreeDepth = 8

// Refusal is a write a server-side guard refused because the flag's
// preconditions do not hold (standard_sealed_default without a master key
// or with tenants that have no key version): the API's and the dashboard's
// 409 with Code and Count. Nothing was written.
type Refusal struct {
	Flag  string
	Code  string
	Count int64
	Msg   string
}

func (e *Refusal) Error() string { return e.Flag + ": " + e.Msg + " (" + e.Code + ")" }

// ErrChangeRefused: the write guard refused the change; nothing was written
// (the guard's own error is wrapped too).
var ErrChangeRefused = errors.New("flags: the change was refused before it was written")

// Change is one row write offered to the write guard before it happens.
type Change struct {
	Key      string
	TenantID string // GlobalTenant for the global row
	Clear    bool   // the row is removed (Unset)
	Enabled  bool   // the value written; false for a clear
}

// WriteGuard runs before every Set and Unset writes the database; a non-nil
// error refuses the write (ErrChangeRefused) and nothing is written.
type WriteGuard func(ctx context.Context, c Change) error

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

	mu         sync.RWMutex
	defaults   map[string]bool
	onWins     map[string]bool
	globalOnly map[string]bool
	inherit    map[string]Inheritance
	guard      WriteGuard
	cache      snapshot
	// parents is every child tenant's parent (tenants.parent_tenant_id),
	// loaded with the table: the tree an inherited flag walks.
	parents map[string]string

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
		onWins:          make(map[string]bool),
		globalOnly:      make(map[string]bool),
		inherit:         make(map[string]Inheritance),
		cache:           snapshot{},
		parents:         map[string]string{},
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

// OnWins marks a registered flag ON-wins: on for a tenant when its row OR
// the global state (row, else default) is on.
func (s *Service) OnWins(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.onWins[key] = true
}

// GlobalOnly marks a flag read only globally: Set refuses a tenant row.
func (s *Service) GlobalOnly(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.globalOnly[key] = true
}

// Inherit declares how a registered flag reads a child's tree.
func (s *Service) Inherit(key string, how Inheritance) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.inherit[key] = how
}

// Inheritances is every flag's declared inheritance (the registration's
// pin test reads it).
func (s *Service) Inheritances() map[string]Inheritance {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make(map[string]Inheritance, len(s.inherit))
	for k, v := range s.inherit {
		out[k] = v
	}
	return out
}

// SetWriteGuard installs g, run before every row write (nil = none).
func (s *Service) SetWriteGuard(g WriteGuard) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.guard = g
}

// Enabled reports whether the flag is on for the given tenant.
// tenantID may be empty for global-only flags (e.g. signups).
func (s *Service) Enabled(key, tenantID string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()

	global := s.globalLocked(key)
	if how := s.inherit[key]; how != NoInherit && tenantID != "" && tenantID != GlobalTenant {
		return s.inheritedLocked(key, tenantID, how, global)
	}
	if row, ok := s.cache[key][tenantID]; ok && tenantID != "" && tenantID != GlobalTenant {
		if s.onWins[key] {
			return row.enabled || global
		}
		return row.enabled
	}
	return global
}

// inheritedLocked resolves an inherited flag for tenantID by walking up
// its tree (the tenant first). s.mu held.
func (s *Service) inheritedLocked(key, tenantID string, how Inheritance, global bool) bool {
	t := tenantID
	for i := 0; t != "" && i < maxTreeDepth; i++ {
		if row, ok := s.cache[key][t]; ok {
			switch {
			case how == InheritDefault:
				return row.enabled
			case row.enabled:
				return true
			}
		}
		t = s.parents[t]
	}
	return global
}

// ancestorsOnLocked reports whether an InheritTighten flag is on for
// tenantID from ABOVE it (the global state or an ancestor's row). s.mu held.
func (s *Service) ancestorsOnLocked(key, tenantID string) bool {
	if s.globalLocked(key) {
		return true
	}
	t := s.parents[tenantID]
	for i := 0; t != "" && i < maxTreeDepth; i++ {
		if row, ok := s.cache[key][t]; ok && row.enabled {
			return true
		}
		t = s.parents[t]
	}
	return false
}

// globalLocked is the flag's global state: its global row, else its
// default. s.mu held.
func (s *Service) globalLocked(key string) bool {
	if row, ok := s.cache[key][GlobalTenant]; ok {
		return row.enabled
	}
	return s.defaults[key]
}

// check refuses a write the flag's rules forbid, then asks the guard.
func (s *Service) check(ctx context.Context, c Change) error {
	tenantRow := c.TenantID != GlobalTenant
	s.mu.RLock()
	onWins, globalOnly, guard, global := s.onWins[c.Key], s.globalOnly[c.Key], s.guard, s.globalLocked(c.Key)
	tightened := s.inherit[c.Key] == InheritTighten && tenantRow && s.ancestorsOnLocked(c.Key, c.TenantID)
	s.mu.RUnlock()
	switch {
	case globalOnly && tenantRow && !c.Clear:
		return ErrGlobalOnly
	case onWins && tenantRow && !c.Clear && !c.Enabled && global:
		return ErrNotExemptible
	case tightened && !c.Clear && !c.Enabled:
		return ErrOnlyTightens
	}
	if guard != nil {
		if err := guard(ctx, c); err != nil {
			return fmt.Errorf("%w: %w", ErrChangeRefused, err)
		}
	}
	return nil
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
	// The account tree an inherited flag walks (WP-ACCT-1): every child's
	// parent. A tenants table older than 117 has no column: no tree.
	parents := map[string]string{}
	prows, err := s.db.QueryContext(ctx, `SELECT id, parent_tenant_id FROM tenants WHERE parent_tenant_id IS NOT NULL`)
	if err != nil {
		s.logger.Warn("feature flags: the account tree could not be read (inherited flags read no parent)", zap.Error(err))
	} else {
		defer func() { _ = prows.Close() }()
		for prows.Next() {
			var id, parent string
			if err := prows.Scan(&id, &parent); err != nil {
				return err
			}
			parents[id] = parent
		}
		if err := prows.Err(); err != nil {
			return err
		}
	}

	s.mu.Lock()
	s.cache = next
	s.parents = parents
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
	if err := s.check(ctx, Change{Key: key, TenantID: tenantID, Enabled: enabled}); err != nil {
		return err
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
	audit.Record(ctx, s.db, audit.Entry{TenantID: auditTenant(tenantID), Action: "flag.set", Resource: "flag:" + key,
		Metadata: map[string]any{"enabled": enabled, "tenant": tenantID, "updated_by": updatedBy}})
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
	if err := s.check(ctx, Change{Key: key, TenantID: tenantID, Clear: true}); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM feature_flags WHERE flag_key = $1 AND tenant_id = $2`, key, tenantID)
	if err != nil {
		return err
	}
	s.logger.Info("feature flag unset",
		zap.String("flag", key),
		zap.String("tenant", tenantID))
	audit.Record(ctx, s.db, audit.Entry{TenantID: auditTenant(tenantID), Action: "flag.unset", Resource: "flag:" + key,
		Metadata: map[string]any{"tenant": tenantID}})
	return s.Refresh(ctx)
}

// auditTenant maps the global row marker to "no tenant" for the audit row.
func auditTenant(tenantID string) string {
	if tenantID == GlobalTenant {
		return ""
	}
	return tenantID
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
