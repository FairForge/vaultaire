// internal/usage/holders.go
//
// The holders of a tenant's bytes (WP-KEY-1 PR B, Stage 1a Prompt 54;
// F-39; D-RP88-2, migration 121): besides the tenant (and its pool), the
// bytes a write holds count to the KEY that wrote them and to the BUCKET
// they are in, so a scoped key and a bucket can each have a storage cap
// inside the tenant's.
//
//   - api_keys.storage_used_bytes: what the rows the key wrote still hold
//     (object_head_cache / object_versions / multipart_uploads
//     .written_by_key). The primary pair writes NULL (F-39 (c): never
//     capped), as every row written before 121 does: counted to no key.
//   - buckets.storage_used_bytes: what the bucket holds; NULL = not counted
//     (a bucket that never had a storage cap). SetBucketCaps seeds it under
//     the bucket's lock at its first cap.
//
// Both are moved by THIS package only (RP82-3's rule, the pool counter's):
// a ledger change carries its holder moves (LedgerChange.Holders) and
// ApplyLedgerTx / ReserveTxFor move them in the transaction that moves the
// tenant's, after the tenant's quota and floor rows (lock order: … →
// tenant_quotas → tenant_floor_quotas → api_keys by id → buckets by name;
// a holder row is locked only while a ledger statement of the same
// transaction runs, and nothing that holds one waits for a quota row).
// A move is released on the row's OWN writer whoever removes it: a delete
// or overwrite by another key releases the writer's bytes.
//
// The check is min(key, bucket, tenant, pool) (F-39 (b)): every cap that
// is set applies, a larger one never widens a smaller. A refusal names the
// first cap that failed in that order (x-stored-cap).
package usage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
)

// Holder is who besides the tenant holds a write's bytes: the key that
// wrote it ("" = none: the primary pair, a background write) and the
// bucket it is in.
type Holder struct {
	KeyID  string
	Bucket string
}

// HolderMove moves a holder's counters by Bytes (signed).
type HolderMove struct {
	Holder
	Bytes int64
}

// The caps a reservation names when it refuses (x-stored-cap).
const (
	CapKey    = "key"
	CapBucket = "bucket"
	CapTenant = "tenant"
	CapPool   = "pool"
)

// holderTotals sums moves per key and per bucket, zero moves dropped,
// sorted (one lock order).
func holderTotals(moves []HolderMove) (keys, buckets []string, byKey, byBucket map[string]int64) {
	byKey, byBucket = map[string]int64{}, map[string]int64{}
	for _, m := range moves {
		if m.Bytes == 0 {
			continue
		}
		if m.KeyID != "" {
			byKey[m.KeyID] += m.Bytes
		}
		if m.Bucket != "" {
			byBucket[m.Bucket] += m.Bytes
		}
	}
	for k, d := range byKey {
		if d != 0 {
			keys = append(keys, k)
		}
	}
	for b, d := range byBucket {
		if d != 0 {
			buckets = append(buckets, b)
		}
	}
	sort.Strings(keys)
	sort.Strings(buckets)
	return keys, buckets, byKey, byBucket
}

