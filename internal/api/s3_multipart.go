package api

import (
	"context"
	"crypto/md5" // #nosec G501 — S3 spec requires MD5 for ETags
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/FairForge/vaultaire/internal/crypto"
	"github.com/FairForge/vaultaire/internal/engine"
	"github.com/FairForge/vaultaire/internal/tenant"
	"github.com/FairForge/vaultaire/internal/usage"

	"github.com/prometheus/client_golang/prometheus"
	"go.uber.org/zap"
)

const (
	// defaultMultipartStagingDir is where parts stage unless
	// MULTIPART_STAGING_DIR names another directory (multipart_staging_dir.go).
	defaultMultipartStagingDir = "/tmp/vaultaire-multipart"
	maxPartNumber              = 10000
	// maxCompleteMultipartBodyBytes bounds the CompleteMultipartUpload XML
	// body: 10,000 parts × ~150 bytes is ~1.5 MiB; S3 POST bodies are
	// otherwise unlimited (R2's requestLimitsMiddleware note).
	maxCompleteMultipartBodyBytes = 8 * 1024 * 1024
)

// In-memory fallback for when DB is not available (test mode).
// Production always uses PostgreSQL.
var (
	memUploads   = make(map[string]*memUpload)
	memUploadsMu sync.RWMutex
)

type memUpload struct {
	TenantID string
	Bucket   string
	Key      string
	Status   string // "active", "completed", "aborted"
	ETag     string // the assembled object's ETag once completed
	Parts    map[int]memPart
	Created  time.Time
	Attrs    objectAttrs
	Class    string
	// SSERequested: x-amz-server-side-encryption: AES256 on Create (089).
	SSERequested bool
}

type memPart struct {
	ETag string
	Size int64
}

// uploadIDPattern is the only shape an upload id can have: the current
// `upload-<32 hex>` (newUploadID) and the pre-R3 `upload-<unix>-<nanos>` still
// possible on rows that were in flight across the upgrade. The id names the
// staging directory under the staging root, so a request-supplied id is
// checked against this BEFORE the tenant-scoped row lookup and before any
// path is built from it — the row lookup is the authority, this is the
// belt (CodeQL go/path-injection on the staging paths).
var uploadIDPattern = regexp.MustCompile(`^upload-(?:[0-9a-f]{32}|[0-9]{1,12}-[0-9]{1,12})$`)

// validUploadID reports whether id can name a staging directory.
func validUploadID(id string) bool {
	return uploadIDPattern.MatchString(id)
}

// multipartDir returns the staging directory of an upload's parts.
func multipartDir(uploadID string) string {
	return filepath.Join(multipartStagingDir(), uploadID)
}

// partFilePath returns the staging file of one part.
func partFilePath(uploadID string, partNumber int) string {
	return filepath.Join(multipartStagingDir(), uploadID, fmt.Sprintf("part-%05d", partNumber))
}

func (s *Server) handleInitiateMultipartUpload(w http.ResponseWriter, r *http.Request, bucket, object string) {
	t, err := tenant.FromContext(r.Context())
	if err != nil || t == nil {
		WriteS3Error(w, ErrAccessDenied, r.URL.Path, generateRequestID())
		return
	}
	if s.freeTierBucketCapBlocksWrite(r.Context(), t.ID, bucket) {
		writeFreeTierBucketCap(w, r)
		return
	}
	// The bucket must exist before an upload row or a staging directory
	// does (R85-1).
	if !s.requireWriteBucket(w, r, "create multipart", t.ID, bucket) {
		return
	}

	if crypto.HasSSECHeaders(r) {
		WriteS3ErrorWithContext(w, ErrNotImplemented, r.URL.Path, generateRequestID(),
			WithSuggestion("SSE-C is not yet supported for multipart uploads."))
		return
	}
	// The key must leave room for its blob address (WP-R2-1): refused
	// before any part is uploaded.
	if why, tooLong := blobKeyTooLong(r.Context(), s.db, t.ID, bucket, object); tooLong {
		WriteS3ErrorWithContext(w, ErrKeyTooLong, r.URL.Path, generateRequestID(), WithSuggestion(why))
		return
	}

	// The object's attributes travel on THIS request (Content-Type,
	// x-amz-meta-*, Cache-Control, ..., x-amz-storage-class); Complete only
	// carries the part list. They are persisted on the upload row and written
	// to the head row at complete (R3-04). Invalid metadata is refused before
	// any state exists.
	attrs, err := objectAttrsFromRequest(r)
	if err != nil {
		WriteS3ErrorWithContext(w, ErrInvalidRequest, r.URL.Path, generateRequestID(), WithSuggestion(err.Error()))
		return
	}
	storageClass := clientStorageClass(r.Header.Get("x-amz-storage-class"))

	// An encrypted bucket, or a request asking x-amz-server-side-encryption:
	// AES256, on a process with no master key: refused before a single part
	// is staged, as a PUT is (WP-R3-1; the placement asks again at Complete —
	// the key or the bucket's setting may change in between). The header is
	// kept on the upload row (089, WP-R8-5): Complete carries only the parts.
	sseRequested := r.Header.Get("x-amz-server-side-encryption") == "AES256"
	bucketSSE, sseErr := bucketSSEEnabled(r.Context(), s.db, t.ID, bucket)
	switch {
	case sseErr != nil:
		s.logger.Error("create multipart: bucket encryption setting unreadable", zap.Error(sseErr), zap.String("bucket", bucket))
		WriteS3Error(w, ErrInternalError, r.URL.Path, generateRequestID())
		return
	case bucketSSE && s.sseService == nil:
		s.logger.Error("multipart upload into an encrypted bucket refused: ENCRYPTION_MASTER_KEY is not configured on this process",
			zap.String("bucket", bucket), zap.String("object", object))
		refuseNoMasterKey.write(w, r)
		return
	case sseRequested && s.sseService == nil:
		// The client chose the header: its refusal is no server failure
		// and no Error line any client could write at will (R85-9).
		s.logger.Warn("multipart upload asking for SSE-S3 refused: ENCRYPTION_MASTER_KEY is not configured on this process",
			zap.String("bucket", bucket), zap.String("object", object))
		markClientRefusal(r.Context())
		refuseNoMasterKeyRequested.write(w, r)
		return
	}

	uploadID, err := newUploadID()
	if err != nil {
		s.logger.Error("failed to generate upload id", zap.Error(err))
		WriteS3Error(w, ErrInternalError, r.URL.Path, generateRequestID())
		return
	}

	// Persist upload record
	if s.db != nil {
		// The floor its parts are reserved on: the class Complete's
		// placement resolves from the same header and the bucket's tier
		// (WP-R10-8 Part C). A tier changed before Complete moves the bill
		// at Complete (the settle releases here, charges there).
		reservedFloor := usage.FloorOf(resolvePutStorageClass(r.Context(), s.db, s.engine, syncPlacementGate(s.flags), t.ID, bucket, storageClass))
		_, err := s.db.ExecContext(r.Context(), `
			INSERT INTO multipart_uploads
				(upload_id, tenant_id, bucket, object_key, status, content_type, metadata, storage_class,
				 content_disposition, content_encoding, content_language, cache_control, http_expires,
				 website_redirect_location, sse_requested, reserved_floor)
			VALUES ($1, $2, $3, $4, 'active', $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15)
		`, uploadID, t.ID, bucket, object, attrs.ContentType, attrs.metadataJSON(), storageClass,
			attrs.ContentDisposition, attrs.ContentEncoding, attrs.ContentLanguage,
			attrs.CacheControl, attrs.Expires, attrs.WebsiteRedirect, sseRequested, reservedFloor)
		if err != nil {
			s.logger.Error("failed to create multipart upload record", zap.Error(err))
			WriteS3Error(w, ErrInternalError, r.URL.Path, generateRequestID())
			return
		}
	} else {
		memUploadsMu.Lock()
		memUploads[uploadID] = &memUpload{
			TenantID: t.ID,
			Bucket:   bucket,
			Key:      object,
			Status:   "active",
			Parts:    make(map[int]memPart),
			Created:  time.Now(),
			Attrs:    attrs,
			Class:    storageClass,

			SSERequested: sseRequested,
		}
		memUploadsMu.Unlock()
	}

	// Create temp directory for part files
	if err := os.MkdirAll(multipartDir(uploadID), 0700); err != nil {
		s.logger.Error("failed to create multipart temp dir", zap.Error(err))
		WriteS3Error(w, ErrInternalError, r.URL.Path, generateRequestID())
		return
	}

	s.logger.Info("initiated multipart upload",
		zap.String("bucket", bucket),
		zap.String("key", object),
		zap.String("uploadID", uploadID),
		zap.String("tenant_id", t.ID))

	// AWS answers x-amz-server-side-encryption on Create when the object
	// will be encrypted at rest: here, when the bucket or the request asks
	// for it (a key is present, or the request was refused above) — the
	// object is then sealed or chunk-encrypted at Complete.
	if bucketSSE || sseRequested {
		w.Header().Set("x-amz-server-side-encryption", "AES256")
	}
	w.Header().Set("Content-Type", "application/xml")
	if err := xml.NewEncoder(w).Encode(InitiateMultipartUploadResult{
		Bucket:   bucket,
		Key:      object,
		UploadID: uploadID,
	}); err != nil {
		s.logger.Error("failed to encode initiate response", zap.Error(err))
	}
}

