package drivers

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"go.uber.org/zap"

	"github.com/FairForge/vaultaire/internal/engine"
)

// MultiWebDAVDriver is one backend (`sync`) spread over several WebDAV
// servers that serve the SAME folder — Sync.com's bridge (`sync-webdav`) is
// one single-core Node process per device profile, so the box runs several
// (127.0.0.1:4918…4922), each with its own password, all mounted on the same
// Sync folder. Live on SLC (2026-10-07), 256 MiB objects: 1 bridge 30/84
// MB/s PUT/GET; 5 bridges × 3 concurrent 156/450 MB/s, 0 stalls.
//
// What the design rests on — cross-bridge staleness: every bridge caches
// metadata. A file written through bridge A is visible through bridge B
// after 1.5–13 s, an overwrite after ~30 s (once 310 s), a delete after
// ~30 s. Within ONE bridge every read is consistent. So:
//
//   - every key lives on ONE bridge: rendezvous (HRW) hashing of the key's
//     full path below the root (`t-<tenant>/<container>/<artifact…>`) over
//     the bridges' URLs. Put, Get, GetRange, Exists and Delete go to it;
//     adding or removing a bridge moves only ~1/N of the keys (after such a
//     change a moved key may read stale until Sync has carried the old
//     bridge's last writes over — minutes at worst);
//   - a read (Get, GetRange, Exists, List) whose bridge is unhealthy — the
//     last probe failed, or 3 consecutive transport errors / stalls /
//     timeouts / 5xx-after-retries opened its breaker for 30 s, or this very
//     call failed that way — falls back to the next bridge in the key's HRW
//     order. A fallback bridge's NOT FOUND is never reported as not found:
//     the object may have been written through the dead bridge seconds ago.
//     It is ErrWebDAVBridgeStale, which wraps engine.ErrAllBackendsUnavailable
//     (the API answers 503 + Retry-After, the client retries);
//   - writes never fail over: a Put or Delete whose bridge is down fails
//     (immutable, content-addressed callers — parity shards, packs — retry
//     later). Writing through another bridge would leave the key's own
//     bridge stale for up to minutes;
//   - List reads one bridge (the container's HRW bridge, with the same
//     fallback): it may miss an object written through another bridge in
//     the last seconds (none is written through another bridge while the
//     bridge set is unchanged). WalkTenant — the erasure sweep — unions the
//     listings of EVERY bridge (each object once) and fails when any bridge
//     cannot list (the sweep then defers the tenant); each object's Remove
//     goes to its routed bridge;
//   - HealthCheck probes every bridge: healthy while ≥ 1 is; per-bridge
//     state in vaultaire_webdav_bridge_up{backend,bridge}.
//
// Each bridge is a full WebDAVDriver (its own credentials, concurrency cap,
// idle timeouts, retries, metrics labelled bridge="<index>"), plus a cap on
// large transfers: a body of ≥ 16 MiB (or of unknown length) takes one of
// LargeConcurrency slots (default 3, SYNC_WEBDAV_LARGE_CONCURRENCY) per
// bridge and direction — the bridge is CPU-bound; 3 large transfers each
// way saturate it. Uploads and downloads have separate slots, so a copy
// from a key to another key of the same bridge cannot deadlock.
//
// Never a server modtime: Sync's bridge reports a wrong getlastmodified for
// every file (propfindBody asks for none).
type MultiWebDAVDriver struct {
	name    string
	bridges []*webdavBridge
	ids     []string // HRW seeds (bridgeID of each URL), by index
	logger  *zap.Logger
	now     func() time.Time

	// Striping (webdav_stripe.go): a known-length Put ≥ stripeMin (≤ 0 =
	// never) is stored as stripePiece pieces staged in stripeSlots files.
	stripeMin   int64
	stripePiece int64
	stagingDir  string
	stripeSlots chan struct{}
	staged      atomic.Int64 // staging files now
	stagedPeak  atomic.Int64 // most staging files at once (tests)
	mcache      *manifestCache
	// stripeSettle is the pause between the cleanup passes of a failed
	// striped upload (an in-flight piece a bridge stores late).
	stripeSettle time.Duration
}

