package engine

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// Review R12: the admin "Set as Primary" button reached SetPrimary with any
// name. Only a registered, general-purpose backend may take writes for
// everyone.
func TestCheckPrimaryEligible(t *testing.T) {
	eng := NewEngine(nil, zap.NewNop(), &Config{DefaultBackend: "idrive"})
	for _, n := range []string{"idrive", "lyve", "r2", "geyser", "permafrost", "sync", "idrive-eu-west-1", "local"} {
		eng.AddDriver(n, &mockDriver{name: n})
	}

	assert.NoError(t, eng.CheckPrimaryEligible("idrive"))
	assert.NoError(t, eng.CheckPrimaryEligible("lyve"))
	assert.NoError(t, eng.CheckPrimaryEligible("local"), "the dev/hub primary")

	for _, n := range []string{"r2", "geyser", "permafrost", "sync", "idrive-eu-west-1"} {
		err := eng.CheckPrimaryEligible(n)
		require.ErrorIs(t, err, ErrNotPrimaryEligible, n)
	}
	err := eng.CheckPrimaryEligible("does-not-exist")
	require.ErrorIs(t, err, ErrNotPrimaryEligible)
	assert.Contains(t, err.Error(), "not a registered backend")
}