// multipartUploadActive reports whether uploadID is an active upload owned by
// tenantID FOR bucket/object. Every multipart query is keyed by upload_id AND
// tenant_id: the id also names the staging directory, so an id that is not
// this tenant's must never reach the filesystem. And by the upload's own
// bucket and key (Prompt 2a.3 H3): an upload for a.bin completed at
// /bucket/b.bin used to create b.bin — AWS answers NoSuchUpload.
func (s *Server) multipartUploadActive(r *http.Request, tenantID, bucket, object, uploadID string) (bool, error) {
	if !validUploadID(uploadID) {
		return false, nil
	}
	if s.db != nil {
		var status string
		err := s.db.QueryRowContext(r.Context(), `
			SELECT status FROM multipart_uploads
			WHERE upload_id = $1 AND tenant_id = $2 AND bucket = $3 AND object_key = $4
		`, uploadID, tenantID, bucket, object).Scan(&status)
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		return status == "active", nil
	}
	memUploadsMu.RLock()
	mu, ok := memUploads[uploadID]
	memUploadsMu.RUnlock()
	return ok && mu.TenantID == tenantID && mu.Bucket == bucket && mu.Key == object && mu.Status == "active", nil
}

func (s *Server) handleUploadPart(w http.ResponseWriter, r *http.Request, bucket, object string) {
	t, err := tenant.FromContext(r.Context())
	if err != nil || t == nil {
		WriteS3Error(w, ErrAccessDenied, r.URL.Path, generateRequestID())
		return
	}

	// UploadPartCopy (x-amz-copy-source on an UploadPart) is not implemented.
	// It used to fall through here, read the EMPTY request body as the part
	// and answer 200 with the empty-string ETag — a server-side copy of a
	// large object through aws-cli / rclone completed as a 0-byte object
	// (R3-02). Fail loudly until WP-R3-2 implements it.
	if r.Header.Get("x-amz-copy-source") != "" {
		WriteS3ErrorWithContext(w, ErrNotImplemented, r.URL.Path, generateRequestID(),
			WithSuggestion("UploadPartCopy is not supported yet. Copy the object with CopyObject, or download and re-upload it."))
		return
	}

	uploadID := r.URL.Query().Get("uploadId")
	partNumberStr := r.URL.Query().Get("partNumber")

	partNumber, err := strconv.Atoi(partNumberStr)
	if err != nil || partNumber < 1 || partNumber > maxPartNumber {
		WriteS3Error(w, ErrInvalidPartNumber, r.URL.Path, generateRequestID())
		return
	}

	// Verify upload exists, is active, and belongs to this tenant
	active, err := s.multipartUploadActive(r, t.ID, bucket, object, uploadID)
	if err != nil {
		s.logger.Error("failed to query multipart upload", zap.Error(err))
		WriteS3Error(w, ErrInternalError, r.URL.Path, generateRequestID())
		return
	}
	if !active {
		WriteS3Error(w, ErrNoSuchUpload, r.URL.Path, generateRequestID())
		return
	}

	// Per-upload in-flight byte cap (WP-10-minimal, H-1): part data sits
	// unbilled on local disk until complete, so without a cap one upload can
	// fill the production disk at zero quota cost. Checked twice: against the
	// DECLARED size here (reject before reading gigabytes we will discard)
	// and against the MEASURED size after the write (declared sizes are
	// client-controlled). Sum excludes this part number — re-uploading an
	// existing part replaces its bytes, it does not add.
	declared := r.ContentLength
	if isAWSChunked(r) {
		declared = -1
		if v, perr := strconv.ParseInt(r.Header.Get("x-amz-decoded-content-length"), 10, 64); perr == nil {
			declared = v
		}
	}
	var existingBytes int64
	if s.db != nil && s.multipartMaxUploadBytes > 0 {
		if err := s.db.QueryRowContext(r.Context(), `
			SELECT COALESCE(SUM(size_bytes), 0) FROM multipart_parts
			WHERE upload_id = $1 AND part_number <> $2
		`, uploadID, partNumber).Scan(&existingBytes); err != nil {
			s.logger.Error("failed to sum in-flight part bytes", zap.Error(err))
			WriteS3Error(w, ErrInternalError, r.URL.Path, generateRequestID())
			return
		}
		if declared > 0 && existingBytes+declared > s.multipartMaxUploadBytes {
			WriteS3ErrorWithContext(w, ErrEntityTooLarge, r.URL.Path, generateRequestID(),
				WithSuggestion(fmt.Sprintf(
					"This part would push the upload past the %d-byte in-flight limit. Complete or abort the upload, or use fewer/smaller parts.",
					s.multipartMaxUploadBytes)))
			return
		}
	}

	// The staging filesystem is shared by every tenant: a part that would
	// leave it below its floor is refused before a byte is staged or
	// reserved (WP-R10-8 Part C, the minimal guard of WP-R3-5).
	if s.stagingWouldOverfill(declared) {
		w.Header().Set("Retry-After", "60")
		WriteS3ErrorWithContext(w, ErrSlowDown, r.URL.Path, generateRequestID(),
			WithMessage("Please reduce your request rate: the server's upload staging space is nearly full."),
			WithSuggestion("Retry the part in a minute."))
		return
	}

	// The part's bytes are reserved on the upload's floor before the first
	// is staged (WP-R10-8 Part C, R10-24 / R3-14): a part past the tenant's
	// quota is refused here, nothing staged. A re-upload of a part number
	// reserves the difference; whatever stops the part before its install
	// gives the reservation back.
	partBilled := s.db != nil && s.quotaManager != nil
	var reserved int64
	if partBilled {
		reserved, err = reserveUploadPart(r.Context(), s.db, t.ID, uploadID, partNumber, max(declared, 0))
		switch {
		case errors.Is(err, errUploadEnded):
			WriteS3Error(w, ErrNoSuchUpload, r.URL.Path, generateRequestID())
			return
		case errors.Is(err, errPartOverQuota):
			WriteS3ErrorWithContext(w, ErrQuotaExceeded, r.URL.Path, generateRequestID(),
				WithSuggestion("Parts in progress count against your storage. Upgrade at "+billingURL()+", or abort uploads you no longer need."))
			return
		case err != nil:
			s.logger.Error("upload part: reservation failed", zap.Error(err))
			WriteS3Error(w, ErrInternalError, r.URL.Path, generateRequestID())
			return
		}
	}
	installed := false
	defer func() {
		if partBilled && !installed {
			if relErr := releaseUploadPart(context.WithoutCancel(r.Context()), s.db, t.ID, uploadID, partNumber, reserved); relErr != nil {
				s.logger.Error("upload part: reservation not given back — the reconcile and the upload's end release it",
					zap.Error(relErr), zap.String("upload_id", uploadID), zap.Int64("bytes", reserved))
			}
		}
	}()

	// Stream part data to temp file while computing MD5 ETag.
	// Decode aws-chunked framing first — aws-cli v2 over HTTPS sends parts
	// as STREAMING-UNSIGNED-PAYLOAD-TRAILER; storing the raw framed body
	// corrupts the assembled object (wire framing embedded in the data) and
	// records framed sizes/ETags for the parts.
	var body io.Reader = r.Body
	if isAWSChunked(r) {
		body = newAWSChunkedReader(r.Body)
	}
	src := newClientBody(body) // the client's own read failures (WP-R2-3)

	// The part is written to a private temp file and renamed over the part
	// path only once it is complete (R3-11). Writing the part path directly
	// (os.Create truncates) meant a client retry of the same part number —
	// aws-cli retries a timed-out part while the first attempt may still be
	// streaming — interleaved two bodies in one file, and the failing attempt
	// then REMOVED the file the retry had just written. A failed attempt now
	// leaves the previous part exactly as it was.
	dir := multipartDir(uploadID)
	if err := os.MkdirAll(dir, 0700); err != nil {
		s.logger.Error("failed to create part temp dir", zap.Error(err))
		WriteS3Error(w, ErrInternalError, r.URL.Path, generateRequestID())
		return
	}
	f, err := os.CreateTemp(dir, fmt.Sprintf(".part-%05d-*", partNumber))
	if err != nil {
		s.logger.Error("failed to create part temp file", zap.Error(err))
		WriteS3Error(w, ErrInternalError, r.URL.Path, generateRequestID())
		return
	}
	tmp := f.Name()

	hasher := md5.New() // #nosec G401 — S3 spec requires MD5 for ETags
	size, err := io.Copy(f, io.TeeReader(src, hasher))
	if closeErr := f.Close(); closeErr != nil && err == nil {
		err = closeErr
	}
	if err != nil {
		_ = os.Remove(tmp)
		if clientBodyFault(r.Context(), src, err) {
			// The client's body broke off (a reset, a dropped tunnel, bad
			// aws-chunked framing): its request, like PutObject's — not a
			// 500 with a stack trace (Prompt 2b.3 D1.8, WP-R2-3).
			answerClientBody(w, r, s.logger, "UploadPart", src, err,
				zap.String("upload_id", uploadID), zap.Int("part", partNumber), zap.Int64("received", size))
			return
		}
		s.logger.Error("failed to write part data", zap.Error(err))
		WriteS3Error(w, ErrInternalError, r.URL.Path, generateRequestID())
		return
	}

	// Measured-size cap check: the declared pre-check above can be defeated
	// by lying (or absent) length headers; what actually landed on disk is
	// authoritative. Remove the file so the rejected bytes don't leak.
	if s.db != nil && s.multipartMaxUploadBytes > 0 && existingBytes+size > s.multipartMaxUploadBytes {
		_ = os.Remove(tmp)
		WriteS3ErrorWithContext(w, ErrEntityTooLarge, r.URL.Path, generateRequestID(),
			WithSuggestion(fmt.Sprintf(
				"This part pushed the upload past the %d-byte in-flight limit. Complete or abort the upload, or use fewer/smaller parts.",
				s.multipartMaxUploadBytes)))
		return
	}

	etag := fmt.Sprintf("\"%x\"", hasher.Sum(nil))
	// The install runs inside settleUploadPart's transaction: the id is
	// checked where the path is built (the staging path's belt, as
	// everywhere a path is built from an upload id — CodeQL go/path-injection).
	install := func() error {
		if !validUploadID(uploadID) {
			return fmt.Errorf("install part file: invalid upload id")
		}
		if err := os.Rename(tmp, partFilePath(uploadID, partNumber)); err != nil {
			return fmt.Errorf("install part file: %w", err)
		}
		return nil
	}

	// Record part metadata
	if partBilled {
		// The install, the part row and the reservation settled to the
		// measured size: one transaction under the upload row's lock.
		sErr := settleUploadPart(r.Context(), s.db, t.ID, uploadID, partNumber, reserved, size, etag, install)
		switch {
		case sErr == nil:
			installed = true
		case errors.Is(sErr, errUploadEnded):
			installed = true // ended meanwhile: its end released this part's reservation
			_ = os.Remove(tmp)
			WriteS3Error(w, ErrNoSuchUpload, r.URL.Path, generateRequestID())
			return
		case errors.Is(sErr, errPartOverQuota):
			installed = true // settleUploadPart gave the reservation back
			_ = os.Remove(tmp)
			WriteS3ErrorWithContext(w, ErrQuotaExceeded, r.URL.Path, generateRequestID(),
				WithSuggestion("Parts in progress count against your storage. Upgrade at "+billingURL()+"."))
			return
		default:
			_ = os.Remove(tmp)
			s.logger.Error("failed to record part", zap.Error(sErr))
			WriteS3Error(w, ErrInternalError, r.URL.Path, generateRequestID())
			return
		}
	} else if err := install(); err != nil {
		_ = os.Remove(tmp)
		s.logger.Error("failed to install part file", zap.Error(err))
		WriteS3Error(w, ErrInternalError, r.URL.Path, generateRequestID())
		return
	} else if s.db != nil {
		_, err := s.db.ExecContext(r.Context(), `
			INSERT INTO multipart_parts (upload_id, part_number, etag, size_bytes)
			VALUES ($1, $2, $3, $4)
			ON CONFLICT (upload_id, part_number) DO UPDATE SET
				etag       = EXCLUDED.etag,
				size_bytes = EXCLUDED.size_bytes,
				created_at = NOW()
		`, uploadID, partNumber, etag, size)
		if err != nil {
			s.logger.Error("failed to record part metadata", zap.Error(err))
			WriteS3Error(w, ErrInternalError, r.URL.Path, generateRequestID())
			return
		}
	} else {
		memUploadsMu.Lock()
		if mu, ok := memUploads[uploadID]; ok {
			mu.Parts[partNumber] = memPart{ETag: etag, Size: size}
		}
		memUploadsMu.Unlock()
	}

	s.logger.Debug("uploaded part",
		zap.String("bucket", bucket),
		zap.String("key", object),
		zap.String("uploadID", uploadID),
		zap.Int("partNumber", partNumber),
		zap.Int64("size", size),
		zap.String("etag", etag))

	w.Header().Set("ETag", etag)
	w.WriteHeader(http.StatusOK)
}

