package drivers

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

const swiftTestHedgeAfter = 100 * time.Millisecond

// newHedgedSwift is a driver on the fake with a short hedge delay, under its
// own metrics name.
func newHedgedSwift(t *testing.T, name string) (*SwiftDriver, *fakeSwift) {
	t.Helper()
	f := newFakeSwift(t)
	d, err := NewSwiftDriver(name, f.config(), zap.NewNop())
	require.NoError(t, err)
	d.hedgeAfter = swiftTestHedgeAfter
	return d, f
}

func swiftHedgesOf(driver, op string) (sent, won float64) {
	return testutil.ToFloat64(driverHedges.WithLabelValues(driver, op)),
		testutil.ToFloat64(driverHedgesWon.WithLabelValues(driver, op))
}

func TestSwiftHedge_AStalledHeadIsAnsweredByTheHedge(t *testing.T) {
	d, f := newHedgedSwift(t, "swift-hedge-head")
	ctx := swiftCtx(swiftTenant(t))
	require.NoError(t, d.Put(ctx, "c", "k", bytes.NewReader([]byte("x"))))
	sent0, won0 := swiftHedgesOf("swift-hedge-head", "head_object")
	f.stallNext(http.MethodHead, 1)

	start := time.Now()
	ok, err := d.Exists(ctx, "c", "k")

	require.NoError(t, err)
	assert.True(t, ok)
	assert.Less(t, time.Since(start), 5*time.Second, "answered by the hedge, not after the 15 s bound")
	assert.Eventually(t, func() bool { return f.cancelledStalls() == 1 }, 5*time.Second, 10*time.Millisecond,
		"the stalled first attempt is cancelled")
	sent, won := swiftHedgesOf("swift-hedge-head", "head_object")
	sent, won = sent-sent0, won-won0
	assert.Equal(t, 1.0, sent)
	assert.Equal(t, 1.0, won)
}

func TestSwiftHedge_TheProbeIsHedged(t *testing.T) {
	d, f := newHedgedSwift(t, "swift-hedge-probe")
	require.NoError(t, d.HealthCheck(context.Background())) // a token first
	sent0, won0 := swiftHedgesOf("swift-hedge-probe", "head_bucket")
	f.stallNext(http.MethodHead, 1)

	require.NoError(t, d.HealthCheck(context.Background()))

	sent, won := swiftHedgesOf("swift-hedge-probe", "head_bucket")
	sent, won = sent-sent0, won-won0
	assert.Equal(t, 1.0, sent)
	assert.Equal(t, 1.0, won)
}

// A GET is hedged on its headers: no byte of a body has been read when the
// hedge fires, so the winner's body is the whole object, and the loser is
// cancelled.
func TestSwiftHedge_AStalledGetIsHedgedOnItsHeaders(t *testing.T) {
	d, f := newHedgedSwift(t, "swift-hedge-get")
	ctx := swiftCtx(swiftTenant(t))
	want := randomBytes(t, 700)
	require.NoError(t, d.Put(ctx, "c", "k", bytes.NewReader(want)))

	sent0, won0 := swiftHedgesOf("swift-hedge-get", "get")
	f.stallNext(http.MethodGet, 1)
	rc, err := d.Get(ctx, "c", "k")
	require.NoError(t, err)
	got, err := io.ReadAll(rc)
	require.NoError(t, err)
	require.NoError(t, rc.Close())
	assert.Equal(t, want, got)

	f.stallNext(http.MethodGet, 1)
	rc, info, err := d.GetRangeInfo(ctx, "c", "k", 100, 50)
	require.NoError(t, err)
	got, err = io.ReadAll(rc)
	require.NoError(t, err)
	require.NoError(t, rc.Close())
	assert.Equal(t, want[100:150], got)
	assert.Equal(t, int64(700), info.Size)

	assert.Eventually(t, func() bool { return f.cancelledStalls() == 2 }, 5*time.Second, 10*time.Millisecond)
	sent, won := swiftHedgesOf("swift-hedge-get", "get")
	sent, won = sent-sent0, won-won0
	assert.Equal(t, 2.0, sent)
	assert.Equal(t, 2.0, won)
}

func TestSwiftHedge_AListingPageIsHedged(t *testing.T) {
	d, f := newHedgedSwift(t, "swift-hedge-list")
	ctx := swiftCtx(swiftTenant(t))
	require.NoError(t, d.Put(ctx, "c", "a", bytes.NewReader([]byte("x"))))
	sent0, won0 := swiftHedgesOf("swift-hedge-list", "list")
	f.stallNext(http.MethodGet, 1)

	names, err := d.List(ctx, "c", "")

	require.NoError(t, err)
	assert.Equal(t, []string{"a"}, names)
	sent, won := swiftHedgesOf("swift-hedge-list", "list")
	sent, won = sent-sent0, won-won0
	assert.Equal(t, 1.0, sent)
	assert.Equal(t, 1.0, won)
}

