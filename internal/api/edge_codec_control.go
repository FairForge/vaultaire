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
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"go.uber.org/zap"

	"github.com/FairForge/vaultaire/internal/common"
	"github.com/FairForge/vaultaire/internal/crypto"
	"github.com/FairForge/vaultaire/internal/dataplane"
	"github.com/FairForge/vaultaire/internal/engine"
)

// edgeIndexMaxChunks bounds one index call (one call per part).
const edgeIndexMaxChunks = 10000

var (
	edgeCodecChunks = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "vaultaire_edge_codec_chunks_total",
		Help: "Chunks of edge-codec writes by outcome. At the index (once per distinct hash asked): hit (a ready row, or another writer's young claim), miss (a claim taken and a link handed out). At the commit (once per chunk of the body): referenced (a hit's reference taken), stored (a miss's blob committed ready), head_mismatch (a commit refused: the backend holds other bytes than the host reported), failed (a commit whose chunks could not be settled).",
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
	for _, o := range []string{"hit", "miss", "referenced", "stored", "head_mismatch", "failed"} {
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
	// The codec's threshold at the edge is EDGE_MIN_BYTES (RP68-1): every
	// edge write of a codec tenant at or above it is chunked.
	return &edgeCodecGrant{Version: version, KEMPublicKey: pub,
		ChunkThreshold: s.edge.minBytes, SegmentSize: crypto.StreamSegmentSize}, true
}

// edgeCodecStoreReady reports whether the chunk store's backend (the engine
// primary) can take codec chunks from the edge: an EdgeWriter whose breaker
// is closed that also answers a HEAD's ETag and size (engine.ObjectHeader —
// the commit's sample, RP68-3).
func (s *Server) edgeCodecStoreReady() bool {
	primary := s.engine.GetPrimary()
	if _, err := s.edgeWriterFor(primary); err != nil {
		return false
	}
	drv, ok := s.engine.GetDriver(primary)
	if !ok {
		return false
	}
	_, ok = drv.(engine.ObjectHeader)
	return ok
}

// edgeAdmitCodecPut is the codec gate of a PUT admit, called once the
// placement said CHUNKED: with the flag and a key version it answers the
// codec admit; with the flag and NO key version it tells the caller to admit
// Part A's whole PUT (never a refusal, never a plaintext chunked write at
// the edge); without the flag it stands aside (the pass-through).
func (s *Server) edgeAdmitCodecPut(req edgeAdmitRequest, p edgePrelude, size int64, attrs objectAttrs,
	storageClass, floor string) (edgeAdmitResponse, int, edgeCodecVerdict) {
	sr, sctx, rec, tenantID, bucket, key := p.sr, p.sctx, p.rec, p.tenantID, p.bucket, p.key
	if !s.edgeCodecFor(tenantID) {
		return edgeAdmitResponse{}, 0, edgeCodecNo
	}
	grant, ok := s.edgeCodecGrantFor(sctx, tenantID)
	if !ok {
		s.logger.Debug("edge admit: no_key — edge_codec is on but this process holds no tenant key version; the whole PUT",
			zap.String("tenant_id", tenantID), zap.String("request_id", req.RequestID))
		return edgeAdmitResponse{}, 0, edgeCodecWhole
	}
	// The chunk links are signed on the engine primary (the chunk store's
	// placement): it must be an EdgeWriter whose breaker is closed and that
	// answers the commit's HEAD sample, or the origin takes the write (the
	// pass-through, as Part A's backend rule).
	if !s.edgeCodecStoreReady() {
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
			CodecVersion: grant.Version, MirrorLeg: s.mirrorFor(tenantID, s.engine.GetPrimary())},
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

// The index call's wire types (internal/dataplane).
type (
	// edgeIndexRequest is one batch of the host's chunk hashes.
	edgeIndexRequest = dataplane.IndexRequest
	// edgeIndexMiss is a chunk the host must store: one presigned PUT at the
	// chunk's one address, signing the SEALED length.
	edgeIndexMiss = dataplane.IndexMiss
	// edgeIndexResponse answers the call.
	edgeIndexResponse = dataplane.IndexResponse
)

// handleEdgeIndex is POST /api/v1/edge/index (the service-token gate, as
// admit and commit).
func (s *Server) handleEdgeIndex(w http.ResponseWriter, r *http.Request) {
	host, _, ok := s.edgeCaller(r)
	if !ok {
		s.edgeNotFound(w, r)
		return
	}
	r = r.WithContext(dataplane.WithHost(r.Context(), host))
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

	// Every chunk validated and keyed first (no database work), then ONE
	// transaction claims them all (RP70-4), then the links are presigned
	// (no database work). A link that cannot be made drops every claim this
	// call took: the host's call fails whole, as it did per chunk.
	items := make([]edgeIndexItem, 0, len(req.Chunks))
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
		items = append(items, edgeIndexItem{hash: c.Hash, keyed: keyed, size: c.Size, public: c.Public})
	}
	// A chunk the host marked public is the catalogue's only when
	// public_chunks names it and the `_public` copy is ready — a false
	// positive of the filter is an ordinary chunk (WP-EDGE-4 PR D).
	if err := s.verifyPublicChunks(ctx, items); err != nil {
		return nil, http.StatusInternalServerError, fmt.Errorf("public catalogue: %w", err)
	}
	if err := s.claimEdgeChunks(ctx, req.AdmitID, row.TenantID, version, items); err != nil {
		if errors.Is(err, errEdgeAdmitExpired) || errors.Is(err, errEdgeAdmitUnknown) {
			return nil, edgeAdmitErrStatus(err), err
		}
		return nil, http.StatusInternalServerError, fmt.Errorf("claim: %w", err)
	}
	dropAll := func() {
		for _, it := range items {
			if !it.hit {
				s.dropEdgeChunkClaim(ctx, row.TenantID, it.keyed, it.since)
			}
		}
	}

	var mirrorEW engine.EdgeWriter
	if row.Attrs.MirrorLeg != "" {
		if mirrorEW, err = s.edgeWriterFor(row.Attrs.MirrorLeg); err != nil {
			mirrorEW = nil // the leg went away since the admit: the job's copies
		}
	}
	resp := &edgeIndexResponse{Hits: []string{}, Misses: []edgeIndexMiss{}}
	for _, it := range items {
		if it.hit {
			resp.Hits = append(resp.Hits, it.hash)
			edgeCodecChunks.WithLabelValues("hit").Inc()
			continue
		}
		sealedLen := crypto.WrappedChunkSealedLength(it.size)
		blobKey := edgeCodecChunkKey(row.TenantID, it.keyed, version)
		presigned, signed, pErr := ew.PresignPut(engine.ChunkContext(ctx), engine.ChunkContainer, blobKey,
			engine.EdgePresign{ContentLength: sealedLen, TTL: edgeAdmitTTL})
		if pErr != nil {
			dropAll()
			return nil, http.StatusInternalServerError, fmt.Errorf("presign chunk link: %w", pErr)
		}
		if cErr := edgePresignSigns(presigned, "content-length"); cErr != nil {
			dropAll()
			return nil, http.StatusInternalServerError, fmt.Errorf("the chunk link does not sign its length: %w", cErr)
		}
		sendHeaders := map[string]string{"content-length": strconv.FormatInt(sealedLen, 10)}
		for _, name := range signed {
			switch name {
			case "host", "content-length":
			default:
				dropAll()
				return nil, http.StatusInternalServerError, fmt.Errorf("the chunk link signs a header the host does not send: %s", name)
			}
		}
		miss := edgeIndexMiss{Hash: it.hash, URL: presigned, SendHeaders: sendHeaders, SealedLength: sealedLen}
		if mirrorEW != nil {
			// WP-EDGE-4: the same sealed bytes at the same address on the
			// mirror leg; a link that cannot be made leaves the copy to the
			// job (the commit's row says pending).
			if murl, msigned, mErr := mirrorEW.PresignPut(engine.ChunkContext(ctx), engine.ChunkContainer, blobKey,
				engine.EdgePresign{ContentLength: sealedLen, TTL: edgeAdmitTTL}); mErr == nil && sameHeaderSet(signed, msigned) {
				miss.URLs = []string{presigned, murl}
			}
		}
		resp.Misses = append(resp.Misses, miss)
		edgeCodecChunks.WithLabelValues("miss").Inc()
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

// edgeCodecTakeover is how old an edge chunk claim must be before another
// writer may take it over: the admit's whole life — a chunk link lives 15
// minutes and may still write the one address (file comment).
const edgeCodecTakeover = edgeAdmitExpiry

// edgeIndexItem is one chunk of an index call: its plain hash (hex, for the
// answer only), its keyed hash, its size — and what the claim decided: hit
// (reference at commit) or a claim of this admit, since identifying it.
type edgeIndexItem struct {
	hash  string
	keyed string
	size  int64
	hit   bool
	since time.Time
	// public: the catalogue holds the chunk (the host marked it, the
	// control plane checked public_chunks): a hit on the `_public` copy,
	// never a claim.
	public bool
}

// claimEdgeChunks is chunk_claim.go's claim for every chunk of one index
// call (file comment: the two differences), in ONE transaction (RP70-4):
// the admit row locked FOR UPDATE (it must still be pending; concurrent
// calls of one admit merge their claims one after the other), then per
// chunk IN KEYED-HASH ORDER — two calls sharing chunks take their locks in
// the same order and can never deadlock — the chunk's advisory lock, its
// row FOR UPDATE, and the claim (an insert, or the takeover of an abandoned
// one); then the claims recorded on the admit (the expiry drops them), one
// commit. items[i].hit / .since are filled in.
func (s *Server) claimEdgeChunks(ctx context.Context, admitID, tenantID string, version int, items []edgeIndexItem) error {
	order := make([]int, len(items))
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(a, b int) bool { return items[order[a]].keyed < items[order[b]].keyed })

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	admit, err := lockEdgeAdmit(ctx, tx, admitID)
	if errors.Is(err, sql.ErrNoRows) {
		return errEdgeAdmitUnknown
	}
	if err != nil {
		return err
	}
	if admit.State != "pending" || time.Now().After(admit.ExpiresAt) {
		return errEdgeAdmitExpired
	}
	primary := s.writeAdapter().primaryName()
	var claimed []string
	var public []string
	for _, i := range order {
		it := &items[i]
		if it.public {
			// The catalogue's: never a claim — and never in `_public`
			// (contract line 5: nothing a tenant uploads enters it).
			it.hit = true
			if admit.Attrs.CodecPublic == nil {
				admit.Attrs.CodecPublic = map[string]bool{}
			}
			admit.Attrs.CodecPublic[it.hash] = true
			public = append(public, it.hash)
			continue
		}
		if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtext($1), hashtext($2))`, tenantID, it.keyed); err != nil {
			return err
		}
		var state string
		var abandoned bool
		err := tx.QueryRowContext(ctx, `
			SELECT state, state = 'pending' AND pending_since < clock_timestamp() - make_interval(secs => $3)
			  FROM global_content_index WHERE dedup_scope = $1 AND plaintext_hash = $2 FOR UPDATE`,
			tenantID, it.keyed, edgeCodecTakeover.Seconds()).Scan(&state, &abandoned)
		found := err == nil
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		blobKey := edgeCodecChunkKey(tenantID, it.keyed, version)
		switch {
		case found && state == "ready":
			it.hit = true
			continue
		case found && !abandoned:
			// A young claim: another host is storing it right now (or this
			// call named the chunk twice). Answered as a hit; the commit's
			// IncrementRef is the check (0 rows = retry).
			it.hit = true
			continue
		case found:
			// Abandoned (its admit expired): taken over with this write's shape.
			err = tx.QueryRowContext(ctx, `
				UPDATE global_content_index
				   SET backend_id = $4, storage_key = $5, size_bytes = $6, compressed_size = NULL, compression_algo = NULL,
				       encrypted = TRUE, encryption_algo = $7, ciphertext_hash = NULL, encryption_key_version = $8,
				       keyed_hash = $3, pending_since = clock_timestamp(), marked_for_deletion = FALSE, marked_at = NULL
				 WHERE dedup_scope = $1 AND plaintext_hash = $2 AND state = 'pending'
				RETURNING pending_since`,
				tenantID, it.keyed, it.keyed, primary, blobKey, it.size, crypto.EdgeChunkEncryptionAlgo, version).Scan(&it.since)
		default:
			err = tx.QueryRowContext(ctx, `
				INSERT INTO global_content_index
				  (dedup_scope, plaintext_hash, keyed_hash, backend_id, storage_key, size_bytes,
				   encrypted, encryption_algo, encryption_key_version, ref_count, state, pending_since)
				VALUES ($1, $2, $3, $4, $5, $6, TRUE, $7, $8, 0, 'pending', clock_timestamp())
				RETURNING pending_since`,
				tenantID, it.keyed, it.keyed, primary, blobKey, it.size, crypto.EdgeChunkEncryptionAlgo, version).Scan(&it.since)
		}
		if err != nil {
			return err
		}
		if admit.Attrs.CodecClaims == nil {
			admit.Attrs.CodecClaims = map[string]time.Time{}
		}
		admit.Attrs.CodecClaims[it.keyed] = it.since
		claimed = append(claimed, it.keyed)
	}
	if len(claimed) > 0 || len(public) > 0 {
		attrs, err := json.Marshal(admit.Attrs)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE edge_admits SET attrs = $2 WHERE id = $1`, admitID, attrs); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	for _, keyed := range claimed {
		s.gci.InvalidateCache(tenantID, keyed)
	}
	return nil
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
	// Public (WP-EDGE-4 PR D): the chunk is the catalogue's — Keyed is then
	// its PLAIN hash in the `_public` scope.
	Public         bool    `json:"p,omitempty"`
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
	var taken []edgeCodecRef // references taken so far (compensation)
	fail := func(ferr error) ([]edgeCodecRef, error) {
		cctx := context.WithoutCancel(ctx)
		for _, cr := range taken {
			if _, decErr := s.gci.DecrementRef(cctx, cr.scope(row.TenantID), cr.Keyed); decErr != nil {
				s.logger.Warn("edge codec: compensating ref decrement failed (reconcile heals)", zap.Error(decErr))
			}
		}
		s.dropEdgeCodecClaims(cctx, row)
		if errors.Is(ferr, errEdgeChunkHeadMismatch) {
			edgeCodecChunks.WithLabelValues("head_mismatch").Inc()
		} else {
			edgeCodecChunks.WithLabelValues("failed").Inc()
		}
		return nil, ferr
	}

	stored := 0 // stored chunks seen: the HEAD sample's position
	// A tenant-scoped (BYO) host's reports are never trusted for integrity
	// (RP68-3 at 100 %): EVERY stored chunk of its commit is opened.
	host, _ := dataplane.HostFrom(ctx)
	openAll := host.TenantScoped()
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
			// The host's word for the blob is checked against the backend
			// before the claim flips: the FIRST stored chunk of the commit is
			// OPENED — its header and first segment, under the tenant's codec
			// seed (RP70-3: a wrong-key blob has an ETag and size of its own)
			// — and every edgeCodecHeadEvery-th after it is HEADed (RP68-3).
			switch {
			case stored == 0 || openAll:
				if oErr := s.openEdgeChunkPrefix(ctx, row.TenantID, keyed, version, since, etag, c.SealedSize); oErr != nil {
					return fail(oErr)
				}
			case stored%edgeCodecHeadEvery == 0:
				if hErr := s.headEdgeChunk(ctx, row.TenantID, keyed, version, since, etag, c.SealedSize); hErr != nil {
					return fail(hErr)
				}
			}
			stored++
			flipped, fErr := s.readyEdgeChunk(ctx, row.TenantID, keyed, since, etag)
			if fErr != nil {
				return fail(fErr)
			}
			if flipped {
				if row.Attrs.MirrorLeg != "" {
					// WP-EDGE-4: the stored chunk's copy on the mirror leg —
					// ready when the leg answered the same sealed bytes' MD5,
					// else owed (the job copies it from the primary). A
					// commit that fails after this leaves a row whose chunk
					// no index row names: the job's orphan pass removes it.
					mirrorETag := ""
					if len(c.BackendETags) > 1 {
						mirrorETag = c.BackendETags[1]
					}
					state, metag := mirrorCopyState(etag, mirrorETag)
					if cErr := recordBlobCopy(ctx, s.db, blobCopy{TenantID: row.TenantID, StorageKey: edgeCodecChunkKey(row.TenantID, keyed, version),
						Source: s.engine.GetPrimary(), Leg: row.Attrs.MirrorLeg, State: state, ETag: metag, Size: c.SealedSize}); cErr != nil {
						s.logger.Warn("edge codec: the chunk's copy row was not written — the backfill pass finds it", zap.Error(cErr))
					}
				}
				taken = append(taken, edgeCodecRef{Keyed: keyed})
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
			if row.Attrs.CodecPublic[c.Hash] {
				// The public catalogue's copy (WP-EDGE-4 PR D): its plain
				// hash in `_public`, plaintext — public content.
				rowSize, refErr := s.referencePublicChunk(ctx, c.Hash)
				if refErr != nil {
					return fail(refErr)
				}
				taken = append(taken, edgeCodecRef{Public: true, Keyed: c.Hash})
				if rowSize != c.Size {
					return fail(fmt.Errorf("%w: the host says %d bytes, the public row %d", errEdgeChunkSizeMismatch, c.Size, rowSize))
				}
				refs = append(refs, edgeCodecRef{Public: true, Keyed: c.Hash, Size: c.Size})
				edgeCodecChunks.WithLabelValues("public").Inc()
				continue
			}
			ct, ver, rowSize, refErr := s.referenceEdgeChunk(ctx, row.TenantID, keyed)
			if refErr != nil {
				return fail(refErr)
			}
			taken = append(taken, edgeCodecRef{Keyed: keyed})
			if rowSize != c.Size {
				// RP68-6: the host's size would shift every later chunk's
				// offset in the manifest.
				return fail(fmt.Errorf("%w: the host says %d bytes, the index row %d", errEdgeChunkSizeMismatch, c.Size, rowSize))
			}
			refs = append(refs, edgeCodecRef{Keyed: keyed, Size: c.Size, CiphertextHash: ct, KeyVersion: ver})
			edgeCodecChunks.WithLabelValues("referenced").Inc()
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

// edgeCodecHeadEvery: the commit HEADs the first stored chunk and every
// edgeCodecHeadEvery-th after it (RP68-3; a tenant-scoped host in Prompt 47
// gets every chunk).
const edgeCodecHeadEvery = 32

// errEdgeChunkHeadMismatch: the backend holds other bytes than the host
// reported for a stored chunk (RP68-3) — the commit's 400, everything given
// back. errEdgeChunkUnverified: the backend could not be asked — the
// commit's retryable 503.
var (
	errEdgeChunkHeadMismatch = errors.New("a stored chunk's backend ETag or size is not what the host reported (head_mismatch)")
	errEdgeChunkUnverified   = errors.New("a stored chunk could not be checked on the backend")
)

// edgeChunkRetryable: the commit's error is the host's retry (503 SlowDown),
// not its fault.
func edgeChunkRetryable(err error) bool {
	return errors.Is(err, errEdgeChunkVanished) || errors.Is(err, errEdgeChunkUnverified)
}

// headEdgeChunk HEADs a stored chunk at its one address on the backend its
// claim names and compares the backend's ETag (unquoted, lower-case) and
// size to what the host reported.
func (s *Server) headEdgeChunk(ctx context.Context, tenantID, keyed string, version int, since time.Time, etag string, sealedSize int64) error {
	var backend string
	if err := s.db.QueryRowContext(ctx, `SELECT backend_id FROM global_content_index
		WHERE dedup_scope = $1 AND plaintext_hash = $2 AND state = 'pending' AND pending_since = $3`,
		tenantID, keyed, since).Scan(&backend); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil // not ours any more: the flip below finds that and references what stands
		}
		return fmt.Errorf("%w: %w", errEdgeChunkUnverified, err)
	}
	hctx, cancel := context.WithTimeout(ctx, edgeBackendCallTimeout)
	defer cancel()
	info, err := s.writeAdapter().chunks().head(hctx, chunkAddr{scope: tenantID, hash: keyed, backend: backend, key: edgeCodecChunkKey(tenantID, keyed, version)})
	if err != nil {
		if isObjectMissingErr(err) {
			return fmt.Errorf("%w: the backend holds no blob at the chunk's address", errEdgeChunkHeadMismatch)
		}
		return fmt.Errorf("%w: %w", errEdgeChunkUnverified, err)
	}
	if strings.ToLower(strings.Trim(info.ETag, `"`)) != etag || info.Size != sealedSize {
		return fmt.Errorf("%w: the backend answers ETag %s, %d bytes", errEdgeChunkHeadMismatch, info.ETag, info.Size)
	}
	return nil
}

// openEdgeChunkPrefix reads a stored chunk's header and FIRST SEGMENT from
// the backend its claim names (one ranged read: at most
// WrappedChunkSealedLength(one segment) + the opener's one look-ahead byte,
// ~1.05 MiB whatever the part size) and opens them under the codec private
// seed of the admit's key version: OpenWrappedChunk's prime unwraps the
// chunk key and verifies the first segment's tag. It proves the host sealed
// under the tenant's key and the stream is well-formed; the keyed-hash
// binding of the whole plaintext stays with the read path and routing_truth
// (RP70-3). The backend's ETag and the blob's size are compared too, so the
// read also covers what the HEAD sample covers. A blob of other bytes, of
// another size or that does not open is errEdgeChunkHeadMismatch (the
// commit's 400); a backend that cannot be asked is errEdgeChunkUnverified.
func (s *Server) openEdgeChunkPrefix(ctx context.Context, tenantID, keyed string, version int, since time.Time, etag string, sealedSize int64) error {
	var backend string
	if err := s.db.QueryRowContext(ctx, `SELECT backend_id FROM global_content_index
		WHERE dedup_scope = $1 AND plaintext_hash = $2 AND state = 'pending' AND pending_since = $3`,
		tenantID, keyed, since).Scan(&backend); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil // not ours any more: the flip below finds that and references what stands
		}
		return fmt.Errorf("%w: %w", errEdgeChunkUnverified, err)
	}
	length := min(sealedSize, crypto.WrappedChunkSealedLength(crypto.StreamSegmentSize)+1)
	rctx, cancel := context.WithTimeout(ctx, edgeBackendCallTimeout)
	defer cancel()
	rc, info, err := s.writeAdapter().chunks().getRange(rctx, chunkAddr{scope: tenantID, hash: keyed, backend: backend,
		key: edgeCodecChunkKey(tenantID, keyed, version)}, 0, length)
	if err != nil {
		if isObjectMissingErr(err) {
			return fmt.Errorf("%w: the backend holds no blob at the chunk's address", errEdgeChunkHeadMismatch)
		}
		return fmt.Errorf("%w: %w", errEdgeChunkUnverified, err)
	}
	defer func() { _ = rc.Close() }()
	if strings.ToLower(strings.Trim(info.ETag, `"`)) != etag || info.Size != sealedSize {
		return fmt.Errorf("%w: the backend answers ETag %s, %d bytes", errEdgeChunkHeadMismatch, info.ETag, info.Size)
	}
	priv, err := s.tenantKeys.EdgeCodecPrivateKey(ctx, tenantID, version)
	if err != nil {
		return fmt.Errorf("%w: the codec key of version %d: %w", errEdgeChunkUnverified, version, err)
	}
	defer clear(priv)
	if _, _, err := crypto.OpenWrappedChunk(priv, io.LimitReader(rc, length)); err != nil {
		if errors.Is(err, crypto.ErrStreamCorrupt) || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return fmt.Errorf("%w: the first stored chunk does not open under the tenant's codec key: %w", errEdgeChunkHeadMismatch, err)
		}
		return fmt.Errorf("%w: %w", errEdgeChunkUnverified, err)
	}
	return nil
}

