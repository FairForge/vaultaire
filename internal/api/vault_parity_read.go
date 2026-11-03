package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/FairForge/vaultaire/internal/common"
	"github.com/FairForge/vaultaire/internal/engine"
	"github.com/FairForge/vaultaire/internal/parfetch"
	"github.com/prometheus/client_golang/prometheus"
)

// The parity read (WP-VAULT-3 PR A): how a rebuild reads its m parity
// shards. Before it, Open and the scrub each opened the four shards one
// after another as whole GETs and the rebuilder pulled 1 MiB from shard 0,
// 1, 2, 3 per stripe — lockstep, nothing read ahead. On cold connections to
// Lyve that read at 3.8 MB/s against ~100 MB/s for the same four streams
// drained independently (bench-results/VAULT-PARITY-SERVE-2026-11.md § 1).
//
// Now ONE opener (openParityShards) serves both, and every shard is read
// ahead of the decoder, independently of the others:
//
//   - a leg with real ranged reads (engine.VersionedRangeGetter: lyve,
//     idrive, idrive-<region>, wasabi, blomp) is read as parallel ranges
//     through internal/parfetch (D-V3-1): a part = whole stripes of the
//     shard (LARGE_GET_PART_BYTES / stripe), LARGE_GET_PARALLEL_PARTS / m
//     parts in flight per shard, the large-GET hedge and budget; every range
//     of a shard reports the same ETag and total size or the read fails
//     (errRangeIdentity) — never a splice;
//   - any other leg (permafrost, sync) keeps one whole stream per shard,
//     opened concurrently and read ahead by its own goroutine into a ring of
//     vaultParityReadAhead pooled stripes from the same budget (D-V3-2).
//
// A rebuild settles each leg's breaker once, at Close (D-V3-5).

var (
	vaultParityRebuildSeconds = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "vaultaire_vault_parity_rebuild_seconds",
		Help:    "A parity rebuild's phases (a GET served from the parity copy, the scrub's rebuild check): open (the shard streams opened — 0 for a leg read by ranges, whose first range is the first stripe's), first_stripe (from the start to the first stripe decoded, before the status line), stream (from the first stripe to the last byte, only for a rebuild that delivered a whole window of at least 16 MiB).",
		Buckets: []float64{0.1, 0.25, 0.5, 1, 1.5, 2.5, 5, 10, 20, 30, 60},
	}, []string{"phase"})
	vaultParityRebuildRate = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "vaultaire_vault_parity_rebuild_bytes_per_second",
		Help: "The rate of the last whole parity rebuild of at least 16 MiB: the bytes delivered over the time from the start of its read to its last byte.",
	})
	// HEAD's live restore lookup of an archive-class object (D-V3-7): its
	// outcome — ok, error, or timeout past vaultHeadRestoreTimeout.
	vaultHeadRestoreLookups = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "vaultaire_vault_head_restore_lookups_total",
		Help: "HEAD's live x-amz-restore lookup of an archive-class object (Architecture Decision 2's one exception), by outcome: ok, error (the header omitted), timeout (no answer within 3 s: the header omitted), breaker_open (no lookup made: the backend's breaker is open, the header omitted).",
	}, []string{"outcome"})
	vaultParityShardReads = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "vaultaire_vault_parity_shard_reads_total",
		Help: "Parity shards read by a rebuild, by leg and kind: range (parallel ranged reads, D-V3-1) or whole (one read-ahead stream, D-V3-2). One per shard per rebuild.",
	}, []string{"leg", "kind"})
)

func init() {
	for _, o := range []string{"ok", "error", "timeout", "breaker_open"} {
		vaultHeadRestoreLookups.WithLabelValues(o)
	}
	for _, ph := range []string{"open", "first_stripe", "stream"} {
		vaultParityRebuildSeconds.WithLabelValues(ph)
	}
	for _, leg := range vaultParityLegs {
		for _, kind := range []string{"range", "whole"} {
			vaultParityShardReads.WithLabelValues(leg, kind)
		}
	}
}

// vaultParityRateMinBytes: a rebuild shorter than this sets no rate (a sealed
// header's window is one stripe; its rate is its latency).
const vaultParityRateMinBytes = 16 << 20

// vaultHeadRestoreTimeout bounds HEAD's live restore lookup (D-V3-7): on
// 2026-11-02 the lookup of a blob Geyser no longer had took 2.76 s.
const vaultHeadRestoreTimeout = 3 * time.Second

// shardReader yields one parity shard stripe by stripe from the first stripe
// of the read's window. The bytes Next returns are the caller's until the
// next Next or Close.
type shardReader interface {
	Next() ([]byte, error)
	Close()
}

