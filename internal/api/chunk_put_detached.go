package api

// RP14-1 (Stage 1a Prompt 14, Part 0): a chunk blob's backend PUT runs on a
// context detached from its request. The pool used to cancel every chunk
// PUT in flight when the request ended — the client cut mid-body, a sibling
// chunk failed — and a cancel that lands after the body went out leaves the
// backend free to commit it late (RP8-11), so the claim had to be kept for
// the takeover window (RP13-1). An interrupted upload thus left up to
// CHUNK_PUT_CONCURRENCY kept claims, and the client's immediate retry (aws-
// cli retries at once) waited on each of them with every worker blocked.
//
// Now a PUT that has STARTED runs to its own end under chunkPutBound,
// whatever happens to the request, and its outcome is known: answered OK →
// the row is made ready at ref 0 (the aborted write takes no reference), so
// a retry of the same body dedups onto it within seconds and a blob nobody
// retries is dedup GC's after its grace; answered with an error → the claim
// is dropped; no answer → the claim is kept (the rare case now). A chunk
// whose PUT had not started when the request ended is never attempted. The
// worker leaves at once when its request ends (the handler answers; a
// sibling failure does not wait 90 s for the others), and the PUT's
// goroutine finishes the claim itself. A stopping slot waits for them
// (Server.drainChunkPuts, after the long operations and before the engine
// closes the database); /health names them to the slot's own port.

import (
	"context"
	"sync/atomic"
	"time"

	"github.com/FairForge/vaultaire/internal/crypto"
	"github.com/FairForge/vaultaire/internal/engine"
	"github.com/prometheus/client_golang/prometheus"
	"go.uber.org/zap"
)

// chunkDetachedFinishBound bounds the row flip or claim drop that follows a
// chunk PUT (on its own context: the request may be gone).
const chunkDetachedFinishBound = 10 * time.Second

var (
	// chunkPutDetached counts chunk PUTs that outlived their request, by
	// what became of their claim. Every outcome exists at 0 from boot
	// (RP14-9: increase() sees no rise in a series born at 1).
	chunkPutDetached = func() *prometheus.CounterVec {
		c := prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "vaultaire_chunk_put_detached_total",
			Help: "Chunk blob PUTs that outlived their request (the client cut, a sibling chunk failed) and finished detached, by outcome: ready (answered OK — the row is ready at ref 0, a retry dedups onto it), dropped (answered with an error — the claim is dropped), kept (no answer — the claim is kept for the takeover window).",
		}, []string{"outcome"})
		for _, o := range []string{"ready", "dropped", "kept"} {
			c.WithLabelValues(o)
		}
		return c
	}()
	chunkPutsInFlightGauge = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "vaultaire_chunk_puts_in_flight",
		Help: "Chunk blob PUTs running right now, each on a context detached from its request under the chunk PUT bound (90 s); a stopping slot waits for them after the long operations.",
	})
)

// chunkPutRegistry counts the chunk PUTs in flight (each with the row flip
// or claim drop that follows it).
type chunkPutRegistry struct{ n atomic.Int64 }

func (g *chunkPutRegistry) add() {
	g.n.Add(1)
	chunkPutsInFlightGauge.Inc()
}

func (g *chunkPutRegistry) done() {
	g.n.Add(-1)
	chunkPutsInFlightGauge.Dec()
}

func (g *chunkPutRegistry) count() int { return int(g.n.Load()) }

// defaultChunkPuts serves an adapter built without a server (tests).
var defaultChunkPuts chunkPutRegistry

func (a *S3ToEngine) chunkPutReg() *chunkPutRegistry {
	if a.chunkPuts != nil {
		return a.chunkPuts
	}
	return &defaultChunkPuts
}

// chunkPuts is the server's registry (built on first use: bare test servers).
func (s *Server) chunkPuts() *chunkPutRegistry {
	s.chunkPutsOnce.Do(func() { s.chunkPutsReg = &chunkPutRegistry{} })
	return s.chunkPutsReg
}

// chunkPutDrainBound is how long a stopping slot waits for the chunk PUTs
// in flight: the PUT bound, the finish that follows, and a margin — never
// near the 15 min long-operation bound.
func (s *Server) chunkPutDrainBound() time.Duration {
	b := s.chunkPutBound
	if b <= 0 {
		b = chunkPutBoundDefault
	}
	return b + chunkDetachedFinishBound + 5*time.Second
}

