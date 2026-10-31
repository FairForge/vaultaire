package api

// The multipart half of the edge codec (WP-EDGE-2 Part A): a codec multipart
// upload is one whose FIRST part was admitted codec — each part is chunked
// ALONE by the host (the manifest's chunk list is the parts' concatenation;
// cross-part dedup is by chunk content, never position), its commit records
// the part's ordered chunk list on the part row (multipart_parts.part_chunks)
// with its references taken, and Complete concatenates the lists into ONE
// manifest at the upload's recorded address. No backend multipart upload
// exists for it: the chunks are the chunk store's.
//
// One upload is one path (D-EDGE-3): the first codec part marks the upload
// (multipart_uploads.codec, under the upload row's lock, when no staged and
// no edge-whole part exists); a staged part on a codec upload is then 400
// (stageUploadPart), an edge-whole part on one is 400 (reserveEdgePart), and
// a first codec-eligible part on an upload that already holds staged parts
// passes through to the origin's stager instead (kinder than D-EDGE-3's
// literal 400, same invariant: the upload stays one path — the note says so).

import (
	"context"
	"database/sql"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/FairForge/vaultaire/internal/common"
	"github.com/FairForge/vaultaire/internal/crypto"
	"github.com/FairForge/vaultaire/internal/tenant"
	"github.com/FairForge/vaultaire/internal/usage"
)

// errUploadCodec: the upload is fed through the edge codec — a staged or an
// edge-whole part is refused (400 InvalidRequest, codecUploadRefusal).
var errUploadCodec = errors.New("multipart upload is fed through the edge codec")

// codecUploadRefusal is the sentence a mismatched part on a codec upload gets.
const codecUploadRefusal = "This upload is fed through the edge codec; send every part of it through s3.stored.cloud (the first part of this upload went that way)."

