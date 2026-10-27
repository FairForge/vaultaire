package api

// The edge codec's control plane (WP-EDGE-2 Part A, plan 40.10 slice 2;
// contract lines 4, 5, 6): the admit's codec answer, POST /api/v1/edge/index
// and the codec commit. A PutObject (or an UploadPart) admitted at the edge
// of a tenant with the edge_codec flag whose placement is CHUNKED
// (placement.go's rule for storeChunkedObject) is chunked, hashed and sealed
// BY THE DATA-PLANE HOST — the admit answers the tenant's codec PUBLIC key
// and no presigned body PUT; the host asks the index per batch of plain
// sha256 hashes (the service-token channel; the control plane applies the
// tenant's INDEX key in memory, never stores or logs a plain hash —
// D-EDGE-16 as RP66-8 rewrote it), uploads each miss as one 0x06 blob to a
// presigned link into `_global` at the chunk's ONE address, and commits the
// ordered chunk list with the body's MD5. The commit produces EXACTLY the
// rows storeChunkedObject produces for the same body — the manifest at its
// own address, the chunked head row, the version row, the bill — in one
// transaction through commitBlobWrite (the harness compares them row by
// row).
//
// In Part A the data-plane host is the Go test harness
// (edge_codec_harness_test.go); Part B makes it the Worker 0.3.0.
//
// A chunk claim here is chunk_claim.go's claim with two differences, each a
// decision: (1) the takeover window is the admit's whole life
// (edgeAdmitExpiry, 20 min) rather than the 3-minute chunk window — a chunk
// link lives edgeAdmitTTL (15 min), and a claim must never be taken over
// while a link that can still write its address is live (one address per
// chunk identity; a late PUT over the taker's bytes would leave a
// ciphertext_hash describing other bytes); (2) a YOUNG pending claim is
// answered as a HIT with nothing referenced — the host cannot wait a
// window out, and the COMMIT is where every hit takes its reference
// (IncrementRef; 0 rows there = the chunk vanished or never landed = the
// commit's error, the host retries the part).

import (
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"go.uber.org/zap"

	"github.com/FairForge/vaultaire/internal/common"
	"github.com/FairForge/vaultaire/internal/crypto"
	"github.com/FairForge/vaultaire/internal/engine"
)

// edgeIndexMaxChunks bounds one index call (one call per part).
const edgeIndexMaxChunks = 10000

var (
	edgeCodecChunks = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "vaultaire_edge_codec_chunks_total",
		Help: "Chunks of edge-codec writes by outcome: hit (deduplicated at index time), miss (a link handed out), stored (a miss's blob committed ready), failed (a chunk the commit could not settle).",
	}, []string{"outcome"})
	edgeCodecBytes = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "vaultaire_edge_codec_bytes_total",
		Help: "Bytes of edge-codec writes: plaintext (the logical bytes committed), sealed (the 0x06 bytes stored for the misses).",
	}, []string{"kind"})
	edgeCodecIndexCalls = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "vaultaire_edge_codec_index_calls_total",
		Help: "POST /api/v1/edge/index calls answered.",
	})
)

func init() {
	for _, o := range []string{"hit", "miss", "stored", "failed"} {
		edgeCodecChunks.WithLabelValues(o)
	}
	edgeCodecBytes.WithLabelValues("plaintext")
	edgeCodecBytes.WithLabelValues("sealed")
}

// edgeCodecChunkKey is a codec chunk's one address inside the chunk
// container: keyed by the KEYED hash and the key version — never the plain
// hash (the full backend key is t-_global/_global/<this>, engine.ChunkContext).
func edgeCodecChunkKey(tenantID, keyedHex string, version int) string {
	return "_chunks/" + tenantID + "/" + keyedHex + ".k" + strconv.Itoa(version)
}

// edgeCodecVerdict is what the codec gate decided for a write admit.
type edgeCodecVerdict int

const (
	edgeCodecNo    edgeCodecVerdict = iota // not the codec's: the caller's own rules decide
	edgeCodecTaken                         // the codec admit is the answer
	edgeCodecWhole                         // edge_codec is ON but no key version exists: Part A's whole PUT (reason no_key)
)

// edgeCodecGrantFor is the tenant's codec grant, or false when this process
// holds no tenant keys (no master key — prod until the F-14 ceremony).
func (s *Server) edgeCodecGrantFor(ctx context.Context, tenantID string) (*edgeCodecGrant, bool) {
	if s.tenantKeys == nil {
		return nil, false
	}
	pub, version, err := s.tenantKeys.EdgeCodecPublicKey(ctx, tenantID)
	if err != nil {
		s.logger.Warn("edge codec: the tenant's codec key could not be derived — the whole path serves",
			zap.Error(err), zap.String("tenant_id", tenantID))
		return nil, false
	}
	return &edgeCodecGrant{Version: version, KEMPublicKey: pub,
		ChunkThreshold: s.writeAdapter().chunkThreshold(), SegmentSize: crypto.StreamSegmentSize}, true
}

