// internal/drivers/onedrive.go
//
// OneDrive fleet storage driver using Microsoft Graph API.
// Ported from cmd/permafrost-v3/main.go (production-quality OneDrive
// implementation) with these key optimizations:
//
//   - Raw HTTP + azidentity (no Graph SDK — binary -77% smaller)
//   - Dual transport: HTTP/2 for Graph API, HTTP/1.1 for CDN downloads
//     (Go HTTP/2 flow-control bugs #54330, #47840, #63520)
//   - Multi-tenant fleet: deterministic placement (FNV-1a of path picks the
//     home account; reads probe home first, then the fleet for pre-placement
//     data), throttle tracking on every Graph call
//   - 60MB upload chunks (Microsoft's max, 35% faster than 10MB), one pooled
//     buffer per upload in flight
//   - 4MB read/write buffers
//   - Decorrelated jitter backoff on 429/503, bounded like the S3-class
//     drivers (WP-R7-4): 2 attempts on a request path, 3 under a job, every
//     wait on ctx
//   - Ranged downloads through a bounded window (internal/parfetch)
package drivers

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/FairForge/vaultaire/internal/common"
	"github.com/FairForge/vaultaire/internal/engine"
	"github.com/FairForge/vaultaire/internal/parfetch"
	"go.uber.org/zap"

	mrand "math/rand/v2"
)

const (
	graphBase = "https://graph.microsoft.com/v1.0"

	simpleUploadMax = 4 << 20 // 4 MB — above this, use upload session
	// odChunkSize is one upload-session fragment: 180 × 320 KiB =
	// 58,982,400 B (WP-FLEET-4 D-F4-3). Microsoft asks for fragments under
	// 60 MiB in multiples of 320 KiB; 60 MiB sat ON the bound. The largest
	// fragment is the fastest (~25 MB/s per session against ~13 at 20 MiB,
	// 2026-11-05).
	odChunkSize  = 180 * 320 * 1024
	odChunkAlign = 320 * 1024

	odRootFolder = "vaultaire"

	// odRangePart is one ranged read of a large Get; odAdaptiveStreams of
	// them are held at most (odRangeWindow — 64 MiB at 8 streams, Geyser's
	// bound).
	odRangePart = 8 << 20

	// odRangePartLarge / odRangePartHuge are a large Get's ranges since
	// WP-FLEET-4 D-F4-1 (odRangePartFor): 32 MiB, and 64 MiB from 1 GiB — the
	// per-user request budget is what one drive meets first, and a 256 MB
	// object cost 32 requests at 8 MiB. odRangePart stays the size of an
	// object's ranges below odRangePartLarge×2.
	odRangePartLarge = 32 << 20
	odRangePartHuge  = 64 << 20

	// odMaxRetryWait caps one wait between attempts, Retry-After included
	// (it was honoured unbounded: a 429 with Retry-After: 120 slept 120 s
	// inside a request).
	odMaxRetryWait = 30 * time.Second

	// odBurstMaxWait caps one wait between attempts at a CDN range or an
	// upload fragment (WP-FLEET-3 D-F3-2): Microsoft answers over-rate
	// traffic with 429 bursts of about a minute (the 2026-11-06 soak), so a
	// Retry-After of up to 90 s is waited for there. Graph's metadata calls
	// keep odMaxRetryWait: a 429 on a small call is cheap to repeat later.
	odBurstMaxWait = 90 * time.Second

	// odBurstBase is the first wait of a range or fragment retry with no
	// Retry-After; it doubles per attempt (2, 4, 8, 16, 32 s — a minute's
	// burst in the request path's five waits), jittered up by half.
	odBurstBase = 2 * time.Second

	// Response-header bounds by request class (RP9-1, WP-R7-4; the bound
	// invariant in CLAUDE.md): a Graph call is metadata — an S3-class meta
	// call's 15 s; a CDN range's first byte a GET's 25 s; an upload fragment
	// is answered after its body, and the LAST one commits the file (the
	// exception, like a Complete), so 2 min.
	odGraphHeaderTimeout  = 15 * time.Second
	odCDNHeaderTimeout    = 25 * time.Second
	odUploadHeaderTimeout = 2 * time.Minute

	// odProbeConcurrency is the accounts HealthCheck asks at once.
	odProbeConcurrency = 8

	// odListMaxFolders bounds the folders one List walks per account: a
	// listing past it is an error, never a truncated answer.
	odListMaxFolders = 100_000
)

// Fleet-wide TLS session cache: enables 0-RTT resumption across tenants.
// Shared so a CDN edge seen by one tenant can be resumed by the next.
var odTLSSessionCache = tls.NewLRUClientSessionCache(256)

// Fleet-wide DNS cache to avoid repeated lookups on reconnection.
var odDNSCache sync.Map

// Pooled 1MB buffers for draining response bodies efficiently.
// io.Discard implements ReaderFrom with 8KB internal buffers, which
// bypasses any buffer passed to CopyBuffer. This sink forces CopyBuffer
// to use our pooled buffer.
type odDevNull struct{}

func (odDevNull) Write(p []byte) (int, error) { return len(p), nil }

var odDrain odDevNull
var odCopyPool = sync.Pool{New: func() any { b := make([]byte, 1<<20); return &b }}

func odPooledDrain(src io.Reader) {
	bp := odCopyPool.Get().(*[]byte)
	_, _ = io.CopyBuffer(odDrain, src, *bp)
	odCopyPool.Put(bp)
}

// OneDriveDriver implements engine.Driver for a fleet of OneDrive tenants.
type OneDriveDriver struct {
	tenants []*odTenant
	logger  *zap.Logger

	// The fleet fingerprint's last verdict (onedrive_fleet.go, RP18-1): the
	// probe re-checks after an unknown one and hourly.
	// probeNext is the rotation cursor of the sampled probe (D-F11-5).
	probeMu   sync.Mutex
	probeNext int

	fleetMu        sync.Mutex
	fleetState     FleetState
	fleetAccept    string
	fleetCheckedAt time.Time
	fleetChecking  bool
	// fleetUnplaced: the last check found a marker whose data is laid out
	// for ANOTHER order (`placed_under` ≠ the current digest — an accepted
	// change not yet rebalanced, RB3-1). Reads walk while it is set,
	// whatever the verdict (onedrive_fleet.go, authoritative()).
	fleetUnplaced bool
	// fleetPlacement is the placement writes go to and reads look at first
	// (the markers' version, or their target, over the current fleet; nil
	// before any check = version 1). fleetPrevious is the placement the
	// markers say the data is laid out under while that differs (WP-FLEET-5
	// D-F5-3); nil when placed. placementSpread is PERMAFROST_PLACEMENT_SPREAD.
	fleetPlacement  *odPlacement
	fleetPrevious   *odPlacement
	placementSpread int

	identityMu    sync.Mutex
	identityIndex map[string]*odTenant // identity → account (tenantByIdentity)
}

type odTenant struct {
	name     string
	tenantID string
	clientID string
	secret   string
	userUPN  string

	cred       azcore.TokenCredential
	graphHTTP  *http.Client
	cdnHTTP    *http.Client
	uploadHTTP *http.Client

	mu           sync.Mutex
	driveID      string
	rootFolderID string

	// pinnedDriveID is TENANT_N_DRIVE_ID: used without a listing (D-F1-1).
	// otherDrives are the user's drives the listing did NOT choose (empty
	// with a pin): the fingerprint check looks for the fleet's tree there
	// when the chosen drive has no marker (onedrive_fleet.go).
	pinnedDriveID string
	otherDrives   []odDrive

	// driveMismatch is the account's last drive verdict (0 none yet, 1
	// match, 2 mismatch): an Error line on a change only.
	driveMismatch atomic.Int32

	// Token cache with mutex to prevent concurrent refresh storms.
	// Azure throttles parallel token requests from the same client.
	tokenMu   sync.Mutex
	cachedTok string
	tokExpiry time.Time // when to renew (5 min before the token ends)
	// D-F3-10: the token's own end, one renewal at a time, and a pause after
	// a failed renewal — callers keep a still-valid token meanwhile and an
	// expired one fails at once, never a renewal per request.
	tokValidUntil   time.Time
	renewing        bool
	renewDone       chan struct{}
	renewErr        error
	renewPauseUntil time.Time

	throttledUntil atomic.Int64 // UnixNano: the account's hold after a 429 (onedrive_pace.go)
	totalReqs      atomic.Int64
	throttleCount  atomic.Int64

	// RateLimit tracking from Microsoft Graph response headers.
	rateLimitRemaining atomic.Int64
	rateLimitReset     atomic.Int64

	// probeState is the account's last probe outcome (0 none yet, 1 up, 2
	// down): HealthCheck logs a change.
	probeState atomic.Int32

	// wait waits between attempts (nil = odWait); tests record the waits.
	wait func(ctx context.Context, d time.Duration) error

	// tokenOwner, when set, is the account of the same tenant and app whose
	// token this one uses (D-F11-2: one token per tenant, not per drive).
	tokenOwner *odTenant
	// probeErr is the last probe's outcome (an odProbeOutcome).
	probeErr atomic.Value
	// lastQuota is the last probe's drive.quota (an odQuota), and throttles
	// the 429/503s per minute of the last hour — the System page's fleet
	// card (WP-FLEET-10, onedrive_observe.go).
	lastQuota atomic.Value
	throttles odThrottleRing

	// pace is the account's buckets and its tenant's (onedrive_pace.go,
	// WP-FLEET-3 D-F3-5); nil = none (the 429 hold still applies).
	pace *odPace
	// rangeHedgeAfter is the base delay of a CDN range's hedge
	// (PERMAFROST_RANGE_HEDGE_AFTER, D-F3-4); 0 = no hedge.
	rangeHedgeAfter time.Duration
}