// applyHolders moves the holders' counters (clamped at zero). guard: an
// increase that leaves a holder past its cap is ErrOverQuota, naming it.
func applyHolders(ctx context.Context, tx *sql.Tx, tenantID string, moves []HolderMove, guard bool) error {
	keys, buckets, byKey, byBucket := holderTotals(moves)
	for _, k := range keys {
		var used int64
		var limit sql.NullInt64
		err := tx.QueryRowContext(ctx, `
			UPDATE api_keys SET storage_used_bytes = GREATEST(0, storage_used_bytes + $3)
			 WHERE id::text = $1 AND tenant_id = $2
			RETURNING storage_used_bytes, storage_limit_bytes`, k, tenantID, byKey[k]).Scan(&used, &limit)
		if errors.Is(err, sql.ErrNoRows) {
			continue // a key row gone (the erasure): nothing holds there
		}
		if err != nil {
			return fmt.Errorf("ledger: key %s of %s: %w", k, tenantID, err)
		}
		if guard && byKey[k] > 0 && limit.Valid && used > limit.Int64 {
			return &CapError{Cap: CapKey, err: fmt.Errorf("%w: key %s holds %d > %d", ErrOverQuota, k, used, limit.Int64)}
		}
	}
	for _, b := range buckets {
		var used int64
		var limit sql.NullInt64
		err := tx.QueryRowContext(ctx, `
			UPDATE buckets SET storage_used_bytes = GREATEST(0, storage_used_bytes + $3)
			 WHERE tenant_id = $1 AND name = $2 AND storage_used_bytes IS NOT NULL
			RETURNING storage_used_bytes, storage_limit_bytes`, tenantID, b, byBucket[b]).Scan(&used, &limit)
		if errors.Is(err, sql.ErrNoRows) {
			continue // not counted (no cap ever), or no row
		}
		if err != nil {
			return fmt.Errorf("ledger: bucket %s of %s: %w", b, tenantID, err)
		}
		if guard && byBucket[b] > 0 && limit.Valid && used > limit.Int64 {
			return &CapError{Cap: CapBucket, err: fmt.Errorf("%w: bucket %s holds %d > %d", ErrOverQuota, b, used, limit.Int64)}
		}
	}
	return nil
}

// CapError is an ErrOverQuota that names its cap.
type CapError struct {
	Cap string
	err error
}

func (e *CapError) Error() string { return e.err.Error() }
func (e *CapError) Unwrap() error { return e.err }

// CapOf is the cap a ledger refusal names ("" = none named: the tenant's).
func CapOf(err error) string {
	var ce *CapError
	if errors.As(err, &ce) {
		return ce.Cap
	}
	return ""
}

// holderFits locks h's key and bucket rows and reports the first of them
// whose cap n more bytes would pass ("" = both fit).
func holderFits(ctx context.Context, tx *sql.Tx, tenantID string, h Holder, n int64) (string, error) {
	if h.KeyID != "" {
		var used int64
		var limit sql.NullInt64
		err := tx.QueryRowContext(ctx, `SELECT storage_used_bytes, storage_limit_bytes FROM api_keys
			 WHERE id::text = $1 AND tenant_id = $2 FOR UPDATE`, h.KeyID, tenantID).Scan(&used, &limit)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return "", fmt.Errorf("checking the cap of key %s: %w", h.KeyID, err)
		}
		if err == nil && limit.Valid && used+n > limit.Int64 {
			return CapKey, nil
		}
	}
	if h.Bucket != "" {
		var used, limit sql.NullInt64
		err := tx.QueryRowContext(ctx, `SELECT storage_used_bytes, storage_limit_bytes FROM buckets
			 WHERE tenant_id = $1 AND name = $2 FOR UPDATE`, tenantID, h.Bucket).Scan(&used, &limit)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return "", fmt.Errorf("checking the cap of bucket %s: %w", h.Bucket, err)
		}
		if err == nil && limit.Valid && used.Valid && used.Int64+n > limit.Int64 {
			return CapBucket, nil
		}
	}
	return "", nil
}

// What a key and a bucket HOLD, by the rows (the billed rule of billed.go:
// head rows, retained versions, active uploads' holdings) — the reconcile's
// and the seed's, never a write's.
const (
	keyHeldSQL = `SELECT
		  COALESCE((SELECT SUM(o.size_bytes) FROM object_head_cache o WHERE o.written_by_key = $1::uuid), 0)
		+ COALESCE((SELECT SUM(v.size_bytes) FROM ` + RetainedVersionsFrom + ` AND v.written_by_key = $1::uuid), 0)
		+ COALESCE((SELECT SUM(u.reserved_bytes) FROM multipart_uploads u WHERE u.written_by_key = $1::uuid AND u.status = 'active'), 0)`
	bucketHeldSQL = `SELECT
		  COALESCE((SELECT SUM(o.size_bytes) FROM object_head_cache o WHERE o.tenant_id = $1 AND o.bucket = $2), 0)
		+ COALESCE((SELECT SUM(v.size_bytes) FROM ` + RetainedVersionsFrom + ` AND v.tenant_id = $1 AND v.bucket = $2), 0)
		+ COALESCE((SELECT SUM(u.reserved_bytes) FROM multipart_uploads u WHERE u.tenant_id = $1 AND u.bucket = $2 AND u.status = 'active'), 0)`
)