// edgeAdmitCodecPut is the codec gate of a PUT admit, called once the
// placement said CHUNKED: with the flag and a key version it answers the
// codec admit; with the flag and NO key version it tells the caller to admit
// Part A's whole PUT (never a refusal, never a plaintext chunked write at
// the edge); without the flag it stands aside (the pass-through).
func (s *Server) edgeAdmitCodecPut(req edgeAdmitRequest, p edgePrelude, size int64, attrs objectAttrs,
	storageClass, floor string) (edgeAdmitResponse, int, edgeCodecVerdict) {
	sr, sctx, rec, tenantID, bucket, key := p.sr, p.sctx, p.rec, p.tenantID, p.bucket, p.key
	if s.flags == nil || !s.flags.Enabled(flagEdgeCodec, tenantID) {
		return edgeAdmitResponse{}, 0, edgeCodecNo
	}
	grant, ok := s.edgeCodecGrantFor(sctx, tenantID)
	if !ok {
		s.logger.Debug("edge admit: no_key — edge_codec is on but this process holds no tenant key version; the whole PUT",
			zap.String("tenant_id", tenantID), zap.String("request_id", req.RequestID))
		return edgeAdmitResponse{}, 0, edgeCodecWhole
	}
	// The chunk links are signed on the engine primary (the chunk store's
	// placement): it must be an EdgeWriter whose breaker is closed, or the
	// origin takes the write (the pass-through, as Part A's backend rule).
	primary := s.engine.GetPrimary()
	if _, err := s.edgeWriterFor(primary); err != nil {
		return edgeAdmitResponse{}, 0, edgeCodecNo
	}

	// Object Lock and If-Match, exactly where the whole path checks them.
	if lockErr := checkObjectLock(sctx, s.db, tenantID, bucket, key, isObjectLockBypass(sr)); lockErr != nil {
		writeObjectLockRefusal(rec, sr, s.logger, lockErr)
		resp, st := refusalOf(rec)
		return resp, st, edgeCodecTaken
	}
	if sr.Header.Get("If-Match") != "" {
		var existingETag string
		if qErr := s.db.QueryRowContext(sctx, `SELECT etag FROM object_head_cache WHERE tenant_id = $1 AND bucket = $2 AND object_key = $3`,
			tenantID, bucket, key).Scan(&existingETag); qErr == nil && checkIfMatch(sr, existingETag) {
			rec.WriteHeader(http.StatusPreconditionFailed)
			resp, st := refusalOf(rec)
			return resp, st, edgeCodecTaken
		}
	}

	row := edgeAdmitRow{TenantID: tenantID, Bucket: bucket, Key: key, Op: edgeOpPut, Backend: chunkedBackendSentinel,
		ContentLength: size, ContentType: attrs.ContentType, Codec: true,
		Attrs: edgeAdmitAttrs{Attrs: attrs, LockMode: sr.Header.Get("x-amz-object-lock-mode"),
			LockUntil: sr.Header.Get("x-amz-object-lock-retain-until-date"), UserAgent: sr.Header.Get("User-Agent"),
			CodecVersion: grant.Version},
		StorageClass: storageClass, Floor: floor, ClientIP: req.ClientIP, RequestID: req.RequestID}
	settled := false
	if s.quotaManager != nil {
		bill, okQ, qErr := s.reserveWrite(sctx, tenantID, bucket, key, floor, size)
		if qErr != nil {
			s.logger.Error("edge codec admit: quota check failed", zap.Error(qErr), zap.String("tenant_id", tenantID))
			return edgeAdmitResponse{}, http.StatusInternalServerError, edgeCodecTaken
		}
		if !okQ {
			WriteS3ErrorWithContext(rec, ErrQuotaExceeded, sr.URL.Path, generateRequestID(), WithSuggestion("Upgrade at "+billingURL()))
			resp, st := refusalOf(rec)
			return resp, st, edgeCodecTaken
		}
		row.ReservedBytes, row.ReservedFloor, row.FloorReserved, row.Credited = bill.Reserved, bill.Floor, bill.FloorReserved, bill.Credited
		defer func() {
			if !settled {
				s.releaseQuota(context.WithoutCancel(sctx), tenantID, row.ReservedFloor, row.ReservedBytes)
			}
		}()
	}
	var err error
	// The manifest's address, minted now: the commit installs the manifest
	// there (WP-VM-1's rule — never the bare key).
	if row.StorageKey, err = newBlobAddress(key); err != nil {
		return edgeAdmitResponse{}, http.StatusInternalServerError, edgeCodecTaken
	}
	if row.ID, err = newEdgeAdmitID(); err != nil {
		return edgeAdmitResponse{}, http.StatusInternalServerError, edgeCodecTaken
	}
	row.ExpiresAt = time.Now().UTC().Add(edgeAdmitExpiry)
	if err := insertEdgeAdmit(sctx, s.db, row); err != nil {
		s.logger.Error("edge codec admit: row not written", zap.Error(err))
		return edgeAdmitResponse{}, http.StatusInternalServerError, edgeCodecTaken
	}
	settled = true
	s.logger.Info("edge admit", zap.String("op", edgeOpPut), zap.Bool("codec", true), zap.String("tenant_id", tenantID),
		zap.String("bucket", bucket), zap.String("key", key), zap.Int64("size", size), zap.Int("key_version", grant.Version),
		zap.String("admit_id", row.ID), zap.String("request_id", req.RequestID))
	return edgeAdmitResponse{Edge: true, AdmitID: row.ID, Codec: grant, ExpiresAt: &row.ExpiresAt}, http.StatusOK, edgeCodecTaken
}

