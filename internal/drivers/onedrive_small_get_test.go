package drivers

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/FairForge/vaultaire/internal/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The review of the WP-FLEET next chain (2026-11-07, RB3-2): WP-FLEET-3 gave
// a CDN RANGE the burst attempts; a whole Get under 10 MiB (one stream) kept
// its ONE plain GET — a 429 held the account and failed the read, a cut body
// streamed through. A Vault parity shard is a quarter of its object, so the
// shards of every Vault object under 40 MiB read this way. Now a small object
// is one part of the same ranged reader: a whole GET (no Range header — the
// CDN's answer to a full-span Range was never relied on), 200 with the
// object's Content-Length, the burst attempts, the hedge, the budget; a
// 0-byte object is an empty body with no CDN call.

// wholeCDN serves a pattern object of size bytes to a plain GET (a Range
// header is refused: the small path must not send one); fail decides, per
// attempt (from 0), an answer that replaces the good one.
type wholeCDN struct {
	size int64
	fail func(attempt int, req *http.Request) *http.Response

	mu       sync.Mutex
	attempts int
}

func (c *wholeCDN) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Header.Get("Range") != "" {
		return nil, fmt.Errorf("a Range header on a whole GET: %q", req.Header.Get("Range"))
	}
	c.mu.Lock()
	n := c.attempts
	c.attempts++
	c.mu.Unlock()
	if c.fail != nil {
		if r := c.fail(n, req); r != nil {
			return r, nil
		}
	}
	return &http.Response{StatusCode: 200, ContentLength: c.size, Header: make(http.Header), Request: req,
		Body: io.NopCloser(&odPatternReader{0, c.size, nil})}, nil
}

func (c *wholeCDN) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.attempts
}

const smallObject = 5 << 20 // under the 10 MiB single-stream line, under one 8 MiB part

func TestOneDriveGet_ASmallObjectRidesA429AndACutBody(t *testing.T) {
	// Arrange: attempt 0 is a 429 with Retry-After, attempt 1 a 200 whose
	// body ends after 1 KiB, attempt 2 the object.
	ctx := common.WithTenantID(context.Background(), "x")
	cdn := &wholeCDN{size: smallObject, fail: func(n int, req *http.Request) *http.Response {
		switch n {
		case 0:
			return throttled(req, "2")
		case 1:
			return &http.Response{StatusCode: 200, ContentLength: smallObject, Header: make(http.Header), Request: req,
				Body: io.NopCloser(&odPatternReader{0, 1 << 10, nil})}
		}
		return nil
	}}
	d := fleetWithCDN(ctx, t, smallObject, cdn)
	wl := &waitLog{}
	d.tenants[0].wait = wl.wait

	// Act
	n, sum, err := burstRead(ctx, t, d)

	// Assert
	require.NoError(t, err)
	assert.Equal(t, int64(smallObject), n)
	assert.Equal(t, patternSum(smallObject), sum)
	assert.Equal(t, 3, cdn.count(), "the 429, the cut body, the object")
	assert.Equal(t, int64(1), d.tenants[0].throttleCount.Load(), "the 429 held the account")
	assert.GreaterOrEqual(t, len(wl.all()), 2, "a wait before each retry")
}

func TestOneDriveGet_AnEmptyObjectIsAnEmptyBodyWithNoCDNCall(t *testing.T) {
	ctx := common.WithTenantID(context.Background(), "x")
	cdn := &wholeCDN{size: 0, fail: func(int, *http.Request) *http.Response {
		t.Fatal("a CDN call for a 0-byte object")
		return nil
	}}
	d := fleetWithCDN(ctx, t, 0, cdn)

	rc, err := d.Get(ctx, "bkt", "big.bin")

	require.NoError(t, err)
	b, err := io.ReadAll(rc)
	require.NoError(t, err)
	assert.Empty(t, b)
	assert.Equal(t, 0, cdn.count())
}

func TestOneDriveGet_ASmallObjectLongerThanItsSizeIsAnErrorNotRetried(t *testing.T) {
	// Arrange: the CDN serves one byte more than Graph said the object is.
	ctx := common.WithTenantID(context.Background(), "x")
	cdn := &wholeCDN{size: smallObject, fail: func(_ int, req *http.Request) *http.Response {
		return &http.Response{StatusCode: 200, ContentLength: -1, Header: make(http.Header), Request: req,
			Body: io.NopCloser(&odPatternReader{0, smallObject + 1, nil})}
	}}
	d := fleetWithCDN(ctx, t, smallObject, cdn)
	d.tenants[0].wait = (&waitLog{}).wait

	// Act
	_, _, err := burstRead(ctx, t, d)

	// Assert: another object's bytes, never served, never asked again.
	require.Error(t, err)
	assert.Contains(t, err.Error(), "longer")
	assert.Equal(t, 1, cdn.count())
}

func TestOneDriveGet_ASmallObjectsWrongContentLengthIsRetried(t *testing.T) {
	// Arrange: attempt 0 answers 200 with Content-Length: 0 and no bytes
	// (the soak's `got 0` shape), attempt 1 the object.
	ctx := common.WithTenantID(context.Background(), "x")
	cdn := &wholeCDN{size: smallObject, fail: func(n int, req *http.Request) *http.Response {
		if n == 0 {
			return &http.Response{StatusCode: 200, ContentLength: 0, Header: make(http.Header), Request: req,
				Body: io.NopCloser(strings.NewReader(""))}
		}
		return nil
	}}
	d := fleetWithCDN(ctx, t, smallObject, cdn)
	d.tenants[0].wait = (&waitLog{}).wait

	// Act
	n, sum, err := burstRead(ctx, t, d)

	// Assert
	require.NoError(t, err)
	assert.Equal(t, int64(smallObject), n)
	assert.Equal(t, patternSum(smallObject), sum)
	assert.Equal(t, 2, cdn.count())
}
