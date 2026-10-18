package api

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/FairForge/vaultaire/internal/crypto"
	"github.com/FairForge/vaultaire/internal/engine"
	"github.com/FairForge/vaultaire/internal/tenant"
	"go.uber.org/zap"
)

// The source of a server-side copy — CopyObject and UploadPartCopy (WP-R3-2)
// — resolved by ONE function and opened by ONE function, so the two entry
// points cannot disagree on which bytes a copy source names: the current
// object or a version (?versionId), its address (WP-R2-1), its backend (the
// row is the one authority, WP-R6-1), its kind (plain, SSE-S3 sealed,
// chunked) and the x-amz-copy-source-if-* conditions.

// copySourceRow is a copy source as its row records it.
type copySourceRow struct {
	Bucket, Key, VersionID string

	Size       int64
	Enc        string
	Chunked    bool
	Backend    string
	Floor      string
	StorageKey string
	ETag       string
	Modified   time.Time
	// VersionAttrs: the attributes of a non-current source version — what
	// its own row recorded (R61-8, 092; a version written before 092 has its
	// content type alone), never the current object's. nil = the head row's.
	VersionAttrs *objectAttrs

	// Resolved: a row was read. False only on a server with no database,
	// where nothing is known of the source but its key.
	Resolved bool
}

// blob is the address the source's bytes are at (WP-R2-1).
func (c copySourceRow) blob() string { return blobArtifact(c.Key, c.StorageKey) }

// sealedS3 reports a whole SSE-S3 source: opened as it streams (WP-R8-5).
func (c copySourceRow) sealedS3() bool { return !c.Chunked && c.Enc != "" && !crypto.IsSSEC(c.Enc) }

// resolveCopySource reads the row of a copy source — the head row, or the
// version row ?versionId names — and decides the copy-source conditions on
// it. A refusal is written to w and ok is false: a row that cannot be read
// (500), no such key (404) or version, a delete marker, a version whose
// bytes were not kept, a failed x-amz-copy-source-if-* (412). The lookup is
// the caller's tenant's: a bucket of the same name in another tenant is
// NoSuchKey.
func (s *Server) resolveCopySource(w http.ResponseWriter, r *http.Request, tenantID, bucket, key, versionID string) (copySourceRow, bool) {
	src := copySourceRow{Bucket: bucket, Key: key, VersionID: versionID}
	if s.db == nil {
		return src, true
	}
	// The source's row is the one authority for where its bytes are
	// (WP-R6-1): a row that cannot be read is a 500 and a key with no row
	// is NoSuchKey — neither is read from the primary at a guess, which
	// is what the ignored error did (Stage 1a Prompt 6 Part 0).
	rowErr := s.db.QueryRowContext(r.Context(), `
		SELECT size_bytes, COALESCE(encryption_algorithm, ''), is_chunked, COALESCE(backend_name, ''), floor, COALESCE(storage_key, ''),
		       etag, updated_at
		FROM object_head_cache
		WHERE tenant_id = $1 AND bucket = $2 AND object_key = $3`,
		tenantID, bucket, key).Scan(&src.Size, &src.Enc, &src.Chunked, &src.Backend, &src.Floor, &src.StorageKey, &src.ETag, &src.Modified)
	found := rowErr == nil
	if rowErr != nil && !errors.Is(rowErr, sql.ErrNoRows) {
		s.logger.Error("copy: source row read failed", zap.Error(rowErr), zap.String("bucket", bucket), zap.String("key", key))
		WriteS3Error(w, ErrInternalError, r.URL.Path, generateRequestID())
		return src, false
	}
	if versionID != "" {
		v, code := s.copySourceVersion(r.Context(), tenantID, bucket, key, versionID)
		switch {
		case code != "":
			WriteS3Error(w, code, r.URL.Path, generateRequestID())
			return src, false
		case !v.current:
			found = true
			// A chunked version is copied through its own manifest
			// (WP-VM-1); a whole one from its own blob.
			src.Size, src.Enc, src.Chunked, src.Backend, src.StorageKey = v.size, v.enc, v.backend == chunkedBackendSentinel, v.backend, v.storageKey
			src.ETag, src.Modified = v.etag, v.createdAt
			va := v.attrs
			src.VersionAttrs = &va
			if v.floor != "" {
				src.Floor = v.floor
			}
		}
	}
	if !found {
		WriteS3Error(w, ErrNoSuchKey, r.URL.Path, generateRequestID())
		return src, false
	}
	// x-amz-copy-source-if-* (R61-8): decided on the source the copy reads —
	// its ETag and Last-Modified — before anything is written.
	if code := evaluateCopySourceConditions(r, src.ETag, src.Modified); code != 0 {
		WriteS3Error(w, ErrPreconditionFailed, r.URL.Path, generateRequestID())
		return src, false
	}
	src.Resolved = true
	return src, true
}

