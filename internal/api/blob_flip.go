package api

// WP-R2-1 Part C (087): the pointer flip. A whole-object writer has already
// put its bytes at a NEW address (newBlobAddress); commitBlobWrite then runs
// ONE transaction, under the key's head-row lock and its version lock, that
//
//   - upserts the head row naming the new address (the caller's upsert),
//   - writes the version row of the new bytes with that address and its
//     encryption (versioned buckets) — recordObjectVersion's separate
//     transaction is gone for these writers, so a crash can no longer leave
//     a head row without its version row,
//   - decides the displaced blob's fate: a version row that keeps it keeps
//     it (Enabled: the previous version is now non-current and names its
//     bytes); otherwise it is retired (retired_blobs, 'overwritten'), never
//     deleted here,
//   - and the bill that goes with it (PR C2): a displaced blob a version
//     keeps stays billed — the head row's charge becomes the version's, so
//     the caller releases nothing for it (displacedRow.KeptByVersion); a
//     retained null version a Suspended write replaces is released
//     (displacedRow.ReplacedVersion) and its blob retired.
//
// A chunked object's manifest is displaced the same way (WP-VM-1): kept
// while a version names it, otherwise retired on the 'chunked' sentinel —
// never released in the flip, so a chunked GET in flight finishes and a
// pre-versioning chunked object is kept as the null version (PM-3).
//
// Nothing before the commit touches the old bytes, so a failure anywhere
// leaves the previous object exactly as it was (R2-05, R3-15); two
// concurrent writers each commit a row that describes its own blob (R2-15).

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/FairForge/vaultaire/internal/crypto"
	"github.com/FairForge/vaultaire/internal/modes"
)

// blobWrite is one writer's commit.
type blobWrite struct {
	TenantID, Bucket, Key string
	// Upsert writes the head row inside the flip's transaction. It must set
	// storage_key itself: the new address for a whole object, NULL for a
	// chunked one.
	Upsert func(tx *sql.Tx) error
	// Version, when set, is the version row of the written bytes, written
	// in the same transaction when the bucket is versioned.
	Version *versionWrite
	// Bill, when set, is settled in the same transaction, last, before
	// COMMIT (WP-R10-8 Part A): the reservation becomes the size the head
	// row records and the displaced bytes are released — no quota statement
	// runs after the commit. nil = an unbilled write (no quota manager).
	Bill *writeBill
	// DryRun runs every statement and rolls back: what the commit WOULD
	// displace and release, by the commit's own rule (predictRelease).
	DryRun bool
}

// versionWrite is what the version row of a write records.
type versionWrite struct {
	Size              int64
	ETag, ContentType string
	Backend           string
	Blob              versionBlobRef
}

// versionBlobRef is a version row's own blob: its address ("" = no bytes
// retained) and what it is sealed with.
type versionBlobRef struct {
	StorageKey string
	Enc        objectEncryption
}

// flipResult is what the flip found and wrote.
type flipResult struct {
	Displaced displacedRow
	VersionID string // "" for an unversioned bucket
}

// headFlipHook runs inside the flip's transaction after the head-row upsert;
// an error rolls the whole write back (tests: a failure after the backend
// write).
var headFlipHook = func(tenantID, bucket, key string) error { return nil }

// blobCommitStartHook runs when a commit starts, before its first lock
// (tests: a mode change between a write's placement and its commit).
var blobCommitStartHook = func(tenantID, bucket, key string) {}

// errSSECRequiredAtCommit: the bucket required a customer key by the time
// the write committed, and the row it would write is not sealed with one
// (RP33-1 b). The client's 400, as refuseSSECRequired at placement; the
// caller queues the blob as an orphan like any failed commit.
var errSSECRequiredAtCommit = errors.New("the bucket requires a customer-provided key (ssec_required) and the write carries none")