// KeyHeldBytes is what the rows keyID wrote hold.
func (m *QuotaManager) KeyHeldBytes(ctx context.Context, keyID string) (int64, error) {
	var n int64
	if err := m.db.QueryRowContext(ctx, keyHeldSQL, keyID).Scan(&n); err != nil {
		return 0, fmt.Errorf("bytes held by key %s: %w", keyID, err)
	}
	return n, nil
}

// BucketHeldBytes is what tenantID's bucket holds.
func (m *QuotaManager) BucketHeldBytes(ctx context.Context, tenantID, bucket string) (int64, error) {
	var n int64
	if err := m.db.QueryRowContext(ctx, bucketHeldSQL, tenantID, bucket).Scan(&n); err != nil {
		return 0, fmt.Errorf("bytes held by bucket %s: %w", bucket, err)
	}
	return n, nil
}

// Caps is a key's or a bucket's own caps (nil = none: inherit).
type Caps struct {
	StorageLimit *int64
	EgressLimit  *int64
}

// ErrNegativeCap: a cap below zero.
var ErrNegativeCap = errors.New("a cap is a number of bytes, 0 or more")

// ErrPrimaryNeverCapped: the primary pair is never capped (F-39 (c)).
var ErrPrimaryNeverCapped = errors.New("the primary key pair is never capped")

// ErrNoSuchHolder: no live key (or bucket) of the tenant has that name.
var ErrNoSuchHolder = errors.New("no such key or bucket")

func (c Caps) validate() error {
	for _, v := range []*int64{c.StorageLimit, c.EgressLimit} {
		if v != nil && *v < 0 {
			return ErrNegativeCap
		}
	}
	return nil
}

// CapsResult is what a cap set wrote, with its notes (F-39 (b): a cap above
// the tenant's is accepted, and the tenant's decides).
type CapsResult struct {
	StorageUsed int64
	Notes       []string
}

// The notes of a cap set.
const (
	NoteStorageAboveTenant = "storage_cap_above_tenant"
	NoteEgressAboveTenant  = "egress_cap_above_tenant"
)

// SetKeyCaps sets keyID's caps (the tenant's live, non-primary key). Its
// storage counter needs no seed: every row written since 121 moved it.
func (m *QuotaManager) SetKeyCaps(ctx context.Context, tenantID, keyID string, c Caps, tenantEgress int64) (CapsResult, error) {
	if err := c.validate(); err != nil {
		return CapsResult{}, err
	}
	tx, err := m.db.BeginTx(ctx, nil)
	if err != nil {
		return CapsResult{}, fmt.Errorf("set the caps of key %s: %w", keyID, err)
	}
	defer func() { _ = tx.Rollback() }()
	var primary bool
	var used int64
	err = tx.QueryRowContext(ctx, `SELECT is_primary, storage_used_bytes FROM api_keys
		 WHERE id::text = $1 AND tenant_id = $2 AND revoked_at IS NULL FOR UPDATE`, keyID, tenantID).Scan(&primary, &used)
	if errors.Is(err, sql.ErrNoRows) {
		return CapsResult{}, ErrNoSuchHolder
	}
	if err != nil {
		return CapsResult{}, fmt.Errorf("set the caps of key %s: %w", keyID, err)
	}
	if primary {
		return CapsResult{}, ErrPrimaryNeverCapped
	}
	if _, err := tx.ExecContext(ctx, `UPDATE api_keys SET storage_limit_bytes = $2, egress_limit_bytes = $3 WHERE id::text = $1`,
		keyID, c.StorageLimit, c.EgressLimit); err != nil {
		return CapsResult{}, fmt.Errorf("set the caps of key %s: %w", keyID, err)
	}
	notes, err := capNotes(ctx, tx, tenantID, c, tenantEgress)
	if err != nil {
		return CapsResult{}, err
	}
	if err := tx.Commit(); err != nil {
		return CapsResult{}, fmt.Errorf("set the caps of key %s: %w", keyID, err)
	}
	return CapsResult{StorageUsed: used, Notes: notes}, nil
}

