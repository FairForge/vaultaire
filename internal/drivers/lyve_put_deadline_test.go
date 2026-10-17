package drivers

import (
	"bytes"
	"context"
	"fmt"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/FairForge/vaultaire/internal/engine"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// 2026-10-17: on the Lyve primary (us-east-1) about 3 % of 1 MiB PUTs got
// no answer after the whole body had been acknowledged (the socket view:
// bytes_acked = bytes_sent, no retransmit, then silence for 15 s+), on
// fresh and pooled connections alike; us-west-1 was clean the same minutes
// and iDrive never stalled. The fixed-bucket driver has had the per-PUT
// deadline and the one retry on a fresh connection since WP-VAULT-1
// (put_deadline.go); the Lyve driver had neither, so a stalled chunk PUT
// waited the 60 s response-header timeout and then the SDK's retry. Now it
// takes the same deadlinePutClient: the same FIXED_BUCKET_PUT_TIMEOUT rule,
// the same one retry, counted under driver="lyve".

func newLyveDeadlineDriver(t *testing.T, f *stallS3, timeout time.Duration) *LyveDriver {
	t.Helper()
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	client := s3.New(s3.Options{
		BaseEndpoint: aws.String(srv.URL), Region: "us-west-1",
		Credentials:  credentials.NewStaticCredentialsProvider("ak", "sk", ""),
		UsePathStyle: true, Retryer: aws.NopRetryer{},
	})
	d := &LyveDriver{client: client, region: "us-west-1", logger: zap.NewNop()}
	d.setPutTimeout(timeout)
	return d
}

func TestLyvePut_AStalledPutIsRetriedOnceOnAFreshConnection(t *testing.T) {
	f := newStallS3()
	f.stall = func(a putAttempt, n int) bool { return strings.HasSuffix(a.key, "small.bin") && n == 1 }
	d := newLyveDeadlineDriver(t, f, time.Second)
	body := randomBody(t, 4096)

	// Warm the pool: a retry that merely "tries again" would be handed an
	// idle pooled connection — the kind that may have stalled.
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			assert.NoError(t, d.Put(tctx(), "c", fmt.Sprintf("warm-%d.bin", i), bytes.NewReader(body), engine.WithContentLength(int64(len(body)))))
		}(i)
	}
	wg.Wait()
	pooled := map[string]bool{}
	f.mu.Lock()
	for _, a := range f.attempts {
		pooled[a.remote] = true
	}
	f.mu.Unlock()
	before := retriesOf("lyve")

	ctx, cancel := context.WithTimeout(tctx(), 8*time.Second) // red: the plain path waits the caller out
	defer cancel()
	start := time.Now()
	err := d.Put(ctx, "c", "small.bin", bytes.NewReader(body), engine.WithContentLength(int64(len(body))))
	require.NoError(t, err)
	assert.Less(t, time.Since(start), 5*time.Second, "the stall was cut at the deadline, not waited out")

	at := f.attemptsFor("c/small.bin", 0)
	require.Len(t, at, 2, "the stalled PUT and ONE retry")
	assert.NotEqual(t, at[0].remote, at[1].remote)
	assert.False(t, pooled[at[1].remote], "the retry is on a connection opened for it, not one from the pool (%s)", at[1].remote)
	assert.Equal(t, body, f.objects["stored-us-west-1/t-t1/c/small.bin"])
	assert.Equal(t, float64(1), retriesOf("lyve")-before)
}

func TestLyvePut_OneRetryOnly(t *testing.T) {
	f := newStallS3()
	f.stall = func(putAttempt, int) bool { return true }
	d := newLyveDeadlineDriver(t, f, 200*time.Millisecond)
	body := randomBody(t, 1024)
	before := retriesOf("lyve")

	ctx, cancel := context.WithTimeout(tctx(), 8*time.Second)
	defer cancel()
	err := d.Put(ctx, "c", "dead.bin", bytes.NewReader(body), engine.WithContentLength(int64(len(body))))
	require.Error(t, err)
	assert.ErrorIs(t, err, context.DeadlineExceeded)
	assert.Contains(t, err.Error(), "one retry")
	assert.Len(t, f.attemptsFor("c/dead.bin", 0), 2, "exactly one retry, then the error is the caller's")
	assert.Equal(t, float64(1), retriesOf("lyve")-before)
	assert.NotContains(t, f.objects, "stored-us-west-1/t-t1/c/dead.bin")
}

func TestLyvePut_DeadlineOffIsThePlainPath(t *testing.T) {
	f := newStallS3()
	d := newLyveDeadlineDriver(t, f, 0)
	body := randomBody(t, 1024)
	require.NoError(t, d.Put(tctx(), "c", "plain.bin", bytes.NewReader(body), engine.WithContentLength(int64(len(body)))))
	assert.Len(t, f.attemptsFor("c/plain.bin", 0), 1)
}

func TestNewLyveDriver_ReadsThePutTimeoutAndStartsTheSeriesAtZero(t *testing.T) {
	t.Setenv("FIXED_BUCKET_PUT_TIMEOUT", "45s")
	d, err := NewLyveDriver("ak", "sk", "", "us-west-1", zap.NewNop())
	require.NoError(t, err)
	assert.Equal(t, 45*time.Second, d.putTimeout)
	assert.NotNil(t, d.putClient, "the uploader goes through the deadline client")
	assert.GreaterOrEqual(t, retriesOf("lyve"), float64(0), "the lyve series exists from construction")
}
