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
	"github.com/prometheus/client_golang/prometheus"
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

// errCopySourceSize: a copy source whose bytes are not the size its head
// row records — another version of the key, or a drifted row. The copy
// fails rather than store those bytes under a new ETag: 503 + Retry-After
// (a retry after the row is repaired succeeds), counted in
// vaultaire_copy_source_size_mismatch_total (Prompt 2b.3 D2.1).
var errCopySourceSize = errors.New("the copy source's bytes are not its recorded size")

var copySourceSizeMismatch = prometheus.NewCounter(prometheus.CounterOpts{
	Name: "vaultaire_copy_source_size_mismatch_total",
	Help: "CopyObject requests refused because the source's bytes were not the size its head row records (a drifted row, or the key overwritten between the row read and the read of its bytes); answered 503.",
})

// exactSizeReader fails a stream that ends before, or runs past, want
// bytes: the driver sees a broken body and stores nothing. A sized writer
// reads exactly want bytes and never reads on (the S3-class single part,
// a stripe's pieces), so the read that reaches want probes the source for
// one more byte before it returns — excess fails it with no byte of that
// read released, which a writer using io.ReadFull would otherwise keep
// (Prompt 2b.3 D2.1: a 20-byte source recorded as 10 was stored as its
// 10-byte prefix, 200). The source is a backend's GET body, framed: the
// probe gets EOF at once.
//
// Invariant: r is a FRAMED backend body (engine.Get's reader), never a
// client's request body. The one-byte probe blocks until the source says
// EOF; a body that withholds EOF (a client that keeps its connection open
// after its declared bytes) would hold the copy until the read deadline.
// copyObject is its only user — pinned by
// TestExactSizeReader_OnlyUserIsTheCopyPath (2b.4 E2.5).
type exactSizeReader struct {
	r    io.Reader
	want int64
	n    int64
}