// --- the index ---

// edgeIndexRequest is one batch of the host's chunk hashes.
type edgeIndexRequest struct {
	AdmitID string `json:"admit_id"`
	Chunks  []struct {
		Hash string `json:"hash"` // plain sha256, hex — for the duration of this call
		Size int64  `json:"size"`
	} `json:"chunks"`
}

// edgeIndexMiss is a chunk the host must store: one presigned PUT at the
// chunk's one address, signing the SEALED length.
type edgeIndexMiss struct {
	Hash         string            `json:"hash"`
	URL          string            `json:"url"`
	SendHeaders  map[string]string `json:"send_headers"`
	SealedLength int64             `json:"sealed_length"`
}

type edgeIndexResponse struct {
	Hits   []string        `json:"hits"`
	Misses []edgeIndexMiss `json:"misses"`
}

// handleEdgeIndex is POST /api/v1/edge/index (the service-token gate, as
// admit and commit).
func (s *Server) handleEdgeIndex(w http.ResponseWriter, r *http.Request) {
	if !s.edgeCallAuthorized(r) {
		s.edgeNotFound(w, r)
		return
	}
	var req edgeIndexRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 4<<20)).Decode(&req); err != nil {
		writeEdgeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_json"})
		return
	}
	if s.db == nil || s.gci == nil || s.tenantKeys == nil {
		writeEdgeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "no_index"})
		return
	}
	resp, status, err := s.edgeIndex(r.Context(), req)
	if err != nil {
		writeEdgeJSON(w, status, map[string]string{"error": err.Error()})
		return
	}
	edgeCodecIndexCalls.Inc()
	writeEdgeJSON(w, status, resp)
}

