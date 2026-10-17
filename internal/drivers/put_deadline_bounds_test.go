package drivers

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsmiddleware "github.com/aws/aws-sdk-go-v2/aws/middleware"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go/middleware"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/FairForge/vaultaire/internal/engine"
)

// Stage 1a Prompt 9, Part 0.3 (the WP-P8 post-merge probe): the deadline
// each byte-carrying request actually carries, read off its context by an
// SDK middleware — no test waits the 15 s it proves. #742 gave a body under
// 64 MiB the base scaled by its size with a 10 s floor; the probe checks it
// end to end through Put, on the Lyve driver and the fixed-bucket driver,
// at the default FIXED_BUCKET_PUT_TIMEOUT: a 16 MiB part (s3UploadPartSize)
// 15 s, the last 8 MiB part and a 2 MiB chunk 10 s. The sync bridge keeps
// its own rule (webdavPutDeadline: 2 min per started 64 MiB) — a bridge
// answers a PUT only after its own upload to Sync's cloud.

type deadlineRecord struct {
	op        string
	remaining time.Duration
	has       bool
}

type deadlineRecorder struct {
	mu   sync.Mutex
	seen []deadlineRecord
}

// option is an s3.Options hook that records, at the Initialize step of every
// operation, its name and how long its context has left.
func (r *deadlineRecorder) option(o *s3.Options) {
	o.APIOptions = append(o.APIOptions, func(stack *middleware.Stack) error {
		return stack.Initialize.Add(middleware.InitializeMiddlewareFunc("p9-deadline-recorder",
			func(ctx context.Context, in middleware.InitializeInput, next middleware.InitializeHandler) (middleware.InitializeOutput, middleware.Metadata, error) {
				rec := deadlineRecord{op: awsmiddleware.GetOperationName(ctx)}
				if dl, ok := ctx.Deadline(); ok {
					rec.remaining, rec.has = time.Until(dl), true
				}
				r.mu.Lock()
				r.seen = append(r.seen, rec)
				r.mu.Unlock()
				return next.HandleInitialize(ctx, in)
			}), middleware.Before)
	})
}

func (r *deadlineRecorder) of(op string) []deadlineRecord {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []deadlineRecord
	for _, s := range r.seen {
		if s.op == op {
			out = append(out, s)
		}
	}
	return out
}

// near asserts a recorded deadline is want, give or take the time the
// request took to reach the middleware.
func near(t *testing.T, want time.Duration, rec deadlineRecord, msg string) {
	t.Helper()
	require.True(t, rec.has, "%s: %s carries no deadline", msg, rec.op)
	assert.InDelta(t, want.Seconds(), rec.remaining.Seconds(), 1.0, "%s: %s", msg, rec.op)
}

func recordingS3Client(t *testing.T, url string, rec *deadlineRecorder) *s3.Client {
	t.Helper()
	return s3.New(s3.Options{
		BaseEndpoint: aws.String(url), Region: "us-west-1",
		Credentials:  credentials.NewStaticCredentialsProvider("ak", "sk", ""),
		UsePathStyle: true, Retryer: aws.NopRetryer{},
	}, rec.option)
}

// putShapes uploads the three shapes through put and checks each request's
// deadline: a 40 MiB body (parts of 16, 16 and 8 MiB) and a 2 MiB chunk.
func putShapes(t *testing.T, rec *deadlineRecorder, put func(key string, body []byte) error) {
	t.Helper()
	big := randomBody(t, 40<<20)
	require.NoError(t, put("big.bin", big))
	parts := rec.of("UploadPart")
	require.Len(t, parts, 3, "40 MiB = parts of 16 + 16 + 8 MiB")
	var fifteen, ten int
	for _, p := range parts {
		require.True(t, p.has)
		switch {
		case p.remaining > 14*time.Second && p.remaining <= 15*time.Second:
			fifteen++
		case p.remaining > 9*time.Second && p.remaining <= 10*time.Second:
			ten++
		}
	}
	assert.Equal(t, 2, fifteen, "a 16 MiB part gets 15 s: %+v", parts)
	assert.Equal(t, 1, ten, "the last 8 MiB part gets the 10 s floor: %+v", parts)
	for _, op := range []string{"CreateMultipartUpload", "CompleteMultipartUpload"} {
		for _, r := range rec.of(op) {
			assert.False(t, r.has, "%s is bookkeeping: no PUT deadline of ours", op)
		}
	}

	chunk := randomBody(t, 2<<20)
	require.NoError(t, put("chunk.bin", chunk))
	puts := rec.of("PutObject")
	require.Len(t, puts, 1)
	near(t, 10*time.Second, puts[0], "a 2 MiB chunk")
}

