package api

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"sync"
	"time"

	"github.com/FairForge/vaultaire/internal/common"
	"github.com/FairForge/vaultaire/internal/crypto"
	"github.com/FairForge/vaultaire/internal/engine"
	"github.com/FairForge/vaultaire/internal/parfetch"
	"github.com/prometheus/client_golang/prometheus"
	"go.uber.org/zap"
)

// Large single-stream GETs (download diagnosis 2026-10-09). Two paths read
// one client stream as many backend requests through internal/parfetch —
// a bounded window, hedged fetches, ordered write-out:
//
//   - chunked objects: one part = one content-defined chunk (~2 MiB). Always
//     on. CHUNK_GET_WINDOW_BYTES (64 MiB) of chunks are read ahead, at most
//     CHUNK_GET_PREFETCH (32) of them; a chunk not done after
//     CHUNK_GET_HEDGE_AFTER (400 ms, or 3× the stream's running chunk time
//     if longer) is requested again and the first verified copy wins.
//     12–36 MB/s on iDrive before; the same algorithm measured 163–183.
//   - whole objects ≥ LARGE_GET_PARALLEL_MIN_BYTES (64 MiB) on a backend
//     whose ranged reads are real and cheap (engine.VersionedRangeGetter —
//     the fixed-bucket S3 driver: idrive, idrive-<region>, wasabi; lyve;
//     the Swift driver: blomp), behind
//     the `parallel_get` flag (default OFF, per tenant first):
//     LARGE_GET_PARALLEL_PARTS (8) ranges of LARGE_GET_PART_BYTES (16 MiB)
//     in flight, a range not done after LARGE_GET_HEDGE_AFTER (1 s) hedged.
//     One iDrive connection swings 37–241 MB/s; 8 × 16 MiB + hedge measured
//     223–309.
//
// Both draw on one process-wide budget, LARGE_GET_PARALLEL_BUDGET_BYTES
// (2 GiB): a chunked GET always keeps chunkGetFreeParts chunks (the depth it
// had before) and degrades to them when the budget is spent; a whole-object
// GET that cannot get its window takes the single stream instead.

const (
	defaultChunkGetWindowBytes = 64 << 20
	defaultChunkGetHedgeAfter  = 400 * time.Millisecond
	defaultLargeGetPartBytes   = 16 << 20
	defaultLargeGetParts       = 8
	defaultLargeGetMinBytes    = 64 << 20
	defaultLargeGetHedgeAfter  = time.Second
	defaultLargeGetBudgetBytes = 2 << 30

	// chunkGetFreeParts is the read-ahead a chunked GET keeps whatever the
	// shared budget says: the old CHUNK_GET_PREFETCH default.
	chunkGetFreeParts = 4
)

// flagParallelGet gates the parallel ranged GET of whole objects per tenant
// (`*` row = everyone). Default OFF.
const flagParallelGet = "parallel_get"

// largeGetConfig is the env-tuned shape of both paths.
type largeGetConfig struct {
	chunkWindowBytes int64
	chunkHedgeAfter  time.Duration // 0 = no hedging on the chunked path
	partBytes        int64
	parallelParts    int
	minBytes         int64
	hedgeAfter       time.Duration // 0 = no hedging on the ranged path
	budget           *parfetch.Budget
}

func defaultLargeGetConfig() largeGetConfig {
	return largeGetConfig{
		chunkWindowBytes: defaultChunkGetWindowBytes,
		chunkHedgeAfter:  defaultChunkGetHedgeAfter,
		partBytes:        defaultLargeGetPartBytes,
		parallelParts:    defaultLargeGetParts,
		minBytes:         defaultLargeGetMinBytes,
		hedgeAfter:       defaultLargeGetHedgeAfter,
	}
}