// edgeIndex answers hit or miss per chunk hash under the admit's index key.
func (s *Server) edgeIndex(ctx context.Context, req edgeIndexRequest) (*edgeIndexResponse, int, error) {
	if req.AdmitID == "" {
		return nil, http.StatusBadRequest, errors.New("admit_id_required")
	}
	if len(req.Chunks) == 0 || len(req.Chunks) > edgeIndexMaxChunks {
		return nil, http.StatusBadRequest, fmt.Errorf("chunks: 1..%d per call", edgeIndexMaxChunks)
	}
	row, err := readPendingCodecAdmit(ctx, s.db, req.AdmitID)
	if err != nil {
		return nil, edgeAdmitErrStatus(err), err
	}
	version := row.Attrs.CodecVersion
	indexKey, _, err := s.tenantKeys.IndexKey(ctx, row.TenantID, version)
	if err != nil {
		return nil, http.StatusInternalServerError, fmt.Errorf("index key: %w", err)
	}
	defer clear(indexKey)
	ew, err := s.edgeWriterFor(s.engine.GetPrimary())
	if err != nil {
		return nil, http.StatusServiceUnavailable, fmt.Errorf("the chunk store's backend cannot be asked: %w", err)
	}

	resp := &edgeIndexResponse{Hits: []string{}, Misses: []edgeIndexMiss{}}
	claims := map[string]time.Time{}
	for _, c := range req.Chunks {
		if c.Size < 1 || c.Size > crypto.ChunkMaxSize {
			return nil, http.StatusBadRequest, fmt.Errorf("a chunk is 1..%d bytes", crypto.ChunkMaxSize)
		}
		plain, decErr := hex.DecodeString(c.Hash)
		if decErr != nil || len(plain) != 32 {
			return nil, http.StatusBadRequest, errors.New("a chunk hash is sha256 hex")
		}
		keyed, kErr := crypto.KeyedChunkHash(indexKey, plain)
		if kErr != nil {
			return nil, http.StatusInternalServerError, kErr
		}
		hit, since, cErr := s.claimEdgeChunk(ctx, row.TenantID, keyed, version, c.Size)
		if cErr != nil {
			return nil, http.StatusInternalServerError, fmt.Errorf("claim: %w", cErr)
		}
		if hit {
			resp.Hits = append(resp.Hits, c.Hash)
			edgeCodecChunks.WithLabelValues("hit").Inc()
			continue
		}
		claims[keyed] = since
		sealedLen := crypto.WrappedChunkSealedLength(c.Size)
		blobKey := edgeCodecChunkKey(row.TenantID, keyed, version)
		presigned, signed, pErr := ew.PresignPut(engine.ChunkContext(ctx), engine.ChunkContainer, blobKey,
			engine.EdgePresign{ContentLength: sealedLen, TTL: edgeAdmitTTL})
		if pErr != nil {
			s.dropEdgeChunkClaim(ctx, row.TenantID, keyed, since)
			return nil, http.StatusInternalServerError, fmt.Errorf("presign chunk link: %w", pErr)
		}
		if cErr := edgePresignSigns(presigned, "content-length"); cErr != nil {
			s.dropEdgeChunkClaim(ctx, row.TenantID, keyed, since)
			return nil, http.StatusInternalServerError, fmt.Errorf("the chunk link does not sign its length: %w", cErr)
		}
		sendHeaders := map[string]string{"content-length": strconv.FormatInt(sealedLen, 10)}
		for _, name := range signed {
			switch name {
			case "host", "content-length":
			default:
				s.dropEdgeChunkClaim(ctx, row.TenantID, keyed, since)
				return nil, http.StatusInternalServerError, fmt.Errorf("the chunk link signs a header the host does not send: %s", name)
			}
		}
		resp.Misses = append(resp.Misses, edgeIndexMiss{Hash: c.Hash, URL: presigned, SendHeaders: sendHeaders, SealedLength: sealedLen})
		edgeCodecChunks.WithLabelValues("miss").Inc()
	}
	if len(claims) > 0 {
		if err := recordEdgeCodecClaims(ctx, s.db, req.AdmitID, claims); err != nil {
			// The claims stand either way (the takeover window and the chunk
			// orphan sweep cover an unrecorded one); the expiry just cannot
			// drop them early.
			s.logger.Warn("edge index: claims not recorded on the admit", zap.Error(err), zap.String("admit_id", req.AdmitID))
		}
	}
	return resp, http.StatusOK, nil
}

// readPendingCodecAdmit reads a codec admit that may still be fed.
func readPendingCodecAdmit(ctx context.Context, db *sql.DB, id string) (edgeAdmitRow, error) {
	var row edgeAdmitRow
	var attrs []byte
	err := db.QueryRowContext(ctx, `SELECT id, tenant_id, bucket, object_key, op, state, codec, content_length, storage_key, attrs, expires_at
		FROM edge_admits WHERE id = $1`, id).
		Scan(&row.ID, &row.TenantID, &row.Bucket, &row.Key, &row.Op, &row.State, &row.Codec, &row.ContentLength, &row.StorageKey, &attrs, &row.ExpiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		return row, errEdgeAdmitUnknown
	}
	if err != nil {
		return row, err
	}
	if len(attrs) > 0 {
		_ = json.Unmarshal(attrs, &row.Attrs)
	}
	if !row.Codec {
		return row, errors.New("not a codec admit")
	}
	if row.State != "pending" || time.Now().After(row.ExpiresAt) {
		return row, errEdgeAdmitExpired
	}
	return row, nil
}

func edgeAdmitErrStatus(err error) int {
	switch {
	case errors.Is(err, errEdgeAdmitUnknown):
		return http.StatusNotFound
	case errors.Is(err, errEdgeAdmitExpired):
		return http.StatusGone
	default:
		return http.StatusBadRequest
	}
}