// NewOneDriveFleetDriver creates a multi-tenant OneDrive driver from
// PERMAFROST_FLEET_FILE, else the TENANT_N_ID, TENANT_N_CLIENT_ID,
// TENANT_N_SECRET, TENANT_N_USER (and TENANT_N_DRIVE_ID) slots, N=1..15
// (onedrive_scale.go, WP-FLEET-11). Both set = an error.
func NewOneDriveFleetDriver(logger *zap.Logger) (*OneDriveDriver, error) {
	specs, err := odFleetSpecs(os.Getenv, logger)
	if err != nil {
		return nil, err
	}
	paceCfg := odPaceFromEnv(os.Getenv, logger)
	hedgeAfter := odRangeHedgeFromEnv(os.Getenv, logger)
	tenants := buildAccounts(specs, paceCfg, hedgeAfter, newODClients, logger)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	tenants = initFleetDrives(ctx, logger, tenants)
	if len(tenants) == 0 {
		return nil, fmt.Errorf("onedrive: no valid tenant configurations found")
	}

	d := &OneDriveDriver{
		tenants:         tenants,
		logger:          logger,
		placementSpread: placementSpreadDefaultEnv(logger),
	}
	logger.Info("OneDrive fleet initialized", zap.Int("tenants", len(tenants)), zap.Int("placement_spread", d.placementSpread),
		zap.Float64("account_rps", paceCfg.AccountRPS), zap.Float64("account_in_gbh", paceCfg.AccountInGBh),
		zap.Float64("account_out_gbh", paceCfg.AccountOutGBh), zap.Float64("tenant_gbh", paceCfg.TenantGBh),
		zap.Duration("range_hedge_after", hedgeAfter))
	return d, nil
}

// initFleetDrives chooses every account's drive and creates its root folder
// (D-F1-1). An account whose listing names no OneDrive library (and no pin)
// is SKIPPED with an Error naming the drives found — as an incomplete slot
// is; an account Graph could not answer for is kept and retried on first use,
// as before. One Info line per kept account names its drive. Every kept
// account's drive-mismatch series starts at 0.
func initFleetDrives(ctx context.Context, logger *zap.Logger, tenants []*odTenant) []*odTenant {
	keep := make([]bool, len(tenants))
	sem := make(chan struct{}, odBootConcurrency) // WP-FLEET-11: 900 drives in a boot
	var wg sync.WaitGroup
	for idx, t := range tenants {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			keep[idx] = initFleetDrive(ctx, logger, t)
		}()
	}
	wg.Wait()
	kept := make([]*odTenant, 0, len(tenants))
	for idx, t := range tenants {
		if keep[idx] {
			InitPermafrostAccountSeries(t.name)
			InitPermafrostPaceSeries(t.name)
			kept = append(kept, t)
		}
	}
	return kept
}

// initFleetDrive is one account's boot: false = skip it.
func initFleetDrive(ctx context.Context, logger *zap.Logger, t *odTenant) bool {
	driveID, err := t.getDriveID(ctx)
	var none *odNoLibraryError
	switch {
	case errors.As(err, &none):
		logger.Error("permafrost account skipped: its user has no drive named OneDrive and TENANT_N_DRIVE_ID is not set — set it to the drive that holds vaultaire/ (onedrive_README.md § Drive selection)",
			zap.String("account", t.name), zap.String("user", t.userUPN), zap.Strings("drives", none.names()))
		return false
	case err != nil:
		logger.Warn("OneDrive tenant init failed — will retry on first use",
			zap.String("tenant", t.name), zap.Error(err))
	default:
		others := make([]string, 0, len(t.otherDrives))
		for _, o := range t.otherDrives {
			others = append(others, o.Name)
		}
		logger.Info("permafrost account drive", zap.String("account", t.name),
			zap.String("drive_id", driveID), zap.Bool("pinned", t.pinnedDriveID != ""),
			zap.Strings("other_drives", others))
		if _, err := t.ensureRootFolder(ctx, driveID); err != nil {
			logger.Warn("OneDrive root folder creation failed",
				zap.String("tenant", t.name), zap.Error(err))
		}
	}
	return true
}

// Name is the registration key main.go uses ("permafrost"). The engine records
// the registration key as an object's backend_name, so the two must never
// drift: the driver was once registered as "onedrive" and 2,613 prod head rows
// still carry that name with no driver to answer it (Review R7-10).
func (d *OneDriveDriver) Name() string { return "permafrost" }

func (d *OneDriveDriver) TenantCount() int { return len(d.tenants) }

// homeTenantIndex is placement version 1 (what the fleet did before
// WP-FLEET-5): FNV-1a of the full storage path mod fleet size, over the
// TENANT_N order. Kept as the definition version-1 markers are read by
// (onedrive_placement.go newPlacementV1 agrees with it, pinned by a test).
func (d *OneDriveDriver) homeTenantIndex(path string) int {
	h := fnv.New32a()
	_, _ = h.Write([]byte(path))
	return int(h.Sum32() % uint32(len(d.tenants)))
}

// tenantsFor returns the accounts a data operation on path asks, in order (an
// object lives on exactly ONE account — a rotating read was the 2026-07-30
// read-after-write failure): the home under the current placement, and —
// only while the fleet is not placed (an accepted order change not yet
// rebalanced, a rebalance running, no verdict yet) — the home under the
// placement the markers say the data is laid out for (WP-FLEET-5 D-F5-3: the
// previous placement, computed; it was a walk of up to eight accounts, which
// could not reach a remapped object past them). Two accounts at most, at any
// fleet size.
func (d *OneDriveDriver) tenantsFor(path string) []*odTenant {
	home := d.homeTenant(path)
	if d.authoritative() {
		// WP-FLEET-11 D-F11-3: the order is settled and the data placed
		// under it — the home account is the only one that can hold it; a
		// miss is a miss.
		return []*odTenant{home}
	}
	prev := d.previousPlacement()
	if prev == nil {
		return []*odTenant{home}
	}
	old := d.tenantByIdentity(prev.home(path).identity)
	if old == nil || old == home {
		return []*odTenant{home}
	}
	return []*odTenant{home, old}
}

// odError is a Graph answer >= 400 (other than throttling and the retried
// 5xx): the account, the status, Graph's error code ({"error":{"code":…}})
// and an excerpt of the body. Callers classify by Status and Code, never by
// the text (Prompt 2b.5 F1.1; WP-R7-4 R7-18).
type odError struct {
	tenant string
	Status int
	Code   string
	body   []byte
}

func (e *odError) Error() string {
	return fmt.Sprintf("onedrive [%s] HTTP %d: %s", e.tenant, e.Status, e.body)
}

// newODError reads Graph's error code out of an excerpt of the body.
func newODError(tenant string, status int, body []byte) *odError {
	var g struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	_ = json.Unmarshal(body, &g)
	return &odError{tenant: tenant, Status: status, Code: g.Error.Code, body: body}
}

// odIsNotFound reports whether a Graph error is an item-absence: a 404 whose
// code is itemNotFound (or a 404 with no Graph body). It read "404" in the
// error text until Prompt 2b.5 F1.1 — and the callers wrap the path into
// that text, so a key named `404.html` answered with a 500 or a 403 was
// "absent". Another 404 code (ResourceNotFound: the drive or the user gone)
// is the account's failure, never an object miss.
func odIsNotFound(err error) bool {
	var oe *odError
	return errors.As(err, &oe) && oe.Status == http.StatusNotFound &&
		(oe.Code == "" || oe.Code == "itemNotFound")
}

// logFallbackHit records an operation that found its object on a non-home
// account — pre-placement (round-robin era) data. These are the migration
// signal: when fallback hits stop appearing, the fleet-wide probe on misses
// can be retired and reads become single-account again.
func (d *OneDriveDriver) logFallbackHit(op, path string, t *odTenant) {
	d.logger.Info("onedrive fallback hit",
		zap.String("op", op),
		zap.String("path", path),
		zap.String("tenant", t.name),
	)
}

// buildPath is the fleet path of a call. A context that names no tenant is
// refused (tenant_ctx.go) — it used to resolve to "default".
func (d *OneDriveDriver) buildPath(ctx context.Context, op, container, artifact string) (string, error) {
	tenantID, err := requireTenant(ctx, d.Name(), op, "", d.logger)
	if err != nil {
		return "", err
	}
	return tenantKey(tenantID, container, artifact), nil
}

// ObjectKey is the path a call would address (engine.KeyAddresser).
func (d *OneDriveDriver) ObjectKey(ctx context.Context, container, artifact string) string {
	return tenantKey(contextTenant(ctx), container, artifact)
}

