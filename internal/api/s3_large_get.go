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
//     the fixed-bucket S3 driver: idrive, idrive-<region>, wasabi), behind
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
)

func init() {
	for _, p := range []string{"chunked", "ranged"} {
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
}

// largeGetCollectors are registered on the server registry.
func largeGetCollectors() []prometheus.Collector {
	return []prometheus.Collector{largeGetHedges, largeGetHedgesWon, largeGetInflight, largeGetFallbacks, largeGetStreams, largeGetThroughput}
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
		}
	}
	return descs, nil
}

// chunkStreamConfig is the parfetch shape of one chunked GET.
func (a *S3ToEngine) chunkStreamConfig() parfetch.Config {
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
		HedgeAfter: a.largeGet.chunkHedgeAfter,
		Budget:     a.largeGet.budget,
		FreeParts:  free,
		Hooks:      largeGetHooks("chunked"),
	}
}

// errRangeIdentity: a range came from a different object than the others
// (overwritten mid-read) or the backend ignored the range. The body is cut
// rather than spliced from two objects.
var errRangeIdentity = errors.New("parallel get: range identity mismatch")

// parallelGetReader is the client stream of a parallel ranged GET. Its
// Close settles the read as ONE outcome on the backend's breaker.
type parallelGetReader struct {
	*parfetch.Reader
	eng     *engine.CoreEngine
	backend string
	start   time.Time
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
		done := r.Done()
		_ = r.Reader.Close()
		switch {
		case done:
			r.eng.RecordReadOutcome(r.backend, nil)
		case err != nil && !errors.Is(err, errRangeIdentity):
			r.eng.RecordReadOutcome(r.backend, err)
		}
		observeLargeGet("ranged", err, done, r.n, r.start)
	})
	return nil
}

// openParallelGet opens bytes [offset, offset+length) of a whole object as
// parallel ranged reads, or returns nil: the caller then takes the single
// stream. objectSize is the recorded size every range must report. The
// first range is read before this returns, so any failure up to there is a
// clean fallback — nothing has been sent.
func (a *S3ToEngine) openParallelGet(ctx context.Context, tenantID, container, artifact string, objectSize, offset, length int64) io.ReadCloser {
	cfg := a.largeGet
	if length < cfg.minBytes || cfg.partBytes <= 0 || cfg.parallelParts < 2 {
		return nil
	}
	if a.flags == nil || !a.flags.Enabled(flagParallelGet, tenantID) {
		return nil
	}
	ce, ok := a.engine.(*engine.CoreEngine)
	if !ok {
		return nil
	}
	backend, rg, ok := ce.ParallelRangeSource(ctx, container, artifact)
	if !ok {
		return nil
	}
	if cfg.budget.Available() < 2*cfg.partBytes {
		largeGetFallbacks.WithLabelValues("budget").Inc()
		return nil
	}

	n := int((length + cfg.partBytes - 1) / cfg.partBytes)
	sizes := make([]int64, n)
	for i := range sizes {
		sizes[i] = cfg.partBytes
	}
	sizes[n-1] = length - int64(n-1)*cfg.partBytes

	var (
		idMu sync.Mutex
		etag string
	)
	fetch := func(fctx context.Context, i int) ([]byte, error) {
		off := offset + int64(i)*cfg.partBytes
		want := sizes[i]
		rc, info, err := rg.GetRangeInfo(fctx, container, artifact, off, want)
		if err != nil {
			return nil, fmt.Errorf("range %d (%d+%d) on %s: %w", i, off, want, backend, err)
		}
		defer func() { _ = rc.Close() }()
		if info.Size != objectSize {
			return nil, fmt.Errorf("%w: range %d reports object size %d, recorded %d", errRangeIdentity, i, info.Size, objectSize)
		}
		idMu.Lock()
		if etag == "" {
			etag = info.ETag
		}
		same := etag == info.ETag
		idMu.Unlock()
		if !same {
			return nil, fmt.Errorf("%w: range %d etag %q, earlier ranges %q", errRangeIdentity, i, info.ETag, etag)
		}
		buf := make([]byte, want)
		if _, err := io.ReadFull(rc, buf); err != nil {
			return nil, fmt.Errorf("range %d (%d+%d) on %s: read: %w", i, off, want, backend, err)
		}
		return buf, nil
	}

	start := time.Now()
	stream := parfetch.Start(ctx, parfetch.Config{
		Window:     int64(cfg.parallelParts) * cfg.partBytes,
		MaxParts:   cfg.parallelParts,
		HedgeAfter: cfg.hedgeAfter,
		Budget:     cfg.budget,
		FreeParts:  1,
		Hooks:      largeGetHooks("ranged"),
	}, sizes, fetch)
	first, err := stream.Next()
	if err != nil {
		stream.Close()
		if ctx.Err() == nil {
			largeGetFallbacks.WithLabelValues("first_part").Inc()
			a.logger.Warn("parallel get: first range failed, taking the single stream",
				zap.String("backend", backend), zap.String("container", container),
				zap.String("artifact", artifact), zap.Error(err))
		}
		return nil
	}
	common.SetBackendUsed(ctx, backend)
	return &parallelGetReader{Reader: parfetch.NewReader(stream, first), eng: ce, backend: backend, start: start}
}