// SetBucketCaps sets tenantID's bucket's caps. A storage cap starts the
// bucket's counter: seeded by the rows under the bucket's lock at its first
// cap (a write's reservation locks the same row), kept while a cap is set,
// and NULL again when the storage cap is cleared (nothing to count for).
func (m *QuotaManager) SetBucketCaps(ctx context.Context, tenantID, bucket string, c Caps, tenantEgress int64) (CapsResult, error) {
	if err := c.validate(); err != nil {
		return CapsResult{}, err
	}
	tx, err := m.db.BeginTx(ctx, nil)
	if err != nil {
		return CapsResult{}, fmt.Errorf("set the caps of bucket %s: %w", bucket, err)
	}
	defer func() { _ = tx.Rollback() }()
	var counted sql.NullInt64
	err = tx.QueryRowContext(ctx, `SELECT storage_used_bytes FROM buckets WHERE tenant_id = $1 AND name = $2 FOR UPDATE`,
		tenantID, bucket).Scan(&counted)
	if errors.Is(err, sql.ErrNoRows) {
		return CapsResult{}, ErrNoSuchHolder
	}
	if err != nil {
		return CapsResult{}, fmt.Errorf("set the caps of bucket %s: %w", bucket, err)
	}
	var used any
	var res CapsResult
	if c.StorageLimit != nil {
		n := counted.Int64
		if !counted.Valid {
			if err := tx.QueryRowContext(ctx, bucketHeldSQL, tenantID, bucket).Scan(&n); err != nil {
				return CapsResult{}, fmt.Errorf("seed the counter of bucket %s: %w", bucket, err)
			}
		}
		used, res.StorageUsed = n, n
	}
	if _, err := tx.ExecContext(ctx, `UPDATE buckets SET storage_limit_bytes = $3, egress_limit_bytes = $4, storage_used_bytes = $5
		 WHERE tenant_id = $1 AND name = $2`, tenantID, bucket, c.StorageLimit, c.EgressLimit, used); err != nil {
		return CapsResult{}, fmt.Errorf("set the caps of bucket %s: %w", bucket, err)
	}
	if res.Notes, err = capNotes(ctx, tx, tenantID, c, tenantEgress); err != nil {
		return CapsResult{}, err
	}
	if err := tx.Commit(); err != nil {
		return CapsResult{}, fmt.Errorf("set the caps of bucket %s: %w", bucket, err)
	}
	return res, nil
}

// capNotes names the caps above the tenant's own (F-39 (b)): the tenant's
// storage limit (a pooled child's own cap when it has one, else its pool's)
// and its egress allowance (tenantEgress; 0 = unknown, no note).
func capNotes(ctx context.Context, tx *sql.Tx, tenantID string, c Caps, tenantEgress int64) ([]string, error) {
	notes := []string{}
	if c.StorageLimit != nil {
		var limit int64
		err := tx.QueryRowContext(ctx, `
			SELECT CASE WHEN q.storage_limit_bytes > 0 THEN q.storage_limit_bytes ELSE COALESCE(o.storage_limit_bytes, 0) END
			  FROM tenant_quotas q
			  LEFT JOIN tenants t ON t.id = q.tenant_id
			  LEFT JOIN tenant_quotas o ON o.tenant_id = t.pool_root_id
			 WHERE q.tenant_id = $1`, tenantID).Scan(&limit)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("the storage limit of %s: %w", tenantID, err)
		}
		if limit > 0 && *c.StorageLimit > limit {
			notes = append(notes, NoteStorageAboveTenant)
		}
	}
	if c.EgressLimit != nil && tenantEgress > 0 && *c.EgressLimit > tenantEgress {
		notes = append(notes, NoteEgressAboveTenant)
	}
	return notes, nil
}

