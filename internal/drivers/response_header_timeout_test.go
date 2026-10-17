package drivers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/FairForge/vaultaire/internal/common"
)

// hangingS3 never answers; it records how long each request was held before
// the client gave it up.
type hangingS3 struct {
	srv      *httptest.Server
	mu       sync.Mutex
	lives    []time.Duration
	arrivals atomic.Int32 // requests received (lives is written only as each ends)
}

func newHangingS3(t *testing.T) *hangingS3 {
	t.Helper()
	h := &hangingS3{}
	release := make(chan struct{})
	h.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.arrivals.Add(1)
		start := time.Now()
		select {
		case <-release:
		case <-r.Context().Done():
		}
		h.mu.Lock()
		h.lives = append(h.lives, time.Since(start))
		h.mu.Unlock()
	}))
	t.Cleanup(func() { close(release); h.srv.Close() })
	return h
}

// assertEachRequestBounded: every attempt was given up near the header
// timeout, whatever the SDK's retry backoff added between them — the bound
// is per request, not on the sum (Prompt 2b.5 F3.10: a 5 s bound on the
// whole call failed at 5.41 s under the full -race run).
func (h *hangingS3) assertEachRequestBounded(ctx context.Context, t *testing.T) {
	t.Helper()
	require.NoError(t, ctx.Err(), "the call ended before the caller's deadline")
	h.mu.Lock()
	defer h.mu.Unlock()
	require.NotEmpty(t, h.lives)
	for i, d := range h.lives {
		assert.Less(t, d, 2*time.Second, "attempt %d held %s", i, d)
	}
}

func shortHeaderTimeout(t *testing.T) {
	old := s3OpTimeouts
	s3OpTimeouts = OpTimeouts{Meta: 200 * time.Millisecond, Get: 200 * time.Millisecond, Put: 200 * time.Millisecond, Complete: 10 * time.Minute}
	t.Cleanup(func() { s3OpTimeouts = old })
}

// A fixed-bucket GET whose response headers never come must fail in bounded
// time (2b.4 E2.1): the client had no ResponseHeaderTimeout and no Timeout,
// so a hung iDrive GET lasted until TCP gave up — and a parfetch part hung
// on it held the whole ordered download.
func TestFixedBucketDriver_GetWithNoResponseHeadersFailsInBoundedTime(t *testing.T) {
	shortHeaderTimeout(t)
	h := newHangingS3(t)
	d, err := NewFixedBucketS3Driver("wasabi", "ak", "sk", h.srv.URL, "us-west-1", "b", zap.NewNop())
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(common.WithTenantID(context.Background(), "t1"), 30*time.Second)
	defer cancel()

	_, err = d.Get(ctx, "c", "k")

	require.Error(t, err)
	h.assertEachRequestBounded(ctx, t)
}

// Prompt 2b.5 F3.4: the (then 60 s) bound landed on the fixed-bucket drivers only;
// lyve (prod's primary since 2026-10-11) built TunedHTTPClient(WithHTTP1Only())
// and a hung GET returned only at the caller's deadline (before: 3.0 s of a
// 3 s deadline, no header timeout on its transport).
func TestLyveDriver_GetWithNoResponseHeadersFailsInBoundedTime(t *testing.T) {
	shortHeaderTimeout(t)
	h := newHangingS3(t)
	// NewLyveDriver's client, the endpoint pointed at the hanging server.
	cfg, err := config.LoadDefaultConfig(context.Background(),
		config.WithCredentialsProvider(credentials.NewStaticCredentialsProvider("ak", "sk", "")),
		config.WithRegion("us-west-1"),
		config.WithHTTPClient(s3ObjectHTTPClient()),
	)
	require.NoError(t, err)
	d := &LyveDriver{
		client: s3.NewFromConfig(cfg, func(o *s3.Options) { o.BaseEndpoint = aws.String(h.srv.URL); o.UsePathStyle = true }),
		region: "us-west-1", logger: zap.NewNop(),
	}
	// …and the real constructor uses that client.
	real, err := NewLyveDriver("ak", "sk", "", "us-west-1", zap.NewNop())
	require.NoError(t, err)
	hc, ok := real.client.Options().HTTPClient.(*http.Client)
	require.True(t, ok)
	split, ok := hc.Transport.(*opSplitTransport)
	require.True(t, ok, "lyve's transport: %T", hc.Transport)
	assert.Equal(t, 200*time.Millisecond, split.byClass[classGet].ResponseHeaderTimeout)
	assert.Equal(t, 10*time.Minute, split.byClass[classComplete].ResponseHeaderTimeout)
	ctx, cancel := context.WithTimeout(common.WithTenantID(context.Background(), "t1"), 30*time.Second)
	defer cancel()

	_, err = d.Get(ctx, "c", "k")

	require.Error(t, err)
	h.assertEachRequestBounded(ctx, t)
}

// Prompt 2b.5 F3.8: CompleteMultipartUpload (POST ?uploadId=) has its own,
// longer header timeout. A header timeout is retried by the SDK; a Complete
// that took longer than 60 s was re-sent and the second answered
// NoSuchUpload after the first had committed.
func TestTunedHTTPClient_CompleteMultipartHasItsOwnHeaderTimeout(t *testing.T) {
	var mu sync.Mutex
	calls := map[string]int{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls[r.Method]++
		mu.Unlock()
		time.Sleep(300 * time.Millisecond) // the vendor assembling the object
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	c := TunedHTTPClient(WithHTTP1Only(), WithOpTimeouts(OpTimeouts{
		Meta: 100 * time.Millisecond, Get: 100 * time.Millisecond, Put: 100 * time.Millisecond, Complete: 5 * time.Second}))

	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/b/k?uploadId=u1", nil)
	resp, err := c.Do(req)
	require.NoError(t, err, "the Complete waits for its answer")
	_ = resp.Body.Close()

	req, _ = http.NewRequest(http.MethodGet, srv.URL+"/b/k", nil)
	_, err = c.Do(req)
	require.Error(t, err, "everything else keeps the short bound")
	req, _ = http.NewRequest(http.MethodPost, srv.URL+"/b?delete", nil)
	_, err = c.Do(req)
	require.Error(t, err, "DeleteObjects is not a Complete")
}