// webdavBridge is one server of a MultiWebDAVDriver.
type webdavBridge struct {
	idx       int
	label     string // the metrics' bridge label: the index
	drv       *WebDAVDriver
	largeUp   chan struct{}
	largeDown chan struct{}

	probeDown atomic.Bool  // the last HealthCheck failed
	failures  atomic.Int32 // consecutive unavailable answers
	openUntil atomic.Int64 // unix nanos: skipped for reads until then
}

const (
	// WebDAVDefaultLargeConcurrency is the large transfers per bridge and
	// direction.
	WebDAVDefaultLargeConcurrency = 3
	// WebDAVLargeTransfer is the body size from which a transfer is large.
	WebDAVLargeTransfer = 16 << 20

	// bridgeTripAfter consecutive unavailable answers open a bridge's
	// breaker for bridgeCooldown (reads skip it; writes still try it).
	bridgeTripAfter = 3
	bridgeCooldown  = 30 * time.Second
)

// ErrWebDAVBridgeStale is a read whose bridge was unavailable and whose
// fallback bridge did not see the object — possibly only not YET (bridges
// see each other's writes after seconds to minutes). It wraps
// engine.ErrAllBackendsUnavailable: never a miss, a retryable 503.
var ErrWebDAVBridgeStale = fmt.Errorf("%w: the object's bridge is unavailable and the other bridges may not see it yet", engine.ErrAllBackendsUnavailable)

var (
	webdavBridgeUp = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "vaultaire_webdav_bridge_up",
		Help: "1 when the last probe of a WebDAV backend's bridge succeeded, 0 when it failed, by backend and bridge index.",
	}, []string{"backend", "bridge"})
	webdavFallbackReads = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "vaultaire_webdav_fallback_reads_total",
		Help: "Reads a multi-bridge WebDAV backend sent to another bridge than the key's, by backend and outcome (served, stale_miss, failed).",
	}, []string{"backend", "outcome"})
)

// NewMultiWebDAVDriver builds the backend over cfg.Bridges (1..16, distinct
// URLs, a password each) with cfg.User and cfg.Root. cfg.Options() and opts
// tune every bridge's limits; cfg.LargeConcurrency (0 = 3) caps its large
// transfers.
func NewMultiWebDAVDriver(name string, cfg WebDAVConfig, logger *zap.Logger, opts ...WebDAVOption) (*MultiWebDAVDriver, error) {
	if strings.TrimSpace(name) == "" {
		return nil, errors.New("webdav: a backend name is required")
	}
	if err := checkWebDAVBridges(name, cfg.Bridges); err != nil {
		return nil, err
	}
	if logger == nil {
		logger = zap.NewNop()
	}
	large := cfg.LargeConcurrency
	if large < 1 {
		large = WebDAVDefaultLargeConcurrency
	}
	m := &MultiWebDAVDriver{name: name, logger: logger, now: time.Now,
		stripeMin: cfg.StripeMin, stripePiece: cfg.StripePiece, stagingDir: cfg.StagingDir}
	if m.stripePiece <= 0 {
		m.stripePiece = WebDAVDefaultStripePiece
	}
	if m.stripeMin == 0 {
		m.stripeMin = WebDAVDefaultStripeMin
	}
	if m.stripeMin > 0 && m.stripeMin < m.stripePiece {
		return nil, fmt.Errorf("webdav: stripe minimum %d is below the piece size %d", m.stripeMin, m.stripePiece)
	}
	if m.stagingDir == "" {
		m.stagingDir = defaultStagingDir()
	}
	if m.stripeMin > 0 {
		if err := os.MkdirAll(m.stagingDir, 0o700); err != nil {
			return nil, fmt.Errorf("webdav: stripe staging dir %s: %w", m.stagingDir, err)
		}
	}
	m.stripeSlots = make(chan struct{}, len(cfg.Bridges)*large)
	m.stripeSettle = webdavDefaultStripeSettle
	m.mcache = newManifestCache(webdavManifestCacheEntries, webdavManifestCacheTTL, func() time.Time { return m.now() })
	for i, bc := range cfg.Bridges {
		label := strconv.Itoa(i)
		all := append(append(cfg.Options(), opts...), withWebDAVBridge(label))
		drv, err := NewWebDAVDriver(name, bc.URL, cfg.User, bc.Password, cfg.Root, logger, all...)
		if err != nil {
			return nil, fmt.Errorf("bridge %d: %w", i, err)
		}
		u, err := parseWebDAVURL(name, bc.URL)
		if err != nil {
			return nil, fmt.Errorf("bridge %d: %w", i, err)
		}
		b := &webdavBridge{idx: i, label: label, drv: drv,
			largeUp: make(chan struct{}, large), largeDown: make(chan struct{}, large)}
		drv.bodyGate = b.gateDownload
		webdavBridgeUp.WithLabelValues(name, label).Set(1)
		m.bridges = append(m.bridges, b)
		m.ids = append(m.ids, bridgeID(u))
	}
	for _, o := range []string{"served", "stale_miss", "failed"} {
		webdavFallbackReads.WithLabelValues(name, o)
	}
	initStripeSeries(name)
	logger.Info("WebDAV backend spread over bridges",
		zap.String("backend", name), zap.Int("bridges", len(m.bridges)), zap.Int("large_concurrency", large),
		zap.Int64("stripe_min", m.stripeMin), zap.Int64("stripe_piece", m.stripePiece), zap.String("staging_dir", m.stagingDir))
	return m, nil
}

