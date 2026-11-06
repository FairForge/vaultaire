package drivers

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/FairForge/vaultaire/internal/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// WP-FLEET-3 D-F3-1 / D-F3-2: Microsoft answers over-rate traffic with
// one-minute 429 BURSTS (the 2026-11-06 soak: 574 range 429s, the round
// after each burst clean). A CDN range had ONE attempt (the window's retry
// runs only with hedging, which the fleet never set) and a fragment two, a
// Retry-After honoured to 30 s — a burst outlasted both and the object was
// lost; and a range whose body was empty ended the GET cleanly at 0 bytes.
// Now ranges and fragments get odBurstAttempts (6; 8 under a job), every
// wait on ctx, Retry-After honoured to odBurstMaxWait (90 s), and a range
// that never comes is an error of the body.

// burstCDN serves a pattern object of size bytes, honouring Range; fail
// decides, per attempt at a range start (counting from 0), an answer that
// replaces the good one (nil = serve it).
type burstCDN struct {
	size int64
	fail func(start int64, attempt int, req *http.Request) *http.Response

	mu       sync.Mutex
	attempts map[int64]int
}

func (c *burstCDN) RoundTrip(req *http.Request) (*http.Response, error) {
	var s, e int64
	if _, err := fmt.Sscanf(req.Header.Get("Range"), "bytes=%d-%d", &s, &e); err != nil {
		return nil, err
	}
	c.mu.Lock()
	if c.attempts == nil {
		c.attempts = map[int64]int{}
	}
	n := c.attempts[s]
	c.attempts[s]++
	c.mu.Unlock()
	if c.fail != nil {
		if r := c.fail(s, n, req); r != nil {
			return r, nil
		}
	}
	h := make(http.Header)
	h.Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", s, e, c.size))
	return &http.Response{StatusCode: 206, ContentLength: e - s + 1, Header: h, Request: req,
		Body: io.NopCloser(&odPatternReader{s, e + 1, nil})}, nil
}

func (c *burstCDN) attemptsAt(start int64) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.attempts[start]
}

// burstFleet is fleetWithCDN with the waits recorded instead of slept.
func burstFleet(ctx context.Context, t *testing.T, cdn *burstCDN) (*OneDriveDriver, *waitLog) {
	t.Helper()
	d := fleetWithCDN(ctx, t, cdn.size, cdn)
	wl := &waitLog{}
	d.tenants[0].wait = wl.wait
	return d, wl
}

func throttled(req *http.Request, retryAfter string) *http.Response {
	h := map[string]string{}
	if retryAfter != "" {
		h["Retry-After"] = retryAfter
	}
	return odResp(req, http.StatusTooManyRequests, `{"error":{"code":"activityLimitReached"}}`, h)
}

// burstRead reads a Get the way the bench and the API do: io.Copy (the
// reader's WriteTo).
func burstRead(ctx context.Context, t *testing.T, d *OneDriveDriver) (int64, [32]byte, error) {
	t.Helper()
	rc, err := d.Get(ctx, "bkt", "big.bin")
	require.NoError(t, err)
	defer func() { _ = rc.Close() }()
	h := sha256.New()
	n, err := io.Copy(h, rc)
	var sum [32]byte
	copy(sum[:], h.Sum(nil))
	return n, sum, err
}

const burstObject = 32 << 20 // four 8 MiB ranges

func TestOneDriveGet_AnEmptyRangeBodyIsAnErrorNeverACleanEnd(t *testing.T) {
	// Arrange: the first range answers 206 with the right Content-Range and
	// no bytes, on every attempt.
	ctx := common.WithTenantID(context.Background(), "x")
	cdn := &burstCDN{size: burstObject, fail: func(s int64, _ int, req *http.Request) *http.Response {
		if s != 0 {
			return nil
		}
		h := make(http.Header)
		h.Set("Content-Range", fmt.Sprintf("bytes 0-%d/%d", odRangePart-1, burstObject))
		return &http.Response{StatusCode: 206, Header: h, Request: req, Body: io.NopCloser(strings.NewReader(""))}
	}}
	d, _ := burstFleet(ctx, t, cdn)

	// Act
	n, _, err := burstRead(ctx, t, d)

	// Assert: the soak's `size mismatch: got 0` was this read returning nil.
	require.Error(t, err, "a range that never comes is an error of the body")
	assert.Equal(t, int64(0), n)
}

