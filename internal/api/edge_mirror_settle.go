package api

// The mirror's settle (RP78-2, Stage 1a Prompt 50 Part 0): a mirrored
// whole PUT commits at the PRIMARY's answer — the client waits for one
// backend, never for the slower leg — and a mirror still running then is
// settled after the commit by POST /api/v1/edge/mirror {admit_id, etag |
// cut}. The write's durability is unchanged: the primary held it either
// way; the copy row is `pending` from the commit (mirror_settle) until the
// settle makes it `ready`, and a settle that never arrives leaves it to the
// blob_copies job, as any owed copy.
//
// Under the admit row's lock, then the key's (the commit's order): the row
// flips pending → ready only while the admit is committed, the head row
// (or a version) still names the admit's storage_key, and the leg answered
// the body's MD5. A leg holding bytes no row names — the object deleted or
// overwritten between the commit and the settle, an admit that never
// committed — is queued in retired_blobs on the LEG (orphan_write): nothing
// else would collect a whole object on a leg for a living tenant. A cut
// mirror stored nothing: counted (cut), the copy owed. Idempotent: a
// second settle of a ready copy answers ready.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"go.uber.org/zap"

	"github.com/FairForge/vaultaire/internal/dataplane"
)

// The settle's answers.
const (
	settleReady   = "ready"
	settlePending = "pending"
	settleRetired = "retired"
)

// handleEdgeMirror is POST /api/v1/edge/mirror.
func (s *Server) handleEdgeMirror(w http.ResponseWriter, r *http.Request) {
	host, _, ok := s.edgeCaller(r)
	if !ok {
		s.edgeNotFound(w, r)
		return
	}
	r = r.WithContext(dataplane.WithHost(r.Context(), host))
	var req dataplane.MirrorSettle
	if err := json.NewDecoder(io.LimitReader(r.Body, edgeAdmitBodyMax)).Decode(&req); err != nil {
		writeEdgeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_json"})
		return
	}
	if req.AdmitID == "" {
		writeEdgeJSON(w, http.StatusBadRequest, map[string]string{"error": "admit_id_required"})
		return
	}
	if s.db == nil {
		writeEdgeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "no_database"})
		return
	}
	cctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), edgeCommitBound)
	defer cancel()
	resp, status, err := s.edgeMirrorSettle(cctx, req)
	if err != nil {
		s.logger.Error("edge mirror settle failed", zap.Error(err), zap.String("admit_id", req.AdmitID))
		writeEdgeJSON(w, status, map[string]string{"error": err.Error()})
		return
	}
	writeEdgeJSON(w, status, resp)
}

var errEdgeNotMirrored = errors.New("not a mirrored PUT")

// edgeMirrorSettle settles one mirror (the route's and the in-process
// control plane's body).
func (s *Server) edgeMirrorSettle(ctx context.Context, req dataplane.MirrorSettle) (dataplane.MirrorSettleResponse, int, error) {
	var out dataplane.MirrorSettleResponse
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return out, http.StatusInternalServerError, err
	}
	defer func() { _ = tx.Rollback() }()
	row, err := lockEdgeAdmit(ctx, tx, req.AdmitID)
	if errors.Is(err, sql.ErrNoRows) {
		return out, http.StatusNotFound, errEdgeAdmitUnknown
	}
	if err != nil {
		return out, http.StatusInternalServerError, err
	}
	// RP80-6: a settle is the admitting host's — another host's call for
	// the same admit id is an unknown admit (once nodes hold tokens of their
	// own, WP-EDGE-3, one host must not settle another's copy).
	if admitted := row.Host; (admitted == "" && admitHost(ctx) != edgeHost) || (admitted != "" && admitted != admitHost(ctx)) {
		return out, http.StatusNotFound, errEdgeAdmitUnknown
	}
	leg := row.Attrs.MirrorLeg
	if row.Op != edgeOpPut || leg == "" {
		return out, http.StatusBadRequest, errEdgeNotMirrored
	}
	if req.Cut || req.ETag == "" {
		// Nothing stored on the leg: the copy is the job's.
		if req.Cut {
			blobCopiesTotal.WithLabelValues(blobCopyCut).Inc()
		}
		out.State = settlePending
		return out, http.StatusOK, nil
	}
	if err := lockHeadKey(ctx, tx, row.TenantID, row.Bucket, row.Key); err != nil {
		return out, http.StatusInternalServerError, err
	}
	var state, etag string
	err = tx.QueryRowContext(ctx, `SELECT state, etag FROM blob_copies
		WHERE tenant_id = $1 AND storage_key = $2 AND leg = $3 AND shard_index = 0 AND kind = 'mirror'`,
		row.TenantID, row.StorageKey, leg).Scan(&state, &etag)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return out, http.StatusInternalServerError, err
	}
	hasRow := err == nil
	named, settled := false, false
	if row.State == "committed" && hasRow && state != copyStale {
		n, nErr := blobNamedBy(ctx, tx, row.TenantID, row.Bucket, row.Key, row.StorageKey)
		if nErr != nil {
			return out, http.StatusInternalServerError, nErr
		}
		named = n == blobNamed
	}
	switch {
	case named && state == copyReady:
		out.State = settleReady // a second settle, or the job's copy first
	case named:
		if st, _ := mirrorCopyState(etag, req.ETag); st != copyReady {
			// The leg answered other bytes: the job rewrites the address.
			out.State = settlePending
			break
		}
		if _, err := tx.ExecContext(ctx, `UPDATE blob_copies SET state = 'ready', last_error = '', updated_at = NOW()
			WHERE tenant_id = $1 AND storage_key = $2 AND leg = $3 AND shard_index = 0 AND state = 'pending'`,
			row.TenantID, row.StorageKey, leg); err != nil {
			return out, http.StatusInternalServerError, err
		}
		out.State, settled = settleReady, true
	default:
		// The leg holds bytes nothing names (deleted, overwritten, never
		// committed): the collector's, on the leg — its own re-check under
		// the key's lock keeps a blob a row names again.
		out.State = settleRetired
	}
	if err := tx.Commit(); err != nil {
		return out, http.StatusInternalServerError, err
	}
	if settled {
		blobCopiesTotal.WithLabelValues(blobCopySettled).Inc()
	}
	if out.State == settleRetired {
		retireOrphanWrite(ctx, s.db, s.logger, row.TenantID, row.Bucket, row.StorageKey, leg)
	}
	s.logger.Info("edge mirror settled", zap.String("tenant_id", row.TenantID), zap.String("bucket", row.Bucket),
		zap.String("key", row.Key), zap.String("leg", leg), zap.String("state", out.State), zap.String("admit_id", row.ID),
		zap.Bool("etag_match", strings.EqualFold(strings.Trim(req.ETag, `"`), etag)))
	return out, http.StatusOK, nil
}
