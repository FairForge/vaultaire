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
	"path/filepath"
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
//   - a read (Get, GetRange, Exists) of an IMMUTABLE name (immutableObject:
//     a parity shard `<digest>/<etag>/p<j>`, a pack `<aa>/<sha256>.pack`;
//     inside the driver a stripe piece and a generation's key file) whose
//     bridge is unhealthy — the last probe failed, or 3 consecutive transport
//     errors / stalls / timeouts / 5xx-after-retries opened its breaker for
//     30 s, or this very call failed that way — falls back to the next
//     bridge in the key's HRW order: whatever another bridge holds under
//     that name is the right bytes. A fallback bridge's NOT FOUND is never
//     reported as not found: the object may have been written through the
//     dead bridge seconds ago. It is ErrWebDAVBridgeStale, which wraps
//     engine.ErrAllBackendsUnavailable (the API answers 503 + Retry-After,
//     the client retries);
//   - a read of a MUTABLE name (an object's `%o` file, a striped object's
//     `%s` manifest) is answered by its routed bridge only (Prompt 2b A1: a
//     fallback that still saw the PREVIOUS version — an overwrite reaches
//     the other bridges after ~30 s, once 310 s; a delete after ~30 s —
//     served old bytes under the new head row's ETag, or a deleted object
//     "existed"). Routed bridge unavailable = ErrWebDAVBridgeDown (a 503 +
//     Retry-After), never another bridge's copy;
//   - writes never fail over: a Put or Delete whose bridge is down fails
//     (immutable, content-addressed callers — parity shards, packs — retry
//     later). Writing through another bridge would leave the key's own
//     bridge stale for up to minutes;
//   - List and WalkTenant — the erasure sweep — union the listings of EVERY
//     bridge (each object once) and fail when any bridge cannot list (the
//     sweep then defers the tenant). No product path calls List on this
//     driver (engine.List reads the primary, which `sync` can never be; the
//     sweep walks, the parity reconcile uses ListDir), so the union is the
//     safe default, not a hot path. A walked object's Remove goes to the
//     bridge that listed it and to its routed bridge;
//   - Delete asks the other bridges when the routed one does not see the
//     object (a write through another bridge a moment ago, a changed bridge
//     set) and deletes it wherever it is seen; a bridge that cannot answer
//     then fails the delete (Prompt 2b A4: a routed miss was a success and
//     the bytes stayed);
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
	// stripeHeartbeat (≤ 0 = only at the start and before the commit),
	// stripeGrace (the reaper's, for the upload's own freshness check) and
	// stripeRetireGrace: Prompt 2b B1/B4.
	stripeHeartbeat   time.Duration
	stripeGrace       time.Duration
	stripeRetireGrace time.Duration
	largePerBridge    int
	// keyLocks serialises the commits of one key (Prompt 2b B3).
	keyLocks keyLocker
	// Test hooks: before the pieces are verified, after the manifest is
	// written, before the reaper's deletes.
	beforeVerify, afterManifest, beforeReapDelete func()
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

// ErrWebDAVBridgeDown is a read of a mutable name whose bridge is
// unavailable: another bridge may hold an older version, so none is asked.
// It wraps engine.ErrAllBackendsUnavailable: a retryable 503.
var ErrWebDAVBridgeDown = fmt.Errorf("%w: the object's bridge is unavailable and another bridge may hold an older version", engine.ErrAllBackendsUnavailable)

