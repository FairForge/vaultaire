package api

// RS k+m at a Go data-plane host (WP-EDGE-4 PR B, Stage 1a Prompt 48; plan
// 40.10 slice 3), built and proven in the lab, DARK (flag edge_rs, default
// OFF; prod has one fast leg). The plan's second-copy policy: an object of
// at least 8 MiB is K data + M parity shards across backends — data shards
// on the fast legs first, no vendor holding more than M shards, a read
// touching the K data shards only. The SPEED bench (2026-10-27) put RS in a
// Worker at 26–29 MB/s per stream, so only a host that says it stripes
// (dataplane.FeatureRS: a node, host=slc) is answered shards; the Worker
// keeps the mirror.
//
// The object: a head row on the `rs` sentinel backend whose storage_key is
// its address, and one blob_copies row per shard (kind 'shard', its index,
// leg, ETag and the stripe geometry); shard i's blob is
// <address>.s<i> on its leg. Every surface reads the shard rows:
//   - GET (origin): handleRSGet rebuilds from the data shards (internal/ec),
//     from any K when a leg does not answer — never wrong bytes;
//   - DELETE / an overwrite: no backend call; the shard rows go stale and the
//     blob_copies job deletes each shard on its leg (blobNamedBy names the
//     address while the head row does);
//   - the erasure sweep walks every leg's tenant prefix (the shards are
//     there); routing_truth samples the shards as copies.
// Not taken: a versioned bucket (a version row would name the sentinel),
// an aws-chunked body, a part, the codec, a sealed write — the mirror or
// the origin as before.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"

	"go.uber.org/zap"

	"github.com/FairForge/vaultaire/internal/common"
	"github.com/FairForge/vaultaire/internal/dataplane"
	"github.com/FairForge/vaultaire/internal/ec"
	"github.com/FairForge/vaultaire/internal/engine"
)

const (
	// rsBackendSentinel is an RS object's head-row backend: its bytes are
	// its shards (blob_copies kind 'shard').
	rsBackendSentinel = "rs"
	// edgeRSK, edgeRSM, edgeRSStripe: 4+2 over 8 MiB stripes (2 MiB pieces)
	// — 1.5×, the plan's shape once a third fast leg exists.
	edgeRSK      = 4
	edgeRSM      = 2
	edgeRSStripe = 2 << 20
	// edgeRSMin: under 8 MiB a shard costs more in requests than it saves —
	// the mirror.
	edgeRSMin = 8 << 20
	// flagEdgeRS stripes a tenant's edge writes K+M (default OFF, per
	// tenant; dark on prod).
	flagEdgeRS = "edge_rs"
)

// edgeRSAttrs is an RS admit's geometry and each shard's leg.
type edgeRSAttrs struct {
	K          int      `json:"k"`
	M          int      `json:"m"`
	Stripe     int      `json:"stripe"`
	ShardBytes int64    `json:"shard_bytes"`
	Legs       []string `json:"legs"`
}

// rsShardLegs places k+m shards on legs round robin, data shards first
// (legs in preference order: the placement's backend, then MIRROR_LEGS);
// nil when a leg would hold more than m (fewer than ⌈(k+m)/m⌉ legs).
func rsShardLegs(legs []string, k, m int) []string {
	n := len(legs)
	if n == 0 || (k+m+n-1)/n > m {
		return nil
	}
	out := make([]string, k+m)
	for j := range out {
		out[j] = legs[j%n]
	}
	return out
}

// rsShardKey is shard i's blob address.
func rsShardKey(addr string, i int) string { return addr + ".s" + strconv.Itoa(i) }