// Writes are never hedged: a PUT would send its bytes twice, and a DELETE
// whose hedge answered 404 while the first attempt deleted would report a
// miss — and Blomp rate-limits writes per container (~3/s).
func TestSwiftHedge_NeverAPutOrADelete(t *testing.T) {
	d, f := newHedgedSwift(t, "swift-hedge-writes")
	ctx := swiftCtx(swiftTenant(t))

	f.stallNext(http.MethodPut, 1)
	time.AfterFunc(4*swiftTestHedgeAfter, f.releaseStalls)
	require.NoError(t, d.Put(ctx, "c", "k", bytes.NewReader([]byte("x"))))
	assert.Equal(t, 1, f.count("PUT object"), "one PUT, held, never a second")

	f2 := newFakeSwift(t)
	d2, err := NewSwiftDriver("swift-hedge-writes", f2.config(), zap.NewNop())
	require.NoError(t, err)
	d2.hedgeAfter = swiftTestHedgeAfter
	require.NoError(t, d2.Put(ctx, "c", "k", bytes.NewReader([]byte("x"))))
	f2.stallNext(http.MethodDelete, 1)
	time.AfterFunc(4*swiftTestHedgeAfter, f2.releaseStalls)
	require.NoError(t, d2.Delete(ctx, "c", "k"))
	assert.Equal(t, 1, f2.count("DELETE object"), "one DELETE, held, never a second")
	assert.False(t, d2.hedgeable(swiftRequest{method: http.MethodPost, account: true, bodyBytes: []byte("x")}), "a bulk delete is a write")
}

// The unit below the driver: a loser that answers after the winner has its
// body closed and its context cancelled; the winner's context lives until
// its body is closed.
func TestSwiftHedge_TheLosersBodyIsClosed(t *testing.T) {
	type tracked struct {
		io.Reader
		closed atomic.Bool
	}
	loserBody := &tracked{Reader: bytes.NewReader([]byte("late"))}
	firstSent := make(chan struct{})
	hedgeWon := make(chan struct{})
	var winnerCtx context.Context
	send := func(ctx context.Context, fresh bool) (*http.Response, error) {
		if !fresh {
			close(firstSent)
			<-hedgeWon // answers only after the hedge has won
			return &http.Response{StatusCode: http.StatusOK, Body: closeFunc{loserBody, func() error { loserBody.closed.Store(true); return nil }}}, nil
		}
		winnerCtx = ctx
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(bytes.NewReader([]byte("first")))}, nil
	}

	resp, err := hedgedRoundTrip(context.Background(), 20*time.Millisecond, "swift-hedge-unit", "get", send)
	close(hedgeWon)

	require.NoError(t, err)
	<-firstSent
	body, _ := io.ReadAll(resp.Body)
	assert.Equal(t, "first", string(body))
	assert.NoError(t, winnerCtx.Err(), "the winner's context lives while its body is read")
	require.NoError(t, resp.Body.Close())
	assert.Error(t, winnerCtx.Err(), "closing the winner's body releases its context")
	assert.Eventually(t, loserBody.closed.Load, 5*time.Second, 5*time.Millisecond, "the late loser's body is closed")
}

type closeFunc struct {
	io.Reader
	close func() error
}

func (c closeFunc) Close() error { return c.close() }

func TestSwiftHedge_NoAnswerFromEitherIsTheFirstAttemptsError(t *testing.T) {
	first := errors.New("first: connection reset")
	send := func(ctx context.Context, fresh bool) (*http.Response, error) {
		if !fresh {
			time.Sleep(60 * time.Millisecond)
			return nil, first
		}
		return nil, errors.New("hedge: connection reset")
	}
	_, err := hedgedRoundTrip(context.Background(), 20*time.Millisecond, "swift-hedge-unit2", "head_object", send)
	assert.ErrorIs(t, err, first)
}

func TestSwiftHedge_ACallerAlreadyGoneIsNotHedged(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var calls atomic.Int32
	send := func(ctx context.Context, fresh bool) (*http.Response, error) {
		calls.Add(1)
		time.Sleep(60 * time.Millisecond)
		return nil, ctx.Err()
	}
	sent0, won0 := swiftHedgesOf("swift-hedge-unit3", "head_object")
	_, err := hedgedRoundTrip(ctx, 10*time.Millisecond, "swift-hedge-unit3", "head_object", send)
	assert.Error(t, err)
	assert.Equal(t, int32(1), calls.Load())
	sent, _ := swiftHedgesOf("swift-hedge-unit3", "head_object")
	sent -= sent0
	_ = won0
	assert.Equal(t, 0.0, sent)
}

func TestSwiftHedge_AQuickAnswerIsNotHedged(t *testing.T) {
	d, _ := newHedgedSwift(t, "swift-hedge-quick")
	d.hedgeAfter = 30 * time.Second // quicker than any loaded runner reaches
	ctx := swiftCtx(swiftTenant(t))

	sent0, won0 := swiftHedgesOf("swift-hedge-quick", "head_object")
	ok, err := d.Exists(ctx, "c", "missing")

	require.NoError(t, err)
	assert.False(t, ok, "a 404 is an answer")
	sent, _ := swiftHedgesOf("swift-hedge-quick", "head_object")
	sent -= sent0
	_ = won0
	assert.Equal(t, 0.0, sent)
}
