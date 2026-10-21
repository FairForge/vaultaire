package engine

import (
	"errors"
	"strings"
	"testing"

	promtest "github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Prompt 2b.5 F3.6: [A fails, B's breaker is open, C answers] was counted
// as B→C; A, the backend that was actually routed to and failed, vanished
// from the series (before: idrive→s3 0, lyve→s3 1).
func TestFallover_LabelKeepsTheFailedBackendWhenTheNextIsOpen(t *testing.T) {
	eng := NewEngine(nil, nopLogger(), &Config{DefaultBackend: "idrive"})
	a := &mockDriver{name: "idrive", putErr: errors.New("dial tcp: connection refused")}
	eng.AddDriver("idrive", a)
	eng.AddDriver("lyve", &mockDriver{name: "lyve"})
	eng.AddDriver("s3", &mockDriver{name: "s3"})
	eng.failover.mu.RLock()
	lb := eng.failover.breakers["lyve"]
	eng.failover.mu.RUnlock()
	for i := 0; i < failureThreshold; i++ {
		lb.RecordFailure()
	}
	require.False(t, lb.Allow())
	ac := promtest.ToFloat64(FalloverCounter().WithLabelValues("idrive", "s3", "put"))
	ab := promtest.ToFloat64(FalloverCounter().WithLabelValues("idrive", "lyve", "put"))
	bc := promtest.ToFloat64(FalloverCounter().WithLabelValues("lyve", "s3", "put"))

	used, err := eng.Put(tenantCtx(), "c", "k", strings.NewReader("x"))
	require.NoError(t, err)
	assert.Equal(t, "s3", used)
	dAC := promtest.ToFloat64(FalloverCounter().WithLabelValues("idrive", "s3", "put")) - ac
	dAB := promtest.ToFloat64(FalloverCounter().WithLabelValues("idrive", "lyve", "put")) - ab
	dBC := promtest.ToFloat64(FalloverCounter().WithLabelValues("lyve", "s3", "put")) - bc
	t.Logf("idrive->s3=%v idrive->lyve=%v lyve->s3=%v", dAC, dAB, dBC)
	assert.Equal(t, 1.0, dAC+dAB, "the failed backend idrive must appear as `from`")
}

// Prompt 2b.5 F3.1: a strict target with an unregistered name was not
// strict — the candidate list was the fan-out from the primary (before:
// readCandidates("sync") = [idrive lyve]).
func TestStrictTarget_UnregisteredAsksNobody(t *testing.T) {
	eng := NewEngine(nil, nopLogger(), &Config{DefaultBackend: "idrive"})
	eng.AddDriver("idrive", &mockDriver{name: "idrive"})
	eng.AddDriver("lyve", &mockDriver{name: "lyve"})
	before := promtest.ToFloat64(StrictUnregisteredCounter().WithLabelValues("sync", "get"))

	c, err := eng.readCandidates(eng.view(), "sync", "get")
	assert.Empty(t, c, "a strict recorded backend that is not registered asks nobody")
	assert.ErrorIs(t, err, ErrBackendNotRegistered)
	assert.Empty(t, eng.buildWriteCandidateList(eng.view(), "sync"), "nor is it written anywhere else")
	assert.Equal(t, 1.0, promtest.ToFloat64(StrictUnregisteredCounter().WithLabelValues("sync", "get"))-before)

	ctx := WithRecordedBackend(tenantCtx(), "sync")
	_, err = eng.Get(ctx, "c", "k")
	assert.ErrorIs(t, err, ErrAllBackendsUnavailable, "503, never the primary's copy")
	err = eng.Delete(ctx, "c", "k")
	assert.ErrorIs(t, err, ErrAllBackendsUnavailable, "the primary's DELETE of an absent key would be a 204 that drops the row")
}

// Prompt 2b.6 G1: the write side through the entry point. ResolveStorageClass
// handed the primary back for SYNC when no sync driver was registered, so
// Put never reached buildWriteCandidateList's strict guard (before:
// used="idrive" err=nil, the primary written once, put counter +0).
func TestStrictTarget_PutWithoutTheDriver_AsksNobody(t *testing.T) {
	eng := NewEngine(nil, nopLogger(), &Config{DefaultBackend: "idrive"})
	primary := &mockDriver{name: "idrive"}
	eng.AddDriver("idrive", primary)
	eng.AddDriver("lyve", &mockDriver{name: "lyve"})
	before := promtest.ToFloat64(StrictUnregisteredCounter().WithLabelValues("sync", "put"))

	used, err := eng.Put(tenantCtx(), "c", "k", strings.NewReader("new bytes"), WithStorageClass("SYNC"))

	t.Logf("Put(SYNC) without a sync driver: used=%q err=%v", used, err)
	assert.ErrorIs(t, err, ErrAllBackendsUnavailable, "503 + Retry-After at the API")
	assert.ErrorIs(t, err, ErrStrictTargetUnregistered, "typed: the cause is the missing driver, not a failing backend")
	assert.Empty(t, used)
	assert.Zero(t, primary.putCount, "the primary is never written")
	assert.Equal(t, 1.0, promtest.ToFloat64(StrictUnregisteredCounter().WithLabelValues("sync", "put"))-before)
}

// The resolver never substitutes another backend for a strict target: it
// names the target, registered or not, and Put refuses it.
func TestResolveStorageClass_StrictTargetIsNeverSubstituted(t *testing.T) {
	for class, backend := range storageClassToBackend {
		if !strictTarget(backend) {
			continue
		}
		got, resolved := ResolveStorageClass(class, "idrive", map[string]Driver{"idrive": nil})
		assert.Equal(t, backend, got, "class %s", class)
		assert.Equal(t, class, resolved)
	}
}

// R6-04 is unchanged for the target-only backends that are not strict: a
// target that fails is followed by the primary (the class-mapped ones,
// geyser and r2, are covered at the handler in api's
// TestStrictTargetWrite_NonStrictTargetsStillDegradeToThePrimary).
func TestWriteCandidates_NonStrictTargetOnlyStillFallToThePrimary(t *testing.T) {
	for _, name := range []string{"local", "r2", "geyser", "permafrost", "idrive-us-west-2"} {
		t.Run(name, func(t *testing.T) {
			require.True(t, writeOnlyWhenTargeted(name))
			require.False(t, strictTarget(name))
			eng := NewEngine(nil, nopLogger(), &Config{DefaultBackend: "lyve"})
			eng.AddDriver("lyve", &mockDriver{name: "lyve"})
			eng.AddDriver(name, &mockDriver{name: name, putErr: errors.New("dial tcp: connection refused")})
			cands := eng.buildWriteCandidateList(eng.view(), name)
			require.NotEmpty(t, cands)
			assert.Equal(t, name, cands[0])
			assert.Contains(t, cands, "lyve", "the primary follows a failing non-strict target")
		})
	}
	for _, class := range []string{"GLACIER", "DEEP_ARCHIVE", "PUBLIC", "RESILIENT"} {
		got, _ := ResolveStorageClass(class, "idrive", map[string]Driver{"idrive": nil})
		assert.Equal(t, "idrive", got, "class %s without its driver degrades to the primary", class)
	}
}

// `sia` is a strict, target-only backend (the sia tier), yet a lab still runs
// it as the primary (STORAGE_MODE=sia): as the primary it takes every write.
func TestSia_StrictTargetStillWorksAsThePrimary(t *testing.T) {
	assert.True(t, strictTarget("sia"))
	assert.True(t, writeOnlyWhenTargeted("sia"))

	eng := NewEngine(nil, nopLogger(), &Config{DefaultBackend: "sia"})
	eng.AddDriver("sia", &mockDriver{name: "sia"})
	eng.AddDriver("idrive", &mockDriver{name: "idrive"})
	eng.SetPrimary("sia")
	used, err := eng.Put(tenantCtx(), "c", "k", strings.NewReader("x"))
	require.NoError(t, err)
	assert.Equal(t, "sia", used)

	// Not the primary: never a fall-over destination for another object.
	eng2 := NewEngine(nil, nopLogger(), &Config{DefaultBackend: "idrive"})
	eng2.AddDriver("idrive", &mockDriver{name: "idrive"})
	eng2.AddDriver("sia", &mockDriver{name: "sia"})
	assert.NotContains(t, eng2.buildWriteCandidateList(eng2.view(), "idrive"), "sia")
	assert.Equal(t, []string{"sia"}, eng2.buildWriteCandidateList(eng2.view(), "sia"))
}
