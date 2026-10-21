package api

import (
	"bytes"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/FairForge/vaultaire/internal/engine"
	"github.com/FairForge/vaultaire/internal/flags"
	"github.com/FairForge/vaultaire/internal/parfetch"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// The sia driver's reads are never hedged (engine.NoHedgeDeclarer): on Sia
// every read spends the account's per-host read budget, so a second request
// for a slow part spends it twice and starves the read it was meant to
// rescue (bench-results/SIA-S3D-2026-10.md, SIA-BENCH-3/5). The driver
// resumes a cut body itself (drivers/sia_read.go).

// noHedgeRangedDriver is the ranged test backend declaring what sia declares.
type noHedgeRangedDriver struct{ *rangedDriver }

func (d *noHedgeRangedDriver) Name() string  { return "sia" }
func (d *noHedgeRangedDriver) NoHedge() bool { return true }

func setupNoHedgeParallelFixture(t *testing.T) *parallelFixture {
	t.Helper()
	f := setupAdapterFixture(t)
	drv := newRangedDriver()
	f.eng.AddDriver("sia", &noHedgeRangedDriver{drv})
	f.eng.SetPrimary("sia")
	f.adapter = NewS3ToEngine(f.eng, f.db, zap.NewNop())
	fl := flags.New(nil, zap.NewNop())
	fl.Register(flagParallelGet, true)
	f.adapter.flags = fl
	f.adapter.largeGet.partBytes = 256 << 10
	f.adapter.largeGet.parallelParts = 4
	f.adapter.largeGet.minBytes = 1 << 20
	f.adapter.largeGet.hedgeAfter = 20 * time.Millisecond
	f.adapter.largeGet.budget = parfetch.NewBudget(64 << 20)
	return &parallelFixture{adapterTestFixture: f, drv: drv}
}

func TestParallelGet_ANoHedgeBackendIsNeverAskedTwice(t *testing.T) {
	// Arrange: one range is slow — far past the 20 ms hedge delay.
	f := setupNoHedgeParallelFixture(t)
	content := randomBytes(2 << 20)
	f.put(t, "big.bin", content)
	slow := int64(2 * (256 << 10))
	f.drv.delay = func(off int64, _ int32) time.Duration {
		if off == slow {
			return 300 * time.Millisecond
		}
		return 0
	}
	hedges := testutil.ToFloat64(largeGetHedges.WithLabelValues("ranged", "slow"))

	// Act
	w := f.get(t, "big.bin", nil)

	// Assert
	require.Equal(t, http.StatusOK, w.Code)
	assert.True(t, bytes.Equal(content, w.Body.Bytes()))
	assert.Equal(t, int32(8), f.drv.ranges.Load(), "one request per part, never a second")
	assert.Equal(t, hedges, testutil.ToFloat64(largeGetHedges.WithLabelValues("ranged", "slow")))
}

func TestParallelGet_AnObjectChangedUnderAResumedReadNeverChargesTheBreaker(t *testing.T) {
	// Arrange: a range after the first fails as the sia driver fails a
	// resume that found another object (412 / another ETag).
	f := setupNoHedgeParallelFixture(t)
	f.put(t, "big.bin", randomBytes(2<<20))
	f.drv.fail = func(off int64, _ int32) error {
		if off > 0 {
			return fmt.Errorf("sia resume of k at %d: %w", off, engine.ErrObjectChanged)
		}
		return nil
	}

	// Act: more failed reads than the breaker's threshold (5).
	for i := 0; i < 6; i++ {
		_ = f.get(t, "big.bin", nil)
	}

	// Assert
	assert.Equal(t, "closed", f.eng.GetFailoverStatus()["sia"])
}

func TestLargeGetHedge_OffForANoHedgeBackendOnly(t *testing.T) {
	// The chunked read's stream config: hedged unless a chunk of the plan
	// is on a backend that declares NoHedge; the other backends unchanged.
	eng := engine.NewEngine(nil, zap.NewNop(), nil)
	eng.AddDriver("idrive", newRangedDriver())
	eng.AddDriver("sia", &noHedgeRangedDriver{newRangedDriver()})
	eng.SetPrimary("idrive")
	a := NewS3ToEngine(eng, nil, zap.NewNop())
	a.largeGet.chunkHedgeAfter = 400 * time.Millisecond

	plan := func(backends ...string) []chunkSlice {
		out := make([]chunkSlice, len(backends))
		for i, b := range backends {
			out[i] = chunkSlice{desc: chunkDesc{backendID: b, size: 1}}
		}
		return out
	}
	assert.Equal(t, 400*time.Millisecond, a.chunkStreamConfig(plan("idrive", "idrive")).HedgeAfter)
	assert.Equal(t, 400*time.Millisecond, a.chunkStreamConfig(plan("unregistered")).HedgeAfter)
	assert.Equal(t, time.Duration(0), a.chunkStreamConfig(plan("sia")).HedgeAfter)
	assert.Equal(t, time.Duration(0), a.chunkStreamConfig(plan("idrive", "sia")).HedgeAfter, "one sia chunk: no hedge")

	// An engine that is not the CoreEngine (a test wrapper) keeps the hedge.
	a.engine = struct{ engine.Engine }{eng}
	assert.Equal(t, 400*time.Millisecond, a.chunkStreamConfig(plan("sia")).HedgeAfter)
}