// The holders' counters as the rows say, correlated (a over api_keys, b
// over buckets) — the reconcile's, the sandbox purge's and the drift
// reading's.
const (
	keyHeldOfA = `(COALESCE((SELECT SUM(o.size_bytes) FROM object_head_cache o WHERE o.written_by_key = a.id), 0)
		+ COALESCE((SELECT SUM(v.size_bytes) FROM ` + RetainedVersionsFrom + ` AND v.written_by_key = a.id), 0)
		+ COALESCE((SELECT SUM(u.reserved_bytes) FROM multipart_uploads u WHERE u.written_by_key = a.id AND u.status = 'active'), 0))`
	bucketHeldOfB = `(COALESCE((SELECT SUM(o.size_bytes) FROM object_head_cache o WHERE o.tenant_id = b.tenant_id AND o.bucket = b.name), 0)
		+ COALESCE((SELECT SUM(v.size_bytes) FROM ` + RetainedVersionsFrom + ` AND v.tenant_id = b.tenant_id AND v.bucket = b.name), 0)
		+ COALESCE((SELECT SUM(u.reserved_bytes) FROM multipart_uploads u WHERE u.tenant_id = b.tenant_id AND u.bucket = b.name AND u.status = 'active'), 0))`
	// A bucket is counted while it has a storage cap (or a counter).
	bucketCounted = `(b.storage_used_bytes IS NOT NULL OR b.storage_limit_bytes IS NOT NULL)`
)

// Execer is a *sql.DB or a *sql.Tx.
type Execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// ReassertHolders rewrites tenantID's keys' and counted buckets' counters
// to what the rows hold ("" = every tenant's). Like the tenant's reconcile
// it is true only while the tenant's writes are quiet: a reservation in
// flight is not in the rows yet.
func ReassertHolders(ctx context.Context, q Execer, tenantID string) error {
	if _, err := q.ExecContext(ctx, `UPDATE api_keys a SET storage_used_bytes = `+keyHeldOfA+`
		 WHERE ($1 = '' OR a.tenant_id = $1)
		   AND a.storage_used_bytes IS DISTINCT FROM `+keyHeldOfA, tenantID); err != nil {
		return fmt.Errorf("re-assert the key counters of %q: %w", tenantID, err)
	}
	if _, err := q.ExecContext(ctx, `UPDATE buckets b SET storage_used_bytes = `+bucketHeldOfB+`
		 WHERE ($1 = '' OR b.tenant_id = $1) AND `+bucketCounted+`
		   AND b.storage_used_bytes IS DISTINCT FROM `+bucketHeldOfB, tenantID); err != nil {
		return fmt.Errorf("re-assert the bucket counters of %q: %w", tenantID, err)
	}
	return nil
}

// HolderDrift is how far the holders' counters are from the rows: per kind
// ("key", "bucket"), the rows whose counter differs and the sum of the
// differences' magnitudes — over the CAPPED keys and the counted buckets
// only (the counters a refusal reads; written_by_key has no index, so a
// walk of every key on every scrape would scan the head rows per key). Read on scrape (a reservation in flight is a
// transient difference; a sustained one is a ledger bug).
func HolderDrift(ctx context.Context, db *sql.DB) (rows, bytes map[string]int64, err error) {
	rows, bytes = map[string]int64{}, map[string]int64{}
	for kind, q := range map[string]string{
		CapKey: `SELECT COUNT(*), COALESCE(SUM(ABS(d)), 0) FROM (
			SELECT a.storage_used_bytes - ` + keyHeldOfA + ` AS d FROM api_keys a
			 WHERE a.storage_limit_bytes IS NOT NULL) x WHERE d <> 0`,
		CapBucket: `SELECT COUNT(*), COALESCE(SUM(ABS(d)), 0) FROM (
			SELECT COALESCE(b.storage_used_bytes, 0) - ` + bucketHeldOfB + ` AS d FROM buckets b WHERE ` + bucketCounted + `) x WHERE d <> 0`,
	} {
		var n, sum int64
		if err := db.QueryRowContext(ctx, q).Scan(&n, &sum); err != nil {
			return nil, nil, fmt.Errorf("holder drift (%s): %w", kind, err)
		}
		rows[kind], bytes[kind] = n, sum
	}
	return rows, bytes, nil
}