// withWebDAVBridge sets the metrics' bridge label.
func withWebDAVBridge(label string) WebDAVOption {
	return func(s *webdavSettings) { s.bridge = label }
}

// --- HRW ----------------------------------------------------------------------

// hrwRank orders the bridges for key, best first: rendezvous hashing — each
// bridge scores hash(id, key), highest wins. A bridge's keys move only when
// it leaves; a new bridge only takes keys.
func hrwRank(ids []string, key string) []int {
	type scored struct {
		idx   int
		score uint64
	}
	s := make([]scored, len(ids))
	for i, id := range ids {
		h := fnv.New64a()
		_, _ = h.Write([]byte(id))
		_, _ = h.Write([]byte{0})
		_, _ = h.Write([]byte(key))
		s[i] = scored{i, mix64(h.Sum64())}
	}
	sort.Slice(s, func(a, b int) bool {
		if s[a].score != s[b].score {
			return s[a].score > s[b].score
		}
		return s[a].idx < s[b].idx
	})
	out := make([]int, len(s))
	for i, x := range s {
		out[i] = x.idx
	}
	return out
}

// mix64 is splitmix64's finalizer: FNV's high bits alone spread poorly.
func mix64(x uint64) uint64 {
	x ^= x >> 30
	x *= 0xbf58476d1ce4e5b9
	x ^= x >> 27
	x *= 0x94d049bb133111eb
	x ^= x >> 31
	return x
}

// rank is the HRW order of resource names (below the root).
func (m *MultiWebDAVDriver) rank(names []string) []int {
	return hrwRank(m.ids, strings.Join(names, "/"))
}

// object resolves a call to its tenant key (for errors), its resource names
// and its bridges in HRW order.
func (m *MultiWebDAVDriver) object(ctx context.Context, op, container, artifact string) (string, []int, error) {
	key, _, order, err := m.resolve(ctx, op, container, artifact)
	return key, order, err
}

// resolve is object plus the object's resource names (leaf `%o`).
func (m *MultiWebDAVDriver) resolve(ctx context.Context, op, container, artifact string) (string, []string, []int, error) {
	tenantID, err := requireTenant(ctx, m.name, op, "", m.logger)
	if err != nil {
		return "", nil, nil, err
	}
	names, err := objectNames(tenantID, container, artifact)
	if err == nil {
		err = checkNames(manifestNamesOf(names))
	}
	if err == nil {
		err = checkNames(names)
	}
	if err != nil {
		return "", nil, nil, fmt.Errorf("%s %s %s/%s: %w", m.name, op, container, artifact, err)
	}
	return tenantKey(tenantID, container, artifact), names, m.rank(names), nil
}