var (
	webdavBridgeUp = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "vaultaire_webdav_bridge_up",
		Help: "1 when the last probe of a WebDAV backend's bridge succeeded, 0 when it failed, by backend and bridge index.",
	}, []string{"backend", "bridge"})
	webdavDeleteElsewhere = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "vaultaire_webdav_delete_elsewhere_total",
		Help: "Deletes on a multi-bridge WebDAV backend whose object was not on its routed bridge but on another (a write through another bridge a moment ago, a changed bridge set), by backend.",
	}, []string{"backend"})
	webdavFallbackReads = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "vaultaire_webdav_fallback_reads_total",
		Help: "Reads a multi-bridge WebDAV backend sent to another bridge than the key's, by backend and outcome (served, stale_miss, failed), and reads of a mutable name refused because its own bridge was unavailable (routed_down).",
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
		stripeMin: cfg.StripeMin, stripePiece: cfg.StripePiece, stagingDir: cfg.StagingDir,
		stripeHeartbeat: WebDAVDefaultStripeHeartbeat, largePerBridge: large}
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
		// One folder per process (`p<pid>`): the two slots share /tmp (the
		// unit has no PrivateTmp), so a boot sweep removes only the folders
		// of processes that are gone — never the other slot's in-flight
		// pieces (Prompt 2b B5).
		root := m.stagingDir
		if err := os.MkdirAll(root, 0o700); err != nil {
			return nil, fmt.Errorf("webdav: stripe staging dir %s: %w", root, err)
		}
		sweepStaging(root, logger)
		m.stagingDir = filepath.Join(root, fmt.Sprintf("p%d", os.Getpid()))
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
	webdavDeleteElsewhere.WithLabelValues(name)
	for _, o := range []string{"served", "stale_miss", "failed", "routed_down"} {
		webdavFallbackReads.WithLabelValues(name, o)
	}
	initStripeSeries(name)
	logger.Info("WebDAV backend spread over bridges",
		zap.String("backend", name), zap.Int("bridges", len(m.bridges)), zap.Int("large_concurrency", large),
		zap.Int64("stripe_min", m.stripeMin), zap.Int64("stripe_piece", m.stripePiece), zap.String("staging_dir", m.stagingDir))
	return m, nil
}