// edgeRSGrantFor is the RS answer of a whole PUT, nil when it is not one
// (see the file comment).
func (s *Server) edgeRSGrantFor(ctx context.Context, req edgeAdmitRequest, tenantID, bucket, placement, container, addr string, size int64) (*dataplane.RSGrant, *edgeRSAttrs) {
	if s.flags == nil || !s.flags.Enabled(flagEdgeRS, tenantID) || size < edgeRSMin || !req.Has(dataplane.FeatureRS) {
		return nil, nil
	}
	if vs := getBucketVersioningStatus(ctx, s.db, tenantID, bucket); vs == "Enabled" || vs == "Suspended" {
		return nil, nil
	}
	candidates := []string{placement}
	for _, leg := range s.mirrorLegs {
		if leg == placement {
			continue
		}
		if _, err := s.edgeWriterFor(leg); err == nil {
			candidates = append(candidates, leg)
		}
	}
	legs := rsShardLegs(candidates, edgeRSK, edgeRSM)
	if legs == nil {
		return nil, nil
	}
	l := ec.Layout{K: edgeRSK, M: edgeRSM, Stripe: edgeRSStripe, Size: size}
	g := &dataplane.RSGrant{K: edgeRSK, M: edgeRSM, Stripe: edgeRSStripe, ShardBytes: l.ShardBytes()}
	for i, leg := range legs {
		ew, err := s.edgeWriterFor(leg)
		if err != nil {
			return nil, nil
		}
		u, signed, err := ew.PresignPut(ctx, container, rsShardKey(addr, i), engine.EdgePresign{ContentLength: g.ShardBytes, TTL: edgeAdmitTTL})
		if err != nil || edgePresignSigns(u, "content-length") != nil {
			s.logger.Warn("edge admit: a shard link could not be made — not RS", zap.Error(err), zap.String("leg", leg))
			return nil, nil
		}
		hdr := map[string]string{"content-length": strconv.FormatInt(g.ShardBytes, 10)}
		for _, name := range signed {
			if name != "host" && name != "content-length" {
				return nil, nil
			}
		}
		g.Shards = append(g.Shards, dataplane.ShardLink{Index: i, Leg: leg, URL: u, SendHeaders: hdr})
	}
	return g, &edgeRSAttrs{K: g.K, M: g.M, Stripe: g.Stripe, ShardBytes: g.ShardBytes, Legs: legs}
}

// settleEdgeRS is an RS write's commit: every shard landed, the head row on
// the sentinel and the shard rows in ONE transaction (the flip's).
func (s *Server) settleEdgeRS(ctx context.Context, row edgeAdmitRow, req edgeCommitRequest, rec *relayRecorder, reqID string) (string, int64) {
	rs := row.Attrs.RS
	path := "/" + row.Bucket + "/" + row.Key
	cctx := common.WithTenantID(context.WithoutCancel(ctx), row.TenantID)
	fail := func(code string) (string, int64) {
		s.releaseEdgeAdmit(ctx, row)
		for i, leg := range rs.Legs {
			retireOrphanWrite(cctx, s.db, s.log(), row.TenantID, row.Bucket, rsShardKey(row.StorageKey, i), leg)
		}
		if code == ErrServiceUnavailable {
			rec.Header().Set("Retry-After", "30")
		}
		WriteS3Error(rec, code, path, reqID)
		s.recordEdgeAccess(row, rec.status, 0, 0)
		return "failed", 0
	}
	etag := strings.ToLower(req.BodyMD5)
	if req.Outcome != edgeOutcomeOK || len(req.ShardETags) != len(rs.Legs) || len(etag) != 32 || !isHex(etag) {
		return fail(ErrServiceUnavailable)
	}
	for _, e := range req.ShardETags {
		if strings.Trim(e, `"`) == "" {
			return fail(ErrServiceUnavailable)
		}
	}
	// RP76-5: the host's word is checked — every shard HEADed on its leg,
	// at the shard's size and the ETag the host reported.
	if err := s.headEdgeRSShards(cctx, row, req.ShardETags); err != nil {
		s.logger.Warn("edge commit: an RS shard is not on its leg as the host said — the write fails", zap.Error(err),
			zap.String("admit_id", row.ID), zap.String("host", row.Host))
		return fail(ErrServiceUnavailable)
	}
	var bill *writeBill
	if s.quotaManager != nil {
		bill = &writeBill{Floor: row.ReservedFloor, Reserved: row.ReservedBytes, FloorReserved: row.FloorReserved, Credited: row.Credited}
	}
	attrs := row.Attrs.Attrs
	if attrs.ContentType == "" {
		attrs.ContentType = row.ContentType
	}
	_, dbErr := commitBlobWrite(cctx, s.db, blobWrite{TenantID: row.TenantID, Bucket: row.Bucket, Key: row.Key, Bill: bill,
		Upsert: func(tx *sql.Tx) error {
			if err := upsertWholeObjectHeadRow(cctx, tx, row.TenantID, row.Bucket, row.Key, row.StorageKey, row.ContentLength, etag,
				rsBackendSentinel, row.Floor, attrs, ""); err != nil {
				return err
			}
			for i, leg := range rs.Legs {
				if _, err := tx.ExecContext(cctx, `
					INSERT INTO blob_copies (tenant_id, bucket, object_key, storage_key, source_backend, leg, kind, shard_index, state, etag, size, rs_k, rs_m, rs_stripe)
					VALUES ($1, $2, $3, $4, $5, $6, 'shard', $7, 'ready', $8, $9, $10, $11, $12)
					ON CONFLICT (tenant_id, storage_key, leg, shard_index) DO UPDATE SET state = 'ready', etag = EXCLUDED.etag, updated_at = NOW()`,
					row.TenantID, row.Bucket, row.Key, row.StorageKey, rsBackendSentinel, leg, i,
					strings.ToLower(strings.Trim(req.ShardETags[i], `"`)), rs.ShardBytes, rs.K, rs.M, rs.Stripe); err != nil {
					return fmt.Errorf("shard row %d: %w", i, err)
				}
			}
			return nil
		}})
	if dbErr != nil {
		s.logger.Error("edge commit: an RS object's rows were not written", zap.Error(dbErr), zap.String("admit_id", row.ID))
		return fail(ErrInternalError)
	}
	applyObjectLockOnPut(cctx, s.db, row.TenantID, row.Bucket, row.Key, &http.Request{Header: http.Header{}})
	rec.Header().Set("ETag", `"`+etag+`"`)
	rec.Header().Set("x-amz-request-id", reqID)
	rec.WriteHeader(http.StatusOK)
	s.recordEdgeAccess(row, http.StatusOK, row.ContentLength, 0)
	s.logger.Info("artifact stored", zap.String("tenant_id", row.TenantID), zap.String("s3.bucket", row.Bucket), zap.String("s3.object", row.Key),
		zap.String("backend", rsBackendSentinel), zap.Int("k", rs.K), zap.Int("m", rs.M), zap.Int64("size", row.ContentLength), zap.String("host", row.Host))
	return "committed", row.ContentLength
}