// largeGetConfigFromEnv reads the knobs. A rejected value is logged at Warn
// and the default kept (the R13-19 convention). The budget is created here,
// once per process.
func largeGetConfigFromEnv(getenv func(string) string, logger *zap.Logger) largeGetConfig {
	cfg := defaultLargeGetConfig()
	bytesKnob := func(name string, min int64, dst *int64) {
		if v := getenv(name); v != "" {
			if n, err := strconv.ParseInt(v, 10, 64); err == nil && n >= min {
				*dst = n
			} else {
				logger.Warn("invalid "+name+", keeping default",
					zap.String("value", v), zap.Int64("min", min), zap.Int64("default", *dst))
			}
		}
	}
	durKnob := func(name string, dst *time.Duration) {
		if v := getenv(name); v != "" {
			if d, err := time.ParseDuration(v); err == nil && d >= 0 && d <= time.Minute {
				*dst = d
			} else {
				logger.Warn("invalid "+name+" (a Go duration, 0 = off, at most 1m), keeping default",
					zap.String("value", v), zap.Duration("default", *dst))
			}
		}
	}
	bytesKnob("CHUNK_GET_WINDOW_BYTES", 1<<20, &cfg.chunkWindowBytes)
	durKnob("CHUNK_GET_HEDGE_AFTER", &cfg.chunkHedgeAfter)
	bytesKnob("LARGE_GET_PART_BYTES", 1<<20, &cfg.partBytes)
	bytesKnob("LARGE_GET_PARALLEL_MIN_BYTES", 1<<20, &cfg.minBytes)
	durKnob("LARGE_GET_HEDGE_AFTER", &cfg.hedgeAfter)
	if v := getenv("LARGE_GET_PARALLEL_PARTS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 2 && n <= 64 {
			cfg.parallelParts = n
		} else {
			logger.Warn("invalid LARGE_GET_PARALLEL_PARTS (2..64), keeping default", zap.String("value", v))
		}
	}
	budget := int64(defaultLargeGetBudgetBytes)
	bytesKnob("LARGE_GET_PARALLEL_BUDGET_BYTES", 64<<20, &budget)
	cfg.budget = parfetch.NewBudget(budget)
	return cfg
}

var (
	largeGetHedges = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "vaultaire_large_get_hedges_total",
		Help: "Second requests for a part of a large GET: path chunked|ranged, reason slow (not done after the hedge delay) | error (the first attempt failed).",
	}, []string{"path", "reason"})
	largeGetHedgesWon = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "vaultaire_large_get_hedges_won_total",
		Help: "Parts of a large GET delivered by their second request.",
	}, []string{"path"})
	largeGetInflight = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "vaultaire_large_get_inflight_bytes",
		Help: "Bytes held by large-GET read-ahead windows (in flight + fetched, not yet written).",
	}, []string{"path"})
	largeGetFallbacks = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "vaultaire_large_get_fallbacks_total",
		Help: "Parallel ranged GETs that took the single stream instead: budget (the process budget could not spare a window) | first_part (the first range failed before any byte was sent).",
	}, []string{"reason"})
	largeGetStreams = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "vaultaire_large_get_streams_total",
		Help: "Large GETs through the parallel reader by path and outcome: ok | error (a part failed mid-stream: the body was cut) | aborted (the client went away).",
	}, []string{"path", "outcome"})
	largeGetThroughput = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "vaultaire_large_get_throughput_bytes_per_second",
		Help:    "Per-stream throughput of complete large GETs (bytes ÷ wall time).",
		Buckets: []float64{8e6, 16e6, 32e6, 64e6, 100e6, 150e6, 200e6, 300e6, 500e6, 1e9},
	}, []string{"path"})
	// chunkedGetTruncated counts chunked GETs whose body was cut after the
	// status line: the connection is reset, never ended cleanly (Stage 1a
	// Part A). reason = fetch (a chunk could not be read) | integrity (its
	// bytes did not hash to the index) | decrypt (it did not decrypt).
	chunkedGetTruncated = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "vaultaire_chunked_get_truncated_total",
		Help: "Chunked GETs cut after the status line was sent, by reason: fetch | integrity | decrypt. The connection is reset so no client can take the short body for the object.",
	}, []string{"reason"})
)

// Reasons of vaultaire_chunked_get_truncated_total.
const (
	truncatedFetch     = "fetch"
	truncatedIntegrity = "integrity"
	truncatedDecrypt   = "decrypt"
)

