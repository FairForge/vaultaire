package drivers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// WP-R7-1: an idrive-<region> driver provisions its fixed bucket at boot when
// the region has none (the reseller account had no `vaultaire` bucket outside
// the primary region — live, Review R7-01). A wrong key or an outage must
// surface as an error, never as a CreateBucket attempt.

type fakeBucketServer struct {
	mu       sync.Mutex
	exists   bool
	headCode int // 0 = derive from exists
	creates  int
}

func (f *fakeBucketServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch r.Method {
	case http.MethodHead:
		if f.headCode != 0 {
			w.WriteHeader(f.headCode)
			return
		}
		if f.exists {
			w.WriteHeader(http.StatusOK)
		} else {
			w.WriteHeader(http.StatusNotFound)
		}
	case http.MethodPut:
		f.creates++
		f.exists = true
		w.WriteHeader(http.StatusOK)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func newBucketTestDriver(t *testing.T, f *fakeBucketServer) *IDriveDriver {
	t.Helper()
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	client := s3.New(s3.Options{
		BaseEndpoint: aws.String(srv.URL),
		Region:       "us-west-2",
		Credentials:  credentials.NewStaticCredentialsProvider("ak", "sk", ""),
		UsePathStyle: true,
		Retryer:      aws.NopRetryer{},
	})
	return &IDriveDriver{client: client, bucket: "vaultaire", region: "us-west-2", logger: zap.NewNop()}
}

func TestIDriveEnsureBucket_CreatesWhenAbsent(t *testing.T) {
	f := &fakeBucketServer{exists: false}
	d := newBucketTestDriver(t, f)

	created, err := d.EnsureBucket(context.Background())
	require.NoError(t, err)
	assert.True(t, created)
	assert.Equal(t, 1, f.creates)

	created, err = d.EnsureBucket(context.Background())
	require.NoError(t, err)
	assert.False(t, created, "second boot: bucket exists, nothing to do")
	assert.Equal(t, 1, f.creates)
	require.NoError(t, d.HealthCheck(context.Background()), "the probe passes once the bucket exists")
}

func TestIDriveEnsureBucket_WrongKeyIsAnErrorNotACreate(t *testing.T) {
	f := &fakeBucketServer{headCode: http.StatusForbidden}
	d := newBucketTestDriver(t, f)

	created, err := d.EnsureBucket(context.Background())
	require.Error(t, err, "403 = the primary pair used in another region (R7-01)")
	assert.False(t, created)
	assert.Equal(t, 0, f.creates, "never try to create a bucket the key cannot see")
	assert.Contains(t, err.Error(), "us-west-2")
}

func TestIDriveEnsureBucket_OutageIsAnError(t *testing.T) {
	f := &fakeBucketServer{headCode: http.StatusServiceUnavailable}
	d := newBucketTestDriver(t, f)

	created, err := d.EnsureBucket(context.Background())
	require.Error(t, err)
	assert.False(t, created)
	assert.Equal(t, 0, f.creates)
}