// recordEdgeCodecClaims appends claims to the admit's attrs under the row's
// lock, so the expiry can drop them (expireEdgeAdmit).
func recordEdgeCodecClaims(ctx context.Context, db *sql.DB, id string, claims map[string]time.Time) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	row, err := lockEdgeAdmit(ctx, tx, id)
	if err != nil {
		return err
	}
	if row.Attrs.CodecClaims == nil {
		row.Attrs.CodecClaims = map[string]time.Time{}
	}
	for k, v := range claims {
		row.Attrs.CodecClaims[k] = v
	}
	attrs, err := json.Marshal(row.Attrs)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE edge_admits SET attrs = $2 WHERE id = $1`, id, attrs); err != nil {
		return err
	}
	return tx.Commit()
}

// edgeCodecTakeover is how old an edge chunk claim must be before another
// writer may take it over: the admit's whole life — a chunk link lives 15
// minutes and may still write the one address (file comment).
const edgeCodecTakeover = edgeAdmitExpiry

// claimEdgeChunk is chunk_claim.go's claim for a codec chunk (file comment:
// the two differences). hit = reference at commit; otherwise the claim is
// this admit's, since identifying it.
func (s *Server) claimEdgeChunk(ctx context.Context, tenantID, keyed string, version int, size int64) (hit bool, since time.Time, err error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, time.Time{}, err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtext($1), hashtext($2))`, tenantID, keyed); err != nil {
		return false, time.Time{}, err
	}
	var state string
	var abandoned bool
	err = tx.QueryRowContext(ctx, `
		SELECT state, state = 'pending' AND pending_since < clock_timestamp() - make_interval(secs => $3)
		  FROM global_content_index WHERE dedup_scope = $1 AND plaintext_hash = $2 FOR UPDATE`,
		tenantID, keyed, edgeCodecTakeover.Seconds()).Scan(&state, &abandoned)
	found := err == nil
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return false, time.Time{}, err
	}
	blobKey := edgeCodecChunkKey(tenantID, keyed, version)
	switch {
	case found && state == "ready":
		return true, time.Time{}, tx.Commit()
	case found && !abandoned:
		// A young claim: another host is storing it right now. Answered as a
		// hit; the commit's IncrementRef is the check (0 rows = retry).
		return true, time.Time{}, tx.Commit()
	case found:
		// Abandoned (its admit expired): taken over with this write's shape.
		err = tx.QueryRowContext(ctx, `
			UPDATE global_content_index
			   SET backend_id = $4, storage_key = $5, size_bytes = $6, compressed_size = NULL, compression_algo = NULL,
			       encrypted = TRUE, encryption_algo = $7, ciphertext_hash = NULL, encryption_key_version = $8,
			       keyed_hash = $3, pending_since = clock_timestamp(), marked_for_deletion = FALSE, marked_at = NULL
			 WHERE dedup_scope = $1 AND plaintext_hash = $2 AND state = 'pending'
			RETURNING pending_since`,
			tenantID, keyed, keyed, s.writeAdapter().primaryName(), blobKey, size, crypto.EdgeChunkEncryptionAlgo, version).Scan(&since)
	default:
		err = tx.QueryRowContext(ctx, `
			INSERT INTO global_content_index
			  (dedup_scope, plaintext_hash, keyed_hash, backend_id, storage_key, size_bytes,
			   encrypted, encryption_algo, encryption_key_version, ref_count, state, pending_since)
			VALUES ($1, $2, $3, $4, $5, $6, TRUE, $7, $8, 0, 'pending', clock_timestamp())
			RETURNING pending_since`,
			tenantID, keyed, keyed, s.writeAdapter().primaryName(), blobKey, size, crypto.EdgeChunkEncryptionAlgo, version).Scan(&since)
	}
	if err != nil {
		return false, time.Time{}, err
	}
	if err := tx.Commit(); err != nil {
		return false, time.Time{}, err
	}
	s.gci.InvalidateCache(tenantID, keyed)
	return false, since, nil
}

// dropEdgeChunkClaim removes this admit's claim (ref 0, ours by
// pending_since) — a failed commit, an expiry, a link that could not be
// made. Best effort on its own clock, as dropChunkClaim.
func (s *Server) dropEdgeChunkClaim(ctx context.Context, tenantID, keyed string, since time.Time) {
	dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if _, err := s.db.ExecContext(dctx, `
		DELETE FROM global_content_index
		 WHERE dedup_scope = $1 AND plaintext_hash = $2 AND state = 'pending' AND pending_since = $3 AND ref_count = 0`,
		tenantID, keyed, since); err != nil {
		s.logger.Warn("edge codec: a claim could not be dropped — it is taken over after its window", zap.Error(err))
	}
	s.gci.InvalidateCache(tenantID, keyed)
}

// dropEdgeCodecClaims drops every claim an admit's index calls recorded
// (the expiry's and the failed commit's rule).
func (s *Server) dropEdgeCodecClaims(ctx context.Context, row edgeAdmitRow) {
	for keyed, since := range row.Attrs.CodecClaims {
		s.dropEdgeChunkClaim(ctx, row.TenantID, keyed, since)
	}
}

// --- the commit ---

// edgeCodecRef is one settled chunk of a codec commit: what the manifest
// row records.
type edgeCodecRef struct {
	Keyed          string  `json:"h"`
	Size           int64   `json:"s"`
	SealedSize     int64   `json:"z,omitempty"`
	CiphertextHash *string `json:"e,omitempty"`
	KeyVersion     int     `json:"v"`
}

