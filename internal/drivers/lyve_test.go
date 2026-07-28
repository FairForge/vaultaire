package drivers

import (
	"bytes"
	"context"
	"crypto/rand"
	"fmt"
	"io"
	"os"
	"testing"
	"time"

	"github.com/FairForge/vaultaire/internal/common"
	"github.com/FairForge/vaultaire/internal/engine"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

func TestNewLyveDriver_DefaultRegion(t *testing.T) {
	// Arrange / Act: empty region must default to us-west-1 (closest Lyve
	// region to the SLC prod box; us-east-1 would silently route every op
	// cross-country — the exact trap behind the 2026-07 "Lyve degradation").
	d, err := NewLyveDriver("test-key", "test-secret", "tenant1", "", zap.NewNop())

	// Assert
	require.NoError(t, err)
	assert.Equal(t, "us-west-1", d.region)
	assert.Equal(t, "stored-us-west-1", d.getBucket())
}

func TestNewLyveDriver_ExplicitRegion(t *testing.T) {
	d, err := NewLyveDriver("test-key", "test-secret", "tenant1", "eu-west-1", zap.NewNop())

	require.NoError(t, err)
	assert.Equal(t, "eu-west-1", d.region)
	assert.Equal(t, "stored-eu-west-1", d.getBucket())
}

func TestLyveDriver_BuildTenantKey(t *testing.T) {
	d, err := NewLyveDriver("test-key", "test-secret", "tenant1", "us-west-1", zap.NewNop())
	require.NoError(t, err)

	key := d.buildTenantKey("acme", "photos", "2026/cat.jpg")

	assert.Equal(t, "t-acme/photos/2026/cat.jpg", key)
}

func TestLyveDriver_TenantFromContext(t *testing.T) {
	d, err := NewLyveDriver("test-key", "test-secret", "default-tenant", "us-west-1", zap.NewNop())
	require.NoError(t, err)

	// Context tenant overrides the driver default.
	ctx := context.WithValue(context.Background(), common.TenantIDKey, "ctx-tenant")
	assert.Equal(t, "ctx-tenant", d.getTenantID(ctx))

	// No context tenant falls back to the driver default.
	assert.Equal(t, "default-tenant", d.getTenantID(context.Background()))
}

// TestLyveDriver_Integration exercises the full CRUD surface against a real
// Lyve Cloud region. Requires LYVE_ACCESS_KEY / LYVE_SECRET_KEY (and
// optionally LYVE_REGION) plus the pre-created, correctly-homed
// stored-<region> bucket. Skipped otherwise.
//
// Lyve Cloud 2 gotcha: any regional endpoint transparently proxies to the
// bucket's HOME region (its replication policy), so these latencies are only
// meaningful when stored-<region> is homed in <region>.
func TestLyveDriver_Integration(t *testing.T) {
	accessKey := os.Getenv("LYVE_ACCESS_KEY")
	secretKey := os.Getenv("LYVE_SECRET_KEY")
	if accessKey == "" || secretKey == "" {
		t.Skip("Skipping integration test - no Lyve credentials")
	}
	region := os.Getenv("LYVE_REGION")

	d, err := NewLyveDriver(accessKey, secretKey, "bench-int", region, zap.NewNop())
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	require.NoError(t, d.HealthCheck(ctx), "HealthCheck (HeadBucket %s) must pass — create the bucket via its own regional endpoint first", d.getBucket())

	container := "it-container"
	artifact := fmt.Sprintf("bench-probe/it-%d.bin", time.Now().UnixNano())

	t.Run("SmallObjectCRUD", func(t *testing.T) {
		payload := []byte("lyve integration probe")
		require.NoError(t, d.Put(ctx, container, artifact, bytes.NewReader(payload), engine.WithContentType("text/plain")))

		exists, err := d.Exists(ctx, container, artifact)
		require.NoError(t, err)
		assert.True(t, exists)

		rc, err := d.Get(ctx, container, artifact)
		require.NoError(t, err)
		got, err := io.ReadAll(rc)
		require.NoError(t, err)
		mustClose(rc)
		assert.Equal(t, payload, got)

		keys, err := d.List(ctx, container, "bench-probe/")
		require.NoError(t, err)
		assert.Contains(t, keys, artifact)

		rc, err = d.GetRange(ctx, container, artifact, 5, 11)
		require.NoError(t, err)
		got, err = io.ReadAll(rc)
		require.NoError(t, err)
		mustClose(rc)
		assert.Equal(t, []byte("integration"), got)

		require.NoError(t, d.Delete(ctx, container, artifact))
		exists, err = d.Exists(ctx, container, artifact)
		require.NoError(t, err)
		assert.False(t, exists)
	})

	t.Run("LargeObjectMultipart", func(t *testing.T) {
		if testing.Short() {
			t.Skip("skipping 20MB upload in -short mode")
		}
		// > s3UploadPartSize so the parallel multipart path actually runs.
		big := make([]byte, 20<<20)
		_, err := rand.Read(big)
		require.NoError(t, err)
		bigKey := artifact + ".large"

		require.NoError(t, d.Put(ctx, container, bigKey, bytes.NewReader(big)))
		defer func() { _ = d.Delete(ctx, container, bigKey) }()

		rc, err := d.Get(ctx, container, bigKey)
		require.NoError(t, err)
		got, err := io.ReadAll(rc)
		require.NoError(t, err)
		mustClose(rc)
		require.Equal(t, len(big), len(got))
		assert.True(t, bytes.Equal(big, got), "round-tripped 20MB payload must match")
	})
}
