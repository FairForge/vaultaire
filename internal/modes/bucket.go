package modes

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/FairForge/vaultaire/internal/audit"
	"github.com/FairForge/vaultaire/internal/crypto"
	"github.com/lib/pq"
)

// The buckets columns a mode follows from (migration 102 derives
// buckets.mode from them). NAME TRAP: visibility 'private' means "not
// public-read" — Standard mode, not Private.
const (
	VisibilityPrivate = "private"
	VisibilityPublic  = "public-read"
)

var (
	// ErrPublicSSEC: a public bucket cannot require a customer-provided key
	// (096, WP-SSEC-1 B.5) — 409 bucket_public.
	ErrPublicSSEC = errors.New("bucket_public")
	// ErrNoBucket: the tenant has no such bucket.
	ErrNoBucket = errors.New("no_such_bucket")
	// ErrBadVisibility: a visibility other than private / public-read.
	ErrBadVisibility = errors.New("invalid_visibility")
)

// FromColumns is migration 102's CASE in Go: public-read → public,
// ssec_required → private, else standard.
func FromColumns(visibility string, ssecRequired bool) string {
	switch {
	case visibility == VisibilityPublic:
		return Public
	case ssecRequired:
		return Private
	}
	return Standard
}

// Change is a mode change: the columns to set (nil = keep).
type Change struct {
	Visibility   *string
	SSECRequired *bool
}

// Querier is a *sql.DB or a *sql.Tx. A *sql.DB runs the change in a
// transaction of its own (the door's lock is transaction-scoped).
type Querier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// ErrReadableObjects: the change would make a bucket Private while it holds
// something we can read (RP33-1) — 409 bucket_has_readable_objects. The
// *ReadableObjectsError carrying it says how many.
var ErrReadableObjects = errors.New("bucket_has_readable_objects")

// ReadableObjectsError is ErrReadableObjects with the count of what we can
// read: head rows and retained versions not sealed with the customer's key,
// and multipart uploads in progress without one.
type ReadableObjectsError struct {
	Bucket                     string
	Objects, Versions, Uploads int64
}

// Total is everything the refusal counted.
func (e *ReadableObjectsError) Total() int64 { return e.Objects + e.Versions + e.Uploads }

func (e *ReadableObjectsError) Error() string {
	return fmt.Sprintf("bucket %s holds %d readable object(s), %d readable version(s) and %d upload(s) without a customer key: %s",
		e.Bucket, e.Objects, e.Versions, e.Uploads, ErrReadableObjects)
}

// Unwrap makes errors.Is(err, ErrReadableObjects) hold.
func (e *ReadableObjectsError) Unwrap() error { return ErrReadableObjects }

// ModeLockSQL takes the bucket-mode advisory lock (RP33-1): EXCLUSIVE in the
// door, SHARED (ModeLockSharedSQL) by every commit of a head or version row
// with bytes, first, before the key's own locks. The two-int form keeps it
// out of the single-bigint key space the head and version locks use.
// $1 = tenant, $2 = bucket.
const (
	ModeLockSQL       = `SELECT pg_advisory_xact_lock(hashtext('bucket-mode'), hashtext($1 || '/' || $2))`
	ModeLockSharedSQL = `SELECT pg_advisory_xact_lock_shared(hashtext('bucket-mode'), hashtext($1 || '/' || $2))`
)

// SSECAlgorithms are the encryption_algorithm values of an object sealed
// with the customer's key (crypto.IsSSEC; the column has recorded them since
// SSE-C existed — ssec_key_md5 only since 094, so it is not the marker).
var SSECAlgorithms = []string{crypto.SSECAlgorithm, crypto.SSECAlgorithmV3, crypto.SSECAlgorithmMultipart}

// readableSQL counts what we can read in a bucket: head rows not sealed with
// the customer's key (a chunked row never is), retained versions with bytes
// not sealed with it (delete markers hold none; a version row written before
// 087 has no encryption_algorithm and counts — the conservative side), and
// active multipart uploads created without a key (096: ssec_key_md5 ”).
const readableSQL = `
	SELECT
	  (SELECT count(*) FROM object_head_cache
	    WHERE tenant_id = $1 AND bucket = $2 AND NOT (COALESCE(encryption_algorithm, '') = ANY($3))),
	  (SELECT count(*) FROM object_versions
	    WHERE tenant_id = $1 AND bucket = $2 AND NOT is_delete_marker AND NOT (COALESCE(encryption_algorithm, '') = ANY($3))),
	  (SELECT count(*) FROM multipart_uploads
	    WHERE tenant_id = $1 AND bucket = $2 AND status = 'active' AND ssec_key_md5 = '')`

// ActionModeChanged is the audit action of every mode change (RP33-4).
const ActionModeChanged = "bucket.mode_changed"