// copySourceRange is the part of a source a copy reads: the whole object,
// or [Off, Off+N).
type copySourceRange struct {
	Ranged bool
	Off, N int64
}

// openCopySource opens the plaintext of a copy source — the whole object or
// a range of it — whatever its kind: a plain blob at its address on the
// backend its row records (engine.Get, or engine.GetRange cut to the
// range's length), a sealed SSE-S3 blob opened as it streams (by segment
// for a range), a chunked object through the content index (every backend
// its chunks are on asked first). The source's recorded size goes with the
// read (Prompt 2b.2 C3: a backend holding two versions of the key serves the
// one of this size). An SSE-C source is the caller's refusal, never opened
// here. Errors are written by writeCopySourceOpenError.
func (s *Server) openCopySource(ctx context.Context, tenantID string, src copySourceRow, rng copySourceRange) (io.ReadCloser, error) {
	container := (&tenant.Tenant{ID: tenantID}).NamespaceContainer(src.Bucket)
	// The read goes to the backend that holds the source (routing truth).
	if src.Backend != "" && s.engine != nil && !src.Chunked {
		noteRecordedBackend(s.engine, s.logger, "copy_source", src.Backend)
	}
	if src.Chunked {
		// The chunked source as one stream: every backend its chunks are on
		// is asked first (503 before anything is reserved or written); each
		// chunk decrypted and verified as it is read.
		cr, err := s.readAdapter().openChunkedObject(ctx, tenantID, src.Bucket, src.Key, src.StorageKey, src.Size)
		if err != nil {
			return nil, err
		}
		if rng.Ranged {
			cr.setRange(rng.Off, rng.N)
		}
		return cr, nil
	}
	// Read from the backend the source's row records, and no other (WP-R6-1;
	// a chunked row's name is a sentinel: its chunks have their own rows).
	gctx := engine.WithRecordedBackend(ctx, src.Backend)
	if src.Size > 0 {
		stored := src.Size
		if src.sealedS3() {
			stored = s.sseService.SealedLength(src.Size)
		}
		gctx = engine.WithExpectedSize(gctx, stored)
	}
	blob := src.blob()
	if src.sealedS3() {
		// Opened as it streams: the header and first segment are verified
		// before anything is reserved or written (sse_stream.go).
		req := sealedRead{Size: src.Size}
		if rng.Ranged {
			req.Ranged, req.Off, req.N = true, rng.Off, rng.N
		}
		return openSealed(ctx, sseS3Opener{svc: s.sseService, tenantID: tenantID}, sealedSource{
			whole:  func() (io.ReadCloser, error) { return s.engine.Get(gctx, container, blob) },
			ranged: func(off, n int64) (io.ReadCloser, error) { return s.engine.GetRange(gctx, container, blob, off, n) },
		}, req)
	}
	if !rng.Ranged {
		return s.engine.Get(gctx, container, blob)
	}
	// A driver with no native ranged read is served a whole read with the
	// head discarded: cut it to the range.
	rc, err := s.engine.GetRange(gctx, container, blob, rng.Off, rng.N)
	if err != nil {
		return nil, err
	}
	return &readCloser{Reader: io.LimitReader(rc, rng.N), Closer: rc}, nil
}

