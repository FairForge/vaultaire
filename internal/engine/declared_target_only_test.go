package engine

import (
	"bytes"
	"context"
	"errors"
	"io"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// WP-R7-7 (R7-16): a backend may declare itself target-only at run time —
// the Lyve driver does when its stored-<region> bucket has Object Lock
// enabled or a default retention, where every fail-over write would become
// undeletable until the retention lapsed (an erasure request could not be
// honoured). The engine's name list (targetOnlyBackends) cannot know that;
// the driver's TargetOnly() can. Such a backend is never a fail-over
// destination and never the primary — the shape `sync` has.

type declaringDriver struct {
	stubDriver
	targetOnly bool
	puts       atomic.Int32
	fail       bool
}

func (d *declaringDriver) TargetOnly() bool { return d.targetOnly }

func (d *declaringDriver) Put(ctx context.Context, c, a string, r io.Reader, o ...PutOption) error {
	d.puts.Add(1)
	if d.fail {
		return errors.New("503 service unavailable")
	}
	_, err := io.Copy(io.Discard, r)
	return err
}

func newDeclaringEngine(lockedLyve bool) (*CoreEngine, *declaringDriver, *declaringDriver) {
	eng := NewEngine(nil, zap.NewNop(), &Config{DefaultBackend: "idrive"})
	primary := &declaringDriver{stubDriver: stubDriver{name: "idrive", data: map[string][]byte{}, healthy: true}, fail: true}
	lyve := &declaringDriver{stubDriver: stubDriver{name: "lyve", data: map[string][]byte{}, healthy: true}, targetOnly: lockedLyve}
	eng.AddDriver("idrive", primary)
	eng.AddDriver("lyve", lyve)
	return eng, primary, lyve
}

func TestEngine_ADeclaredTargetOnlyBackendIsNeverAFailoverDestination(t *testing.T) {
	eng, primary, lyve := newDeclaringEngine(true)

	_, err := eng.Put(tenantCtx(), "c", "k", bytes.NewReader([]byte("x")))

	require.Error(t, err, "the primary failed and nothing else may take the write")
	assert.ErrorIs(t, err, ErrAllBackendsUnavailable)
	assert.Equal(t, int32(1), primary.puts.Load())
	assert.Zero(t, lyve.puts.Load(), "a locked lyve bucket never takes someone else's object")
	assert.NotContains(t, eng.buildWriteCandidateList(""), "lyve")
}

// Control: an unlocked lyve stays the general fail-over leg (R6-04).
func TestEngine_AnUndeclaredLyveStaysTheFailoverLeg(t *testing.T) {
	eng, _, lyve := newDeclaringEngine(false)

	backend, err := eng.Put(tenantCtx(), "c", "k", bytes.NewReader([]byte("x")))

	require.NoError(t, err)
	assert.Equal(t, "lyve", backend)
	assert.Equal(t, int32(1), lyve.puts.Load())
}

// The explicit target still receives its writes (RESILIENT → lyve): a
// declared target-only backend is exactly that, a target.
func TestEngine_ADeclaredTargetOnlyBackendStillTakesItsOwnClass(t *testing.T) {
	eng, _, lyve := newDeclaringEngine(true)

	backend, err := eng.Put(tenantCtx(), "c", "k", bytes.NewReader([]byte("x")), WithStorageClass("RESILIENT"))

	require.NoError(t, err)
	assert.Equal(t, "lyve", backend)
	assert.Equal(t, int32(1), lyve.puts.Load())
}

func TestEngine_ADeclaredTargetOnlyBackendCannotBeThePrimary(t *testing.T) {
	eng, _, lyve := newDeclaringEngine(true)

	err := eng.CheckPrimaryEligible("lyve")
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrNotPrimaryEligible)

	lyve.targetOnly = false
	assert.NoError(t, eng.CheckPrimaryEligible("lyve"))
	assert.True(t, IsDeclaredTargetOnly(&declaringDriver{targetOnly: true}))
	assert.False(t, IsDeclaredTargetOnly(&stubDriver{}))
	_ = context.Background()
}
