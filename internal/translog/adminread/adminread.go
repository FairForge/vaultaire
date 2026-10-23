// Package adminread writes and finds the transparency log's admin_read
// entries (K4c-1, Stage 1a Prompt 33; docs/ARCHITECTURE_CONTRACT.md rule 7:
// "an admin read of customer metadata" writes to the log; "customers read
// outcomes; auditors read the log"; docs/TRANSPARENCY.md § Admin reads).
//
// An entry names a scope and, for a read of ONE tenant, an opaque
// commitment: a random 16-byte nonce and HMAC-SHA256(salt, label ‖ nonce),
// the salt being the tenant's row of translog_subjects (migration 104),
// created at its first admin read and erased with the account. So:
//   - the public sees how many admin reads happened and of which scope, and
//     cannot link two entries to one tenant;
//   - the server, for the tenant, finds the tenant's own entries (Find);
//   - after an erasure no one can link past entries — their coalescing keys
//     (translog_entries.dedup_key, never public) are keyed by the same salt.
//
// Coalescing: one entry per (operator, scope, tenant) per UTC hour — a
// refresh appends nothing (the log's own dedup, in Append's transaction).
package adminread

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"sort"
	"time"

	"github.com/FairForge/vaultaire/internal/translog"
	"github.com/FairForge/vaultaire/internal/translog/logstore"
)

// Store records and finds admin reads.
type Store struct {
	db   *sql.DB
	log  *logstore.Store
	now  func() time.Time
	rand io.Reader
}

// New returns the store over db (translog_subjects) and log.
func New(db *sql.DB, log *logstore.Store) *Store {
	return &Store{db: db, log: log, now: time.Now, rand: rand.Reader}
}

// ErrNoSubject: a scope that reads one tenant was given none.
var ErrNoSubject = errors.New("adminread: this scope reads one tenant, and none was named")

// Record appends the entry of one admin read BEFORE the data is read and
// served (the caller refuses the read when it fails — fail closed): scope
// is one of translog.AdminReadScopes(); tenantID names the tenant read for a
// subject scope ("" for a list or an export); actorID is the operator, used
// only for the coalescing key and never written to the body. appended =
// false when the same operator read the same scope of the same tenant this
// hour (idx is that entry's). A frozen log is logstore.ErrLogFrozen.
func (s *Store) Record(ctx context.Context, actorID, scope, tenantID string) (idx int64, appended bool, err error) {
	if s == nil || s.log == nil {
		return 0, false, errors.New("adminread: no log")
	}
	hour := s.now().UTC().Truncate(time.Hour).Format(time.RFC3339)
	e := translog.AdminRead{Scope: scope, ActorRole: translog.ActorRoleAdmin}
	var key string
	if translog.ScopeHasSubject(scope) {
		if tenantID == "" {
			return 0, false, ErrNoSubject
		}
		salt, err := s.salt(ctx, tenantID, true)
		if err != nil {
			return 0, false, err
		}
		nonce := make([]byte, 16)
		if _, err := io.ReadFull(s.rand, nonce); err != nil {
			return 0, false, fmt.Errorf("adminread: nonce: %w", err)
		}
		e.Nonce, e.Commitment = hex.EncodeToString(nonce), hex.EncodeToString(Commitment(salt, nonce))
		key = hex.EncodeToString(mac(salt, "coalesce", actorID, scope, hour))
	} else {
		if tenantID != "" {
			return 0, false, fmt.Errorf("adminread: %s reads across tenants and names none", scope)
		}
		sum := sha256.Sum256([]byte(join("admin_read", actorID, scope, hour)))
		key = hex.EncodeToString(sum[:])
	}
	return s.log.Append(ctx, translog.CoalescedAdminRead(e, key))
}

// Commitment is HMAC-SHA256(salt, translog.AdminReadLabel ‖ nonce).
func Commitment(salt, nonce []byte) []byte {
	m := hmac.New(sha256.New, salt)
	m.Write([]byte(translog.AdminReadLabel))
	m.Write(nonce)
	return m.Sum(nil)
}

func mac(salt []byte, parts ...string) []byte {
	m := hmac.New(sha256.New, salt)
	m.Write([]byte(join(parts...)))
	return m.Sum(nil)
}

func join(parts ...string) string {
	out := ""
	for i, p := range parts {
		if i > 0 {
			out += "\x00"
		}
		out += p
	}
	return out
}