func TestOneDriveGet_ARangeThrottledThreeTimesIsRiddenOut(t *testing.T) {
	// Arrange: the second range is refused three times, Retry-After 1.
	ctx := common.WithTenantID(context.Background(), "x")
	cdn := &burstCDN{size: burstObject, fail: func(s int64, n int, req *http.Request) *http.Response {
		if s == odRangePart && n < 3 {
			return throttled(req, "1")
		}
		return nil
	}}
	d, wl := burstFleet(ctx, t, cdn)

	// Act
	n, sum, err := burstRead(ctx, t, d)

	// Assert
	require.NoError(t, err)
	assert.Equal(t, int64(burstObject), n)
	assert.Equal(t, patternSum(burstObject), sum)
	assert.Equal(t, 4, cdn.attemptsAt(odRangePart), "three refusals, then the range")
	waits := wl.all()
	// Three retries of the range — and, since WP-FLEET-3 D-F3-6, the
	// object's other ranges waiting out the account's hold too.
	require.GreaterOrEqual(t, len(waits), 3)
	for _, w := range waits[:3] {
		assert.GreaterOrEqual(t, w, 900*time.Millisecond, "Retry-After is a floor")
	}
}

func TestOneDriveGet_ARangeThrottledForeverFailsAfterItsAttempts(t *testing.T) {
	for _, tc := range []struct {
		name string
		ctx  context.Context
		want int
	}{
		{"request path", context.Background(), 6},
		{"job", common.WithJob(context.Background()), 8},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Arrange
			ctx := common.WithTenantID(tc.ctx, "x")
			cdn := &burstCDN{size: burstObject, fail: func(s int64, _ int, req *http.Request) *http.Response {
				if s == 2*odRangePart {
					return throttled(req, "")
				}
				return nil
			}}
			d, wl := burstFleet(ctx, t, cdn)

			// Act
			n, _, err := burstRead(ctx, t, d)

			// Assert
			require.Error(t, err)
			assert.Contains(t, err.Error(), "429")
			assert.Equal(t, int64(2*odRangePart), n, "the ranges before it, nothing after")
			assert.Equal(t, tc.want, cdn.attemptsAt(2*odRangePart))
			assert.GreaterOrEqual(t, len(wl.all()), tc.want-1, "a wait before every retry (and the other ranges' holds)")
		})
	}
}

func TestOneDriveGet_ARetryAfterOfAMinuteIsWaitedForAndCapped(t *testing.T) {
	ctx := common.WithTenantID(context.Background(), "x")
	cdn := &burstCDN{size: burstObject, fail: func(s int64, n int, req *http.Request) *http.Response {
		switch {
		case s == 0 && n == 0:
			return throttled(req, "60")
		case s == 0 && n == 1:
			return throttled(req, "600")
		}
		return nil
	}}
	d, wl := burstFleet(ctx, t, cdn)

	_, sum, err := burstRead(ctx, t, d)

	require.NoError(t, err)
	assert.Equal(t, patternSum(burstObject), sum)
	waits := wl.all()
	require.GreaterOrEqual(t, len(waits), 2)
	longest := time.Duration(0)
	for _, w := range waits {
		assert.LessOrEqual(t, w, odBurstMaxWait, "never more than the burst cap")
		longest = max(longest, w)
	}
	assert.GreaterOrEqual(t, waits[0], 59*time.Second, "a burst lasts about a minute: waited for")
	assert.Equal(t, odBurstMaxWait, longest, "Retry-After: 600 is waited for to the cap")
}