// headEdgeRSShards HEADs each shard of an RS write on its leg, all at once:
// a shard that is absent, of another size than the stripe geometry's, or
// whose ETag is not the one the host reported fails the commit (RP76-5).
func (s *Server) headEdgeRSShards(ctx context.Context, row edgeAdmitRow, etags []string) error {
	rs := row.Attrs.RS
	errs := make([]error, len(rs.Legs))
	var wg sync.WaitGroup
	for i, leg := range rs.Legs {
		wg.Add(1)
		go func(i int, leg string) {
			defer wg.Done()
			drv, ok := s.engine.GetDriver(leg)
			if !ok {
				errs[i] = fmt.Errorf("shard %d: leg %s is not registered", i, leg)
				return
			}
			h, ok := drv.(engine.ObjectHeader)
			if !ok {
				errs[i] = fmt.Errorf("shard %d: leg %s cannot be asked for an object's size", i, leg)
				return
			}
			hctx, cancel := context.WithTimeout(ctx, edgeBackendCallTimeout)
			defer cancel()
			info, err := h.HeadObject(hctx, row.TenantID+"_"+row.Bucket, rsShardKey(row.StorageKey, i))
			switch {
			case err != nil:
				errs[i] = fmt.Errorf("shard %d on %s: %w", i, leg, err)
			case info.Size != rs.ShardBytes:
				errs[i] = fmt.Errorf("shard %d on %s is %d bytes, the stripes make %d", i, leg, info.Size, rs.ShardBytes)
			case !strings.EqualFold(strings.Trim(info.ETag, `"`), strings.Trim(etags[i], `"`)):
				errs[i] = fmt.Errorf("shard %d on %s has ETag %s, the host said %s", i, leg, info.ETag, etags[i])
			}
		}(i, leg)
	}
	wg.Wait()
	return errors.Join(errs...)
}

// rsShard is one shard row of an object.
type rsShard struct {
	index        int
	leg, etag    string
	k, m, stripe int
}