// Put stores an artifact via OneDrive Graph API.
// Files <4MB use simple PUT; larger files use chunked upload sessions.
func (d *OneDriveDriver) Put(ctx context.Context, container, artifact string, data io.Reader, opts ...engine.PutOption) error {
	path, pErr := d.buildPath(ctx, "Put", container, artifact)
	if pErr != nil {
		return pErr
	}
	// Deterministic placement: the path's home account under the current
	// placement (a rebalance's target while one runs), always — never the
	// rotation. A throttled home still receives the write (graphDo retries
	// with backoff); writing elsewhere would strand the object where reads
	// don't look first.
	t := d.homeTenant(path)
	driveID, err := t.getDriveID(ctx)
	if err != nil {
		return fmt.Errorf("onedrive get drive: %w", err)
	}
	rootID, err := t.ensureRootFolder(ctx, driveID)
	if err != nil {
		return fmt.Errorf("onedrive ensure folder: %w", err)
	}
	options := engine.ApplyPutOptions(opts...)

	// When ContentLength is known (from S3 API adapter), stream directly
	// without buffering the entire body. Unknown length: spool it to learn
	// the size (materialize: in memory up to 64 MiB, else a temp file), then
	// upload FROM the spool — re-readable, so up to 200 MiB it is one simple
	// PUT (onedrive_upload.go, WP-FLEET-4).
	if options.ContentLength > 0 {
		d.logger.Debug("onedrive put (streaming)",
			zap.String("tenant", t.name), zap.String("path", path), zap.Int64("size", options.ContentLength))
		return t.upload(ctx, driveID, rootID, path, data, options.ContentLength)
	}
	body, size, cleanup, err := materialize(data)
	if err != nil {
		return fmt.Errorf("onedrive buffer: %w", err)
	}
	defer cleanup()
	d.logger.Debug("onedrive put (materialized)",
		zap.String("tenant", t.name), zap.String("path", path), zap.Int64("size", size))
	return t.upload(ctx, driveID, rootID, path, body, size)
}

// Get retrieves an artifact from OneDrive.
// Fetches the pre-authenticated CDN URL from item metadata, then downloads
// via HTTP/1.1 (bypassing Go's HTTP/2 flow-control bugs).
func (d *OneDriveDriver) Get(ctx context.Context, container, artifact string) (io.ReadCloser, error) {
	path, pErr := d.buildPath(ctx, "Get", container, artifact)
	if pErr != nil {
		return nil, pErr
	}

	// Home tenant first, then probe the fleet: pre-placement data may live
	// on any account (the old round-robin Put).
	var (
		t        *odTenant
		dlURL    string
		fileSize int64
	)
	var lastErr, downErr error
	for i, cand := range d.tenantsFor(path) {
		if i == 1 {
			noteWalk("get")
		}
		driveID, err := cand.getDriveID(ctx)
		if err != nil {
			downErr = fmt.Errorf("onedrive get drive: %w", err)
			lastErr = downErr
			continue
		}
		dlURL, fileSize, err = cand.getDownloadURL(ctx, driveID, path)
		if err == nil {
			t = cand
			if i > 0 {
				d.logFallbackHit("get", path, cand)
			}
			break
		}
		lastErr = fmt.Errorf("onedrive get metadata %s: %w", path, err)
		if !odIsNotFound(err) {
			return nil, lastErr
		}
	}
	if t == nil {
		// Absent on every account that answered — a miss only when every
		// account answered (an unreachable one may hold it). Typed, so the
		// engine never reads the text (Prompt 2b.5 F1.1).
		if downErr == nil && odIsNotFound(lastErr) {
			return nil, fmt.Errorf("%w: %w", engine.ErrNotFound(container, artifact), lastErr)
		}
		return nil, lastErr
	}

	if fileSize == 0 {
		// Nothing to fetch; downloadRanges has no part to ask for.
		return io.NopCloser(bytes.NewReader(nil)), nil
	}
	// Every read is the ranged reader's (RB3-2): a small object is its one
	// part, fetched whole (no Range header) with the burst attempts, the
	// hedge and the budget a range gets. Until then a Get under 10 MiB was
	// one plain GET: a 429 failed it and a cut body streamed through.
	return t.downloadRanges(ctx, dlURL, fileSize, odAdaptiveStreams(fileSize))
}

// odAdaptiveStreams picks optimal parallel stream count based on file size.
// Benchmarked in permafrost-v3: 8 streams is 32% faster than 4 at 100MB.
func odAdaptiveStreams(fileSize int64) int {
	switch {
	case fileSize < 10<<20:
		return 1
	case fileSize < 25<<20:
		return 2
	case fileSize < 50<<20:
		return 4
	default:
		return 8
	}
}

// odRangeWindow is the most a large Get holds: streams × one range.
func odRangeWindow(fileSize int64) int64 {
	return int64(odRangeStreams(fileSize)) * odRangePartFor(fileSize)
}

// odRangeStreams is the ranges a Get of fileSize holds at once: the adaptive
// streams, at most odRangeWindowMax bytes of them.
func odRangeStreams(fileSize int64) int {
	return max(1, min(odAdaptiveStreams(fileSize), int(odRangeWindowMax/odRangePartFor(fileSize))))
}

// downloadRanges reads a file as odRangePart byte ranges over the HTTP/1.1
// CDN transport, `streams` of them in flight, handed to the caller in order
// through a bounded window (parfetch): at most streams × odRangePart held —
// ranges in flight, fetched and waiting, and the one being read. Every range
// read whole into memory before the first byte went out until R7-07 (a 5 GB
// GET held 5 GB). A range that is not exactly the 206 asked for fails the
// stream: never a splice.
func (t *odTenant) downloadRanges(ctx context.Context, dlURL string, fileSize int64, streams int) (io.ReadCloser, error) {
	part := odRangePartFor(fileSize)
	streams = min(streams, odRangeStreams(fileSize))
	n := int((fileSize + part - 1) / part)
	sizes := make([]int64, n)
	for i := range sizes {
		sizes[i] = part
	}
	sizes[n-1] = fileSize - int64(n-1)*part
	cfg := parfetch.Config{
		Window:      int64(streams) * part,
		MaxParts:    streams,
		StrictSizes: true,
		// Every fleet Get shares one budget past its first range (WP-FLEET-4):
		// under load a Get degrades to fewer ranges, never more memory.
		Budget:    odRangeBudget,
		FreeParts: 1,
	}
	times := &odRangeTimes{}
	stream := parfetch.Start(ctx, cfg, sizes, func(ctx context.Context, i int) ([]byte, error) {
		return t.fetchRange(ctx, dlURL, int64(i)*part, sizes[i], fileSize, times)
	})
	return parfetch.NewReader(stream, nil), nil
}

// fetchRange is one ranged CDN read of size bytes at start, with the burst
// attempts (odBurstAttempts, WP-FLEET-3 D-F3-2): a transport error, a body
// cut short, a 429/503 (Retry-After honoured to odBurstMaxWait) or another
// 5xx is asked again — the same range of the same URL. A 200 (the CDN
// ignored Range), a Content-Range other than the one asked, any other 4xx,
// and a caller already gone are not: no attempt would differ.
//
// Paced (onedrive_pace.go): the range's bytes are taken from the account's
// and its tenant's egress once, before the first attempt; each attempt
// waits out the account's hold and takes a request token. A slow attempt is
// hedged (fetchRangeHedged).
func (t *odTenant) fetchRange(ctx context.Context, dlURL string, start, size, total int64, times *odRangeTimes) ([]byte, error) {
	end := start + size - 1
	if err := t.paceOut(ctx, size); err != nil {
		return nil, fmt.Errorf("onedrive range %d-%d: %w", start, end, err)
	}
	attempts := odBurstAttempts(ctx)
	var lastErr error
	var covered time.Time
	for attempt := 0; attempt < attempts; attempt++ {
		if err := t.paceRequestFrom(ctx, covered); err != nil {
			if lastErr != nil {
				return nil, fmt.Errorf("onedrive range %d-%d: %w (after %w)", start, end, err, lastErr)
			}
			return nil, fmt.Errorf("onedrive range %d-%d: %w", start, end, err)
		}
		buf, retryAfter, retry, err := t.fetchRangeHedged(ctx, dlURL, start, end, size, total, times)
		if err == nil {
			return buf, nil
		}
		if !retry {
			return nil, err
		}
		lastErr = err
		if attempt == attempts-1 {
			break
		}
		c, err := t.burstRetry(ctx, attempt+1, retryAfter, lastErr)
		if err != nil {
			return nil, err
		}
		covered = c
	}
	return nil, fmt.Errorf("onedrive range %d-%d: %d attempts: %w", start, end, attempts, lastErr)
}

// fetchRangeOnce is one attempt at a range: its bytes, or the error, whether
// another attempt may differ, and the answer's Retry-After. A range that IS
// the whole object (a small Get's one part, RB3-2) is asked for with no
// Range header and must answer 200 with exactly the object's bytes (the
// soak's `got 0` — a 200 with no bytes — is a body that did not come, asked
// again; one byte more is another object, never served) — the CDN's answer
// to a full-span Range is not relied on.
func (t *odTenant) fetchRangeOnce(ctx context.Context, dlURL string, start, end, size, total int64) (buf []byte, retryAfter time.Duration, retry bool, err error) {
	whole := start == 0 && size == total
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, dlURL, nil)
	if err != nil {
		return nil, 0, false, fmt.Errorf("onedrive range request: %w", err)
	}
	if !whole {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", start, end))
	}
	req.Header.Set("User-Agent", odUserAgent())
	resp, err := t.cdnHTTP.Do(req)
	t.totalReqs.Add(1)
	if err != nil {
		return nil, 0, ctx.Err() == nil, fmt.Errorf("onedrive range %d-%d: %w", start, end, err)
	}
	defer func() { _ = resp.Body.Close() }()
	switch {
	case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == http.StatusServiceUnavailable:
		ra := odRetryAfter(resp.Header)
		t.hold("range", ra)
		odPooledDrain(resp.Body)
		return nil, ra, true, fmt.Errorf("onedrive range %d-%d: HTTP %d (throttled)", start, end, resp.StatusCode)
	case resp.StatusCode >= 500:
		odPooledDrain(resp.Body)
		return nil, 0, true, fmt.Errorf("onedrive range %d-%d: HTTP %d", start, end, resp.StatusCode)
	case whole && resp.StatusCode != http.StatusOK:
		return nil, 0, false, fmt.Errorf("onedrive download of %d bytes: HTTP %d, not 200", size, resp.StatusCode)
	case !whole && resp.StatusCode != http.StatusPartialContent:
		// Never drained: a 200 is the whole object.
		return nil, 0, false, fmt.Errorf("onedrive range %d-%d: HTTP %d, not 206", start, end, resp.StatusCode)
	}
	if !whole {
		if want, got := fmt.Sprintf("bytes %d-%d/%d", start, end, total), resp.Header.Get("Content-Range"); got != want {
			return nil, 0, false, fmt.Errorf("onedrive range %d-%d: Content-Range %q, asked %q", start, end, got, want)
		}
	}
	buf = make([]byte, size)
	if _, err := io.ReadFull(resp.Body, buf); err != nil {
		// An empty body is io.EOF, a short one io.ErrUnexpectedEOF: both a
		// range that did not come, never the end of the object.
		if errors.Is(err, io.EOF) {
			err = io.ErrUnexpectedEOF
		}
		return nil, 0, ctx.Err() == nil, fmt.Errorf("onedrive range %d-%d: body: %w", start, end, err)
	}
	if whole {
		// A Content-Range pins a range's length; a whole body is pinned by
		// its end: one more byte is another object's, never served and
		// never asked again.
		switch n, err := resp.Body.Read(make([]byte, 1)); {
		case n > 0:
			return nil, 0, false, fmt.Errorf("onedrive download of %d bytes: the body is longer", size)
		case err != nil && !errors.Is(err, io.EOF):
			return nil, 0, ctx.Err() == nil, fmt.Errorf("onedrive download of %d bytes: the body's end: %w", size, err)
		}
	}
	return buf, 0, false, nil
}