// lockBucketModeShared takes the bucket-mode lock SHARED (modes.ModeLockSQL
// is the door's exclusive one) and reads ssec_required under it: a mode
// change either committed before this (the flag is read here) or waits for
// this commit (and then counts what it wrote). No bucket row = no rule.
func lockBucketModeShared(ctx context.Context, tx *sql.Tx, tenantID, bucket string) (ssecRequired bool, err error) {
	if _, err := tx.ExecContext(ctx, modes.ModeLockSharedSQL, tenantID, bucket); err != nil {
		return false, fmt.Errorf("lock the bucket's mode: %w", err)
	}
	err = tx.QueryRowContext(ctx, `SELECT ssec_required FROM buckets WHERE tenant_id = $1 AND name = $2`,
		tenantID, bucket).Scan(&ssecRequired)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read the bucket's ssec_required: %w", err)
	}
	return ssecRequired, nil
}

// commitBlobWrite is the flip (see the file comment). Lock order, the same
// on every path that takes more than one (WP-R10-8; usage/ledger_tx.go):
// the bucket-mode lock (SHARED here, exclusive in the door modes.SetBucket —
// RP33-1) → the key's advisory lock → the head row → the key's version lock
// and rows → the upload row (a Complete's markCompleted / Held) →
// tenant_quotas → tenant_floor_quotas. The ledger is the last statement
// before COMMIT, so the tenant's quota row is held from its UPDATE to COMMIT
// only, and nothing holding it waits for a key, row or upload lock. The door
// holds the mode lock and then the buckets row only; nothing here locks a
// buckets row, so the two never wait on each other in a cycle.
//
// Every commit of a head row or a version row with new bytes runs here
// (TestBlobRowWritersOnlyThroughTheFlip): PutObject whole and chunked,
// CompleteMultipartUpload whole and chunked (UploadPartCopy's parts land
// with it), CopyObject whole and chunked, generated objects, the direct
// upload's complete.
func commitBlobWrite(ctx context.Context, db *sql.DB, w blobWrite) (flipResult, error) {
	blobCommitStartHook(w.TenantID, w.Bucket, w.Key)
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return flipResult{}, fmt.Errorf("begin head-cache upsert: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// A bucket made Private after this write was placed must not take its
	// unkeyed row (RP33-1 b): the flag is read under the mode lock and the
	// written row is checked below, before anything else is decided.
	ssecRequired, err := lockBucketModeShared(ctx, tx, w.TenantID, w.Bucket)
	if err != nil {
		return flipResult{}, err
	}

	// Serialise writers of the same key BEFORE the probe. A row that does not
	// exist yet cannot be locked by FOR UPDATE, so two concurrent first
	// writers of a new key both saw "no previous row", both reported
	// displaced = 0, and the loser's reserved bytes were never released
	// (Review R15-17, TestAtomicHeadUpsert_ConcurrentFirstWritersAccountOnce).
	if err := lockHeadKey(ctx, tx, w.TenantID, w.Bucket, w.Key); err != nil {
		return flipResult{}, err
	}
	vStatus := ""
	if w.Version != nil {
		if err := lockVersionKey(ctx, tx, w.TenantID, w.Bucket, w.Key); err != nil {
			return flipResult{}, err
		}
		vStatus = bucketVersioningStatus(ctx, tx, w.TenantID, w.Bucket)
		// An object written before versioning was enabled is the key's
		// "null" version (ListObjectVersions lists it so): an Enabled write
		// over it keeps it as one, read from the head row before the upsert
		// replaces it (Stage 1a Prompt 3, Part 0).
		if vStatus == "Enabled" {
			if err := anchorNullVersion(ctx, tx, w.TenantID, w.Bucket, w.Key); err != nil {
				return flipResult{}, err
			}
		}
	}

	var displaced displacedRow
	err = tx.QueryRowContext(ctx, `
		SELECT size_bytes, floor, COALESCE(backend_name, ''), is_chunked, COALESCE(storage_key, ''),
		       etag, COALESCE(encryption_algorithm, ''), encryption_key_version
		FROM object_head_cache
		WHERE tenant_id = $1 AND bucket = $2 AND object_key = $3
		FOR UPDATE`,
		w.TenantID, w.Bucket, w.Key).Scan(&displaced.Size, &displaced.Floor, &displaced.Backend, &displaced.Chunked,
		&displaced.StorageKey, &displaced.etag, &displaced.enc.Algorithm, &displaced.enc.KeyVersion)
	switch {
	case err == nil:
		displaced.Found = true
	case errors.Is(err, sql.ErrNoRows):
	default:
		return flipResult{}, fmt.Errorf("lock head-cache row: %w", err)
	}

	// A displaced chunked row's manifest is NOT released here (WP-VM-1): its
	// fate is decided below, with a whole blob's — kept by a version that
	// names it, otherwise retired for the collector.
	if err := w.Upsert(tx); err != nil {
		return flipResult{}, fmt.Errorf("upsert head-cache row: %w", err)
	}
	if !w.DryRun {
		if err := headFlipHook(w.TenantID, w.Bucket, w.Key); err != nil {
			return flipResult{}, err
		}
	}
	var newChunked bool
	var newSK, newFloor, newEnc string
	var newSize int64
	if err := tx.QueryRowContext(ctx, `SELECT is_chunked, COALESCE(storage_key, ''), floor, size_bytes, COALESCE(encryption_algorithm, '') FROM object_head_cache
		WHERE tenant_id = $1 AND bucket = $2 AND object_key = $3`, w.TenantID, w.Bucket, w.Key).Scan(&newChunked, &newSK, &newFloor, &newSize, &newEnc); err != nil {
		return flipResult{}, fmt.Errorf("read the written head row: %w", err)
	}
	if ssecRequired && !w.DryRun && !crypto.IsSSEC(newEnc) {
		return flipResult{}, errSSECRequiredAtCommit
	}
	// The displaced row's bytes and the new row's: an address and a kind
	// (a whole blob, or a manifest — WP-VM-1). <key> for a row written
	// before 087 (whole) or 092 (chunked).
	dAddr, nAddr := "", blobArtifact(w.Key, newSK)
	if displaced.Found {
		dAddr = blobArtifact(w.Key, displaced.StorageKey)
	}
	sameBytes := displaced.Found && dAddr == nAddr && displaced.Chunked == newChunked

	var res flipResult
	if w.Version != nil {
		if vStatus == "Enabled" || vStatus == "Suspended" {
			if displaced.Found {
				if err := anchorDisplacedVersion(ctx, tx, w.TenantID, w.Bucket, w.Key, displaced); err != nil {
					return flipResult{}, err
				}
			}
			var replaced replacedNullVersion
			if vStatus == "Suspended" {
				if replaced, err = retainedNullVersion(ctx, tx, w.TenantID, w.Bucket, w.Key, dAddr, displaced.Chunked, displaced.Floor); err != nil {
					return flipResult{}, err
				}
			}
			v := w.Version
			blob := v.Blob
			backend := v.Backend
			if newChunked {
				// A chunked version names its manifest (WP-VM-1).
				blob, backend = versionBlobRef{StorageKey: newSK, Enc: objectEncryption{Algorithm: newEnc}}, chunkedBackendSentinel
			}
			res.VersionID, err = writeVersionRow(ctx, tx, vStatus, w.TenantID, w.Bucket, w.Key, v.Size, v.ETag, v.ContentType, backend, newFloor, blob)
			if err != nil {
				return flipResult{}, fmt.Errorf("write version row: %w", err)
			}
			// The null version this write replaced was a retained
			// non-current version (not the displaced current object, whose
			// fate is below): its bytes are no longer named and no longer
			// billed.
			if replaced.storageKey != "" {
				if err := retireRowBytes(ctx, tx, w.TenantID, w.Bucket, w.Key, replaced.storageKey, replaced.backend, replaced.chunked, retiredOverwritten); err != nil &&
					!errors.Is(err, errUnaddressableBlob) {
					return flipResult{}, err
				}
				displaced.ReplacedVersion = quotaRelease{Floor: replaced.floor, Bytes: replaced.size}
			}
		}
	}

	// The displaced blob or manifest: kept while a row names it (a version,
	// the new row itself when the address is the same — a concurrent
	// complete of the same upload), retired otherwise. A whole blob with no
	// recorded backend cannot be addressed by the collector: left for the
	// erasure sweep. Bytes a version keeps keep their bill (the version's
	// now).
	if displaced.Found && !sameBytes {
		var kept bool
		if displaced.Chunked {
			kept, err = versionKeepsManifest(ctx, tx, w.TenantID, w.Bucket, w.Key, dAddr)
		} else {
			kept, err = versionKeepsBlob(ctx, tx, w.TenantID, w.Bucket, w.Key, dAddr)
		}
		if err != nil {
			return flipResult{}, err
		}
		displaced.KeptByVersion = kept
		if err := retireRowBytes(ctx, tx, w.TenantID, w.Bucket, w.Key, dAddr, displaced.Backend, displaced.Chunked, retiredOverwritten); err != nil &&
			!errors.Is(err, errUnaddressableBlob) {
			return flipResult{}, err
		}
	}
	// A promoted copy of an older object of the key (a Sia object hydrated
	// into the primary, sia_promotion.go) is never served again: retired
	// with the write.
	if err := retireSiaPromotionsOnWrite(ctx, tx, w.TenantID, w.Bucket, w.Key, nAddr); err != nil {
		return flipResult{}, err
	}
	if w.DryRun {
		res.Displaced = displaced
		return res, nil // rolled back by the deferred Rollback
	}
	// The bill, last: the tenant's quota rows are locked from here to COMMIT.
	if w.Bill != nil {
		if err := w.Bill.settleTx(ctx, tx, w.TenantID, newSize, newFloor, displaced); err != nil {
			return flipResult{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		return flipResult{}, fmt.Errorf("commit head-cache upsert: %w", err)
	}
	res.Displaced = displaced
	return res, nil
}

// lockHeadKey takes the transaction-scoped advisory lock every head-row
// writer of a key holds.
func lockHeadKey(ctx context.Context, tx *sql.Tx, tenantID, bucket, key string) error {
	if _, err := tx.ExecContext(ctx,
		`SELECT pg_advisory_xact_lock(hashtext($1 || '/' || $2 || '/' || $3)::bigint)`,
		tenantID, bucket, key); err != nil {
		return fmt.Errorf("lock head-cache key: %w", err)
	}
	return nil
}

// anchorDisplacedVersion makes the latest version row that describes the
// displaced head row name its blob: a row a 2C writer wrote already does
// (re-asserted with the head row's backend and encryption — a demotion may
// have moved it since); a version row of a blob written in place before 2C
// (storage_key NULL, the bytes at <key>) gets <key> as its address, so the
// bytes the next write no longer overwrites stay that version's. Only when
// the etags agree: a version row that describes other bytes is left as it is.
func anchorDisplacedVersion(ctx context.Context, tx *sql.Tx, tenantID, bucket, key string, d displacedRow) error {
	addr := blobArtifact(key, d.StorageKey)
	backend := d.Backend
	if d.Chunked {
		// A chunked version names its manifest (WP-VM-1): a version a chunked
		// write recorded before 092 named none, and the manifest is at <key>.
		backend = chunkedBackendSentinel
	}
	// The floor too (PR C2): the version is billed where the head row was.
	if _, err := tx.ExecContext(ctx, `
		UPDATE object_versions SET storage_key = $4, backend_name = NULLIF($5, ''),
		       encryption_algorithm = NULLIF($6, ''), encryption_key_version = $7, floor = $9
		WHERE tenant_id = $1 AND bucket = $2 AND object_key = $3 AND is_latest AND NOT is_delete_marker
		  AND etag = $8 AND (storage_key IS NULL OR storage_key = $4)`,
		tenantID, bucket, key, addr, backend, d.enc.Algorithm, d.enc.KeyVersion, d.etag, d.Floor); err != nil {
		return fmt.Errorf("anchor the displaced version's blob: %w", err)
	}
	return nil
}

// anchorNullVersion makes the key's current object its "null" version when
// the key has no version row at all — an object written while the bucket
// was unversioned (or before versioning existed), which AWS keeps as the
// null version once versioning is Enabled and ListObjectVersions already
// lists as one. Called under the head-row lock and the version lock, before
// a write or a delete marker displaces the head row: the version row then
// names the blob (COALESCE(storage_key, object_key): <key> for a row older
// than 086), so the flip keeps it and its bill carries over
// (displacedRow.KeptByVersion). A chunked head row is anchored too (WP-VM-1,
// PM-3): the version names its manifest (backend 'chunked', the head row's
// address, <key> for a manifest written before 092). The row is is_latest;
// the caller's version write or marker makes it non-current. The head row's
// attributes go with it (R61-8: a GET of the version sends them).
func anchorNullVersion(ctx context.Context, tx *sql.Tx, tenantID, bucket, key string) error {
	if _, err := tx.ExecContext(ctx, anchorNullVersionSQL, tenantID, bucket, key); err != nil {
		return fmt.Errorf("anchor the pre-versioning object as the null version: %w", err)
	}
	return nil
}

const anchorNullVersionSQL = `
	INSERT INTO object_versions
		(tenant_id, bucket, object_key, version_id, size_bytes, etag, content_type, is_latest, is_delete_marker,
		 backend_name, storage_key, encryption_algorithm, encryption_key_version, floor, created_at,
		 metadata, content_disposition, content_encoding, content_language, cache_control, http_expires, website_redirect_location)
	SELECT h.tenant_id, h.bucket, h.object_key, 'null', h.size_bytes, h.etag, h.content_type, TRUE, FALSE,
	       CASE WHEN h.is_chunked THEN '` + chunkedBackendSentinel + `' ELSE h.backend_name END,
	       COALESCE(h.storage_key, h.object_key), NULLIF(h.encryption_algorithm, ''), h.encryption_key_version,
	       h.floor, h.updated_at,
	       h.metadata, h.content_disposition, h.content_encoding, h.content_language, h.cache_control, h.http_expires, h.website_redirect_location
	FROM object_head_cache h
	WHERE h.tenant_id = $1 AND h.bucket = $2 AND h.object_key = $3
	  AND NOT EXISTS (SELECT 1 FROM object_versions v
	                  WHERE v.tenant_id = $1 AND v.bucket = $2 AND v.object_key = $3)`

// replacedNullVersion is the retained null version a Suspended write is
// about to replace.
type replacedNullVersion struct {
	storageKey, backend, floor string
	size                       int64
	chunked                    bool // its address names a manifest (WP-VM-1)
}

// retainedNullVersion reads the key's "null" version when it keeps bytes
// OTHER than the displaced current object's (dAddr, of kind dChunked): a
// non-current version the Suspended write's null-row rewrite replaces. Its
// floor as the bill knows it (usage.RetainedVersionFloor; the head row read
// is the displaced one, under the same lock).
func retainedNullVersion(ctx context.Context, tx *sql.Tx, tenantID, bucket, key, dAddr string, dChunked bool, headFloor string) (replacedNullVersion, error) {
	var r replacedNullVersion
	err := tx.QueryRowContext(ctx, `
		SELECT storage_key, COALESCE(backend_name, ''), size_bytes,
		       COALESCE(floor, NULLIF($5, ''), CASE WHEN backend_name = 'geyser' THEN 'vault' ELSE 'standard' END),
		       COALESCE(backend_name, '') = '`+chunkedBackendSentinel+`'
		FROM object_versions
		WHERE tenant_id = $1 AND bucket = $2 AND object_key = $3 AND version_id = 'null'
		  AND NOT is_delete_marker AND storage_key IS NOT NULL
		  AND NOT (storage_key = $4 AND (COALESCE(backend_name, '') = '`+chunkedBackendSentinel+`') = $6)`,
		tenantID, bucket, key, dAddr, headFloor, dChunked).Scan(&r.storageKey, &r.backend, &r.size, &r.floor, &r.chunked)
	if errors.Is(err, sql.ErrNoRows) {
		return replacedNullVersion{}, nil
	}
	if err != nil {
		return replacedNullVersion{}, fmt.Errorf("read the null version: %w", err)
	}
	return r, nil
}