func TestOneDriveGet_ARangeIsNeverRetriedPastTheCallersDeadline(t *testing.T) {
	ctx, cancel := context.WithTimeout(common.WithTenantID(context.Background(), "x"), 10*time.Second)
	defer cancel()
	cdn := &burstCDN{size: burstObject, fail: func(s int64, _ int, req *http.Request) *http.Response {
		if s == 0 {
			return throttled(req, "60")
		}
		return nil
	}}
	d, wl := burstFleet(ctx, t, cdn)

	_, _, err := burstRead(ctx, t, d)

	require.Error(t, err)
	assert.Equal(t, 1, cdn.attemptsAt(0), "a Retry-After past the deadline is given up on at once")
	assert.Empty(t, wl.all())
}

func TestOneDriveGet_AWholeObjectAnswerIsNeverRetried(t *testing.T) {
	ctx := common.WithTenantID(context.Background(), "x")
	cdn := &burstCDN{size: burstObject, fail: func(s int64, _ int, req *http.Request) *http.Response {
		if s == 0 {
			return &http.Response{StatusCode: 200, Header: make(http.Header), Request: req,
				Body: io.NopCloser(&odPatternReader{0, burstObject, nil})}
		}
		return nil
	}}
	d, wl := burstFleet(ctx, t, cdn)

	_, _, err := burstRead(ctx, t, d)

	require.Error(t, err)
	assert.Equal(t, 1, cdn.attemptsAt(0), "a 200 is a CDN that ignores Range: no attempt will differ")
	assert.Empty(t, wl.all())
}

func TestPutChunk_AFragmentThrottledTwiceIsRiddenOut(t *testing.T) {
	// Arrange
	rt := &odScriptRT{fn: func(n int, req *http.Request) (*http.Response, error) {
		if n < 2 {
			return throttled(req, "1"), nil
		}
		return odResp(req, 202, `{}`, nil), nil
	}}
	wl := &waitLog{}
	tn := scriptedTenant(rt, wl)

	// Act
	_, err := tn.putChunk(context.Background(), "https://upload.example.test/s", []byte("abc"), 0, 2, 3)

	// Assert
	require.NoError(t, err)
	assert.Equal(t, 3, rt.calls())
	assert.Len(t, wl.all(), 2)
}

func TestPutChunk_HonoursRetryAfterToTheBurstCap(t *testing.T) {
	rt := &odScriptRT{fn: func(n int, req *http.Request) (*http.Response, error) {
		switch n {
		case 0:
			return throttled(req, "45"), nil
		case 1:
			return throttled(req, "3600"), nil
		}
		return odResp(req, 201, `{}`, nil), nil
	}}
	wl := &waitLog{}
	tn := scriptedTenant(rt, wl)

	require.NoError(t, func() error {
		_, err := tn.putChunk(context.Background(), "https://upload.example.test/s", []byte("abc"), 0, 2, 3)
		return err
	}())

	waits := wl.all()
	require.Len(t, waits, 2)
	assert.GreaterOrEqual(t, waits[0], 45*time.Second)
	assert.Equal(t, odBurstMaxWait, waits[1])
}

func TestGraphDo_MetadataCallsKeepTheirTwoAttempts(t *testing.T) {
	rt := &odScriptRT{fn: func(_ int, req *http.Request) (*http.Response, error) { return throttled(req, "60"), nil }}
	wl := &waitLog{}
	tn := scriptedTenant(rt, wl)

	_, err := tn.graphDo(context.Background(), graphGet(context.Background(), t))

	require.Error(t, err)
	assert.Equal(t, 2, rt.calls(), "a 429 on a small call is cheap to repeat later (D-F3-2)")
	waits := wl.all()
	require.Len(t, waits, 2)
	assert.Equal(t, odMaxRetryWait, waits[0], "Graph's own retry wait stays capped at 30 s")
	assert.InDelta(t, 30, waits[1].Seconds(), 1, "the account's hold (D-F3-6) makes the call honour the rest of the Retry-After")
}
