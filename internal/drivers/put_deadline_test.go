package drivers

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/FairForge/vaultaire/internal/common"
	"github.com/FairForge/vaultaire/internal/engine"
	"github.com/aws/aws-sdk-go-v2/feature/s3/manager"
	promtest "github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

// WP-VAULT-1 part 4: Wasabi stalled 4 % of PUTs for 9–123 s (bench
// 2026-10-04). The fixed-bucket driver gives every PutObject and every
// UploadPart a deadline (FIXED_BUCKET_PUT_TIMEOUT per 64 MiB, scaled by
// size) and ONE retry on a fresh connection — the same request again: the
// same part number under the same upload id, never a second part.
//
// stallS3 is a fake S3 that can stall chosen attempts (it reads the body and
// then says nothing until the client gives up), records the TCP connection
// of every attempt, and assembles multipart uploads so the stored bytes can
// be compared.

type putAttempt struct {
	key, remote, uploadID string
	part                  int
}

type stallS3 struct {
	mu       sync.Mutex
	attempts []putAttempt
	// stall decides whether to stall an attempt: n is the 1-based count of
	// attempts for the same (key, part).
	stall     func(a putAttempt, n int) bool
	fail500   bool
	objects   map[string][]byte
	parts     map[string]map[int][]byte // uploadID -> part -> bytes
	completes map[string][]int          // uploadID -> part numbers in the Complete body
	seen      map[string]int
	nextID    int
}

func newStallS3() *stallS3 {
	return &stallS3{objects: map[string][]byte{}, parts: map[string]map[int][]byte{}, completes: map[string][]int{}, seen: map[string]int{}}
}

func (f *stallS3) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	key := strings.TrimPrefix(r.URL.Path, "/")
	q := r.URL.Query()
	switch {
	case r.Method == http.MethodPost && q.Has("uploads"):
		f.mu.Lock()
		f.nextID++
		id := fmt.Sprintf("upload-%d", f.nextID)
		f.parts[id] = map[int][]byte{}
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/xml")
		_, _ = fmt.Fprintf(w, `<?xml version="1.0"?><InitiateMultipartUploadResult><Bucket>b</Bucket><Key>%s</Key><UploadId>%s</UploadId></InitiateMultipartUploadResult>`, key, id)
	case r.Method == http.MethodPost && q.Get("uploadId") != "":
		var body struct {
			Parts []struct {
				PartNumber int `xml:"PartNumber"`
			} `xml:"Part"`
		}
		raw, _ := io.ReadAll(r.Body)
		_ = xml.Unmarshal(raw, &body)
		id := q.Get("uploadId")
		f.mu.Lock()
		var nums []int
		var whole []byte
		for _, p := range body.Parts {
			nums = append(nums, p.PartNumber)
		}
		sort.Ints(nums)
		for _, n := range nums {
			whole = append(whole, f.parts[id][n]...)
		}
		f.completes[id] = nums
		f.objects[key] = whole
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/xml")
		_, _ = fmt.Fprintf(w, `<?xml version="1.0"?><CompleteMultipartUploadResult><Key>%s</Key><ETag>"done"</ETag></CompleteMultipartUploadResult>`, key)
	case r.Method == http.MethodDelete && q.Get("uploadId") != "":
		w.WriteHeader(http.StatusNoContent)
	case r.Method == http.MethodPut:
		a := putAttempt{key: key, remote: r.RemoteAddr, uploadID: q.Get("uploadId")}
		a.part, _ = strconv.Atoi(q.Get("partNumber"))
		data, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.attempts = append(f.attempts, a)
		id := fmt.Sprintf("%s#%d", key, a.part)
		f.seen[id]++
		n := f.seen[id]
		stall := f.stall != nil && f.stall(a, n)
		fail := f.fail500
		f.mu.Unlock()
		if stall {
			select { // say nothing until the client gives up
			case <-r.Context().Done():
			case <-time.After(10 * time.Second):
			}
			return
		}
		if fail {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`<?xml version="1.0"?><Error><Code>InternalError</Code><Message>boom</Message></Error>`))
			return
		}
		f.mu.Lock()
		if a.uploadID != "" {
			f.parts[a.uploadID][a.part] = data
		} else {
			f.objects[key] = data
		}
		f.mu.Unlock()
		w.Header().Set("ETag", `"etag"`)
		w.WriteHeader(http.StatusOK)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (f *stallS3) attemptsFor(key string, part int) []putAttempt {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []putAttempt
	for _, a := range f.attempts {
		if strings.HasSuffix(a.key, key) && a.part == part {
			out = append(out, a)
		}
	}
	return out
}