// sweepStaging removes what dead processes left in the staging root: the
// `p<pid>` folder of a process that no longer exists, and a staging file of
// the shared-root layout older than webdavStagingStale. Best effort.
func sweepStaging(root string, logger *zap.Logger) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return
	}
	removed := 0
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() && strings.HasPrefix(name, "p") {
			pid, err := strconv.Atoi(name[1:])
			if err != nil || pid <= 0 || pid == os.Getpid() || pidAlive(pid) {
				continue
			}
			if os.RemoveAll(filepath.Join(root, name)) == nil {
				removed++
			}
			continue
		}
		if !e.IsDir() && strings.HasPrefix(name, "piece-") {
			if info, err := e.Info(); err == nil && time.Since(info.ModTime()) > webdavStagingStale {
				if os.Remove(filepath.Join(root, name)) == nil {
					removed++
				}
			}
		}
	}
	if removed > 0 {
		logger.Info("webdav stripe: staging left by stopped processes removed", zap.String("dir", root), zap.Int("entries", removed))
	}
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
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, engine.ErrInvalidInput) || errors.Is(err, errWebDAVSource) {
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

// readRouted runs op on the key's own bridge only: the read of a mutable
// name. When that bridge is marked down (and another is not) or answers as
// an unavailable bridge, the read is ErrWebDAVBridgeDown — never another
// bridge's copy, which may be an older version.
func readRouted[T any](ctx context.Context, m *MultiWebDAVDriver, what string, order []int, op func(b *webdavBridge) (T, error)) (T, error) {
	var zero T
	routed := order[0]
	b := m.bridges[routed]
	if !m.healthy(b) && m.anyHealthyBut(routed) {
		webdavFallbackReads.WithLabelValues(m.name, "routed_down").Inc()
		return zero, fmt.Errorf("%s %s: bridge %d (the key's) is marked down: %w", m.name, what, routed, ErrWebDAVBridgeDown)
	}
	v, err := op(b)
	m.note(ctx, b, err)
	if ctx.Err() == nil && bridgeUnavailable(err) {
		webdavFallbackReads.WithLabelValues(m.name, "routed_down").Inc()
		return zero, fmt.Errorf("%s %s: bridge %d (the key's) failed (%s): %w", m.name, what, routed, err.Error(), ErrWebDAVBridgeDown)
	}
	return v, err
}

func (m *MultiWebDAVDriver) anyHealthyBut(skip int) bool {
	for i, b := range m.bridges {
		if i != skip && m.healthy(b) {
			return true
		}
	}
	return false
}

// immutableObject reports an object name whose bytes never change once
// written (the content is in the name), so any bridge's copy is the right
// one: a vault parity shard (`<tenant>__parity` — a bucket name no S3
// bucket can take — `<24 hex>/<etag>/p<j>`) and a pack (tenant `_global`,
// container `_packs`, `<aa>/<64 hex>.pack`).
func immutableObject(tenantID, container, artifact string) bool {
	segs := strings.Split(artifact, "/")
	switch {
	case strings.HasSuffix(container, "__parity"):
		return len(segs) == 3 && len(segs[0]) == 24 && isLowerHex(segs[0]) && segs[1] != "" &&
			len(segs[2]) >= 2 && segs[2][0] == 'p' && isDigits(segs[2][1:])
	case tenantID == engine.ChunkAddressTenant && container == "_packs":
		sum := strings.TrimSuffix(segs[len(segs)-1], ".pack")
		return len(segs) == 2 && strings.HasSuffix(segs[1], ".pack") && len(sum) == 64 && isLowerHex(sum) && segs[0] == sum[:2]
	}
	return false
}

func isLowerHex(s string) bool {
	for i := 0; i < len(s); i++ {
		if !('0' <= s[i] && s[i] <= '9' || 'a' <= s[i] && s[i] <= 'f') {
			return false
		}
	}
	return s != ""
}

func isDigits(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return s != ""
}

// readObject is the read of an object name: with the fallback for an
// immutable one, from its own bridge only for a mutable one.
func readObject[T any](ctx context.Context, m *MultiWebDAVDriver, container, artifact, what string, order []int,
	op func(b *webdavBridge) (T, error), missed func(T, error) bool) (T, error) {
	if immutableObject(contextTenant(ctx), container, artifact) {
		return readFrom(ctx, m, what, order, op, missed)
	}
	return readRouted(ctx, m, what, order, op)
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
	// The write and the removal of a striped version are one commit under
	// the key's lock, like the striped path's (Prompt 2b B3).
	unlock := m.keyLocks.lock(manifestCacheKey(names))
	defer unlock()
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

// dropManifest deletes the key's manifest on its bridge b (if any) and
// retires its generation (the reaper deletes it an hour later, so reads
// already streaming it finish — Prompt 2b B4).
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
		m.retireStripe(ctx, key, man) // reads already streaming it finish (Prompt 2b B4)
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
	return readObject(ctx, m, container, artifact, "get "+key, order,
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
	return readObject(ctx, m, container, artifact, "get range "+key, order,
		func(b *webdavBridge) (io.ReadCloser, error) {
			return m.openOn(ctx, b, key, names, nf, offset, length, false)
		},
		func(_ io.ReadCloser, err error) bool { return notFoundErr(err) })
}

// Exists asks the key's bridge — or, for an immutable name, a fallback,
// whose "no" is ErrWebDAVBridgeStale; never true from another bridge for a
// mutable name (ErrWebDAVBridgeDown).
func (m *MultiWebDAVDriver) Exists(ctx context.Context, container, artifact string) (bool, error) {
	key, names, order, err := m.resolve(ctx, "Exists", container, artifact)
	if errors.Is(err, errNameTooLong) {
		return false, nil // never stored
	}
	if err != nil {
		return false, err
	}
	return readObject(ctx, m, container, artifact, "exists "+key, order,
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

// Delete removes the object through its routed bridge. When that bridge
// does not see it (neither the `%o` file nor a manifest) — a write through
// another bridge a moment ago, or a bridge-set change that re-routed the
// key — every other bridge is asked (a PROPFIND each, answered from the
// bridge's metadata cache) and the object is deleted wherever it is seen;
// a bridge that cannot answer then fails the delete: a miss is never
// "deleted" on one bridge's word (Prompt 2b A4).
func (m *MultiWebDAVDriver) Delete(ctx context.Context, container, artifact string) error {
	key, names, order, err := m.resolve(ctx, "Delete", container, artifact)
	if errors.Is(err, errNameTooLong) {
		return nil // never stored
	}
	if err != nil {
		return err
	}
	m.mcache.drop(manifestCacheKey(names))
	unlock := m.keyLocks.lock(manifestCacheKey(names))
	defer unlock()
	b := m.bridges[order[0]]
	found, err := m.deleteOn(ctx, b, key, names, container, artifact)
	m.note(ctx, b, err)
	if err != nil {
		return fmt.Errorf("bridge %d: %w", b.idx, err)
	}
	if found {
		return nil
	}
	var errs []error
	for _, i := range order[1:] {
		o := m.bridges[i]
		seen, err := m.deleteOn(ctx, o, key, names, container, artifact)
		m.note(ctx, o, err)
		if err != nil {
			errs = append(errs, fmt.Errorf("bridge %d: %w", o.idx, err))
			continue
		}
		if seen {
			webdavDeleteElsewhere.WithLabelValues(m.name).Inc()
			m.logger.Info("webdav delete: the object was on another bridge than its routed one",
				zap.String("backend", m.name), zap.String("key", key), zap.Int("bridge", o.idx), zap.Int("routed", b.idx))
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("%s delete %s: the routed bridge %d does not see the object and another bridge could not be asked: %w: %w",
			m.name, key, b.idx, errors.Join(errs...), ErrWebDAVBridgeDown)
	}
	return nil
}

// deleteOn deletes the object at names as bridge b sees it — the `%o` file
// and a striped version's manifest and pieces; found says whether b saw
// either.
func (m *MultiWebDAVDriver) deleteOn(ctx context.Context, b *webdavBridge, key string, names []string, container, artifact string) (bool, error) {
	e, found, err := b.drv.stat(ctx, names)
	if err != nil {
		return false, err
	}
	plain := found && !e.dir
	if plain {
		if err := b.drv.removeNames(ctx, names); err != nil {
			return true, err
		}
	}
	me, mfound, err := b.drv.stat(ctx, manifestNamesOf(names))
	if err != nil {
		return plain, err
	}
	if !mfound || me.dir {
		return plain, nil
	}
	return true, m.dropManifest(ctx, b, key, names, container, artifact) // manifest, then pieces
}

// RemoveEmptyDir removes the folder at dir under container when it holds
// nothing — on the ONE account every bridge mounts. A collection DELETE is
// recursive (RFC 4918 §9.6.1) and a bridge lags the others' writes by
// seconds to minutes, so one bridge's empty view proves nothing: every
// bridge must list the folder empty first (the first that does not stops
// it, ErrDirNotEmpty), then ONE DELETE goes through one bridge. A missing
// folder is fine.
//
// Nothing here can see a write that lands between the last empty listing
// and the DELETE: it goes with the folder. The callers make that window
// empty — the vault parity job is the only writer of its shards and the
// only remover of its folders, under one lock; the stripe reaper removes
// only first-layout key folders, which no upload writes any more. The
// listing after the DELETE sees only what came after it, which survived:
// logged at Warn, not a failure.
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
			m.logger.Warn("webdav: something was written into a folder right after its removal — it survived (the folder exists again)",
				zap.String("backend", m.name), zap.String("folder", strings.Join(names, "/")), zap.Int("bridge", other.idx))
			return nil
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

// List is the union of every bridge's listing of the container (each key
// once, sorted), so a key written through any bridge is listed even while
// the others cannot see it yet; a bridge that cannot list fails it — a
// caller acting on "not listed" must never act on one bridge's view. A key
// deleted a moment ago may still be listed by a lagging bridge. No product
// path calls it today (engine.List reads the primary, which `sync` can
// never be; the erasure sweep walks with WalkTenant, the parity reconcile
// lists with ListDir) — Prompt 2b A3.
func (m *MultiWebDAVDriver) List(ctx context.Context, container, prefix string) ([]string, error) {
	tenantID, err := requireTenant(ctx, m.name, "List", "", m.logger)
	if err != nil {
		return nil, err
	}
	if _, err := tenantNames(tenantID, container); err != nil {
		return nil, fmt.Errorf("%s list %s: %w", m.name, container, err)
	}
	seen := map[string]struct{}{}
	for _, b := range m.bridges {
		keys, err := b.drv.List(ctx, container, prefix)
		m.note(ctx, b, err)
		if err != nil {
			return nil, fmt.Errorf("%s list %s: bridge %d: %w", m.name, tenantKey(tenantID, container, prefix), b.idx, err)
		}
		for _, k := range keys {
			seen[k] = struct{}{}
		}
	}
	out := make([]string, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	sort.Strings(out)
	return out, nil
}

// WalkTenant implements engine.TenantWalker for the erasure sweep: the union
// of every bridge's walk of `t-<tenant>/` (an object written through any
// bridge is found even while the others cannot see it yet), each object
// once, its Remove on the bridge that listed it and on its routed bridge. A bridge that cannot
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
			// Removed through the bridge that listed it (it sees the file)
			// and through its routed bridge (a manifest goes where its
			// object would): the routed bridge alone answered 404 for a file
			// written through another bridge a moment ago, and the bytes
			// stayed (Prompt 2b A4).
			lister, routed := b, m.bridges[m.rank(routingNames(names))[0]]
			return fn(tenantObject(names, func(ctx context.Context) error {
				if err := lister.drv.removeNames(ctx, names); err != nil {
					return err
				}
				if routed != lister {
					return routed.drv.removeNames(ctx, names)
				}
				return nil
			}))
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
