package engine

// WP-R6-2: the driver map and the primary are read under e.mu. The admin
// dashboard swaps the primary on a serving engine (HandleSetPrimary →
// SwapPrimary), so a call that read e.primary or e.drivers outside the lock
// raced it — and a map read concurrent with a map write (AddDriver) is a Go
// runtime fault ("concurrent map read and map write"), not a race report.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/FairForge/vaultaire/internal/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

func TestEngine_ServingCallsDuringPrimarySwapsAndLateRegistrations(t *testing.T) {
	// Arrange: two drivers, sealed as main seals it; it has served one call.
	eng := NewEngine(nil, zap.NewNop(), &Config{DefaultBackend: "a"})
	require.NoError(t, eng.AddDriver("a", newCountingStore("a")))
	require.NoError(t, eng.AddDriver("b", newCountingStore("b")))
	eng.Seal()
	ctx := common.WithTenantID(context.Background(), "tenant-r6-2")
	_, err := eng.Put(ctx, "c", "seed", bytes.NewReader([]byte("x")))
	require.NoError(t, err)

	// Act: callers on every entry point while the primary flips between the
	// two and late registrations arrive.
	stop := make(chan struct{})
	var bg sync.WaitGroup
	bg.Add(2)
	go func() {
		defer bg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			eng.SetPrimary([]string{"a", "b"}[i%2])
		}
	}()
	var lateRefused atomic.Int32
	go func() {
		defer bg.Done()
		for i := 0; i < 50; i++ {
			if err := eng.AddDriver(fmt.Sprintf("late-%d", i), newCountingStore("late")); errors.Is(err, ErrEngineServing) {
				lateRefused.Add(1)
			}
		}
	}()

	var callers sync.WaitGroup
	var failures atomic.Int32
	for g := 0; g < 4; g++ {
		callers.Add(1)
		go func(g int) {
			defer callers.Done()
			for i := 0; i < 100; i++ {
				key := fmt.Sprintf("k-%d-%d", g, i)
				backend, err := eng.Put(ctx, "c", key, bytes.NewReader([]byte("v")))
				if err != nil {
					failures.Add(1)
					continue
				}
				rctx := WithRecordedBackend(ctx, backend)
				if rc, err := eng.Get(rctx, "c", key); err == nil {
					_, _ = io.Copy(io.Discard, rc)
					_ = rc.Close()
				} else {
					failures.Add(1)
				}
				if rc, err := eng.GetRange(rctx, "c", key, 0, 1); err == nil {
					_ = rc.Close()
				} else {
					failures.Add(1)
				}
				if rc, err := eng.GetOn(ctx, backend, "c", key); err == nil {
					_ = rc.Close()
				} else {
					failures.Add(1)
				}
				_, _ = eng.ExistsOn(ctx, backend, "c", key)
				_, _ = eng.List(ctx, "c", "")
				_ = eng.CanAsk(backend)
				_, _, _ = eng.ParallelRangeSource(rctx, "c", key)
				_ = eng.CheckDriver(ctx, backend)
				_, _ = eng.GetMetrics(ctx)
				_ = eng.HealthCheck(ctx)
				_ = eng.CheckPrimaryEligible("b")
				// A read naming no backend is the primary's, whichever it is now.
				if rc, err := eng.Get(ctx, "c", key); err == nil {
					_ = rc.Close()
				}
				if err := eng.Delete(rctx, "c", key); err != nil {
					failures.Add(1)
				}
			}
		}(g)
	}
	callers.Wait()
	close(stop)
	bg.Wait()

	// Assert: every recorded-backend call answered from its own backend; no
	// driver registered after the seal.
	assert.Zero(t, failures.Load(), "a call named its backend and still failed")
	assert.EqualValues(t, 50, lateRefused.Load(), "AddDriver on a sealed engine must be refused")
	assert.Equal(t, []string{"a", "b"}, eng.GetDriverNames())
}

func TestAddDriver_RefusedOnceTheEngineIsSealed(t *testing.T) {
	// Arrange: before the seal a registration is taken, even after a call
	// (a test fixture swapping a driver in).
	eng := NewEngine(nil, zap.NewNop(), &Config{DefaultBackend: "a"})
	require.NoError(t, eng.AddDriver("a", newCountingStore("a")))
	ctx := common.WithTenantID(context.Background(), "tenant-r6-2")
	_, _ = eng.Get(WithRecordedBackend(ctx, "a"), "c", "absent")
	require.NoError(t, eng.AddDriver("b", newCountingStore("b")))

	// Act
	eng.Seal()
	err := eng.AddDriver("late", newCountingStore("late"))

	// Assert: refused, nothing registered, no breaker for it.
	require.ErrorIs(t, err, ErrEngineServing)
	assert.Equal(t, []string{"a", "b"}, eng.GetDriverNames())
	assert.NotContains(t, eng.GetFailoverStatus(), "late")
}

func TestAddDriver_UnsealedDuringTraffic_IsRaceFree(t *testing.T) {
	// Arrange: an unsealed engine (a fixture) serving calls.
	eng := NewEngine(nil, zap.NewNop(), &Config{DefaultBackend: "a"})
	require.NoError(t, eng.AddDriver("a", newCountingStore("a")))
	ctx := common.WithTenantID(context.Background(), "tenant-r6-2")

	// Act: registrations beside reads and writes — the map is copy-on-write,
	// a call keeps the map its view took.
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 50; i++ {
			assert.NoError(t, eng.AddDriver(fmt.Sprintf("late-%d", i), newCountingStore("late")))
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			backend, err := eng.Put(ctx, "c", fmt.Sprintf("k%d", i), bytes.NewReader([]byte("v")))
			if assert.NoError(t, err) {
				_ = eng.Delete(WithRecordedBackend(ctx, backend), "c", fmt.Sprintf("k%d", i))
			}
			_ = eng.HealthCheck(ctx)
		}
	}()
	wg.Wait()

	// Assert
	assert.Len(t, eng.GetDriverNames(), 51)
}

func TestSwapPrimary_ChecksAndSwapsUnderOneLock(t *testing.T) {
	// Arrange
	eng := NewEngine(nil, zap.NewNop(), &Config{DefaultBackend: "idrive"})
	for _, n := range []string{"idrive", "wasabi", "r2"} {
		require.NoError(t, eng.AddDriver(n, newCountingStore(n)))
	}
	locked := &declaringDriver{stubDriver: stubDriver{name: "lyve", data: map[string][]byte{}, healthy: true}, targetOnly: true}
	require.NoError(t, eng.AddDriver("lyve", locked))

	// Act + Assert: an ineligible backend is refused and the primary kept.
	prev, err := eng.SwapPrimary("r2")
	require.ErrorIs(t, err, ErrNotPrimaryEligible)
	assert.Equal(t, "idrive", prev)
	_, err = eng.SwapPrimary("lyve")
	require.ErrorIs(t, err, ErrNotPrimaryEligible, "a driver declaring itself target-only")
	_, err = eng.SwapPrimary("nope")
	require.ErrorIs(t, err, ErrNotPrimaryEligible)
	assert.Equal(t, "idrive", eng.GetPrimary())

	// A dormant backend may be made the primary; the previous one is named.
	prev, err = eng.SwapPrimary("wasabi")
	require.NoError(t, err)
	assert.Equal(t, "idrive", prev)
	assert.Equal(t, "wasabi", eng.GetPrimary())
	prev, err = eng.SwapPrimary("idrive")
	require.NoError(t, err)
	assert.Equal(t, "wasabi", prev)
}
