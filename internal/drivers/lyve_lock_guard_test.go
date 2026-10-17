package drivers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	promtest "github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/FairForge/vaultaire/internal/engine"
)

// WP-R7-7 (R7-16): Lyve is the only general-purpose fail-over leg — a
// fail-over PUT lands in stored-<region>. With Object Lock (or a default
// retention) on that bucket every such write would be undeletable until the
// retention lapsed: the delete answers AccessDenied, an erasure request
// cannot be honoured. At boot the driver reads the bucket's lock
// configuration; enabled = target-only (engine.TargetOnlyDeclarer) and
// vaultaire_lyve_bucket_lock_enabled{region} 1. Prod 2026-10-17: both
// stored-* buckets answer ObjectLockConfigurationNotFoundError.

func lockFake(t *testing.T, status int, body string) *LyveDriver {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || !r.URL.Query().Has("object-lock") || r.URL.Path != "/stored-us-east-1" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	client := s3.New(s3.Options{
		BaseEndpoint: aws.String(srv.URL), Region: "us-east-1",
		Credentials:  credentials.NewStaticCredentialsProvider("ak", "sk", ""),
		UsePathStyle: true, Retryer: aws.NopRetryer{},
	})
	return &LyveDriver{client: client, region: "us-east-1", logger: zap.NewNop()}
}

const lockNotFound = `<?xml version="1.0"?><Error><Code>ObjectLockConfigurationNotFoundError</Code><Message>Object Lock configuration does not exist for this bucket</Message></Error>`

func lockGauge() float64 {
	return promtest.ToFloat64(lyveBucketLockEnabled.WithLabelValues("us-east-1"))
}

func TestLyveLockGuard_NoConfigurationIsNoLock(t *testing.T) {
	d := lockFake(t, http.StatusNotFound, lockNotFound)

	locked, err := d.CheckBucketLock(context.Background())

	require.NoError(t, err)
	assert.False(t, locked)
	assert.False(t, engine.IsDeclaredTargetOnly(d))
	assert.Equal(t, 0.0, lockGauge())
}

func TestLyveLockGuard_ObjectLockEnabledMakesItTargetOnly(t *testing.T) {
	d := lockFake(t, http.StatusOK, `<?xml version="1.0"?><ObjectLockConfiguration><ObjectLockEnabled>Enabled</ObjectLockEnabled></ObjectLockConfiguration>`)

	locked, err := d.CheckBucketLock(context.Background())

	require.NoError(t, err)
	assert.True(t, locked)
	assert.True(t, engine.IsDeclaredTargetOnly(d), "never a fail-over destination, never the primary")
	assert.Equal(t, 1.0, lockGauge())
}

func TestLyveLockGuard_ADefaultRetentionMakesItTargetOnly(t *testing.T) {
	d := lockFake(t, http.StatusOK, `<?xml version="1.0"?><ObjectLockConfiguration><ObjectLockEnabled>Enabled</ObjectLockEnabled><Rule><DefaultRetention><Mode>GOVERNANCE</Mode><Days>1</Days></DefaultRetention></Rule></ObjectLockConfiguration>`)

	locked, err := d.CheckBucketLock(context.Background())

	require.NoError(t, err)
	assert.True(t, locked)
	assert.True(t, engine.IsDeclaredTargetOnly(d))
}

// The scoped service user may not be allowed to read the configuration:
// a Warn at boot and no guard, never a failed boot.
func TestLyveLockGuard_AccessDeniedIsUnreadableNotLocked(t *testing.T) {
	d := lockFake(t, http.StatusForbidden, `<?xml version="1.0"?><Error><Code>AccessDenied</Code><Message>Access Denied</Message></Error>`)

	locked, err := d.CheckBucketLock(context.Background())

	require.ErrorIs(t, err, ErrLyveLockUnreadable)
	assert.False(t, locked)
	assert.False(t, engine.IsDeclaredTargetOnly(d))
}

func TestLyveLockGuard_AServerErrorIsAnErrorNotADecision(t *testing.T) {
	d := lockFake(t, http.StatusInternalServerError, `<?xml version="1.0"?><Error><Code>InternalError</Code><Message>boom</Message></Error>`)

	locked, err := d.CheckBucketLock(context.Background())

	require.Error(t, err)
	assert.NotErrorIs(t, err, ErrLyveLockUnreadable)
	assert.False(t, locked)
	assert.False(t, engine.IsDeclaredTargetOnly(d))
}
