package api

// WP-R2-6 (+ R8-12), Stage 1a Prompt 14 Part C: a process-wide bound on the
// memory chunked PUTs hold.
//
// A chunked write runs CHUNK_PUT_CONCURRENCY (8) store workers, each holding
// one chunk (≤ 16 MiB), its compressed form and its ciphertext — ≈ 72 MiB a
// typical write, ≤ 576 MiB the worst (R8-12) — and nothing bounded how many
// such writes ran at once. The large GETs have had one budget since Prompt
// 2b.3 (LARGE_GET_PARALLEL_BUDGET_BYTES, the model); this is the PUT side.
//
// One budget per process (CHUNK_PUT_BUDGET_BYTES; default a quarter of
// GOMEMLIMIT when one is set, else 2 GiB). A chunked write's FIRST act, before
// it reads a byte of its body or claims a chunk, is to reserve one worker's
// worst-case buffers (chunkPutReserveBytes, 48 MiB) — within
// chunkPutBudgetWait (2 s), else it is refused: 503 SlowDown + Retry-After
// before anything is stored, counted in vaultaire_chunk_put_budget_refusals_total
// and in vaultaire_errors_total (real saturation, unlike the egress guard's
// per-client 503). A write that holds its reserve always progresses: every
// chunk takes its buffers from what the write holds, else from the budget —
// WAITING for its own chunks to finish when the budget has none to give,
// which is how a write degrades to fewer chunks in flight (down to one)
// instead of being refused. A chunk's hold shrinks to its buffers' real size
// once compressed and sealed, and is given back when its store ends; a PUT
// that outlives its request (RP14-1) keeps its blob's share until it ends.
// The chunker's own look-ahead is bounded apart (chunkerLookahead).

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/FairForge/vaultaire/internal/crypto"
	"github.com/FairForge/vaultaire/internal/parfetch"
	"github.com/prometheus/client_golang/prometheus"
	"go.uber.org/zap"
)

const (
	chunkPutBudgetDefaultBytes = int64(2 << 30)
	chunkPutBudgetMinBytes     = int64(256 << 20)
	chunkPutBudgetMaxBytes     = int64(64 << 30)
	// chunkPutReserveBytes is one worker's buffers at the largest chunk —
	// the chunk, its compressed form, its ciphertext — reserved by a write
	// before it reads a byte and kept while it runs.
	chunkPutReserveBytes = int64(3 * crypto.ChunkMaxSize)
	// chunkPutBudgetWait bounds how long a write waits for that reserve
	// before it is refused.
	chunkPutBudgetWait = 2 * time.Second
	chunkPutBudgetPoll = 10 * time.Millisecond
	// chunkerLookahead is how many chunks the chunker may hold beyond what
	// the write has taken from the budget (it allocates each chunk before
	// the dispatcher sees it): with its 16 MiB work buffer, ≤ 64 MiB per
	// write outside the budget — it was 10 (≤ 192 MiB).
	chunkerLookahead = 2
)

// errChunkPutBudget: the budget could not spare one worker's buffers within
// the wait — nothing was read, claimed or stored.
var errChunkPutBudget = errors.New("chunked write: the process cannot take another large upload right now")

var (
	chunkPutBudgetRefusals = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "vaultaire_chunk_put_budget_refusals_total",
		Help: "Chunked writes answered 503 SlowDown because the process-wide chunk-buffer budget (CHUNK_PUT_BUDGET_BYTES) could not spare one worker's buffers within 2 s — before any chunk was stored. Real saturation: counted in vaultaire_errors_total too. Rule ChunkPutBudgetRefusing.",
	})
	chunkPutBudgetInUse = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "vaultaire_chunk_put_budget_bytes_in_use",
		Help: "Bytes of the process-wide chunk-buffer budget held by chunked writes now (reserves + chunks in flight, a PUT that outlived its request included).",
	})
	chunkPutBudgetSize = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "vaultaire_chunk_put_budget_bytes",
		Help: "Size of the process-wide chunk-buffer budget (CHUNK_PUT_BUDGET_BYTES; default a quarter of GOMEMLIMIT, else 2 GiB).",
	})
)

// chunkPutBudgetFromEnv reads CHUNK_PUT_BUDGET_BYTES ("2GiB", "512MiB", or
// bytes; 256 MiB–64 GiB): a rejected value is a Warn and the default — a
// quarter of memLimit when one is set (debug.SetMemoryLimit(-1): GOMEMLIMIT),
// else 2 GiB, clamped to the range.
func chunkPutBudgetFromEnv(getenv func(string) string, memLimit int64, logger *zap.Logger) int64 {
	def := chunkPutBudgetDefaultBytes
	if memLimit > 0 && memLimit < 1<<62 {
		def = min(max(memLimit/4, chunkPutBudgetMinBytes), chunkPutBudgetMaxBytes)
	}
	raw := getenv("CHUNK_PUT_BUDGET_BYTES")
	if raw == "" {
		return def
	}
	n, err := parseByteSize(raw)
	if err != nil || n < chunkPutBudgetMinBytes || n > chunkPutBudgetMaxBytes {
		logger.Warn("invalid CHUNK_PUT_BUDGET_BYTES (a size such as 2GiB, 256MiB..64GiB), keeping default",
			zap.String("value", raw), zap.Int64("default", def))
		return def
	}
	return n
}