// chunkedGetRereads counts the chunked GET's second look at a chunk's index
// row after its resolved address answered "not there" (WP-R8-1 follow-up
// A2): moved = the row names another blob now (a re-encrypt swap, a chunk
// move, a re-store) and the read went there; missing = the row is unchanged
// (or gone): a real miss, the body is cut as before.
var chunkedGetRereads = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "vaultaire_chunked_get_rereads_total",
	Help: "Chunked GETs whose resolved chunk address answered not-found and re-read the chunk's index row once: moved (the row named a new blob, the read continued there) | missing (the row is unchanged, gone or could not be read: the miss stands, the body is cut).",
}, []string{"outcome"})

const (
	rereadMoved   = "moved"
	rereadMissing = "missing"
)

func init() {
	chunkedGetRereads.WithLabelValues(rereadMoved)
	chunkedGetRereads.WithLabelValues(rereadMissing)
	for _, p := range []string{"chunked", "ranged", "sealed"} {
		for _, r := range []string{"slow", "error"} {
			largeGetHedges.WithLabelValues(p, r)
		}
		largeGetHedgesWon.WithLabelValues(p)
		largeGetInflight.WithLabelValues(p)
		for _, o := range []string{"ok", "error", "aborted"} {
			largeGetStreams.WithLabelValues(p, o)
		}
	}
	for _, r := range []string{"budget", "first_part"} {
		largeGetFallbacks.WithLabelValues(r)
	}
	for _, r := range []string{truncatedFetch, truncatedIntegrity, truncatedDecrypt} {
		chunkedGetTruncated.WithLabelValues(r)
	}
}

// largeGetCollectors are registered on the server registry.
func largeGetCollectors() []prometheus.Collector {
	return []prometheus.Collector{largeGetHedges, largeGetHedgesWon, largeGetInflight, largeGetFallbacks, largeGetStreams, largeGetThroughput, chunkedGetTruncated, chunkedGetRereads, largeGetParallelReads,
		objectIntegrityFailures, objectIntegrityUnverified}
}

func largeGetHooks(path string) parfetch.Hooks {
	inflight := largeGetInflight.WithLabelValues(path)
	won := largeGetHedgesWon.WithLabelValues(path)
	return parfetch.Hooks{
		Held:     func(d int64) { inflight.Add(float64(d)) },
		Hedged:   func(reason string) { largeGetHedges.WithLabelValues(path, reason).Inc() },
		HedgeWon: won.Inc,
	}
}

// observeLargeGet records one stream's end.
func observeLargeGet(path string, err error, done bool, n int64, start time.Time) {
	switch {
	case done:
		largeGetStreams.WithLabelValues(path, "ok").Inc()
		if s := time.Since(start).Seconds(); s > 0 && n > 0 {
			largeGetThroughput.WithLabelValues(path).Observe(float64(n) / s)
		}
	case err != nil && !errors.Is(err, context.Canceled):
		largeGetStreams.WithLabelValues(path, "error").Inc()
	default:
		largeGetStreams.WithLabelValues(path, "aborted").Inc()
	}
}