// partRecord holds part metadata from either DB or in-memory store.
type partRecord struct {
	PartNumber int
	ETag       string
	Size       int64
}

// parseCompleteMultipartBody reads the CompleteMultipartUpload XML. Read to
// EOF before decoding: a streaming xml.Decoder stops at the closing element
// and never drains the body, which would silently skip the signed
// x-amz-content-sha256 verification — a truncated-but-well-formed part list
// would commit a shorter object undetected. The body is bounded (R3-12) and
// a non-empty body that is not the expected XML is the client's error, not
// a licence to assemble every uploaded part.
func parseCompleteMultipartBody(r *http.Request) (CompleteMultipartUploadRequest, string, error) {
	var req CompleteMultipartUploadRequest
	if r.Body == nil {
		return req, "", nil
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxCompleteMultipartBodyBytes+1))
	if err != nil {
		return req, bodyReadErrorCode(err), err
	}
	if len(body) > maxCompleteMultipartBodyBytes {
		return req, ErrEntityTooLarge, fmt.Errorf("complete body exceeds %d bytes", maxCompleteMultipartBodyBytes)
	}
	if len(strings.TrimSpace(string(body))) == 0 {
		return req, "", nil
	}
	if err := xml.Unmarshal(body, &req); err != nil {
		return req, ErrMalformedXML, fmt.Errorf("parse complete body: %w", err)
	}
	return req, "", nil
}

// multipartUploadAttrs loads the attributes and storage class recorded on
// the upload row at CreateMultipartUpload.
func (s *Server) multipartUploadAttrs(r *http.Request, uploadID string) (objectAttrs, string, bool, error) {
	if s.db == nil {
		memUploadsMu.RLock()
		defer memUploadsMu.RUnlock()
		if mu, ok := memUploads[uploadID]; ok {
			return mu.Attrs, mu.Class, mu.SSERequested, nil
		}
		return objectAttrs{ContentType: "application/octet-stream"}, "", false, nil
	}
	var a objectAttrs
	var metaJSON []byte
	var class string
	var sseRequested bool
	err := s.db.QueryRowContext(r.Context(), `
		SELECT content_type, metadata, storage_class, content_disposition, content_encoding,
		       content_language, cache_control, http_expires, website_redirect_location, sse_requested
		FROM multipart_uploads WHERE upload_id = $1`, uploadID).Scan(
		&a.ContentType, &metaJSON, &class, &a.ContentDisposition, &a.ContentEncoding,
		&a.ContentLanguage, &a.CacheControl, &a.Expires, &a.WebsiteRedirect, &sseRequested)
	if err != nil {
		return objectAttrs{}, "", false, fmt.Errorf("load upload attributes: %w", err)
	}
	if len(metaJSON) > 0 {
		_ = json.Unmarshal(metaJSON, &a.Metadata)
	}
	if a.ContentType == "" {
		a.ContentType = "application/octet-stream"
	}
	return a, class, sseRequested, nil
}

// handleCompleteMultipartUpload runs the complete with the long-operation
// keep-alive (s3_long_op.go): the assembled object is one backend PUT — or
// every chunk of it through the chunk pipeline, when a PUT of the same bytes
// would be chunked (WP-R3-1, placement.go) — which on a slow backend
// outlasts Cloudflare's 100 s origin timeout.
func (s *Server) handleCompleteMultipartUpload(w http.ResponseWriter, r *http.Request, bucket, object string) {
	s.runLongS3Op(w, r, longOpInfo{Op: longOpComplete, Bucket: bucket, Key: object}, func(w http.ResponseWriter, r *http.Request) {
		s.completeMultipartUpload(w, r, bucket, object)
	})
}