// drainChunkPuts waits for the chunk PUTs in flight — each ends by itself
// within chunkPutBound and flips or drops its claim right after — and
// returns how many were still running when bound ran out (0 = none left).
// Called by Server.Shutdown after the long operations (a detached Complete's
// chunked branch holds chunk PUTs of its own) and before the trackers flush
// and the engine closes the database.
func (s *Server) drainChunkPuts(bound time.Duration) int {
	reg := s.chunkPuts()
	deadline := time.Now().Add(bound)
	for reg.count() > 0 {
		if time.Now().After(deadline) {
			return reg.count()
		}
		time.Sleep(10 * time.Millisecond)
	}
	return 0
}

// chunkPutResult is what one chunk's backend PUT came to.
type chunkPutResult struct {
	backend string
	outcome *engine.PutOutcome
	err     error
}

// putChunk runs a claimed chunk's backend PUT detached from the request,
// under the chunk bound, and returns its result — or detached = true when
// the request ended first: the PUT then finishes its claim by itself
// (finishDetachedChunkPut), and this writer must take no reference.
func (a *S3ToEngine) putChunk(ctx context.Context, scope, storageKey string, data []byte, since time.Time, entry *crypto.GCIEntry) (chunkPutResult, bool) {
	putCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), a.chunkPutBound)
	reg := a.chunkPutReg()
	reg.add()
	// decided: whoever sets it first owns the result — the worker (the
	// request ended; the goroutine finishes the claim) or the goroutine
	// (the PUT ended; the worker reads the result).
	var decided atomic.Bool
	res := make(chan chunkPutResult, 1)
	go func() {
		defer reg.done()
		bn, outcome, err := a.chunks().put(putCtx, storageKey, data)
		cancel()
		r := chunkPutResult{backend: bn, outcome: outcome, err: err}
		if decided.CompareAndSwap(false, true) {
			res <- r
			return
		}
		a.finishDetachedChunkPut(ctx, scope, storageKey, since, entry, r)
	}()
	select {
	case r := <-res:
		return r, false
	case <-ctx.Done():
		if decided.CompareAndSwap(false, true) {
			return chunkPutResult{}, true
		}
		return <-res, false // the PUT ended first
	}
}

// finishDetachedChunkPut settles the claim of a PUT whose request has
// gone, by what the backend said (the chunk_claim.go header): OK → ready
// at ref 0; an answered error → dropped; no answer → kept for the window.
func (a *S3ToEngine) finishDetachedChunkPut(ctx context.Context, scope, storageKey string, since time.Time, entry *crypto.GCIEntry, r chunkPutResult) {
	fctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), chunkDetachedFinishBound)
	defer cancel()
	hash := shortHash(entry.PlaintextHash)
	switch {
	case r.err == nil:
		_, done, err := a.readyChunk(fctx, scope, storageKey, r.backend, since, entry, false)
		if err != nil {
			// The blob landed but the row could not be flipped: the claim
			// stays, and the next writer takes it over after the window.
			chunkPutDetached.WithLabelValues("kept").Inc()
			a.logger.Warn("chunk store: a PUT that outlived its request landed but its row could not be made ready — its claim is taken over after its window",
				zap.String("scope", scope), zap.String("hash", hash), zap.String("backend", r.backend), zap.Error(err))
			return
		}
		chunkPutDetached.WithLabelValues("ready").Inc()
		if done {
			a.logger.Info("chunk store: a PUT that outlived its request landed — the chunk is ready for the next writer (no reference taken)",
				zap.String("scope", scope), zap.String("hash", hash), zap.String("backend", r.backend))
		}
	case r.outcome.MayHaveLanded():
		chunkPutDetached.WithLabelValues("kept").Inc()
		a.logger.Warn("chunk store: a PUT that outlived its request ended with no answer and may still land — its claim is kept for the takeover window",
			zap.String("scope", scope), zap.String("hash", hash),
			zap.Strings("unanswered", r.outcome.Unanswered()), zap.Duration("takeover_after", a.chunkPendingTakeover), zap.Error(r.err))
	default:
		chunkPutDetached.WithLabelValues("dropped").Inc()
		a.dropChunkClaim(fctx, scope, entry.PlaintextHash, since)
		a.logger.Info("chunk store: a PUT that outlived its request was answered with an error — its claim is dropped",
			zap.String("scope", scope), zap.String("hash", hash), zap.Error(r.err))
	}
}
