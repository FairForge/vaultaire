package api

// Review R3 (docs/reviews/R3-multipart-copy-batch.md): the pieces of an
// object write that plain PUT, CompleteMultipartUpload and CopyObject must
// share. Each of the three used to re-implement them and drifted — multipart
// complete wrote no backend_name (routing truth), ignored the bucket's
// region and the attributes sent on CreateMultipartUpload, and both
// multipart complete and copy left the previous object's encryption_algorithm
// / metadata on the head row (a plaintext object recorded as encrypted is
// unreadable) and never wrote the versioning ledger.

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/FairForge/vaultaire/internal/engine"
	"github.com/FairForge/vaultaire/internal/usage"
)

// objectAttrs is everything the head row records about an object besides
// its bytes: the HTTP attributes a client sends on PUT / CreateMultipartUpload
// / CopyObject(REPLACE), or copies from the source on CopyObject(COPY).
type objectAttrs struct {
	ContentType        string
	ContentDisposition string
	ContentEncoding    string
	ContentLanguage    string
	CacheControl       string
	Expires            string
	WebsiteRedirect    string
	Metadata           map[string]string
	// Tags is the object's tag set (x-amz-tagging on PUT / CopyObject
	// REPLACE, the source's under COPY, empty otherwise). Written on every
	// whole-object upsert: a tag set used to survive an overwrite (R4-06).
	Tags map[string]string
}

// objectAttrsFromRequest reads the attributes a PUT-shaped request carries.
// Metadata is validated here so callers refuse the request before any state
// is written (R2-05: a rejection after the write corrupts the existing key).
func objectAttrsFromRequest(r *http.Request) (objectAttrs, error) {
	meta := extractS3Metadata(r)
	if err := validateMetadata(meta); err != nil {
		return objectAttrs{}, err
	}
	tags, err := parseTaggingHeader(r.Header.Get("x-amz-tagging"))
	if err != nil {
		return objectAttrs{}, err
	}
	echo := requestEchoHeaders(r)
	ct := r.Header.Get("Content-Type")
	if ct == "" {
		ct = "application/octet-stream"
	}
	return objectAttrs{
		ContentType:        ct,
		ContentDisposition: sanitizeContentDisposition(r.Header.Get("Content-Disposition")),
		ContentEncoding:    requestContentEncoding(r),
		ContentLanguage:    requestContentLanguage(r),
		CacheControl:       echo.CacheControl,
		Expires:            echo.Expires,
		WebsiteRedirect:    echo.WebsiteRedirect,
		Metadata:           meta,
		Tags:               tags,
	}, nil
}

func (o objectAttrs) tagsJSON() []byte {
	m := o.Tags
	if m == nil {
		m = map[string]string{}
	}
	b, _ := json.Marshal(m)
	return b
}

func (o objectAttrs) metadataJSON() []byte {
	m := o.Metadata
	if m == nil {
		m = map[string]string{}
	}
	b, _ := json.Marshal(m)
	return b
}

// loadHeadAttrs reads the attributes recorded on an object's head row — the
// source side of a COPY-directive CopyObject. sql.ErrNoRows when the key has
// no row.
func loadHeadAttrs(ctx context.Context, db *sql.DB, tenantID, bucket, key string) (objectAttrs, error) {
	var a objectAttrs
	var metaJSON, tagsJSON []byte
	err := db.QueryRowContext(ctx, `
		SELECT content_type, metadata, content_disposition, content_encoding, content_language,
		       cache_control, http_expires, website_redirect_location, COALESCE(tags, '{}')
		FROM object_head_cache
		WHERE tenant_id = $1 AND bucket = $2 AND object_key = $3`,
		tenantID, bucket, key).Scan(&a.ContentType, &metaJSON, &a.ContentDisposition, &a.ContentEncoding,
		&a.ContentLanguage, &a.CacheControl, &a.Expires, &a.WebsiteRedirect, &tagsJSON)
	if err != nil {
		return objectAttrs{}, err
	}
	if len(metaJSON) > 0 {
		_ = json.Unmarshal(metaJSON, &a.Metadata)
	}
	if len(tagsJSON) > 0 {
		_ = json.Unmarshal(tagsJSON, &a.Tags)
	}
	if a.ContentType == "" {
		a.ContentType = "application/octet-stream"
	}
	return a, nil
}