// Delete removes an artifact from OneDrive — from the accounts tenantsFor
// names: the home account alone while the fleet is settled and its data
// placed under the current order (WP-FLEET-11 D-F11-3, RB3-1), else the
// home and up to seven more, every copy found. The walk exists because the
// old round-robin Put could leave copies of the same path on several
// accounts (every overwrite landed wherever the rotation pointed), and an
// accepted order change leaves objects on their old homes until a
// rebalance: deleting only one copy leaves a stale duplicate that the next
// walking Get resurrects.
func (d *OneDriveDriver) Delete(ctx context.Context, container, artifact string) error {
	path, pErr := d.buildPath(ctx, "Delete", container, artifact)
	if pErr != nil {
		return pErr
	}

	deleted := false
	var hardErr error  // an account we could not fully check — a copy may survive there
	var probeErr error // 404s from the sweep, reported only if nothing was deleted
	for i, t := range d.tenantsFor(path) {
		if i == 1 {
			noteWalk("delete")
		}
		driveID, err := t.getDriveID(ctx)
		if err != nil {
			hardErr = fmt.Errorf("onedrive get drive: %w", err)
			continue
		}
		itemID, err := t.getItemID(ctx, driveID, path)
		if err != nil {
			if odIsNotFound(err) {
				probeErr = fmt.Errorf("onedrive resolve %s: %w", path, err)
				continue
			}
			hardErr = fmt.Errorf("onedrive resolve %s: %w", path, err)
			continue
		}

		if err := t.deleteItem(ctx, driveID, itemID); err != nil {
			hardErr = fmt.Errorf("onedrive delete %s: %w", path, err)
			continue
		}
		deleted = true
		if i > 0 {
			d.logFallbackHit("delete", path, t)
		}
	}
	// A hard error means an account may still hold a copy — surface it so the
	// engine retries (Delete is idempotent; re-deleting swept accounts is safe).
	if hardErr != nil {
		return hardErr
	}
	if deleted {
		return nil
	}
	if probeErr != nil {
		return fmt.Errorf("%w: %w", engine.ErrNotFound(container, artifact), probeErr)
	}
	return nil
}

// List returns every key under a container — relative to it, `prefix`
// honoured, all pages — the union of the fleet. Placement scatters a
// container's objects across the WHOLE fleet (each object hashes to its own
// home tenant), so a listing must union every account. A key with '/' is a
// folder tree on Graph: the walk descends every folder that can hold a key
// under prefix (R7-06: List returned the container's direct children — a
// folder's name in place of its keys, nested keys never). A folder that
// cannot be listed fails the listing; never a partial answer.
func (d *OneDriveDriver) List(ctx context.Context, container string, prefix string) ([]string, error) {
	tenantID, tErr := requireTenant(ctx, d.Name(), "List", "", d.logger)
	if tErr != nil {
		return nil, tErr
	}
	base := fmt.Sprintf("%s/t-%s/%s", odRootFolder, tenantID, container)
	seen := make(map[string]struct{})
	var keys []string
	for _, t := range d.tenants {
		driveID, err := t.getDriveID(ctx)
		if err != nil {
			return nil, fmt.Errorf("onedrive get drive: %w", err)
		}
		err = t.walkFiles(ctx, driveID, base, prefix, func(key string) {
			if _, dup := seen[key]; !dup {
				seen[key] = struct{}{}
				keys = append(keys, key)
			}
		})
		if err != nil {
			return nil, fmt.Errorf("onedrive list %s: %w", base, err)
		}
	}
	return keys, nil
}

// walkFiles calls fn with every file under base on this account, relative
// to base, whose name starts with prefix; a folder is descended only when a
// key under prefix can be in it. A folder that is gone (deleted since its
// parent was listed) holds nothing.
func (t *odTenant) walkFiles(ctx context.Context, driveID, base, prefix string, fn func(key string)) error {
	queue := []string{""}
	for walked := 0; len(queue) > 0; walked++ {
		if walked >= odListMaxFolders {
			return fmt.Errorf("more than %d folders on account %s", odListMaxFolders, t.name)
		}
		rel := queue[0]
		queue = queue[1:]
		folder := base
		if rel != "" {
			folder += "/" + rel
		}
		err := t.listChildren(ctx, driveID, folder, func(c odChild) {
			key := c.name
			if rel != "" {
				key = rel + "/" + c.name
			}
			switch {
			case c.folder:
				if dir := key + "/"; strings.HasPrefix(dir, prefix) || strings.HasPrefix(prefix, dir) {
					queue = append(queue, key)
				}
			case strings.HasPrefix(key, prefix):
				fn(key)
			}
		})
		if err != nil && !odIsNotFound(err) {
			return err
		}
	}
	return nil
}

// RemoveContainerTree removes the container's folder — the empty folders
// under it too, whose names are the keys' prefixes — on every account, when
// no account holds a key in it (List is empty): ErrDirNotEmpty otherwise.
// The account-erasure sweep calls it after deleting every key List returned
// (a folder's permanentDelete is recursive: what a write landed between that
// List and the delete goes with it — an erased tenant's). A folder missing
// everywhere is fine.
func (d *OneDriveDriver) RemoveContainerTree(ctx context.Context, container string) error {
	tenantID, tErr := requireTenant(ctx, d.Name(), "RemoveContainerTree", "", d.logger)
	if tErr != nil {
		return tErr
	}
	if container == "" || strings.Contains(container, "/") {
		return fmt.Errorf("onedrive remove container: %q is not a container", container)
	}
	keys, err := d.List(ctx, container, "")
	if err != nil {
		return fmt.Errorf("onedrive remove container %s: %w", container, err)
	}
	if len(keys) > 0 {
		return fmt.Errorf("onedrive remove container %s: %d key(s) left: %w", container, len(keys), ErrDirNotEmpty)
	}
	path := fmt.Sprintf("t-%s/%s", tenantID, container)
	for _, t := range d.tenants {
		driveID, err := t.getDriveID(ctx)
		if err != nil {
			return fmt.Errorf("onedrive get drive: %w", err)
		}
		itemID, err := t.getItemID(ctx, driveID, path)
		if odIsNotFound(err) {
			continue
		}
		if err != nil {
			return fmt.Errorf("onedrive remove container %s: %w", container, err)
		}
		if err := t.deleteItem(ctx, driveID, itemID); err != nil {
			return fmt.Errorf("onedrive remove container %s on %s: %w", container, t.name, err)
		}
	}
	return nil
}

// deleteItem removes the item for good (WP-FLEET-2, D-F1-5): POST
// …/permanentDelete — no recycle-bin entry. A Graph DELETE parked the file in
// the user's recycle bin for 93 days against the drive's quota (68 / 23 /
// 21 GiB on the three fleet drives after one bench day). A folder goes with
// everything under it. A 404 is already gone; a 403 or a 5xx past the
// retries is the error it always was. Every delete the driver makes goes
// through here (TestPermanentDelete_NoGraphDeleteRequestInTheDriver).
func (t *odTenant) deleteItem(ctx context.Context, driveID, itemID string) error {
	u := fmt.Sprintf("%s/drives/%s/items/%s/permanentDelete", graphBase, driveID, itemID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, http.NoBody)
	if err != nil {
		return fmt.Errorf("onedrive permanent delete request: %w", err)
	}
	resp, err := t.graphDo(ctx, req)
	if odIsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	odPooledDrain(resp.Body)
	_ = resp.Body.Close()
	return nil
}

// ListDir lists the direct members of the folder dir under container ("" =
// the container): sub-folder names and file names. A GROUP folder (odGroupDepth
// folders under the container — the parity layout's `<digest>/<etag>/`, whose
// members are files) is listed on the accounts placement names for it — the
// group's spread set, one listing each (WP-FLEET-5 D-F5-4); a shallower folder,
// whose members are folders that may sit anywhere, is the union of every
// account (a folder on any account is a folder). The vault parity reconcile
// walks `<digest>/<etag>/` with it — List alone sees only the container's
// direct children.
func (d *OneDriveDriver) ListDir(ctx context.Context, container, dir string) (dirs, files []string, err error) {
	tenantID, tErr := requireTenant(ctx, d.Name(), "ListDir", "", d.logger)
	if tErr != nil {
		return nil, nil, tErr
	}
	sub := strings.Trim(dir, "/")
	folderPath := fmt.Sprintf("%s/t-%s/%s", odRootFolder, tenantID, container)
	accounts := d.tenants
	if sub != "" {
		folderPath += "/" + sub
		if odGroupFolder(container, sub) {
			accounts = d.spreadTenants(strings.TrimPrefix(folderPath, odRootFolder+"/"))
		}
	}
	children, err := d.children(ctx, folderPath, accounts)
	if err != nil {
		return nil, nil, err
	}
	for _, c := range children {
		if c.folder {
			dirs = append(dirs, c.name)
		} else {
			files = append(files, c.name)
		}
	}
	return dirs, files, nil
}