// resolveChunkDescs is the chunked GET's preflight: every chunk's location
// from the index in ONE query per dedup scope (it was one query per chunk —
// ~0.3 s before the first byte of a 1 GiB object). A missing entry makes
// the manifest unresolvable.
func (a *S3ToEngine) resolveChunkDescs(ctx context.Context, refs []crypto.TenantChunkRef) ([]chunkDesc, error) {
	byScope := map[string][]string{}
	scopeOf := make([]string, len(refs))
	for i, ref := range refs {
		scope := ref.DedupScope
		if scope == "" {
			scope = crypto.GlobalDedupScope
		}
		scopeOf[i] = scope
		byScope[scope] = append(byScope[scope], ref.PlaintextHash)
	}
	found := make(map[string]map[string]*crypto.ChunkLookupResult, len(byScope))
	for scope, hashes := range byScope {
		res, err := a.gci.LookupChunks(ctx, scope, hashes)
		if err != nil {
			return nil, fmt.Errorf("lookup %d chunks: %w", len(hashes), err)
		}
		found[scope] = res
	}
	descs := make([]chunkDesc, len(refs))
	for i, ref := range refs {
		lookup := found[scopeOf[i]][ref.PlaintextHash]
		if lookup == nil || !lookup.Exists || lookup.Entry == nil {
			return nil, fmt.Errorf("chunk %s missing from index", shortHash(ref.PlaintextHash))
		}
		storageKey := lookup.Entry.StorageKey
		if storageKey == "" {
			storageKey = "_chunks/" + ref.PlaintextHash
		}
		// The GCI row's ciphertext hash is authoritative (it was computed
		// from the blob actually stored); per-ref copies are a fallback for
		// rows written before the hash lived on the index.
		var ctHash string
		if lookup.Entry.CiphertextHash != nil {
			ctHash = *lookup.Entry.CiphertextHash
		} else if ref.CiphertextHash != nil {
			ctHash = *ref.CiphertextHash
		}
		// The key version likewise (WP-R8-1): the row's is the blob's (the
		// re-encrypt job changes it with the blob); the ref's copy only
		// stands in for rows sealed before 084 carried one.
		keyVersion := ref.EncryptionKeyVersion
		if lookup.Entry.EncryptionKeyVersion != nil {
			keyVersion = *lookup.Entry.EncryptionKeyVersion
		}
		descs[i] = chunkDesc{
			scope:          scopeOf[i],
			storageKey:     storageKey,
			backendID:      lookup.Entry.BackendID,
			plaintextHash:  ref.PlaintextHash,
			offset:         ref.ChunkOffset,
			size:           lookup.Entry.SizeBytes,
			compressed:     lookup.Entry.CompressionAlgo != nil,
			encrypted:      lookup.Entry.Encrypted,
			ciphertextHash: ctHash,
			keyVersion:     keyVersion,
		}
	}
	return descs, nil
}

// noHedgeBackend reports whether any of these backends declares that its
// reads are never requested twice (engine.NoHedgeDeclarer — the `sia`
// driver: a hedge spends the Sia account's read budget twice and starves the
// read, SIA-BENCH-3/5; the driver resumes a cut body itself). An engine that
// cannot name its drivers keeps the hedge.
func (a *S3ToEngine) noHedgeBackend(backends ...string) bool {
	ge, ok := a.engine.(interface {
		GetDriver(string) (engine.Driver, bool)
	})
	if !ok {
		return false
	}
	for _, b := range backends {
		if d, ok := ge.GetDriver(b); ok && engine.IsNoHedge(d) {
			return true
		}
	}
	return false
}

// chunkStreamConfig is the parfetch shape of one chunked GET of plan: no
// hedge (and no retry of a failed chunk) when a chunk of it is on a backend
// that is never hedged.
func (a *S3ToEngine) chunkStreamConfig(plan []chunkSlice) parfetch.Config {
	hedgeAfter := a.largeGet.chunkHedgeAfter
	if hedgeAfter > 0 {
		seen := map[string]bool{}
		for _, p := range plan {
			if b := p.desc.backendID; b != "" && !seen[b] {
				seen[b] = true
				if a.noHedgeBackend(b) {
					hedgeAfter = 0
					break
				}
			}
		}
	}
	maxParts := a.chunkGetPrefetch
	if maxParts < 1 {
		maxParts = 1
	}
	free := chunkGetFreeParts
	if free > maxParts {
		free = maxParts
	}
	return parfetch.Config{
		Window:     a.largeGet.chunkWindowBytes,
		MaxParts:   maxParts,
		HedgeAfter: hedgeAfter,
		Budget:     a.largeGet.budget,
		FreeParts:  free,
		Hooks:      largeGetHooks("chunked"),
	}
}

// errRangeIdentity: a range came from a different object than the others
// (overwritten mid-read) or the backend ignored the range. The body is cut
// rather than spliced from two objects.
var errRangeIdentity = errors.New("parallel get: range identity mismatch")

// largeGetParallelReads counts the parallel ranged reads started, by the
// backend they read from (registered drivers only) and path (ranged = a
// plaintext whole object, sealed = an SSE-S3 / SSE-C 0x03 blob by segment).
// It answers "did the parallel reader take this backend?" — the 2026-10-14
// bench could not tell a pinned bucket's single stream from a parallel one.
var largeGetParallelReads = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "vaultaire_large_get_parallel_reads_total",
	Help: "Parallel ranged GETs started (the first range read), by backend and path: ranged (a plaintext whole object) | sealed (an SSE-S3 or SSE-C blob, by segment).",
}, []string{"backend", "path"})