// salt is the tenant's 32-byte salt; create = make it when absent (the
// first admin read). ok-less: absent and !create = nil, nil.
func (s *Store) salt(ctx context.Context, tenantID string, create bool) ([]byte, error) {
	if create {
		fresh := make([]byte, 32)
		if _, err := io.ReadFull(s.rand, fresh); err != nil {
			return nil, fmt.Errorf("adminread: salt: %w", err)
		}
		if _, err := s.db.ExecContext(ctx, `INSERT INTO translog_subjects (tenant_id, salt) VALUES ($1, $2) ON CONFLICT (tenant_id) DO NOTHING`,
			tenantID, fresh); err != nil {
			return nil, fmt.Errorf("adminread: salt of %s: %w", tenantID, err)
		}
	}
	var salt []byte
	err := s.db.QueryRowContext(ctx, `SELECT salt FROM translog_subjects WHERE tenant_id = $1`, tenantID).Scan(&salt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("adminread: salt of %s: %w", tenantID, err)
	}
	return salt, nil
}

// Found is one of a tenant's admin-read entries.
type Found struct {
	Index int64     `json:"index"`
	Scope string    `json:"scope"`
	At    time.Time `json:"at"`
	// CheckpointSize is the size of the first signed checkpoint whose tree
	// includes the entry (index < size); nil = not signed yet.
	CheckpointSize *int64 `json:"checkpoint_size"`
}

// Find returns the tenant's own admin-read entries, oldest first: every
// admin_read entry of a subject scope whose commitment is the HMAC of its
// nonce under the tenant's salt. A tenant never read has no salt and no
// entries. Linear in the admin_read entries (they are few).
func (s *Store) Find(ctx context.Context, tenantID string) ([]Found, error) {
	salt, err := s.salt(ctx, tenantID, false)
	if err != nil || salt == nil {
		return []Found{}, err
	}
	sizes, err := s.checkpointSizes(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT idx, body FROM translog_entries WHERE kind = $1 ORDER BY idx`, translog.KindAdminRead)
	if err != nil {
		return nil, fmt.Errorf("adminread: entries: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := []Found{}
	for rows.Next() {
		var idx int64
		var body []byte
		if err := rows.Scan(&idx, &body); err != nil {
			return nil, fmt.Errorf("adminread: entries: %w", err)
		}
		d, err := translog.Decode(body)
		if err != nil {
			return nil, fmt.Errorf("adminread: entry %d: %w", idx, err)
		}
		a, ok := d.Entry.(translog.AdminRead)
		if !ok || !translog.ScopeHasSubject(a.Scope) {
			continue
		}
		nonce, err1 := hex.DecodeString(a.Nonce)
		want, err2 := hex.DecodeString(a.Commitment)
		if err1 != nil || err2 != nil || !hmac.Equal(Commitment(salt, nonce), want) {
			continue
		}
		f := Found{Index: idx, Scope: a.Scope, At: d.At}
		if i := sort.Search(len(sizes), func(i int) bool { return sizes[i] > idx }); i < len(sizes) {
			n := sizes[i]
			f.CheckpointSize = &n
		}
		out = append(out, f)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("adminread: entries: %w", err)
	}
	return out, nil
}

func (s *Store) checkpointSizes(ctx context.Context) ([]int64, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT size FROM translog_checkpoints ORDER BY size`)
	if err != nil {
		return nil, fmt.Errorf("adminread: checkpoints: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []int64
	for rows.Next() {
		var n int64
		if err := rows.Scan(&n); err != nil {
			return nil, fmt.Errorf("adminread: checkpoints: %w", err)
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// TenantExists reports whether tenantID is a tenant (a read of a tenant that
// does not exist reads no one, and records nothing).
func TenantExists(ctx context.Context, db *sql.DB, tenantID string) (bool, error) {
	if db == nil || tenantID == "" {
		return false, nil
	}
	var ok bool
	if err := db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM tenants WHERE id = $1)`, tenantID).Scan(&ok); err != nil {
		return false, fmt.Errorf("adminread: tenant %s: %w", tenantID, err)
	}
	return ok, nil
}

// TenantOfUser is the tenant of a user (tenants.email = users.email, the
// registration's pairing); "" when the user has none.
func TenantOfUser(ctx context.Context, db *sql.DB, userID string) (string, error) {
	if db == nil || userID == "" {
		return "", errors.New("adminread: no database")
	}
	var t string
	err := db.QueryRowContext(ctx, `SELECT t.id FROM users u JOIN tenants t ON t.email = u.email WHERE u.id::text = $1 LIMIT 1`, userID).Scan(&t)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("adminread: tenant of user %s: %w", userID, err)
	}
	return t, nil
}