func newDeadlineDriver(t *testing.T, f *stallS3, timeout time.Duration) *IDriveDriver {
	t.Helper()
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	d, err := NewFixedBucketS3Driver("wasabi", "ak", "sk", srv.URL, "us-west-1", "b", zap.NewNop())
	require.NoError(t, err)
	d.setPutTimeout(timeout)
	return d
}

func tctx() context.Context { return common.WithTenantID(context.Background(), "t1") }

func randomBody(t *testing.T, n int) []byte {
	t.Helper()
	b := make([]byte, n)
	_, err := rand.Read(b)
	require.NoError(t, err)
	return b
}

func retriesOf(driver string) float64 {
	return promtest.ToFloat64(driverPutRetries.WithLabelValues(driver))
}

func TestFixedBucketPut_AStalledPutIsRetriedOnceOnAFreshConnection(t *testing.T) {
	f := newStallS3()
	f.stall = func(a putAttempt, n int) bool { return strings.HasSuffix(a.key, "small.bin") && n == 1 }
	d := newDeadlineDriver(t, f, time.Second) // roomy for the honest attempts on a slow runner
	body := randomBody(t, 4096)

	// Warm the pool: two PUTs in flight together leave two idle
	// connections. A retry that merely "tries again" would be handed one of
	// them — and a pooled connection is exactly what may have stalled.
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
	before := retriesOf("wasabi")

	start := time.Now()
	err := d.Put(tctx(), "c", "small.bin", bytes.NewReader(body), engine.WithContentLength(int64(len(body))))
	require.NoError(t, err)
	assert.Less(t, time.Since(start), 5*time.Second, "the stall was cut at the deadline, not waited out")

	at := f.attemptsFor("c/small.bin", 0)
	require.Len(t, at, 2, "the stalled PUT and ONE retry")
	assert.NotEqual(t, at[0].remote, at[1].remote)
	assert.False(t, pooled[at[1].remote], "the retry is on a connection opened for it, not one from the pool (%s)", at[1].remote)
	assert.Equal(t, body, f.objects["b/t-t1/c/small.bin"])
	assert.Equal(t, float64(1), retriesOf("wasabi")-before)
}

func TestFixedBucketPut_OneRetryOnly(t *testing.T) {
	f := newStallS3()
	f.stall = func(putAttempt, int) bool { return true }
	d := newDeadlineDriver(t, f, 200*time.Millisecond)
	body := randomBody(t, 1024)
	before := retriesOf("wasabi")

	err := d.Put(tctx(), "c", "dead.bin", bytes.NewReader(body), engine.WithContentLength(int64(len(body))))
	require.Error(t, err)
	assert.ErrorIs(t, err, context.DeadlineExceeded)
	assert.Contains(t, err.Error(), "one retry")
	assert.Len(t, f.attemptsFor("c/dead.bin", 0), 2, "exactly one retry, then the error is the caller's")
	assert.Equal(t, float64(1), retriesOf("wasabi")-before)
	assert.NotContains(t, f.objects, "b/t-t1/c/dead.bin")
}