// bridgeFor is the index of the bridge a key lives on.
func (m *MultiWebDAVDriver) bridgeFor(ctx context.Context, container, artifact string) (int, error) {
	_, order, err := m.object(ctx, "route", container, artifact)
	if err != nil {
		return 0, err
	}
	return order[0], nil
}

// --- bridge health ---------------------------------------------------------------

func (m *MultiWebDAVDriver) healthy(b *webdavBridge) bool {
	return !b.probeDown.Load() && m.now().UnixNano() >= b.openUntil.Load()
}

// bridgeUnavailable: the error says the bridge is in trouble (refused,
// reset, a stall, a timeout, a 5xx/423 left after the retries) — not that
// the object is missing, the request was refused (4xx) or the caller left.
func bridgeUnavailable(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, engine.ErrInvalidInput) {
		return false
	}
	var nf engine.NotFoundError
	if errors.As(err, &nf) {
		return false
	}
	if errors.Is(err, engine.ErrTimeout) {
		return true
	}
	var se *webdavStatusError
	if errors.As(err, &se) {
		return se.code >= 500 || se.code == http.StatusLocked
	}
	var ue *url.Error
	var ne net.Error
	return errors.As(err, &ue) || errors.As(err, &ne) || errors.Is(err, io.ErrUnexpectedEOF)
}

// note records an answer of b: an unavailable one counts toward its
// breaker, anything else closes it.
func (m *MultiWebDAVDriver) note(ctx context.Context, b *webdavBridge, err error) {
	if ctx.Err() != nil {
		return // the caller left: says nothing about the bridge
	}
	if !bridgeUnavailable(err) {
		b.failures.Store(0)
		return
	}
	if b.failures.Add(1) >= bridgeTripAfter {
		until := m.now().Add(bridgeCooldown)
		if b.openUntil.Swap(until.UnixNano()) < m.now().UnixNano() {
			m.logger.Warn("webdav bridge unavailable — reads go to the next bridge for a while",
				zap.String("backend", m.name), zap.Int("bridge", b.idx), zap.Duration("for", bridgeCooldown), zap.Error(err))
		}
	}
}

// --- large transfers -------------------------------------------------------------

func isLarge(size int64) bool { return size <= 0 || size >= WebDAVLargeTransfer }

// gateDownload holds a large-download slot for a GET body of size bytes
// (-1 unknown) until its Close.
func (b *webdavBridge) gateDownload(ctx context.Context, size int64) (func(), error) {
	if size >= 0 && size < WebDAVLargeTransfer {
		return nil, nil
	}
	return acquireSlot(ctx, b.largeDown, fmt.Sprintf("large-download slots of bridge %d", b.idx))
}

func acquireSlot(ctx context.Context, sem chan struct{}, what string) (func(), error) {
	select {
	case sem <- struct{}{}:
		var once sync.Once
		return func() { once.Do(func() { <-sem }) }, nil
	case <-ctx.Done():
		return nil, fmt.Errorf("waiting for one of %d %s: %w", cap(sem), what, ctx.Err())
	}
}

// --- reads with fallback -----------------------------------------------------------

