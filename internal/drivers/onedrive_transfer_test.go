package drivers

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/FairForge/vaultaire/internal/common"
	"github.com/FairForge/vaultaire/internal/engine"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// WP-FLEET-4 (docs/reviews/WP-FLEET-NEXT-PROMPT.md Part 2): transfer
// tuning. D-F4-1 ranges of 32 MiB (64 MiB from 1 GiB): a 256 MB read was 32
// requests of the per-user budget, now ≤ 8. D-F4-2 a body the driver can
// re-read (an io.ReaderAt — the materialize spool, a bytes.Reader) up to
// 200 MiB is ONE simple PUT, retried with the burst attempts; a streamed
// body keeps the session (one request it cannot re-send would lose a shard
// to a 429 burst — WP-FLEET-3). D-F4-3 a session's fragments are 180 ×
// 320 KiB (58,982,400 B; 60 MiB sat ON Microsoft's < 60 MiB bound) and
// double-buffered: one in flight while the next is read. D-F4-4 every write
// is verified by quickXorHash: a mismatch deletes the item for good and
// fails the Put.

// qxB64 is the base64 quickXorHash of b, as Graph reports it.
func qxB64(b []byte) string {
	h := NewQuickXorHash()
	_, _ = h.Write(b)
	return base64.StdEncoding.EncodeToString(h.Sum(nil))
}

// graphSink plays Graph for uploads: simple PUTs, sessions and fragments,
// answering the final write with the item and its quickXorHash of what it
// received (corrupt = report another hash).
type graphSink struct {
	mu        sync.Mutex
	simple    int
	sessions  int
	fragments []string
	body      bytes.Buffer // the session's received bytes, in order
	corrupt   bool
	deletes   []string
	// fragmentSent is signalled when a fragment's body has been read whole;
	// holdFirst, when set, holds fragment 0's ANSWER until it is closed.
	fragmentSent chan int64
	holdFirst    chan struct{}
}

func (g *graphSink) item(b []byte) string {
	hash := qxB64(b)
	if g.corrupt {
		hash = qxB64(append([]byte("x"), b...))
	}
	return fmt.Sprintf(`{"id":"ITEM-1","size":%d,"file":{"hashes":{"quickXorHash":%q}}}`, len(b), hash)
}

func (g *graphSink) RoundTrip(req *http.Request) (*http.Response, error) {
	switch {
	case req.Method == http.MethodPost && strings.HasSuffix(req.URL.Path, "/permanentDelete"):
		g.mu.Lock()
		g.deletes = append(g.deletes, req.URL.Path)
		g.mu.Unlock()
		return odResp(req, 204, "", nil), nil
	case req.Method == http.MethodPost && strings.HasSuffix(req.URL.Path, ":/createUploadSession"):
		g.mu.Lock()
		g.sessions++
		g.body.Reset()
		g.mu.Unlock()
		return odResp(req, 200, `{"uploadUrl":"https://upload.example.test/session"}`, nil), nil
	case req.Method == http.MethodPut && strings.HasSuffix(req.URL.Path, ":/content"):
		b, err := io.ReadAll(req.Body)
		if err != nil {
			return nil, err
		}
		g.mu.Lock()
		defer g.mu.Unlock()
		g.simple++
		return odResp(req, 201, g.item(b), nil), nil
	case req.Method == http.MethodPut && req.URL.Host == "upload.example.test":
		cr := req.Header.Get("Content-Range")
		var s, e, total int64
		_, _ = fmt.Sscanf(cr, "bytes %d-%d/%d", &s, &e, &total)
		b, err := io.ReadAll(req.Body)
		if err != nil {
			return nil, err
		}
		if g.fragmentSent != nil {
			g.fragmentSent <- s
		}
		if s == 0 && g.holdFirst != nil {
			<-g.holdFirst
		}
		g.mu.Lock()
		defer g.mu.Unlock()
		g.fragments = append(g.fragments, cr)
		g.body.Write(b)
		if e+1 == total {
			return odResp(req, 201, g.item(g.body.Bytes()), nil), nil
		}
		return odResp(req, 202, `{"nextExpectedRanges":["`+fmt.Sprint(e+1)+`-"]}`, nil), nil
	}
	return odResp(req, 404, `{"error":{"code":"itemNotFound"}}`, nil), nil
}