func TestFixedBucketPut_AMultipartPartRetryNeverDuplicatesAPart(t *testing.T) {
	f := newStallS3()
	f.stall = func(a putAttempt, n int) bool { return a.part == 2 && n == 1 }
	// The deadline is generous on purpose: only the attempt the fake stalls
	// may pass it. (500 ms with 16 MiB parts was a flake on a slow CI runner
	// under the race detector — the honest retry of the part took longer.)
	d := newDeadlineDriver(t, f, 4*time.Second)
	// Small parts keep the test light: the driver's uploader is cached per
	// client, so seed that cache with one that cuts 5 MiB parts (the S3
	// minimum). The body's size is not declared, so it streams through the
	// uploader rather than the single-PUT shortcut.
	uploaderCache.Store(d.uploadClient(), manager.NewUploader(d.uploadClient(), func(u *manager.Uploader) { //nolint:staticcheck // same uploader the driver uses
		u.PartSize = 5 << 20
		u.Concurrency = 3
	}))
	body := randomBody(t, 12<<20) // three parts: 5 + 5 + 2 MiB
	before := retriesOf("wasabi")

	err := d.Put(tctx(), "c", "big.bin", bytes.NewReader(body))
	require.NoError(t, err)

	stored := f.objects["b/t-t1/c/big.bin"]
	require.Len(t, stored, len(body))
	assert.Equal(t, sha256.Sum256(body), sha256.Sum256(stored), "the object is the body: no part doubled, none missing")
	p2 := f.attemptsFor("c/big.bin", 2)
	require.Len(t, p2, 2, "part 2 was sent twice")
	assert.Equal(t, p2[0].uploadID, p2[1].uploadID, "the retry is the SAME part of the SAME upload")
	assert.NotEqual(t, p2[0].remote, p2[1].remote, "on a fresh connection")
	assert.Len(t, f.attemptsFor("c/big.bin", 1), 1)
	assert.Len(t, f.attemptsFor("c/big.bin", 3), 1)
	require.Len(t, f.completes, 1, "one upload, completed once")
	for _, nums := range f.completes {
		assert.Equal(t, []int{1, 2, 3}, nums, "the Complete names three parts")
	}
	assert.Equal(t, float64(1), retriesOf("wasabi")-before)
}

func TestFixedBucketPut_AClientThatLeftIsNotRetried(t *testing.T) {
	f := newStallS3()
	f.stall = func(putAttempt, int) bool { return true }
	d := newDeadlineDriver(t, f, 5*time.Second)
	body := randomBody(t, 1024)
	before := retriesOf("wasabi")
	ctx, cancel := context.WithTimeout(tctx(), 150*time.Millisecond) // the caller's own deadline

	err := d.Put(ctx, "c", "gone.bin", bytes.NewReader(body), engine.WithContentLength(int64(len(body))))
	cancel()
	require.Error(t, err)
	assert.Len(t, f.attemptsFor("c/gone.bin", 0), 1, "the caller is gone: nothing is replayed for it")
	assert.Equal(t, float64(0), retriesOf("wasabi")-before)
}

func TestFixedBucketPut_AnErrorAnswerIsNotOurRetry(t *testing.T) {
	// A 500 is an answer; the SDK's own retryer handles it. The deadline
	// retry is for silence.
	f := newStallS3()
	f.fail500 = true
	// A deadline the SDK's own three attempts and their back-off fit in
	// (as the 60 s default does): the answer arrives, it is an error.
	d := newDeadlineDriver(t, f, 30*time.Second)
	body := randomBody(t, 1024)
	before := retriesOf("wasabi")
	err := d.Put(tctx(), "c", "err.bin", bytes.NewReader(body), engine.WithContentLength(int64(len(body))))
	require.Error(t, err)
	assert.Equal(t, float64(0), retriesOf("wasabi")-before)
}

func TestFixedBucketPut_DeadlineOffIsThePlainPath(t *testing.T) {
	f := newStallS3()
	d := newDeadlineDriver(t, f, 0)
	body := randomBody(t, 2048)
	require.NoError(t, d.Put(tctx(), "c", "plain.bin", bytes.NewReader(body), engine.WithContentLength(int64(len(body)))))
	assert.Equal(t, body, f.objects["b/t-t1/c/plain.bin"])
	assert.Len(t, f.attemptsFor("c/plain.bin", 0), 1)
}