// RemoveEmptyDir removes the folder at dir under container when no account
// that can hold anything in it does (ListDir: the group's spread set for a
// group folder, else every account), then deletes it on each of those
// accounts (a folder exists on every account that ever held a file in it).
// ErrDirNotEmpty when an account holds something; a folder missing
// everywhere is fine; the container itself is refused. A folder's
// permanentDelete is recursive: the caller makes the window between the
// listing and the delete empty — the vault parity reconcile, the only writer and the only
// remover of its folders, under the job's lock (Prompt 2a.3 H1: emptied
// permafrost folders were listed again on every walk, forever).
func (d *OneDriveDriver) RemoveEmptyDir(ctx context.Context, container, dir string) error {
	sub := strings.Trim(dir, "/")
	if sub == "" {
		return fmt.Errorf("onedrive remove dir: %q is the container, not a folder", dir)
	}
	dirs, files, err := d.ListDir(ctx, container, sub)
	if err != nil {
		return fmt.Errorf("onedrive remove dir %s: %w", sub, err)
	}
	if len(files)+len(dirs) > 0 {
		return fmt.Errorf("onedrive remove dir %s: %w", sub, ErrDirNotEmpty)
	}
	if !odGroupFolder(container, sub) {
		if err := d.Delete(ctx, container, sub); err != nil && !odIsNotFound(err) {
			return fmt.Errorf("onedrive remove dir %s: %w", sub, err)
		}
		return nil
	}
	path, pErr := d.buildPath(ctx, "RemoveEmptyDir", container, sub)
	if pErr != nil {
		return pErr
	}
	var hardErr error
	for _, t := range d.spreadTenants(path) {
		driveID, err := t.getDriveID(ctx)
		if err != nil {
			hardErr = fmt.Errorf("onedrive get drive: %w", err)
			continue
		}
		itemID, err := t.getItemID(ctx, driveID, path)
		if err != nil {
			if !odIsNotFound(err) {
				hardErr = fmt.Errorf("onedrive resolve %s: %w", path, err)
			}
			continue
		}
		if err := t.deleteItem(ctx, driveID, itemID); err != nil && !odIsNotFound(err) {
			hardErr = fmt.Errorf("onedrive delete %s: %w", path, err)
		}
	}
	if hardErr != nil {
		return fmt.Errorf("onedrive remove dir %s: %w", sub, hardErr)
	}
	return nil
}

type odChild struct {
	name   string
	folder bool
	// size and hash (quickXorHash, "" when Graph reports none) of a file;
	// the rebalance walks with them (WP-FLEET-5 PR B).
	size int64
	hash string
}

// children is the union of the folder's direct children on the given
// accounts (a name once; a folder on any account is a folder).
func (d *OneDriveDriver) children(ctx context.Context, folderPath string, accounts []*odTenant) ([]odChild, error) {
	seen := make(map[string]int)
	var out []odChild
	for _, t := range accounts {
		driveID, err := t.getDriveID(ctx)
		if err != nil {
			return nil, fmt.Errorf("onedrive get drive: %w", err)
		}
		err = t.listChildren(ctx, driveID, folderPath, func(c odChild) {
			i, ok := seen[c.name]
			if !ok {
				seen[c.name] = len(out)
				out = append(out, c)
				return
			}
			out[i].folder = out[i].folder || c.folder
		})
		if odIsNotFound(err) {
			continue // this account holds nothing under the folder
		}
		if err != nil {
			return nil, fmt.Errorf("onedrive list %s: %w", folderPath, err)
		}
	}
	return out, nil
}

// listChildren calls fn with every direct child of the folder on this
// account. Graph pages children (default 200): @odata.nextLink is followed
// to the end — a single-page read silently truncated every listing of a
// container with more objects than one page. A missing folder is the odError
// odIsNotFound reports.
func (t *odTenant) listChildren(ctx context.Context, driveID, folderPath string, fn func(odChild)) error {
	u := fmt.Sprintf("%s/drives/%s/items/root:/%s:/children?$top=999",
		graphBase, driveID, odEscapePath(folderPath))
	for u != "" {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			return fmt.Errorf("onedrive list request: %w", err)
		}
		resp, err := t.graphDo(ctx, req)
		if err != nil {
			return err
		}
		var result struct {
			Value []struct {
				Name   string           `json:"name"`
				Folder *json.RawMessage `json:"folder"`
				Size   int64            `json:"size"`
				File   *struct {
					Hashes struct {
						QuickXorHash string `json:"quickXorHash"`
					} `json:"hashes"`
				} `json:"file"`
			} `json:"value"`
			NextLink string `json:"@odata.nextLink"`
		}
		decodeErr := json.NewDecoder(resp.Body).Decode(&result)
		_ = resp.Body.Close()
		if decodeErr != nil {
			return fmt.Errorf("onedrive list decode: %w", decodeErr)
		}
		for _, item := range result.Value {
			c := odChild{name: item.Name, folder: item.Folder != nil, size: item.Size}
			if item.File != nil {
				c.hash = item.File.Hashes.QuickXorHash
			}
			fn(c)
		}
		u = result.NextLink
	}
	return nil
}

// Exists checks if an artifact exists in OneDrive (home tenant first, then
// the fleet — pre-placement data may live on any account).
func (d *OneDriveDriver) Exists(ctx context.Context, container, artifact string) (bool, error) {
	path, pErr := d.buildPath(ctx, "Exists", container, artifact)
	if pErr != nil {
		return false, pErr
	}
	var downErr error // an account we could not check — absence is not provable past it
	for i, t := range d.tenantsFor(path) {
		if i == 1 {
			noteWalk("exists")
		}
		driveID, err := t.getDriveID(ctx)
		if err != nil {
			// A down account must not abort the probe (same as Get) — the
			// object may be reachable on a healthy account.
			downErr = fmt.Errorf("onedrive get drive: %w", err)
			continue
		}
		u := fmt.Sprintf("%s/drives/%s/items/root:/%s/%s",
			graphBase, driveID, odRootFolder, odEscapePath(path))
		req, _ := http.NewRequestWithContext(ctx, "GET", u, nil)
		resp, err := t.graphDo(ctx, req)
		if err != nil {
			if odIsNotFound(err) {
				continue
			}
			return false, fmt.Errorf("onedrive exists %s: %w", path, err)
		}
		odPooledDrain(resp.Body)
		_ = resp.Body.Close()
		if i > 0 {
			d.logFallbackHit("exists", path, t)
		}
		return true, nil
	}
	// Not found on any reachable account. If one was unreachable we cannot
	// honestly answer "false" — the object may live there.
	if downErr != nil {
		return false, downErr
	}
	return false, nil
}

// HealthCheck probes a batch of accounts (probeBatch, WP-FLEET-11 D-F11-5:
// a small fleet every account, a large one a rotating sample with every
// account within the hour, plus the failing ones again), odProbeConcurrency
// at once, within the caller's (the probe's 15 s) context, asks each
// tenant's token once, and reports every account whose LAST probe (or whose
// tenant's token) failed, by name (R7-18). An account whose outcome changed
// since its last probe is logged.
func (d *OneDriveDriver) HealthCheck(ctx context.Context) error {
	batch := d.probeBatch()
	errs := make([]error, len(batch))
	sem := make(chan struct{}, odProbeConcurrency)
	var wg sync.WaitGroup
	for bi, idx := range batch {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			errs[bi] = d.tenants[idx].probe(ctx)
		}()
	}
	// Each tenant's token once per interval (D-F11-5): a dead app
	// registration is seen at once, whichever drives were sampled.
	tokenErr := map[*odTenant]error{}
	for _, t := range d.tenants {
		owner := t
		if t.tokenOwner != nil {
			owner = t.tokenOwner
		}
		if _, done := tokenErr[owner]; !done {
			_, tokenErr[owner] = owner.token(ctx)
		}
	}
	wg.Wait()
	for bi, idx := range batch {
		t := d.tenants[idx]
		d.logProbeChange(t, errs[bi])
		t.probeErr.Store(odProbeOutcome{errs[bi]})
	}
	var failing []string
	for _, t := range d.tenants {
		owner := t
		if t.tokenOwner != nil {
			owner = t.tokenOwner
		}
		switch o, _ := t.probeErr.Load().(odProbeOutcome); {
		case tokenErr[owner] != nil:
			failing = append(failing, fmt.Sprintf("%s: %v", t.name, tokenErr[owner]))
		case o.err != nil:
			failing = append(failing, fmt.Sprintf("%s: %v", t.name, o.err))
		}
	}
	d.recheckFleet(ctx)
	if len(failing) > 0 {
		return fmt.Errorf("onedrive health: %d of %d accounts failing: %s",
			len(failing), len(d.tenants), strings.Join(failing, "; "))
	}
	return nil
}

// odProbeOutcome is an account's last probe outcome (nil err = up).
type odProbeOutcome struct{ err error }