// objectEncryption is what a whole object's blob is sealed with: the zero
// value is plaintext; SSE-S3 is crypto.SSEAlgorithm + the tenant key version
// the blob records (WP-R8-1).
type objectEncryption struct {
	Algorithm  string
	KeyVersion *int
}

// upsertWholeObjectHeadRow writes the head row for a WHOLE, UNENCRYPTED
// object (multipart complete, plain copy, direct upload) whose blob is at
// storageKey (WP-R2-1: the writer's new address) inside the caller's
// transaction.
// Every attribute column is set explicitly: an upsert that only touched
// size/etag/content_type kept the displaced object's encryption_algorithm,
// metadata and cache headers, so a plaintext object overwriting an SSE one
// was served through the decryptor (live: GET 500, R3-05). is_chunked is
// forced FALSE — the caller's atomicHeadUpsertReleasing frees a displaced
// manifest in the same transaction.
func upsertWholeObjectHeadRow(ctx context.Context, tx *sql.Tx, tenantID, bucket, key, storageKey string,
	size int64, etag, backendName, floor string, attrs objectAttrs) error {
	return upsertWholeObjectHeadRowEnc(ctx, tx, tenantID, bucket, key, storageKey, size, etag, backendName, floor, attrs, objectEncryption{})
}

// upsertWholeObjectHeadRowEnc is upsertWholeObjectHeadRow for a blob sealed
// with enc (a generated object or a copy into an sse_enabled bucket, WP-R8-1
// follow-up A1): size and etag are the PLAINTEXT's, as HandlePut records them.
func upsertWholeObjectHeadRowEnc(ctx context.Context, tx *sql.Tx, tenantID, bucket, key, storageKey string,
	size int64, etag, backendName, floor string, attrs objectAttrs, enc objectEncryption) error {
	_, err := tx.ExecContext(ctx, `
		INSERT INTO object_head_cache
			(tenant_id, bucket, object_key, size_bytes, etag, content_type, backend_name, metadata,
			 encryption_algorithm, content_disposition, content_encoding, content_language,
			 cache_control, http_expires, website_redirect_location, floor, is_chunked, tags, encryption_key_version, storage_key, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $17, $9, $10, $11, $12, $13, $14, $15, FALSE, $16, $18, NULLIF($19, ''), NOW())
		ON CONFLICT (tenant_id, bucket, object_key) DO UPDATE SET
			size_bytes                = EXCLUDED.size_bytes,
			etag                      = EXCLUDED.etag,
			content_type              = EXCLUDED.content_type,
			backend_name              = EXCLUDED.backend_name,
			metadata                  = EXCLUDED.metadata,
			encryption_algorithm      = EXCLUDED.encryption_algorithm,
			encryption_key_version    = EXCLUDED.encryption_key_version,
			content_disposition       = EXCLUDED.content_disposition,
			content_encoding          = EXCLUDED.content_encoding,
			content_language          = EXCLUDED.content_language,
			cache_control             = EXCLUDED.cache_control,
			http_expires              = EXCLUDED.http_expires,
			website_redirect_location = EXCLUDED.website_redirect_location,
			floor                     = EXCLUDED.floor,
			is_chunked                = FALSE,
			tags                      = EXCLUDED.tags,
			storage_key               = EXCLUDED.storage_key, -- the writer's own address (WP-R2-1)
			updated_at                = NOW()`,
		tenantID, bucket, key, size, etag, attrs.ContentType, backendName, attrs.metadataJSON(),
		attrs.ContentDisposition, attrs.ContentEncoding, attrs.ContentLanguage,
		attrs.CacheControl, attrs.Expires, attrs.WebsiteRedirect, floor, attrs.tagsJSON(), enc.Algorithm, enc.KeyVersion, storageKey)
	if err != nil {
		return fmt.Errorf("upsert head row %s/%s: %w", bucket, key, err)
	}
	return nil
}

