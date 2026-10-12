package api

// WP-R2-1: a whole object's bytes live at the address its row records —
// object_head_cache.storage_key (object_versions.storage_key for a retained
// version, smart_demotions.storage_key for the copies a demotion made) — and
// NULL means "<key>", for every row written before 2C, for ever. Every reader
// of whole-object bytes resolves the address through objectBlobAddress (Part
// B, 086); the engine's routing map (HintBackend, objectBackends,
// object_locations) is keyed by (container, artifact), so a hint names the
// address too.
//
// The writers (Part C, 087): every whole-object write — PUT, multipart
// complete, CopyObject, generated objects, direct-upload complete — puts its
// bytes at a NEW address, newBlobAddress(key) = <key>@<26 base32 chars>, and
// one transaction (commitBlobWrite, blob_flip.go) flips the head row and the
// version row to it. The blob it displaced is RETIRED (retired_blobs), never
// overwritten: kept while a version row names it, deleted by the collector
// (retired_blobs.go) only after its grace and a re-check that no row names
// it. Nothing else deletes a blob a row may name; a write that failed after
// its bytes landed queues them as orphan_write. TestWholeObjectReads_
// GoThroughTheBlobAddress keeps every read and write on this path.

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/FairForge/vaultaire/internal/drivers"
	"go.uber.org/zap"
)

// blobIDAlphabet is Crockford's base32, lower case: no i, l, o, u — an id
// reads unambiguously and never needs escaping on S3, Sync's davName or
// OneDrive.
const blobIDAlphabet = "0123456789abcdefghjkmnpqrstvwxyz"

// blobIDRand is the id's randomness (crypto/rand; a test pins it).
var blobIDRand io.Reader = rand.Reader

// blobIDLen is the length of an address's id: 26 × 5 = 130 random bits.
const blobIDLen = 26

// blobAddressSuffixLen is what an address adds to its key: "@" + the id.
const blobAddressSuffixLen = 1 + blobIDLen

// newBlobAddress is the address a whole-object write puts its bytes at:
// key + "@" + 26 lower-case Crockford base32 characters from crypto/rand
// (130 bits). Independent of the version id (an unversioned bucket's write
// gets one too) and never reused: two writes of a key never share a blob, so
// a write can neither destroy the bytes a row names nor be destroyed by a
// delete aimed at an older blob (R2-05, R2-15, R3-15, R13-06).
func newBlobAddress(key string) (string, error) {
	var b [17]byte // 136 bits; the first 130 are used
	if _, err := io.ReadFull(blobIDRand, b[:]); err != nil {
		return "", fmt.Errorf("generate blob address: %w", err)
	}
	id := make([]byte, blobIDLen)
	var acc uint64
	bits, j := 0, 0
	for i := 0; i < blobIDLen; i++ {
		for bits < 5 {
			acc = acc<<8 | uint64(b[j])
			j++
			bits += 8
		}
		bits -= 5
		id[i] = blobIDAlphabet[(acc>>uint(bits))&31]
	}
	return key + "@" + string(id), nil
}

// blobAddressFor is the address a writer uses: newBlobAddress, or the key
// itself when the process has no database. An address lives only in a row;
// without one (dev and test servers) GET, LIST and DELETE address the bytes
// by the key, so the bytes must be there.
func blobAddressFor(db *sql.DB, key string) (string, error) {
	if db == nil {
		return key, nil
	}
	return newBlobAddress(key)
}

// isBlobAddressOf reports whether addr is exactly key + "@" + a 26-character
// id of newBlobAddress's alphabet — the only address a client may hand back
// (direct-upload complete).
func isBlobAddressOf(key, addr string) bool {
	id, ok := strings.CutPrefix(addr, key+"@")
	if !ok || len(id) != blobIDLen {
		return false
	}
	for i := 0; i < len(id); i++ {
		if strings.IndexByte(blobIDAlphabet, id[i]) < 0 {
			return false
		}
	}
	return true
}

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
	retiredOrphanWrite    = "orphan_write"    // a write whose bytes landed and whose row never committed
)

// retiredBlobGrace is how long a displaced blob stays readable after the row
// that named it changed: a GET that resolved the old row a moment before may
// still be streaming it (a multi-GiB body at a slow client's pace), so the
// grace is generous — storage for a day costs nothing next to a cut download.
// The collector (retired_blobs.go) deletes after it.
const retiredBlobGrace = 24 * time.Hour

// queryRower is what an address lookup needs: a *sql.DB or a *sql.Tx.
type queryRower interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// blobNaming is who still names a blob: a committed row (head row, a version
// that keeps bytes, an open demotion ledger row, the newest version behind a
// delete marker for a blob at <key>), or only an ACTIVE multipart upload
// that chose the address for its complete (087) — whose head row may be
// about to commit it.
type blobNaming int

const (
	blobFree        blobNaming = iota // no row names it: the collector may delete it
	blobNamed                         // a committed row names it: never deleted
	blobUploadOwned                   // only an active upload names it: kept, asked again later
)

