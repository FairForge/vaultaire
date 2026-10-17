package drivers

import (
	"context"
	"errors"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	promtest "github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

// WP-R7-6: the drivers' DNS cache stored an answer for the life of the
// process and every dial took addrs[0] — prod dialled one Wasabi IP (.101)
// since boot (2026-10-17) and would do the same to Lyve. An answer now
// expires after DRIVER_DNS_TTL (5 min), the dials rotate over its addresses
// and a dial that fails on one address tries the next in the same call; a
// resolver outage keeps the stale answer (it must not take the drivers
// down); one lookup per host per TTL, never per dial.

type fakeResolver struct {
	mu      sync.Mutex
	answers map[string][]string
	fail    bool
	calls   atomic.Int32
	block   chan struct{} // nil = answer at once
}

func (r *fakeResolver) lookup(ctx context.Context, host string) ([]string, error) {
	r.calls.Add(1)
	if r.block != nil {
		<-r.block
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.fail {
		return nil, &net.DNSError{Err: "server misbehaving", Name: host, IsTemporary: true}
	}
	return append([]string(nil), r.answers[host]...), nil
}

func (r *fakeResolver) set(host string, addrs ...string) {
	r.mu.Lock()
	r.answers[host] = addrs
	r.mu.Unlock()
}

// fakeDial records each address dialled; addresses in down refuse.
type fakeDial struct {
	mu     sync.Mutex
	dialed []string
	down   map[string]bool
}

func (d *fakeDial) dial(ctx context.Context, network, addr string) (net.Conn, error) {
	if err := ctx.Err(); err != nil { // as net.Dialer: a finished context is not dialled
		return nil, err
	}
	d.mu.Lock()
	d.dialed = append(d.dialed, addr)
	refused := d.down[addr]
	d.mu.Unlock()
	if refused {
		return nil, &net.OpError{Op: "dial", Net: network, Err: errors.New("connection refused")}
	}
	c1, c2 := net.Pipe()
	_ = c2.Close()
	return c1, nil
}

func (d *fakeDial) last() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.dialed[len(d.dialed)-1]
}

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *fakeClock) add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func newTestDNS(r *fakeResolver, clock *fakeClock) *dnsDialCache {
	return &dnsDialCache{lookup: r.lookup, ttl: 5 * time.Minute, now: clock.now}
}

func dialOnce(t *testing.T, dial func(context.Context, string, string) (net.Conn, error), addr string) {
	t.Helper()
	c, err := dial(context.Background(), "tcp", addr)
	require.NoError(t, err)
	_ = c.Close()
}

func TestDNSCache_ARefreshedAnswerIsUsedAfterTheTTL(t *testing.T) {
	r := &fakeResolver{answers: map[string][]string{}}
	r.set("s3.lyve.example", "10.0.0.1")
	clock := &fakeClock{t: time.Unix(1_800_000_000, 0)}
	d := &fakeDial{}
	dial := newTestDNS(r, clock).dialer(d.dial)

	dialOnce(t, dial, "s3.lyve.example:443")
	r.set("s3.lyve.example", "10.0.0.9") // the vendor moved the name
	clock.add(4 * time.Minute)
	dialOnce(t, dial, "s3.lyve.example:443")
	assert.Equal(t, "10.0.0.1:443", d.last(), "inside the TTL: the cached answer")
	assert.Equal(t, int32(1), r.calls.Load(), "one lookup per TTL, never per dial")

	clock.add(2 * time.Minute)
	dialOnce(t, dial, "s3.lyve.example:443")
	assert.Equal(t, "10.0.0.1:443", d.last(), "the dial after the TTL starts the refresh and takes the stale answer: it never waits on the resolver")
	require.Eventually(t, func() bool { return r.calls.Load() == 2 }, time.Second, time.Millisecond, "after the TTL the name is asked again")
	require.Eventually(t, func() bool {
		dialOnce(t, dial, "s3.lyve.example:443")
		return d.last() == "10.0.0.9:443"
	}, time.Second, time.Millisecond, "the dials after the refresh take the new answer")
	assert.Equal(t, int32(2), r.calls.Load(), "one lookup per TTL, never per dial")
}