// readFrom runs op on the key's bridge (order[0]) and, when that bridge is
// unhealthy or answers as one, on the next bridges in order. A fallback's
// miss (missed) is never returned as such: ErrWebDAVBridgeStale.
func readFrom[T any](ctx context.Context, m *MultiWebDAVDriver, what string, order []int,
	op func(b *webdavBridge) (T, error), missed func(T, error) bool) (T, error) {
	var zero T
	cands := make([]int, 0, len(order))
	for _, i := range order {
		if m.healthy(m.bridges[i]) {
			cands = append(cands, i)
		}
	}
	if len(cands) == 0 {
		cands = order[:1] // nothing looks healthy: ask the key's own bridge
	}
	routed := order[0]
	var routedErr, lastErr error
	var missedOn []int
	for _, i := range cands {
		b := m.bridges[i]
		v, err := op(b)
		m.note(ctx, b, err)
		if ctx.Err() != nil {
			return v, err
		}
		if i == routed {
			if !bridgeUnavailable(err) {
				return v, err // the key's bridge answered: authoritative, a miss included
			}
			routedErr = err
			continue
		}
		switch {
		case missed(v, err):
			missedOn = append(missedOn, i)
		case err == nil:
			webdavFallbackReads.WithLabelValues(m.name, "served").Inc()
			m.logger.Debug("webdav read served by a fallback bridge", zap.String("backend", m.name),
				zap.String("what", what), zap.Int("bridge", i), zap.Int("routed", routed))
			return v, nil
		default:
			lastErr = err
		}
	}
	reason := "is marked down"
	if routedErr != nil {
		reason = "failed: " + routedErr.Error()
	}
	if len(missedOn) > 0 {
		webdavFallbackReads.WithLabelValues(m.name, "stale_miss").Inc()
		return zero, fmt.Errorf("%s %s: bridge %d (the key's) %s; bridges %v do not see the object, possibly not yet: %w",
			m.name, what, routed, reason, missedOn, ErrWebDAVBridgeStale)
	}
	if routedErr != nil && lastErr == nil {
		return zero, fmt.Errorf("bridge %d: %w", routed, routedErr)
	}
	if lastErr == nil {
		lastErr = errors.New("no bridge answered")
	}
	webdavFallbackReads.WithLabelValues(m.name, "failed").Inc()
	return zero, fmt.Errorf("%s %s: bridge %d (the key's) %s; fallback: %w", m.name, what, routed, reason, lastErr)
}

func notFoundErr(err error) bool {
	var nf engine.NotFoundError
	return err != nil && errors.As(err, &nf)
}

// --- engine.Driver -------------------------------------------------------------------

// Name returns the engine backend name.
func (m *MultiWebDAVDriver) Name() string { return m.name }

// Bridges is the number of bridges.
func (m *MultiWebDAVDriver) Bridges() int { return len(m.bridges) }

// Put writes through the key's bridge only (no failover: see the type).
// A body of ≥ 16 MiB or of unknown length waits for a large-upload slot.
func (m *MultiWebDAVDriver) Put(ctx context.Context, container, artifact string, data io.Reader, opts ...engine.PutOption) error {
	key, names, order, err := m.resolve(ctx, "Put", container, artifact)
	if err != nil {
		return err
	}
	o := engine.ApplyPutOptions(opts...)
	m.mcache.drop(manifestCacheKey(names)) // whatever this write ends as, the cached version is not it
	if m.stripes(o) {
		return m.putStriped(ctx, key, names, order, container, artifact, data, o)
	}
	b := m.bridges[order[0]]
	if isLarge(o.ContentLength) {
		release, err := acquireSlot(ctx, b.largeUp, fmt.Sprintf("large-upload slots of bridge %d", b.idx))
		if err != nil {
			return fmt.Errorf("%s put %s: %w", m.name, key, err)
		}
		defer release()
	}
	err = b.drv.Put(ctx, container, artifact, data, opts...)
	m.note(ctx, b, err)
	if err != nil {
		return fmt.Errorf("bridge %d: %w", b.idx, err)
	}
	// A striped previous version: its manifest would be shadowed by the
	// plain file now, but its pieces must go.
	if err := m.dropManifest(ctx, b, key, names, container, artifact); err != nil {
		return fmt.Errorf("bridge %d: the object is stored, the previous striped version not removed: %w: %w", b.idx, err, engine.ErrNoFailover)
	}
	return nil
}

// dropManifest deletes the key's manifest on its bridge b (if any), then
// its pieces (best effort: what stays is the reaper's).
func (m *MultiWebDAVDriver) dropManifest(ctx context.Context, b *webdavBridge, key string, names []string, container, artifact string) error {
	m.mcache.drop(manifestCacheKey(names))
	e, found, err := b.drv.stat(ctx, manifestNamesOf(names))
	if err != nil {
		return err
	}
	if !found || e.dir {
		return nil
	}
	man, err := m.readManifestOn(ctx, b, key, names, engine.ErrNotFound(container, artifact))
	if notFoundErr(err) {
		return nil
	}
	if err != nil {
		m.logger.Warn("webdav stripe: unreadable manifest removed — its pieces are left to the reaper",
			zap.String("backend", m.name), zap.String("key", key), zap.Error(err))
		man = nil
	}
	if err := b.drv.removeNames(ctx, manifestNamesOf(names)); err != nil {
		return err
	}
	if man != nil {
		if err := m.deleteStripe(ctx, key, man); err != nil {
			webdavStripeOrphans.WithLabelValues(m.name, "left").Inc()
			m.logger.Warn("webdav stripe: pieces not deleted — left to the reaper",
				zap.String("backend", m.name), zap.String("key", key), zap.String("gen", man.Gen), zap.Error(err))
		}
	}
	return nil
}

