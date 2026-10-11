package engine

import (
	"context"
	"fmt"
	"io/fs"
	"strings"
	"testing"

	promtest "github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Prompt 2b.4 E1.1: a walk past a failed backend was a Warn line only.
func TestExecuteOp_CountsAFalloverFromTheFailedBackendToTheNext(t *testing.T) {
	// Arrange
	eng := NewEngine(nil, nopLogger(), &Config{DefaultBackend: "idrive"})
	idrive, lyve := newMemDriver("idrive"), newMemDriver("lyve")
	idrive.putErr = fmt.Errorf("dial tcp: connection refused")
	eng.AddDriver("idrive", idrive)
	eng.AddDriver("lyve", lyve)
	before := promtest.ToFloat64(FalloverCounter().WithLabelValues("idrive", "lyve", "put"))

	// Act
	used, err := eng.Put(context.Background(), "c", "k", strings.NewReader("x"))

	// Assert
	require.NoError(t, err)
	assert.Equal(t, "lyve", used)
	assert.Equal(t, 1.0, promtest.ToFloat64(FalloverCounter().WithLabelValues("idrive", "lyve", "put"))-before)
}

// missDriver answers a delete of an absent object the way LocalDriver does:
// an *fs.PathError wrapping fs.ErrNotExist, not a NotFoundError.
type missDriver struct{ *memDriver }

func (d missDriver) Delete(context.Context, string, string) error {
	return &fs.PathError{Op: "remove", Path: "/data/c/k", Err: fs.ErrNotExist}
}

// Prompt 2b.4 E1.4: Delete's "miss" matched NotFoundError only, so a local
// miss kept objectBackends/object_locations while the API dropped the head
// row. Before (eb79aaf): the route stayed "lyve".
func TestDelete_ALocalStyleMissDropsTheRoute(t *testing.T) {
	// Arrange
	eng := NewEngine(nil, nopLogger(), &Config{DefaultBackend: "idrive"})
	eng.AddDriver("idrive", newMemDriver("idrive"))
	eng.AddDriver("lyve", missDriver{newMemDriver("lyve")})
	eng.HintBackend("c", "k", "lyve")

	// Act
	err := eng.Delete(context.Background(), "c", "k")

	// Assert
	require.Error(t, err)
	_, kept := eng.objectBackends.Load(objectKey("c", "k"))
	assert.False(t, kept, "a miss is gone: the route goes with it")
}
