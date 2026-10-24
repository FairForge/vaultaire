package drivers

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"go.uber.org/zap"
	"golang.org/x/sync/singleflight"
)

// The drivers' DNS cache (WP-R7-6), shared by every S3-class driver
// (OneDrive keeps its own odDNSCache).
//
// It used to store a host's answer for the life of the process and dial
// addrs[0] every time: prod dialled ONE Wasabi address (.101) from boot on
// (2026-10-17) and would do the same to Lyve — a vendor that moved the name,
// or one bad edge node, was ours until a restart. Now:
//
//   - an answer expires after DRIVER_DNS_TTL (5 min); the next dial asks
//     again, once for every dial waiting on it (singleflight), and a
//     refresh that fails keeps the stale answer — a resolver outage must
//     not take the drivers down (the answer is still the vendor's);
//   - a dial never waits on a refresh: once an answer exists, the dial
//     after the TTL starts the refresh and takes the stale answer while it
//     runs (the one after it gets the new one); only a host with no answer
//     yet waits, and within the dial's own context — the lookup itself is
//     bounded by dnsLookupTimeout (5 s), never by a dial's context (a refresh
//     the first dial abandoned still lands for the next). So a hedge "at 2 s
//     on a fresh connection" and a probe's budget are bounded by their
//     callers, not by the resolver (the post-merge review of #748 saw a
//     100 ms dial wait a whole hung lookup);
//   - the dials rotate over the answer's addresses (round-robin per host);
//   - a dial that fails on one address tries the next within the same call,
//     until one connects, every address has been tried, or the loop's ONE
//     budget ends — the dialer's Timeout (10 s) across every address, each
//     attempt given the time left over the addresses left, and the caller's
//     context when it is shorter (Prompt 10 Part 0.4: the Timeout applied
//     per address, 4 lost SYNs = 40 s).
//
// vaultaire_driver_dns_refreshes_total{host,outcome} counts the lookups
// (ok | error).

const driverDNSTTLDefault = 5 * time.Minute

// dnsLookupTimeout bounds one lookup of the cache: a hung resolver costs a
// dial at most this, once per host per TTL (Go's own resolver gives up after
// resolv.conf's timeout × attempts, ~10 s with one nameserver — a bound the
// box's config holds, not this code).
const dnsLookupTimeout = 5 * time.Second

var driverDNSRefreshes = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "vaultaire_driver_dns_refreshes_total",
	Help: "Lookups of an S3-class driver's endpoint by the drivers' DNS cache (one per host per DRIVER_DNS_TTL), by host and outcome (ok, error: the stale answer is kept).",
}, []string{"host", "outcome"})

// driverDNSTTLFromEnv reads DRIVER_DNS_TTL: a Go duration between 30 s and
// 1 h. Anything else is logged at Warn and the default kept (R13-19).
func driverDNSTTLFromEnv(logger *zap.Logger) time.Duration {
	v := strings.TrimSpace(os.Getenv("DRIVER_DNS_TTL"))
	if v == "" {
		return driverDNSTTLDefault
	}
	d, err := time.ParseDuration(v)
	if err != nil || d < 30*time.Second || d > time.Hour {
		logger.Warn("invalid DRIVER_DNS_TTL (need a duration 30s..1h such as 5m), keeping default",
			zap.String("value", v), zap.Duration("default", driverDNSTTLDefault))
		return driverDNSTTLDefault
	}
	return d
}

// ConfigureDriverDNS sets the shared cache's TTL from DRIVER_DNS_TTL; the
// server calls it once at boot, before the drivers dial.
func ConfigureDriverDNS(logger *zap.Logger) {
	ttl := driverDNSTTLFromEnv(logger)
	sharedDNS.mu.Lock()
	sharedDNS.ttl = ttl
	sharedDNS.mu.Unlock()
}

type dnsEntry struct {
	addrs   []string
	expires time.Time
	next    atomic.Uint32 // the round-robin cursor
}

// dnsDialCache is the cache: one entry per host.
type dnsDialCache struct {
	lookup func(ctx context.Context, host string) ([]string, error)
	now    func() time.Time

	mu            sync.Mutex
	ttl           time.Duration
	lookupTimeout time.Duration // 0 = dnsLookupTimeout
	// dialAttemptMin is the least time one address of the loop is given
	// (0 = dialAttemptMinDefault).
	dialAttemptMin time.Duration
	entries        map[string]*dnsEntry
	flight         singleflight.Group
	// beforeFlight, when set, runs between the read of the cache and the
	// singleflight call (a test seam: RP56-2's race lives there).
	beforeFlight func(host string)
}

// lookupBound is how long one lookup may take.
func (c *dnsDialCache) lookupBound() time.Duration {
	if c.lookupTimeout > 0 {
		return c.lookupTimeout
	}
	return dnsLookupTimeout
}

// dialAttemptMinDefault is the least one address of the dial loop is given
// (net.Dialer's own floor between the addresses of a name).
const dialAttemptMinDefault = 2 * time.Second