// Get streams the object from its bridge, or from a fallback (see readFrom).
func (m *MultiWebDAVDriver) Get(ctx context.Context, container, artifact string) (io.ReadCloser, error) {
	key, names, order, err := m.resolve(ctx, "Get", container, artifact)
	if err != nil {
		return nil, unstorableMiss(err, container, artifact)
	}
	nf := engine.ErrNotFound(container, artifact)
	return readFrom(ctx, m, "get "+key, order,
		func(b *webdavBridge) (io.ReadCloser, error) { return m.openOn(ctx, b, key, names, nf, 0, 0, true) },
		func(_ io.ReadCloser, err error) bool { return notFoundErr(err) })
}

// GetRange implements engine.RangeGetter, like Get.
func (m *MultiWebDAVDriver) GetRange(ctx context.Context, container, artifact string, offset, length int64) (io.ReadCloser, error) {
	key, names, order, err := m.resolve(ctx, "GetRange", container, artifact)
	if err != nil {
		return nil, unstorableMiss(err, container, artifact)
	}
	if offset < 0 {
		return nil, fmt.Errorf("%s get range %s: %w: negative offset", m.name, key, engine.ErrInvalidInput)
	}
	nf := engine.ErrNotFound(container, artifact)
	return readFrom(ctx, m, "get range "+key, order,
		func(b *webdavBridge) (io.ReadCloser, error) {
			return m.openOn(ctx, b, key, names, nf, offset, length, false)
		},
		func(_ io.ReadCloser, err error) bool { return notFoundErr(err) })
}

// Exists asks the key's bridge, or a fallback; a fallback's "no" is
// ErrWebDAVBridgeStale.
func (m *MultiWebDAVDriver) Exists(ctx context.Context, container, artifact string) (bool, error) {
	key, names, order, err := m.resolve(ctx, "Exists", container, artifact)
	if errors.Is(err, errNameTooLong) {
		return false, nil // never stored
	}
	if err != nil {
		return false, err
	}
	return readFrom(ctx, m, "exists "+key, order,
		func(b *webdavBridge) (bool, error) {
			ok, err := b.drv.Exists(ctx, container, artifact)
			if err != nil || ok {
				return ok, err
			}
			e, found, err := b.drv.stat(ctx, manifestNamesOf(names))
			return found && !e.dir, err
		},
		func(ok bool, err error) bool { return err == nil && !ok })
}

// Delete removes the object through its bridge only.
func (m *MultiWebDAVDriver) Delete(ctx context.Context, container, artifact string) error {
	key, names, order, err := m.resolve(ctx, "Delete", container, artifact)
	if errors.Is(err, errNameTooLong) {
		return nil // never stored
	}
	if err != nil {
		return err
	}
	b := m.bridges[order[0]]
	m.mcache.drop(manifestCacheKey(names))
	err = b.drv.Delete(ctx, container, artifact)
	if err == nil {
		err = m.dropManifest(ctx, b, key, names, container, artifact) // a striped object: manifest, then pieces
	}
	m.note(ctx, b, err)
	if err != nil {
		return fmt.Errorf("bridge %d: %w", b.idx, err)
	}
	return nil
}

