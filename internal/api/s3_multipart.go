package api

import (
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

	"go.uber.org/zap"
)

const (
	multipartTempBase = "/tmp/vaultaire-multipart"
	maxPartNumber     = 10000
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
	Parts    map[int]memPart
	Created  time.Time
	Attrs    objectAttrs
	Class    string
}

type memPart struct {
	ETag string
	Size int64
}

// uploadIDPattern is the only shape an upload id can have: the current
// `upload-<32 hex>` (newUploadID) and the pre-R3 `upload-<unix>-<nanos>` still
// possible on rows that were in flight across the upgrade. The id names the
// staging directory under multipartTempBase, so a request-supplied id is
// checked against this BEFORE the tenant-scoped row lookup and before any
// path is built from it — the row lookup is the authority, this is the
// belt (CodeQL go/path-injection on the staging paths).
var uploadIDPattern = regexp.MustCompile(`^upload-(?:[0-9a-f]{32}|[0-9]{1,12}-[0-9]{1,12})$`)

// validUploadID reports whether id can name a staging directory.
func validUploadID(id string) bool {
	return uploadIDPattern.MatchString(id)
}

// multipartDir returns the temp directory for a specific upload's parts.
func multipartDir(uploadID string) string {
	return filepath.Join(multipartTempBase, uploadID)
}