func (c *dnsDialCache) attemptMin() time.Duration {
	if c.dialAttemptMin > 0 {
		return c.dialAttemptMin
	}
	return dialAttemptMinDefault
}

var sharedDNS = &dnsDialCache{lookup: net.DefaultResolver.LookupHost, ttl: driverDNSTTLDefault, now: time.Now}

// entry is host's current answer: the cached one inside its TTL; past it
// the stale one at once while a refresh runs (one per host however many
// dials trigger it; a refresh that fails keeps it); for a host with no
// answer yet, the lookup's result, waited for within the dial's context.
// nil = no answer at all (the caller dials the name itself, through the
// system resolver, or gets its context's error).
func (c *dnsDialCache) entry(ctx context.Context, host string) *dnsEntry {
	c.mu.Lock()
	e := c.entries[host]
	ttl := c.ttl
	c.mu.Unlock()
	if e != nil && c.now().Before(e.expires) {
		return e
	}
	if c.beforeFlight != nil {
		c.beforeFlight(host)
	}
	ch := c.flight.DoChan(host, func() (any, error) {
		// Another dial's flight may have stored a fresh answer between the
		// read above and this call (RP56-2): it serves, no second lookup.
		c.mu.Lock()
		cur := c.entries[host]
		c.mu.Unlock()
		if cur != nil && c.now().Before(cur.expires) {
			return cur, nil
		}
		// A refresh the waiting dial abandoned still completes for the next:
		// under its own bound, never the dial's context.
		lctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), c.lookupBound())
		defer cancel()
		addrs, err := c.lookup(lctx, host)
		if err != nil || len(addrs) == 0 {
			driverDNSRefreshes.WithLabelValues(host, "error").Inc()
			return e, nil
		}
		driverDNSRefreshes.WithLabelValues(host, "ok").Inc()
		fresh := &dnsEntry{addrs: addrs, expires: c.now().Add(ttl)}
		if e != nil {
			fresh.next.Store(e.next.Load())
		}
		c.mu.Lock()
		if c.entries == nil {
			c.entries = map[string]*dnsEntry{}
		}
		c.entries[host] = fresh
		c.mu.Unlock()
		return fresh, nil
	})
	if e != nil {
		return e // stale while the refresh runs
	}
	select {
	case r := <-ch:
		got, _ := r.Val.(*dnsEntry)
		return got
	case <-ctx.Done():
		return nil // the dial's own deadline: the dialer returns its error
	}
}

// dialer dials addr through the cache: the next address of the answer in
// turn, and on failure the ones after it. budget (the dialer's Timeout; 0 =
// none) bounds the whole loop, not each address: net.Dialer applies its
// Timeout per call, and the loop used to call it once per address — four
// addresses whose SYNs are lost held one dial 4 × 10 s before the header
// bound even started (Prompt 10 Part 0.4). Each attempt gets the time left
// shared out over the addresses left (attemptContext), so a blackholed first
// address cannot spend the budget before the good one is asked.
func (c *dnsDialCache) dialer(dial func(ctx context.Context, network, addr string) (net.Conn, error), budget time.Duration) func(ctx context.Context, network, addr string) (net.Conn, error) {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		if budget > 0 {
			var cancel context.CancelFunc
			ctx, cancel = context.WithTimeout(ctx, budget)
			defer cancel()
		}
		host, port, err := net.SplitHostPort(addr)
		if err != nil || net.ParseIP(host) != nil {
			return dial(ctx, network, addr)
		}
		e := c.entry(ctx, host)
		if e == nil {
			return dial(ctx, network, addr)
		}
		n := len(e.addrs)
		start := int(e.next.Add(1)-1) % n
		var errs []error
		for i := 0; i < n; i++ {
			actx, acancel := c.attemptContext(ctx, n-i)
			conn, derr := dial(actx, network, net.JoinHostPort(e.addrs[(start+i)%n], port))
			acancel() // a connected conn outlives its dial's context
			if derr == nil {
				return conn, nil
			}
			errs = append(errs, derr)
			if ctx.Err() != nil {
				break
			}
		}
		return nil, fmt.Errorf("dial %s (%d address(es) tried): %w", host, len(errs), errors.Join(errs...))
	}
}

// attemptContext is one address's share of the loop's deadline, the rule
// net.Dialer uses between the addresses of one name: the time left divided
// by the addresses left, never under attemptMin (2 s, about two SYN
// retransmits) unless less than that is left. No deadline = ctx as is.
func (c *dnsDialCache) attemptContext(ctx context.Context, addrsLeft int) (context.Context, context.CancelFunc) {
	deadline, ok := ctx.Deadline()
	if !ok || addrsLeft <= 1 {
		return ctx, func() {}
	}
	left := time.Until(deadline)
	share := left / time.Duration(addrsLeft)
	if floor := c.attemptMin(); share < floor {
		share = min(floor, left)
	}
	return context.WithTimeout(ctx, share)
}