func TestPutDeadline_ScalesWithSize(t *testing.T) {
	base := 60 * time.Second
	for _, c := range []struct {
		size int64
		want time.Duration
	}{
		{0, base}, {-1, base}, {1, base}, {64 << 20, base}, {64<<20 + 1, 2 * base},
		{128 << 20, 2 * base}, {1 << 30, 16 * base}, {5 << 30, 80 * base},
	} {
		assert.Equal(t, c.want, putDeadline(base, c.size), "size %d", c.size)
	}
}

func TestFixedBucketPutTimeout_EnvRule(t *testing.T) {
	for _, c := range []struct {
		val  string
		want time.Duration
		warn bool
	}{
		{"", 60 * time.Second, false}, {"90s", 90 * time.Second, false}, {"2m", 2 * time.Minute, false},
		{"0", 0, false}, {"off", 0, false},
		{"abc", 60 * time.Second, true}, {"-5s", 60 * time.Second, true}, {"500ms", 60 * time.Second, true},
		{"2h", 60 * time.Second, true}, {"60", 60 * time.Second, true},
	} {
		t.Setenv("FIXED_BUCKET_PUT_TIMEOUT", c.val)
		core, logs := observer.New(zap.WarnLevel)
		got := fixedBucketPutTimeoutFromEnv(zap.New(core))
		assert.Equal(t, c.want, got, "value %q", c.val)
		if c.warn {
			require.Equal(t, 1, logs.Len(), "value %q is logged at Warn and the default kept (R13-19)", c.val)
			assert.Contains(t, logs.All()[0].Message, "FIXED_BUCKET_PUT_TIMEOUT")
		} else {
			assert.Equal(t, 0, logs.Len(), "value %q", c.val)
		}
	}
}

func TestNewFixedBucketS3Driver_ReadsThePutTimeoutAndStartsTheSeriesAtZero(t *testing.T) {
	t.Setenv("FIXED_BUCKET_PUT_TIMEOUT", "45s")
	d, err := NewFixedBucketS3Driver("idrive-test-region", "ak", "sk", "https://example.invalid", "us-x", "b", zap.NewNop())
	require.NoError(t, err)
	assert.Equal(t, 45*time.Second, d.putTimeout)
	assert.Equal(t, float64(0), retriesOf("idrive-test-region"), "the series exists at 0 so the first retry is an increase")
}

// Live check 2026-10-05: prod's ten regional iDrive drivers are built by the
// same constructor under the internal name "idrive", so a stalling region
// was counted as the primary. A driver registered under another backend name
// counts its retries under THAT name.
func TestFixedBucketPut_RetriesAreCountedUnderTheBackendName(t *testing.T) {
	f := newStallS3()
	f.stall = func(_ putAttempt, n int) bool { return n == 1 }
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	core, logs := observer.New(zap.WarnLevel)
	d, err := NewIDriveDriver("ak", "sk", srv.URL, "eu-west-1", zap.New(core))
	require.NoError(t, err)
	d.SetBackendName("idrive-eu-west-1")
	d.setPutTimeout(time.Second)
	assert.Equal(t, "idrive", d.Name(), "the driver's own name is unchanged: head rows and routing key on the engine's registration")
	assert.Equal(t, float64(0), retriesOf("idrive-eu-west-1"), "the region's series exists at 0")
	primaryBefore := retriesOf("idrive")

	body := randomBody(t, 2048)
	require.NoError(t, d.Put(tctx(), "c", "r.bin", bytes.NewReader(body), engine.WithContentLength(int64(len(body)))))

	assert.Equal(t, float64(1), retriesOf("idrive-eu-west-1"))
	assert.Equal(t, primaryBefore, retriesOf("idrive"), "the primary's series did not move")
	require.Equal(t, 1, logs.Len())
	fields := logs.All()[0].ContextMap()
	assert.Equal(t, "idrive-eu-west-1", fields["driver"])
	assert.Equal(t, "eu-west-1", fields["region"], "the log line names the region too")
}