// probe is one account's health: its drive's quota, one call, exported as
// the account's quota gauges (WP-FLEET-2, D-F1-6).
func (t *odTenant) probe(ctx context.Context) error {
	driveID, err := t.getDriveID(ctx)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("%s/drives/%s?$select=quota", graphBase, driveID), nil)
	if err != nil {
		return fmt.Errorf("probe request: %w", err)
	}
	resp, err := t.graphDo(ctx, req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	var out struct {
		Quota *odQuota `json:"quota"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&out); err == nil && out.Quota != nil {
		out.Quota.export(t.name)
		t.lastQuota.Store(*out.Quota)
	}
	odPooledDrain(resp.Body)
	return nil
}

// logProbeChange logs an account whose probe outcome differs from its last.
func (d *OneDriveDriver) logProbeChange(t *odTenant, err error) {
	state := int32(1)
	if err != nil {
		state = 2
	}
	if t.probeState.Swap(state) == state {
		return
	}
	if err != nil {
		d.logger.Warn("permafrost account probe: failing", zap.String("account", t.name), zap.Error(err))
		return
	}
	d.logger.Info("permafrost account probe: up", zap.String("account", t.name))
}

// ── Tenant methods ──────────────────────────────────────────────────────────

// odTokenRenewPause is how long a failed token renewal stops the next
// (D-F3-10): callers keep a still-valid token, an expired one fails at once.
const odTokenRenewPause = 15 * time.Second

// token is the account's Graph token. It is renewed 5 min before its end by
// ONE caller at a time; the others keep the valid token meanwhile (or wait
// for the renewal when there is none). A renewal that fails pauses renewals
// for odTokenRenewPause — a transient identity-service error no longer
// becomes a renewal per request (WP-FLEET-3 D-F3-10, the Share-one-Drive
// 3.10.5 pattern).
func (t *odTenant) token(ctx context.Context) (string, error) {
	if t.tokenOwner != nil && t.tokenOwner != t {
		return t.tokenOwner.token(ctx) // one token per tenant and app (D-F11-2)
	}
	for {
		t.tokenMu.Lock()
		now := time.Now()
		if t.cachedTok != "" && now.Before(t.tokExpiry) {
			tok := t.cachedTok
			t.tokenMu.Unlock()
			return tok, nil
		}
		valid := t.cachedTok != "" && now.Before(t.tokValidUntil)
		if t.renewing {
			if valid {
				tok := t.cachedTok
				t.tokenMu.Unlock()
				return tok, nil
			}
			done := t.renewDone
			t.tokenMu.Unlock()
			select {
			case <-done:
				continue
			case <-ctx.Done():
				return "", fmt.Errorf("onedrive token [%s]: %w", t.name, ctx.Err())
			}
		}
		if now.Before(t.renewPauseUntil) {
			tok, err := t.cachedTok, t.renewErr
			t.tokenMu.Unlock()
			if valid {
				return tok, nil
			}
			return "", fmt.Errorf("onedrive token [%s]: renewal paused after: %w", t.name, err)
		}
		t.renewing = true
		done := make(chan struct{})
		t.renewDone = done
		t.tokenMu.Unlock()

		tok, err := t.cred.GetToken(ctx, policy.TokenRequestOptions{
			Scopes: []string{"https://graph.microsoft.com/.default"},
		})

		t.tokenMu.Lock()
		t.renewing = false
		close(done)
		if err != nil {
			if ctx.Err() == nil { // a caller gone is no failure of the service
				t.renewErr = err
				t.renewPauseUntil = time.Now().Add(odTokenRenewPause)
			}
			still, old := t.cachedTok != "" && time.Now().Before(t.tokValidUntil), t.cachedTok
			t.tokenMu.Unlock()
			if still {
				return old, nil
			}
			return "", fmt.Errorf("onedrive token [%s]: %w", t.name, err)
		}
		t.cachedTok = tok.Token
		t.tokValidUntil = tok.ExpiresOn
		t.tokExpiry = tok.ExpiresOn.Add(-5 * time.Minute)
		t.renewPauseUntil = time.Time{}
		t.tokenMu.Unlock()
		return tok.Token, nil
	}
}

// odAttempts is the attempts a Graph call or an upload fragment gets — the
// S3-class drivers' bound (hedge.go, WP-R6-3): 2 on a request path, 3 under
// common.WithJob. It was 5 for every Graph call and 3 per fragment (R7-08).
func odAttempts(ctx context.Context) int {
	if common.IsJob(ctx) {
		return jobRetryMaxAttempts
	}
	return requestRetryMaxAttempts
}

// odBurstAttempts is the attempts a CDN range or an upload fragment gets
// (WP-FLEET-3 D-F3-2): 6 on a request path, 8 under common.WithJob — enough
// to wait out one of Microsoft's one-minute 429 bursts (odBurstBackoff,
// Retry-After to odBurstMaxWait). A range or a fragment is idempotent: the
// same bytes of the same URL, nothing doubled. The caller's context bounds
// the whole: a wait past its deadline is never begun.
func odBurstAttempts(ctx context.Context) int {
	if common.IsJob(ctx) {
		return 8
	}
	return 6
}

// odBurstBackoff is the wait before attempt n (n ≥ 1) with no Retry-After:
// odBurstBase × 2^(n-1), jittered up to half again (so 48 ranges refused in
// the same second do not come back in the same second), capped at
// odBurstMaxWait.
func odBurstBackoff(n int) time.Duration {
	d := odBurstBase << uint(min(max(n-1, 0), 10))
	d += time.Duration(mrand.Int64N(int64(d)/2 + 1)) // #nosec G404 -- jitter for backoff, not security
	return min(d, odBurstMaxWait)
}

// odRetryAfter is a response's Retry-After as a duration: delta-seconds or
// an HTTP date; 0 when absent or unreadable.
func odRetryAfter(h http.Header) time.Duration {
	v := strings.TrimSpace(h.Get("Retry-After"))
	if v == "" {
		return 0
	}
	if n, err := strconv.Atoi(v); err == nil && n > 0 {
		return time.Duration(n) * time.Second
	}
	if at, err := http.ParseTime(v); err == nil {
		if d := time.Until(at); d > 0 {
			return d
		}
	}
	return 0
}

// burstRetry waits before attempt n of a range or a fragment: the largest of
// the backoff, the answer's Retry-After and what is left of the account's
// hold, capped at odBurstMaxWait, on ctx. It returns the time the wait
// covers (the next attempt waits only for a hold beyond it), or the error to
// return when there is no attempt to make — the caller is gone, or the wait
// would outlast its deadline.
func (t *odTenant) burstRetry(ctx context.Context, n int, retryAfter time.Duration, lastErr error) (time.Time, error) {
	held := time.Until(time.Unix(0, t.throttledUntil.Load()))
	w, ok := retryWaitCapped(ctx, max(odBurstBackoff(n), retryAfter, held), odBurstMaxWait)
	if !ok {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return time.Time{}, fmt.Errorf("%w (%w)", lastErr, ctxErr)
		}
		return time.Time{}, lastErr
	}
	covered := time.Now().Add(w)
	if err := t.sleep(ctx, w); err != nil {
		return time.Time{}, fmt.Errorf("%w (waiting to retry: %w)", lastErr, err)
	}
	return covered, nil
}

// odWait waits d, or until ctx is done (every wait between attempts did a
// time.Sleep that no caller could end).
func odWait(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// retryWaitCapped is the wait before another attempt, capped at limit
// (odMaxRetryWait for a Graph call, odBurstMaxWait for data);
// false when there is no attempt to make: the caller is gone, or the wait
// would outlast the caller's deadline (a Retry-After past it is given up on
// at once, never slept through).
func retryWaitCapped(ctx context.Context, d, limit time.Duration) (time.Duration, bool) {
	if ctx.Err() != nil {
		return 0, false
	}
	if d > limit {
		d = limit
	}
	if dl, ok := ctx.Deadline(); ok && time.Until(dl) <= d {
		return 0, false
	}
	return d, true
}

// sleep is the tenant's wait between attempts.
func (t *odTenant) sleep(ctx context.Context, d time.Duration) error {
	if t.wait != nil {
		return t.wait(ctx, d)
	}
	return odWait(ctx, d)
}

// graphDo sends one Graph request with the bounded retries: a transport
// error, a 429/503 (Retry-After honoured up to odMaxRetryWait) or another
// 5xx is tried again, odAttempts attempts in all, decorrelated jitter, every
// wait on ctx; a caller already gone is never retried. Any other answer >= 400
// is an *odError at once.
func (t *odTenant) graphDo(ctx context.Context, req *http.Request) (*http.Response, error) {
	return t.graphDoWith(ctx, t.graphHTTP, req, odAttempts(ctx), odMaxRetryWait)
}

// graphDoWith is graphDo with its own attempts and wait cap: a simple PUT of
// up to 200 MiB is data, not metadata, and rides a burst like a fragment
// (odBurstAttempts, odBurstMaxWait; WP-FLEET-4 D-F4-2).
//
// client is the transport: the Graph client (HTTP/2) for metadata; the
// upload client (HTTP/1.1) for a large simple PUT — Go's HTTP/2 flow
// control held a 200 MiB body to ~10 MB/s (the 2026-11-07 proof).
func (t *odTenant) graphDoWith(ctx context.Context, client *http.Client, req *http.Request, attempts int, maxWait time.Duration) (*http.Response, error) {
	base := 500 * time.Millisecond
	prev := base

	token, err := t.token(ctx)
	if err != nil {
		return nil, err
	}

	var lastErr error
	var covered time.Time
	for attempt := 0; attempt < attempts; attempt++ {
		if attempt > 0 {
			if req.GetBody == nil && req.Body != nil && req.Body != http.NoBody {
				return nil, lastErr // a body that cannot be sent again
			}
			if req.GetBody != nil {
				body, err := req.GetBody()
				if err != nil {
					return nil, fmt.Errorf("onedrive [%s] rewind body: %w", t.name, err)
				}
				req.Body = body
			}
		}
		if err := t.paceRequestFrom(ctx, covered); err != nil {
			if lastErr != nil {
				return nil, fmt.Errorf("%w (after %w)", err, lastErr)
			}
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("User-Agent", odUserAgent())

		resp, err := client.Do(req)
		t.totalReqs.Add(1)
		var wait time.Duration
		switch {
		case err != nil:
			if ctxErr := ctx.Err(); ctxErr != nil {
				return nil, fmt.Errorf("onedrive [%s]: %w", t.name, ctxErr)
			}
			lastErr = fmt.Errorf("onedrive [%s]: %w", t.name, err)
			prev = odDecorrelatedJitter(base, prev, maxWait)
			wait = prev
		case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == http.StatusServiceUnavailable:
			ra := odRetryAfter(resp.Header)
			wait = odDecorrelatedJitter(base, prev, maxWait)
			if ra > wait {
				wait = ra
			}
			if wait > maxWait {
				wait = maxWait
			}
			// The whole account waits the Retry-After out (D-F3-6): the next
			// attempt, and every other caller, at paceRequest.
			t.hold("graph", ra)
			prev = wait
			odPooledDrain(resp.Body)
			_ = resp.Body.Close()
			lastErr = fmt.Errorf("onedrive throttled [%s] (%d)", t.name, resp.StatusCode)
		case resp.StatusCode >= 500:
			odPooledDrain(resp.Body)
			_ = resp.Body.Close()
			lastErr = fmt.Errorf("onedrive [%s] HTTP %d", t.name, resp.StatusCode)
			prev = odDecorrelatedJitter(base, prev, maxWait)
			wait = prev
		case resp.StatusCode >= 400:
			b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
			_ = resp.Body.Close()
			return nil, newODError(t.name, resp.StatusCode, b)
		default:
			t.trackRateLimit(resp)
			return resp, nil
		}
		if attempt == attempts-1 {
			break
		}
		// The wait covers the account's hold (D-F3-6) as far as Graph's
		// 30 s cap goes; the next attempt waits out the rest of it.
		wait = max(wait, time.Until(time.Unix(0, t.throttledUntil.Load())))
		w, ok := retryWaitCapped(ctx, wait, maxWait)
		if !ok {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return nil, fmt.Errorf("%w (%w)", lastErr, ctxErr)
			}
			return nil, lastErr
		}
		covered = time.Now().Add(w)
		if err := t.sleep(ctx, w); err != nil {
			return nil, fmt.Errorf("%w (waiting to retry: %w)", lastErr, err)
		}
	}
	return nil, lastErr
}

// trackRateLimit reads Graph's RateLimit headers (sent at ≥ 80 % of a
// budget; never seen in the 2026-11 soaks): Remaining below 20 % of Limit
// holds the account until Reset (D-F3-6) — back-pressure before the 429.
func (t *odTenant) trackRateLimit(resp *http.Response) {
	remaining, rerr := strconv.ParseInt(resp.Header.Get("RateLimit-Remaining"), 10, 64)
	if rerr == nil {
		t.rateLimitRemaining.Store(remaining)
	}
	reset, serr := strconv.ParseInt(resp.Header.Get("RateLimit-Reset"), 10, 64)
	if serr == nil {
		t.rateLimitReset.Store(time.Now().Add(time.Duration(reset) * time.Second).Unix())
	}
	limit, lerr := strconv.ParseInt(resp.Header.Get("RateLimit-Limit"), 10, 64)
	if rerr == nil && serr == nil && lerr == nil && limit > 0 && reset > 0 &&
		float64(remaining) < odRateLimitLow*float64(limit) {
		t.hold("", min(time.Duration(reset)*time.Second, odBurstMaxWait))
	}
}

// odDrive is one of a user's drives as Graph lists it.
type odDrive struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	DriveType string `json:"driveType"`
}

// odOneDriveLibrary is the name of the user's own document library — the
// default drive (/users/{upn}/drive). A user can have others: a hidden
// PersonalCacheLibrary is listed FIRST for two of prod's three fleet users.
const odOneDriveLibrary = "OneDrive"

// odNoLibraryError is a listing that names no drive to choose.
type odNoLibraryError struct {
	user   string
	drives []odDrive
}

func (e *odNoLibraryError) Error() string {
	return fmt.Sprintf("no drive named %s for %s (drives: %s)", odOneDriveLibrary, e.user, strings.Join(e.names(), ", "))
}

func (e *odNoLibraryError) names() []string {
	out := make([]string, len(e.drives))
	for i, d := range e.drives {
		out[i] = d.Name
	}
	return out
}

// chooseDrive is the account's drive among its user's drives (D-F1-1):
// one listed = that drive; else the one named OneDrive; else the one
// business drive when exactly one is; never drives[0] — Graph's order is
// not pinned and differs per account.
func chooseDrive(user string, drives []odDrive) (chosen odDrive, others []odDrive, err error) {
	pick := func(i int) (odDrive, []odDrive, error) {
		rest := make([]odDrive, 0, len(drives)-1)
		rest = append(rest, drives[:i]...)
		return drives[i], append(rest, drives[i+1:]...), nil
	}
	if len(drives) == 1 {
		return pick(0)
	}
	named, business := -1, -1
	nNamed, nBusiness := 0, 0
	for i, d := range drives {
		if d.Name == odOneDriveLibrary {
			named, nNamed = i, nNamed+1
		}
		if d.DriveType == "business" {
			business, nBusiness = i, nBusiness+1
		}
	}
	switch {
	case nNamed == 1:
		return pick(named)
	case nNamed == 0 && nBusiness == 1:
		return pick(business)
	}
	return odDrive{}, nil, &odNoLibraryError{user: user, drives: drives}
}

// getDriveID is the account's drive: TENANT_N_DRIVE_ID when set (no
// listing), else chooseDrive over /users/{upn}/drives — cached for the life
// of the process.
func (t *odTenant) getDriveID(ctx context.Context) (string, error) {
	t.mu.Lock()
	if t.driveID != "" {
		d := t.driveID
		t.mu.Unlock()
		return d, nil
	}
	if t.pinnedDriveID != "" {
		t.driveID = t.pinnedDriveID
		t.mu.Unlock()
		return t.pinnedDriveID, nil
	}
	t.mu.Unlock()

	u := fmt.Sprintf("%s/users/%s/drives?$select=id,name,driveType", graphBase, url.PathEscape(t.userUPN))
	req, _ := http.NewRequestWithContext(ctx, "GET", u, nil)
	resp, err := t.graphDo(ctx, req)
	if err != nil {
		return "", fmt.Errorf("get drives [%s]: %w", t.name, err)
	}
	defer func() { _ = resp.Body.Close() }()

	var out struct {
		Value []odDrive `json:"value"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", fmt.Errorf("decode drives [%s]: %w", t.name, err)
	}
	if len(out.Value) == 0 {
		return "", fmt.Errorf("no drives for %s", t.userUPN)
	}
	chosen, others, err := chooseDrive(t.userUPN, out.Value)
	if err != nil {
		return "", fmt.Errorf("choose drive [%s]: %w", t.name, err)
	}

	t.mu.Lock()
	t.driveID, t.otherDrives = chosen.ID, others
	t.mu.Unlock()
	return chosen.ID, nil
}

