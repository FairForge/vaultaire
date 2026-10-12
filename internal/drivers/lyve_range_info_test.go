package drivers

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/FairForge/vaultaire/internal/common"
	"github.com/FairForge/vaultaire/internal/engine"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// The parallel ranged GET (parallel_get) covered idrive/wasabi only: on the
// Lyve primary every multipart-uploaded object read back single-stream
// (38–70 MB/s vs 124 chunked, bench 2026-10-12 finding 1). Lyve reports the
// same ETag + total size per range so the reader can check them.

func newTestLyveDriver(srvURL string) *LyveDriver {
	client := s3.New(s3.Options{
		BaseEndpoint: aws.String(srvURL), Region: "us-west-1",
		Credentials:  credentials.NewStaticCredentialsProvider("ak", "sk", ""),
		UsePathStyle: true, Retryer: aws.NopRetryer{},
	})
	return &LyveDriver{client: client, region: "us-west-1", logger: zap.NewNop()}
}

func TestLyveGetRangeInfo_ReportsETagAndTotalSize(t *testing.T) {
	// Arrange
	f := &fakeRangeServer{body: []byte(strings.Repeat("0123456789", 100)), etag: "lyve-etag-3"}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	d := newTestLyveDriver(srv.URL)
	var _ engine.VersionedRangeGetter = d
	ctx := common.WithTenantID(context.Background(), "tnt")

	// Act
	rc, info, err := d.GetRangeInfo(ctx, "tnt_b", "k.bin", 100, 50)

	// Assert
	require.NoError(t, err)
	defer func() { _ = rc.Close() }()
	got, err := io.ReadAll(rc)
	require.NoError(t, err)
	assert.Equal(t, f.body[100:150], got)
	assert.Equal(t, "lyve-etag-3", info.ETag, "quotes stripped")
	assert.Equal(t, int64(1000), info.Size, "total size from Content-Range")
	assert.Equal(t, "/stored-us-west-1/t-tnt/tnt_b/k.bin", f.keys[0], "the region's bucket, the tenant-prefixed key")
}

func TestLyveGetRangeInfo_RefusesAContextWithoutTenant(t *testing.T) {
	d := &LyveDriver{region: "us-west-1", logger: zap.NewNop()}
	_, _, err := d.GetRangeInfo(context.Background(), "c", "a", 0, 10)
	require.ErrorIs(t, err, ErrNoTenant)
}

func TestLyveGetRangeInfo_RefusesAnEmptyRange(t *testing.T) {
	d := &LyveDriver{region: "us-west-1", logger: zap.NewNop()}
	_, _, err := d.GetRangeInfo(common.WithTenantID(context.Background(), "tnt"), "c", "a", 0, 0)
	require.ErrorIs(t, err, engine.ErrInvalidInput)
}

func TestLyveGetRangeInfo_AMissIsAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`<Error><Code>NoSuchKey</Code></Error>`))
	}))
	t.Cleanup(srv.Close)
	d := newTestLyveDriver(srv.URL)
	rc, _, err := d.GetRangeInfo(common.WithTenantID(context.Background(), "tnt"), "c", "gone", 0, 10)
	require.Error(t, err)
	assert.Nil(t, rc)
	assert.True(t, s3IsNotFound(err), "a NoSuchKey is classified as a miss: %v", err)
}