// parityShardSource is one parity shard of a rebuild: its leg and index,
// how it is read, and the first error it met (what its leg is charged with).
type parityShardSource struct {
	leg   string
	index int
	kind  string // range | whole
	r     shardReader
	err   error
}

// Next is the shard's next stripe. An error is kept (the leg's outcome).
func (s *parityShardSource) Next() ([]byte, error) {
	if s.r == nil {
		if s.err == nil {
			s.err = errors.New("parity shard is not open")
		}
		return nil, s.err
	}
	b, err := s.r.Next()
	if err != nil && s.err == nil {
		s.err = err
	}
	return b, err
}

// Close ends the shard's reads (idempotent).
func (s *parityShardSource) Close() {
	if s.r != nil {
		s.r.Close()
		s.r = nil
	}
}

// parityShardSet is what openParityShards hands the rebuilder: the sources
// (nil = no leg, not registered, or its breaker open: never asked, never
// charged) and how they are settled.
type parityShardSet struct {
	srcs  []*parityShardSource
	eng   *engine.CoreEngine
	start time.Time
}

// readable counts the sources that opened.
func (s *parityShardSet) readable() int {
	n := 0
	for _, src := range s.srcs {
		if src != nil && src.r != nil {
			n++
		}
	}
	return n
}

// settle records ONE outcome per leg on its breaker (D-V3-5): the first
// failure of any of its shards; else, when the read delivered everything
// it was asked for, a success; else nothing (a client that went away says
// nothing about the leg). An object that changed under the read is no
// failure of the leg (errRangeIdentity), nor is a cancelled request — the
// engine's isBackendFailure decides the rest (a missing shard is no failure).
func (s *parityShardSet) settle(done bool) {
	if s == nil || s.eng == nil {
		return
	}
	type outcome struct {
		err  error
		seen bool
	}
	legs := map[string]*outcome{}
	var order []string
	for _, src := range s.srcs {
		if src == nil {
			continue
		}
		o, ok := legs[src.leg]
		if !ok {
			o = &outcome{}
			legs[src.leg] = o
			order = append(order, src.leg)
		}
		o.seen = true
		if src.err != nil && o.err == nil && !errors.Is(src.err, io.EOF) && !errors.Is(src.err, errRangeIdentity) {
			o.err = src.err
		}
	}
	for _, leg := range order {
		switch o := legs[leg]; {
		case o.err != nil:
			s.eng.RecordReadOutcome(leg, o.err)
		case done && o.seen:
			s.eng.RecordReadOutcome(leg, nil)
		}
	}
}

func (s *parityShardSet) close() {
	for _, src := range s.srcs {
		if src != nil {
			src.Close()
		}
	}
}

// openParityShards opens the m parity shards of row r for stripes
// [firstStripe, lastStripe), concurrently: a leg with ranged reads starts
// its parfetch stream (its first range is in flight when this returns), a
// whole-stream leg's GET is answered and its read-ahead goroutine started.
// The breakers are read once. all=true (the scrub's rebuild check): every
// shard must have a usable leg and open, or nothing is returned. A shard
// that fails to open is a source with its error (the leg is charged at
// settle); the read goes on without it.
func (p *VaultParity) openParityShards(ctx context.Context, r parityRow, l parityLayout, firstStripe, lastStripe int64, all bool) (*parityShardSet, error) {
	set := &parityShardSet{srcs: make([]*parityShardSource, l.m), eng: p.eng, start: time.Now()}
	tctx := common.WithTenantID(ctx, r.tenantID)
	container := parityContainer(r.tenantID)
	status := p.eng.GetFailoverStatus()
	var wg sync.WaitGroup
	for j := 0; j < l.m; j++ {
		legName := ""
		if j < len(r.legs) {
			legName = r.legs[j]
		}
		drv, ok := p.eng.GetDriver(legName)
		if legName == "" || !ok || status[legName] == engine.StateOpen.String() {
			if all {
				set.close()
				return nil, fmt.Errorf("shard p%d: leg %q unavailable", j, legName)
			}
			continue
		}
		src := &parityShardSource{leg: legName, index: j}
		set.srcs[j] = src
		artifact := shardArtifact(r.prefix, j)
		if rg, ok := drv.(engine.VersionedRangeGetter); ok {
			src.kind = "range"
			src.r = p.rangedShard(tctx, drv, rg, container, artifact, l, firstStripe, lastStripe)
			vaultParityShardReads.WithLabelValues(legName, src.kind).Inc()
			continue
		}
		src.kind = "whole"
		vaultParityShardReads.WithLabelValues(legName, src.kind).Inc()
		wg.Add(1)
		go func() {
			defer wg.Done()
			sr, err := p.wholeShard(tctx, drv, container, artifact, l, firstStripe, lastStripe)
			if err != nil {
				src.err = fmt.Errorf("shard p%d on %s: %w", j, legName, err)
				return
			}
			src.r = sr
		}()
	}
	wg.Wait()
	vaultParityRebuildSeconds.WithLabelValues("open").Observe(time.Since(set.start).Seconds())
	if all {
		for _, src := range set.srcs {
			if src.r == nil {
				err := src.err
				set.settle(false)
				set.close()
				return nil, err
			}
		}
	}
	return set, nil
}