func (s *Server) completeMultipartUpload(w http.ResponseWriter, r *http.Request, bucket, object string) {
	t, err := tenant.FromContext(r.Context())
	if err != nil || t == nil {
		WriteS3Error(w, ErrAccessDenied, r.URL.Path, generateRequestID())
		return
	}

	uploadID := r.URL.Query().Get("uploadId")

	// Verify upload is active and belongs to this tenant
	active, err := s.multipartUploadActive(r, t.ID, bucket, object, uploadID)
	if err != nil {
		s.logger.Error("failed to query multipart upload", zap.Error(err))
		WriteS3Error(w, ErrInternalError, r.URL.Path, generateRequestID())
		return
	}
	if !active {
		// A retry of a complete that landed but whose answer never reached
		// the client (a deploy cut the keep-alive after the commit): the same
		// result again, as AWS answers a repeated complete with the same
		// parts. Anything else about a non-active upload is NoSuchUpload.
		if res, ok := s.completedUploadResult(r, t.ID, uploadID, bucket, object); ok {
			// The first complete may have been cut after its commit, before
			// its version row and the bucket's default retention were
			// written (Prompt 2a.2 G1): the retry puts back what is missing.
			s.reassertCompletedRows(r, t.ID, uploadID, bucket, object)
			w.Header().Set("Content-Type", "application/xml")
			w.Header().Set("ETag", res.ETag)
			_ = xml.NewEncoder(w).Encode(res)
			return
		}
		WriteS3Error(w, ErrNoSuchUpload, r.URL.Path, generateRequestID())
		return
	}

	// A bucket deleted since Create: NoSuchBucket, before the keep-alive
	// can commit a 200 (R85-1). The upload stays for Abort or the reaper.
	if !s.requireWriteBucket(w, r, "complete multipart", t.ID, bucket) {
		return
	}

	// Object Lock is checked before anything is written, like plain PUT: the
	// assembled object replaces the key's bytes IN PLACE on the backend, so a
	// multipart complete over a COMPLIANCE-retained key destroyed it (R3-01,
	// live-proven). Refused with the same 403 as PUT.
	if lockErr := checkObjectLock(r.Context(), s.db, t.ID, bucket, object, isObjectLockBypass(r)); lockErr != nil {
		writeObjectLockRefusal(w, r, s.log(), lockErr)
		return
	}

	completeReq, errCode, err := parseCompleteMultipartBody(r)
	if err != nil {
		s.logger.Warn("complete multipart: bad request body", zap.Error(err))
		WriteS3Error(w, errCode, r.URL.Path, generateRequestID())
		return
	}

	// Load all uploaded parts, ordered by part number
	var parts []partRecord
	if s.db != nil {
		rows, err := s.db.QueryContext(r.Context(), `
			SELECT part_number, etag, size_bytes FROM multipart_parts
			WHERE upload_id = $1
			ORDER BY part_number ASC
		`, uploadID)
		if err != nil {
			s.logger.Error("failed to query parts", zap.Error(err))
			WriteS3Error(w, ErrInternalError, r.URL.Path, generateRequestID())
			return
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var p partRecord
			if err := rows.Scan(&p.PartNumber, &p.ETag, &p.Size); err != nil {
				s.logger.Error("failed to scan part row", zap.Error(err))
				WriteS3Error(w, ErrInternalError, r.URL.Path, generateRequestID())
				return
			}
			parts = append(parts, p)
		}
		if err := rows.Err(); err != nil {
			s.logger.Error("parts iteration error", zap.Error(err))
			WriteS3Error(w, ErrInternalError, r.URL.Path, generateRequestID())
			return
		}
	} else {
		memUploadsMu.RLock()
		mu := memUploads[uploadID]
		for pn, mp := range mu.Parts {
			parts = append(parts, partRecord{PartNumber: pn, ETag: mp.ETag, Size: mp.Size})
		}
		memUploadsMu.RUnlock()
		// Sort by part number (map iteration is random)
		sortParts(parts)
	}

	if len(parts) == 0 {
		WriteS3Error(w, ErrInvalidPart, r.URL.Path, generateRequestID())
		return
	}

	// If the client sent a specific part list, validate and select those parts
	if len(completeReq.Parts) > 0 {
		// Validate ascending order
		for i := 1; i < len(completeReq.Parts); i++ {
			if completeReq.Parts[i].PartNumber <= completeReq.Parts[i-1].PartNumber {
				WriteS3Error(w, ErrInvalidPartOrder, r.URL.Path, generateRequestID())
				return
			}
		}
		// Build lookup of uploaded parts
		uploaded := make(map[int]partRecord, len(parts))
		for _, p := range parts {
			uploaded[p.PartNumber] = p
		}
		// Match requested parts against uploaded parts
		selected := make([]partRecord, 0, len(completeReq.Parts))
		for _, rp := range completeReq.Parts {
			up, ok := uploaded[rp.PartNumber]
			if !ok {
				WriteS3Error(w, ErrInvalidPart, r.URL.Path, generateRequestID())
				return
			}
			if strings.Trim(rp.ETag, "\"") != strings.Trim(up.ETag, "\"") {
				WriteS3Error(w, ErrInvalidPart, r.URL.Path, generateRequestID())
				return
			}
			selected = append(selected, up)
		}
		parts = selected
	}

	// Every selected part's bytes must be on disk with the recorded size
	// before anything is reserved or written: the staging directory lives
	// under /tmp (wiped at boot by systemd-tmpfiles on the prod box, R3-13),
	// and the assembly pipe would otherwise deliver a clean, SHORT stream
	// that a length-lax backend commits under the declared size (R7-12).
	for _, p := range parts {
		st, statErr := os.Stat(partFilePath(uploadID, p.PartNumber))
		if statErr != nil || st.Size() != p.Size {
			s.logger.Warn("complete multipart: part data missing or short on disk",
				zap.String("uploadID", uploadID), zap.Int("partNumber", p.PartNumber),
				zap.Int64("recorded", p.Size), zap.Error(statErr))
			WriteS3ErrorWithContext(w, ErrInvalidPart, r.URL.Path, generateRequestID(),
				WithSuggestion(fmt.Sprintf("The data for part %d is no longer available on the server. Upload the part again.", p.PartNumber)))
			return
		}
	}

	// Compute total size and S3-compatible multipart ETag:
	// ETag = MD5(concat(MD5_part1 + MD5_part2 + ...))-N
	var totalSize int64
	etagHasher := md5.New() // #nosec G401 — S3 spec requires MD5 for multipart ETags
	for _, p := range parts {
		totalSize += p.Size
		raw := strings.Trim(p.ETag, "\"")
		if decoded, err := hex.DecodeString(raw); err == nil {
			etagHasher.Write(decoded)
		}
	}
	finalETag := fmt.Sprintf("\"%x-%d\"", etagHasher.Sum(nil), len(parts))
	etagValue := strings.Trim(finalETag, "\"")

	attrs, requestedClass, sseRequested, err := s.multipartUploadAttrs(r, uploadID)
	if err != nil {
		s.logger.Error("complete multipart: upload attributes unavailable", zap.Error(err))
		WriteS3Error(w, ErrInternalError, r.URL.Path, generateRequestID())
		return
	}

	// The class: the header sent on CreateMultipartUpload is honoured exactly
	// like PUT's (R10-24: it used to be dropped, so a GLACIER multipart
	// landed downstairs), then the bucket's tier — aws-cli's default
	// multipart uploads must not ignore tier placement. The floor follows.
	tierClass := resolvePutStorageClass(r.Context(), s.db, s.engine, syncPlacementGate(s.flags), t.ID, bucket, requestedClass)
	floor := usage.FloorOf(tierClass)

	// Placed exactly as a single PUT of the same bytes, class and SSE header
	// (WP-R3-1, placement.go; the header kept from Create, 089): chunked,
	// whole and SSE-sealed (at any size, WP-R8-5), whole — or refused here,
	// before anything is reserved or written and before the keep-alive can
	// commit a 200: 503 when encryption is required and this process has no
	// master key (asked again: Create's answer may be stale).
	adapter := s.writeAdapter()
	placement := adapter.decidePlacement(r.Context(), placementRequest{
		TenantID: t.ID, Bucket: bucket, Class: tierClass, Size: totalSize, SSERequested: sseRequested,
	})
	if placement.Refusal != nil {
		placement.Refusal.write(w, r)
		return
	}
	if placement.Kind == placeWholeSealed {
		// The tenant key, before the keep-alive can commit a 200: the seal
		// below then takes it from the key store's cache.
		if _, kErr := s.sseService.Current(r.Context(), t.ID); kErr != nil {
			s.logger.Error("complete: SSE-S3 tenant key unavailable", zap.Error(kErr))
			WriteS3Error(w, ErrInternalError, r.URL.Path, generateRequestID())
			return
		}
	}

	// WP-1: multipart bypasses the PUT handler's reservation, so reserve the
	// assembled size here before streaming to the backend. If the object
	// overwrites an existing key, the overwritten bytes are captured
	// atomically by the head-cache upsert and released after success.
	// The parts are already reserved, on the upload row (WP-R10-8 Part C):
	// Complete reserves nothing more. Its commit turns what the upload
	// holds into the object's bill — the parts it lists become the object,
	// the parts it does not are released — and releases what it overwrote,
	// in one transaction. A Complete that fails leaves the upload active and
	// holding its parts, for the client's retry, Abort or the reaper.
	var bill *writeBill
	if s.quotaManager != nil {
		bill = &writeBill{Floor: floor, Held: uploadHeld(uploadID)}
	}

	// The address a WHOLE assembled object goes to (WP-R2-1), recorded on
	// the upload row BEFORE the backend write: a retry of a complete whose
	// head row did not commit — and a concurrent complete of the same
	// upload — writes the same address, never a second one, and the
	// collector keeps an address an active upload names. A chunked object
	// has no whole blob: none is chosen (an address an earlier, whole
	// attempt recorded stays on the row and names nothing — its orphan is
	// queued, kept while the upload is active, collected after).
	var blob string
	if placement.Kind != placeChunked {
		if blob, err = s.completeBlobAddress(r.Context(), t.ID, uploadID, object); err != nil {
			s.logger.Error("complete multipart: no blob address", zap.Error(err), zap.String("upload_id", uploadID))
			WriteS3Error(w, ErrInternalError, r.URL.Path, generateRequestID())
			return
		}
	}

	// Every refusal is behind us: from here the keep-alive may commit.
	longOpBegin(r)

	containerName := t.NamespaceContainer(bucket)
	// The staged parts, in order, as one stream, each re-hashed against the
	// ETag its UploadPart answered as it is read (multipart_staged.go).
	staged := newStagedPartsReader(uploadID, parts)
	defer func() { _ = staged.Close() }()
	// A shutdown's cut stops the body at once (the backend stores nothing);
	// the call itself may still answer for longOpAnswerGrace, so bytes that
	// landed get their head row (s3_long_op.go).
	putCtx, putCancel := backendCallCtx(r.Context(), s.longOpAnswerGrace())
	defer putCancel()
	body := &cutReader{ctx: r.Context(), r: staged}
	markCompleted := func(ctx context.Context, tx *sql.Tx) error {
		// An abort is overridden on purpose: the upload was active when this
		// request started and every part was read in full, so a reaper abort
		// that raced the assembly must not leave a fully written object
		// without its row. A COMPLETED upload is not: a concurrent Complete
		// of the same upload (a client retrying a long one) committed first,
		// and a second commit wrote a second version row naming the same
		// blob — billed twice once the key was overwritten (Stage 1a Prompt
		// 7, Part 0). This one rolls back and answers as a replay.
		res, err := tx.ExecContext(ctx, `
			UPDATE multipart_uploads SET status = 'completed'
			WHERE upload_id = $1 AND tenant_id = $2 AND status <> 'completed'`, uploadID, t.ID)
		if err != nil {
			return fmt.Errorf("mark upload completed: %w", err)
		}
		if n, err := res.RowsAffected(); err != nil || n == 0 {
			return errUploadAlreadyCompleted
		}
		return nil
	}

	var versionID, backendName string
	// Encrypted at rest (sealed or chunk-encrypted): the response says so,
	// as AWS's does.
	encryptedAtRest := placement.Kind == placeWholeSealed || (placement.Kind == placeChunked && placement.ChunkEncrypted)
	if placement.Kind == placeChunked {
		// The chunk pipeline (chunked_write.go): dedup, compression, per-chunk
		// encryption; the manifest, the head row (is_chunked, no address, the
		// -N ETag) and the upload's status in one transaction, run to the end
		// once every chunk is stored. A cut or a failure before that gives
		// back every chunk reference this Complete took (orphan chunk blobs
		// are dedup GC's after its 7-day grace); nothing is committed.
		res, cErr := adapter.storeChunkedObject(putCtx, chunkedWrite{
			TenantID: t.ID, Bucket: bucket, Key: object,
			Body: body, DeclaredSize: totalSize, Attrs: attrs,
			ETag: func() (string, error) {
				if !staged.verified() {
					return "", fmt.Errorf("%w: not every part was read and checked", errStagedPartCorrupt)
				}
				return etagValue, nil
			},
			DetachInstall: true,
			InTx:          markCompleted,
			Bill:          bill,
		})
		if errors.Is(cErr, errUploadAlreadyCompleted) {
			s.answerConcurrentComplete(w, r, t.ID, bucket, object, finalETag)
			return
		}
		if cErr != nil {
			s.writeCompleteStoreError(w, r, cErr, bucket, object, uploadID, tierClass)
			return
		}
		versionID, backendName = res.Flip.VersionID, chunkedBackendSentinel
	} else {
		var src io.Reader = body
		putLength := totalSize
		var sealer *sseWriteSealer
		if placement.Kind == placeWholeSealed {
			// Sealed as the parts stream (WP-R8-5, = WP-R3-3: this held the
			// assembled plaintext and its ciphertext, 2 × size per request).
			// The ETag stays the -N of the parts; size and ETag are the
			// plaintext's, as a sealed PUT records them.
			var sErr error
			if sealer, sErr = newSSEWriteSealer(r.Context(), s.sseService, t.ID, body, totalSize); sErr != nil {
				s.writeCompleteStoreError(w, r, sErr, bucket, object, uploadID, tierClass)
				return
			}
			src = sealer
			putLength = s.sseService.SealedLength(totalSize)
		}
		// Upload the assembled stream where the bucket's placement says: the
		// region driver for a region-pinned bucket (R3-08: complete used to go
		// straight to the engine, i.e. the primary, under a residency label),
		// else the backend the resolved class picks.
		completeOpts := []engine.PutOption{engine.WithContentLength(putLength)}
		if tierClass != "" {
			completeOpts = append(completeOpts, engine.WithStorageClass(tierClass))
		}
		if attrs.ContentType != "" {
			completeOpts = append(completeOpts, engine.WithContentType(attrs.ContentType))
		}
		var uploadErr error
		backendName, uploadErr = placeObject(putCtx, s.db, s.engine, t.ID, bucket, containerName, blob, src, completeOpts...)
		if uploadErr == nil && !staged.verified() {
			// A backend that took the declared length without reading to
			// the end: the bytes it holds were never checked. Not committed.
			uploadErr = fmt.Errorf("%w: the backend stopped reading before the last part was checked", errStagedPartCorrupt)
			retireOrphanWrite(putCtx, s.db, s.logger, t.ID, bucket, blob, backendName)
		}
		if uploadErr != nil {
			s.writeCompleteStoreError(w, r, uploadErr, bucket, object, uploadID, tierClass)
			return
		}

		// The backend holds the assembled bytes: everything from here runs to
		// the end, whatever happens to the operation (a shutdown's cut, the
		// client).
		pctx, pcancel := postCommit(r.Context())
		defer pcancel()

		// Head row + version row + upload status in ONE transaction (the
		// pointer flip, WP-R2-1). The head row is the only thing
		// HEAD/GET/DELETE and the bill read: answering 200 without it means
		// the object is invisible and unbilled forever (R3-10). The status
		// flips in the same transaction so a failed complete stays 'active'
		// and the client's retry is not NoSuchUpload; the key's previous
		// object is untouched until the commit (R3-15).
		enc := sealer.encryption()
		if s.db != nil {
			flip, dbErr := commitBlobWrite(pctx, s.db, blobWrite{TenantID: t.ID, Bucket: bucket, Key: object,
				Bill: bill,
				Version: &versionWrite{Size: totalSize, ETag: etagValue, ContentType: attrs.ContentType, Backend: backendName,
					Blob: versionBlobRef{StorageKey: blob, Enc: enc}},
				Upsert: func(tx *sql.Tx) error {
					if err := upsertWholeObjectHeadRowEnc(pctx, tx, t.ID, bucket, object, blob, totalSize, etagValue, backendName, floor, attrs, enc); err != nil {
						return err
					}
					return markCompleted(pctx, tx)
				}})
			if errors.Is(dbErr, errUploadAlreadyCompleted) {
				// The other Complete committed this same address: the head
				// row names the blob, nothing to queue.
				s.answerConcurrentComplete(w, r, t.ID, bucket, object, finalETag)
				return
			}
			if dbErr != nil {
				// The assembled blob is an orphan while no row names it. Queued
				// for the collector, which keeps it while the upload is active
				// (its retry commits this same address) and removes it once the
				// upload is aborted or reaped.
				retireOrphanWrite(pctx, s.db, s.logger, t.ID, bucket, blob, backendName)
				s.logger.Error("multipart complete: head row write failed — failing the request",
					zap.Error(dbErr), zap.String("bucket", bucket), zap.String("key", object))
				WriteS3Error(w, ErrInternalError, r.URL.Path, generateRequestID())
				return
			}
			versionID = flip.VersionID
		} else {
			memUploadsMu.Lock()
			if mu, ok := memUploads[uploadID]; ok {
				mu.Status = "completed"
				mu.ETag = finalETag
			}
			memUploadsMu.Unlock()
		}
	}

	pctx, pcancel := postCommit(r.Context())
	defer pcancel()

	// The bucket's default retention, exactly as plain PUT.
	applyObjectLockOnPut(pctx, s.db, t.ID, bucket, object, r)

	// Clean up temp files
	_ = os.RemoveAll(multipartDir(uploadID))

	s.logger.Info("multipart upload completed",
		zap.String("bucket", bucket),
		zap.String("key", object),
		zap.String("uploadID", uploadID),
		zap.String("backend", backendName),
		zap.Int64("totalSize", totalSize),
		zap.Int("parts", len(parts)),
		zap.String("etag", finalETag))

	location := fmt.Sprintf("http://%s/%s/%s", r.Host, bucket, object)
	w.Header().Set("Content-Type", "application/xml")
	w.Header().Set("ETag", finalETag)
	if versionID != "" {
		w.Header().Set("x-amz-version-id", versionID)
	}
	if encryptedAtRest {
		w.Header().Set("x-amz-server-side-encryption", "AES256")
	}
	if err := xml.NewEncoder(w).Encode(CompleteMultipartUploadResult{
		Location: location,
		Bucket:   bucket,
		Key:      object,
		ETag:     finalETag,
	}); err != nil {
		s.logger.Error("failed to encode complete response", zap.Error(err))
	}

	notifySvc := NewNotificationDispatcher(s.db, s.logger)
	notifySvc.Fire(t.ID, bucket, "s3:ObjectCreated:CompleteMultipartUpload", object, totalSize, etagValue)
}