// List lists the container through ONE bridge (the container's HRW bridge,
// with the read fallback). It may miss an object another bridge wrote in
// the last seconds to minutes (cross-bridge staleness); WalkTenant does not.
// RemoveEmptyDir removes the folder at dir under container when it holds
// nothing — on the ONE account every bridge mounts. A collection DELETE is
// recursive (RFC 4918 §9.6.1) and a bridge lags the others' writes by
// seconds to minutes, so one bridge's empty view proves nothing: every
// bridge must list the folder empty first (the first that does not stops
// it, ErrDirNotEmpty), then ONE DELETE goes through one bridge, then every
// bridge lists it again (something that appeared in between is reported —
// it cannot be undone). A missing folder is fine.
func (m *MultiWebDAVDriver) RemoveEmptyDir(ctx context.Context, container, dir string) error {
	tenantID, err := requireTenant(ctx, m.name, "RemoveEmptyDir", "", m.logger)
	if err != nil {
		return err
	}
	names, err := dirNames(tenantID, container, dir)
	if err != nil {
		return fmt.Errorf("%s remove dir %s: %w", m.name, dir, err)
	}
	if err := m.removeEmptyNames(ctx, names); err != nil {
		return fmt.Errorf("%s remove dir %s: %w", m.name, dir, err)
	}
	return nil
}

// removeEmptyNames is RemoveEmptyDir on resource names (also the stripe
// reaper's key folders).
func (m *MultiWebDAVDriver) removeEmptyNames(ctx context.Context, names []string) error {
	order := m.rank(names)
	for _, i := range order {
		b := m.bridges[i]
		empty, err := b.drv.emptyDir(ctx, names)
		m.note(ctx, b, err)
		if err != nil {
			return fmt.Errorf("bridge %d: %w", b.idx, err)
		}
		if !empty {
			return fmt.Errorf("bridge %d: %w", b.idx, ErrDirNotEmpty)
		}
	}
	b := m.bridges[order[0]]
	err := b.drv.removeDir(ctx, names)
	m.note(ctx, b, err)
	if err != nil {
		return fmt.Errorf("bridge %d: %w", b.idx, err)
	}
	for _, other := range m.bridges {
		other.drv.forgetCollections(other.drv.parentPaths(append(append([]string(nil), names...), "_")))
	}
	for _, i := range order {
		other := m.bridges[i]
		if empty, err := other.drv.emptyDir(ctx, names); err == nil && !empty {
			m.logger.Error("webdav: something appeared in a folder while it was being removed — it went with the folder",
				zap.String("backend", m.name), zap.String("folder", strings.Join(names, "/")), zap.Int("bridge", other.idx))
			return fmt.Errorf("bridge %d: content appeared during the removal of %s", other.idx, strings.Join(names, "/"))
		}
	}
	return nil
}

// ListDir lists the direct members of the folder dir under container ("" =
// the container) through ONE bridge (the folder's HRW bridge, with the read
// fallback): sub-folders and object names, in key space. A bridge may not
// see another's last writes yet — callers that delete on what it says go
// through RemoveEmptyDir, which asks every bridge.
func (m *MultiWebDAVDriver) ListDir(ctx context.Context, container, dir string) (dirs, files []string, err error) {
	tenantID, err := requireTenant(ctx, m.name, "ListDir", "", m.logger)
	if err != nil {
		return nil, nil, err
	}
	names, err := listDirNames(tenantID, container, dir)
	if err != nil {
		return nil, nil, fmt.Errorf("%s list dir %s: %w", m.name, dir, err)
	}
	type listing struct{ dirs, files []string }
	l, err := readFrom(ctx, m, "list dir "+strings.Join(names, "/"), m.rank(names),
		func(b *webdavBridge) (listing, error) {
			d, f, err := b.drv.listDirNames(ctx, names)
			return listing{d, f}, err
		},
		func(listing, error) bool { return false })
	return l.dirs, l.files, err
}

func (m *MultiWebDAVDriver) List(ctx context.Context, container, prefix string) ([]string, error) {
	tenantID, err := requireTenant(ctx, m.name, "List", "", m.logger)
	if err != nil {
		return nil, err
	}
	names, err := tenantNames(tenantID, container)
	if err != nil {
		return nil, fmt.Errorf("%s list %s: %w", m.name, container, err)
	}
	return readFrom(ctx, m, "list "+tenantKey(tenantID, container, prefix), m.rank(names),
		func(b *webdavBridge) ([]string, error) { return b.drv.List(ctx, container, prefix) },
		func([]string, error) bool { return false })
}