// parallelGetReader is the client stream of a parallel ranged GET. Its
// Close settles the read as ONE outcome on the backend's breaker.
type parallelGetReader struct {
	*parfetch.Reader
	eng     *engine.CoreEngine
	backend string
	path    string // ranged | sealed
	start   time.Time
	length  int64 // the bytes the stream yields: all of them is done
	n       int64
	once    sync.Once
}

func (r *parallelGetReader) Read(p []byte) (int, error) {
	n, err := r.Reader.Read(p)
	r.n += int64(n)
	return n, err
}

func (r *parallelGetReader) WriteTo(w io.Writer) (int64, error) {
	n, err := r.Reader.WriteTo(w)
	r.n += n
	return n, err
}

func (r *parallelGetReader) Close() error {
	r.once.Do(func() {
		err := r.Err()
		// A Range GET is written with io.CopyN, which stops after exactly
		// length bytes without the read that would see EOF: every byte
		// delivered is done too (2b.4 E2.2 — it was counted "aborted" and
		// recorded no breaker success).
		done := r.Done() || (err == nil && r.n == r.length)
		_ = r.Reader.Close()
		switch {
		case done:
			r.eng.RecordReadOutcome(r.backend, nil)
		// An object that changed mid-read, or stored bytes that do not open,
		// say nothing about the backend's health.
		case err != nil && !errors.Is(err, errRangeIdentity) && !errors.Is(err, errSealedUnreadable):
			r.eng.RecordReadOutcome(r.backend, err)
		}
		observeLargeGet(r.path, err, done, r.n, r.start)
	})
	return nil
}

// parallelSource is the gate both parallel paths share: the length is
// worth it, the tenant's flag is on, the row's backend has real ranged
// reads and a closed breaker (engine.ParallelRangeSource — any registered
// driver: the primary, an idrive-<region> driver of a pinned bucket, lyve,
// blomp),
// and the process budget can spare two parts. ok=false: the single stream.
func (a *S3ToEngine) parallelSource(ctx context.Context, tenantID, container, blob string, length int64) (string, engine.VersionedRangeGetter, *engine.CoreEngine, bool) {
	cfg := a.largeGet
	if length < cfg.minBytes || cfg.partBytes <= 0 || cfg.parallelParts < 2 {
		return "", nil, nil, false
	}
	if a.flags == nil || !a.flags.Enabled(flagParallelGet, tenantID) {
		return "", nil, nil, false
	}
	ce, ok := a.engine.(*engine.CoreEngine)
	if !ok {
		return "", nil, nil, false
	}
	backend, rg, ok := ce.ParallelRangeSource(ctx, container, blob)
	if !ok {
		return "", nil, nil, false
	}
	if cfg.budget.Available() < 2*cfg.partBytes {
		largeGetFallbacks.WithLabelValues("budget").Inc()
		return "", nil, nil, false
	}
	return backend, rg, ce, true
}

// startParallel runs the parts and reads the first before it returns, so
// any failure up to there is a clean fallback — nothing has been sent.
func (a *S3ToEngine) startParallel(ctx context.Context, path string, ce *engine.CoreEngine, backend, container, blob string,
	sizes []int64, length int64, fetch func(context.Context, int) ([]byte, error)) io.ReadCloser {
	cfg := a.largeGet
	// The window holds parallelParts parts; a sealed part is whole segments,
	// so it can be larger than LARGE_GET_PART_BYTES.
	part := cfg.partBytes
	for _, sz := range sizes {
		part = max(part, sz)
	}
	// A backend that is never hedged (the `sia` driver) gets one request per
	// range: no hedge, no second try of a failed range.
	hedgeAfter := cfg.hedgeAfter
	if d, ok := ce.GetDriver(backend); ok && engine.IsNoHedge(d) {
		hedgeAfter = 0
	}
	start := time.Now()
	stream := parfetch.Start(ctx, parfetch.Config{
		Window:     int64(cfg.parallelParts) * part,
		MaxParts:   cfg.parallelParts,
		HedgeAfter: hedgeAfter,
		Budget:     cfg.budget,
		FreeParts:  1,
		Hooks:      largeGetHooks(path),
	}, sizes, fetch)
	first, err := stream.Next()
	if err != nil {
		stream.Close()
		if ctx.Err() == nil {
			largeGetFallbacks.WithLabelValues("first_part").Inc()
			a.logger.Warn("parallel get: first range failed, taking the single stream",
				zap.String("backend", backend), zap.String("path", path), zap.String("container", container),
				zap.String("blob", blob), zap.Error(err))
		}
		return nil
	}
	largeGetParallelReads.WithLabelValues(backend, path).Inc()
	common.SetBackendUsed(ctx, backend)
	return &parallelGetReader{Reader: parfetch.NewReader(stream, first), eng: ce, backend: backend, path: path, start: start, length: length}
}