func (e *exactSizeReader) Read(p []byte) (int, error) {
	n, err := e.r.Read(p)
	e.n += int64(n)
	if e.n > e.want {
		return 0, fmt.Errorf("%w: more than %d bytes", errCopySourceSize, e.want)
	}
	if errors.Is(err, io.EOF) && e.n < e.want {
		return n, fmt.Errorf("%w: %d of %d bytes", errCopySourceSize, e.n, e.want)
	}
	if e.n == e.want && err == nil {
		var one [1]byte
		for i := 0; i < 3; i++ { // a reader may answer (0, nil): ask again
			m, perr := e.r.Read(one[:])
			if m > 0 {
				e.n += int64(m)
				return 0, fmt.Errorf("%w: more than %d bytes", errCopySourceSize, e.want)
			}
			if perr != nil {
				if errors.Is(perr, io.EOF) {
					return n, io.EOF
				}
				return 0, perr
			}
		}
	}
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
//
// The copy runs under the long-operation keep-alive (s3_long_op.go): the
// destination is ONE backend PUT, which on a slow backend (the sync tier,
// ~30 MB/s per object) outlasts Cloudflare's 100 s origin timeout. Every
// refusal (auth, copy source, free-tier cap, object lock, preconditions,
// missing source, quota) is decided before the destination write starts and
// keeps its own status however long it took — the keep-alive threshold runs
// from longOpBegin, after the last refusal;
// a copy still writing after the threshold answers 200 + whitespace +
// CopyObjectResult (or an <Error> document), and finishes even if the client
// goes away. x-amz-version-id is known only after the copy and is not
// delivered on that slow path (the ETag is in the body).
func (s *Server) handleCopyObject(w http.ResponseWriter, r *http.Request, req *S3Request) {
	s.runLongS3Op(w, r, longOpInfo{Op: longOpCopy, Bucket: req.Bucket, Key: req.Object}, func(w http.ResponseWriter, r *http.Request) {
		s.copyObject(w, r, req)
	})
}

// copyObject is CopyObject itself (see handleCopyObject).
func (s *Server) copyObject(w http.ResponseWriter, r *http.Request, req *S3Request) {
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
	// A copy into a bucket that does not exist is NoSuchBucket, before the
	// keep-alive can commit a 200 (it used to answer 200 + a result).
	if exists, bErr := s.longOpBucketExists(r, t.ID, destBucket); bErr != nil {
		s.log().Error("copy: bucket lookup failed", zap.Error(bErr), zap.String("bucket", destBucket))
		WriteS3Error(w, ErrInternalError, r.URL.Path, generateRequestID())
		return
	} else if !exists {
		s.writeNoSuchBucket(w, r, t.ID, destBucket)
		return
	}

	// The destination key must leave room for its blob address (WP-R2-1).
	if why, tooLong := blobKeyTooLong(r.Context(), s.db, t.ID, destBucket, destKey); tooLong {
		WriteS3ErrorWithContext(w, ErrKeyTooLong, r.URL.Path, generateRequestID(), WithSuggestion(why))
		return
	}

	// Object Lock on the DESTINATION, before anything is read or written: the
	// copy replaces the key's bytes in place, so a copy over a
	// COMPLIANCE-retained key destroyed it (R3-01, live-proven).
	if lockErr := checkObjectLock(r.Context(), s.db, t.ID, destBucket, destKey, isObjectLockBypass(r)); lockErr != nil {
		writeObjectLockRefusal(w, r, s.log(), lockErr)
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
	var srcFloor, srcStorageKey string
	if s.db != nil {
		_ = s.db.QueryRowContext(r.Context(), `
			SELECT size_bytes, COALESCE(encryption_algorithm, ''), is_chunked, COALESCE(backend_name, ''), floor, COALESCE(storage_key, '')
			FROM object_head_cache
			WHERE tenant_id = $1 AND bucket = $2 AND object_key = $3`,
			t.ID, srcBucket, srcKey).Scan(&srcSize, &srcEnc, &srcChunked, &srcBackend, &srcFloor, &srcStorageKey)
	}
	// The source's bytes are at its row's address (WP-R2-1).
	srcBlob := blobArtifact(srcKey, srcStorageKey)
	// A plaintext source copied into an sse_enabled bucket is sealed there
	// exactly as a PUT into it is (WP-R8-1 follow-up A1: it used to land
	// plaintext). Refused before anything is read or written when it cannot
	// be: no master key on this process, or over the SSE-S3 limit.
	destSSE, sseErr := bucketSSEEnabled(r.Context(), s.db, t.ID, destBucket)
	if sseErr != nil {
		s.logger.Error("copy: destination encryption setting unreadable", zap.Error(sseErr), zap.String("bucket", destBucket))
		WriteS3Error(w, ErrInternalError, r.URL.Path, generateRequestID())
		return
	}
	// The destination's class decides both where the copy lands and which
	// floor it is billed on (header, then the destination bucket's tier).
	destClass := resolvePutStorageClass(r.Context(), s.db, s.engine, syncPlacementGate(s.flags), t.ID, destBucket,
		r.Header.Get("x-amz-storage-class"))
	// A chunked source is copied as a manifest only where a PUT of its
	// bytes would be chunked too (placement.go, WP-R3-1); a versioned, region-
	// pinned or non-chunkable destination (archive, RESILIENT, PUBLIC, SYNC)
	// gets the bytes re-stored whole, read through the content index
	// (chunked_read.go) — a manifest there broke the bucket's promise (a
	// version with no version row, a tier's object on the primary). Before,
	// every chunked copy was a manifest copy whatever the destination.
	sourceWhole := false
	if srcChunked && s.gci != nil {
		dp := s.writeAdapter().decidePlacement(r.Context(), placementRequest{
			TenantID: t.ID, Bucket: destBucket, Class: destClass, Size: srcSize,
			SSERequested: r.Header.Get("x-amz-server-side-encryption") == "AES256",
		})
		if dp.Refusal != nil {
			dp.Refusal.write(w, r)
			return
		}
		sourceWhole = dp.Kind != placeChunked
	}
	if srcChunked && s.gci != nil && !sourceWhole {
		// A manifest copy moves no data: the chunks stay at their one
		// address on the primary (WP-R8-7). A destination bucket pinned to
		// a region never gets here (it is not chunkable: the source is
		// re-stored whole in the region); the region's driver is asked
		// again here as every write into it is.
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
		if destSSE && srcEnc == "" {
			// A manifest copy moves no bytes: the destination would name
			// the source's PLAINTEXT chunks from an encrypted bucket.
			WriteS3ErrorWithContext(w, ErrNotImplemented, r.URL.Path, generateRequestID(),
				WithSuggestion("Copying a large unencrypted object into a bucket that encrypts at rest is not supported yet. Download it and upload it into that bucket."))
			return
		}
		s.handleChunkedCopy(w, r, t, srcBucket, srcKey, destBucket, destKey, srcSize, directive, requestAttrs)
		return
	}
	// An SSE-S3 whole source opens as it streams (WP-R8-5): its plaintext is
	// the copy's source, re-sealed or not as the destination's placement
	// says. Before, every sealed source answered 501 — after the master-key
	// ceremony every object a new bucket holds is sealed or chunk-encrypted.
	// SSE-C needs the customer's key on the copy (x-amz-copy-source-server-
	// side-encryption-customer-*): still 501.
	srcSealedS3 := !srcChunked && srcEnc != "" && !crypto.IsSSEC(srcEnc)
	if (srcEnc != "" && !sourceWhole && !srcSealedS3) || (srcChunked && !sourceWhole) {
		WriteS3ErrorWithContext(w, ErrNotImplemented, r.URL.Path, generateRequestID(),
			WithSuggestion("Copying objects encrypted with a customer-provided key (SSE-C) is not yet supported. Download and re-upload instead."))
		return
	}
	if srcSealedS3 && s.sseService == nil {
		s.logger.Error("copy of an encrypted source with no SSE service: ENCRYPTION_MASTER_KEY is not configured on this process",
			zap.String("bucket", srcBucket), zap.String("key", srcKey))
		WriteS3Error(w, ErrInternalError, r.URL.Path, generateRequestID())
		return
	}

	if destSSE && s.sseService == nil {
		s.logger.Error("copy into an encrypted bucket refused: ENCRYPTION_MASTER_KEY is not configured on this process",
			zap.String("bucket", destBucket), zap.String("key", destKey))
		w.Header().Set("Retry-After", "30")
		WriteS3Error(w, ErrServiceUnavailable, r.URL.Path, generateRequestID())
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
		noteRecordedBackend(s.engine, s.logger, "copy_source", srcBackend)
		s.engine.HintBackend(srcContainer, srcBlob, srcBackend)
	}
	// The source's recorded size goes with the read and the write (Prompt
	// 2b.2 C3): a backend holding two versions of the key after an
	// interrupted commit (the multi-bridge Sync driver's plain file and
	// striped manifest) serves the one of this size — the copy took the
	// stale plain bytes and recorded their MD5 as the new object — and a
	// destination of known length is striped over the Sync bridges (an
	// unsized one was one plain file on one bridge, ~30 MB/s).
	gctx := r.Context()
	if srcSize > 0 {
		stored := srcSize
		if srcSealedS3 {
			stored = s.sseService.SealedLength(srcSize)
		}
		gctx = engine.WithExpectedSize(gctx, stored)
	}
	var reader io.ReadCloser
	if srcSealedS3 {
		// Opened as it streams: the header and first segment are verified
		// before anything is reserved or written (sse_stream.go).
		reader, err = openSealed(r.Context(), sseS3Opener{svc: s.sseService, tenantID: t.ID}, sealedSource{
			whole: func() (io.ReadCloser, error) { return s.engine.Get(gctx, srcContainer, srcBlob) },
		}, sealedRead{Size: srcSize})
		if errors.Is(err, errSealedUnreadable) {
			s.logger.Error("copy: the encrypted source does not open", zap.Error(err),
				zap.String("bucket", srcBucket), zap.String("key", srcKey))
			WriteS3Error(w, ErrInternalError, r.URL.Path, generateRequestID())
			return
		}
	} else if sourceWhole {
		// The chunked source as one stream: every backend its chunks are
		// on is asked first (503 before anything is reserved or written);
		// each chunk decrypted and verified as it is read.
		var cr *chunkedObjectReader
		if cr, err = s.readAdapter().openChunkedObject(r.Context(), t.ID, srcBucket, srcKey, srcSize); err == nil {
			reader = cr
		}
	} else {
		reader, err = s.engine.Get(gctx, srcContainer, srcBlob)
	}
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
			writeArchivedRead(w, r, s.smartPromoter, t.ID, srcBucket, srcKey, srcBlob, srcFloor,
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
					WithSuggestion("Upgrade at "+billingURL()))
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
					WithSuggestion("Upgrade at "+billingURL()))
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

	// Every refusal is behind us: from here the keep-alive may commit.
	longOpBegin(r)

	// Stream source → MD5 hasher → destination, tallying bytes as we go so
	// the persisted size never depends on the source cache row being present.
	// The source stream is the write's body: a shutdown's cut stops it at
	// once (the destination stores nothing), the call may still answer for
	// longOpAnswerGrace (s3_long_op.go).
	// The source's own read failure (a backend GET body that stalled):
	// engine.Put tags any source error as the caller's — right for a
	// client's body, wrong here, so the copy answers 503, not 500 (Prompt
	// 2b.3 D1.6).
	source := &readErrReader{r: reader}
	counter := &countingReader{r: &cutReader{ctx: r.Context(), r: source}}
	var src io.Reader = counter
	if srcSize > 0 || destSSE {
		// Bytes that are not the recorded size are not the source: the
		// write fails rather than store them under a new ETag. Into an
		// sse_enabled bucket a source with no recorded size must be empty:
		// a seal needs its size, and its bytes are never stored plaintext
		// there (the copy answers the size drift's 503).
		src = &exactSizeReader{r: counter, want: srcSize}
	}
	hasher := md5.New() // #nosec G401 — S3 spec requires MD5 for ETags
	tee := io.TeeReader(src, hasher)
	var sealer *sseWriteSealer
	putLength := srcSize
	if destSSE && srcSize > 0 {
		// The hasher and the counter see the plaintext; the backend gets
		// the blob sealed as it streams (WP-R8-5), of a length known in
		// advance. An empty source is stored empty, as an empty PUT is.
		var sErr error
		if sealer, sErr = newSSEWriteSealer(r.Context(), s.sseService, t.ID, tee, srcSize); sErr != nil {
			releaseReservation()
			s.logger.Error("copy: SSE-S3 tenant key unavailable", zap.Error(sErr))
			WriteS3Error(w, ErrInternalError, r.URL.Path, generateRequestID())
			return
		}
		tee = sealer
		putLength = s.sseService.SealedLength(srcSize)
	}

	putOpts := []engine.PutOption{engine.WithContentType(attrs.ContentType)}
	if putLength > 0 {
		putOpts = append(putOpts, engine.WithContentLength(putLength))
	}
	if destClass != "" {
		putOpts = append(putOpts, engine.WithStorageClass(destClass))
	}
	// Placement is the shared helper: a region-pinned destination bucket
	// goes to its region driver or is refused (R3-08 — copy used to write
	// the primary under a residency label).
	// A new address (WP-R2-1): the destination's current object — the
	// source itself, for a copy onto its own key — is never written over.
	destBlob, addrErr := blobAddressFor(s.db, destKey)
	if addrErr != nil {
		releaseReservation()
		s.logger.Error("copy: no blob address", zap.Error(addrErr))
		WriteS3Error(w, ErrInternalError, r.URL.Path, generateRequestID())
		return
	}
	putCtx, putCancel := backendCallCtx(r.Context(), s.longOpAnswerGrace())
	backendName, err := placeObject(putCtx, s.db, s.engine, t.ID, destBucket, destContainer, destBlob, tee, putOpts...)
	putCancel()
	if err != nil {
		releaseReservation()
		switch {
		case errors.Is(err, errCopySourceSize):
			// The source's head row and its bytes disagree: drift, or the key
			// overwritten between the row read and the read of the bytes. A
			// retry after the row is repaired (or the overwrite settled) works.
			copySourceSizeMismatch.Inc()
			s.logger.Warn("copy: the source's bytes are not its recorded size (head-row drift or a concurrent overwrite) — 503",
				zap.String("tenant_id", t.ID), zap.String("source_bucket", srcBucket), zap.String("source_key", srcKey),
				zap.Int64("recorded_size", srcSize), zap.Error(err))
			w.Header().Set("Retry-After", "30")
			WriteS3Error(w, ErrServiceUnavailable, r.URL.Path, generateRequestID())
		case errors.Is(err, errRegionDriverUnavailable):
			s.logger.Error("copy: region-pinned bucket has no driver — refused",
				zap.String("bucket", destBucket), zap.Error(err))
			w.Header().Set("Retry-After", "300")
			WriteS3ErrorWithContext(w, ErrServiceUnavailable, r.URL.Path, generateRequestID(),
				WithSuggestion("This bucket's region is not enabled on this deployment."))
		case errors.Is(err, engine.ErrAllBackendsUnavailable):
			w.Header().Set("Retry-After", "30")
			WriteS3Error(w, ErrServiceUnavailable, r.URL.Path, generateRequestID())
		case source.err != nil && (errors.Is(source.err, crypto.ErrStreamCorrupt) || errors.Is(source.err, errSealedUnreadable)):
			// A sealed source that does not open (a truncated or corrupt
			// blob) is an integrity failure, as on the GET: 500 and an
			// Error line — a 503 would be retried for ever (R85-10).
			s.logger.Error("copy: the sealed source does not open — 500",
				zap.String("tenant_id", t.ID), zap.String("source_bucket", srcBucket), zap.String("source_key", srcKey),
				zap.NamedError("source_error", source.err))
			WriteS3Error(w, ErrInternalError, r.URL.Path, generateRequestID())
		case source.err != nil && r.Context().Err() == nil:
			s.logger.Warn("copy: the source could not be read — 503",
				zap.String("tenant_id", t.ID), zap.String("source_bucket", srcBucket), zap.String("source_key", srcKey),
				zap.NamedError("source_error", source.err))
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

	// The destination holds the bytes: the bookkeeping runs to the end.
	pctx, pcancel := postCommit(r.Context())
	defer pcancel()

	// Update object_head_cache for the copied object through the pointer
	// flip (WP-R2-1): the head row names the new address, the version row is
	// written in the same transaction, the displaced blob is retired. Every
	// attribute column is written (R3-05). A failed head write is a 500,
	// never a 200 with no row (R3-10): the destination is untouched and the
	// new blob is queued as an orphan.
	var displaced displacedRow
	var versionID string
	if s.db != nil {
		enc := sealer.encryption()
		flip, dbErr := commitBlobWrite(pctx, s.db, manifestReleaser(s.gci), blobWrite{TenantID: t.ID, Bucket: destBucket, Key: destKey,
			Version: &versionWrite{Size: counter.n, ETag: etag, ContentType: attrs.ContentType, Backend: backendName,
				Blob: versionBlobRef{StorageKey: destBlob, Enc: enc}},
			Upsert: func(tx *sql.Tx) error {
				return upsertWholeObjectHeadRowEnc(pctx, tx, t.ID, destBucket, destKey, destBlob, counter.n, etag, backendName, floor, attrs, enc)
			}})
		if dbErr != nil {
			releaseReservation()
			retireOrphanWrite(pctx, s.db, s.logger, t.ID, destBucket, destBlob, backendName)
			s.logger.Error("copy: head row write failed — failing the request",
				zap.Error(dbErr),
				zap.String("tenant_id", t.ID),
				zap.String("bucket", destBucket),
				zap.String("key", destKey))
			WriteS3Error(w, ErrInternalError, r.URL.Path, generateRequestID())
			return
		}
		displaced, versionID = flip.Displaced, flip.VersionID
	}

	if quotaOn {
		s.settlePutQuota(pctx, t.ID, floor, reservedBytes, counter.n, displaced)
	}

	// The bucket's default retention.
	applyObjectLockOnPut(pctx, s.db, t.ID, destBucket, destKey, r)

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
	if sealer.encryption().Algorithm != "" {
		w.Header().Set("x-amz-server-side-encryption", "AES256")
	}
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
	// version_id — review-A F3). CopyObject never sends one here since
	// WP-R3-1 (a versioned destination is not chunkable: the source is
	// re-stored whole); kept against a status change between the two reads.
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
				WithSuggestion("Upgrade at "+billingURL()))
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

	// The destination's attributes, resolved exactly as the whole copy
	// resolves them (metadata directive, tagging directive), and written in
	// full: the head row used to set only type, metadata and disposition, so
	// an overwrite kept the displaced object's cache headers and tags.
	contentType := resolveCopyContentType(directive, requestAttrs.ContentType, srcCT)
	sourceAttrs, aErr := loadHeadAttrs(r.Context(), s.db, t.ID, srcBucket, srcKey)
	if aErr != nil {
		sourceAttrs = objectAttrs{ContentDisposition: srcDisposition}
		_ = json.Unmarshal(srcUserMeta, &sourceAttrs.Metadata)
	}
	destAttrs := resolveCopyAttrs(directive, requestAttrs, sourceAttrs)
	destAttrs.Tags = resolveCopyTags(r.Header.Get("x-amz-tagging-directive"), requestAttrs, sourceAttrs)
	destAttrs.ContentType = contentType

	// Build the destination manifest: same chunks, new object identity.
	destRefs := make([]crypto.TenantChunkRef, len(srcRefs))
	for i, ref := range srcRefs {
		destRefs[i] = ref
		destRefs[i].BucketName = destBucket
		destRefs[i].ObjectKey = destKey
	}

	physicalSize := int64(0) // a copy adds no new physical bytes
	dedupRatio := float32(0)
	// Every refusal is behind us: from here the keep-alive may commit (the
	// chunked path never said so and waited out the 60 s prelude cap).
	longOpBegin(r)
	// The install is ONE transaction — a shutdown's cut rolls it back whole;
	// once it has committed, the rest runs to the end.
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
		// The copy reports the source's ETag: no byte is read, and the
		// chunk hashes are SHA-256, not MD5 (a -N stays -N; AWS would report
		// a fresh single-part ETag — the whole copy does). The backend name
		// is the chunked sentinel, as every chunked row's.
		execErr := upsertChunkedObjectHeadRow(r.Context(), tx, t.ID, destBucket, destKey, srcMeta.LogicalSize, srcETag, srcEncAlgo, destAttrs)
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

	pctx, pcancel := postCommit(r.Context())
	defer pcancel()

	// The bucket's default retention, as on every other write (the chunked
	// copy wrote none).
	applyObjectLockOnPut(pctx, s.db, t.ID, destBucket, destKey, r)

	// A whole blob the destination held before (a plain object, at <key> or
	// at its address) was retired inside the install's transaction (the
	// flip, WP-R2-1): the collector removes it after its grace.

	if quotaOn {
		s.settlePutQuota(pctx, t.ID, chunkedObjectFloor, reservedBytes, srcMeta.LogicalSize, displaced)
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