func graphSinkFleet(g *graphSink) *OneDriveDriver {
	d, _ := stubFleet(1, zap.NewNop())
	tn := d.tenants[0]
	tn.graphHTTP = &http.Client{Transport: g}
	tn.uploadHTTP = &http.Client{Transport: g}
	tn.rootFolderID = "ROOT"
	tn.wait = (&waitLog{}).wait
	return d
}

func patternBytes(n int64) []byte {
	b := make([]byte, n)
	_, _ = io.ReadFull(&odPatternReader{0, n, nil}, b)
	return b
}

func TestOneDrivePut_ARewindable200MiBBodyIsOneSimplePUT(t *testing.T) {
	ctx := common.WithTenantID(context.Background(), "x")
	g := &graphSink{}
	d := graphSinkFleet(g)
	body := patternBytes(odSimplePutMax)

	err := d.Put(ctx, "bkt", "shard.bin", bytes.NewReader(body), engine.WithContentLength(int64(len(body))))

	require.NoError(t, err)
	assert.Equal(t, 1, g.simple, "one request")
	assert.Equal(t, 0, g.sessions)
}

func TestOneDrivePut_AStreamedBodyKeepsTheSessionInFragmentsOf180x320KiB(t *testing.T) {
	ctx := common.WithTenantID(context.Background(), "x")
	g := &graphSink{}
	d := graphSinkFleet(g)
	const size = 300 << 20

	err := d.Put(ctx, "bkt", "shard.bin", &odPatternReader{0, size, nil}, engine.WithContentLength(size))

	require.NoError(t, err)
	assert.Equal(t, 0, g.simple, "a body the driver cannot re-read is never one unretryable request")
	require.Len(t, g.fragments, 6, "5 × 58,982,400 B + the rest")
	assert.Equal(t, int64(58_982_400), int64(odChunkSize))
	assert.Equal(t, fmt.Sprintf("bytes 0-%d/%d", odChunkSize-1, size), g.fragments[0])
	assert.Equal(t, patternBytes(size), g.body.Bytes())
}

// slowSource yields its bytes slowly and records how far it has been read.
type slowSource struct {
	r    io.Reader
	read atomic.Int64
}

func (s *slowSource) Read(p []byte) (int, error) {
	if len(p) > 4<<20 {
		p = p[:4<<20]
	}
	time.Sleep(2 * time.Millisecond)
	n, err := s.r.Read(p)
	s.read.Add(int64(n))
	return n, err
}

