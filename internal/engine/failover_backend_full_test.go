package engine

import (
	"errors"
	"fmt"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A backend whose account is full (Blomp's Swift answered 413 "Upload
// exceeds quota" past 40 GiB on prod 2026-10-21) is not unhealthy — its
// reads and deletes still work — and the write is not the caller's to
// retry: the verdict is ErrBackendFull, never ErrAllBackendsUnavailable
// (which the API answers 503 "please retry"), no breaker charge, and no
// other backend is asked (the bytes would land off the tier the bucket
// promises).
func TestFailoverManager_BackendFullIsAVerdictNotAFailure(t *testing.T) {
	fm := NewFailoverManager(nopLogger())
	fm.Register("full")
	fm.Register("next")
	before := testutil.ToFloat64(backendFull.WithLabelValues("full", "other"))

	for i := 0; i < failureThreshold+2; i++ {
		_, err := fm.Execute(tenantCtx(), []string{"full", "next"}, func(name string) error {
			require.Equal(t, "full", name, "a full backend ends the walk")
			return fmt.Errorf("swift PUT k: %w", ErrBackendFull)
		})
		require.Error(t, err)
		assert.True(t, errors.Is(err, ErrBackendFull), "got: %v", err)
		assert.False(t, errors.Is(err, ErrAllBackendsUnavailable), "never the retryable 503: %v", err)
	}
	assert.Equal(t, "closed", fm.GetStatus("full"), "a full backend still serves reads")
	assert.Equal(t, float64(failureThreshold+2), testutil.ToFloat64(backendFull.WithLabelValues("full", "other"))-before)
}
