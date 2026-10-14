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
	used, err := eng.Put(tenantCtx(), "c", "k", strings.NewReader("x"))

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
// miss was not a miss. The engine keeps no route since WP-R6-1; the miss is
// still the verdict (the API's idempotent delete).
func TestDelete_ALocalStyleMissIsAMiss(t *testing.T) {
	// Arrange
	eng := NewEngine(nil, nopLogger(), &Config{DefaultBackend: "idrive"})
	eng.AddDriver("idrive", newMemDriver("idrive"))
	eng.AddDriver("lyve", missDriver{newMemDriver("lyve")})

	// Act
	err := eng.Delete(WithRecordedBackend(tenantCtx(), "lyve"), "c", "k")

	// Assert
	require.Error(t, err)
	assert.True(t, IsObjectMiss(err), "%v", err)
}