func (t *odTenant) ensureRootFolder(ctx context.Context, driveID string) (string, error) {
	t.mu.Lock()
	if t.rootFolderID != "" {
		id := t.rootFolderID
		t.mu.Unlock()
		return id, nil
	}
	t.mu.Unlock()

	u := fmt.Sprintf("%s/drives/%s/items/root/children", graphBase, driveID)
	body, _ := json.Marshal(map[string]any{
		"name": odRootFolder, "folder": map[string]any{},
		"@microsoft.graph.conflictBehavior": "fail",
	})
	req, _ := http.NewRequestWithContext(ctx, "POST", u, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(body)), nil }

	resp, err := t.graphDo(ctx, req)
	if err != nil {
		// A conflict = the folder exists — fetch its ID instead. By status
		// and code: the text matched "409" anywhere, a message included.
		var oe *odError
		if errors.As(err, &oe) && (oe.Status == http.StatusConflict || oe.Code == "nameAlreadyExists") {
			return t.getFolderID(ctx, driveID, odRootFolder)
		}
		return "", fmt.Errorf("create root folder [%s]: %w", t.name, err)
	}
	defer func() { _ = resp.Body.Close() }()

	var out struct {
		ID string `json:"id"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&out)

	t.mu.Lock()
	t.rootFolderID = out.ID
	t.mu.Unlock()
	return out.ID, nil
}

func (t *odTenant) getFolderID(ctx context.Context, driveID, name string) (string, error) {
	u := fmt.Sprintf("%s/drives/%s/items/root:/%s", graphBase, driveID, url.PathEscape(name))
	req, _ := http.NewRequestWithContext(ctx, "GET", u, nil)
	resp, err := t.graphDo(ctx, req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()

	var out struct {
		ID string `json:"id"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&out)

	t.mu.Lock()
	t.rootFolderID = out.ID
	t.mu.Unlock()
	return out.ID, nil
}

