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

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/FairForge/vaultaire/internal/common"
	"github.com/FairForge/vaultaire/internal/engine"
)

// Cloudflare R2 is the PUBLIC-BUCKET / CDN-origin backend only (Smart tier
// design, revised 2026-09-19): $0 egress, Cloudflare-adjacent to the
// cdn.stored.ge host. It is deliberately NOT a storage tier — nothing routes
// here unless the bucket is public-read (see api.publicBucketStorageClass).

func TestR2Endpoint(t *testing.T) {
	tests := []struct {
		jurisdiction string
		want         string
		wantErr      bool
	}{
		{"", "https://acct123.r2.cloudflarestorage.com", false},
		{"default", "https://acct123.r2.cloudflarestorage.com", false},
		{"eu", "https://acct123.eu.r2.cloudflarestorage.com", false},
		{"us", "https://acct123.us.r2.cloudflarestorage.com", false},
		{"fedramp", "https://acct123.fedramp.r2.cloudflarestorage.com", false},
		{"EU", "https://acct123.eu.r2.cloudflarestorage.com", false},
		{"mars", "", true},
	}
	for _, tt := range tests {
		t.Run("jurisdiction="+tt.jurisdiction, func(t *testing.T) {
			got, err := R2Endpoint("acct123", tt.jurisdiction)
			if tt.wantErr {
				assert.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestNewR2Driver_Validation(t *testing.T) {
	logger := zap.NewNop()

	t.Run("requires account id", func(t *testing.T) {
		d, err := NewR2Driver("", "ak", "sk", "", "", logger)
		assert.Nil(t, d)
		assert.ErrorContains(t, err, "account id")
	})
	t.Run("requires credentials", func(t *testing.T) {
		d, err := NewR2Driver("acct", "", "sk", "", "", logger)
		assert.Nil(t, d)
		assert.ErrorContains(t, err, "credentials")
	})
	t.Run("rejects unknown jurisdiction", func(t *testing.T) {
		d, err := NewR2Driver("acct", "ak", "sk", "moon", "", logger)
		assert.Nil(t, d)
		assert.ErrorContains(t, err, "jurisdiction")
	})
	t.Run("defaults bucket and jurisdiction", func(t *testing.T) {
		d, err := NewR2Driver("acct", "ak", "sk", "", "", logger)
		require.NoError(t, err)
		assert.Equal(t, "r2", d.Name())
		assert.Equal(t, R2DefaultBucket, d.bucket)
		assert.Equal(t, "https://acct.r2.cloudflarestorage.com", d.endpoint)
		assert.Equal(t, "default", d.jurisdiction)
	})
	t.Run("honours explicit bucket and jurisdiction", func(t *testing.T) {
		d, err := NewR2Driver("acct", "ak", "sk", "eu", "my-public", logger)
		require.NoError(t, err)
		assert.Equal(t, "my-public", d.bucket)
		assert.Equal(t, "https://acct.eu.r2.cloudflarestorage.com", d.endpoint)
	})
}

// Keys use the same tenant-prefixed layout as iDrive/Geyser so a public
// object's R2 key is derivable from (tenant, container, artifact) alone —
// that is what a future presigned-302 / custom-domain serve path needs.
func TestR2Driver_BuildKey(t *testing.T) {
	d, err := NewR2Driver("acct", "ak", "sk", "", "", zap.NewNop())
	require.NoError(t, err)

	assert.Equal(t, "t-tenant-1/tenant-1_photos/2026/a.jpg",
		d.buildKey("tenant-1", "tenant-1_photos", "2026/a.jpg"))

	ctx := common.WithTenantID(context.Background(), "tenant-9")
	assert.Equal(t, "tenant-9", d.getTenantID(ctx))
	assert.Equal(t, "default", d.getTenantID(context.Background()))
}

func TestR2Driver_ImplementsEngineInterfaces(t *testing.T) {
	d, err := NewR2Driver("acct", "ak", "sk", "", "", zap.NewNop())
	require.NoError(t, err)
	var _ engine.Driver = d
	var _ engine.RangeGetter = d
}

// Live round-trip against the real account. Skipped without R2_* env vars
// (CI); run locally with the Keychain creds exported. This is the honest
// verification that the SDK defaults (checksums, path-style, region "auto")
// actually work against R2 — R2 has rejected SDK default checksum modes
// before.
func TestR2Driver_LiveRoundTrip(t *testing.T) {
	acct := os.Getenv("R2_ACCOUNT_ID")
	ak := os.Getenv("R2_ACCESS_KEY")
	sk := os.Getenv("R2_SECRET_KEY")
	if acct == "" || ak == "" || sk == "" {
		t.Skip("R2_ACCOUNT_ID/R2_ACCESS_KEY/R2_SECRET_KEY not set — skipping live R2 test")
	}
	if testing.Short() {
		t.Skip("live R2 round-trip skipped in -short mode")
	}

	d, err := NewR2Driver(acct, ak, sk, os.Getenv("R2_JURISDICTION"), os.Getenv("R2_BUCKET"), zap.NewNop())
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(common.WithTenantID(context.Background(), "r2-test"), 2*time.Minute)
	defer cancel()

	require.NoError(t, d.HealthCheck(ctx), "HeadBucket on the configured public bucket")

	container := fmt.Sprintf("r2-test_%d", time.Now().UnixNano())
	small := []byte("hello from r2 " + time.Now().String())
	big := make([]byte, 20<<20) // crosses the 16 MiB part size → multipart path
	_, err = rand.Read(big)
	require.NoError(t, err)

	t.Cleanup(func() {
		_ = d.Delete(context.Background(), container, "small.txt")
		_ = d.Delete(context.Background(), container, "sub/big.bin")
	})

	// Put with known length (the API adapter always passes it) and unknown.
	require.NoError(t, d.Put(ctx, container, "small.txt", bytes.NewReader(small),
		engine.WithContentLength(int64(len(small))), engine.WithContentType("text/plain")))
	require.NoError(t, d.Put(ctx, container, "sub/big.bin", bytes.NewReader(big)))

	ok, err := d.Exists(ctx, container, "small.txt")
	require.NoError(t, err)
	assert.True(t, ok)
	ok, err = d.Exists(ctx, container, "nope.txt")
	require.NoError(t, err)
	assert.False(t, ok)

	rc, err := d.Get(ctx, container, "small.txt")
	require.NoError(t, err)
	got, err := io.ReadAll(rc)
	_ = rc.Close()
	require.NoError(t, err)
	assert.Equal(t, small, got)

	rc, err = d.Get(ctx, container, "sub/big.bin")
	require.NoError(t, err)
	gotBig, err := io.ReadAll(rc)
	_ = rc.Close()
	require.NoError(t, err)
	assert.True(t, bytes.Equal(big, gotBig), "20 MiB multipart round-trip must be byte-identical")

	rc, err = d.GetRange(ctx, container, "sub/big.bin", 1024, 16)
	require.NoError(t, err)
	part, err := io.ReadAll(rc)
	_ = rc.Close()
	require.NoError(t, err)
	assert.Equal(t, big[1024:1040], part)

	keys, err := d.List(ctx, container, "")
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"small.txt", "sub/big.bin"}, keys, "List strips the tenant/container prefix")
	keys, err = d.List(ctx, container, "sub/")
	require.NoError(t, err)
	assert.Equal(t, []string{"sub/big.bin"}, keys)

	require.NoError(t, d.Delete(ctx, container, "small.txt"))
	ok, err = d.Exists(ctx, container, "small.txt")
	require.NoError(t, err)
	assert.False(t, ok)

	_, err = d.Get(ctx, container, "small.txt")
	assert.Error(t, err, "Get of a deleted object must error, never return an empty body")
}
