package api

// The multipart side of the edge mover (WP-EDGE-1 Part A, migration 109):
// an edge multipart upload is one whose FIRST part came through the edge.
// Its admit, under the upload row's lock, creates the backend multipart
// upload under the upload's recorded blob address (one creator; a
// concurrent first part waits on the lock and reuses it) and marks the row
// edge = true; every later edge part reserves on the same row exactly as a
// staged UploadPart does (multipart_reservation.go). A staged part on an
// edge upload — and an edge part on an upload that already holds staged
// parts — is refused: one upload is one path, because Complete either
// assembles staged files or asks the backend to assemble ITS parts, never
// both. The control plane completes and aborts the backend upload itself
// (completeEdgeUpload, abortEdgeUpload); the reaper retries an abort the
// handler could not make.

import (
	"context"
	"database/sql"
	"encoding/xml"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/FairForge/vaultaire/internal/common"
	"github.com/FairForge/vaultaire/internal/engine"
	"github.com/FairForge/vaultaire/internal/tenant"
	"github.com/FairForge/vaultaire/internal/usage"
)

// errUploadStaged: the upload already holds a staged part — the edge may
// not take this one (the origin stages it; {edge:false}).
var errUploadStaged = errors.New("multipart upload holds staged parts")

// errUploadEdge: the upload is fed through the edge — a staged part is
// refused (400 InvalidRequest, edgeUploadRefusal).
var errUploadEdge = errors.New("multipart upload is fed through the edge")

// edgeUploadRefusal is the sentence a staged part on an edge upload gets.
const edgeUploadRefusal = "This upload is fed through the edge; send every part of it through s3.stored.cloud (the first part of this upload went that way)."

// edgeUpload is what the upload row says about its edge state.
type edgeUpload struct {
	Edge bool
	// Codec: the upload is fed through the edge codec (WP-EDGE-2) — its
	// parts carry chunk lists, no backend upload exists, and Complete
	// concatenates the lists into one manifest at StorageKey.
	Codec           bool
	Backend         string
	BackendUploadID string
	StorageKey      string
	Ended           bool
}

// readEdgeUpload reads the upload's edge fields (any status).
func readEdgeUpload(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, tenantID, uploadID string) (edgeUpload, error) {
	var u edgeUpload
	var backend, buid, sk sql.NullString
	var ended sql.NullTime
	err := q.QueryRowContext(ctx, `
		SELECT edge, codec, edge_backend, backend_upload_id, storage_key, backend_upload_ended_at
		FROM multipart_uploads WHERE upload_id = $1 AND tenant_id = $2`, uploadID, tenantID).
		Scan(&u.Edge, &u.Codec, &backend, &buid, &sk, &ended)
	if err != nil {
		return edgeUpload{}, err
	}
	u.Backend, u.BackendUploadID, u.StorageKey, u.Ended = backend.String, buid.String, sk.String, ended.Valid
	return u, nil
}

// edgePartReservation is what reserveEdgePart settled: the bytes it
// reserved and the upload's edge state (the backend upload to presign on).
type edgePartReservation struct {
	Reserved int64
	Upload   edgeUpload
}

