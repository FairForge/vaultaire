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
	assert.Equal(t, "10.0.0.9:443", d.last(), "after the TTL the name is asked again")
	assert.Equal(t, int32(2), r.calls.Load())
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
	assert.Equal(t, errs0+1, promtest.ToFloat64(driverDNSRefreshes.WithLabelValues("s3.lyve.example", "error")))
	assert.Equal(t, int32(2), r.calls.Load())
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