// writeCompleteStoreError answers a Complete whose bytes could not be stored
// (whole or chunked): nothing was committed and the upload keeps holding its
// parts (WP-R10-8 Part C) for the retry, Abort or the reaper.
// errUploadAlreadyCompleted: a Complete whose commit found its upload
// completed by a concurrent Complete — rolled back, nothing written.
var errUploadAlreadyCompleted = errors.New("multipart upload completed by a concurrent request")

// answerConcurrentComplete answers a Complete that lost to a concurrent
// Complete of the same upload: the result again when the object is the one
// this request's part list makes (as a replay of a completed upload is
// answered), NoSuchUpload otherwise (the other request completed other
// parts).
func (s *Server) answerConcurrentComplete(w http.ResponseWriter, r *http.Request, tenantID, bucket, object, finalETag string) {
	var current string
	if err := s.db.QueryRowContext(r.Context(), `SELECT etag FROM object_head_cache WHERE tenant_id = $1 AND bucket = $2 AND object_key = $3`,
		tenantID, bucket, object).Scan(&current); err != nil || strings.Trim(current, "\"") != strings.Trim(finalETag, "\"") {
		WriteS3Error(w, ErrNoSuchUpload, r.URL.Path, generateRequestID())
		return
	}
	w.Header().Set("Content-Type", "application/xml")
	w.Header().Set("ETag", finalETag)
	_ = xml.NewEncoder(w).Encode(CompleteMultipartUploadResult{
		Location: fmt.Sprintf("http://%s/%s/%s", r.Host, bucket, object), Bucket: bucket, Key: object, ETag: finalETag,
	})
}

