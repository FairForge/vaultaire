package drivers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/FairForge/vaultaire/internal/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// A fixed-bucket GET whose response headers never come must fail in bounded
// time (2b.4 E2.1): the client had no ResponseHeaderTimeout and no Timeout,
// so a hung iDrive GET lasted until TCP gave up — and a parfetch part hung
// on it held the whole ordered download.
func TestFixedBucketDriver_GetWithNoResponseHeadersFailsInBoundedTime(t *testing.T) {
	// Arrange
	old := fixedBucketResponseHeaderTimeout
	fixedBucketResponseHeaderTimeout = 200 * time.Millisecond
	t.Cleanup(func() { fixedBucketResponseHeaderTimeout = old })
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(func() { close(release); srv.Close() })
	d, err := NewFixedBucketS3Driver("wasabi", "ak", "sk", srv.URL, "us-west-1", "b", zap.NewNop())
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(common.WithTenantID(context.Background(), "t1"), 10*time.Second)
	defer cancel()

	// Act
	start := time.Now()
	_, err = d.Get(ctx, "c", "k")
	took := time.Since(start)

	// Assert
	require.Error(t, err)
	assert.Less(t, took, 5*time.Second, "bounded by the header timeout (× the SDK's attempts), not the caller's deadline: %s (%v)", took, err)
}
