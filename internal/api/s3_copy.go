package api

import (
	"crypto/md5" // #nosec G501 — S3 spec requires MD5 for ETags
	"database/sql"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/FairForge/vaultaire/internal/crypto"
	"github.com/FairForge/vaultaire/internal/engine"
	"github.com/FairForge/vaultaire/internal/tenant"
	"github.com/FairForge/vaultaire/internal/usage"
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

// resolveCopyAttrs applies x-amz-metadata-directive to the whole attribute
// set, not just Content-Type (R3-06: COPY used to drop the source's
// metadata, disposition and cache headers; REPLACE ignored the request's
// x-amz-meta-*). request is already validated by objectAttrsFromRequest.
func resolveCopyAttrs(directive string, request, source objectAttrs) objectAttrs {
	if strings.EqualFold(directive, "REPLACE") {
		return request
	}
	return source
}

// resolveCopyTags applies x-amz-tagging-directive (independent of the
// metadata directive, as on AWS): COPY (default) carries the source's tags,
// REPLACE takes the request's x-amz-tagging (R4-06).
func resolveCopyTags(taggingDirective string, request, source objectAttrs) map[string]string {
	if strings.EqualFold(taggingDirective, "REPLACE") {
		return request.Tags
	}
	return source.Tags
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
	if s.freeTierBucketCapBlocksWrite(r.Context(), t.ID, destBucket) {
		writeFreeTierBucketCap(w, r)
		return
	}
	destKey := req.Object

	// Object Lock on the DESTINATION, before anything is read or written: the
	// copy replaces the key's bytes in place, so a copy over a
	// COMPLIANCE-retained key destroyed it (R3-01, live-proven).
	if lockErr := checkObjectLock(r.Context(), s.db, t.ID, destBucket, destKey, isObjectLockBypass(r)); lockErr != nil {
		WriteS3ErrorWithContext(w, ErrAccessDenied, r.URL.Path, generateRequestID(),
			WithSuggestion(lockDeniedHint(r)))
		return
	}

	// Everything the request itself can be refused for is checked before the
	// source is opened or the destination touched (R2-05).
	directive := r.Header.Get("x-amz-metadata-directive")
	requestAttrs, err := objectAttrsFromRequest(r)
	if err != nil {
		WriteS3ErrorWithContext(w, ErrInvalidRequest, r.URL.Path, generateRequestID(), WithSuggestion(err.Error()))
		return
	}

	srcContainer := t.NamespaceContainer(srcBucket)
	destContainer := t.NamespaceContainer(destBucket)

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
	var srcBackend string
	var srcFloor string
	if s.db != nil {
		_ = s.db.QueryRowContext(r.Context(), `
			SELECT size_bytes, COALESCE(encryption_algorithm, ''), is_chunked, COALESCE(backend_name, ''), floor
			FROM object_head_cache
			WHERE tenant_id = $1 AND bucket = $2 AND object_key = $3`,
			t.ID, srcBucket, srcKey).Scan(&srcSize, &srcEnc, &srcChunked, &srcBackend, &srcFloor)
	}
	if srcChunked && s.gci != nil {
		// A manifest copy moves no data: the chunks stay at their one
		// address on the primary (WP-R8-7). A destination bucket pinned to
		// a region promised where its bytes live (WP-R7-1), so it gets what
		// every other write into it gets: a refusal when the region has no
		// driver, and — until a chunked source can be re-stored whole in the
		// region — 501 rather than a copy whose bytes are elsewhere.
		regionDriver, rErr := bucketRegionDriver(r.Context(), s.db, s.engine, t.ID, destBucket)
		if rErr != nil {
			s.logger.Error("copy: region-pinned bucket has no driver — refused",
				zap.String("bucket", destBucket), zap.Error(rErr))
			w.Header().Set("Retry-After", "300")
			WriteS3ErrorWithContext(w, ErrServiceUnavailable, r.URL.Path, generateRequestID(),
				WithSuggestion("This bucket's region is not enabled on this deployment."))
			return
		}
		if regionDriver != "" {
			WriteS3ErrorWithContext(w, ErrNotImplemented, r.URL.Path, generateRequestID(),
				WithSuggestion("Copying a large (chunked) object into a bucket pinned to a region is not supported yet. Download it and upload it into that bucket."))
			return
		}
		s.handleChunkedCopy(w, r, t, srcBucket, srcKey, destBucket, destKey, srcSize, directive, requestAttrs)
		return
	}
	if srcEnc != "" || srcChunked {
		WriteS3ErrorWithContext(w, ErrNotImplemented, r.URL.Path, generateRequestID(),
			WithSuggestion("Copying encrypted or chunked objects is not yet supported. Download and re-upload instead."))
		return
	}

	// The source's recorded attributes (COPY directive) — read before the
	// stream so a self-copy with REPLACE sees the pre-write row.
	sourceAttrs := objectAttrs{ContentType: "application/octet-stream"}
	if s.db != nil {
		if a, aErr := loadHeadAttrs(r.Context(), s.db, t.ID, srcBucket, srcKey); aErr == nil {
			sourceAttrs = a
		}
	}
	attrs := resolveCopyAttrs(directive, requestAttrs, sourceAttrs)
	attrs.Tags = resolveCopyTags(r.Header.Get("x-amz-tagging-directive"), requestAttrs, sourceAttrs)

	// Route the read to the backend that holds the source (routing truth).
	if srcBackend != "" && s.engine != nil {
		s.engine.HintBackend(srcContainer, srcKey, srcBackend)
	}
	reader, err := s.engine.Get(r.Context(), srcContainer, srcKey)
	if err != nil {
		switch {
		case errors.Is(err, engine.ErrAllBackendsUnavailable):
			w.Header().Set("Retry-After", "30")
			WriteS3Error(w, ErrServiceUnavailable, r.URL.Path, generateRequestID())
		case errors.Is(err, engine.ErrArchived):
			// The source's bytes are on tape (this was a 500). Same rule as
			// GET (WP-R13-1): a downstairs source is recalled on the
			// caller's behalf and the copy answers a retryable 503; an attic
			// source answers what AWS answers for a GLACIER copy source.
			writeArchivedRead(w, r, s.smartPromoter, t.ID, srcBucket, srcKey, srcFloor,
				"The source object is archived on tape. Restore it (POST ?restore), then retry the copy.")
		case isObjectMissingErr(err):
			WriteS3Error(w, ErrNoSuchKey, r.URL.Path, generateRequestID())
		default:
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
	// The destination's class decides both where the copy lands and which
	// floor it is billed on (header, then the destination bucket's tier).
	destClass := resolvePutStorageClass(r.Context(), s.db, s.engine, t.ID, destBucket,
		r.Header.Get("x-amz-storage-class"))
	floor := usage.FloorOf(destClass)
	quotaOn := s.quotaManager != nil
	var reservedBytes int64
	if quotaOn {
		if srcSize > 0 {
			ok, qErr := reserveQuota(r.Context(), s.quotaManager, t.ID, floor, srcSize)
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
	releaseReservation := func() {
		if quotaOn && reservedBytes > 0 {
			ctx, cancel := quotaCtx(r)
			s.releaseQuota(ctx, t.ID, floor, reservedBytes)
			cancel()
		}
	}

	// Stream source → MD5 hasher → destination, tallying bytes as we go so
	// the persisted size never depends on the source cache row being present.
	counter := &countingReader{r: reader}
	hasher := md5.New() // #nosec G401 — S3 spec requires MD5 for ETags
	tee := io.TeeReader(counter, hasher)

	putOpts := []engine.PutOption{engine.WithContentType(attrs.ContentType)}
	if destClass != "" {
		putOpts = append(putOpts, engine.WithStorageClass(destClass))
	}
	// Placement is the shared helper: a region-pinned destination bucket
	// goes to its region driver or is refused (R3-08 — copy used to write
	// the primary under a residency label).
	backendName, err := placeObject(r.Context(), s.db, s.engine, t.ID, destBucket, destContainer, destKey, tee, putOpts...)
	if err != nil {
		releaseReservation()
		switch {
		case errors.Is(err, errRegionDriverUnavailable):
			s.logger.Error("copy: region-pinned bucket has no driver — refused",
				zap.String("bucket", destBucket), zap.Error(err))
			w.Header().Set("Retry-After", "300")
			WriteS3ErrorWithContext(w, ErrServiceUnavailable, r.URL.Path, generateRequestID(),
				WithSuggestion("This bucket's region is not enabled on this deployment."))
		case errors.Is(err, engine.ErrAllBackendsUnavailable):
			w.Header().Set("Retry-After", "30")
			WriteS3Error(w, ErrServiceUnavailable, r.URL.Path, generateRequestID())
		default:
			s.logger.Error("copy: dest put failed",
				zap.Error(err),
				zap.String("container", destContainer),
				zap.String("key", destKey))
			WriteS3Error(w, ErrInternalError, r.URL.Path, generateRequestID())
		}
		return
	}

	etag := fmt.Sprintf("%x", hasher.Sum(nil))
	now := time.Now().UTC()

	// Update object_head_cache for the copied object. Every attribute column
	// is written (R3-05: the old upsert left the displaced object's
	// encryption_algorithm on the row, so a plain copy over an SSE key was
	// served through the decryptor — live GET 500). A failed head write is a
	// 500, never a 200 with no row (R10 ledger row / R3-10): the blob is
	// durable and the client's retry is idempotent.
	var displaced displacedRow
	if s.db != nil {
		var dbErr error
		displaced, dbErr = atomicHeadUpsertReleasing(r.Context(), s.db, manifestReleaser(s.gci), t.ID, destBucket, destKey, func(tx *sql.Tx) error {
			return upsertWholeObjectHeadRow(r.Context(), tx, t.ID, destBucket, destKey, counter.n, etag, backendName, floor, attrs)
		})
		if dbErr != nil {
			releaseReservation()
			s.logger.Error("copy: head row write failed — failing the request",
				zap.Error(dbErr),
				zap.String("tenant_id", t.ID),
				zap.String("bucket", destBucket),
				zap.String("key", destKey))
			WriteS3Error(w, ErrInternalError, r.URL.Path, generateRequestID())
			return
		}
	}

	if quotaOn {
		ctx, cancel := quotaCtx(r)
		s.settlePutQuota(ctx, t.ID, floor, reservedBytes, counter.n, displaced)
		cancel()
	}

	// The destination's previous blob on another backend (a demoted object
	// copied over, R13-10) — exactly as plain PUT.
	dropDisplacedBlob(r.Context(), s.db, s.engine, s.logger, lostWriteOverwrite,
		t.ID, destBucket, t.NamespaceContainer(destBucket), destKey, displaced, backendName)

	versionID := recordObjectVersion(r.Context(), s.db, t.ID, destBucket, destKey, counter.n, etag, attrs.ContentType, backendName)
	applyObjectLockOnPut(r.Context(), s.db, t.ID, destBucket, destKey, r)

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
	if versionID != "" {
		w.Header().Set("x-amz-version-id", versionID)
	}
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
// handleChunkedPut after review-A). The destination's Object Lock was checked
// by the caller before the source was looked at.
func (s *Server) handleChunkedCopy(w http.ResponseWriter, r *http.Request,
	t *tenant.Tenant, srcBucket, srcKey, destBucket, destKey string,
	srcSize int64, directive string, requestAttrs objectAttrs) {

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
	// Chunk blobs always live on the engine's primary backend, so a chunked
	// copy is billed downstairs whatever the destination bucket's tier.
	quotaOn := s.quotaManager != nil
	var reservedBytes int64
	if quotaOn && srcSize > 0 {
		ok, qErr := reserveQuota(r.Context(), s.quotaManager, t.ID, chunkedObjectFloor, srcSize)
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
			s.releaseQuota(ctx, t.ID, chunkedObjectFloor, reservedBytes)
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

	contentType := resolveCopyContentType(directive, requestAttrs.ContentType, srcCT)
	destUserMeta := srcUserMeta
	destDisposition := srcDisposition
	if strings.EqualFold(directive, "REPLACE") {
		destUserMeta, _ = json.Marshal(requestAttrs.Metadata)
		destDisposition = requestAttrs.ContentDisposition
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
			// The copy's chunks are the source's: cut by the chunker the
			// source records (the product has only ever cut with the
			// default one, so a source without a record gets that).
			PipelineConfig: copiedPipeline(srcMeta),
		}); repErr != nil {
			return fmt.Errorf("install destination manifest: %w", repErr)
		}
		_, execErr := tx.ExecContext(r.Context(), `
			INSERT INTO object_head_cache
				(tenant_id, bucket, object_key, size_bytes, etag, content_type, backend_name, metadata, encryption_algorithm, content_disposition, floor, is_chunked, updated_at)
			VALUES ($1, $2, $3, $4, $5, $6, '', $7, $8, $9, $10, TRUE, NOW())
			ON CONFLICT (tenant_id, bucket, object_key) DO UPDATE SET
				size_bytes            = EXCLUDED.size_bytes,
				etag                  = EXCLUDED.etag,
				content_type          = EXCLUDED.content_type,
				backend_name          = EXCLUDED.backend_name,
				metadata              = EXCLUDED.metadata,
				encryption_algorithm  = EXCLUDED.encryption_algorithm,
				content_disposition   = EXCLUDED.content_disposition,
				floor                 = EXCLUDED.floor,
				is_chunked            = EXCLUDED.is_chunked,
				updated_at            = NOW()
		`, t.ID, destBucket, destKey, srcMeta.LogicalSize, srcETag, contentType,
			destUserMeta, srcEncAlgo, destDisposition, chunkedObjectFloor)
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
	// The displaced row's backend is the hint (R2-20 / WP-R6-1); a miss in
	// any driver's shape is fine (R6-25).
	if displaced.Backend != "" && s.engine != nil {
		s.engine.HintBackend(t.NamespaceContainer(destBucket), destKey, displaced.Backend)
	}
	if blobErr := s.engine.Delete(r.Context(), t.NamespaceContainer(destBucket), destKey); blobErr != nil &&
		!isObjectMissingErr(blobErr) {
		s.logger.Warn("chunked copy: stale destination blob delete failed",
			zap.Error(blobErr), zap.String("bucket", destBucket), zap.String("key", destKey))
	}

	if quotaOn {
		ctx, cancel := quotaCtx(r)
		s.settlePutQuota(ctx, t.ID, chunkedObjectFloor, reservedBytes, srcMeta.LogicalSize, displaced)
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