func (s *Server) writeCompleteStoreError(w http.ResponseWriter, r *http.Request, err error, bucket, object, uploadID, tierClass string) {
	switch {
	case errors.Is(err, errRegionDriverUnavailable):
		s.logger.Error("multipart complete: region-pinned bucket has no driver — refused",
			zap.String("bucket", bucket), zap.Error(err))
		w.Header().Set("Retry-After", "300")
		WriteS3ErrorWithContext(w, ErrServiceUnavailable, r.URL.Path, generateRequestID(),
			WithSuggestion("This bucket's region is not enabled on this deployment."))
	case errors.Is(err, engine.ErrAllBackendsUnavailable):
		// A 503 the client retries: Warn, no stack trace (Prompt 2b.3 D1.7).
		s.logger.Warn("multipart complete: the backend is unavailable — 503",
			zap.Error(err), zap.String("bucket", bucket), zap.String("key", object))
		w.Header().Set("Retry-After", "30")
		WriteS3Error(w, ErrServiceUnavailable, r.URL.Path, generateRequestID())
	case errors.Is(err, engine.ErrInvalidInput):
		// The client's key, decided on a backend answer: never silent
		// (bench 2026-10-12 finding 2 — a bridge's transient refusal of a
		// stripe piece reached the customer as this 400 with no line in the
		// log).
		s.logger.Warn("multipart complete: the backend refused the key — 400",
			zap.Error(err), zap.String("bucket", bucket), zap.String("key", object),
			zap.String("upload_id", uploadID), zap.String("storage_class", tierClass))
		WriteS3ErrorWithContext(w, ErrInvalidArgument, r.URL.Path, generateRequestID(),
			WithSuggestion(refusedKeySuggestion))
	case errors.Is(err, errStagedPartCorrupt):
		// The server's own staging copy changed or shrank at rest: an
		// internal error, nothing stored under the client's ETags. The
		// upload stays active; the client uploads the part again.
		s.logger.Error("multipart complete: a staged part failed its check — nothing committed",
			zap.Error(err), zap.String("bucket", bucket), zap.String("key", object), zap.String("upload_id", uploadID))
		WriteS3ErrorWithContext(w, ErrInternalError, r.URL.Path, generateRequestID(),
			WithSuggestion("A part of this upload no longer matches what was uploaded. Upload the part again, then complete."))
	default:
		s.logger.Error("multipart backend storage failed",
			zap.Error(err),
			zap.String("bucket", bucket),
			zap.String("key", object))
		WriteS3Error(w, ErrInternalError, r.URL.Path, generateRequestID())
	}
}

