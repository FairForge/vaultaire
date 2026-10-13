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
// Nothing before the commit touches the old bytes, so a failure anywhere
// leaves the previous object exactly as it was (R2-05, R3-15); two
// concurrent writers each commit a row that describes its own blob (R2-15).

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
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

// commitBlobWrite is the flip (see the file comment). Lock order: the head
// row's advisory lock, then the version lock — every path that takes both
// takes them in this order (deleteVersion too).
func commitBlobWrite(ctx context.Context, db *sql.DB, gci chunkManifestReleaser, w blobWrite) (flipResult, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return flipResult{}, fmt.Errorf("begin head-cache upsert: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

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

	if displaced.Chunked && gci != nil {
		if relErr := gci.DeleteObjectChunksTx(ctx, tx, w.TenantID, w.Bucket, w.Key); relErr != nil {
			return flipResult{}, fmt.Errorf("release displaced chunk manifest: %w", relErr)
		}
	}

	if err := w.Upsert(tx); err != nil {
		return flipResult{}, fmt.Errorf("upsert head-cache row: %w", err)
	}
	if err := headFlipHook(w.TenantID, w.Bucket, w.Key); err != nil {
		return flipResult{}, err
	}
	var newChunked bool
	var newSK, newFloor string
	if err := tx.QueryRowContext(ctx, `SELECT is_chunked, COALESCE(storage_key, ''), floor FROM object_head_cache
		WHERE tenant_id = $1 AND bucket = $2 AND object_key = $3`, w.TenantID, w.Bucket, w.Key).Scan(&newChunked, &newSK, &newFloor); err != nil {
		return flipResult{}, fmt.Errorf("read the written head row: %w", err)
	}
	dAddr, nAddr := "", ""
	if displaced.Found && !displaced.Chunked {
		dAddr = blobArtifact(w.Key, displaced.StorageKey)
	}
	if !newChunked {
		nAddr = blobArtifact(w.Key, newSK)
	}

	var res flipResult
	if w.Version != nil {
		if vStatus == "Enabled" || vStatus == "Suspended" {
			if dAddr != "" {
				if err := anchorDisplacedVersion(ctx, tx, w.TenantID, w.Bucket, w.Key, displaced); err != nil {
					return flipResult{}, err
				}
			}
			var replaced replacedNullVersion
			if vStatus == "Suspended" {
				if replaced, err = retainedNullVersion(ctx, tx, w.TenantID, w.Bucket, w.Key, dAddr, displaced.Floor); err != nil {
					return flipResult{}, err
				}
			}
			v := w.Version
			res.VersionID, err = writeVersionRow(ctx, tx, vStatus, w.TenantID, w.Bucket, w.Key, v.Size, v.ETag, v.ContentType, v.Backend, newFloor, v.Blob)
			if err != nil {
				return flipResult{}, fmt.Errorf("write version row: %w", err)
			}
			// The null version this write replaced was a retained
			// non-current version (not the displaced current object, whose
			// fate is below): its bytes are no longer named and no longer
			// billed.
			if replaced.storageKey != "" {
				if err := retireBlob(ctx, tx, w.TenantID, w.Bucket, w.Key, replaced.storageKey, replaced.backend, retiredOverwritten); err != nil &&
					!errors.Is(err, errUnaddressableBlob) {
					return flipResult{}, err
				}
				displaced.ReplacedVersion = quotaRelease{Floor: replaced.floor, Bytes: replaced.size}
			}
		}
	}

	// The displaced blob: kept while a row names it (a version, the new
	// row itself when the address is the same — a concurrent complete of the
	// same upload), retired otherwise. A blob with no recorded backend
	// cannot be addressed by the collector: left for the erasure sweep. A
	// blob a version keeps keeps its bill (the version's now).
	if dAddr != "" && dAddr != nAddr {
		kept, err := versionKeepsBlob(ctx, tx, w.TenantID, w.Bucket, w.Key, dAddr)
		if err != nil {
			return flipResult{}, err
		}
		displaced.KeptByVersion = kept
		if err := retireBlob(ctx, tx, w.TenantID, w.Bucket, w.Key, dAddr, displaced.Backend, retiredOverwritten); err != nil &&
			!errors.Is(err, errUnaddressableBlob) {
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
	// The floor too (PR C2): the version is billed where the head row was.
	if _, err := tx.ExecContext(ctx, `
		UPDATE object_versions SET storage_key = $4, backend_name = NULLIF($5, ''),
		       encryption_algorithm = NULLIF($6, ''), encryption_key_version = $7, floor = $9
		WHERE tenant_id = $1 AND bucket = $2 AND object_key = $3 AND is_latest AND NOT is_delete_marker
		  AND etag = $8 AND (storage_key IS NULL OR storage_key = $4)`,
		tenantID, bucket, key, addr, d.Backend, d.enc.Algorithm, d.enc.KeyVersion, d.etag, d.Floor); err != nil {
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
// (displacedRow.KeptByVersion). A chunked head row is not anchored: a
// manifest has no version-addressed form yet (it is released, as before —
// docs/reviews/WP-R2-1.md, post-merge review). The row is is_latest; the
// caller's version write or marker makes it non-current.
func anchorNullVersion(ctx context.Context, tx *sql.Tx, tenantID, bucket, key string) error {
	if _, err := tx.ExecContext(ctx, anchorNullVersionSQL, tenantID, bucket, key); err != nil {
		return fmt.Errorf("anchor the pre-versioning object as the null version: %w", err)
	}
	return nil
}

const anchorNullVersionSQL = `
	INSERT INTO object_versions
		(tenant_id, bucket, object_key, version_id, size_bytes, etag, content_type, is_latest, is_delete_marker,
		 backend_name, storage_key, encryption_algorithm, encryption_key_version, floor, created_at)
	SELECT h.tenant_id, h.bucket, h.object_key, 'null', h.size_bytes, h.etag, h.content_type, TRUE, FALSE,
	       h.backend_name, COALESCE(h.storage_key, h.object_key), NULLIF(h.encryption_algorithm, ''), h.encryption_key_version,
	       h.floor, h.updated_at
	FROM object_head_cache h
	WHERE h.tenant_id = $1 AND h.bucket = $2 AND h.object_key = $3 AND NOT h.is_chunked
	  AND NOT EXISTS (SELECT 1 FROM object_versions v
	                  WHERE v.tenant_id = $1 AND v.bucket = $2 AND v.object_key = $3)`

// replacedNullVersion is the retained null version a Suspended write is
// about to replace.
type replacedNullVersion struct {
	storageKey, backend, floor string
	size                       int64
}

// retainedNullVersion reads the key's "null" version when it keeps bytes
// OTHER than the displaced current object's (dAddr): a non-current version
// the Suspended write's null-row rewrite replaces. Its floor as the bill
// knows it (usage.RetainedVersionFloor; the head row read is the displaced
// one, under the same lock).
func retainedNullVersion(ctx context.Context, tx *sql.Tx, tenantID, bucket, key, dAddr, headFloor string) (replacedNullVersion, error) {
	var r replacedNullVersion
	err := tx.QueryRowContext(ctx, `
		SELECT storage_key, COALESCE(backend_name, ''), size_bytes,
		       COALESCE(floor, NULLIF($5, ''), CASE WHEN backend_name = 'geyser' THEN 'vault' ELSE 'standard' END)
		FROM object_versions
		WHERE tenant_id = $1 AND bucket = $2 AND object_key = $3 AND version_id = 'null'
		  AND NOT is_delete_marker AND storage_key IS NOT NULL AND storage_key <> $4`,
		tenantID, bucket, key, dAddr, headFloor).Scan(&r.storageKey, &r.backend, &r.size, &r.floor)
	if errors.Is(err, sql.ErrNoRows) {
		return replacedNullVersion{}, nil
	}
	if err != nil {
		return replacedNullVersion{}, fmt.Errorf("read the null version: %w", err)
	}
	return r, nil
}