// blobNamedBy reports who still names the blob at artifact (an address, or
// the object key itself for a blob at <key>) of (tenant, bucket, key). It is
// deliberately backend-agnostic: a row naming the address on ANY backend
// keeps it (a leaked copy costs money; a deleted one costs a customer's
// bytes).
func blobNamedBy(ctx context.Context, q queryRower, tenantID, bucket, key, artifact string) (blobNaming, error) {
	var named, upload bool
	err := q.QueryRowContext(ctx, `
		SELECT EXISTS (SELECT 1 FROM object_head_cache
		                WHERE tenant_id = $1 AND bucket = $2 AND object_key = $3
		                  AND COALESCE(storage_key, object_key) = $4 AND NOT is_chunked)
		    OR EXISTS (SELECT 1 FROM object_versions
		                WHERE tenant_id = $1 AND bucket = $2 AND object_key = $3
		                  AND storage_key = $4 AND NOT is_delete_marker)
		    -- the blob at <key> of the newest live version behind a delete
		    -- marker (versionBytesIntact serves it): kept, the undelete path
		    OR ($4 = $3 AND EXISTS (SELECT 1 FROM object_versions
		                WHERE tenant_id = $1 AND bucket = $2 AND object_key = $3
		                  AND is_latest AND is_delete_marker))
		    OR EXISTS (SELECT 1 FROM smart_demotions
		                WHERE tenant_id = $1 AND bucket = $2 AND object_key = $3
		                  AND COALESCE(storage_key, object_key) = $4 AND hot_outcome IN ('', 'delete_failed')),
		       EXISTS (SELECT 1 FROM multipart_uploads
		                WHERE tenant_id = $1 AND bucket = $2 AND object_key = $3
		                  AND storage_key = $4 AND status = 'active')`,
		tenantID, bucket, key, artifact).Scan(&named, &upload)
	if err != nil {
		return blobFree, fmt.Errorf("is blob %s still named: %w", artifact, err)
	}
	switch {
	case named:
		return blobNamed, nil
	case upload:
		return blobUploadOwned, nil
	}
	return blobFree, nil
}

// blobStillNamed reports whether any row still names the blob at
// (tenant, bucket, storageKey) — blobNamedBy, with an active upload's address
// counted as named. Such a blob is never retired or deleted by a path that
// only changed one of the rows.
func blobStillNamed(ctx context.Context, q queryRower, tenantID, bucket, key, storageKey string) (bool, error) {
	n, err := blobNamedBy(ctx, q, tenantID, bucket, key, storageKey)
	return n != blobFree, err
}

// retireBlob queues one displaced whole-object blob for the collector, inside
// the caller's transaction, unless a row still names it. storageKey is the
// blob's address; "" means the blob is at <key> and nothing is queued (a
// version without bytes, a legacy head row a path leaves in place) — a
// writer that displaces a blob at <key> passes the key itself. A blob with
// no recorded backend cannot be addressed by the collector: it is left to
// the erasure sweep and reported as an error (the caller logs it).
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

// retireOrphanWrite queues the bytes of a write that failed after they
// landed — a short body, a bad digest, a head-row transaction that did not
// commit — for the collector, due at once. Never a direct delete: a commit
// that reported an error may still have committed, and the collector's
// re-check (blobNamedBy) is what keeps a blob a row names. On its own
// context: the request's may be gone. A failure here leaks the blob (the
// erasure sweep removes it with the account), logged at Error.
func retireOrphanWrite(ctx context.Context, db *sql.DB, logger *zap.Logger, tenantID, bucket, addr, backend string) {
	if db == nil || addr == "" {
		return
	}
	if backend == "" {
		logger.Error("orphan write not queued: the backend that took it is unknown — left for the erasure sweep",
			zap.String("tenant_id", tenantID), zap.String("bucket", bucket), zap.String("storage_key", addr))
		return
	}
	qctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	container, _ := objectBlobAddress(tenantID, bucket, "", addr)
	if _, err := db.ExecContext(qctx, `
		INSERT INTO retired_blobs (backend_id, container, storage_key, tenant_id, reason, retire_after)
		VALUES ($1, $2, $3, $4, $5, NOW())
		ON CONFLICT (backend_id, container, storage_key) DO UPDATE SET reason = EXCLUDED.reason, retire_after = EXCLUDED.retire_after`,
		backend, container, addr, tenantID, retiredOrphanWrite); err != nil {
		logger.Error("orphan write not queued for the collector — left for the erasure sweep",
			zap.Error(err), zap.String("tenant_id", tenantID), zap.String("bucket", bucket),
			zap.String("storage_key", addr), zap.String("backend", backend))
	}
}

// maxObjectKeyBytes is the longest key a write accepts (Decision 1 of
// docs/reviews/WP-R2-1.md): S3's 1,024-byte object key, less the worst-case
// fixed-bucket prefix t-<36-char tenant>/<tenant>_<63-char bucket>/ (140
// bytes), less the address suffix @<26> (27 bytes).
const maxObjectKeyBytes = 1024 - 140 - blobAddressSuffixLen

// blobKeyTooLong reports whether key cannot carry its blob address on the
// bucket's backend, with the reason a client reads: longer than
// maxObjectKeyBytes anywhere, or — for a bucket on the Sync tier — a folder
// segment or the leaf plus the address over the bridge's per-name limit
// (the WebDAV driver's own mapping). Checked before any body byte is read.
// The tier is read from the bucket row; a bucket that cannot be read is
// judged by the byte limit only (the write fails later for the same reason).
func blobKeyTooLong(ctx context.Context, db *sql.DB, tenantID, bucket, key string) (string, bool) {
	if len(key) > maxObjectKeyBytes {
		return fmt.Sprintf("Object keys are limited to %d bytes on this service (this key is %d).", maxObjectKeyBytes, len(key)), true
	}
	if bucketTierStorageClass(ctx, db, tenantID, bucket) != "SYNC" {
		return "", false
	}
	if drivers.WebDAVArtifactFits(key+"@"+strings.Repeat("0", blobIDLen)) != nil {
		return "This bucket's storage limits each part of a key between slashes to about 248 characters once encoded, " +
			"and the last part to about 219: shorten the key.", true
	}
	return "", false
}
