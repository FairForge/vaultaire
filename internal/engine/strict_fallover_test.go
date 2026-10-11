package engine

import (
	"context"
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

	used, err := eng.Put(context.Background(), "c", "k", strings.NewReader("x"))
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

	assert.Empty(t, eng.readCandidates("sync", "get"), "a strict recorded backend that is not registered asks nobody")
	assert.Empty(t, eng.buildWriteCandidateList("sync"), "nor is it written anywhere else")
	assert.Equal(t, 1.0, promtest.ToFloat64(StrictUnregisteredCounter().WithLabelValues("sync", "get"))-before)

	eng.HintBackend("c", "k", "sync")
	_, err := eng.Get(context.Background(), "c", "k")
	assert.ErrorIs(t, err, ErrAllBackendsUnavailable, "503, never the primary's copy")
	err = eng.Delete(context.Background(), "c", "k")
	assert.ErrorIs(t, err, ErrAllBackendsUnavailable, "the primary's DELETE of an absent key would be a 204 that drops the row")
}
