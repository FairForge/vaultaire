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

	"github.com/FairForge/vaultaire/internal/common"
)

// Review R7-15: R2 is one fixed bucket shared by every tenant under
// t-<tenant>/<container>/<artifact>. S3 keys are opaque strings, so the only
// way a crafted key could leave its tenant prefix is if something between the
// driver and the wire NORMALISED dot segments. This captures the raw request
// path the SDK + Go HTTP client actually send.
func TestR2Driver_KeyDotSegmentsAreSentLiterally(t *testing.T) {
	var mu sync.Mutex
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.URL.EscapedPath())
		mu.Unlock()
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(s3ErrXML("NoSuchKey", "The specified key does not exist.")))
	}))
	t.Cleanup(srv.Close)
	client := s3.New(s3.Options{
		BaseEndpoint: aws.String(srv.URL),
		Region:       "auto",
		Credentials:  credentials.NewStaticCredentialsProvider("ak", "sk", ""),
		UsePathStyle: true,
		Retryer:      aws.NopRetryer{},
	})
	d := &R2Driver{client: client, bucket: R2DefaultBucket, logger: zap.NewNop()}

	ctxA := common.WithTenantID(context.Background(), "tenantA")
	_, err := d.Get(ctxA, "c", "x/../../t-tenantB/c/secret")
	require.Error(t, err)
	assert.True(t, s3IsNotFound(err), "R2's answer for such a key is a plain miss: %v", err)

	_, err = d.Get(ctxA, "c", "/leading/slash")
	require.Error(t, err)

	// No tenant in ctx → refused before any request (WP-R8-7); it used to
	// be sent under the shared "default" prefix (R6-22).
	_, err = d.Get(context.Background(), "c", "k")
	require.ErrorIs(t, err, ErrNoTenant)

	mu.Lock()
	defer mu.Unlock()
	require.Len(t, paths, 2)
	assert.Equal(t, "/"+R2DefaultBucket+"/t-tenantA/c/x/../../t-tenantB/c/secret", paths[0],
		"dot segments travel literally — nothing normalises the key toward another tenant's prefix")
	assert.Equal(t, "/"+R2DefaultBucket+"/t-tenantA/c//leading/slash", paths[1])
}