func TestOneDrivePut_TheNextFragmentIsReadWhileOneIsInFlight(t *testing.T) {
	// Arrange: the sink holds the FIRST fragment's answer until the source
	// has been read past the second fragment.
	ctx := common.WithTenantID(context.Background(), "x")
	const size = 3 * odChunkSize
	src := &slowSource{r: &odPatternReader{0, size, nil}}
	g := &graphSink{fragmentSent: make(chan int64, 8), holdFirst: make(chan struct{})}
	d := graphSinkFleet(g)
	overlapped := make(chan bool, 1)
	go func() {
		<-g.fragmentSent // fragment 0's body is in; its answer is held
		defer close(g.holdFirst)
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			if src.read.Load() >= 2*odChunkSize {
				overlapped <- true
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
		overlapped <- false
	}()

	// Act
	err := d.Put(ctx, "bkt", "shard.bin", src, engine.WithContentLength(size))

	// Assert
	require.NoError(t, err)
	assert.True(t, <-overlapped, "fragment 1 was read from the source while fragment 0 was sent")
	assert.Equal(t, patternBytes(size), g.body.Bytes())
}

func TestOneDrivePut_AQuickXorHashMismatchFailsThePutAndDeletesTheItem(t *testing.T) {
	for _, tc := range []struct {
		name string
		body func() (io.Reader, int64)
	}{
		{"simple PUT", func() (io.Reader, int64) { b := patternBytes(5 << 20); return bytes.NewReader(b), int64(len(b)) }},
		{"session", func() (io.Reader, int64) { return &odPatternReader{0, 70 << 20, nil}, 70 << 20 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := common.WithTenantID(context.Background(), "x")
			g := &graphSink{corrupt: true}
			d := graphSinkFleet(g)
			before := testutil.ToFloat64(permafrostHashMismatch.WithLabelValues(d.tenants[0].name))
			body, size := tc.body()

			err := d.Put(ctx, "bkt", "shard.bin", body, engine.WithContentLength(size))

			require.Error(t, err)
			assert.ErrorIs(t, err, engine.ErrObjectChanged)
			assert.Contains(t, err.Error(), "quickXorHash")
			require.Len(t, g.deletes, 1, "the item that does not match is deleted for good")
			assert.Contains(t, g.deletes[0], "ITEM-1")
			assert.Equal(t, before+1, testutil.ToFloat64(permafrostHashMismatch.WithLabelValues(d.tenants[0].name)))
		})
	}
}

func TestOneDrivePut_AMatchingHashPasses(t *testing.T) {
	ctx := common.WithTenantID(context.Background(), "x")
	g := &graphSink{}
	d := graphSinkFleet(g)

	require.NoError(t, d.Put(ctx, "bkt", "a.bin", bytes.NewReader(patternBytes(5<<20)), engine.WithContentLength(5<<20)))
	require.NoError(t, d.Put(ctx, "bkt", "b.bin", &odPatternReader{0, 70 << 20, nil}, engine.WithContentLength(70<<20)))
	require.NoError(t, d.Put(ctx, "bkt", "c.bin", bytes.NewReader([]byte("small")), engine.WithContentLength(5)))
	assert.Empty(t, g.deletes)
}

func TestOneDriveGet_A256MBReadIsAtMostEightRanges(t *testing.T) {
	ctx := common.WithTenantID(context.Background(), "x")
	const size = 256_000_000
	cdn := &rangeCDN{size: size}
	d := fleetWithCDN(ctx, t, size, cdn)

	rc, err := d.Get(ctx, "bkt", "big.bin")
	require.NoError(t, err)
	n, err := io.Copy(io.Discard, rc)
	_ = rc.Close()

	require.NoError(t, err)
	assert.Equal(t, int64(size), n)
	assert.LessOrEqual(t, len(cdn.ranges), 8, "ranges of 32 MiB: %v", len(cdn.ranges))
	assert.Equal(t, int64(32<<20), odRangePartFor(size))
	assert.Equal(t, int64(64<<20), odRangePartFor(1<<30))
}

// The 2026-11-07 proof on the test drives: a 200 MiB simple PUT wrote 10 MB/s
// per object — it went over the Graph client, HTTP/2, whose flow control is
// why CDN reads and upload fragments are HTTP/1.1 (README § v3). A simple PUT
// of more than 4 MiB goes over the upload transport, with its Graph token.
func TestOneDrivePut_ALargeSimplePUTGoesOverTheHTTP1UploadTransport(t *testing.T) {
	ctx := common.WithTenantID(context.Background(), "x")
	g := &graphSink{}
	d := graphSinkFleet(g)
	var graphPuts, uploadPuts atomic.Int32
	var auth atomic.Value
	d.tenants[0].graphHTTP = &http.Client{Transport: odRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.Method == http.MethodPut {
			graphPuts.Add(1)
		}
		return g.RoundTrip(req)
	})}
	d.tenants[0].uploadHTTP = &http.Client{Transport: odRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.Method == http.MethodPut && strings.HasSuffix(req.URL.Path, ":/content") {
			uploadPuts.Add(1)
			auth.Store(req.Header.Get("Authorization"))
		}
		return g.RoundTrip(req)
	})}

	require.NoError(t, d.Put(ctx, "bkt", "big.bin", bytes.NewReader(patternBytes(8<<20)), engine.WithContentLength(8<<20)))
	require.NoError(t, d.Put(ctx, "bkt", "small.bin", bytes.NewReader([]byte("tiny")), engine.WithContentLength(4)))

	assert.Equal(t, int32(1), uploadPuts.Load(), "the 8 MiB simple PUT: HTTP/1.1")
	assert.Equal(t, int32(1), graphPuts.Load(), "a small one stays on the Graph client")
	assert.Equal(t, "Bearer stub-token", auth.Load(), "with its Graph token")
}
