package api

import (
	"crypto/md5" // #nosec G501 — S3 spec requires MD5 for ETags
	"database/sql"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/FairForge/vaultaire/internal/crypto"
	"github.com/FairForge/vaultaire/internal/tenant"
	"go.uber.org/zap"
)

// CopyObjectResult is the XML response for a successful CopyObject.
type CopyObjectResult struct {
	XMLName      xml.Name `xml:"CopyObjectResult"`
	ETag         string   `xml:"ETag"`
	LastModified string   `xml:"LastModified"`
}

// countingReader wraps an io.Reader and tracks the total bytes read.
// Used during CopyObject so the destination size can be persisted from the
// authoritative byte count rather than relying on the source's head_cache row
// (which can be missing or stale for objects written before the cache existed).
type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

// resolveCopyContentType picks the content-type for a CopyObject destination.
//
// Per S3 spec: when x-amz-metadata-directive is "REPLACE" the request's own
// Content-Type header wins; when "COPY" (the default) the source object's
// content-type is preserved. Either way we fall back to
// application/octet-stream if the chosen source is empty.
func resolveCopyContentType(directive, requestCT, sourceCT string) string {
	if strings.EqualFold(directive, "REPLACE") {
		if requestCT != "" {
			return requestCT
		}
		return "application/octet-stream"
	}
	if sourceCT != "" {
		return sourceCT
	}
	return "application/octet-stream"
}