func TestDNSCache_DialsRotateOverTheAnswer(t *testing.T) {
	r := &fakeResolver{answers: map[string][]string{}}
	r.set("s3.wasabi.example", "10.0.0.1", "10.0.0.2", "10.0.0.3")
	d := &fakeDial{}
	dial := newTestDNS(r, &fakeClock{t: time.Unix(1_800_000_000, 0)}).dialer(d.dial)

	seen := map[string]int{}
	for i := 0; i < 6; i++ {
		dialOnce(t, dial, "s3.wasabi.example:443")
		seen[d.last()]++
	}

	assert.Equal(t, map[string]int{"10.0.0.1:443": 2, "10.0.0.2:443": 2, "10.0.0.3:443": 2}, seen, "round-robin, not addrs[0] for ever")
}

func TestDNSCache_ADialThatFailsTriesTheNextAddress(t *testing.T) {
	r := &fakeResolver{answers: map[string][]string{}}
	r.set("s3.lyve.example", "10.0.0.1", "10.0.0.2")
	d := &fakeDial{down: map[string]bool{"10.0.0.1:443": true}}
	dial := newTestDNS(r, &fakeClock{t: time.Unix(1_800_000_000, 0)}).dialer(d.dial)

	for i := 0; i < 3; i++ {
		c, err := dial(context.Background(), "tcp", "s3.lyve.example:443")
		require.NoError(t, err, "dial %d: the next address answers in the same call", i)
		_ = c.Close()
		assert.Equal(t, "10.0.0.2:443", d.last())
	}

	d.down["10.0.0.2:443"] = true
	_, err := dial(context.Background(), "tcp", "s3.lyve.example:443")
	require.Error(t, err, "every address down: the dial's error")
}

func TestDNSCache_AResolverOutageKeepsTheStaleAnswer(t *testing.T) {
	r := &fakeResolver{answers: map[string][]string{}}
	r.set("s3.lyve.example", "10.0.0.1")
	clock := &fakeClock{t: time.Unix(1_800_000_000, 0)}
	d := &fakeDial{}
	dial := newTestDNS(r, clock).dialer(d.dial)
	dialOnce(t, dial, "s3.lyve.example:443")
	errs0 := promtest.ToFloat64(driverDNSRefreshes.WithLabelValues("s3.lyve.example", "error"))

	r.mu.Lock()
	r.fail = true
	r.mu.Unlock()
	clock.add(6 * time.Minute)
	dialOnce(t, dial, "s3.lyve.example:443")

	assert.Equal(t, "10.0.0.1:443", d.last(), "the stale answer, not an outage")
	require.Eventually(t, func() bool {
		return promtest.ToFloat64(driverDNSRefreshes.WithLabelValues("s3.lyve.example", "error")) == errs0+1
	}, time.Second, time.Millisecond, "the failed refresh is counted")
	assert.Equal(t, int32(2), r.calls.Load())
	dialOnce(t, dial, "s3.lyve.example:443")
	assert.Equal(t, "10.0.0.1:443", d.last(), "still the stale answer after the failed refresh")
}

func TestDNSCache_OneLookupForManyDialsAtOnce(t *testing.T) {
	r := &fakeResolver{answers: map[string][]string{}, block: make(chan struct{})}
	r.set("s3.idrive.example", "10.0.0.1", "10.0.0.2")
	d := &fakeDial{}
	dial := newTestDNS(r, &fakeClock{t: time.Unix(1_800_000_000, 0)}).dialer(d.dial)

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c, err := dial(context.Background(), "tcp", "s3.idrive.example:443")
			if assert.NoError(t, err) {
				_ = c.Close()
			}
		}()
	}
	require.Eventually(t, func() bool { return r.calls.Load() >= 1 }, time.Second, time.Millisecond)
	time.Sleep(50 * time.Millisecond) // let the other dials pile up behind it
	close(r.block)
	wg.Wait()

	assert.Equal(t, int32(1), r.calls.Load(), "one lookup per host, shared by every dial waiting on it")
}

func TestDriverDNSTTL_EnvRule(t *testing.T) {
	core, logs := observer.New(zap.WarnLevel)
	logger := zap.New(core)
	for _, c := range []struct {
		v    string
		want time.Duration
		warn bool
	}{
		{"", 5 * time.Minute, false},
		{"30s", 30 * time.Second, false},
		{"1h", time.Hour, false},
		{"10s", 5 * time.Minute, true},
		{"2h", 5 * time.Minute, true},
		{"forever", 5 * time.Minute, true},
	} {
		t.Setenv("DRIVER_DNS_TTL", c.v)
		before := logs.Len()
		assert.Equal(t, c.want, driverDNSTTLFromEnv(logger), "value %q", c.v)
		assert.Equal(t, c.warn, logs.Len() > before, "value %q: a rejected value is a Warn", c.v)
	}
}