func (s *Server) handleAbortMultipartUpload(w http.ResponseWriter, r *http.Request, bucket, object string) {
	t, err := tenant.FromContext(r.Context())
	if err != nil || t == nil {
		WriteS3Error(w, ErrAccessDenied, r.URL.Path, generateRequestID())
		return
	}

	uploadID := r.URL.Query().Get("uploadId")
	if !validUploadID(uploadID) {
		WriteS3Error(w, ErrNoSuchUpload, r.URL.Path, generateRequestID())
		return
	}

	if s.db != nil {
		// The abort and the release of what the upload holds: one
		// transaction (WP-R10-8 Part C).
		ended, err := endUploadReleasing(r.Context(), s.db, uploadID, t.ID, bucket, object)
		if err != nil {
			s.logger.Error("failed to abort multipart upload", zap.Error(err))
			WriteS3Error(w, ErrInternalError, r.URL.Path, generateRequestID())
			return
		}
		if !ended {
			WriteS3Error(w, ErrNoSuchUpload, r.URL.Path, generateRequestID())
			return
		}
	} else {
		memUploadsMu.Lock()
		mu, ok := memUploads[uploadID]
		if !ok || mu.TenantID != t.ID || mu.Bucket != bucket || mu.Key != object || mu.Status != "active" {
			memUploadsMu.Unlock()
			WriteS3Error(w, ErrNoSuchUpload, r.URL.Path, generateRequestID())
			return
		}
		mu.Status = "aborted"
		memUploadsMu.Unlock()
	}

	_ = os.RemoveAll(multipartDir(uploadID))

	s.logger.Info("aborted multipart upload",
		zap.String("bucket", bucket),
		zap.String("object", object),
		zap.String("uploadID", uploadID))

	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleListParts(w http.ResponseWriter, r *http.Request, bucket, object string) {
	t, err := tenant.FromContext(r.Context())
	if err != nil || t == nil {
		WriteS3Error(w, ErrAccessDenied, r.URL.Path, generateRequestID())
		return
	}

	uploadID := r.URL.Query().Get("uploadId")

	// Verify upload exists and is active
	active, err := s.multipartUploadActive(r, t.ID, bucket, object, uploadID)
	if err != nil {
		s.logger.Error("failed to query multipart upload for list parts", zap.Error(err))
		WriteS3Error(w, ErrInternalError, r.URL.Path, generateRequestID())
		return
	}
	if !active {
		WriteS3Error(w, ErrNoSuchUpload, r.URL.Path, generateRequestID())
		return
	}

	// Fetch parts
	var items []ListPartItem
	if s.db != nil {
		rows, err := s.db.QueryContext(r.Context(), `
			SELECT part_number, etag, size_bytes, created_at FROM multipart_parts
			WHERE upload_id = $1
			ORDER BY part_number ASC
		`, uploadID)
		if err != nil {
			s.logger.Error("failed to list parts", zap.Error(err))
			WriteS3Error(w, ErrInternalError, r.URL.Path, generateRequestID())
			return
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var item ListPartItem
			var createdAt time.Time
			if err := rows.Scan(&item.PartNumber, &item.ETag, &item.Size, &createdAt); err != nil {
				continue
			}
			item.LastModified = createdAt.UTC().Format(time.RFC3339)
			items = append(items, item)
		}
		if err := rows.Err(); err != nil {
			s.logger.Warn("iterate rows", zap.Error(err))
		}
	} else {
		memUploadsMu.RLock()
		mu := memUploads[uploadID]
		for pn, mp := range mu.Parts {
			items = append(items, ListPartItem{
				PartNumber:   pn,
				ETag:         mp.ETag,
				Size:         mp.Size,
				LastModified: time.Now().UTC().Format(time.RFC3339),
			})
		}
		memUploadsMu.RUnlock()
		sortPartItems(items)
	}

	w.Header().Set("Content-Type", "application/xml")
	if err := xml.NewEncoder(w).Encode(ListPartsResult{
		Bucket:   bucket,
		Key:      object,
		UploadID: uploadID,
		Parts:    items,
	}); err != nil {
		s.logger.Error("failed to encode list parts response", zap.Error(err))
	}
}

func (s *Server) handleListMultipartUploads(w http.ResponseWriter, r *http.Request, bucket string) {
	t, err := tenant.FromContext(r.Context())
	if err != nil || t == nil {
		WriteS3Error(w, ErrAccessDenied, r.URL.Path, generateRequestID())
		return
	}

	var uploads []ListMultipartUploadItem
	if s.db != nil {
		rows, err := s.db.QueryContext(r.Context(), `
			SELECT upload_id, object_key, created_at FROM multipart_uploads
			WHERE tenant_id = $1 AND bucket = $2 AND status = 'active'
			ORDER BY created_at ASC
		`, t.ID, bucket)
		if err != nil {
			s.logger.Error("failed to list multipart uploads", zap.Error(err))
			WriteS3Error(w, ErrInternalError, r.URL.Path, generateRequestID())
			return
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var item ListMultipartUploadItem
			var createdAt time.Time
			if err := rows.Scan(&item.UploadID, &item.Key, &createdAt); err != nil {
				continue
			}
			item.Initiated = createdAt.UTC().Format(time.RFC3339)
			uploads = append(uploads, item)
		}
		if err := rows.Err(); err != nil {
			s.logger.Warn("iterate rows", zap.Error(err))
		}
	} else {
		memUploadsMu.RLock()
		for uid, mu := range memUploads {
			if mu.TenantID == t.ID && mu.Bucket == bucket && mu.Status == "active" {
				uploads = append(uploads, ListMultipartUploadItem{
					UploadID:  uid,
					Key:       mu.Key,
					Initiated: mu.Created.UTC().Format(time.RFC3339),
				})
			}
		}
		memUploadsMu.RUnlock()
	}

	w.Header().Set("Content-Type", "application/xml")
	if err := xml.NewEncoder(w).Encode(ListMultipartUploadsResult{
		Bucket:  bucket,
		Uploads: uploads,
	}); err != nil {
		s.logger.Error("failed to encode list uploads response", zap.Error(err))
	}
}

// sortParts sorts partRecord slices by PartNumber ascending.
func sortParts(parts []partRecord) {
	for i := 1; i < len(parts); i++ {
		for j := i; j > 0 && parts[j].PartNumber < parts[j-1].PartNumber; j-- {
			parts[j], parts[j-1] = parts[j-1], parts[j]
		}
	}
}

// sortPartItems sorts ListPartItem slices by PartNumber ascending.
func sortPartItems(items []ListPartItem) {
	for i := 1; i < len(items); i++ {
		for j := i; j > 0 && items[j].PartNumber < items[j-1].PartNumber; j-- {
			items[j], items[j-1] = items[j-1], items[j]
		}
	}
}

// XML structures for multipart upload requests and responses.

type InitiateMultipartUploadResult struct {
	XMLName  xml.Name `xml:"InitiateMultipartUploadResult"`
	Bucket   string   `xml:"Bucket"`
	Key      string   `xml:"Key"`
	UploadID string   `xml:"UploadId"`
}

type CompleteMultipartUploadRequest struct {
	XMLName xml.Name `xml:"CompleteMultipartUpload"`
	Parts   []struct {
		PartNumber int    `xml:"PartNumber"`
		ETag       string `xml:"ETag"`
	} `xml:"Part"`
}

type CompleteMultipartUploadResult struct {
	XMLName  xml.Name `xml:"CompleteMultipartUploadResult"`
	Location string   `xml:"Location"`
	Bucket   string   `xml:"Bucket"`
	Key      string   `xml:"Key"`
	ETag     string   `xml:"ETag"`
}

type ListPartsResult struct {
	XMLName  xml.Name       `xml:"ListPartsResult"`
	Bucket   string         `xml:"Bucket"`
	Key      string         `xml:"Key"`
	UploadID string         `xml:"UploadId"`
	Parts    []ListPartItem `xml:"Part"`
}

type ListPartItem struct {
	PartNumber   int    `xml:"PartNumber"`
	ETag         string `xml:"ETag"`
	Size         int64  `xml:"Size"`
	LastModified string `xml:"LastModified"`
}

type ListMultipartUploadsResult struct {
	XMLName xml.Name                  `xml:"ListMultipartUploadsResult"`
	Bucket  string                    `xml:"Bucket"`
	Uploads []ListMultipartUploadItem `xml:"Upload"`
}

type ListMultipartUploadItem struct {
	Key       string `xml:"Key"`
	UploadID  string `xml:"UploadId"`
	Initiated string `xml:"Initiated"`
}

// completedUploadResult is the CompleteMultipartUploadResult of a retry
// against an upload this tenant already completed AT THIS bucket/key, when
// the part list the
// retry names (or, with no list, every part of the upload) assembles to the
// ETag the key holds now — i.e. the object the first complete produced is
// still there. Any other case (not completed, other parts, the key
// overwritten since) is not a retry: ok is false and the caller answers
// NoSuchUpload. The parts rows outlive the completion (the reaper purges
// them after MULTIPART_TERMINAL_RETENTION_DAYS), which is the retry window.
func (s *Server) completedUploadResult(r *http.Request, tenantID, uploadID, bucket, object string) (CompleteMultipartUploadResult, bool) {
	none := CompleteMultipartUploadResult{}
	if !validUploadID(uploadID) {
		return none, false
	}
	req, _, err := parseCompleteMultipartBody(r)
	if err != nil {
		return none, false
	}
	var uploaded []partRecord
	var currentETag string
	if s.db != nil {
		var status string
		err := s.db.QueryRowContext(r.Context(), `
			SELECT status FROM multipart_uploads WHERE upload_id = $1 AND tenant_id = $2 AND bucket = $3 AND object_key = $4`,
			uploadID, tenantID, bucket, object).Scan(&status)
		if err != nil || status != "completed" {
			return none, false
		}
		rows, err := s.db.QueryContext(r.Context(), `
			SELECT part_number, etag, size_bytes FROM multipart_parts WHERE upload_id = $1 ORDER BY part_number ASC`, uploadID)
		if err != nil {
			return none, false
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var p partRecord
			if err := rows.Scan(&p.PartNumber, &p.ETag, &p.Size); err != nil {
				return none, false
			}
			uploaded = append(uploaded, p)
		}
		if err := rows.Err(); err != nil {
			return none, false
		}
		if err := s.db.QueryRowContext(r.Context(), `
			SELECT etag FROM object_head_cache WHERE tenant_id = $1 AND bucket = $2 AND object_key = $3`,
			tenantID, bucket, object).Scan(&currentETag); err != nil {
			return none, false
		}
	} else {
		memUploadsMu.RLock()
		mu, ok := memUploads[uploadID]
		ok = ok && mu.TenantID == tenantID && mu.Bucket == bucket && mu.Key == object
		if ok && mu.Status == "completed" {
			currentETag = mu.ETag
			for pn, mp := range mu.Parts {
				uploaded = append(uploaded, partRecord{PartNumber: pn, ETag: mp.ETag, Size: mp.Size})
			}
		}
		memUploadsMu.RUnlock()
		if !ok || mu.Status != "completed" {
			return none, false
		}
		sortParts(uploaded)
	}
	parts := uploaded
	if len(req.Parts) > 0 {
		byNumber := make(map[int]partRecord, len(uploaded))
		for _, p := range uploaded {
			byNumber[p.PartNumber] = p
		}
		parts = parts[:0:0]
		for _, rp := range req.Parts {
			up, ok := byNumber[rp.PartNumber]
			if !ok || strings.Trim(rp.ETag, "\"") != strings.Trim(up.ETag, "\"") {
				return none, false
			}
			parts = append(parts, up)
		}
	}
	if len(parts) == 0 {
		return none, false
	}
	h := md5.New() // #nosec G401 — S3 spec requires MD5 for multipart ETags
	for _, p := range parts {
		if decoded, err := hex.DecodeString(strings.Trim(p.ETag, "\"")); err == nil {
			h.Write(decoded)
		}
	}
	etag := fmt.Sprintf("\"%x-%d\"", h.Sum(nil), len(parts))
	if strings.Trim(etag, "\"") != strings.Trim(currentETag, "\"") {
		return none, false
	}
	return CompleteMultipartUploadResult{
		Location: fmt.Sprintf("http://%s/%s/%s", r.Host, bucket, object),
		Bucket:   bucket,
		Key:      object,
		ETag:     etag,
	}, true
}

// reassertCompletedRows puts back what a complete cut after its commit may
// have missed (Prompt 2a.2 G1): the bucket's default retention and the
// version row of the key's current bytes — but only what the object SHOULD
// have had when it was created (Prompt 2a.3 H3: the replay wrote the
// bucket's CURRENT default, so an object completed before a 3650-day
// COMPLIANCE default was enabled became undeletable; AWS applies a default
// only at creation). The bucket's configuration as of the completion is
// known only when it has not changed since the upload began
// (buckets.updated_at, stamped by every configuration write, is not after
// multipart_uploads.created_at): then the current default is the one that
// applied. Otherwise nothing is written — a lock that may be owed is logged
// and counted (vaultaire_complete_replay_lock_undetermined_total). A
// retention is written only when the key has none (an expired one is never
// re-dated, an explicit one never touched; a row holding only a legal hold —
// an empty retention_mode — has none, and keeps its hold: Prompt 2b 0.6), its
// retain-until counted from when the object landed (the head row's
// updated_at). The version row is
// checked and written under a transaction-scoped advisory lock on the key:
// concurrent retries each saw "no latest row" and each inserted one.
func (s *Server) reassertCompletedRows(r *http.Request, tenantID, uploadID, bucket, object string) {
	if s.db == nil {
		return
	}
	ctx, cancel := postCommit(r.Context())
	defer cancel()
	var unchanged, hasLock, defaultNow bool
	if err := s.db.QueryRowContext(ctx, `
		SELECT b.updated_at <= u.created_at,
		       EXISTS(SELECT 1 FROM object_locks l WHERE l.tenant_id = $1 AND l.bucket = $2 AND l.object_key = $3
		              AND l.retention_mode <> ''),
		       b.object_lock_enabled AND b.default_retention_mode IN ('GOVERNANCE', 'COMPLIANCE') AND b.default_retention_days > 0
		FROM buckets b
		JOIN multipart_uploads u ON u.upload_id = $4 AND u.tenant_id = b.tenant_id
		WHERE b.tenant_id = $1 AND b.name = $2`,
		tenantID, bucket, object, uploadID).Scan(&unchanged, &hasLock, &defaultNow); err != nil {
		s.log().Error("complete retry: bucket configuration not read; nothing re-asserted", zap.Error(err),
			zap.String("tenant_id", tenantID), zap.String("bucket", bucket), zap.String("key", object))
		return
	}
	if !unchanged {
		if !hasLock && defaultNow {
			completeReplayLockUndetermined.Inc()
			s.log().Warn("complete retry: the key has no retention and the bucket's configuration changed since the upload began — the default at completion is unknown, no lock written",
				zap.String("tenant_id", tenantID), zap.String("bucket", bucket), zap.String("key", object), zap.String("upload_id", uploadID))
		}
		return
	}
	if !hasLock && defaultNow {
		if _, err := s.db.ExecContext(ctx, `
			INSERT INTO object_locks (tenant_id, bucket, object_key, retention_mode, retain_until_date, updated_at)
			SELECT h.tenant_id, h.bucket, h.object_key, b.default_retention_mode,
			       h.updated_at + make_interval(days => b.default_retention_days), NOW()
			FROM object_head_cache h
			JOIN buckets b ON b.tenant_id = h.tenant_id AND b.name = h.bucket
			WHERE h.tenant_id = $1 AND h.bucket = $2 AND h.object_key = $3
			ON CONFLICT (tenant_id, bucket, object_key) DO UPDATE SET
			    retention_mode = EXCLUDED.retention_mode, retain_until_date = EXCLUDED.retain_until_date, updated_at = NOW()
			WHERE object_locks.retention_mode = ''`,
			tenantID, bucket, object); err != nil {
			s.log().Error("complete retry: default retention not re-asserted", zap.Error(err),
				zap.String("tenant_id", tenantID), zap.String("bucket", bucket), zap.String("key", object))
		}
	}
	if err := s.reassertVersionRow(ctx, tenantID, bucket, object); err != nil {
		s.log().Error("complete retry: version row not re-asserted", zap.Error(err),
			zap.String("tenant_id", tenantID), zap.String("bucket", bucket), zap.String("key", object))
	}
}

// completeReplayLockUndetermined counts complete retries that found the key
// without retention while the bucket's configuration had changed since the
// upload began: the default at completion is unknown, no lock is written.
var completeReplayLockUndetermined = prometheus.NewCounter(prometheus.CounterOpts{
	Name: "vaultaire_complete_replay_lock_undetermined_total",
	Help: "CompleteMultipartUpload retries that found the key without retention while the bucket's configuration changed since the upload began: the default retention at completion cannot be determined, so none was written (a Warn line names the key).",
})

// reassertVersionWrittenHook runs after the version write, before its
// commit (tests hold two retries there).
var reassertVersionWrittenHook = func() {}

// reassertVersionRow writes the version row of the key's current bytes when
// no latest row describes them, in one transaction under the key's version
// lock, every statement on the transaction's connection.
func (s *Server) reassertVersionRow(ctx context.Context, tenantID, bucket, object string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := lockVersionKey(ctx, tx, tenantID, bucket, object); err != nil {
		return err
	}
	vStatus := bucketVersioningStatus(ctx, tx, tenantID, bucket)
	if vStatus != "Enabled" && vStatus != "Suspended" {
		return nil
	}
	var size int64
	var etag, contentType, backend, floor string
	var described bool
	var blob versionBlobRef
	if err := tx.QueryRowContext(ctx, `
		SELECT h.size_bytes, h.etag, h.content_type, COALESCE(h.backend_name, ''), h.floor,
		       COALESCE(h.storage_key, ''), COALESCE(h.encryption_algorithm, ''), h.encryption_key_version,
		       EXISTS(SELECT 1 FROM object_versions v
		              WHERE v.tenant_id = h.tenant_id AND v.bucket = h.bucket AND v.object_key = h.object_key
		                AND v.is_latest AND NOT v.is_delete_marker AND v.etag = h.etag)
		FROM object_head_cache h
		WHERE h.tenant_id = $1 AND h.bucket = $2 AND h.object_key = $3`,
		tenantID, bucket, object).Scan(&size, &etag, &contentType, &backend, &floor,
		&blob.StorageKey, &blob.Enc.Algorithm, &blob.Enc.KeyVersion, &described); err != nil {
		return err
	}
	if described {
		return nil
	}
	// The version names the head row's own blob (WP-R2-1).
	if err := writeObjectVersion(ctx, tx, vStatus, tenantID, bucket, object, size, etag, contentType, backend, floor, blob); err != nil {
		return err
	}
	reassertVersionWrittenHook()
	return tx.Commit()
}

// completeBlobAddress is the address a CompleteMultipartUpload writes the
// assembled object to (WP-R2-1): the one the upload row records when an
// earlier complete chose it, else a new one recorded now — before any byte
// is written. Without a database (the in-memory uploads of tests and dev)
// the key itself: no row could name another address.
func (s *Server) completeBlobAddress(ctx context.Context, tenantID, uploadID, object string) (string, error) {
	fresh, err := blobAddressFor(s.db, object)
	if err != nil || s.db == nil {
		return fresh, err
	}
	var chosen string
	if err := s.db.QueryRowContext(ctx, `
		UPDATE multipart_uploads SET storage_key = COALESCE(storage_key, $3)
		WHERE upload_id = $1 AND tenant_id = $2
		RETURNING storage_key`, uploadID, tenantID, fresh).Scan(&chosen); err != nil {
		return "", fmt.Errorf("record the address of upload %s: %w", uploadID, err)
	}
	return chosen, nil
}