// writeCopySourceOpenError answers a copy source that could not be opened,
// before anything is reserved or written.
func (s *Server) writeCopySourceOpenError(w http.ResponseWriter, r *http.Request, tenantID string, src copySourceRow, err error) {
	switch {
	case errors.Is(err, errSealedUnreadable):
		s.logger.Error("copy: the encrypted source does not open", zap.Error(err),
			zap.String("bucket", src.Bucket), zap.String("key", src.Key))
		WriteS3Error(w, ErrInternalError, r.URL.Path, generateRequestID())
	case errors.Is(err, engine.ErrAllBackendsUnavailable):
		w.Header().Set("Retry-After", "30")
		WriteS3Error(w, ErrServiceUnavailable, r.URL.Path, generateRequestID())
	case errors.Is(err, engine.ErrArchived):
		// The source's bytes are on tape (this was a 500). Same rule as
		// GET (WP-R13-1): a downstairs source is recalled on the caller's
		// behalf and the copy answers a retryable 503; an attic source
		// answers what AWS answers for a GLACIER copy source.
		writeArchivedRead(w, r, s.smartPromoter, tenantID, src.Bucket, src.Key, src.blob(), src.Floor,
			"The source object is archived on tape. Restore it (POST ?restore), then retry the copy.")
	case isObjectMissingErr(err):
		WriteS3Error(w, ErrNoSuchKey, r.URL.Path, generateRequestID())
	default:
		s.logger.Error("copy: source get failed",
			zap.Error(err),
			zap.String("container", (&tenant.Tenant{ID: tenantID}).NamespaceContainer(src.Bucket)),
			zap.String("key", src.Key))
		WriteS3Error(w, ErrInternalError, r.URL.Path, generateRequestID())
	}
}

// copySourceReadFailed answers a copy whose SOURCE failed while it was read
// (D1.6: the body is a backend's, never the client's — never 400
// IncompleteBody): a sealed source that does not open is an integrity
// failure (500, as on the GET; a 503 would be retried for ever, R85-10);
// bytes that are not the recorded size, or a backend body that broke off
// while the request is alive, are 503 + Retry-After. handled is false when
// the failure was not the source's (the caller's own write).
func (s *Server) copySourceReadFailed(w http.ResponseWriter, r *http.Request, tenantID string, src copySourceRow, source *readErrReader, err error) (handled bool) {
	switch {
	case source.err != nil && (errors.Is(source.err, crypto.ErrStreamCorrupt) || errors.Is(source.err, errSealedUnreadable)):
		s.logger.Error("copy: the sealed source does not open — 500",
			zap.String("tenant_id", tenantID), zap.String("source_bucket", src.Bucket), zap.String("source_key", src.Key),
			zap.NamedError("source_error", source.err))
		WriteS3Error(w, ErrInternalError, r.URL.Path, generateRequestID())
	case errors.Is(err, errCopySourceSize):
		// The source's head row and its bytes disagree: drift, or the key
		// overwritten between the row read and the read of the bytes. A
		// retry after the row is repaired (or the overwrite settled) works.
		copySourceSizeMismatch.Inc()
		s.logger.Warn("copy: the source's bytes are not its recorded size (head-row drift or a concurrent overwrite) — 503",
			zap.String("tenant_id", tenantID), zap.String("source_bucket", src.Bucket), zap.String("source_key", src.Key),
			zap.Int64("recorded_size", src.Size), zap.Error(err))
		w.Header().Set("Retry-After", "30")
		WriteS3Error(w, ErrServiceUnavailable, r.URL.Path, generateRequestID())
	case source.err != nil && r.Context().Err() == nil:
		s.logger.Warn("copy: the source could not be read — 503",
			zap.String("tenant_id", tenantID), zap.String("source_bucket", src.Bucket), zap.String("source_key", src.Key),
			zap.NamedError("source_error", source.err))
		w.Header().Set("Retry-After", "30")
		WriteS3Error(w, ErrServiceUnavailable, r.URL.Path, generateRequestID())
	default:
		return false
	}
	return true
}