// chunkPutBudget is the process's budget: the parfetch budget with the gauge.
type chunkPutBudget struct{ b *parfetch.Budget }

func newChunkPutBudget(limit int64) *chunkPutBudget {
	return &chunkPutBudget{b: parfetch.NewBudget(limit)}
}

func (c *chunkPutBudget) tryAcquire(n int64) bool {
	ok := c.b.TryAcquire(n)
	if ok {
		chunkPutBudgetInUse.Set(float64(c.b.InUse()))
	}
	return ok
}

func (c *chunkPutBudget) release(n int64) {
	c.b.Release(n)
	chunkPutBudgetInUse.Set(float64(c.b.InUse()))
}

func (c *chunkPutBudget) inUse() int64 { return c.b.InUse() }
func (c *chunkPutBudget) limit() int64 { return c.b.Limit() }

// reserve is a write's first act: one worker's buffers within wait, else
// errChunkPutBudget (counted). ctx ending first is the same refusal.
func (c *chunkPutBudget) reserve(ctx context.Context, n int64, wait time.Duration) (*chunkPutHold, error) {
	deadline := time.Now().Add(wait)
	for {
		if c.tryAcquire(n) {
			return &chunkPutHold{budget: c, reserve: n, granted: n}, nil
		}
		if ctx.Err() != nil || !time.Now().Before(deadline) {
			chunkPutBudgetRefusals.Inc()
			return nil, errChunkPutBudget
		}
		t := time.NewTimer(chunkPutBudgetPoll)
		select {
		case <-ctx.Done():
			t.Stop()
			chunkPutBudgetRefusals.Inc()
			return nil, errChunkPutBudget
		case <-t.C:
		}
	}
}

// chunkPutHold is what one chunked write holds from the budget: a reserve
// it keeps while it runs, and the chunks in flight on top of it.
type chunkPutHold struct {
	budget  *chunkPutBudget
	mu      sync.Mutex
	reserve int64 // kept while the write runs; 0 once closed
	granted int64 // taken from the budget
	inUse   int64 // assigned to chunks in flight
}

// chunkPutHoldBytes is a chunk's worst-case buffers: itself, compressed,
// sealed.
func chunkPutHoldBytes(size int) int64 { return 3 * int64(size) }

// hold takes n bytes for one chunk: from what the write holds, else from
// the budget; when neither has them it waits for the write's own chunks to
// finish (the degradation), or for ctx to end.
func (h *chunkPutHold) hold(ctx context.Context, n int64) error {
	if h == nil {
		return nil
	}
	for {
		h.mu.Lock()
		if h.granted-h.inUse >= n {
			h.inUse += n
			h.mu.Unlock()
			return nil
		}
		need := n - (h.granted - h.inUse)
		if h.budget.tryAcquire(need) {
			h.granted += need
			h.inUse += n
			h.mu.Unlock()
			return nil
		}
		h.mu.Unlock()
		t := time.NewTimer(chunkPutBudgetPoll)
		select {
		case <-ctx.Done():
			t.Stop()
			return ctx.Err()
		case <-t.C:
		}
	}
}

// release gives back n bytes of a chunk's hold; what the write then holds
// beyond its reserve returns to the budget at once.
func (h *chunkPutHold) release(n int64) {
	if h == nil || n <= 0 {
		return
	}
	h.mu.Lock()
	h.inUse -= n
	if excess := h.granted - h.inUse - h.reserve; excess > 0 {
		h.granted -= excess
		h.budget.release(excess)
	}
	h.mu.Unlock()
}

// close ends the write: the reserve goes back; what chunks still in flight
// hold (a PUT that outlived the request) returns as each of them ends.
func (h *chunkPutHold) close() {
	if h == nil {
		return
	}
	h.mu.Lock()
	h.reserve = 0
	if excess := h.granted - h.inUse; excess > 0 {
		h.granted -= excess
		h.budget.release(excess)
	}
	h.mu.Unlock()
}

// chunkLease is one chunk's share of a hold, handed down the store so the
// goroutine of a PUT that outlives its request can give its part back.
type chunkLease struct {
	h    *chunkPutHold
	mu   sync.Mutex
	left int64
}

// release gives back n of what is left.
func (l *chunkLease) release(n int64) {
	if l == nil {
		return
	}
	l.mu.Lock()
	n = min(n, l.left)
	l.left -= n
	l.mu.Unlock()
	l.h.release(n)
}

// releaseAll gives back everything left.
func (l *chunkLease) releaseAll() {
	if l == nil {
		return
	}
	l.mu.Lock()
	n := l.left
	l.left = 0
	l.mu.Unlock()
	l.h.release(n)
}

// split carves n bytes out of the lease for another owner (the detached
// PUT's goroutine) and returns their lease.
func (l *chunkLease) split(n int64) *chunkLease {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	n = min(n, l.left)
	l.left -= n
	l.mu.Unlock()
	return &chunkLease{h: l.h, left: n}
}

// defaultChunkPutBudget serves an adapter built without a server (tests).
var defaultChunkPutBudget = newChunkPutBudget(chunkPutBudgetDefaultBytes)

func (a *S3ToEngine) chunkBudget() *chunkPutBudget {
	if a.chunkPutBudget != nil {
		return a.chunkPutBudget
	}
	return defaultChunkPutBudget
}
