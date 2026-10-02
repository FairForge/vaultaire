package drivers

import (
	"bytes"
	"context"
	"testing"

	"github.com/FairForge/vaultaire/internal/common"
	"github.com/FairForge/vaultaire/internal/engine"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// WP-R8-7: a fixed-bucket driver keys `t-<tenant>/<container>/<artifact>`
// with the tenant from the CONTEXT of the call. A call that named no tenant
// used to be filed under "default" — a read that misses an object that
// exists, a write nothing will find, a delete that "succeeds" on a key that
// was never there. It is refused before any request leaves.

// tenantCtx is a call made for a tenant, as every product call is.
func tenantCtx() context.Context {
	return common.WithTenantID(context.Background(), "tenant-test")
}

// fixedBucketCases are the S3-class drivers that key by the context tenant.
func fixedBucketCases() []s3WalkCase {
	var out []s3WalkCase
	for _, tc := range s3WalkCases() {
		switch tc.name {
		case "idrive", "lyve", "geyser", "r2":
			out = append(out, tc)
		}
	}
	return out
}

func TestFixedBucketDrivers_RefuseACallWithoutATenant(t *testing.T) {
	for _, tc := range fixedBucketCases() {
		t.Run(tc.name, func(t *testing.T) {
			// Arrange: an object under t-default/ — what the fallback would
			// have addressed — so a call that reached the backend would find it.
			fake := newWalkS3(100, "t-default/c/k")
			d := tc.build(walkClient(t, fake))
			ctx := context.Background()
			before := testutil.ToFloat64(callsWithoutTenant.WithLabelValues(d.Name()))

			// Act
			_, getErr := d.Get(ctx, "c", "k")
			putErr := d.Put(ctx, "c", "k", bytes.NewReader([]byte("x")), engine.WithContentLength(1))
			delErr := d.Delete(ctx, "c", "k")
			_, exErr := d.Exists(ctx, "c", "k")
			_, listErr := d.List(ctx, "c", "")
			errs := []error{getErr, putErr, delErr, exErr, listErr}
			if rg, ok := d.(engine.RangeGetter); ok {
				_, rangeErr := rg.GetRange(ctx, "c", "k", 0, 1)
				errs = append(errs, rangeErr)
			}

			// Assert: every call is an error the caller sees, none reached
			// the backend, each is counted, and the error is the caller's
			// (ErrInvalidInput: no charge on the backend's breaker).
			for _, err := range errs {
				require.ErrorIs(t, err, ErrNoTenant)
				assert.ErrorIs(t, err, engine.ErrInvalidInput)
			}
			assert.Zero(t, fake.requests, "refused before any request")
			assert.Empty(t, fake.deleted)
			assert.Equal(t, float64(len(errs)), testutil.ToFloat64(callsWithoutTenant.WithLabelValues(d.Name()))-before)

			// With a tenant the same call goes through.
			names, err := d.List(common.WithTenantID(ctx, "default"), "c", "")
			require.NoError(t, err)
			assert.Equal(t, []string{"k"}, names)
			assert.Equal(t, 1, fake.requests)
		})
	}
}

func TestOneDrive_RefusesACallWithoutATenant(t *testing.T) {
	d, stubs := stubFleet(2, zap.NewNop())
	ctx := context.Background()

	_, getErr := d.Get(ctx, "c", "k")
	putErr := d.Put(ctx, "c", "k", bytes.NewReader([]byte("x")), engine.WithContentLength(1))
	delErr := d.Delete(ctx, "c", "k")
	_, exErr := d.Exists(ctx, "c", "k")
	_, listErr := d.List(ctx, "c", "")

	for _, err := range []error{getErr, putErr, delErr, exErr, listErr} {
		require.ErrorIs(t, err, ErrNoTenant)
	}
	for _, s := range stubs {
		assert.Empty(t, s.calls, "refused before any Graph request")
	}
}

// Tools build Lyve and Geyser drivers with a tenant of their own; the server
// passes none. The configured tenant is used only when the context has none.
func TestLyveAndGeyser_AConfiguredDefaultTenantIsStillHonoured(t *testing.T) {
	fake := newWalkS3(100, "t-bench/c/k", "t-other/c/x")
	client := walkClient(t, fake)
	for _, d := range []engine.Driver{
		&LyveDriver{client: client, region: "us-east-1", tenantID: "bench", logger: zap.NewNop()},
		&GeyserDriver{client: client, bucket: "tape", tenantID: "bench", logger: zap.NewNop()},
	} {
		names, err := d.List(context.Background(), "c", "")
		require.NoError(t, err, d.Name())
		assert.Equal(t, []string{"k"}, names, d.Name())
		// The context's tenant wins over the configured one.
		names, err = d.List(common.WithTenantID(context.Background(), "other"), "c", "")
		require.NoError(t, err, d.Name())
		assert.Equal(t, []string{"x"}, names, d.Name())
	}
}

// engine.KeyAddresser: the chunk move decides from these keys whether a
// "legacy" address is another object or the blob itself.
func TestObjectKey_DependsOnTheTenantOnlyOnFixedBucketDrivers(t *testing.T) {
	a := common.WithTenantID(context.Background(), "tenant-a")
	chunk := engine.ChunkContext(context.Background())
	var client *s3.Client

	fixed := map[string]engine.KeyAddresser{
		"idrive":     &IDriveDriver{client: client, bucket: "vaultaire"},
		"lyve":       &LyveDriver{client: client, region: "us-east-1"},
		"geyser":     &GeyserDriver{client: client, bucket: "tape"},
		"r2":         &R2Driver{client: client, bucket: "public"},
		"permafrost": &OneDriveDriver{},
	}
	for name, d := range fixed {
		assert.Equal(t, "t-tenant-a/_global/_chunks/h", d.ObjectKey(a, "_global", "_chunks/h"), name)
		assert.Equal(t, "t-_global/_global/_chunks/h", d.ObjectKey(chunk, "_global", "_chunks/h"), name)
	}

	containerKeyed := map[string]engine.KeyAddresser{
		"local":    NewLocalDriver(t.TempDir(), zap.NewNop()),
		"s3compat": &S3CompatDriver{bucket: "data", prefix: "personal-files/vaultaire"},
	}
	for name, d := range containerKeyed {
		assert.Equal(t, d.ObjectKey(chunk, "_global", "_chunks/h"), d.ObjectKey(a, "_global", "_chunks/h"),
			"%s: one object whatever the tenant — never to be 'moved'", name)
	}
}