// rangedShard reads stripes [first, last) of a shard as parallel ranges
// (D-V3-1). The stream starts fetching now; Next blocks on the first part.
func (p *VaultParity) rangedShard(ctx context.Context, drv engine.Driver, rg engine.VersionedRangeGetter, container, artifact string,
	l parityLayout, first, last int64) shardReader {
	cfg := p.LargeGet
	stripe := int64(l.stripe)
	partStripes := max(rangePartBytes(cfg, rg)/stripe, 1)
	perShard := max(cfg.parallelParts/l.m, 1)
	total := last - first
	n := int((total + partStripes - 1) / partStripes)
	sizes := make([]int64, n)
	for i := range sizes {
		sizes[i] = min(partStripes, total-int64(i)*partStripes) * stripe
	}
	hedgeAfter := cfg.hedgeAfter
	if engine.IsNoHedge(drv) {
		hedgeAfter = 0
	}
	id := &rangeIdentity{size: l.shardBytes()}
	base := first * stripe
	fetch := func(fctx context.Context, i int) ([]byte, error) {
		off := base + int64(i)*partStripes*stripe
		want := sizes[i]
		rc, info, err := rg.GetRangeInfo(fctx, container, artifact, off, want)
		if err != nil {
			return nil, fmt.Errorf("parity range %d (%d+%d) of %s: %w", i, off, want, artifact, err)
		}
		defer func() { _ = rc.Close() }()
		if err := id.check(i, info); err != nil {
			return nil, err
		}
		buf := make([]byte, want)
		if _, err := io.ReadFull(rc, buf); err != nil {
			return nil, fmt.Errorf("parity range %d (%d+%d) of %s: read: %w", i, off, want, artifact, err)
		}
		return buf, nil
	}
	// The window counts the part the decoder holds: perShard + 1 parts
	// keep perShard ranges in flight while it works.
	stream := parfetch.Start(ctx, parfetch.Config{
		Window:     int64(perShard+1) * partStripes * stripe,
		MaxParts:   perShard + 1,
		HedgeAfter: hedgeAfter,
		Budget:     cfg.budget,
		FreeParts:  1,
		Hooks:      largeGetHooks("parity"),
	}, sizes, fetch)
	return &rangedShardReader{stream: stream, stripe: l.stripe}
}

// rangedShardReader cuts a shard's parts into stripes.
type rangedShardReader struct {
	stream *parfetch.Stream
	part   []byte
	stripe int
}

func (r *rangedShardReader) Next() ([]byte, error) {
	if len(r.part) == 0 {
		part, err := r.stream.Next()
		if err != nil {
			return nil, err
		}
		if len(part)%r.stripe != 0 {
			return nil, fmt.Errorf("parity range of %d bytes is not whole stripes of %d", len(part), r.stripe)
		}
		r.part = part
	}
	b := r.part[:r.stripe]
	r.part = r.part[r.stripe:]
	return b, nil
}

func (r *rangedShardReader) Close() { r.stream.Close() }

// wholeShard opens one whole stream of a shard (a range of it when the leg
// has plain ranged reads and the window is not the whole shard) and reads
// it ahead (D-V3-2).
func (p *VaultParity) wholeShard(ctx context.Context, drv engine.Driver, container, artifact string, l parityLayout, first, last int64) (shardReader, error) {
	stripe := int64(l.stripe)
	var rc io.ReadCloser
	var err error
	skip := first * stripe
	if rg, ok := drv.(engine.RangeGetter); ok && (first > 0 || last < l.stripes()) {
		rc, err = rg.GetRange(ctx, container, artifact, first*stripe, (last-first)*stripe)
		skip = 0
	} else {
		rc, err = drv.Get(ctx, container, artifact)
	}
	if err != nil {
		return nil, err
	}
	return newAheadShardReader(rc, l.stripe, skip, last-first, p.LargeGet.budget), nil
}

// aheadShardReader reads a whole shard stream into a ring of pooled stripes
// from its own goroutine, so a shard waiting on the decoder keeps its
// connection draining (D-V3-2). The ring takes vaultParityReadAhead stripes
// from the GET budget; when the budget cannot spare them the ring is ONE
// stripe, under no budget (the per-stream bound the lockstep reader had).
type aheadShardReader struct {
	rc       io.ReadCloser
	pool     *sync.Pool
	bufs     []*[]byte // every buffer of the ring, given back at Close
	full     chan *[]byte
	free     chan *[]byte
	done     chan struct{}
	exited   chan struct{}
	err      error // set by the goroutine before it closes full
	cur      *[]byte
	budget   *parfetch.Budget
	reserved int64
	once     sync.Once
}