func TestLyveUploadPart_ASixteenMiBPartGetsFifteenSecondsAtTheDefault(t *testing.T) {
	t.Setenv("FIXED_BUCKET_PUT_TIMEOUT", "")
	srv := httptest.NewServer(newStallS3())
	t.Cleanup(srv.Close)
	rec := &deadlineRecorder{}
	d := &LyveDriver{client: recordingS3Client(t, srv.URL, rec), region: "us-west-1", logger: zap.NewNop()}
	d.setPutTimeout(fixedBucketPutTimeoutFromEnv(zap.NewNop()))
	require.Equal(t, 60*time.Second, d.putTimeout)

	putShapes(t, rec, func(key string, body []byte) error {
		return d.Put(tctx(), "c", key, onlyReader{bytes.NewReader(body)}, engine.WithContentLength(int64(len(body))))
	})
}

func TestFixedBucketUploadPart_ASixteenMiBPartGetsFifteenSecondsAtTheDefault(t *testing.T) {
	t.Setenv("FIXED_BUCKET_PUT_TIMEOUT", "")
	srv := httptest.NewServer(newStallS3())
	t.Cleanup(srv.Close)
	d, err := NewFixedBucketS3Driver("idrive", "ak", "sk", srv.URL, "us-west-1", "b", zap.NewNop())
	require.NoError(t, err)
	require.Equal(t, 60*time.Second, d.putTimeout)
	rec := &deadlineRecorder{}
	d.client = recordingS3Client(t, srv.URL, rec)
	d.setPutTimeout(d.putTimeout)

	putShapes(t, rec, func(key string, body []byte) error {
		return d.Put(tctx(), "c", key, onlyReader{bytes.NewReader(body)}, engine.WithContentLength(int64(len(body))))
	})
}

// The sync bridge keeps putDeadline's whole base for a small body. Scaled
// down here so the test is fast: base 2 s, the S3 floor lowered to 100 ms.
// Under s3PutDeadline a 2 MiB PUT would get 100 ms and be cut — a second
// attempt; under the bridge's rule it gets the whole 2 s and the first
// attempt's answer, 700 ms late, is taken.
func TestSyncPut_ASmallPutKeepsTheBridgesWholeBase(t *testing.T) {
	prev := putDeadlineFloor
	putDeadlineFloor = 100 * time.Millisecond
	t.Cleanup(func() { putDeadlineFloor = prev })
	assert.Equal(t, 2*time.Minute, webdavPutDeadline(WebDAVDefaultPutTimeout, 2<<20), "the default: 2 min for a 2 MiB PUT")
	assert.Equal(t, 100*time.Millisecond, s3PutDeadline(2*time.Second, 2<<20), "what an S3 PUT of the same size would get")

	var mu sync.Mutex
	puts := 0
	f := newDAVFixture(t, func(h http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodPut {
				body, _ := io.ReadAll(r.Body)
				mu.Lock()
				puts++
				mu.Unlock()
				select { // the bridge uploading to Sync's cloud before it answers
				case <-r.Context().Done():
					return
				case <-time.After(700 * time.Millisecond):
				}
				r.Body = io.NopCloser(bytes.NewReader(body))
			}
			h.ServeHTTP(w, r)
		})
	}, WithWebDAVIdleTimeout(time.Hour), WithWebDAVPutTimeout(2*time.Second), fastRetries)

	body := randomBody(t, 2<<20)
	ctx := davCtx("tenant-a")
	require.NoError(t, f.drv.Put(ctx, "c", "k", onlyReader{bytes.NewReader(body)}, engine.WithContentLength(int64(len(body)))))
	mu.Lock()
	assert.Equal(t, 1, puts, "one attempt: the bridge's PUT is not cut at the S3 floor")
	mu.Unlock()
	assert.True(t, bytes.Equal(body, readAllClose(t, mustGet(ctx, t, f.drv, "c", "k"))))
}
