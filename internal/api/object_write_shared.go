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
	"fmt"
	"io"
	"net/http"

	"github.com/FairForge/vaultaire/internal/engine"
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

// upsertWholeObjectHeadRow writes the head row for a WHOLE, UNENCRYPTED
// object (multipart complete, plain copy) inside the caller's transaction.
// Every attribute column is set explicitly: an upsert that only touched
// size/etag/content_type kept the displaced object's encryption_algorithm,
// metadata and cache headers, so a plaintext object overwriting an SSE one
// was served through the decryptor (live: GET 500, R3-05). is_chunked is
// forced FALSE — the caller's atomicHeadUpsertReleasing frees a displaced
// manifest in the same transaction.
func upsertWholeObjectHeadRow(ctx context.Context, tx *sql.Tx, tenantID, bucket, key string,
	size int64, etag, backendName, floor string, attrs objectAttrs) error {
	_, err := tx.ExecContext(ctx, `
		INSERT INTO object_head_cache
			(tenant_id, bucket, object_key, size_bytes, etag, content_type, backend_name, metadata,
			 encryption_algorithm, content_disposition, content_encoding, content_language,
			 cache_control, http_expires, website_redirect_location, floor, is_chunked, tags, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, '', $9, $10, $11, $12, $13, $14, $15, FALSE, $16, NOW())
		ON CONFLICT (tenant_id, bucket, object_key) DO UPDATE SET
			size_bytes                = EXCLUDED.size_bytes,
			etag                      = EXCLUDED.etag,
			content_type              = EXCLUDED.content_type,
			backend_name              = EXCLUDED.backend_name,
			metadata                  = EXCLUDED.metadata,
			encryption_algorithm      = '',
			content_disposition       = EXCLUDED.content_disposition,
			content_encoding          = EXCLUDED.content_encoding,
			content_language          = EXCLUDED.content_language,
			cache_control             = EXCLUDED.cache_control,
			http_expires              = EXCLUDED.http_expires,
			website_redirect_location = EXCLUDED.website_redirect_location,
			floor                     = EXCLUDED.floor,
			is_chunked                = FALSE,
			tags                      = EXCLUDED.tags,
			updated_at                = NOW()`,
		tenantID, bucket, key, size, etag, attrs.ContentType, backendName, attrs.metadataJSON(),
		attrs.ContentDisposition, attrs.ContentEncoding, attrs.ContentLanguage,
		attrs.CacheControl, attrs.Expires, attrs.WebsiteRedirect, floor, attrs.tagsJSON())
	if err != nil {
		return fmt.Errorf("upsert head row %s/%s: %w", bucket, key, err)
	}
	return nil
}

// placeObject writes body where the bucket's placement says (WP-R7-1): a
// bucket pinned to a region this process serves goes straight to that
// region's driver; a region with no driver is REFUSED with
// errRegionDriverUnavailable (never silently the primary — that is a
// residency breach recorded as normal); everything else goes through the
// engine, which maps the storage class in opts to a backend. Returns the
// backend that took the bytes — the head row's backend_name.
func placeObject(ctx context.Context, db *sql.DB, eng engine.Engine, tenantID, bucket, container, key string,
	body io.Reader, opts ...engine.PutOption) (string, error) {
	regionDriver, err := bucketRegionDriver(ctx, db, eng, tenantID, bucket)
	if err != nil {
		return "", err
	}
	if regionDriver != "" {
		if ce, ok := eng.(*engine.CoreEngine); ok {
			if drv, exists := ce.GetDriver(regionDriver); exists {
				if putErr := drv.Put(ctx, container, key, body, opts...); putErr != nil {
					return "", fmt.Errorf("region driver %s put %s/%s: %w", regionDriver, container, key, putErr)
				}
				ce.HintBackend(container, key, regionDriver)
				return regionDriver, nil
			}
		}
	}
	return eng.Put(ctx, container, key, body, opts...)
}

// recordObjectVersion writes the object_versions ledger row for a completed
// write on a versioned bucket and returns the version id to echo in
// x-amz-version-id ("" for an unversioned bucket or no DB). Enabled buckets
// get a fresh id, Suspended buckets the "null" version, exactly as plain PUT.
// Versioning is metadata-only today (WP-R2-1): the row describes the key's
// current bytes; older rows describe bytes that were overwritten in place.
func recordObjectVersion(ctx context.Context, db *sql.DB, tenantID, bucket, key string,
	size int64, etag, contentType, backendName string) string {
	if db == nil {
		return ""
	}
	vStatus := getBucketVersioningStatus(ctx, db, tenantID, bucket)
	if vStatus != "Enabled" && vStatus != "Suspended" {
		return ""
	}
	versionID, _ := writeVersionRow(ctx, db, vStatus, tenantID, bucket, key, size, etag, contentType, backendName)
	return versionID
}

// execer is what a version write needs: a *sql.DB or a *sql.Tx.
type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// writeObjectVersion is the version write of recordObjectVersion inside the
// caller's transaction (vStatus already read): the first error is returned.
func writeObjectVersion(ctx context.Context, tx execer, vStatus, tenantID, bucket, key string,
	size int64, etag, contentType, backendName string) error {
	_, err := writeVersionRow(ctx, tx, vStatus, tenantID, bucket, key, size, etag, contentType, backendName)
	return err
}

// writeVersionRow makes the key's current bytes its latest version: a new
// id with versioning Enabled, "null" when Suspended.
func writeVersionRow(ctx context.Context, q execer, vStatus, tenantID, bucket, key string,
	size int64, etag, contentType, backendName string) (string, error) {
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
	_, err := q.ExecContext(ctx, `
		INSERT INTO object_versions
			(tenant_id, bucket, object_key, version_id, size_bytes, etag, content_type, is_latest, is_delete_marker, backend_name)
		VALUES ($1, $2, $3, $4, $5, $6, $7, TRUE, FALSE, $8)
		ON CONFLICT (tenant_id, bucket, object_key, version_id) DO UPDATE SET
			size_bytes = EXCLUDED.size_bytes, etag = EXCLUDED.etag,
			content_type = EXCLUDED.content_type, is_latest = TRUE,
			is_delete_marker = FALSE, backend_name = EXCLUDED.backend_name`,
		tenantID, bucket, key, versionID, size, etag, contentType, backendName)
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