func newAheadShardReader(rc io.ReadCloser, stripe int, skip, stripes int64, budget *parfetch.Budget) *aheadShardReader {
	depth := vaultParityReadAhead
	reserved := int64(depth * stripe)
	if !budget.TryAcquire(reserved) {
		depth, reserved = 1, 0
	}
	a := &aheadShardReader{rc: rc, pool: stripePool(stripe), full: make(chan *[]byte, depth), free: make(chan *[]byte, depth),
		done: make(chan struct{}), exited: make(chan struct{}), budget: budget, reserved: reserved}
	for i := 0; i < depth; i++ {
		b := a.pool.Get().(*[]byte)
		a.bufs = append(a.bufs, b)
		a.free <- b
	}
	go a.run(skip, stripes)
	return a
}

func (a *aheadShardReader) run(skip, stripes int64) {
	defer close(a.exited)
	defer close(a.full)
	if skip > 0 {
		if _, err := io.CopyN(io.Discard, a.rc, skip); err != nil {
			a.err = fmt.Errorf("skip to stripe: %w", err)
			return
		}
	}
	for s := int64(0); s < stripes; s++ {
		var b *[]byte
		select {
		case b = <-a.free:
		case <-a.done:
			return
		}
		if _, err := io.ReadFull(a.rc, *b); err != nil {
			a.err = err
			return
		}
		select {
		case a.full <- b:
		case <-a.done:
			return
		}
	}
}

func (a *aheadShardReader) Next() ([]byte, error) {
	if a.cur != nil {
		a.free <- a.cur
		a.cur = nil
	}
	b, ok := <-a.full
	if !ok {
		if a.err != nil {
			return nil, a.err
		}
		return nil, io.EOF
	}
	a.cur = b
	return *b, nil
}

// Close stops the goroutine (closing the stream unblocks its read), waits
// for it and gives every stripe back to the pool and the budget.
func (a *aheadShardReader) Close() {
	a.once.Do(func() {
		close(a.done)
		_ = a.rc.Close()
		<-a.exited
		for _, b := range a.bufs {
			a.pool.Put(b)
		}
		a.bufs, a.cur = nil, nil
		a.budget.Release(a.reserved)
	})
}

// stripePools: one pool of stripe buffers per stripe size (1 MiB on prod;
// tests use smaller stripes).
var stripePools sync.Map // int -> *sync.Pool

func stripePool(size int) *sync.Pool {
	if p, ok := stripePools.Load(size); ok {
		return p.(*sync.Pool)
	}
	p, _ := stripePools.LoadOrStore(size, &sync.Pool{New: func() any { return newStripeBuffer(size) }})
	return p.(*sync.Pool)
}

func newStripeBuffer(size int) *[]byte {
	b := make([]byte, size)
	return &b
}

// The scrub's digest read (WP-VAULT-2, moved here from vault_parity_scrub.go
// by WP-VAULT-3 so that every read of a parity shard lives in this file):
// one shard read end to end and hashed against the digest protect recorded.

// shardVerdict is one shard's read: its verdict, its SHA-256, the bytes read.
type shardVerdict struct {
	outcome string // ok | unreadable | missing | corrupt | "" (cut by the deadline: no verdict)
	sum     string
	read    int64
	err     error
}

// readShard reads one shard end to end and judges it.
func (p *VaultParity) readShard(ctx context.Context, drv engine.Driver, container, artifact string, want int64, digest string) shardVerdict {
	rc, err := drv.Get(ctx, container, artifact)
	if err != nil {
		if ctx.Err() != nil {
			return shardVerdict{}
		}
		if isObjectMissingErr(err) {
			return shardVerdict{outcome: "missing", err: err}
		}
		return shardVerdict{outcome: "unreadable", err: err}
	}
	defer func() { _ = rc.Close() }()
	h := sha256.New()
	n, err := io.Copy(h, io.LimitReader(rc, want+1))
	switch {
	case ctx.Err() != nil:
		return shardVerdict{read: n}
	case err != nil:
		return shardVerdict{outcome: "unreadable", read: n, err: err}
	}
	sum := hex.EncodeToString(h.Sum(nil))
	if n != want {
		return shardVerdict{outcome: "corrupt", sum: sum, read: n, err: fmt.Errorf("%d bytes, the row says %d", n, want)}
	}
	if digest != "" && sum != digest {
		return shardVerdict{outcome: "corrupt", sum: sum, read: n, err: errors.New("SHA-256 is not the one protect recorded")}
	}
	return shardVerdict{outcome: "ok", sum: sum, read: n}
}