// partFilePath returns the temp file path for a specific part.
func partFilePath(uploadID string, partNumber int) string {
	return filepath.Join(multipartTempBase, uploadID, fmt.Sprintf("part-%05d", partNumber))
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

	if crypto.HasSSECHeaders(r) {
		WriteS3ErrorWithContext(w, ErrNotImplemented, r.URL.Path, generateRequestID(),
			WithSuggestion("SSE-C is not yet supported for multipart uploads."))
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

	uploadID, err := newUploadID()
	if err != nil {
		s.logger.Error("failed to generate upload id", zap.Error(err))
		WriteS3Error(w, ErrInternalError, r.URL.Path, generateRequestID())
		return
	}

	// Persist upload record
	if s.db != nil {
		_, err := s.db.ExecContext(r.Context(), `
			INSERT INTO multipart_uploads
				(upload_id, tenant_id, bucket, object_key, status, content_type, metadata, storage_class,
				 content_disposition, content_encoding, content_language, cache_control, http_expires,
				 website_redirect_location)
			VALUES ($1, $2, $3, $4, 'active', $5, $6, $7, $8, $9, $10, $11, $12, $13)
		`, uploadID, t.ID, bucket, object, attrs.ContentType, attrs.metadataJSON(), storageClass,
			attrs.ContentDisposition, attrs.ContentEncoding, attrs.ContentLanguage,
			attrs.CacheControl, attrs.Expires, attrs.WebsiteRedirect)
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
// tenantID. Every multipart query is keyed by upload_id AND tenant_id: the id
// also names the staging directory, so an id that is not this tenant's must
// never reach the filesystem.
func (s *Server) multipartUploadActive(r *http.Request, tenantID, uploadID string) (bool, error) {
	if !validUploadID(uploadID) {
		return false, nil
	}
	if s.db != nil {
		var status string
		err := s.db.QueryRowContext(r.Context(), `
			SELECT status FROM multipart_uploads
			WHERE upload_id = $1 AND tenant_id = $2
		`, uploadID, tenantID).Scan(&status)
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
	return ok && mu.TenantID == tenantID && mu.Status == "active", nil
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
	active, err := s.multipartUploadActive(r, t.ID, uploadID)
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
		declared := r.ContentLength
		if isAWSChunked(r) {
			if v, perr := strconv.ParseInt(r.Header.Get("x-amz-decoded-content-length"), 10, 64); perr == nil {
				declared = v
			}
		}
		if declared > 0 && existingBytes+declared > s.multipartMaxUploadBytes {
			WriteS3ErrorWithContext(w, ErrEntityTooLarge, r.URL.Path, generateRequestID(),
				WithSuggestion(fmt.Sprintf(
					"This part would push the upload past the %d-byte in-flight limit. Complete or abort the upload, or use fewer/smaller parts.",
					s.multipartMaxUploadBytes)))
			return
		}
	}

	// Stream part data to temp file while computing MD5 ETag.
	// Decode aws-chunked framing first — aws-cli v2 over HTTPS sends parts
	// as STREAMING-UNSIGNED-PAYLOAD-TRAILER; storing the raw framed body
	// corrupts the assembled object (wire framing embedded in the data) and
	// records framed sizes/ETags for the parts.
	var body io.Reader = r.Body
	if isAWSChunked(r) {
		body = newAWSChunkedReader(r.Body)
	}

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
	size, err := io.Copy(f, io.TeeReader(body, hasher))
	if closeErr := f.Close(); closeErr != nil && err == nil {
		err = closeErr
	}
	if err != nil {
		_ = os.Remove(tmp)
		s.logger.Error("failed to write part data", zap.Error(err))
		WriteS3Error(w, bodyReadErrorCode(err), r.URL.Path, generateRequestID())
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

	pp := partFilePath(uploadID, partNumber)
	if err := os.Rename(tmp, pp); err != nil {
		_ = os.Remove(tmp)
		s.logger.Error("failed to install part file", zap.Error(err))
		WriteS3Error(w, ErrInternalError, r.URL.Path, generateRequestID())
		return
	}

	etag := fmt.Sprintf("\"%x\"", hasher.Sum(nil))

	// Record part metadata
	if s.db != nil {
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
func (s *Server) multipartUploadAttrs(r *http.Request, uploadID string) (objectAttrs, string, error) {
	if s.db == nil {
		memUploadsMu.RLock()
		defer memUploadsMu.RUnlock()
		if mu, ok := memUploads[uploadID]; ok {
			return mu.Attrs, mu.Class, nil
		}
		return objectAttrs{ContentType: "application/octet-stream"}, "", nil
	}
	var a objectAttrs
	var metaJSON []byte
	var class string
	err := s.db.QueryRowContext(r.Context(), `
		SELECT content_type, metadata, storage_class, content_disposition, content_encoding,
		       content_language, cache_control, http_expires, website_redirect_location
		FROM multipart_uploads WHERE upload_id = $1`, uploadID).Scan(
		&a.ContentType, &metaJSON, &class, &a.ContentDisposition, &a.ContentEncoding,
		&a.ContentLanguage, &a.CacheControl, &a.Expires, &a.WebsiteRedirect)
	if err != nil {
		return objectAttrs{}, "", fmt.Errorf("load upload attributes: %w", err)
	}
	if len(metaJSON) > 0 {
		_ = json.Unmarshal(metaJSON, &a.Metadata)
	}
	if a.ContentType == "" {
		a.ContentType = "application/octet-stream"
	}
	return a, class, nil
}

// handleCompleteMultipartUpload runs the complete with the long-operation
// keep-alive (s3_long_op.go): the assembled object is one backend PUT, which
// on a slow backend outlasts Cloudflare's 100 s origin timeout.
func (s *Server) handleCompleteMultipartUpload(w http.ResponseWriter, r *http.Request, bucket, object string) {
	s.runLongS3Op(w, r, func(w http.ResponseWriter, r *http.Request) {
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
	active, err := s.multipartUploadActive(r, t.ID, uploadID)
	if err != nil {
		s.logger.Error("failed to query multipart upload", zap.Error(err))
		WriteS3Error(w, ErrInternalError, r.URL.Path, generateRequestID())
		return
	}
	if !active {
		WriteS3Error(w, ErrNoSuchUpload, r.URL.Path, generateRequestID())
		return
	}

	// Object Lock is checked before anything is written, like plain PUT: the
	// assembled object replaces the key's bytes IN PLACE on the backend, so a
	// multipart complete over a COMPLIANCE-retained key destroyed it (R3-01,
	// live-proven). Refused with the same 403 as PUT.
	if lockErr := checkObjectLock(r.Context(), s.db, t.ID, bucket, object, isObjectLockBypass(r)); lockErr != nil {
		WriteS3ErrorWithContext(w, ErrAccessDenied, r.URL.Path, generateRequestID(),
			WithSuggestion(lockDeniedHint(r)))
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

	attrs, requestedClass, err := s.multipartUploadAttrs(r, uploadID)
	if err != nil {
		s.logger.Error("complete multipart: upload attributes unavailable", zap.Error(err))
		WriteS3Error(w, ErrInternalError, r.URL.Path, generateRequestID())
		return
	}

	// WP-1: multipart bypasses the PUT handler's reservation, so reserve the
	// assembled size here before streaming to the backend. If the object
	// overwrites an existing key, the overwritten bytes are captured
	// atomically by the head-cache upsert and released after success.
	// Multipart bypasses the tier-aware plain-PUT path, so resolve the
	// bucket's tier here too — without this, aws-cli's default multipart
	// uploads would ignore tier placement (a resilient-tier bucket would
	// silently store on the primary backend). The class header sent on
	// CreateMultipartUpload is honoured exactly like PUT's (R10-24: it used
	// to be dropped, so a GLACIER multipart landed downstairs). The floor
	// follows the class.
	tierClass := resolvePutStorageClass(r.Context(), s.db, s.engine, syncPlacementGate(s.flags), t.ID, bucket, requestedClass)
	floor := usage.FloorOf(tierClass)
	quotaOn := s.quotaManager != nil
	var reservedBytes int64
	if quotaOn {
		ok, qErr := reserveQuota(r.Context(), s.quotaManager, t.ID, floor, totalSize)
		if qErr != nil {
			s.logger.Error("multipart complete: quota check failed",
				zap.Error(qErr), zap.String("tenant_id", t.ID))
			WriteS3Error(w, ErrInternalError, r.URL.Path, generateRequestID())
			return
		}
		if !ok {
			WriteS3ErrorWithContext(w, ErrQuotaExceeded, r.URL.Path, generateRequestID(),
				WithSuggestion("Upgrade at https://stored.ge/dashboard/billing"))
			return
		}
		reservedBytes = totalSize
	}
	releaseReservation := func() {
		if quotaOn {
			ctx, cancel := quotaCtx(r)
			s.releaseQuota(ctx, t.ID, floor, reservedBytes)
			cancel()
		}
	}

	// Stream assembled parts to backend via pipe
	pr, pw := io.Pipe()
	containerName := t.NamespaceContainer(bucket)

	errCh := make(chan error, 1)

	// Writer goroutine: read temp files in order, write into pipe. Bounded by
	// the reader: once the backend write returns, pr is closed and every
	// further pw.Write fails with ErrClosedPipe, so the goroutine exits.
	go func() {
		defer func() {
			if err := pw.Close(); err != nil {
				s.logger.Debug("pipe writer close", zap.Error(err))
			}
		}()
		for _, p := range parts {
			pp := partFilePath(uploadID, p.PartNumber)
			f, err := os.Open(pp) // #nosec G304 — path derived from validated uploadID
			if err != nil {
				_ = pw.CloseWithError(fmt.Errorf("open part %d: %w", p.PartNumber, err))
				return
			}
			_, copyErr := io.Copy(pw, f)
			_ = f.Close()
			if copyErr != nil {
				_ = pw.CloseWithError(fmt.Errorf("stream part %d: %w", p.PartNumber, copyErr))
				return
			}
		}
	}()

	// Upload the assembled stream where the bucket's placement says: the
	// region driver for a region-pinned bucket (R3-08: complete used to go
	// straight to the engine, i.e. the primary, under a residency label), else
	// the backend the resolved class picks.
	completeOpts := []engine.PutOption{engine.WithContentLength(totalSize)}
	if tierClass != "" {
		completeOpts = append(completeOpts, engine.WithStorageClass(tierClass))
	}
	if attrs.ContentType != "" {
		completeOpts = append(completeOpts, engine.WithContentType(attrs.ContentType))
	}
	var backendName string
	go func() {
		var putErr error
		backendName, putErr = placeObject(r.Context(), s.db, s.engine, t.ID, bucket, containerName, object, pr, completeOpts...)
		_ = pr.Close()
		errCh <- putErr
	}()

	if uploadErr := <-errCh; uploadErr != nil {
		releaseReservation()
		if errors.Is(uploadErr, errRegionDriverUnavailable) {
			s.logger.Error("multipart complete: region-pinned bucket has no driver — refused",
				zap.String("bucket", bucket), zap.Error(uploadErr))
			w.Header().Set("Retry-After", "300")
			WriteS3ErrorWithContext(w, ErrServiceUnavailable, r.URL.Path, generateRequestID(),
				WithSuggestion("This bucket's region is not enabled on this deployment."))
			return
		}
		s.logger.Error("multipart backend storage failed",
			zap.Error(uploadErr),
			zap.String("bucket", bucket),
			zap.String("key", object))
		if errors.Is(uploadErr, engine.ErrAllBackendsUnavailable) {
			w.Header().Set("Retry-After", "30")
			WriteS3Error(w, ErrServiceUnavailable, r.URL.Path, generateRequestID())
			return
		}
		WriteS3Error(w, ErrInternalError, r.URL.Path, generateRequestID())
		return
	}

	// Head row + upload status in ONE transaction. The head row is the only
	// thing HEAD/GET/DELETE and the bill read: answering 200 without it means
	// the object is invisible and unbilled forever (R3-10 — it used to log
	// and return 200). The status flips in the same transaction so a failed
	// complete stays 'active' and the client's retry is not NoSuchUpload.
	var displaced displacedRow
	if s.db != nil {
		var dbErr error
		displaced, dbErr = atomicHeadUpsertReleasing(r.Context(), s.db, manifestReleaser(s.gci), t.ID, bucket, object, func(tx *sql.Tx) error {
			if err := upsertWholeObjectHeadRow(r.Context(), tx, t.ID, bucket, object, totalSize, etagValue, backendName, floor, attrs); err != nil {
				return err
			}
			// Unconditional on purpose: the upload was active when this
			// request started and every part was read in full, so a reaper
			// abort that raced the assembly must not turn a fully written
			// object into "new bytes under the old head row"; a concurrent
			// complete of the same upload writes identical bytes and an
			// identical row (the displaced size it releases is the size the
			// other request reserved, so the bill stays exact).
			if _, err := tx.ExecContext(r.Context(), `
				UPDATE multipart_uploads SET status = 'completed'
				WHERE upload_id = $1 AND tenant_id = $2`, uploadID, t.ID); err != nil {
				return fmt.Errorf("mark upload completed: %w", err)
			}
			return nil
		})
		if dbErr != nil {
			releaseReservation()
			s.logger.Error("multipart complete: head row write failed — failing the request",
				zap.Error(dbErr), zap.String("bucket", bucket), zap.String("key", object))
			WriteS3Error(w, ErrInternalError, r.URL.Path, generateRequestID())
			return
		}
	} else {
		memUploadsMu.Lock()
		if mu, ok := memUploads[uploadID]; ok {
			mu.Status = "completed"
		}
		memUploadsMu.Unlock()
	}

	if quotaOn && displaced.Size > 0 {
		ctx, cancel := quotaCtx(r)
		s.releaseQuota(ctx, t.ID, displaced.Floor, displaced.Size)
		cancel()
	}

	// The key's previous blob on another backend (a demoted object replaced
	// by a multipart upload, R13-10) — exactly as plain PUT.
	dropDisplacedBlob(r.Context(), s.db, s.engine, s.logger, lostWriteOverwrite,
		t.ID, bucket, t.NamespaceContainer(bucket), object, displaced, backendName)

	// Versioning ledger row + bucket default retention, exactly as plain PUT
	// (R3-09: multipart objects never appeared in object_versions).
	versionID := recordObjectVersion(r.Context(), s.db, t.ID, bucket, object, totalSize, etagValue, attrs.ContentType, backendName)
	applyObjectLockOnPut(r.Context(), s.db, t.ID, bucket, object, r)

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
		result, err := s.db.ExecContext(r.Context(), `
			UPDATE multipart_uploads SET status = 'aborted'
			WHERE upload_id = $1 AND tenant_id = $2 AND status = 'active'
		`, uploadID, t.ID)
		if err != nil {
			s.logger.Error("failed to abort multipart upload", zap.Error(err))
			WriteS3Error(w, ErrInternalError, r.URL.Path, generateRequestID())
			return
		}
		if rows, _ := result.RowsAffected(); rows == 0 {
			WriteS3Error(w, ErrNoSuchUpload, r.URL.Path, generateRequestID())
			return
		}
	} else {
		memUploadsMu.Lock()
		mu, ok := memUploads[uploadID]
		if !ok || mu.TenantID != t.ID || mu.Status != "active" {
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
	active, err := s.multipartUploadActive(r, t.ID, uploadID)
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