// SetBucket is THE door through which a bucket's mode changes (K3): the
// only writer of buckets.visibility and buckets.ssec_required after the
// bucket's creation (which always writes Standard; a create asking for
// Private comes through here in the create's transaction). Under the
// bucket's mode lock (exclusive) it reads the row, refuses Private over
// readable bytes (RP33-1: a false→true ssec_required change while the bucket
// holds anything we can read — ErrReadableObjects), writes the columns in one
// statement that enforces the 096 rule against the row's values after the
// change — a public bucket never requires a key — and records the change in
// audit_logs in the same transaction (RP33-4). Returns the mode the row now
// has (the generated column). ErrNoBucket, ErrPublicSSEC, ErrBadVisibility,
// ErrReadableObjects; nothing written on a refusal.
//
// The readable check is its own statement AFTER the lock: under READ
// COMMITTED a statement's snapshot is taken when it starts, so a subquery
// inside the UPDATE would keep the snapshot from before the wait and miss a
// row a writer committed meanwhile. A writer commits under the shared lock
// and re-reads ssec_required there (internal/api commitBlobWrite), so a
// write either committed before the lock (counted here) or sees the flag.
func SetBucket(ctx context.Context, q Querier, tenantID, bucket string, c Change) (string, error) {
	if c.Visibility != nil && *c.Visibility != VisibilityPrivate && *c.Visibility != VisibilityPublic {
		return "", fmt.Errorf("bucket %s: visibility %q: %w", bucket, *c.Visibility, ErrBadVisibility)
	}
	if db, ok := q.(*sql.DB); ok {
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return "", fmt.Errorf("bucket %s: begin: %w", bucket, err)
		}
		defer func() { _ = tx.Rollback() }()
		mode, err := SetBucket(ctx, tx, tenantID, bucket, c)
		if err != nil {
			return "", err
		}
		if err := tx.Commit(); err != nil {
			return "", fmt.Errorf("bucket %s: commit: %w", bucket, err)
		}
		return mode, nil
	}

	if _, err := q.ExecContext(ctx, ModeLockSQL, tenantID, bucket); err != nil {
		return "", fmt.Errorf("bucket %s: mode lock: %w", bucket, err)
	}
	var fromMode string
	var fromSSEC bool
	err := q.QueryRowContext(ctx, `SELECT mode, ssec_required FROM buckets WHERE tenant_id = $1 AND name = $2`,
		tenantID, bucket).Scan(&fromMode, &fromSSEC)
	if errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("bucket %s: %w", bucket, ErrNoBucket)
	}
	if err != nil {
		return "", fmt.Errorf("bucket %s: read the mode: %w", bucket, err)
	}
	if c.SSECRequired != nil && *c.SSECRequired && !fromSSEC {
		r := &ReadableObjectsError{Bucket: bucket}
		if err := q.QueryRowContext(ctx, readableSQL, tenantID, bucket, pq.Array(SSECAlgorithms)).
			Scan(&r.Objects, &r.Versions, &r.Uploads); err != nil {
			return "", fmt.Errorf("bucket %s: count readable objects: %w", bucket, err)
		}
		if r.Total() > 0 {
			return "", r
		}
	}

	var vis sql.NullString
	if c.Visibility != nil {
		vis = sql.NullString{String: *c.Visibility, Valid: true}
	}
	var ssec sql.NullBool
	if c.SSECRequired != nil {
		ssec = sql.NullBool{Bool: *c.SSECRequired, Valid: true}
	}
	var mode, visibility string
	var ssecRequired bool
	err = q.QueryRowContext(ctx, `
		UPDATE buckets
		   SET visibility = COALESCE($3::text, visibility), ssec_required = COALESCE($4::boolean, ssec_required), updated_at = NOW()
		 WHERE tenant_id = $1 AND name = $2
		   AND NOT (COALESCE($3::text, visibility) = 'public-read' AND COALESCE($4::boolean, ssec_required))
		RETURNING mode, visibility, ssec_required`, tenantID, bucket, vis, ssec).Scan(&mode, &visibility, &ssecRequired)
	if errors.Is(err, sql.ErrNoRows) {
		var exists bool
		if err := q.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM buckets WHERE tenant_id = $1 AND name = $2)`,
			tenantID, bucket).Scan(&exists); err != nil {
			return "", fmt.Errorf("bucket %s: %w", bucket, err)
		}
		if exists {
			return "", fmt.Errorf("bucket %s: %w", bucket, ErrPublicSSEC)
		}
		return "", fmt.Errorf("bucket %s: %w", bucket, ErrNoBucket) // deleted meanwhile
	}
	if err != nil {
		return "", fmt.Errorf("set the mode of bucket %s: %w", bucket, err)
	}
	if mode != fromMode {
		if err := audit.RecordTx(ctx, q, audit.Entry{
			TenantID: tenantID, EventType: "bucket", Action: ActionModeChanged, Resource: bucket,
			Metadata: map[string]any{"bucket": bucket, "from": fromMode, "to": mode,
				"visibility": visibility, "ssec_required": ssecRequired},
		}); err != nil {
			return "", fmt.Errorf("bucket %s: %w", bucket, err)
		}
	}
	return mode, nil
}