// handleCopyObject handles S3 CopyObject requests.
//
// S3 spec: PUT /dest-bucket/dest-key with x-amz-copy-source header. The source
// is streamed through a TeeReader into the destination — never buffered in
// memory. The byte count from the wrapping countingReader is used as the
// authoritative size for the destination's head_cache row.
//
// x-amz-metadata-directive selects whether to preserve source metadata
// (default, "COPY") or take it from the request ("REPLACE"). Self-copy is now
// handled by the same Get→Put streaming path because LocalDriver.Put is atomic
// (writes via temp+rename) and no longer truncates the source mid-read.
func (s *Server) handleCopyObject(w http.ResponseWriter, r *http.Request, req *S3Request) {
	t, err := tenant.FromContext(r.Context())
	if err != nil || t == nil {
		WriteS3Error(w, ErrAccessDenied, r.URL.Path, generateRequestID())
		return
	}

	copySource := r.Header.Get("x-amz-copy-source")
	srcBucket, srcKey, err := parseCopySource(copySource)
	if err != nil {
		s.logger.Warn("invalid x-amz-copy-source",
			zap.String("copy_source", copySource),
			zap.Error(err))
		WriteS3Error(w, ErrInvalidRequest, r.URL.Path, generateRequestID())
		return
	}

	destBucket := req.Bucket
	destKey := req.Object

	srcContainer := t.NamespaceContainer(srcBucket)
	destContainer := t.NamespaceContainer(destBucket)

	directive := r.Header.Get("x-amz-metadata-directive")

	s.logger.Debug("CopyObject",
		zap.String("tenant_id", t.ID),
		zap.String("src_bucket", srcBucket),
		zap.String("src_key", srcKey),
		zap.String("dest_bucket", destBucket),
		zap.String("dest_key", destKey),
		zap.String("directive", directive))

	// Plain copy streams the source's raw stored bytes: for whole-object
	// encrypted sources that would hand back undecryptable ciphertext (and
	// bill physical, not logical, size) — refuse those cleanly. Chunked
	// sources (including per-chunk AES256-CE — same tenant, same convergent
	// keys) take the manifest-copy path instead: no data moves, each shared
	// chunk just gains a reference.
	var srcSize int64
	var srcEnc string
	var srcChunked bool
	if s.db != nil {
		_ = s.db.QueryRowContext(r.Context(), `
			SELECT size_bytes, COALESCE(encryption_algorithm, ''), is_chunked
			FROM object_head_cache
			WHERE tenant_id = $1 AND bucket = $2 AND object_key = $3`,
			t.ID, srcBucket, srcKey).Scan(&srcSize, &srcEnc, &srcChunked)
	}
	if srcChunked && s.gci != nil {
		s.handleChunkedCopy(w, r, t, srcBucket, srcKey, destBucket, destKey, srcSize, directive)
		return
	}
	if srcEnc != "" || srcChunked {
		WriteS3ErrorWithContext(w, ErrNotImplemented, r.URL.Path, generateRequestID(),
			WithSuggestion("Copying encrypted or chunked objects is not yet supported. Download and re-upload instead."))
		return
	}

	reader, err := s.engine.Get(r.Context(), srcContainer, srcKey)
	if err != nil {
		if strings.Contains(err.Error(), "no such file or directory") ||
			strings.Contains(err.Error(), "not found") {
			WriteS3Error(w, ErrNoSuchKey, r.URL.Path, generateRequestID())
		} else {
			s.logger.Error("copy: source get failed",
				zap.Error(err),
				zap.String("container", srcContainer),
				zap.String("key", srcKey))
			WriteS3Error(w, ErrInternalError, r.URL.Path, generateRequestID())
		}
		return
	}
	defer func() { _ = reader.Close() }()

	// WP-1: copy bypasses the PUT handler's reservation, so reserve the
	// source's recorded size before writing the destination. The reservation
	// is settled against the actual streamed byte count after the write, and
	// an overwritten destination's bytes (captured atomically by the upsert)
	// are released.
	quotaOn := s.quotaManager != nil
	var reservedBytes int64
	if quotaOn {
		if srcSize > 0 {
			ok, qErr := s.quotaManager.CheckAndReserve(r.Context(), t.ID, srcSize)
			if qErr != nil {
				s.logger.Error("copy: quota check failed",
					zap.Error(qErr), zap.String("tenant_id", t.ID))
				WriteS3Error(w, ErrInternalError, r.URL.Path, generateRequestID())
				return
			}
			if !ok {
				WriteS3ErrorWithContext(w, ErrQuotaExceeded, r.URL.Path, generateRequestID(),
					WithSuggestion("Upgrade at https://stored.ge/dashboard/billing"))
				return
			}
			reservedBytes = srcSize
		} else {
			// Unknown source size (drifted or missing head-cache row): the
			// bytes are accounted after the stream, but refuse outright when
			// the tenant is already at their limit.
			used, limit, uErr := s.quotaManager.GetUsage(r.Context(), t.ID)
			if uErr == nil && limit > 0 && used >= limit {
				WriteS3ErrorWithContext(w, ErrQuotaExceeded, r.URL.Path, generateRequestID(),
					WithSuggestion("Upgrade at https://stored.ge/dashboard/billing"))
				return
			}
		}
	}

	// Stream source → MD5 hasher → destination, tallying bytes as we go so
	// the persisted size never depends on the source cache row being present.
	counter := &countingReader{r: reader}
	hasher := md5.New() // #nosec G401 — S3 spec requires MD5 for ETags
	tee := io.TeeReader(counter, hasher)

	backendName, err := s.engine.Put(r.Context(), destContainer, destKey, tee)
	if err != nil {
		if quotaOn {
			ctx, cancel := quotaCtx(r)
			s.releaseQuota(ctx, t.ID, reservedBytes)
			cancel()
		}
		s.logger.Error("copy: dest put failed",
			zap.Error(err),
			zap.String("container", destContainer),
			zap.String("key", destKey))
		WriteS3Error(w, ErrInternalError, r.URL.Path, generateRequestID())
		return
	}

	etag := fmt.Sprintf("%x", hasher.Sum(nil))
	now := time.Now().UTC()

	// Update object_head_cache for the copied object.
	var displacedSize int64
	if s.db != nil {
		// Look up source content-type (for COPY directive). Size is now
		// authoritative from counter.n — no fallback path needed.
		var sourceCT string
		_ = s.db.QueryRowContext(r.Context(), `
			SELECT content_type
			FROM object_head_cache
			WHERE tenant_id = $1 AND bucket = $2 AND object_key = $3
		`, t.ID, srcBucket, srcKey).Scan(&sourceCT)

		contentType := resolveCopyContentType(directive, r.Header.Get("Content-Type"), sourceCT)

		// atomicHeadUpsert captures the overwritten row's size (WP-1).
		var dbErr error
		displacedSize, dbErr = atomicHeadUpsertReleasing(r.Context(), s.db, manifestReleaser(s.gci), t.ID, destBucket, destKey, func(tx *sql.Tx) error {
			// is_chunked=FALSE explicitly: overwriting a chunked destination
			// must flip the flag (and the releaser above frees its manifest),
			// or GET keeps reading the stale manifest.
			_, execErr := tx.ExecContext(r.Context(), `
				INSERT INTO object_head_cache
					(tenant_id, bucket, object_key, size_bytes, etag, content_type, backend_name, is_chunked, updated_at)
				VALUES ($1, $2, $3, $4, $5, $6, $7, FALSE, $8)
				ON CONFLICT (tenant_id, bucket, object_key) DO UPDATE SET
					size_bytes   = EXCLUDED.size_bytes,
					etag         = EXCLUDED.etag,
					content_type = EXCLUDED.content_type,
					backend_name = EXCLUDED.backend_name,
					is_chunked   = FALSE,
					updated_at   = EXCLUDED.updated_at
			`, t.ID, destBucket, destKey, counter.n, etag, contentType, backendName, now)
			return execErr
		})
		if dbErr != nil {
			displacedSize = 0
			s.logger.Error("copy: failed to cache object metadata",
				zap.Error(dbErr),
				zap.String("tenant_id", t.ID),
				zap.String("bucket", destBucket),
				zap.String("key", destKey))
		}
	}

	if quotaOn {
		ctx, cancel := quotaCtx(r)
		s.settlePutQuota(ctx, t.ID, reservedBytes, counter.n, displacedSize)
		cancel()
	}

	result := CopyObjectResult{
		ETag:         fmt.Sprintf(`"%s"`, etag),
		LastModified: now.Format("2006-01-02T15:04:05.000Z"),
	}

	xmlData, err := xml.MarshalIndent(result, "", "  ")
	if err != nil {
		s.logger.Error("copy: XML marshal failed", zap.Error(err))
		WriteS3Error(w, ErrInternalError, r.URL.Path, generateRequestID())
		return
	}

	w.Header().Set("Content-Type", "application/xml")
	w.Header().Set("x-amz-request-id", generateRequestID())
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(xml.Header))
	_, _ = w.Write(xmlData)

	s.logger.Info("object copied",
		zap.String("tenant_id", t.ID),
		zap.String("src", srcBucket+"/"+srcKey),
		zap.String("dest", destBucket+"/"+destKey),
		zap.String("backend", backendName),
		zap.String("directive", directive),
		zap.Int64("size", counter.n),
		zap.String("etag", etag))

	notifySvc := NewNotificationDispatcher(s.db, s.logger)
	notifySvc.Fire(t.ID, destBucket, "s3:ObjectCreated:Copy", destKey, counter.n, etag)
}