// settleEdgeCodecChunks turns the commit's chunk list into manifest refs:
// a stored chunk's claim flips ready (ciphertext_hash = the backend's MD5
// ETag of the sealed bytes, +1 reference); a hit takes its reference NOW
// (IncrementRef — 0 rows is the commit's error: the chunk vanished or its
// writer never landed; the host retries). On any error every reference
// taken is given back and every claim of ours dropped.
func (s *Server) settleEdgeCodecChunks(ctx context.Context, row edgeAdmitRow, chunks []edgeCommitChunk) ([]edgeCodecRef, error) {
	version := row.Attrs.CodecVersion
	indexKey, _, err := s.tenantKeys.IndexKey(ctx, row.TenantID, version)
	if err != nil {
		return nil, fmt.Errorf("index key: %w", err)
	}
	defer clear(indexKey)

	refs := make([]edgeCodecRef, 0, len(chunks))
	var taken []string // keyed hashes referenced so far (compensation)
	fail := func(ferr error) ([]edgeCodecRef, error) {
		cctx := context.WithoutCancel(ctx)
		for _, keyed := range taken {
			if _, decErr := s.gci.DecrementRef(cctx, row.TenantID, keyed); decErr != nil {
				s.logger.Warn("edge codec: compensating ref decrement failed (reconcile heals)", zap.Error(decErr))
			}
		}
		s.dropEdgeCodecClaims(cctx, row)
		edgeCodecChunks.WithLabelValues("failed").Inc()
		return nil, ferr
	}

	for _, c := range chunks {
		plain, decErr := hex.DecodeString(c.Hash)
		if decErr != nil || len(plain) != 32 {
			return fail(errors.New("a chunk hash is sha256 hex"))
		}
		if c.Size < 1 || c.Size > crypto.ChunkMaxSize {
			return fail(fmt.Errorf("a chunk is 1..%d bytes", crypto.ChunkMaxSize))
		}
		keyed, kErr := crypto.KeyedChunkHash(indexKey, plain)
		if kErr != nil {
			return fail(kErr)
		}
		switch c.Status {
		case "stored":
			etag := strings.ToLower(strings.Trim(strings.TrimSpace(c.BackendETag), `"`))
			if len(etag) != 32 || !isHex(etag) {
				return fail(fmt.Errorf("a stored chunk's backend ETag is its sealed bytes' MD5"))
			}
			if want := crypto.WrappedChunkSealedLength(c.Size); c.SealedSize != want {
				return fail(fmt.Errorf("a stored chunk's sealed size is a function of its size (%d, not %d)", want, c.SealedSize))
			}
			since, isOurs := row.Attrs.CodecClaims[keyed]
			if !isOurs {
				return fail(errors.New("a stored chunk this admit never claimed"))
			}
			flipped, fErr := s.readyEdgeChunk(ctx, row.TenantID, keyed, since, etag)
			if fErr != nil {
				return fail(fErr)
			}
			if flipped {
				taken = append(taken, keyed)
				refs = append(refs, edgeCodecRef{Keyed: keyed, Size: c.Size, SealedSize: c.SealedSize, CiphertextHash: &etag, KeyVersion: version})
				edgeCodecChunks.WithLabelValues("stored").Inc()
				edgeCodecBytes.WithLabelValues("sealed").Add(float64(c.SealedSize))
				continue
			}
			// Taken over meanwhile (this admit expired and was revived? a
			// retry's second commit?): fall through to referencing whatever
			// row stands, exactly as a hit.
			fallthrough
		case "hit":
			ct, ver, refErr := s.referenceEdgeChunk(ctx, row.TenantID, keyed)
			if refErr != nil {
				return fail(refErr)
			}
			taken = append(taken, keyed)
			refs = append(refs, edgeCodecRef{Keyed: keyed, Size: c.Size, CiphertextHash: ct, KeyVersion: ver})
			edgeCodecChunks.WithLabelValues("hit").Inc()
		default:
			return fail(fmt.Errorf("a chunk's status is stored or hit, not %q", c.Status))
		}
	}
	return refs, nil
}

// errEdgeChunkVanished: a hit's row is not ready any more (GC, a dead
// writer's claim) — the host retries the part (503 SlowDown).
var errEdgeChunkVanished = errors.New("a deduplicated chunk vanished before the commit referenced it")