// rsShardsOf reads an RS object's shard rows (index order).
func rsShardsOf(ctx context.Context, db *sql.DB, tenantID, addr string) ([]rsShard, error) {
	rows, err := db.QueryContext(ctx, `SELECT shard_index, leg, etag, rs_k, rs_m, rs_stripe FROM blob_copies
		WHERE tenant_id = $1 AND storage_key = $2 AND kind = 'shard' AND state = 'ready' ORDER BY shard_index`, tenantID, addr)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []rsShard
	for rows.Next() {
		var s rsShard
		if err := rows.Scan(&s.index, &s.leg, &s.etag, &s.k, &s.m, &s.stripe); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// lazyShard opens a shard's GET at its first Read: a parity shard is asked
// only when a data shard fails.
type lazyShard struct {
	open func() (io.ReadCloser, error)
	rc   io.ReadCloser
}

func (l *lazyShard) Read(p []byte) (int, error) {
	if l.rc == nil {
		rc, err := l.open()
		if err != nil {
			return 0, err
		}
		l.rc = rc
	}
	return l.rc.Read(p)
}

func (l *lazyShard) Close() error {
	if l.rc != nil {
		return l.rc.Close()
	}
	return nil
}

// errRSUnreadable: fewer than K shards of an RS object can be read.
var errRSUnreadable = errors.New("rs: the object's shards cannot be read")

// openRSObject is an RS object's bytes rebuilt from its shards.
func (a *S3ToEngine) openRSObject(ctx context.Context, tenantID, bucket, addr string, size int64) (io.ReadCloser, error) {
	shards, err := rsShardsOf(ctx, a.db, tenantID, addr)
	if err != nil {
		return nil, err
	}
	if len(shards) == 0 {
		return nil, errRSUnreadable
	}
	g := shards[0]
	l := ec.Layout{K: g.k, M: g.m, Stripe: g.stripe, Size: size}
	readers := make([]io.Reader, l.K+l.M)
	var closers []io.Closer
	one, ok := a.engine.(singleBackendEngine)
	if !ok {
		return nil, errRSUnreadable
	}
	tctx := common.WithTenantID(ctx, tenantID)
	container := tenantID + "_" + bucket
	for _, sh := range shards {
		if sh.index < 0 || sh.index >= len(readers) {
			continue
		}
		leg, key := sh.leg, rsShardKey(addr, sh.index)
		ls := &lazyShard{open: func() (io.ReadCloser, error) { return one.GetOn(tctx, leg, container, key) }}
		readers[sh.index] = ls
		closers = append(closers, ls)
	}
	r, err := ec.NewReader(l, readers)
	if err != nil {
		return nil, errRSUnreadable
	}
	return &rsObjectReader{r: r, closers: closers}, nil
}

type rsObjectReader struct {
	r       io.Reader
	closers []io.Closer
}

func (o *rsObjectReader) Read(p []byte) (int, error) { return o.r.Read(p) }

func (o *rsObjectReader) Close() error {
	for _, c := range o.closers {
		_ = c.Close()
	}
	return nil
}

// handleRSGet serves an RS object (whole or one range): the first bytes are
// rebuilt before the status line, so a read with fewer than K shards is a
// clean 503; a whole read is checked against the row's MD5 as it streams.
func (a *S3ToEngine) handleRSGet(w http.ResponseWriter, r *http.Request, tenantID, bucket, addr, etag string, size int64, oh objectHeaders) {
	rc, err := a.openRSObject(r.Context(), tenantID, bucket, addr, size)
	if err != nil {
		a.rsReadFailed(w, r, bucket, addr, err)
		return
	}
	defer func() { _ = rc.Close() }()
	var rng *httpRange
	if rh := r.Header.Get("Range"); rh != "" && !errors.Is(rangeParseErr(rh, size), errMultiRange) {
		parsed, perr := parseRangeHeader(rh, size)
		if perr != nil {
			writeRangeNotSatisfiable(w, size)
			return
		}
		rng = parsed
	}
	var src io.Reader = rc
	if rng != nil {
		if _, err := io.CopyN(io.Discard, rc, rng.start); err != nil {
			a.rsReadFailed(w, r, bucket, addr, err)
			return
		}
		src = io.LimitReader(rc, rng.length)
	} else if ir, armed := newIntegrityReader(rc, etag, size); armed {
		src = ir
	}
	// The first byte before the status line: fewer than K shards is a 503.
	first := make([]byte, 1)
	n, ferr := io.ReadFull(src, first)
	if ferr != nil && size > 0 && (rng == nil || rng.length > 0) {
		a.rsReadFailed(w, r, bucket, addr, ferr)
		return
	}
	w.Header().Set("x-amz-request-id", generateRequestID())
	if rng != nil {
		oh.write(w, headersRange, rng)
		w.WriteHeader(http.StatusPartialContent)
	} else {
		oh.write(w, headersWhole, nil)
	}
	_, _ = w.Write(first[:n])
	if _, err := io.Copy(w, src); err != nil {
		a.logger.Error("rs get: the body was cut", zap.Error(err), zap.String("bucket", bucket), zap.String("storage_key", addr))
		cutUnlessClientGone(r.Context())
	}
}

func (a *S3ToEngine) rsReadFailed(w http.ResponseWriter, r *http.Request, bucket, addr string, err error) {
	a.logger.Error("rs get: fewer than k shards could be read", zap.Error(err), zap.String("bucket", bucket), zap.String("storage_key", addr))
	w.Header().Set("Retry-After", "30")
	WriteS3Error(w, ErrServiceUnavailable, r.URL.Path, generateRequestID())
}

// staleShardsTx makes an RS object's shards the blob_copies job's to
// delete (DELETE, an overwrite — the object's address is named by nothing
// any more).
func staleShardsTx(ctx context.Context, q execer, tenantID, addr string) error {
	_, err := q.ExecContext(ctx, `UPDATE blob_copies SET state = 'stale', updated_at = NOW()
		WHERE tenant_id = $1 AND storage_key = $2 AND kind = 'shard' AND state <> 'stale'`, tenantID, addr)
	return err
}