// edgeAdmitCodecPart is the codec gate of an UploadPart admit: a part of a
// codec upload, or a first part of an upload whose bucket allows chunking
// (the edge's rule, RP68-1), is the host's to chunk and seal.
func (s *Server) edgeAdmitCodecPart(req edgeAdmitRequest, p edgePrelude, size int64, uploadAttrs objectAttrs,
	storageClass, floor string) (edgeAdmitResponse, int, edgeCodecVerdict) {
	sr, sctx, rec, tenantID, bucket, key := p.sr, p.sctx, p.rec, p.tenantID, p.bucket, p.key
	if !s.edgeCodecFor(tenantID) {
		return edgeAdmitResponse{}, 0, edgeCodecNo
	}
	uploadID := sr.URL.Query().Get("uploadId")
	partNumber, pErr := strconv.Atoi(sr.URL.Query().Get("partNumber"))
	if uploadID == "" || pErr != nil || partNumber < 1 || partNumber > maxPartNumber {
		return edgeAdmitResponse{}, 0, edgeCodecNo // the generic part path answers it
	}
	up, err := readEdgeUpload(sctx, s.db, tenantID, uploadID)
	if err != nil {
		return edgeAdmitResponse{}, 0, edgeCodecNo // ended or unknown: the generic path's NoSuchUpload
	}
	eligible := up.Codec
	if !eligible && !up.Edge {
		// A first part: the EDGE's rule decides (RP68-1) — every part of a
		// codec tenant's upload is a codec candidate wherever the bucket
		// allows chunking (S3's 5 MiB part minimum is above EDGE_MIN_BYTES).
		pl := s.writeAdapter().decidePlacement(sctx, placementRequest{TenantID: tenantID, Bucket: bucket, Class: storageClass, Size: size, EdgeCodec: true})
		eligible = pl.Refusal == nil && pl.Kind == placeChunked
	}
	if !eligible {
		return edgeAdmitResponse{}, 0, edgeCodecNo
	}
	grant, ok := s.edgeCodecGrantFor(sctx, tenantID)
	if !ok || !s.edgeCodecStoreReady() {
		if up.Codec {
			// Mid-upload the keys or the chunk store's backend went away:
			// the one-path rule refuses any other shape, so the honest
			// answer is a retryable 503, never a staged part.
			rec.Header().Set("Retry-After", "30")
			WriteS3Error(rec, ErrServiceUnavailable, sr.URL.Path, generateRequestID())
			resp, st := refusalOf(rec)
			return resp, st, edgeCodecTaken
		}
		return edgeAdmitResponse{}, 0, edgeCodecNo // a first part: Part A decides
	}
	active, aErr := s.multipartUploadActive(sr, tenantID, bucket, key, uploadID)
	if aErr != nil {
		return edgeAdmitResponse{}, http.StatusInternalServerError, edgeCodecTaken
	}
	if !active {
		WriteS3Error(rec, ErrNoSuchUpload, sr.URL.Path, generateRequestID())
		resp, st := refusalOf(rec)
		return resp, st, edgeCodecTaken
	}
	if s.multipartMaxUploadBytes > 0 {
		var existing int64
		if qErr := s.db.QueryRowContext(sctx, `SELECT COALESCE(SUM(size_bytes), 0) FROM multipart_parts WHERE upload_id = $1 AND part_number <> $2`,
			uploadID, partNumber).Scan(&existing); qErr != nil {
			return edgeAdmitResponse{}, http.StatusInternalServerError, edgeCodecTaken
		}
		if existing+size > s.multipartMaxUploadBytes {
			WriteS3ErrorWithContext(rec, ErrEntityTooLarge, sr.URL.Path, generateRequestID(),
				WithSuggestion(fmt.Sprintf("This part would push the upload past the %d-byte in-flight limit. Complete or abort the upload, or use fewer/smaller parts.", s.multipartMaxUploadBytes)))
			resp, st := refusalOf(rec)
			return resp, st, edgeCodecTaken
		}
	}
	if s.quotaManager == nil {
		return edgeAdmitResponse{}, 0, edgeCodecNo // unbilled: the origin stages it (Part A's rule)
	}
	res, rErr := reserveEdgeCodecPart(sctx, s.db, tenantID, uploadID, key, partNumber, size)
	switch {
	case errors.Is(rErr, errUploadStaged):
		resp, st := passThrough("staged_parts")
		return resp, st, edgeCodecTaken
	case errors.Is(rErr, errUploadEdge):
		WriteS3ErrorWithContext(rec, ErrInvalidRequest, sr.URL.Path, generateRequestID(), WithSuggestion(edgeUploadRefusal))
		resp, st := refusalOf(rec)
		return resp, st, edgeCodecTaken
	case errors.Is(rErr, errUploadEnded):
		WriteS3Error(rec, ErrNoSuchUpload, sr.URL.Path, generateRequestID())
		resp, st := refusalOf(rec)
		return resp, st, edgeCodecTaken
	case errors.Is(rErr, errPartOverQuota):
		setStoredCap(rec, refusedCap(rErr))
		WriteS3ErrorWithContext(rec, ErrQuotaExceeded, sr.URL.Path, generateRequestID(),
			WithSuggestion("Parts in progress count against your storage. Upgrade at "+billingURL()+", or abort uploads you no longer need."))
		resp, st := refusalOf(rec)
		return resp, st, edgeCodecTaken
	case rErr != nil:
		s.logger.Error("edge codec admit: part reservation failed", zap.Error(rErr), zap.String("upload_id", uploadID))
		return edgeAdmitResponse{}, http.StatusInternalServerError, edgeCodecTaken
	}
	row := edgeAdmitRow{TenantID: tenantID, Bucket: bucket, Key: key, Op: edgeOpPart, Backend: chunkedBackendSentinel,
		UploadID: uploadID, PartNumber: partNumber, StorageKey: res.StorageKey, ContentLength: size,
		ContentType: uploadAttrs.ContentType, Codec: true, ReservedBytes: res.Reserved,
		Attrs:        edgeAdmitAttrs{UserAgent: sr.Header.Get("User-Agent"), KeyID: attemptedAccessKey(sr), CodecVersion: grant.Version, MirrorLeg: s.mirrorFor(sctx, tenantID, bucket, s.engine.GetPrimary())},
		StorageClass: storageClass, Floor: floor, ClientIP: req.ClientIP, RequestID: req.RequestID}
	settled := false
	defer func() {
		if !settled {
			if relErr := releaseUploadPart(context.WithoutCancel(sctx), s.db, tenantID, uploadID, partNumber, res.Reserved); relErr != nil {
				s.logger.Error("edge codec admit: part reservation not given back", zap.Error(relErr), zap.String("upload_id", uploadID))
			}
		}
	}()
	var idErr error
	if row.ID, idErr = newEdgeAdmitID(); idErr != nil {
		return edgeAdmitResponse{}, http.StatusInternalServerError, edgeCodecTaken
	}
	row.ExpiresAt = time.Now().UTC().Add(edgeAdmitExpiry)
	if err := insertEdgeAdmit(sctx, s.db, row); err != nil {
		s.logger.Error("edge codec admit: row not written", zap.Error(err))
		return edgeAdmitResponse{}, http.StatusInternalServerError, edgeCodecTaken
	}
	settled = true
	s.logger.Info("edge admit", zap.String("op", edgeOpPart), zap.Bool("codec", true), zap.String("tenant_id", tenantID),
		zap.String("bucket", bucket), zap.String("key", key), zap.String("upload_id", uploadID), zap.Int("part", partNumber),
		zap.Int64("size", size), zap.String("admit_id", row.ID), zap.String("request_id", req.RequestID))
	return edgeAdmitResponse{Edge: true, AdmitID: row.ID, Codec: grant, ExpiresAt: &row.ExpiresAt}, http.StatusOK, edgeCodecTaken
}