func (t *odTenant) getItemID(ctx context.Context, driveID, path string) (string, error) {
	u := fmt.Sprintf("%s/drives/%s/items/root:/%s/%s",
		graphBase, driveID, odRootFolder, odEscapePath(path))
	req, _ := http.NewRequestWithContext(ctx, "GET", u, nil)
	resp, err := t.graphDo(ctx, req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()

	var out struct {
		ID string `json:"id"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return out.ID, nil
}

func (t *odTenant) getDownloadURL(ctx context.Context, driveID, path string) (string, int64, error) {
	u := fmt.Sprintf("%s/drives/%s/items/root:/%s/%s",
		graphBase, driveID, odRootFolder, odEscapePath(path))
	req, _ := http.NewRequestWithContext(ctx, "GET", u, nil)
	resp, err := t.graphDo(ctx, req)
	if err != nil {
		return "", 0, err
	}
	defer func() { _ = resp.Body.Close() }()

	var meta struct {
		DownloadURL string `json:"@microsoft.graph.downloadUrl"`
		Size        int64  `json:"size"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&meta)
	if meta.DownloadURL == "" {
		return "", 0, fmt.Errorf("no downloadUrl for %s", path)
	}
	return meta.DownloadURL, meta.Size, nil
}

// ── Upload ──────────────────────────────────────────────────────────────────

// putChunk sends one upload fragment with the burst attempts
// (odBurstAttempts, WP-FLEET-3 D-F3-2; every wait on ctx, Retry-After
// honoured to odBurstMaxWait): a transport error, a 429 or a 5xx is sent
// again — the same byte range of the same session, so nothing is doubled; a
// caller already gone is never retried. The body is fenced after each
// attempt: the transport may still read a request body after Do returns,
// and chunk is the caller's pooled buffer.
//
// The answer to the LAST fragment is the item (200/201): it is returned for
// the write's quickXorHash check (WP-FLEET-4 D-F4-4); a 202 returns nil.
func (t *odTenant) putChunk(ctx context.Context, uploadURL string, chunk []byte, start, endInc, total int) (*odItem, error) {
	attempts := odBurstAttempts(ctx)
	var lastErr error
	var retryAfter time.Duration
	var covered time.Time
	for attempt := 0; attempt < attempts; attempt++ {
		if attempt > 0 {
			c, err := t.burstRetry(ctx, attempt, retryAfter, lastErr)
			if err != nil {
				return nil, fmt.Errorf("onedrive chunk: %w", err)
			}
			covered = c
		}
		retryAfter = 0
		if err := t.paceRequestFrom(ctx, covered); err != nil {
			if lastErr != nil {
				return nil, fmt.Errorf("onedrive chunk: %w (after %w)", err, lastErr)
			}
			return nil, fmt.Errorf("onedrive chunk: %w", err)
		}
		body := &odFragment{b: chunk}
		req, err := http.NewRequestWithContext(ctx, http.MethodPut, uploadURL, body)
		if err != nil {
			return nil, fmt.Errorf("onedrive chunk request: %w", err)
		}
		req.ContentLength = int64(len(chunk))
		req.Header.Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, endInc, total))
		req.Header.Set("User-Agent", odUserAgent())
		// No Authorization header on chunk PUTs — the upload URL is pre-authenticated
		resp, err := t.uploadHTTP.Do(req)
		body.fence()
		t.totalReqs.Add(1)
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return nil, fmt.Errorf("onedrive chunk: %w", ctxErr)
			}
			lastErr = err
			continue
		}
		if resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusCreated {
			item, derr := decodeODItem(resp.Body)
			odPooledDrain(resp.Body)
			_ = resp.Body.Close()
			if derr != nil {
				return nil, fmt.Errorf("onedrive chunk: the final answer: %w", derr)
			}
			return item, nil
		}
		odPooledDrain(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			return nil, nil
		}
		if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == http.StatusServiceUnavailable {
			retryAfter = odRetryAfter(resp.Header)
			t.hold("fragment", retryAfter)
			lastErr = fmt.Errorf("onedrive chunk HTTP %d (throttled)", resp.StatusCode)
			continue
		}
		if resp.StatusCode >= 500 {
			lastErr = fmt.Errorf("onedrive chunk HTTP %d", resp.StatusCode)
			continue
		}
		return nil, fmt.Errorf("onedrive chunk HTTP %d", resp.StatusCode)
	}
	return nil, fmt.Errorf("onedrive chunk: %d attempts: %w", attempts, lastErr)
}

// errFragmentFenced is a read of a fragment body after its attempt ended.
var errFragmentFenced = errors.New("onedrive: upload fragment read after its attempt ended")

// odFragment is an upload fragment's request body over a pooled buffer;
// fence ends its reads, so the buffer can be refilled for the next fragment
// while a transport goroutine still holds the request.
type odFragment struct {
	mu     sync.Mutex
	b      []byte
	off    int
	fenced bool
}

func (f *odFragment) Read(p []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fenced {
		return 0, errFragmentFenced
	}
	if f.off >= len(f.b) {
		return 0, io.EOF
	}
	n := copy(p, f.b[f.off:])
	f.off += n
	return n, nil
}

func (f *odFragment) Close() error { return nil }

func (f *odFragment) fence() {
	f.mu.Lock()
	f.fenced = true
	f.mu.Unlock()
}

// odChunkPool holds the upload fragment buffers (odChunkSize each): one per
// upload in flight, reused across uploads (a fresh 60 MiB per PUT before).
var odChunkPool = sync.Pool{New: newODChunkBuffer}

func newODChunkBuffer() any {
	b := make([]byte, odChunkSize)
	return &b
}

// ── Transport setup ─────────────────────────────────────────────────────────

func odCachedDialContext() func(ctx context.Context, network, addr string) (net.Conn, error) {
	dialer := &net.Dialer{
		Timeout:   10 * time.Second,
		KeepAlive: 30 * time.Second,
	}
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(addr)
		if err != nil {
			return dialer.DialContext(ctx, network, addr)
		}
		if cached, ok := odDNSCache.Load(host); ok {
			if addrs, ok := cached.([]string); ok && len(addrs) > 0 {
				return dialer.DialContext(ctx, network, net.JoinHostPort(addrs[0], port))
			}
		}
		addrs, err := net.DefaultResolver.LookupHost(ctx, host)
		if err != nil || len(addrs) == 0 {
			return dialer.DialContext(ctx, network, addr)
		}
		odDNSCache.Store(host, addrs)
		return dialer.DialContext(ctx, network, net.JoinHostPort(addrs[0], port))
	}
}

func odGraphTransport(dial func(ctx context.Context, network, addr string) (net.Conn, error), tlsCache tls.ClientSessionCache) *http.Transport {
	t := &http.Transport{
		MaxIdleConns:          200,
		MaxIdleConnsPerHost:   200,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: odGraphHeaderTimeout,
		ReadBufferSize:        1 << 20,
		WriteBufferSize:       1 << 20,
		DisableCompression:    true,
		ForceAttemptHTTP2:     true,
		DialContext:           dial,
		TLSClientConfig: &tls.Config{
			MinVersion:         tls.VersionTLS12,
			ClientSessionCache: tlsCache,
		},
	}
	return t
}

// HTTP/1.1 for CDN bulk downloads — bypasses Go HTTP/2 flow-control bugs
func odCDNTransport(dial func(ctx context.Context, network, addr string) (net.Conn, error), tlsCache tls.ClientSessionCache) *http.Transport {
	return &http.Transport{
		TLSNextProto:          make(map[string]func(authority string, c *tls.Conn) http.RoundTripper),
		MaxConnsPerHost:       128,
		MaxIdleConnsPerHost:   32,
		IdleConnTimeout:       60 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: odCDNHeaderTimeout,
		ReadBufferSize:        4 * 1024 * 1024,
		WriteBufferSize:       4 * 1024 * 1024,
		DisableCompression:    true,
		DialContext:           dial,
		TLSClientConfig: &tls.Config{
			MinVersion:         tls.VersionTLS12,
			ClientSessionCache: tlsCache,
		},
	}
}

// HTTP/1.1 for bulk chunk uploads. The upload session's large fragment PUTs hit
// the same Go HTTP/2 flow-control window bug that throttled CDN downloads (which
// is why downloads moved to HTTP/1.1 for a +60% fleet gain). Forcing HTTP/1.1 here
// (TLSNextProto disables the h2 upgrade) applies the same fix to the write path.
// The Graph API calls (small, multiplexed) stay on HTTP/2 via odGraphTransport.
func odUploadTransport(dial func(ctx context.Context, network, addr string) (net.Conn, error), tlsCache tls.ClientSessionCache) *http.Transport {
	return &http.Transport{
		TLSNextProto:          make(map[string]func(authority string, c *tls.Conn) http.RoundTripper),
		MaxIdleConns:          200,
		MaxIdleConnsPerHost:   200,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: odUploadHeaderTimeout,
		ReadBufferSize:        4 * 1024 * 1024,
		WriteBufferSize:       4 * 1024 * 1024,
		DisableCompression:    true,
		DialContext:           dial,
		TLSClientConfig: &tls.Config{
			MinVersion:         tls.VersionTLS12,
			ClientSessionCache: tlsCache,
		},
	}
}

// ── Helpers ─────────────────────────────────────────────────────────────────

// odEscapePath escapes each segment of a path individually, preserving
// the "/" separators so Graph API creates proper nested folders.
// Without this, url.PathEscape encodes slashes and creates flat filenames.
func odEscapePath(path string) string {
	parts := strings.Split(path, "/")
	for i, p := range parts {
		parts[i] = url.PathEscape(p)
	}
	return strings.Join(parts, "/")
}

func odDecorrelatedJitter(base, previous, cap time.Duration) time.Duration {
	high := previous * 3
	if high > cap {
		high = cap
	}
	if high <= base {
		return base
	}
	return base + time.Duration(mrand.Int64N(int64(high-base))) // #nosec G404 -- jitter for backoff, not security
}