// Post-merge review of #748 (WP-P9): the refresh ran under
// context.WithoutCancel and every dial waiting on it blocked in
// singleflight.Do — a dial with a 100 ms context waited the whole hung
// lookup (3 s in the probe), so a hedge "at 2 s on a fresh connection" and a
// probe's 15 s budget were bounded by the resolver's own timeouts, not by
// their callers (before #748 the lookup ran under the dial's context). Now a
// dial whose context ends while the lookup runs returns at its deadline —
// with the stale answer when there is one, else its context's error — and
// the lookup itself has its own bound (dnsLookupTimeout), still finishing for
// the next dial.

func TestDNSCache_ADialWhoseContextEndsDuringTheLookupReturnsAtItsDeadline(t *testing.T) {
	r := &fakeResolver{answers: map[string][]string{}, block: make(chan struct{})}
	r.set("s3.lyve.example", "10.0.0.1")
	d := &fakeDial{}
	dial := newTestDNS(r, &fakeClock{t: time.Unix(1_800_000_000, 0)}).dialer(d.dial)

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := dial(ctx, "tcp", "s3.lyve.example:443")
	require.ErrorIs(t, err, context.DeadlineExceeded, "no answer yet: the dial's own error, not a wait on the resolver")
	assert.Less(t, time.Since(start), time.Second, "it returned at its deadline, not when the resolver answered")
	assert.Empty(t, d.dialed, "nothing was dialled by name either: the dial's context had ended")

	close(r.block) // the abandoned refresh completes for the next dial
	require.Eventually(t, func() bool {
		c, err := dial(context.Background(), "tcp", "s3.lyve.example:443")
		if err != nil {
			return false
		}
		_ = c.Close()
		return true
	}, time.Second, 5*time.Millisecond)
	assert.Equal(t, "10.0.0.1:443", d.last())
	assert.Equal(t, int32(1), r.calls.Load(), "the refresh the first dial abandoned was the one that answered")
}

func TestDNSCache_ADialAfterTheTTLTakesTheStaleAnswerWhileTheRefreshRuns(t *testing.T) {
	r := &fakeResolver{answers: map[string][]string{}}
	r.set("s3.lyve.example", "10.0.0.1")
	clock := &fakeClock{t: time.Unix(1_800_000_000, 0)}
	d := &fakeDial{}
	dial := newTestDNS(r, clock).dialer(d.dial)
	dialOnce(t, dial, "s3.lyve.example:443")

	r.block = make(chan struct{}) // the refresh after the TTL hangs
	clock.add(6 * time.Minute)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	c, err := dial(ctx, "tcp", "s3.lyve.example:443")
	require.NoError(t, err, "the stale answer serves the dial at once while the refresh hangs")
	_ = c.Close()
	assert.Less(t, time.Since(start), 50*time.Millisecond, "no wait on the resolver at all")
	assert.Equal(t, "10.0.0.1:443", d.last())
	require.Eventually(t, func() bool { return r.calls.Load() == 2 }, time.Second, time.Millisecond, "the refresh was started beside it")
	close(r.block)
}

func TestDNSCache_TheLookupItselfIsBounded(t *testing.T) {
	r := &fakeResolver{answers: map[string][]string{}}
	hung := func(ctx context.Context, host string) ([]string, error) {
		r.calls.Add(1)
		<-ctx.Done() // a resolver that never answers: only its own bound ends it
		return nil, ctx.Err()
	}
	c := &dnsDialCache{lookup: hung, ttl: 5 * time.Minute, now: time.Now, lookupTimeout: 50 * time.Millisecond}
	d := &fakeDial{}
	dial := c.dialer(d.dial)

	start := time.Now()
	c1, err := dial(context.Background(), "tcp", "s3.lyve.example:443")
	require.NoError(t, err, "no answer within the lookup's bound: the name is dialled as given")
	_ = c1.Close()
	assert.Less(t, time.Since(start), time.Second, "a caller without a deadline is still not held by the resolver")
	assert.Equal(t, "s3.lyve.example:443", d.last())
	assert.Equal(t, int32(1), r.calls.Load())
}

func TestDNSCache_TheDefaultLookupBoundIsFiveSeconds(t *testing.T) {
	assert.Equal(t, 5*time.Second, (&dnsDialCache{}).lookupBound())
	assert.Equal(t, 5*time.Second, sharedDNS.lookupBound(), "the shared cache: a hung resolver costs a dial at most this, once per host per TTL")
}
