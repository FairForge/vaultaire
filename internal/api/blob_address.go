package api

// WP-R2-1, the "expand" half (Stage 1a Prompt 2B, migration 086): a whole
// object's bytes live at the address its row records —
// object_head_cache.storage_key (object_versions.storage_key for a retained
// version, smart_demotions.storage_key for the copies a demotion made) — and
// NULL means "<key>", for every row written before 2C, for ever. Every reader
// of whole-object bytes resolves the address through objectBlobAddress; the
// engine's routing map (HintBackend, objectBackends, object_locations) is
// keyed by (container, artifact), so a hint names the address too.
//
// No writer sets an address yet: this build writes in place at <key>, as
// every build before it. What it does for a row a 2C slot wrote (the two-slot
// deploy runs both for up to 15 min): an in-place write clears the address
// on the row it replaces and queues the displaced blob in retired_blobs
// (atomicHeadUpsertReleasing); a delete removes the blob the row names.
// TestWholeObjectReads_GoThroughTheBlobAddress keeps every read on this path.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// objectBlobAddress is where a whole object's bytes are: the engine container
// of the bucket and the artifact — storageKey when the row records one, else
// the object key. storageKey is the column read as COALESCE(storage_key, ”)
// (086 forbids ” as an address).
func objectBlobAddress(tenantID, bucket, key, storageKey string) (container, artifact string) {
	container = tenantID + "_" + bucket
	if storageKey != "" {
		return container, storageKey
	}
	return container, key
}

// blobArtifact is the artifact half of objectBlobAddress, for callers whose
// container is already in hand.
func blobArtifact(key, storageKey string) string {
	_, a := objectBlobAddress("", "", key, storageKey)
	return a
}

// Reasons a blob waits in retired_blobs.
const (
	retiredOverwritten    = "overwritten"     // a write replaced the row that named it
	retiredVersionDeleted = "version_deleted" // DELETE ?versionId of a version that kept bytes
)

// retiredBlobGrace is how long a displaced blob stays readable after the row
// that named it changed: a GET that resolved the old row a moment before may
// still be streaming it (a multi-GiB body at a slow client's pace), so the
// grace is generous — storage for a day costs nothing next to a cut download.
// The collector that deletes after it is 2C's.
const retiredBlobGrace = 24 * time.Hour

// queryRower is what an address lookup needs: a *sql.DB or a *sql.Tx.
type queryRower interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// blobStillNamed reports whether any row still names the blob at
// (tenant, bucket, storageKey): the head row, or a version row that keeps
// its bytes. Such a blob is never retired or deleted by a path that only
// changed one of the rows.
func blobStillNamed(ctx context.Context, q queryRower, tenantID, bucket, key, storageKey string) (bool, error) {
	var named bool
	err := q.QueryRowContext(ctx, `
		SELECT EXISTS (SELECT 1 FROM object_head_cache
		                WHERE tenant_id = $1 AND bucket = $2 AND object_key = $3 AND storage_key = $4)
		    OR EXISTS (SELECT 1 FROM object_versions
		                WHERE tenant_id = $1 AND bucket = $2 AND object_key = $3 AND storage_key = $4)`,
		tenantID, bucket, key, storageKey).Scan(&named)
	if err != nil {
		return false, fmt.Errorf("is blob %s still named: %w", storageKey, err)
	}
	return named, nil
}

// retireBlob queues one displaced whole-object blob for the collector, inside
// the caller's transaction, unless a row still names it. A blob with no
// recorded backend cannot be addressed by the collector: it is left to the
// erasure sweep and reported as an error (the caller logs it).
func retireBlob(ctx context.Context, tx *sql.Tx, tenantID, bucket, key, storageKey, backend, reason string) error {
	if storageKey == "" {
		return nil // the blob is at <key>: an in-place write overwrote it
	}
	named, err := blobStillNamed(ctx, tx, tenantID, bucket, key, storageKey)
	if err != nil || named {
		return err
	}
	if backend == "" {
		return errUnaddressableBlob
	}
	container, _ := objectBlobAddress(tenantID, bucket, key, storageKey)
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO retired_blobs (backend_id, container, storage_key, tenant_id, reason, retire_after)
		VALUES ($1, $2, $3, $4, $5, NOW() + make_interval(secs => $6))
		ON CONFLICT (backend_id, container, storage_key) DO UPDATE SET reason = EXCLUDED.reason, retire_after = EXCLUDED.retire_after`,
		backend, container, storageKey, tenantID, reason, retiredBlobGrace.Seconds()); err != nil {
		return fmt.Errorf("retire blob %s: %w", storageKey, err)
	}
	return nil
}

// retireBlobNow is retireBlob in a transaction of its own (a path whose row
// change has already committed: the delete-marker branch).
func retireBlobNow(ctx context.Context, db *sql.DB, tenantID, bucket, key, storageKey, backend, reason string) error {
	if storageKey == "" || db == nil {
		return nil
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin retire: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := retireBlob(ctx, tx, tenantID, bucket, key, storageKey, backend, reason); err != nil {
		return err
	}
	return tx.Commit()
}

// versionKeepsBlob reports whether a version row keeps the blob at
// storageKey: a delete of the head row must then leave the bytes.
func versionKeepsBlob(ctx context.Context, q queryRower, tenantID, bucket, key, storageKey string) (bool, error) {
	var kept bool
	err := q.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM object_versions
		WHERE tenant_id = $1 AND bucket = $2 AND object_key = $3 AND storage_key = $4 AND NOT is_delete_marker)`,
		tenantID, bucket, key, storageKey).Scan(&kept)
	if err != nil {
		return false, fmt.Errorf("does a version keep blob %s: %w", storageKey, err)
	}
	return kept, nil
}

// errUnaddressableBlob: a displaced blob whose row recorded no backend.
var errUnaddressableBlob = errors.New("displaced blob has no recorded backend: left for the erasure sweep")