// placeObject writes body at blob — the write's own address (newBlobAddress,
// WP-R2-1), never the bare key — where the bucket's placement says (WP-R7-1): a
// bucket pinned to a region this process serves goes straight to that
// region's driver; a region with no driver is REFUSED with
// errRegionDriverUnavailable (never silently the primary — that is a
// residency breach recorded as normal); everything else goes through the
// engine, which maps the storage class in opts to a backend. Returns the
// backend that took the bytes — the head row's backend_name.
func placeObject(ctx context.Context, db *sql.DB, eng engine.Engine, tenantID, bucket, container, blob string,
	body io.Reader, opts ...engine.PutOption) (string, error) {
	regionDriver, err := bucketRegionDriver(ctx, db, eng, tenantID, bucket)
	if err != nil {
		return "", err
	}
	if regionDriver != "" {
		if ce, ok := eng.(*engine.CoreEngine); ok {
			if drv, exists := ce.GetDriver(regionDriver); exists {
				if putErr := drv.Put(ctx, container, blob, body, opts...); putErr != nil {
					return "", fmt.Errorf("region driver %s put %s/%s: %w", regionDriver, container, blob, putErr)
				}
				ce.HintBackend(container, blob, regionDriver)
				return regionDriver, nil
			}
		}
	}
	return eng.Put(ctx, container, blob, body, opts...)
}