// parseCopySource parses the x-amz-copy-source header value.
//
// Accepts: /bucket/key, bucket/key, /bucket/key?versionId=xxx. The key portion
// is percent-decoded per AWS spec, so x-amz-copy-source: /b/foo%20bar resolves
// to source key "foo bar" (and %2F resolves to a literal '/' inside the key).
func parseCopySource(source string) (bucket, key string, err error) {
	if source == "" {
		return "", "", fmt.Errorf("empty copy source")
	}

	source = strings.TrimPrefix(source, "/")

	if idx := strings.IndexByte(source, '?'); idx >= 0 {
		source = source[:idx]
	}

	parts := strings.SplitN(source, "/", 2)
	if len(parts) < 2 || parts[0] == "" || parts[1] == "" {
		return "", "", fmt.Errorf("invalid copy source format: %q", source)
	}

	decodedKey, err := url.PathUnescape(parts[1])
	if err != nil {
		return "", "", fmt.Errorf("invalid percent-encoding in copy source key: %w", err)
	}

	return parts[0], decodedKey, nil
}

// handleChunkedCopy copies a chunked object without moving any data: the
// destination gets its own manifest rows referencing the same chunks, each
// chunk's refcount is incremented, and the head-cache/metadata rows are
// copied. Works for per-chunk encrypted (AES256-CE) sources too — same
// tenant, same convergent keys. Everything commits in ONE transaction via
// atomicHeadUpsert + ReplaceObjectManifestTx, so an overwritten chunked
// destination releases its old manifest atomically (same contract as
// handleChunkedPut after review-A).
func (s *Server) handleChunkedCopy(w http.ResponseWriter, r *http.Request,
	t *tenant.Tenant, srcBucket, srcKey, destBucket, destKey string,
	srcSize int64, directive string) {

	if srcBucket == destBucket && srcKey == destKey {
		// AWS requires changed metadata/storage-class for a self-copy; we
		// don't support in-place rewrites of chunked objects.
		WriteS3Error(w, ErrInvalidRequest, r.URL.Path, generateRequestID())
		return
	}

	// Chunked objects cannot live in versioned buckets (manifests have no
	// version_id — review-A F3), so refuse a chunked copy INTO one.
	destVersioning := getBucketVersioningStatus(r.Context(), s.db, t.ID, destBucket)
	if destVersioning == "Enabled" || destVersioning == "Suspended" {
		WriteS3ErrorWithContext(w, ErrNotImplemented, r.URL.Path, generateRequestID(),
			WithSuggestion("Copying a large (chunked) object into a versioned bucket is not yet supported."))
		return
	}

	// Reserve the destination's logical bytes (copy bypasses the PUT
	// handler's reservation — WP-1 contract).
	quotaOn := s.quotaManager != nil
	var reservedBytes int64
	if quotaOn && srcSize > 0 {
		ok, qErr := s.quotaManager.CheckAndReserve(r.Context(), t.ID, srcSize)
		if qErr != nil {
			s.logger.Error("chunked copy: quota check failed",
				zap.Error(qErr), zap.String("tenant_id", t.ID))
			WriteS3Error(w, ErrInternalError, r.URL.Path, generateRequestID())
			return
		}
		if !ok {
			WriteS3ErrorWithContext(w, ErrQuotaExceeded, r.URL.Path, generateRequestID(),
				WithSuggestion("Upgrade at https://stored.ge/dashboard/billing"))
			return
		}
		reservedBytes = srcSize
	}
	releaseReservation := func() {
		if quotaOn && reservedBytes > 0 {
			ctx, cancel := quotaCtx(r)
			s.releaseQuota(ctx, t.ID, reservedBytes)
			cancel()
		}
	}

	// Source manifest + head row (ETag is the plaintext MD5 — identical
	// content, identical ETag).
	srcRefs, err := s.gci.GetObjectChunks(r.Context(), t.ID, srcBucket, srcKey)
	if err != nil || len(srcRefs) == 0 {
		releaseReservation()
		s.logger.Error("chunked copy: source manifest unavailable",
			zap.Error(err), zap.String("bucket", srcBucket), zap.String("key", srcKey))
		WriteS3Error(w, ErrNoSuchKey, r.URL.Path, generateRequestID())
		return
	}
	srcMeta, err := s.gci.GetObjectMetadata(r.Context(), t.ID, srcBucket, srcKey)
	if err != nil {
		releaseReservation()
		s.logger.Error("chunked copy: source metadata unavailable", zap.Error(err))
		WriteS3Error(w, ErrInternalError, r.URL.Path, generateRequestID())
		return
	}

	var srcETag, srcCT, srcEncAlgo, srcDisposition string
	var srcUserMeta []byte
	if dbErr := s.db.QueryRowContext(r.Context(), `
		SELECT etag, content_type, COALESCE(encryption_algorithm, ''),
		       COALESCE(content_disposition, ''), COALESCE(metadata, '{}')
		FROM object_head_cache
		WHERE tenant_id = $1 AND bucket = $2 AND object_key = $3`,
		t.ID, srcBucket, srcKey).Scan(&srcETag, &srcCT, &srcEncAlgo, &srcDisposition, &srcUserMeta); dbErr != nil {
		releaseReservation()
		WriteS3Error(w, ErrNoSuchKey, r.URL.Path, generateRequestID())
		return
	}

	contentType := resolveCopyContentType(directive, r.Header.Get("Content-Type"), srcCT)
	destUserMeta := srcUserMeta
	destDisposition := srcDisposition
	if strings.EqualFold(directive, "REPLACE") {
		userMeta := extractS3Metadata(r)
		if vErr := validateMetadata(userMeta); vErr != nil {
			releaseReservation()
			WriteS3Error(w, ErrInvalidRequest, r.URL.Path, generateRequestID())
			return
		}
		destUserMeta, _ = json.Marshal(userMeta)
		destDisposition = sanitizeContentDisposition(r.Header.Get("Content-Disposition"))
	}

	// Build the destination manifest: same chunks, new object identity.
	destRefs := make([]crypto.TenantChunkRef, len(srcRefs))
	for i, ref := range srcRefs {
		destRefs[i] = ref
		destRefs[i].BucketName = destBucket
		destRefs[i].ObjectKey = destKey
	}

	physicalSize := int64(0) // a copy adds no new physical bytes
	dedupRatio := float32(0)
	displaced, dbErr := atomicHeadUpsert(r.Context(), s.db, t.ID, destBucket, destKey, func(tx *sql.Tx) error {
		// Each destination ref is a new reference to its chunk. Incremented
		// inside the same tx so a failed install leaves counts untouched.
		for _, ref := range destRefs {
			scope := ref.DedupScope
			if scope == "" {
				scope = crypto.GlobalDedupScope
			}
			if _, incErr := tx.ExecContext(r.Context(),
				`SELECT increment_chunk_ref($1, $2)`, scope, ref.PlaintextHash); incErr != nil {
				return fmt.Errorf("increment chunk ref %s: %w", ref.PlaintextHash, incErr)
			}
		}
		if repErr := s.gci.ReplaceObjectManifestTx(r.Context(), tx, t.ID, destBucket, destKey, destRefs, &crypto.ObjectMeta{
			TenantID:     t.ID,
			BucketName:   destBucket,
			ObjectKey:    destKey,
			TotalSize:    srcMeta.TotalSize,
			ChunkCount:   srcMeta.ChunkCount,
			ContentType:  &contentType,
			LogicalSize:  srcMeta.LogicalSize,
			PhysicalSize: &physicalSize,
			DedupRatio:   &dedupRatio,
		}); repErr != nil {
			return fmt.Errorf("install destination manifest: %w", repErr)
		}
		_, execErr := tx.ExecContext(r.Context(), `
			INSERT INTO object_head_cache
				(tenant_id, bucket, object_key, size_bytes, etag, content_type, backend_name, metadata, encryption_algorithm, content_disposition, is_chunked, updated_at)
			VALUES ($1, $2, $3, $4, $5, $6, '', $7, $8, $9, TRUE, NOW())
			ON CONFLICT (tenant_id, bucket, object_key) DO UPDATE SET
				size_bytes            = EXCLUDED.size_bytes,
				etag                  = EXCLUDED.etag,
				content_type          = EXCLUDED.content_type,
				backend_name          = EXCLUDED.backend_name,
				metadata              = EXCLUDED.metadata,
				encryption_algorithm  = EXCLUDED.encryption_algorithm,
				content_disposition   = EXCLUDED.content_disposition,
				is_chunked            = EXCLUDED.is_chunked,
				updated_at            = NOW()
		`, t.ID, destBucket, destKey, srcMeta.LogicalSize, srcETag, contentType,
			destUserMeta, srcEncAlgo, destDisposition)
		return execErr
	})
	if dbErr != nil {
		releaseReservation()
		s.logger.Error("chunked copy: install failed",
			zap.Error(dbErr),
			zap.String("dest_bucket", destBucket), zap.String("dest_key", destKey))
		WriteS3Error(w, ErrInternalError, r.URL.Path, generateRequestID())
		return
	}

	// A stale whole-object blob at the destination key (previously a plain
	// object) must not survive the overwrite — same invariant as chunked PUT.
	if blobErr := s.engine.Delete(r.Context(), t.NamespaceContainer(destBucket), destKey); blobErr != nil &&
		!strings.Contains(blobErr.Error(), "no such file or directory") &&
		!strings.Contains(blobErr.Error(), "not found") {
		s.logger.Warn("chunked copy: stale destination blob delete failed",
			zap.Error(blobErr), zap.String("bucket", destBucket), zap.String("key", destKey))
	}

	if quotaOn {
		ctx, cancel := quotaCtx(r)
		s.settlePutQuota(ctx, t.ID, reservedBytes, srcMeta.LogicalSize, displaced)
		cancel()
	}

	now := time.Now().UTC()
	result := CopyObjectResult{
		ETag:         fmt.Sprintf(`"%s"`, srcETag),
		LastModified: now.Format("2006-01-02T15:04:05.000Z"),
	}
	xmlData, xmlErr := xml.MarshalIndent(result, "", "  ")
	if xmlErr != nil {
		s.logger.Error("chunked copy: XML marshal failed", zap.Error(xmlErr))
		WriteS3Error(w, ErrInternalError, r.URL.Path, generateRequestID())
		return
	}
	w.Header().Set("Content-Type", "application/xml")
	w.Header().Set("x-amz-request-id", generateRequestID())
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(xml.Header))
	_, _ = w.Write(xmlData)
}