// errEdgeChunkSizeMismatch: a hit's size is not its index row's (RP68-6) —
// the commit's 400.
var errEdgeChunkSizeMismatch = errors.New("a deduplicated chunk's size is not its index row's")

// edgeCodecHitWaitDefault bounds a hit's wait on a young claim (RP68-2).
const edgeCodecHitWaitDefault = 60 * time.Second

// referenceEdgeChunk takes a hit's reference and reads what the ready row
// records (the blob's ciphertext hash, key version and plaintext size —
// copied, never recomputed: storeChunkedObject's rule). A row that is
// another writer's YOUNG claim (D-EDGE-20's hit) is waited for — parallel
// parts of one upload sharing a chunk (RP68-2): polled with chunk_claim.go's
// backoff (25 ms doubling to 1 s) for up to edgeCodecHitWait on the commit's
// own clock, then errEdgeChunkVanished; a row that is gone, or a claim past
// the takeover window, is errEdgeChunkVanished at once (the host retries
// and its index call stores it).
func (s *Server) referenceEdgeChunk(ctx context.Context, tenantID, keyed string) (*string, int, int64, error) {
	bound := s.edge.codecHitWait
	if bound <= 0 {
		bound = edgeCodecHitWaitDefault
	}
	wctx, cancel := context.WithTimeout(ctx, bound)
	defer cancel()
	const waitMin, waitMax = 25 * time.Millisecond, time.Second
	wait := waitMin
	for {
		var ct sql.NullString
		var ver sql.NullInt32
		var size int64
		err := s.db.QueryRowContext(wctx, `
			UPDATE global_content_index
			   SET ref_count = ref_count + 1, last_accessed_at = NOW(), marked_for_deletion = FALSE, marked_at = NULL
			 WHERE dedup_scope = $1 AND plaintext_hash = $2 AND state = 'ready'
			RETURNING ciphertext_hash, encryption_key_version, size_bytes`, tenantID, keyed).Scan(&ct, &ver, &size)
		if err == nil {
			s.gci.InvalidateCache(tenantID, keyed)
			var ctp *string
			if ct.Valid {
				v := ct.String
				ctp = &v
			}
			return ctp, int(ver.Int32), size, nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			if wctx.Err() != nil && ctx.Err() == nil {
				return nil, 0, 0, errEdgeChunkVanished
			}
			return nil, 0, 0, err
		}
		var young bool
		err = s.db.QueryRowContext(wctx, `
			SELECT state = 'pending' AND pending_since >= clock_timestamp() - make_interval(secs => $3)
			  FROM global_content_index WHERE dedup_scope = $1 AND plaintext_hash = $2`,
			tenantID, keyed, edgeCodecTakeover.Seconds()).Scan(&young)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			return nil, 0, 0, errEdgeChunkVanished
		case err != nil:
			if wctx.Err() != nil && ctx.Err() == nil {
				return nil, 0, 0, errEdgeChunkVanished
			}
			return nil, 0, 0, err
		case !young:
			return nil, 0, 0, errEdgeChunkVanished
		}
		t := time.NewTimer(wait)
		select {
		case <-wctx.Done():
			t.Stop()
			if ctx.Err() != nil {
				return nil, 0, 0, ctx.Err()
			}
			return nil, 0, 0, errEdgeChunkVanished
		case <-t.C:
		}
		wait = min(wait*2, waitMax)
	}
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
	if req.Outcome == edgeOutcomeChecksumMismatch {
		// The host decoded the aws-chunked body and its trailer is not the
		// body's (D-EDGE-31): the client's error, everything given back.
		return fail(ErrXAmzContentChecksumMismatch)
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
		if edgeChunkRetryable(err) {
			rec.Header().Set("Retry-After", "5")
			WriteS3ErrorWithContext(rec, ErrServiceUnavailable, path, reqID,
				WithSuggestion("A chunk could not be settled before the commit ("+err.Error()+"); retry the upload."))
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
			ChunkIndex: i, ChunkOffset: offset, PlaintextHash: cr.Keyed, DedupScope: cr.scope(row.TenantID),
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
			if _, decErr := s.gci.DecrementRef(compCtx, cr.scope(row.TenantID), cr.Keyed); decErr != nil {
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
		zap.Int64("size", total), zap.String("host", row.Host))
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