// WalkTenant implements engine.TenantWalker for the erasure sweep: the union
// of every bridge's walk of `t-<tenant>/` (an object written through any
// bridge is found even while the others cannot see it yet), each object
// once, its Remove on the object's routed bridge. A bridge that cannot
// list fails the walk — the sweep defers the tenant rather than call it
// erased while a bridge may still name an object.
func (m *MultiWebDAVDriver) WalkTenant(ctx context.Context, tenantID string, fn func(engine.TenantObject) error) error {
	if err := checkWalkTenant(tenantID); err != nil {
		return err
	}
	seen := map[string]struct{}{}
	for _, b := range m.bridges {
		err := b.drv.walkTenantNames(ctx, tenantID, func(names []string) error {
			k := strings.Join(names, "/")
			if _, dup := seen[k]; dup {
				return nil
			}
			seen[k] = struct{}{}
			routed := m.bridges[m.rank(routingNames(names))[0]] // a manifest goes where its object would
			return fn(tenantObject(names, func(ctx context.Context) error { return routed.drv.removeNames(ctx, names) }))
		})
		m.note(ctx, b, err)
		if err != nil {
			return fmt.Errorf("bridge %d: %w", b.idx, err)
		}
	}
	return nil
}

// HealthCheck probes every bridge (in parallel): nil while at least one is
// up. Each bridge's state goes to vaultaire_webdav_bridge_up and decides
// whether reads skip it; a degraded backend is logged at each change.
func (m *MultiWebDAVDriver) HealthCheck(ctx context.Context) error {
	errs := make([]error, len(m.bridges))
	var wg sync.WaitGroup
	for i, b := range m.bridges {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = b.drv.HealthCheck(ctx)
		}()
	}
	wg.Wait()
	var down []string
	for i, b := range m.bridges {
		up := errs[i] == nil
		if was := !b.probeDown.Swap(!up); was != up {
			if up {
				m.logger.Info("webdav bridge back up", zap.String("backend", m.name), zap.Int("bridge", i))
			} else {
				m.logger.Warn("webdav bridge down — reads of its keys go to the next bridge, its writes fail",
					zap.String("backend", m.name), zap.Int("bridge", i), zap.Error(errs[i]))
			}
		}
		if up {
			b.failures.Store(0)
			b.openUntil.Store(0)
			webdavBridgeUp.WithLabelValues(m.name, b.label).Set(1)
			continue
		}
		webdavBridgeUp.WithLabelValues(m.name, b.label).Set(0)
		down = append(down, fmt.Sprintf("bridge %d: %v", i, errs[i]))
	}
	if len(down) == len(m.bridges) {
		return fmt.Errorf("%s health check: every bridge is down: %s", m.name, strings.Join(down, "; "))
	}
	return nil
}

// DownBridges are the bridges whose last probe failed.
func (m *MultiWebDAVDriver) DownBridges() []int {
	var out []int
	for _, b := range m.bridges {
		if b.probeDown.Load() {
			out = append(out, b.idx)
		}
	}
	return out
}

// ObjectKey implements engine.KeyAddresser (the same on every bridge).
func (m *MultiWebDAVDriver) ObjectKey(ctx context.Context, container, artifact string) string {
	return tenantKey(contextTenant(ctx), container, artifact)
}

// StoreID implements engine.StoreIdentifier: every bridge's store, sorted
// (the configured order is not the store).
func (m *MultiWebDAVDriver) StoreID() string {
	ids := make([]string, len(m.bridges))
	for i, b := range m.bridges {
		ids[i] = b.drv.StoreID()
	}
	sort.Strings(ids)
	return "dav-multi:" + strings.Join(ids, ",")
}

// Stats sums every bridge's stall and retry counts.
func (m *MultiWebDAVDriver) Stats() WebDAVStats {
	var s WebDAVStats
	for _, b := range m.bridges {
		st := b.drv.Stats()
		s.UploadStalls += st.UploadStalls
		s.DownloadStalls += st.DownloadStalls
		s.Retries += st.Retries
	}
	return s
}
