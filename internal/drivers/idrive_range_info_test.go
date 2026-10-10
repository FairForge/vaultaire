package drivers

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
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

// The parallel ranged GET (internal/api, parallel_get flag) reads one object
// as many ranges and must prove they all came from the same object: every
// range reports the object's ETag and total size.

type fakeRangeServer struct {
	body []byte
	etag string
	keys []string
}

func (f *fakeRangeServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.keys = append(f.keys, r.URL.Path)
	rh := r.Header.Get("Range")
	var start, end int
	if _, err := fmt.Sscanf(rh, "bytes=%d-%d", &start, &end); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	if end >= len(f.body) {
		end = len(f.body) - 1
	}
	w.Header().Set("ETag", `"`+f.etag+`"`)
	w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(f.body)))
	w.Header().Set("Content-Length", strconv.Itoa(end-start+1))
	w.WriteHeader(http.StatusPartialContent)
	_, _ = w.Write(f.body[start : end+1])
}

func TestIDriveGetRangeInfo_ReportsETagAndTotalSize(t *testing.T) {
	// Arrange
	f := &fakeRangeServer{body: []byte(strings.Repeat("0123456789", 100)), etag: "abc123-4"}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	client := s3.New(s3.Options{
		BaseEndpoint: aws.String(srv.URL), Region: "us-west-2",
		Credentials:  credentials.NewStaticCredentialsProvider("ak", "sk", ""),
		UsePathStyle: true, Retryer: aws.NopRetryer{},
	})
	d := &IDriveDriver{name: "idrive", client: client, bucket: "vaultaire", region: "us-west-2", logger: zap.NewNop()}
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
	assert.Equal(t, "abc123-4", info.ETag, "quotes stripped")
	assert.Equal(t, int64(1000), info.Size, "total size from Content-Range")
	assert.Equal(t, "/vaultaire/t-tnt/tnt_b/k.bin", f.keys[0], "the tenant-prefixed key")
}

func TestIDriveGetRangeInfo_RefusesAContextWithoutTenant(t *testing.T) {
	d := &IDriveDriver{name: "idrive", bucket: "vaultaire", logger: zap.NewNop()}
	_, _, err := d.GetRangeInfo(context.Background(), "c", "a", 0, 10)
	require.ErrorIs(t, err, ErrNoTenant)
}