// edgeCodecPartReservation is what reserveEdgeCodecPart settled.
type edgeCodecPartReservation struct {
	Reserved   int64
	StorageKey string // the upload's manifest address (Complete installs there)
}

// reserveEdgeCodecPart reserves a codec part under the upload row's lock: an
// upload not yet codec becomes one here — no staged part and no edge-whole
// part may exist, and the manifest's address is minted once — then the
// part's reservation exactly as reserveEdgePart makes it.
func reserveEdgeCodecPart(ctx context.Context, db *sql.DB, tenantID, uploadID, object string, partNumber int, declared int64) (edgeCodecPartReservation, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return edgeCodecPartReservation{}, fmt.Errorf("begin codec part reservation: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	floor, held, edge, err := lockActiveUpload(ctx, tx, tenantID, uploadID, partNumber)
	if err != nil {
		return edgeCodecPartReservation{}, err
	}
	if edge {
		return edgeCodecPartReservation{}, errUploadEdge
	}
	up, err := readEdgeUpload(ctx, tx, tenantID, uploadID)
	if err != nil {
		return edgeCodecPartReservation{}, fmt.Errorf("read upload %s: %w", uploadID, err)
	}
	if !up.Codec {
		// A staged part installed, or one IN FLIGHT (its reservation is on
		// the upload row and no row exists yet — the stager installs after
		// the body): either makes this a staged upload. Read under the
		// upload's lock, the stager's own (one upload is one path, the lab's
		// mixed upload of Prompt 46b step 12 d).
		staged, sErr := uploadHasStagedParts(ctx, tx, uploadID)
		if sErr != nil {
			return edgeCodecPartReservation{}, sErr
		}
		if staged {
			return edgeCodecPartReservation{}, errUploadStaged
		}
		if up.StorageKey == "" {
			if up.StorageKey, err = newBlobAddress(object); err != nil {
				return edgeCodecPartReservation{}, err
			}
		}
		if _, err := tx.ExecContext(ctx, `UPDATE multipart_uploads SET codec = TRUE, storage_key = $2 WHERE upload_id = $1`,
			uploadID, up.StorageKey); err != nil {
			return edgeCodecPartReservation{}, fmt.Errorf("mark upload %s codec: %w", uploadID, err)
		}
	}
	res := edgeCodecPartReservation{StorageKey: up.StorageKey}
	if upBytes := declared - held; upBytes > 0 {
		if _, err := tx.ExecContext(ctx, `UPDATE multipart_uploads SET reserved_bytes = reserved_bytes + $2 WHERE upload_id = $1`,
			uploadID, upBytes); err != nil {
			return edgeCodecPartReservation{}, fmt.Errorf("record the part reservation: %w", err)
		}
		h, err := uploadHolder(ctx, tx, uploadID)
		if err != nil {
			return edgeCodecPartReservation{}, err
		}
		qr, err := usage.ReserveTxFor(ctx, tx, tenantID, floor, upBytes, h)
		if err != nil {
			return edgeCodecPartReservation{}, err
		}
		if !qr.OK {
			return edgeCodecPartReservation{}, &partOverQuota{cap: qr.Cap}
		}
		res.Reserved = upBytes
	}
	if err := tx.Commit(); err != nil {
		return edgeCodecPartReservation{}, fmt.Errorf("commit codec part reservation: %w", err)
	}
	return res, nil
}

// commitEdgeCodecPart settles a codec part: the chunks referenced or flipped
// ready, then the part row with its ordered chunk list (part_chunks) and the
// reservation settled to the measured size. The part's ETag is its own
// plaintext MD5 (the host's body_md5), as a staged part's would be — the
// Complete's -N follows from them.
func (s *Server) commitEdgeCodecPart(ctx context.Context, row edgeAdmitRow, req edgeCommitRequest, rec *relayRecorder) (string, int64) {
	cctx := common.WithTenantID(context.WithoutCancel(ctx), row.TenantID)
	path := "/" + row.Bucket + "/" + row.Key
	reqID := row.RequestID
	if reqID == "" {
		reqID = generateRequestID()
	}
	fail := func(code string, opts ...ErrorOption) (string, int64) {
		s.releaseEdgeAdmit(ctx, row)
		s.dropEdgeCodecClaims(cctx, row)
		WriteS3ErrorWithContext(rec, code, path, reqID, opts...)
		s.recordEdgeAccess(row, rec.status, 0, 0)
		return "failed", 0
	}
	if req.Outcome == edgeOutcomeChecksumMismatch {
		// The host decoded the aws-chunked body and its trailer is not the
		// body's (D-EDGE-31): the client's error, everything given back.
		return fail(ErrXAmzContentChecksumMismatch)
	}
	if req.Outcome != edgeOutcomeOK {
		rec.Header().Set("Retry-After", "30")
		return fail(ErrServiceUnavailable)
	}
	md5Hex := strings.ToLower(strings.Trim(strings.TrimSpace(req.BodyMD5), `"`))
	if len(md5Hex) != 32 || !isHex(md5Hex) {
		return fail(ErrInvalidDigest, WithSuggestion("A codec commit carries the part's MD5 (hex)."))
	}
	var total int64
	for _, c := range req.Chunks {
		total += c.Size
	}
	// An empty part (S3 allows an empty last part; D-EDGE-29) commits with
	// an empty chunk list; any other part names at least one chunk.
	if total != row.ContentLength || (len(req.Chunks) == 0 && row.ContentLength != 0) {
		return fail(ErrIncompleteBody)
	}
	refs, err := s.settleEdgeCodecChunks(cctx, row, req.Chunks)
	if err != nil {
		s.releaseEdgeAdmit(ctx, row)
		if edgeChunkRetryable(err) {
			rec.Header().Set("Retry-After", "5")
			WriteS3ErrorWithContext(rec, ErrServiceUnavailable, path, reqID,
				WithSuggestion("A chunk could not be settled before the commit ("+err.Error()+"); retry the part."))
		} else {
			s.logger.Warn("edge codec part commit: chunks not settled", zap.Error(err), zap.String("admit_id", row.ID))
			WriteS3ErrorWithContext(rec, ErrInvalidRequest, path, reqID, WithSuggestion(err.Error()))
		}
		s.recordEdgeAccess(row, rec.status, 0, 0)
		return "failed", 0
	}
	partChunks, mErr := json.Marshal(refs)
	if mErr != nil {
		return failWithRefs(cctx, s, row, refs, fail, ErrInternalError)
	}
	etag := `"` + md5Hex + `"`
	sErr := settleEdgeCodecPart(cctx, s.db, s.gci, s.logger, row.TenantID, row.UploadID, row.PartNumber, row.ReservedBytes, total, etag, partChunks)
	switch {
	case sErr == nil:
	case errors.Is(sErr, errUploadEnded):
		return failWithRefs(cctx, s, row, refs, fail, ErrNoSuchUpload)
	case errors.Is(sErr, errUploadStaged), errors.Is(sErr, errUploadEdge):
		st, n := failWithRefs(cctx, s, row, refs, func(code string, opts ...ErrorOption) (string, int64) {
			return fail(code, append(opts, WithSuggestion(codecUploadRefusal))...)
		}, ErrInvalidRequest)
		return st, n
	case errors.Is(sErr, errPartOverQuota):
		return failWithRefs(cctx, s, row, refs, fail, ErrQuotaExceeded)
	default:
		s.logger.Error("edge codec part commit: failed to record part", zap.Error(sErr), zap.String("upload_id", row.UploadID))
		return failWithRefs(cctx, s, row, refs, fail, ErrInternalError)
	}
	edgeCodecBytes.WithLabelValues("plaintext").Add(float64(total))
	rec.Header().Set("ETag", etag)
	rec.Header().Set("x-amz-request-id", reqID)
	rec.WriteHeader(http.StatusOK)
	s.recordEdgeAccess(row, http.StatusOK, total, 0)
	return "committed", total
}

// failWithRefs gives back the references a failed part settle left this
// commit holding, then answers.
func failWithRefs(ctx context.Context, s *Server, row edgeAdmitRow, refs []edgeCodecRef,
	fail func(string, ...ErrorOption) (string, int64), code string) (string, int64) {
	for _, cr := range refs {
		if _, decErr := s.gci.DecrementRef(ctx, cr.scope(row.TenantID), cr.Keyed); decErr != nil {
			s.logger.Warn("edge codec part: compensating decrement failed (reconcile heals)", zap.Error(decErr))
		}
	}
	return fail(code)
}

// settleEdgeCodecPart records a codec part — its row with the part's chunk
// list, no file — and settles its reservation, exactly as
// settleUploadPartWith installs a staged one (its shape, with the codec
// upload's own one-path check and the part_chunks column). A re-uploaded
// part number releases the chunk references its previous list held.
func settleEdgeCodecPart(ctx context.Context, db *sql.DB, gci *crypto.GlobalContentIndex, logger *zap.Logger,
	tenantID, uploadID string, partNumber int, reserved, measured int64, etag string, partChunks []byte) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin codec part install: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	floor, held, edge, err := lockActiveUpload(ctx, tx, tenantID, uploadID, partNumber)
	if err != nil {
		return err
	}
	if edge {
		return releaseMismatchedPart(ctx, tx, tenantID, uploadID, floor, reserved, true)
	}
	up, err := readEdgeUpload(ctx, tx, tenantID, uploadID)
	if err != nil {
		return fmt.Errorf("read upload %s: %w", uploadID, err)
	}
	if !up.Codec {
		return releaseMismatchedPart(ctx, tx, tenantID, uploadID, floor, reserved, false)
	}
	var oldChunks []byte
	if err := tx.QueryRowContext(ctx, `SELECT part_chunks FROM multipart_parts WHERE upload_id = $1 AND part_number = $2`,
		uploadID, partNumber).Scan(&oldChunks); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("read the part's previous chunks: %w", err)
	}
	delta := measured - held - reserved
	h, err := uploadHolder(ctx, tx, uploadID)
	if err != nil {
		return err
	}
	if delta > 0 {
		res, err := usage.ReserveTxFor(ctx, tx, tenantID, floor, delta, h)
		if err != nil {
			return err
		}
		if !res.OK {
			if err := giveBackPart(ctx, tx, tenantID, uploadID, floor, reserved); err != nil {
				return err
			}
			if err := tx.Commit(); err != nil {
				return fmt.Errorf("commit the part's release: %w", err)
			}
			return &partOverQuota{cap: res.Cap}
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE multipart_uploads SET reserved_bytes = GREATEST(0, reserved_bytes + $2) WHERE upload_id = $1`,
		uploadID, delta); err != nil {
		return fmt.Errorf("settle the part reservation: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO multipart_parts (upload_id, part_number, etag, size_bytes, sealed_md5, part_chunks)
		VALUES ($1, $2, $3, $4, '', $5)
		ON CONFLICT (upload_id, part_number) DO UPDATE SET
			etag        = EXCLUDED.etag,
			size_bytes  = EXCLUDED.size_bytes,
			sealed_md5  = '',
			backend_etag = NULL,
			part_chunks = EXCLUDED.part_chunks,
			created_at  = NOW()`, uploadID, partNumber, etag, measured, partChunks); err != nil {
		return fmt.Errorf("record part metadata: %w", err)
	}
	if delta < 0 {
		if err := usage.ApplyLedgerTx(ctx, tx, tenantID, usage.LedgerChange{Total: delta, Floors: map[string]int64{floor: delta},
			Holders: []usage.HolderMove{{Holder: h, Bytes: delta}}}); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	// The replaced part's references, after the new row is the row.
	releaseCodecRefList(context.WithoutCancel(ctx), gci, logger, tenantID, oldChunks)
	return nil
}

// releaseCodecRefList decrements every reference a part_chunks list holds.
func releaseCodecRefList(ctx context.Context, gci *crypto.GlobalContentIndex, logger *zap.Logger, tenantID string, partChunks []byte) {
	if len(partChunks) == 0 || gci == nil {
		return
	}
	var refs []edgeCodecRef
	if err := json.Unmarshal(partChunks, &refs); err != nil {
		logger.Warn("edge codec: a part's chunk list did not parse — its references leak until the reconcile", zap.Error(err))
		return
	}
	for _, cr := range refs {
		if _, err := gci.DecrementRef(ctx, cr.scope(tenantID), cr.Keyed); err != nil {
			logger.Warn("edge codec: a replaced part's reference not given back (reconcile heals)", zap.Error(err))
		}
	}
}

// releaseAbortedCodecParts gives back the chunk references the parts of
// ABORTED codec uploads hold (the abort handler calls it inline; the
// multipart reaper is the net for a crash between the abort and the
// release). part_chunks is NULLed with each release, so a release runs
// once; completed uploads are never touched — their references belong to
// the installed manifest.
func releaseAbortedCodecParts(ctx context.Context, db *sql.DB, gci *crypto.GlobalContentIndex, logger *zap.Logger) (int, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT p.upload_id, p.part_number, p.part_chunks, u.tenant_id
		  FROM multipart_parts p JOIN multipart_uploads u ON u.upload_id = p.upload_id
		 WHERE u.codec AND u.status = 'aborted' AND p.part_chunks IS NOT NULL
		 LIMIT 500`)
	if err != nil {
		return 0, fmt.Errorf("select aborted codec parts: %w", err)
	}
	type row struct {
		uploadID string
		part     int
		chunks   []byte
		tenant   string
	}
	var due []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.uploadID, &r.part, &r.chunks, &r.tenant); err != nil {
			_ = rows.Close()
			return 0, fmt.Errorf("scan aborted codec part: %w", err)
		}
		due = append(due, r)
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	released := 0
	for _, r := range due {
		res, uErr := db.ExecContext(ctx, `UPDATE multipart_parts SET part_chunks = NULL WHERE upload_id = $1 AND part_number = $2 AND part_chunks IS NOT NULL`,
			r.uploadID, r.part)
		if uErr != nil {
			return released, uErr
		}
		if n, _ := res.RowsAffected(); n == 0 {
			continue // another pass won
		}
		releaseCodecRefList(ctx, gci, logger, r.tenant, r.chunks)
		released++
	}
	return released, nil
}

// completeCodecUpload is CompleteMultipartUpload of a codec upload: the
// parts' chunk lists concatenated into ONE manifest at the upload's
// recorded address, the chunked head row and the version row in one
// transaction through commitBlobWrite — storeChunkedObject's rows, the
// references already taken at the part commits. The ETag is the parts' -N,
// as the staged chunked Complete's.
func (s *Server) completeCodecUpload(w http.ResponseWriter, r *http.Request, t *tenant.Tenant, bucket, object, uploadID string,
	up edgeUpload, parts []partRecord, attrs objectAttrs, floor string, bill *writeBill, finalETag string) {
	if up.StorageKey == "" {
		s.logger.Error("complete multipart: a codec upload without its manifest address", zap.String("upload_id", uploadID))
		WriteS3Error(w, ErrInternalError, r.URL.Path, generateRequestID())
		return
	}
	var refs []crypto.TenantChunkRef
	var totalSize int64
	count := 0
	for _, p := range parts {
		if len(p.PartChunks) == 0 {
			s.logger.Error("complete multipart: a codec upload's part carries no chunk list", zap.String("upload_id", uploadID), zap.Int("part", p.PartNumber))
			WriteS3Error(w, ErrInternalError, r.URL.Path, generateRequestID())
			return
		}
		var list []edgeCodecRef
		if err := json.Unmarshal(p.PartChunks, &list); err != nil {
			s.logger.Error("complete multipart: a codec part's chunk list did not parse", zap.Error(err), zap.String("upload_id", uploadID))
			WriteS3Error(w, ErrInternalError, r.URL.Path, generateRequestID())
			return
		}
		for _, cr := range list {
			refs = append(refs, crypto.TenantChunkRef{TenantID: t.ID, BucketName: bucket, ObjectKey: up.StorageKey,
				ChunkIndex: count, ChunkOffset: totalSize, PlaintextHash: cr.Keyed, DedupScope: cr.scope(t.ID),
				EncryptionKeyVersion: cr.KeyVersion, CiphertextHash: cr.CiphertextHash})
			totalSize += cr.Size
			count++
		}
	}
	etagValue := strings.Trim(finalETag, `"`)
	contentType := attrs.ContentType
	if contentType == "" {
		contentType = "application/octet-stream"
		attrs.ContentType = contentType
	}
	chunker, err := crypto.DefaultChunker()
	if err != nil {
		WriteS3Error(w, ErrInternalError, r.URL.Path, generateRequestID())
		return
	}

	// Every refusal is behind us: from here the keep-alive may commit, and
	// once it has the install runs to the end (postCommit).
	longOpBegin(r)
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
	flip, dbErr := commitBlobWrite(pctx, s.db, blobWrite{TenantID: t.ID, Bucket: bucket, Key: object, Bill: bill,
		Version: &versionWrite{Size: totalSize, ETag: etagValue, ContentType: contentType, Backend: chunkedBackendSentinel,
			Blob: versionBlobRef{StorageKey: up.StorageKey, Enc: objectEncryption{Algorithm: crypto.EdgeChunkEncryptionAlgo}}},
		Upsert: func(tx *sql.Tx) error {
			if metaErr := s.gci.ReplaceObjectManifestTx(pctx, tx, t.ID, bucket, up.StorageKey, refs, &crypto.ObjectMeta{
				TenantID: t.ID, BucketName: bucket, ObjectKey: up.StorageKey,
				TotalSize: totalSize, ChunkCount: count, ContentType: &contentType, LogicalSize: totalSize,
				PipelineConfig: chunkedPipeline(chunker, true),
			}); metaErr != nil {
				return fmt.Errorf("replace object manifest: %w", metaErr)
			}
			if err := upsertChunkedObjectHeadRow(pctx, tx, t.ID, bucket, object, up.StorageKey, totalSize, etagValue,
				crypto.EdgeChunkEncryptionAlgo, attrs); err != nil {
				return err
			}
			return markCompleted(pctx, tx)
		}})
	if errors.Is(dbErr, errUploadAlreadyCompleted) {
		s.answerConcurrentComplete(w, r, t.ID, bucket, object, `"`+etagValue+`"`)
		return
	}
	if dbErr != nil {
		// Nothing installed; the upload stays active and its parts keep
		// their references — the retry re-runs the install whole.
		if refusedAtCommit(w, r, dbErr) {
			return
		}
		s.logger.Error("codec multipart complete: install failed — failing the request",
			zap.Error(dbErr), zap.String("bucket", bucket), zap.String("key", object))
		WriteS3Error(w, ErrInternalError, r.URL.Path, generateRequestID())
		return
	}
	applyObjectLockOnPut(pctx, s.db, t.ID, bucket, object, r)
	if validUploadID(uploadID) {
		_ = os.RemoveAll(multipartDir(uploadID))
	}
	s.logger.Info("multipart upload completed", zap.String("bucket", bucket), zap.String("key", object), zap.String("uploadID", uploadID),
		zap.String("backend", chunkedBackendSentinel), zap.Bool("codec", true), zap.Int64("totalSize", totalSize),
		zap.Int("parts", len(parts)), zap.Int("chunks", count), zap.String("etag", etagValue), zap.String("host", edgeHost))
	w.Header().Set("Content-Type", "application/xml")
	w.Header().Set("ETag", `"`+etagValue+`"`)
	w.Header().Set("x-amz-server-side-encryption", "AES256")
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