// lockVersionKey takes the transaction-scoped advisory lock every write of a
// key's object_versions rows holds (version writes, delete markers, version
// deletes, a Complete retry's re-assert): one latest row per key.
func lockVersionKey(ctx context.Context, tx *sql.Tx, tenantID, bucket, key string) error {
	_, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('object_versions/' || $1 || '/' || $2 || '/' || $3, 0))`,
		tenantID, bucket, key)
	if err != nil {
		return fmt.Errorf("lock the version rows of %s/%s: %w", bucket, key, err)
	}
	return nil
}

// versionRowUpdatedHook runs between a version write's two statements
// (tests hold two writers there).
var versionRowUpdatedHook = func() {}

// writeDeleteMarker makes a delete marker the key's latest version, under
// the key's version lock.
func writeDeleteMarker(ctx context.Context, db *sql.DB, tenantID, bucket, key, markerID string) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := lockVersionKey(ctx, tx, tenantID, bucket, key); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE object_versions SET is_latest = FALSE
		WHERE tenant_id = $1 AND bucket = $2 AND object_key = $3 AND is_latest = TRUE`, tenantID, bucket, key); err != nil {
		return err
	}
	versionRowUpdatedHook()
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO object_versions
			(tenant_id, bucket, object_key, version_id, size_bytes, etag, content_type, is_latest, is_delete_marker)
		VALUES ($1, $2, $3, $4, 0, '', 'application/octet-stream', TRUE, TRUE)`, tenantID, bucket, key, markerID); err != nil {
		return err
	}
	return tx.Commit()
}

// headRepoint is what a version delete did to the bill: the caller settles
// quota from it. Changed: the deleted version was the current object's blob,
// so the head row's bytes (OldSize on Floor) are released; the newly-latest
// version it was re-pointed to was billed as a retained version and stays
// billed — as the head row now (PR C2), nothing more is charged.
// VersionReleased: the deleted version was a retained non-current version
// with bytes of its own (usage.RetainedVersionsFrom), released on its floor.
type headRepoint struct {
	Changed         bool
	Dropped         bool // no live version with bytes is left: the head row is gone
	OldSize         int64
	NewSize         int64
	Floor           string
	Backend         string // the dropped/old row's backend
	VersionReleased quotaRelease
}

// deleteVersion removes one version of a key and, when no latest version is
// left, makes the newest remaining one latest — under the key's head-row
// lock and version lock (the flip's order). found is false when no such
// version exists.
//
// Deleting the CURRENT version of a key whose head row names that version's
// blob (WP-R2-1 Part C) re-points the head row, in the same transaction, to
// the newly-latest version's address, size, ETag, type and encryption — or
// drops it when the newly-latest is a delete marker, no version is left, or
// it kept no bytes. The deleted version's blob is retired unless a row still
// names it. A head row that does not name the deleted version's blob (every
// row written before 2C) is left as it was.
func deleteVersion(ctx context.Context, db *sql.DB, tenantID, bucket, key, versionID string) (found bool, rp headRepoint, err error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return false, rp, err
	}
	defer func() { _ = tx.Rollback() }()
	if err := lockHeadKey(ctx, tx, tenantID, bucket, key); err != nil {
		return false, rp, err
	}
	if err := lockVersionKey(ctx, tx, tenantID, bucket, key); err != nil {
		return false, rp, err
	}
	// What the version holds, read under both locks before it goes: a
	// retained version (bytes of its own, not the current object's blob) is
	// billed on its own and released with it.
	var storageKey, backend, floor string
	var size int64
	var retained bool
	err = tx.QueryRowContext(ctx, `
		SELECT COALESCE(v.storage_key, ''), COALESCE(v.backend_name, ''), v.size_bytes, `+usage.RetainedVersionFloor+`,
		       (v.storage_key IS NOT NULL AND NOT v.is_delete_marker
		        AND (h.tenant_id IS NULL OR h.is_chunked OR COALESCE(h.storage_key, h.object_key) <> v.storage_key))
		FROM object_versions v
		LEFT JOIN object_head_cache h ON h.tenant_id = v.tenant_id AND h.bucket = v.bucket AND h.object_key = v.object_key
		WHERE v.tenant_id = $1 AND v.bucket = $2 AND v.object_key = $3 AND v.version_id = $4`,
		tenantID, bucket, key, versionID).Scan(&storageKey, &backend, &size, &floor, &retained)
	if errors.Is(err, sql.ErrNoRows) {
		return false, rp, nil
	}
	if err != nil {
		return false, rp, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM object_versions
		WHERE tenant_id = $1 AND bucket = $2 AND object_key = $3 AND version_id = $4`, tenantID, bucket, key, versionID); err != nil {
		return false, rp, err
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE object_versions SET is_latest = TRUE
		WHERE tenant_id = $1 AND bucket = $2 AND object_key = $3
		  AND version_id = (SELECT version_id FROM object_versions
		                    WHERE tenant_id = $1 AND bucket = $2 AND object_key = $3
		                    ORDER BY created_at DESC, version_id DESC LIMIT 1)
		  AND NOT EXISTS (SELECT 1 FROM object_versions
		                  WHERE tenant_id = $1 AND bucket = $2 AND object_key = $3 AND is_latest)`,
		tenantID, bucket, key); err != nil {
		return true, rp, err
	}
	if storageKey != "" {
		if rp, err = repointHeadAfterVersionDelete(ctx, tx, tenantID, bucket, key, storageKey); err != nil {
			return true, rp, err
		}
	}
	if retained && size > 0 {
		rp.VersionReleased = quotaRelease{Floor: floor, Bytes: size}
	}
	// A version that kept its bytes (WP-R2-1): its blob goes to the
	// collector with the row delete, unless the head row or another version
	// still names it. Retired, not deleted here: a GET that resolved the
	// version a moment ago may still be reading it.
	if err := retireBlob(ctx, tx, tenantID, bucket, key, storageKey, backend, retiredVersionDeleted); err != nil {
		return true, rp, err
	}
	return true, rp, tx.Commit()
}

// repointHeadAfterVersionDelete is deleteVersion's head-row step, inside its
// transaction: only a head row that names deletedBlob is touched.
func repointHeadAfterVersionDelete(ctx context.Context, tx *sql.Tx, tenantID, bucket, key, deletedBlob string) (headRepoint, error) {
	var rp headRepoint
	err := tx.QueryRowContext(ctx, `
		SELECT size_bytes, floor, COALESCE(backend_name, '') FROM object_head_cache
		WHERE tenant_id = $1 AND bucket = $2 AND object_key = $3 AND storage_key = $4 AND NOT is_chunked
		FOR UPDATE`, tenantID, bucket, key, deletedBlob).Scan(&rp.OldSize, &rp.Floor, &rp.Backend)
	if errors.Is(err, sql.ErrNoRows) {
		return headRepoint{}, nil // the head row names other bytes (or none): not this delete's business
	}
	if err != nil {
		return headRepoint{}, fmt.Errorf("read the head row of the deleted version: %w", err)
	}
	rp.Changed = true
	// The newly-latest version, when it is a live version that kept bytes.
	res, err := tx.ExecContext(ctx, `
		UPDATE object_head_cache h SET
			size_bytes = v.size_bytes, etag = v.etag, content_type = v.content_type,
			backend_name = v.backend_name, storage_key = v.storage_key,
			floor = COALESCE(v.floor, h.floor), -- where the version was billed (088)
			encryption_algorithm = COALESCE(v.encryption_algorithm, ''), encryption_key_version = v.encryption_key_version,
			-- no per-version copy of these exists: they described the deleted version
			metadata = '{}', tags = '{}', content_disposition = '', content_encoding = '', content_language = '',
			cache_control = '', http_expires = '', website_redirect_location = '',
			updated_at = v.created_at
		FROM object_versions v
		WHERE h.tenant_id = $1 AND h.bucket = $2 AND h.object_key = $3
		  AND v.tenant_id = h.tenant_id AND v.bucket = h.bucket AND v.object_key = h.object_key
		  AND v.is_latest AND NOT v.is_delete_marker AND v.storage_key IS NOT NULL`,
		tenantID, bucket, key)
	if err != nil {
		return headRepoint{}, fmt.Errorf("re-point the head row: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 1 {
		if err := tx.QueryRowContext(ctx, `SELECT size_bytes FROM object_head_cache
			WHERE tenant_id = $1 AND bucket = $2 AND object_key = $3`, tenantID, bucket, key).Scan(&rp.NewSize); err != nil {
			return headRepoint{}, fmt.Errorf("read the re-pointed head row: %w", err)
		}
		return rp, nil
	}
	// Nothing live with bytes is latest: the key has no current object.
	if _, err := tx.ExecContext(ctx, `DELETE FROM object_head_cache
		WHERE tenant_id = $1 AND bucket = $2 AND object_key = $3`, tenantID, bucket, key); err != nil {
		return headRepoint{}, fmt.Errorf("drop the head row: %w", err)
	}
	rp.Dropped = true
	return rp, nil
}

// execer is what a version write needs: a *sql.DB or a *sql.Tx.
type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// writeObjectVersion is the version write inside the caller's transaction (vStatus already read, the key's version lock held):
// the first error is returned.
func writeObjectVersion(ctx context.Context, tx execer, vStatus, tenantID, bucket, key string,
	size int64, etag, contentType, backendName, floor string, blob versionBlobRef) error {
	_, err := writeVersionRow(ctx, tx, vStatus, tenantID, bucket, key, size, etag, contentType, backendName, floor, blob)
	return err
}

// writeVersionRow makes the key's current bytes its latest version: a new
// id with versioning Enabled, "null" when Suspended (a rewrite of the "null"
// row replaces its address: the blob it named is no longer named and the
// flip retires it). blob is the version's own address and encryption; the
// zero value = no bytes retained (the chunked path). floor is the head row's
// (088): where a retained version is billed.
func writeVersionRow(ctx context.Context, q execer, vStatus, tenantID, bucket, key string,
	size int64, etag, contentType, backendName, floor string, blob versionBlobRef) (string, error) {
	versionID := "null"
	if vStatus == "Enabled" {
		versionID = generateVersionID()
	}
	if _, err := q.ExecContext(ctx, `
		UPDATE object_versions SET is_latest = FALSE
		WHERE tenant_id = $1 AND bucket = $2 AND object_key = $3 AND is_latest = TRUE`,
		tenantID, bucket, key); err != nil {
		return versionID, err
	}
	versionRowUpdatedHook()
	_, err := q.ExecContext(ctx, `
		INSERT INTO object_versions
			(tenant_id, bucket, object_key, version_id, size_bytes, etag, content_type, is_latest, is_delete_marker, backend_name,
			 storage_key, encryption_algorithm, encryption_key_version, floor)
		VALUES ($1, $2, $3, $4, $5, $6, $7, TRUE, FALSE, $8, NULLIF($9, ''), NULLIF($10, ''), $11, NULLIF($12, ''))
		ON CONFLICT (tenant_id, bucket, object_key, version_id) DO UPDATE SET
			size_bytes = EXCLUDED.size_bytes, etag = EXCLUDED.etag,
			content_type = EXCLUDED.content_type, is_latest = TRUE,
			is_delete_marker = FALSE, backend_name = EXCLUDED.backend_name,
			storage_key = EXCLUDED.storage_key, encryption_algorithm = EXCLUDED.encryption_algorithm,
			encryption_key_version = EXCLUDED.encryption_key_version, floor = EXCLUDED.floor`,
		tenantID, bucket, key, versionID, size, etag, contentType, backendName,
		blob.StorageKey, blob.Enc.Algorithm, blob.Enc.KeyVersion, floor)
	return versionID, err
}

// newUploadID returns "upload-" + 32 hex chars from crypto/rand (R5-28: the
// old unix-nanos ids were predictable). Hex only — the id names the staging
// directory under multipartTempBase.
func newUploadID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generate upload id: %w", err)
	}
	return "upload-" + hex.EncodeToString(b[:]), nil
}