// readyEdgeChunk flips this admit's claim ready with the stored blob's
// ciphertext MD5 and one reference. flipped = false when the claim is not
// ours any more (the caller references whatever row stands).
func (s *Server) readyEdgeChunk(ctx context.Context, tenantID, keyed string, since time.Time, ciphertextMD5 string) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtext($1), hashtext($2))`, tenantID, keyed); err != nil {
		return false, err
	}
	res, err := tx.ExecContext(ctx, `
		UPDATE global_content_index
		   SET state = 'ready', pending_since = NULL, ciphertext_hash = $4, ref_count = ref_count + 1,
		       last_accessed_at = NOW(), marked_for_deletion = FALSE, marked_at = NULL
		 WHERE dedup_scope = $1 AND plaintext_hash = $2 AND state = 'pending' AND pending_since = $3`,
		tenantID, keyed, since, ciphertextMD5)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	if err := tx.Commit(); err != nil {
		return false, err
	}
	s.gci.InvalidateCache(tenantID, keyed)
	return n == 1, nil
}

// referenceEdgeChunk takes a hit's reference and reads what the ready row
// records (the blob's ciphertext hash and key version — copied, never
// recomputed: storeChunkedObject's rule).
func (s *Server) referenceEdgeChunk(ctx context.Context, tenantID, keyed string) (*string, int, error) {
	var ct sql.NullString
	var ver sql.NullInt32
	err := s.db.QueryRowContext(ctx, `
		UPDATE global_content_index
		   SET ref_count = ref_count + 1, last_accessed_at = NOW(), marked_for_deletion = FALSE, marked_at = NULL
		 WHERE dedup_scope = $1 AND plaintext_hash = $2 AND state = 'ready'
		RETURNING ciphertext_hash, encryption_key_version`, tenantID, keyed).Scan(&ct, &ver)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, 0, errEdgeChunkVanished
	}
	if err != nil {
		return nil, 0, err
	}
	s.gci.InvalidateCache(tenantID, keyed)
	var ctp *string
	if ct.Valid {
		v := ct.String
		ctp = &v
	}
	return ctp, int(ver.Int32), nil
}

// commitEdgeCodecPut is the codec PUT's commit: the chunks settled, then the
// manifest at the admit's address and the chunked head row in ONE
// transaction through commitBlobWrite — exactly storeChunkedObject's rows
// for the same body.
func (s *Server) commitEdgeCodecPut(ctx context.Context, row edgeAdmitRow, req edgeCommitRequest, rec *relayRecorder) (string, int64) {
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
	if req.Outcome != edgeOutcomeOK {
		rec.Header().Set("Retry-After", "30")
		return fail(ErrServiceUnavailable)
	}
	etag := strings.ToLower(strings.Trim(strings.TrimSpace(req.BodyMD5), `"`))
	if len(etag) != 32 || !isHex(etag) {
		return fail(ErrInvalidDigest, WithSuggestion("A codec commit carries the whole body's MD5 (hex)."))
	}
	var total int64
	for _, c := range req.Chunks {
		total += c.Size
	}
	if total != row.ContentLength || len(req.Chunks) == 0 {
		return fail(ErrIncompleteBody)
	}
	codecRefs, err := s.settleEdgeCodecChunks(cctx, row, req.Chunks)
	if err != nil {
		s.releaseEdgeAdmit(ctx, row)
		if errors.Is(err, errEdgeChunkVanished) {
			rec.Header().Set("Retry-After", "5")
			WriteS3ErrorWithContext(rec, ErrServiceUnavailable, path, reqID,
				WithSuggestion("A deduplicated chunk vanished before the commit; retry the upload."))
			s.recordEdgeAccess(row, rec.status, 0, 0)
			return "failed", 0
		}
		s.logger.Warn("edge codec commit: chunks not settled", zap.Error(err), zap.String("admit_id", row.ID))
		WriteS3ErrorWithContext(rec, ErrInvalidRequest, path, reqID, WithSuggestion(err.Error()))
		s.recordEdgeAccess(row, rec.status, 0, 0)
		return "failed", 0
	}

	var bill *writeBill
	if s.quotaManager != nil {
		bill = &writeBill{Floor: row.ReservedFloor, Reserved: row.ReservedBytes, FloorReserved: row.FloorReserved, Credited: row.Credited}
	}
	attrs := row.Attrs.Attrs
	contentType := attrs.ContentType
	if contentType == "" {
		contentType = "application/octet-stream"
		attrs.ContentType = contentType
	}
	refs := make([]crypto.TenantChunkRef, 0, len(codecRefs))
	var offset int64
	for i, cr := range codecRefs {
		refs = append(refs, crypto.TenantChunkRef{TenantID: row.TenantID, BucketName: row.Bucket, ObjectKey: row.StorageKey,
			ChunkIndex: i, ChunkOffset: offset, PlaintextHash: cr.Keyed, DedupScope: row.TenantID,
			EncryptionKeyVersion: cr.KeyVersion, CiphertextHash: cr.CiphertextHash})
		offset += cr.Size
	}
	chunker, err := crypto.DefaultChunker()
	if err != nil {
		s.releaseEdgeAdmit(ctx, row)
		WriteS3Error(rec, ErrInternalError, path, reqID)
		return "failed", 0
	}
	count := len(codecRefs)
	flip, dbErr := commitBlobWrite(cctx, s.db, blobWrite{TenantID: row.TenantID, Bucket: row.Bucket, Key: row.Key, Bill: bill,
		Version: &versionWrite{Size: total, ETag: etag, ContentType: contentType, Backend: chunkedBackendSentinel,
			Blob: versionBlobRef{StorageKey: row.StorageKey, Enc: objectEncryption{Algorithm: crypto.EdgeChunkEncryptionAlgo}}},
		Upsert: func(tx *sql.Tx) error {
			if metaErr := s.gci.ReplaceObjectManifestTx(cctx, tx, row.TenantID, row.Bucket, row.StorageKey, refs, &crypto.ObjectMeta{
				TenantID: row.TenantID, BucketName: row.Bucket, ObjectKey: row.StorageKey,
				TotalSize: total, ChunkCount: count, ContentType: &contentType, LogicalSize: total,
				PipelineConfig: chunkedPipeline(chunker, true),
			}); metaErr != nil {
				return fmt.Errorf("replace object manifest: %w", metaErr)
			}
			return upsertChunkedObjectHeadRow(cctx, tx, row.TenantID, row.Bucket, row.Key, row.StorageKey, total, etag,
				crypto.EdgeChunkEncryptionAlgo, attrs)
		}})
	if dbErr != nil {
		// Nothing installed: the refs this commit took are given back and
		// the claims dropped (chunk blobs that landed are dedup GC's after
		// the grace), the reservation released.
		compCtx := context.WithoutCancel(cctx)
		for _, cr := range codecRefs {
			if _, decErr := s.gci.DecrementRef(compCtx, row.TenantID, cr.Keyed); decErr != nil {
				s.logger.Warn("edge codec commit: compensating decrement failed (reconcile heals)", zap.Error(decErr))
			}
		}
		s.releaseEdgeAdmit(ctx, row)
		s.dropEdgeCodecClaims(compCtx, row)
		sr := (&http.Request{Method: http.MethodPut, URL: &url.URL{Path: path}}).WithContext(cctx)
		switch {
		case refusedAtCommit(rec, sr, dbErr):
		case errors.Is(dbErr, errQuotaRace):
			WriteS3ErrorWithContext(rec, ErrQuotaExceeded, path, reqID, WithSuggestion("Upgrade at "+billingURL()))
		default:
			s.logger.Error("edge codec commit: install failed", zap.Error(dbErr), zap.String("tenant_id", row.TenantID),
				zap.String("bucket", row.Bucket), zap.String("key", row.Key))
			WriteS3Error(rec, ErrInternalError, path, reqID)
		}
		s.recordEdgeAccess(row, rec.status, 0, 0)
		return "failed", 0
	}
	applyObjectLockOnPutHeaders(cctx, s.db, row)
	edgeCodecBytes.WithLabelValues("plaintext").Add(float64(total))
	rec.Header().Set("ETag", `"`+etag+`"`)
	rec.Header().Set("x-amz-request-id", reqID)
	rec.Header().Set("x-amz-object-size", strconv.FormatInt(total, 10))
	rec.Header().Set("x-amz-server-side-encryption", "AES256")
	if flip.VersionID != "" {
		rec.Header().Set("x-amz-version-id", flip.VersionID)
	}
	rec.WriteHeader(http.StatusOK)
	s.recordEdgeAccess(row, http.StatusOK, total, 0)
	s.logger.Info("artifact stored", zap.String("tenant_id", row.TenantID), zap.String("s3.bucket", row.Bucket), zap.String("s3.object", row.Key),
		zap.String("backend", chunkedBackendSentinel), zap.Bool("codec", true), zap.String("etag", etag), zap.Int("chunks", count),
		zap.Int64("size", total), zap.String("host", edgeHost))
	NewNotificationDispatcher(s.db, s.logger).Fire(row.TenantID, row.Bucket, "s3:ObjectCreated:Put", row.Key, total, etag)
	emitEvent(cctx, s.db, s.logger, "object.created", row.TenantID, map[string]interface{}{
		"bucket": row.Bucket, "key": row.Key, "size": total, "etag": etag,
	})
	return "committed", total
}

// applyObjectLockOnPutHeaders replays the admit's lock headers, as
// commitEdgePut does.
func applyObjectLockOnPutHeaders(ctx context.Context, db *sql.DB, row edgeAdmitRow) {
	lr := &http.Request{Header: http.Header{}}
	if row.Attrs.LockMode != "" {
		lr.Header.Set("x-amz-object-lock-mode", row.Attrs.LockMode)
		lr.Header.Set("x-amz-object-lock-retain-until-date", row.Attrs.LockUntil)
	}
	applyObjectLockOnPut(ctx, db, row.TenantID, row.Bucket, row.Key, lr)
}