// openParallelGet opens bytes [offset, offset+length) of a whole object as
// parallel ranged reads of its blob (the row's address, WP-R2-1), or
// returns nil: the caller then takes the single stream. objectSize is the
// recorded size every range must report.
func (a *S3ToEngine) openParallelGet(ctx context.Context, tenantID, container, blob string, objectSize, offset, length int64) io.ReadCloser {
	backend, rg, ce, ok := a.parallelSource(ctx, tenantID, container, blob, length)
	if !ok {
		return nil
	}
	cfg := a.largeGet
	n := int((length + cfg.partBytes - 1) / cfg.partBytes)
	sizes := make([]int64, n)
	for i := range sizes {
		sizes[i] = cfg.partBytes
	}
	sizes[n-1] = length - int64(n-1)*cfg.partBytes

	id := &rangeIdentity{size: objectSize}
	fetch := func(fctx context.Context, i int) ([]byte, error) {
		off := offset + int64(i)*cfg.partBytes
		want := sizes[i]
		rc, info, err := rg.GetRangeInfo(fctx, container, blob, off, want)
		if err != nil {
			return nil, fmt.Errorf("range %d (%d+%d) on %s: %w", i, off, want, backend, err)
		}
		defer func() { _ = rc.Close() }()
		if err := id.check(i, info); err != nil {
			return nil, err
		}
		buf := make([]byte, want)
		if _, err := io.ReadFull(rc, buf); err != nil {
			return nil, fmt.Errorf("range %d (%d+%d) on %s: read: %w", i, off, want, backend, err)
		}
		return buf, nil
	}
	return a.startParallel(ctx, "ranged", ce, backend, container, blob, sizes, length, fetch)
}

// rangeIdentity: every range of one read must come from one object — the
// recorded size and the first range's ETag (a concurrent overwrite cuts the
// body, never splices two objects).
type rangeIdentity struct {
	size int64
	mu   sync.Mutex
	etag string
}

func (id *rangeIdentity) check(i int, info engine.RangeInfo) error {
	if info.Size != id.size {
		return fmt.Errorf("%w: range %d reports object size %d, recorded %d", errRangeIdentity, i, info.Size, id.size)
	}
	id.mu.Lock()
	defer id.mu.Unlock()
	if id.etag == "" {
		id.etag = info.ETag
	}
	if id.etag != info.ETag {
		return fmt.Errorf("%w: range %d etag %q, earlier ranges %q", errRangeIdentity, i, info.ETag, id.etag)
	}
	return nil
}

