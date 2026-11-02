package api

import (
	"bytes"
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"sync/atomic"
	"testing"
	"time"

	"github.com/FairForge/vaultaire/internal/engine"
	"github.com/FairForge/vaultaire/internal/parfetch"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// WP-VAULT-3 PR A: the fast rebuild. A parity read of the 2026-11-02 E2E
// proof ran at 4.7 MB/s: four whole shard GETs opened one after another,
// then 1 MiB from each in lockstep (bench-results/VAULT-PARITY-SERVE-2026-11.md
// § 1). The leg here is a Lyve-shaped driver: ranged reads with identity
// (engine.VersionedRangeGetter), and a whole-object GET whose stream is
// paced, so the lockstep reader is slow in the test as it was on the wire.

// rangedLegDriver is a parity leg with real ranged reads: rangedDriver
// (in-memory, per-range delay / failure / ETag) plus a paced whole-object
// stream and an open delay for Get.
type rangedLegDriver struct {
	*rangedDriver
	getDelay   time.Duration // before Get answers
	streamStep time.Duration // per 256 KiB of a whole-object stream
	failGet    atomic.Bool   // Get answers an error
	failFirst  atomic.Bool   // the first byte of every shard is unreadable (Get and range 0)
}

func (d *rangedLegDriver) Get(ctx context.Context, container, artifact string) (io.ReadCloser, error) {
	if d.getDelay > 0 {
		time.Sleep(d.getDelay)
	}
	if d.failGet.Load() {
		return nil, errors.New("lyve: 503 service unavailable")
	}
	rc, err := d.rangedDriver.Get(ctx, container, artifact)
	if err != nil {
		return nil, err
	}
	if d.failFirst.Load() {
		return &brokenStream{Reader: io.LimitReader(rc, 0), Closer: rc}, nil
	}
	return &pacedStream{r: rc, step: d.streamStep}, nil
}

func (d *rangedLegDriver) GetRangeInfo(ctx context.Context, container, artifact string, offset, length int64) (io.ReadCloser, engine.RangeInfo, error) {
	if d.failGet.Load() {
		return nil, engine.RangeInfo{}, errors.New("lyve: 503 service unavailable")
	}
	if d.failFirst.Load() && offset == 0 {
		return nil, engine.RangeInfo{}, errors.New("lyve: 500 internal error")
	}
	return d.rangedDriver.GetRangeInfo(ctx, container, artifact, offset, length)
}

// pacedStream serves 256 KiB per step: a single stream's throughput.
type pacedStream struct {
	r    io.ReadCloser
	step time.Duration
	left int
}

func (p *pacedStream) Read(b []byte) (int, error) {
	if p.left == 0 {
		if p.step > 0 {
			time.Sleep(p.step)
		}
		p.left = 256 << 10
	}
	if len(b) > p.left {
		b = b[:p.left]
	}
	n, err := p.r.Read(b)
	p.left -= n
	return n, err
}

func (p *pacedStream) Close() error { return p.r.Close() }

// setupRangedLegFixture: the parity fixture with the ranged leg registered
// as "lyve" and taken as the leg, and small large-GET parts (4 stripes of
// the fixture's 64 KiB) so a few MiB exercise many ranges.
func setupRangedLegFixture(t *testing.T) (*parityFixture, *rangedLegDriver) {
	t.Helper()
	f := setupParityFixture(t)
	leg := &rangedLegDriver{rangedDriver: newRangedDriver()}
	f.eng.AddDriver("lyve", leg)
	f.svc.Legs = []string{"lyve"}
	f.svc.LargeGet.partBytes = 4 * int64(f.stripe)
	f.svc.LargeGet.parallelParts = 8 // 2 per shard
	f.svc.LargeGet.hedgeAfter = 0
	f.svc.LargeGet.budget = parfetch.NewBudget(64 << 20)
	return f, leg
}

func TestParityRebuild_OpensShardsConcurrently(t *testing.T) {
	f, leg := setupRangedLegFixture(t)
	size := 4 * f.stripe * 8 // 8 stripes
	body, etag := f.object("films/open.mkv", size)
	f.run()
	leg.getDelay = 300 * time.Millisecond
	leg.delay = func(int64, int32) time.Duration { return 300 * time.Millisecond }

	start := time.Now()
	rc, err := f.svc.Open(context.Background(), f.tenantID, f.bucket, "films/open.mkv", "", etag, int64(size), 0, int64(size))
	took := time.Since(start)
	require.NoError(t, err)
	defer func() { _ = rc.Close() }()
	assert.Less(t, took, 700*time.Millisecond, "four shards opened at once, not 4 × 300 ms one after another")
	got, err := io.ReadAll(rc)
	require.NoError(t, err)
	assert.True(t, bytes.Equal(body, got))
}

func TestParityRebuild_RangesInParallelNotLockstep(t *testing.T) {
	f, leg := setupRangedLegFixture(t)
	const stripes = 64
	size := 4 * f.stripe * stripes // 16 MiB: 4 MiB per shard = 16 ranges of 4 stripes
	body, etag := f.object("films/big.mkv", size)
	f.run()
	const delay = 50 * time.Millisecond
	leg.delay = func(int64, int32) time.Duration { return delay }
	leg.streamStep = delay // the whole stream: 256 KiB per 50 ms, as slow per byte as one range

	start := time.Now()
	rc, err := f.svc.Open(context.Background(), f.tenantID, f.bucket, "films/big.mkv", "", etag, int64(size), 0, int64(size))
	require.NoError(t, err)
	got, err := io.ReadAll(rc)
	require.NoError(t, err)
	_ = rc.Close()
	took := time.Since(start)
	assert.True(t, bytes.Equal(body, got))

	rangesPerShard := stripes * f.stripe / int(f.svc.LargeGet.partBytes) // 16
	inFlight := f.svc.LargeGet.parallelParts / vaultParityM              // 2
	bound := time.Duration(float64(rangesPerShard/inFlight+1) * float64(delay) * 1.5)
	assert.Less(t, took, bound, "ranges in flight on every shard at once; the lockstep reader takes 64 × 50 ms")
	assert.Equal(t, int32(0), leg.gets.Load(), "a leg with ranged reads is never read as a whole stream")
	assert.Equal(t, int32(4*rangesPerShard), leg.ranges.Load())
}

func TestParityRebuild_RangeIdentityChecked(t *testing.T) {
	f, leg := setupRangedLegFixture(t)
	size := 4 * f.stripe * 16
	body, etag := f.object("films/swap.mkv", size)
	f.run()
	// From the fourth range of each shard on, another object answers: an
	// overwrite of the shards mid-read. (The window holds three ranges per
	// shard — two in flight beside the one the decoder holds — and any of
	// the first three may be checked first; the fourth starts only once the
	// decoder has moved past the first.)
	part := f.svc.LargeGet.partBytes
	leg.etag = func(off int64) string {
		if off < 3*part {
			return "v1"
		}
		return "v2"
	}

	rc, err := f.svc.Open(context.Background(), f.tenantID, f.bucket, "films/swap.mkv", "", etag, int64(size), 0, int64(size))
	require.NoError(t, err, "the first range of every shard agrees with itself")
	got, err := io.ReadAll(rc)
	_ = rc.Close()
	require.Error(t, err)
	assert.True(t, errors.Is(err, errRangeIdentity), "got %v", err)
	assert.Less(t, len(got), size, "the body never completes")
	assert.True(t, bytes.Equal(body[:len(got)], got), "what was delivered is the object's own prefix — never spliced")
}

func TestParityRebuild_WholeStreamLegReadsAhead(t *testing.T) {
	// The fixture's leg (permafrost, a local driver): no ranged reads with
	// identity, so one whole stream per shard, opened at once and read ahead
	// by its own goroutine into a ring of vaultParityReadAhead stripes taken
	// from the GET budget (D-V3-2).
	f := setupParityFixture(t)
	budget := parfetch.NewBudget(64 << 20)
	f.svc.LargeGet.budget = budget
	size := 4*f.stripe*24 + 999
	body, etag := f.object("films/whole.mkv", size)
	f.run()

	rc, err := f.svc.Open(context.Background(), f.tenantID, f.bucket, "films/whole.mkv", "", etag, int64(size), 0, int64(size))
	require.NoError(t, err)
	var got bytes.Buffer
	var peak int64
	buf := make([]byte, 32<<10)
	for {
		n, rerr := rc.Read(buf)
		got.Write(buf[:n])
		peak = max(peak, budget.InUse())
		if errors.Is(rerr, io.EOF) {
			break
		}
		require.NoError(t, rerr)
	}
	_ = rc.Close()
	assert.True(t, bytes.Equal(body, got.Bytes()))
	limit := int64(vaultParityM * vaultParityReadAhead * f.stripe)
	assert.Positive(t, peak, "the read-ahead ring is taken from the GET budget")
	assert.LessOrEqual(t, peak, limit, "≤ m × vaultParityReadAhead stripes held")
	assert.Equal(t, int64(0), budget.InUse(), "Close gives every stripe back")

	// A window, and a window the leg reads as a range of each shard.
	rc, err = f.svc.Open(context.Background(), f.tenantID, f.bucket, "films/whole.mkv", "", etag, int64(size), 700_000, 900_000)
	require.NoError(t, err)
	w, err := io.ReadAll(rc)
	require.NoError(t, err)
	_ = rc.Close()
	assert.True(t, bytes.Equal(body[700_000:1_600_000], w))
}

func TestParityRebuild_FirstStripeBeforeReturn(t *testing.T) {
	// D-V3-4: the first stripe is decoded before Open returns. A shard set
	// that cannot give it (every parity shard unreadable at stripe 0, the
	// tape's ranges down too) fails Open — before any status line — never
	// the body.
	f, leg := setupRangedLegFixture(t)
	size := 4 * f.stripe * 4
	_, etag := f.object("films/first.mkv", size)
	f.run()
	leg.failFirst.Store(true)
	f.geyser.failGet.Store(true)
	f.geyser.failRange.Store(true)

	rc, err := f.svc.Open(context.Background(), f.tenantID, f.bucket, "films/first.mkv", "", etag, int64(size), 0, int64(size))
	if rc != nil {
		_ = rc.Close()
	}
	require.Error(t, err, "Open decides the first stripe")
	assert.True(t, errors.Is(err, errParityUnavailable), "got %v", err)
}

func TestParityRebuild_SettlesTheLegBreaker(t *testing.T) {
	// D-V3-5: one rebuild is one outcome on the leg's breaker — four shards
	// failing are one failure, not four (the breaker opens at five).
	f, leg := setupRangedLegFixture(t)
	size := 4 * f.stripe * 4
	_, etag := f.object("films/charge.mkv", size)
	f.run()
	leg.failGet.Store(true)
	f.geyser.failGet.Store(true)
	f.geyser.failRange.Store(true)

	rebuild := func() {
		rc, err := f.svc.Open(context.Background(), f.tenantID, f.bucket, "films/charge.mkv", "", etag, int64(size), 0, int64(size))
		if rc != nil {
			_ = rc.Close()
		}
		require.Error(t, err)
	}
	for i := 0; i < 4; i++ {
		rebuild()
	}
	assert.NotEqual(t, engine.StateOpen.String(), f.eng.GetFailoverStatus()["lyve"], "four rebuilds = four charges, not sixteen")
	rebuild()
	assert.Equal(t, engine.StateOpen.String(), f.eng.GetFailoverStatus()["lyve"], "the fifth failed rebuild opens the leg's breaker")

	// A rebuild that delivers every byte settles a success.
	f2, leg2 := setupRangedLegFixture(t)
	body, etag2 := f2.object("films/ok.mkv", size)
	f2.run()
	leg2.failGet.Store(true)
	for i := 0; i < 4; i++ {
		rc, err := f2.svc.Open(context.Background(), f2.tenantID, f2.bucket, "films/ok.mkv", "", etag2, int64(size), 0, int64(size))
		if rc != nil {
			_ = rc.Close()
		}
		require.Error(t, err)
	}
	leg2.failGet.Store(false)
	rc, err := f2.svc.Open(context.Background(), f2.tenantID, f2.bucket, "films/ok.mkv", "", etag2, int64(size), 0, int64(size))
	require.NoError(t, err)
	got, err := io.ReadAll(rc)
	require.NoError(t, err)
	_ = rc.Close()
	assert.True(t, bytes.Equal(body, got))
	leg2.failGet.Store(true)
	rc, _ = f2.svc.Open(context.Background(), f2.tenantID, f2.bucket, "films/ok.mkv", "", etag2, int64(size), 0, int64(size))
	if rc != nil {
		_ = rc.Close()
	}
	assert.NotEqual(t, engine.StateOpen.String(), f2.eng.GetFailoverStatus()["lyve"], "the success reset the count")
}

func TestScrubRebuild_SharesTheOpener(t *testing.T) {
	// D-V3-3: the scrub's rebuild check and the GET's Open read the shards
	// through ONE opener and ONE reader (newParityRead → openParityShards →
	// newParityRebuilder). The scrub file reads no backend of its own: a
	// shard read there (a Get, a range) would be a second opener that the
	// fast rebuild does not cover — every parity shard read lives in
	// vault_parity_read.go.
	fset := token.NewFileSet()
	calls := func(file string) map[string][]string {
		f, err := parser.ParseFile(fset, file, nil, 0)
		require.NoError(t, err)
		out := map[string][]string{} // function → the selector calls it makes
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				if c, ok := n.(*ast.CallExpr); ok {
					if sel, ok := c.Fun.(*ast.SelectorExpr); ok {
						out[fn.Name.Name] = append(out[fn.Name.Name], sel.Sel.Name)
					}
				}
				return true
			})
		}
		return out
	}
	scrub := calls("vault_parity_scrub.go")
	for fn, names := range scrub {
		for _, name := range names {
			switch name {
			case "Get", "GetRange", "GetRangeInfo", "GetOn":
				t.Errorf("vault_parity_scrub.go:%s calls .%s( — a parity read of its own; go through newParityRead (or vault_parity_read.go)", fn, name)
			}
		}
	}
	assert.Contains(t, scrub["rebuild"], "newParityRead", "the scrub's rebuild goes through the GET's opener")
	assert.Contains(t, calls("vault_parity.go")["Open"], "newParityRead", "the GET's Open goes through the one opener")
}
