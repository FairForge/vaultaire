package engine

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

func nopLogger() *zap.Logger { return zap.NewNop() }

// R6-02: the backend that HOLDS the object is down; the walk falls through to
// other backends which — correctly — do not have it. The aggregate used to be
// the last backend's not-found, which the API mapped to 404 NoSuchKey: an
// outage answered "this object does not exist" to every sync client.
func TestEngineGet_PreferredOutageIsNotReportedAsMissing(t *testing.T) {
	eng := NewEngine(nil, nopLogger(), &Config{DefaultBackend: "idrive"})
	eng.AddDriver("idrive", &mockDriver{name: "idrive", getErr: fmt.Errorf("dial tcp: connection refused")})
	eng.AddDriver("local", &fileWritingDriver{dir: t.TempDir()}) // *fs.PathError on a miss, like prod
	eng.HintBackend("c", "k", "idrive")

	_, err := eng.Get(context.Background(), "c", "k")
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrAllBackendsUnavailable),
		"an unreachable recorded backend must surface as unavailability, got: %v", err)
}

// The converse must hold too: when the recorded backend itself says "not
// found", that is the verdict — an unrelated backend being down must not turn
// a legitimate 404 into a 503.
func TestEngineGet_AuthoritativeMissStaysMissWhenOtherBackendDown(t *testing.T) {
	eng := NewEngine(nil, nopLogger(), &Config{DefaultBackend: "idrive"})
	eng.AddDriver("idrive", &stubDriver{name: "idrive", data: map[string][]byte{}, healthy: true}) // NotFoundError
	eng.AddDriver("lyve", &mockDriver{name: "lyve", getErr: fmt.Errorf("dial tcp: connection refused")})
	eng.HintBackend("c", "k", "idrive")

	_, err := eng.Get(context.Background(), "c", "k")
	require.Error(t, err)
	assert.False(t, errors.Is(err, ErrAllBackendsUnavailable), "an authoritative miss is a miss: %v", err)
	var nf NotFoundError
	var nfp *NotFoundError
	assert.True(t, errors.As(err, &nf) || errors.As(err, &nfp),
		"the recorded backend's not-found must be visible: %v", err)
}

// A recorded backend whose breaker is OPEN was never even asked; a fallback's
// not-found is equally not a verdict.
func TestFailoverManager_OpenPreferredBreakerPoisonsNotFound(t *testing.T) {
	fm := NewFailoverManager(nopLogger())
	fm.Register("idrive")
	fm.Register("local")
	for i := 0; i < failureThreshold; i++ {
		_, _ = fm.Execute(context.Background(), []string{"idrive"}, func(string) error {
			return fmt.Errorf("dial tcp: connection refused")
		})
	}
	require.Equal(t, "open", fm.GetStatus("idrive"))

	_, err := fm.Execute(context.Background(), []string{"idrive", "local"}, func(name string) error {
		require.Equal(t, "local", name, "open breaker must be skipped")
		return NotFoundError{Container: "c", Artifact: "k"}
	})
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrAllBackendsUnavailable), "got: %v", err)
}

// Existing semantics preserved: a real failure on every candidate is still
// reported as "all backends failed" wrapping the last error, and a client-level
// error from the FIRST candidate keeps its identity.
func TestFailoverManager_FirstCandidateClientErrorKeepsIdentity(t *testing.T) {
	fm := NewFailoverManager(nopLogger())
	fm.Register("a")
	fm.Register("b")

	_, err := fm.Execute(context.Background(), []string{"a", "b"}, func(name string) error {
		if name == "a" {
			return ErrQuotaExceeded
		}
		return fmt.Errorf("dial tcp: connection refused")
	})
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrQuotaExceeded))
	assert.False(t, errors.Is(err, ErrAllBackendsUnavailable))
}