// openParallelSealed opens plaintext [offset, offset+length) of a sealed
// whole object — an SSE-S3 or SSE-C blob in the streaming format 0x03 — as
// parallel ranged reads aligned to its segments, each part opened as it
// arrives (Prompt 8 Part B): the header is read first (one ranged GET), the
// key derived once, then every part reads the ciphertext span of its
// segments and decrypts it into the window — only plaintext is held, so the
// process budget counts what it holds. Every range reports the stored
// length and one ETag, as on the plaintext path. nil: the caller takes the
// single sealed stream (an old-format blob, a header or first part that
// fails — a wrong SSE-C key among them: the single stream answers its 403).
// Never a vault-floor object (the caller checks), never a chunked one (they
// are not sealed whole), never Geyser (no VersionedRangeGetter).
func (a *S3ToEngine) openParallelSealed(ctx context.Context, op blobOpener, tenantID, container, blob string, size, stored, offset, length int64) io.ReadCloser {
	if !op.streaming() || stored < int64(crypto.MaxStreamHeader) {
		return nil
	}
	backend, rg, ce, ok := a.parallelSource(ctx, tenantID, container, blob, length)
	if !ok {
		return nil
	}
	id := &rangeIdentity{size: stored}
	h, err := readStreamHeaderRange(ctx, rg, container, blob, id)
	if err != nil {
		if !errors.Is(err, errSealedLegacy) && ctx.Err() == nil {
			largeGetFallbacks.WithLabelValues("first_part").Inc()
			a.logger.Warn("parallel get: the sealed header could not be read, taking the single stream",
				zap.String("backend", backend), zap.String("blob", blob), zap.Error(err))
		}
		return nil
	}
	opener, err := op.rangeOpener(ctx, h)
	if err != nil {
		return nil
	}
	cfg := a.largeGet
	seg := int64(h.SegmentSize)
	partSegs := max(cfg.partBytes/seg, 1)
	first, last := offset/seg, (offset+length-1)/seg
	n := int((last-first)/partSegs + 1)
	starts := make([]int64, n)
	sizes := make([]int64, n)
	for i := range sizes {
		s0 := first + int64(i)*partSegs
		s1 := min(s0+partSegs-1, last)
		starts[i] = max(offset, s0*seg)
		sizes[i] = min(offset+length, (s1+1)*seg) - starts[i]
	}
	fetch := func(fctx context.Context, i int) ([]byte, error) {
		plan, err := crypto.PlanStreamRange(h, size, starts[i], sizes[i])
		if err != nil {
			return nil, fmt.Errorf("%w: part %d: %w", errSealedUnreadable, i, err)
		}
		rc, info, err := rg.GetRangeInfo(fctx, container, blob, plan.CipherOffset, plan.CipherLength)
		if err != nil {
			return nil, fmt.Errorf("sealed range %d (%d+%d) on %s: %w", i, plan.CipherOffset, plan.CipherLength, backend, err)
		}
		defer func() { _ = rc.Close() }()
		if err := id.check(i, info); err != nil {
			return nil, err
		}
		buf := make([]byte, sizes[i])
		pt, err := opener.OpenRange(size, plan, io.LimitReader(rc, plan.CipherLength))
		if err == nil {
			_, err = io.ReadFull(pt, buf)
		}
		if err != nil {
			if errors.Is(err, crypto.ErrStreamCorrupt) || errors.Is(err, crypto.ErrSSECKeyMismatch) {
				return nil, fmt.Errorf("%w: part %d: %w", errSealedUnreadable, i, err)
			}
			return nil, fmt.Errorf("sealed range %d on %s: read: %w", i, backend, err)
		}
		return buf, nil
	}
	return a.startParallel(ctx, "sealed", ce, backend, container, blob, sizes, length, fetch)
}

// readStreamHeaderRange reads a 0x03 blob's header with one ranged read
// (errSealedLegacy: not a streaming blob).
func readStreamHeaderRange(ctx context.Context, rg engine.VersionedRangeGetter, container, blob string, id *rangeIdentity) (*crypto.StreamHeader, error) {
	rc, info, err := rg.GetRangeInfo(ctx, container, blob, 0, int64(crypto.MaxStreamHeader))
	if err != nil {
		return nil, fmt.Errorf("sealed header: %w", err)
	}
	defer func() { _ = rc.Close() }()
	if err := id.check(-1, info); err != nil {
		return nil, err
	}
	var hb [crypto.MaxStreamHeader]byte
	k, err := io.ReadFull(rc, hb[:])
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) {
		return nil, fmt.Errorf("sealed header: read: %w", err)
	}
	if k == 0 || !crypto.IsStreamFormat(hb[0]) {
		return nil, errSealedLegacy
	}
	h, err := crypto.ParseStreamHeader(hb[:k])
	if err != nil {
		return nil, fmt.Errorf("%w: %w", errSealedUnreadable, err)
	}
	return h, nil
}