// reserveEdgePart reserves a part admitted at the edge, under the upload
// row's lock: an upload not yet edge becomes one here — no staged part may
// exist (errUploadStaged), the blob address is recorded (the same address
// Complete reuses) and the backend multipart upload is created by
// createBackend (one creator: a concurrent first part waits on the lock
// and reuses the id). Then the part's reservation as reserveUploadPart
// makes it.
func reserveEdgePart(ctx context.Context, db *sql.DB, tenantID, uploadID, object string, partNumber int, declared int64,
	createBackend func(ctx context.Context, storageKey string) (backend, backendUploadID string, err error)) (edgePartReservation, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return edgePartReservation{}, fmt.Errorf("begin edge part reservation: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	floor, held, _, err := lockActiveUpload(ctx, tx, tenantID, uploadID, partNumber)
	if err != nil {
		return edgePartReservation{}, err
	}
	up, err := readEdgeUpload(ctx, tx, tenantID, uploadID)
	if err != nil {
		return edgePartReservation{}, fmt.Errorf("read upload %s: %w", uploadID, err)
	}
	if up.Codec {
		// One upload is one path (D-EDGE-3): a codec upload takes only codec
		// parts.
		return edgePartReservation{}, errUploadCodec
	}
	if !up.Edge {
		// A staged part installed, or one IN FLIGHT (its reservation is on
		// the upload row and no row exists yet — the stager installs after
		// the body): either makes this a staged upload. Read under the
		// upload's lock, the stager's own (one upload is one path, the lab's
		// mixed upload of Prompt 46b step 12 d).
		staged, sErr := uploadHasStagedParts(ctx, tx, uploadID)
		if sErr != nil {
			return edgePartReservation{}, sErr
		}
		if staged {
			return edgePartReservation{}, errUploadStaged
		}
		if up.StorageKey == "" {
			if up.StorageKey, err = newBlobAddress(object); err != nil {
				return edgePartReservation{}, err
			}
		}
		backend, buid, cErr := createBackend(ctx, up.StorageKey)
		if cErr != nil {
			return edgePartReservation{}, cErr
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE multipart_uploads SET edge = TRUE, edge_backend = $2, backend_upload_id = $3, storage_key = $4
			WHERE upload_id = $1`, uploadID, backend, buid, up.StorageKey); err != nil {
			return edgePartReservation{}, fmt.Errorf("mark upload %s edge: %w", uploadID, err)
		}
		up.Edge, up.Backend, up.BackendUploadID = true, backend, buid
	}
	res := edgePartReservation{Upload: up}
	if upBytes := declared - held; upBytes > 0 {
		if _, err := tx.ExecContext(ctx, `UPDATE multipart_uploads SET reserved_bytes = reserved_bytes + $2 WHERE upload_id = $1`,
			uploadID, upBytes); err != nil {
			return edgePartReservation{}, fmt.Errorf("record the part reservation: %w", err)
		}
		qr, err := usage.ReserveTx(ctx, tx, tenantID, floor, upBytes)
		if err != nil {
			return edgePartReservation{}, err
		}
		if !qr.OK {
			return edgePartReservation{}, errPartOverQuota
		}
		res.Reserved = upBytes
	}
	if err := tx.Commit(); err != nil {
		return edgePartReservation{}, fmt.Errorf("commit edge part reservation: %w", err)
	}
	return res, nil
}

// settleEdgePart records an edge part the backend holds — its row with the
// backend's ETag, no file — and settles its reservation to the measured
// size, exactly as settleUploadPart installs a staged one.
func settleEdgePart(ctx context.Context, db *sql.DB, tenantID, uploadID string, partNumber int, reserved, measured int64, etag, backendETag string) error {
	return settleUploadPartWith(ctx, db, tenantID, uploadID, partNumber, reserved, measured, etag, "", backendETag, func() error { return nil }, true)
}

// edgeWriterFor is the backend's EdgeWriter, or an error naming why the
// edge cannot write to it (not registered, not an edge writer, or its
// breaker open — the origin's own write path then decides).
func (s *Server) edgeWriterFor(backend string) (engine.EdgeWriter, error) {
	if s.engine == nil {
		return nil, errors.New("no engine")
	}
	if err := s.engine.CanAsk(backend); err != nil {
		return nil, err
	}
	drv, ok := s.engine.GetDriver(backend)
	if !ok {
		return nil, fmt.Errorf("%w: %q", engine.ErrBackendNotRegistered, backend)
	}
	ew, ok := drv.(engine.EdgeWriter)
	if !ok {
		return nil, fmt.Errorf("backend %q is not an edge writer", backend)
	}
	return ew, nil
}

// edgeBackendCallTimeout bounds the control plane's own backend calls of an
// edge upload (create at the first part, abort): the meta class's 15 s
// plus the SDK's second attempt.
const edgeBackendCallTimeout = 40 * time.Second

// abortEdgeUpload aborts the backend half of an edge upload whose row has
// ended (aborted by the client, the reaper or the erasure) and stamps the
// row; a failure is logged and left for the reaper's retry. ctx is the
// caller's (detached by the reaper).
func (s *Server) abortEdgeUpload(ctx context.Context, tenantID, bucket, uploadID string, up edgeUpload) error {
	if !up.Edge || up.BackendUploadID == "" || up.Ended {
		return nil
	}
	return abortEdgeUploadOn(ctx, s.db, s.engine, s.log(), tenantID, bucket, uploadID, up)
}

// abortEdgeUploadOn is abortEdgeUpload without a Server (the reaper's).
func abortEdgeUploadOn(ctx context.Context, db *sql.DB, eng *engine.CoreEngine, logger *zap.Logger, tenantID, bucket, uploadID string, up edgeUpload) error {
	if eng == nil {
		return errors.New("abort edge upload: no engine")
	}
	drv, ok := eng.GetDriver(up.Backend)
	if !ok {
		return fmt.Errorf("abort edge upload %s: %w: %q", uploadID, engine.ErrBackendNotRegistered, up.Backend)
	}
	ew, ok := drv.(engine.EdgeWriter)
	if !ok {
		return fmt.Errorf("abort edge upload %s: backend %q is not an edge writer", uploadID, up.Backend)
	}
	if err := eng.CanAsk(up.Backend); err != nil {
		return fmt.Errorf("abort edge upload %s: %w", uploadID, err)
	}
	actx, cancel := context.WithTimeout(common.WithTenantID(ctx, tenantID), edgeBackendCallTimeout)
	defer cancel()
	if err := ew.AbortMultipart(actx, tenantID+"_"+bucket, up.StorageKey, up.BackendUploadID); err != nil {
		logger.Warn("edge upload: the backend abort failed — the multipart_reaper retries it",
			zap.String("upload_id", uploadID), zap.String("backend", up.Backend), zap.Error(err))
		return err
	}
	if _, err := db.ExecContext(ctx, `UPDATE multipart_uploads SET backend_upload_ended_at = NOW() WHERE upload_id = $1 AND backend_upload_ended_at IS NULL`, uploadID); err != nil {
		return fmt.Errorf("stamp the backend abort of %s: %w", uploadID, err)
	}
	return nil
}

// completeEdgeUpload is CompleteMultipartUpload of an edge upload: the
// backend assembles its own parts (one small call from SLC), then the
// commit of a whole object exactly as the staged path's — the head row,
// the version row, the upload's holding settled into the bill, the
// upload completed — in one transaction. The ETag is the backend's -N
// value (the MD5 of the part MD5s on both backends, Step 0.2); body_md5
// stays NULL (094's rule for a multipart assembled off-box: served
// size-checked).
func (s *Server) completeEdgeUpload(w http.ResponseWriter, r *http.Request, t *tenant.Tenant, bucket, object, uploadID string,
	up edgeUpload, parts []partRecord, attrs objectAttrs, floor string, bill *writeBill, finalETag string) {
	if up.StorageKey == "" || up.BackendUploadID == "" {
		s.logger.Error("complete multipart: an edge upload without its backend upload", zap.String("upload_id", uploadID))
		WriteS3Error(w, ErrInternalError, r.URL.Path, generateRequestID())
		return
	}
	ew, err := s.edgeWriterFor(up.Backend)
	if err != nil {
		s.logger.Warn("complete multipart: the edge upload's backend cannot be asked — 503", zap.Error(err),
			zap.String("upload_id", uploadID), zap.String("backend", up.Backend))
		w.Header().Set("Retry-After", "30")
		WriteS3Error(w, ErrServiceUnavailable, r.URL.Path, generateRequestID())
		return
	}
	var totalSize int64
	edgeParts := make([]engine.EdgePart, 0, len(parts))
	for _, p := range parts {
		totalSize += p.Size
		edgeParts = append(edgeParts, engine.EdgePart{Number: p.PartNumber, ETag: p.BackendETag})
	}
	blob := up.StorageKey
	containerName := t.NamespaceContainer(bucket)

	// Every refusal is behind us: from here the keep-alive may commit.
	longOpBegin(r)
	cctx, ccancel := backendCallCtx(r.Context(), s.longOpAnswerGrace())
	defer ccancel()
	backendETag, err := ew.CompleteMultipart(common.WithTenantID(cctx, t.ID), containerName, blob, up.BackendUploadID, edgeParts)
	if err != nil {
		s.writeCompleteStoreError(w, r, err, bucket, object, uploadID, "")
		return
	}
	etagValue := strings.Trim(backendETag, `"`)
	if want := strings.Trim(finalETag, `"`); etagValue != want {
		// Both backends answer the MD5 of the part MD5s; another value is
		// recorded as the backend gave it (it is what the object reads as).
		s.logger.Warn("complete multipart: the backend's ETag is not the parts' -N", zap.String("upload_id", uploadID),
			zap.String("backend_etag", etagValue), zap.String("computed", want))
	}
	if _, err := s.db.ExecContext(context.WithoutCancel(r.Context()), `UPDATE multipart_uploads SET backend_upload_ended_at = NOW() WHERE upload_id = $1 AND backend_upload_ended_at IS NULL`, uploadID); err != nil {
		s.logger.Warn("complete multipart: backend complete not stamped", zap.Error(err), zap.String("upload_id", uploadID))
	}

	// The backend holds the assembled bytes: everything from here runs to
	// the end, whatever happens to the operation.
	pctx, pcancel := postCommit(r.Context())
	defer pcancel()
	markCompleted := func(ctx context.Context, tx *sql.Tx) error {
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
	flip, dbErr := commitBlobWrite(pctx, s.db, blobWrite{TenantID: t.ID, Bucket: bucket, Key: object,
		Bill: bill,
		Version: &versionWrite{Size: totalSize, ETag: etagValue, ContentType: attrs.ContentType, Backend: up.Backend,
			Blob: versionBlobRef{StorageKey: blob}},
		Upsert: func(tx *sql.Tx) error {
			if err := upsertWholeObjectHeadRowEnc(pctx, tx, t.ID, bucket, object, blob, totalSize, etagValue, up.Backend, floor, attrs, objectEncryption{}, ""); err != nil {
				return err
			}
			return markCompleted(pctx, tx)
		}})
	if errors.Is(dbErr, errUploadAlreadyCompleted) {
		s.answerConcurrentComplete(w, r, t.ID, bucket, object, `"`+etagValue+`"`)
		return
	}
	if dbErr != nil {
		retireOrphanWrite(pctx, s.db, s.logger, t.ID, bucket, blob, up.Backend)
		if refusedAtCommit(w, r, dbErr) {
			return
		}
		s.logger.Error("edge multipart complete: head row write failed — failing the request",
			zap.Error(dbErr), zap.String("bucket", bucket), zap.String("key", object))
		WriteS3Error(w, ErrInternalError, r.URL.Path, generateRequestID())
		return
	}
	applyObjectLockOnPut(pctx, s.db, t.ID, bucket, object, r)
	// An edge upload stages nothing, but its Create made the directory: the
	// id is checked where the path is built (the staging path's belt,
	// CodeQL go/path-injection), as every path built from an upload id.
	if validUploadID(uploadID) {
		_ = os.RemoveAll(multipartDir(uploadID))
	}

	s.logger.Info("multipart upload completed", zap.String("bucket", bucket), zap.String("key", object), zap.String("uploadID", uploadID),
		zap.String("backend", up.Backend), zap.Int64("totalSize", totalSize), zap.Int("parts", len(parts)), zap.String("etag", etagValue),
		zap.String("host", edgeHost))
	w.Header().Set("Content-Type", "application/xml")
	w.Header().Set("ETag", `"`+etagValue+`"`)
	if flip.VersionID != "" {
		w.Header().Set("x-amz-version-id", flip.VersionID)
	}
	if err := xml.NewEncoder(w).Encode(CompleteMultipartUploadResult{
		Location: fmt.Sprintf("http://%s/%s/%s", r.Host, bucket, object), Bucket: bucket, Key: object, ETag: `"` + etagValue + `"`,
	}); err != nil {
		s.logger.Error("failed to encode complete response", zap.Error(err))
	}
	NewNotificationDispatcher(s.db, s.logger).Fire(t.ID, bucket, "s3:ObjectCreated:CompleteMultipartUpload", object, totalSize, etagValue)
}
