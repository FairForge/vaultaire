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
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/FairForge/vaultaire/internal/common"
	"github.com/FairForge/vaultaire/internal/engine"
	"github.com/FairForge/vaultaire/internal/parfetch"
	"go.uber.org/zap"

	mrand "math/rand/v2"
)

const (
	graphBase   = "https://graph.microsoft.com/v1.0"
	odUserAgent = "ISV|FairForge|Vaultaire/1.0"

	simpleUploadMax = 4 << 20  // 4 MB — above this, use upload session
	odChunkSize     = 60 << 20 // 60 MB — Microsoft's max per chunk PUT
	odChunkAlign    = 320 * 1024

	odRootFolder = "vaultaire"

	// odRangePart is one ranged read of a large Get; odAdaptiveStreams of
	// them are held at most (odRangeWindow — 64 MiB at 8 streams, Geyser's
	// bound).
	odRangePart = 8 << 20

	// odMaxRetryWait caps one wait between attempts, Retry-After included
	// (it was honoured unbounded: a 429 with Retry-After: 120 slept 120 s
	// inside a request).
	odMaxRetryWait = 30 * time.Second

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

	// Token cache with mutex to prevent concurrent refresh storms.
	// Azure throttles parallel token requests from the same client.
	tokenMu   sync.Mutex
	cachedTok string
	tokExpiry time.Time

	throttledUntil atomic.Int64
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
}

// NewOneDriveFleetDriver creates a multi-tenant OneDrive driver.
// Reads TENANT_N_ID, TENANT_N_CLIENT_ID, TENANT_N_SECRET, TENANT_N_USER
// environment variables for N=1..15.
func NewOneDriveFleetDriver(logger *zap.Logger) (*OneDriveDriver, error) {
	var tenants []*odTenant
	for i := 1; i <= 15; i++ {
		p := fmt.Sprintf("TENANT_%d_", i)
		tid := os.Getenv(p + "ID")
		if tid == "" {
			continue
		}
		clientID := os.Getenv(p + "CLIENT_ID")
		secret := os.Getenv(p + "SECRET")
		userUPN := os.Getenv(p + "USER")
		if clientID == "" || secret == "" || userUPN == "" {
			logger.Warn("incomplete OneDrive tenant config, skipping",
				zap.Int("tenant", i))
			continue
		}

		cred, err := azidentity.NewClientSecretCredential(tid, clientID, secret, nil)
		if err != nil {
			logger.Warn("failed to create OneDrive credential",
				zap.Int("tenant", i), zap.Error(err))
			continue
		}

		dial := odCachedDialContext()

		t := &odTenant{
			name:       fmt.Sprintf("tenant-%d", i),
			tenantID:   tid,
			clientID:   clientID,
			secret:     secret,
			userUPN:    userUPN,
			cred:       cred,
			graphHTTP:  &http.Client{Transport: odGraphTransport(dial, odTLSSessionCache)},
			cdnHTTP:    &http.Client{Transport: odCDNTransport(dial, odTLSSessionCache)},
			uploadHTTP: &http.Client{Transport: odUploadTransport(dial, odTLSSessionCache)},
		}
		tenants = append(tenants, t)
	}

	if len(tenants) == 0 {
		return nil, fmt.Errorf("onedrive: no valid tenant configurations found")
	}

	d := &OneDriveDriver{
		tenants: tenants,
		logger:  logger,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for _, t := range tenants {
		driveID, err := t.getDriveID(ctx)
		if err != nil {
			logger.Warn("OneDrive tenant init failed — will retry on first use",
				zap.String("tenant", t.name), zap.Error(err))
			continue
		}
		if _, err := t.ensureRootFolder(ctx, driveID); err != nil {
			logger.Warn("OneDrive root folder creation failed",
				zap.String("tenant", t.name), zap.Error(err))
		}
	}

	logger.Info("OneDrive fleet initialized", zap.Int("tenants", len(tenants)))
	return d, nil
}

// Name is the registration key main.go uses ("permafrost"). The engine records
// the registration key as an object's backend_name, so the two must never
// drift: the driver was once registered as "onedrive" and 2,613 prod head rows
// still carry that name with no driver to answer it (Review R7-10).
func (d *OneDriveDriver) Name() string { return "permafrost" }

func (d *OneDriveDriver) TenantCount() int { return len(d.tenants) }

// homeTenantIndex is the deterministic fleet placement for a path: FNV-1a of
// the full storage path mod fleet size. Every operation on the same path
// lands on the same account, restart-stable and config-order-stable as long
// as the TENANT_N ordering doesn't change.
func (d *OneDriveDriver) homeTenantIndex(path string) int {
	h := fnv.New32a()
	_, _ = h.Write([]byte(path))
	return int(h.Sum32() % uint32(len(d.tenants)))
}

// tenantsFor returns the fleet ordered for a data operation on path (an object
// lives on exactly ONE account — a rotating read was the 2026-07-30
// read-after-write failure): the
// deterministic home tenant first, then the rest as fallbacks. The fallback
// probe exists for data written before deterministic placement (the old
// round-robin Put scattered objects across accounts) — reads walk the fleet
// on 404 instead of failing.
func (d *OneDriveDriver) tenantsFor(path string) []*odTenant {
	home := d.homeTenantIndex(path)
	ordered := make([]*odTenant, 0, len(d.tenants))
	ordered = append(ordered, d.tenants[home])
	for i, t := range d.tenants {
		if i != home {
			ordered = append(ordered, t)
		}
	}
	return ordered
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
	// Deterministic placement: the path's home tenant, always — never the
	// rotation. A throttled home tenant still receives the write (graphDo
	// retries with backoff); writing elsewhere would strand the object where
	// deterministic reads don't look first.
	t := d.tenants[d.homeTenantIndex(path)]
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
	// without buffering the entire body. This eliminates the double-copy
	// that materialize() + io.ReadAll() would cause.
	if options.ContentLength > 0 {
		d.logger.Debug("onedrive put (streaming)",
			zap.String("tenant", t.name),
			zap.String("path", path),
			zap.Int64("size", options.ContentLength),
		)
		if options.ContentLength <= simpleUploadMax {
			buf, err := io.ReadAll(data)
			if err != nil {
				return fmt.Errorf("onedrive read body: %w", err)
			}
			return t.simpleUpload(ctx, driveID, rootID, path, buf)
		}
		return t.streamingChunkedUpload(ctx, driveID, rootID, path, data, options.ContentLength)
	}

	// Unknown length: spool it to learn the size (materialize: in memory up
	// to 64 MiB, else a temp file), then upload FROM the spool. It was read
	// whole into memory a second time for chunkedUpload (R7-07).
	body, size, cleanup, err := materialize(data)
	if err != nil {
		return fmt.Errorf("onedrive buffer: %w", err)
	}
	defer cleanup()

	d.logger.Debug("onedrive put (materialized)",
		zap.String("tenant", t.name),
		zap.String("path", path),
		zap.Int64("size", size),
	)

	if size <= simpleUploadMax {
		buf, err := io.ReadAll(body)
		if err != nil {
			return fmt.Errorf("onedrive read body: %w", err)
		}
		return t.simpleUpload(ctx, driveID, rootID, path, buf)
	}
	return t.streamingChunkedUpload(ctx, driveID, rootID, path, body, size)
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

	streams := odAdaptiveStreams(fileSize)
	if streams > 1 {
		return t.downloadRanges(ctx, dlURL, fileSize, streams)
	}

	req, err := http.NewRequestWithContext(ctx, "GET", dlURL, nil)
	if err != nil {
		return nil, fmt.Errorf("onedrive create download request: %w", err)
	}
	resp, err := t.cdnHTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("onedrive download %s: %w", path, err)
	}
	if resp.StatusCode != http.StatusOK {
		odPooledDrain(resp.Body)
		_ = resp.Body.Close()
		return nil, fmt.Errorf("onedrive download %s: HTTP %d", path, resp.StatusCode)
	}
	return resp.Body, nil
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
	return int64(odAdaptiveStreams(fileSize)) * odRangePart
}

// downloadRanges reads a file as odRangePart byte ranges over the HTTP/1.1
// CDN transport, `streams` of them in flight, handed to the caller in order
// through a bounded window (parfetch): at most streams × odRangePart held —
// ranges in flight, fetched and waiting, and the one being read. Every range
// read whole into memory before the first byte went out until R7-07 (a 5 GB
// GET held 5 GB). A range that is not exactly the 206 asked for fails the
// stream: never a splice.
func (t *odTenant) downloadRanges(ctx context.Context, dlURL string, fileSize int64, streams int) (io.ReadCloser, error) {
	n := int((fileSize + odRangePart - 1) / odRangePart)
	sizes := make([]int64, n)
	for i := range sizes {
		sizes[i] = odRangePart
	}
	sizes[n-1] = fileSize - int64(n-1)*odRangePart
	cfg := parfetch.Config{
		Window:      int64(streams) * odRangePart,
		MaxParts:    streams,
		StrictSizes: true,
	}
	stream := parfetch.Start(ctx, cfg, sizes, func(ctx context.Context, i int) ([]byte, error) {
		return t.fetchRange(ctx, dlURL, int64(i)*odRangePart, sizes[i], fileSize)
	})
	return parfetch.NewReader(stream, nil), nil
}

// fetchRange is one ranged CDN read of size bytes at start.
func (t *odTenant) fetchRange(ctx context.Context, dlURL string, start, size, total int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, dlURL, nil)
	if err != nil {
		return nil, fmt.Errorf("onedrive range request: %w", err)
	}
	end := start + size - 1
	req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", start, end))
	resp, err := t.cdnHTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("onedrive range %d-%d: %w", start, end, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusPartialContent {
		// Never drained: a 200 is the whole object.
		return nil, fmt.Errorf("onedrive range %d-%d: HTTP %d, not 206", start, end, resp.StatusCode)
	}
	if want, got := fmt.Sprintf("bytes %d-%d/%d", start, end, total), resp.Header.Get("Content-Range"); got != want {
		return nil, fmt.Errorf("onedrive range %d-%d: Content-Range %q, asked %q", start, end, got, want)
	}
	buf := make([]byte, size)
	if _, err := io.ReadFull(resp.Body, buf); err != nil {
		return nil, fmt.Errorf("onedrive range %d-%d: %w", start, end, err)
	}
	return buf, nil
}

// Delete removes an artifact from OneDrive. It sweeps the WHOLE fleet, not
// just the first copy found: the old round-robin Put could leave copies of
// the same path on several accounts (every overwrite landed wherever the
// rotation pointed), and deleting only one of them leaves a stale duplicate
// that the next Get's fleet probe resurrects.
func (d *OneDriveDriver) Delete(ctx context.Context, container, artifact string) error {
	path, pErr := d.buildPath(ctx, "Delete", container, artifact)
	if pErr != nil {
		return pErr
	}

	deleted := false
	var hardErr error  // an account we could not fully check — a copy may survive there
	var probeErr error // 404s from the sweep, reported only if nothing was deleted
	for i, t := range d.tenantsFor(path) {
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

		u := fmt.Sprintf("%s/drives/%s/items/%s", graphBase, driveID, itemID)
		req, _ := http.NewRequestWithContext(ctx, "DELETE", u, nil)
		resp, err := t.graphDo(ctx, req)
		if err != nil {
			hardErr = fmt.Errorf("onedrive delete %s: %w", path, err)
			continue
		}
		odPooledDrain(resp.Body)
		_ = resp.Body.Close()
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
// (a Graph folder DELETE is recursive: what a write landed between that List
// and the DELETE goes with it — an erased tenant's). A folder missing
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
		u := fmt.Sprintf("%s/drives/%s/items/%s", graphBase, driveID, itemID)
		req, err := http.NewRequestWithContext(ctx, http.MethodDelete, u, nil)
		if err != nil {
			return fmt.Errorf("onedrive remove container request: %w", err)
		}
		resp, err := t.graphDo(ctx, req)
		if err != nil && !odIsNotFound(err) {
			return fmt.Errorf("onedrive remove container %s on %s: %w", container, t.name, err)
		}
		if resp != nil {
			odPooledDrain(resp.Body)
			_ = resp.Body.Close()
		}
	}
	return nil
}

// ListDir lists the direct members of the folder dir under container ("" =
// the container) on every account of the fleet: sub-folder names and file
// names, the union (a folder on any account is a folder). One Graph listing
// per account. The vault parity reconcile walks `<digest>/<etag>/` with it —
// List alone sees only the container's direct children.
func (d *OneDriveDriver) ListDir(ctx context.Context, container, dir string) (dirs, files []string, err error) {
	tenantID, tErr := requireTenant(ctx, d.Name(), "ListDir", "", d.logger)
	if tErr != nil {
		return nil, nil, tErr
	}
	folderPath := fmt.Sprintf("%s/t-%s/%s", odRootFolder, tenantID, container)
	if sub := strings.Trim(dir, "/"); sub != "" {
		folderPath += "/" + sub
	}
	children, err := d.children(ctx, folderPath)
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
// of the fleet holds anything in it (one listing per account, the union),
// then deletes it on every account that has it (Delete sweeps the fleet).
// ErrDirNotEmpty when an account holds something; a folder missing
// everywhere is fine; the container itself is refused. A Graph folder DELETE
// is recursive: the caller makes the window between the listing and the
// DELETE empty — the vault parity reconcile, the only writer and the only
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
	if err := d.Delete(ctx, container, sub); err != nil && !odIsNotFound(err) {
		return fmt.Errorf("onedrive remove dir %s: %w", sub, err)
	}
	return nil
}

type odChild struct {
	name   string
	folder bool
}

// children is the union of the folder's direct children on every account
// (a name once; a folder on any account is a folder).
func (d *OneDriveDriver) children(ctx context.Context, folderPath string) ([]odChild, error) {
	seen := make(map[string]int)
	var out []odChild
	for _, t := range d.tenants {
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
			} `json:"value"`
			NextLink string `json:"@odata.nextLink"`
		}
		decodeErr := json.NewDecoder(resp.Body).Decode(&result)
		_ = resp.Body.Close()
		if decodeErr != nil {
			return fmt.Errorf("onedrive list decode: %w", decodeErr)
		}
		for _, item := range result.Value {
			fn(odChild{name: item.Name, folder: item.Folder != nil})
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

// HealthCheck asks EVERY account for its drive metadata, odProbeConcurrency
// at once, within the caller's (the probe's 15 s) context, and reports the
// worst: an error naming each failing account (R7-18: one rotating account
// per call saw a dead one 1/N of the time). An account whose outcome changed
// since its last probe is logged — the first probe after a boot names every
// account.
func (d *OneDriveDriver) HealthCheck(ctx context.Context) error {
	errs := make([]error, len(d.tenants))
	sem := make(chan struct{}, odProbeConcurrency)
	var wg sync.WaitGroup
	for i, t := range d.tenants {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			errs[i] = t.probe(ctx)
		}()
	}
	wg.Wait()
	var failing []string
	for i, t := range d.tenants {
		d.logProbeChange(t, errs[i])
		if errs[i] != nil {
			failing = append(failing, fmt.Sprintf("%s: %v", t.name, errs[i]))
		}
	}
	if len(failing) > 0 {
		return fmt.Errorf("onedrive health: %d of %d accounts failing: %s",
			len(failing), len(d.tenants), strings.Join(failing, "; "))
	}
	return nil
}

// probe is one account's health: its drive's metadata.
func (t *odTenant) probe(ctx context.Context) error {
	driveID, err := t.getDriveID(ctx)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("%s/drives/%s", graphBase, driveID), nil)
	if err != nil {
		return fmt.Errorf("probe request: %w", err)
	}
	resp, err := t.graphDo(ctx, req)
	if err != nil {
		return err
	}
	odPooledDrain(resp.Body)
	_ = resp.Body.Close()
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

func (t *odTenant) token(ctx context.Context) (string, error) {
	t.tokenMu.Lock()
	defer t.tokenMu.Unlock()

	if t.cachedTok != "" && time.Now().Before(t.tokExpiry) {
		return t.cachedTok, nil
	}

	tok, err := t.cred.GetToken(ctx, policy.TokenRequestOptions{
		Scopes: []string{"https://graph.microsoft.com/.default"},
	})
	if err != nil {
		return "", fmt.Errorf("onedrive token [%s]: %w", t.name, err)
	}
	t.cachedTok = tok.Token
	t.tokExpiry = tok.ExpiresOn.Add(-5 * time.Minute)
	return tok.Token, nil
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

// retryWait is the wait before another attempt, capped at odMaxRetryWait;
// false when there is no attempt to make: the caller is gone, or the wait
// would outlast the caller's deadline (a Retry-After past it is given up on
// at once, never slept through).
func retryWait(ctx context.Context, d time.Duration) (time.Duration, bool) {
	if ctx.Err() != nil {
		return 0, false
	}
	if d > odMaxRetryWait {
		d = odMaxRetryWait
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
	attempts := odAttempts(ctx)
	base := 500 * time.Millisecond
	prev := base

	token, err := t.token(ctx)
	if err != nil {
		return nil, err
	}

	var lastErr error
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
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("User-Agent", odUserAgent)

		resp, err := t.graphHTTP.Do(req)
		t.totalReqs.Add(1)
		var wait time.Duration
		switch {
		case err != nil:
			if ctxErr := ctx.Err(); ctxErr != nil {
				return nil, fmt.Errorf("onedrive [%s]: %w", t.name, ctxErr)
			}
			lastErr = fmt.Errorf("onedrive [%s]: %w", t.name, err)
			prev = odDecorrelatedJitter(base, prev, odMaxRetryWait)
			wait = prev
		case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == http.StatusServiceUnavailable:
			t.throttleCount.Add(1)
			retryAfter, _ := strconv.Atoi(resp.Header.Get("Retry-After"))
			wait = odDecorrelatedJitter(base, prev, odMaxRetryWait)
			if ra := time.Duration(retryAfter) * time.Second; ra > wait {
				wait = ra
			}
			if wait > odMaxRetryWait {
				wait = odMaxRetryWait
			}
			t.throttledUntil.Store(time.Now().Add(wait).Unix())
			prev = wait
			odPooledDrain(resp.Body)
			_ = resp.Body.Close()
			lastErr = fmt.Errorf("onedrive throttled [%s] (%d)", t.name, resp.StatusCode)
		case resp.StatusCode >= 500:
			odPooledDrain(resp.Body)
			_ = resp.Body.Close()
			lastErr = fmt.Errorf("onedrive [%s] HTTP %d", t.name, resp.StatusCode)
			prev = odDecorrelatedJitter(base, prev, odMaxRetryWait)
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
		w, ok := retryWait(ctx, wait)
		if !ok {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return nil, fmt.Errorf("%w (%w)", lastErr, ctxErr)
			}
			return nil, lastErr
		}
		if err := t.sleep(ctx, w); err != nil {
			return nil, fmt.Errorf("%w (waiting to retry: %w)", lastErr, err)
		}
	}
	return nil, lastErr
}

// trackRateLimit records Graph's RateLimit headers for throttle visibility.
func (t *odTenant) trackRateLimit(resp *http.Response) {
	if remaining := resp.Header.Get("RateLimit-Remaining"); remaining != "" {
		if n, err := strconv.ParseInt(remaining, 10, 64); err == nil {
			t.rateLimitRemaining.Store(n)
		}
	}
	if reset := resp.Header.Get("RateLimit-Reset"); reset != "" {
		if n, err := strconv.ParseInt(reset, 10, 64); err == nil {
			t.rateLimitReset.Store(time.Now().Add(time.Duration(n) * time.Second).Unix())
		}
	}
}

func (t *odTenant) getDriveID(ctx context.Context) (string, error) {
	t.mu.Lock()
	if t.driveID != "" {
		d := t.driveID
		t.mu.Unlock()
		return d, nil
	}
	t.mu.Unlock()

	u := fmt.Sprintf("%s/users/%s/drives", graphBase, url.PathEscape(t.userUPN))
	req, _ := http.NewRequestWithContext(ctx, "GET", u, nil)
	resp, err := t.graphDo(ctx, req)
	if err != nil {
		return "", fmt.Errorf("get drives [%s]: %w", t.name, err)
	}
	defer func() { _ = resp.Body.Close() }()

	var out struct {
		Value []struct {
			ID string `json:"id"`
		} `json:"value"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", fmt.Errorf("decode drives [%s]: %w", t.name, err)
	}
	if len(out.Value) == 0 {
		return "", fmt.Errorf("no drives for %s", t.userUPN)
	}

	t.mu.Lock()
	t.driveID = out.Value[0].ID
	t.mu.Unlock()
	return t.driveID, nil
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

func (t *odTenant) simpleUpload(ctx context.Context, driveID, parentID, path string, data []byte) error {
	u := fmt.Sprintf("%s/drives/%s/items/%s:/%s:/content",
		graphBase, driveID, parentID, odEscapePath(path))
	req, _ := http.NewRequestWithContext(ctx, "PUT", u, bytes.NewReader(data))
	req.ContentLength = int64(len(data))
	req.Header.Set("Content-Type", "application/octet-stream")
	req.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(data)), nil }
	resp, err := t.graphDo(ctx, req)
	if err != nil {
		return fmt.Errorf("onedrive simple upload: %w", err)
	}
	odPooledDrain(resp.Body)
	_ = resp.Body.Close()
	return nil
}

// putChunk sends one upload fragment with the bounded retries (odAttempts;
// 1 s, then 2 s between, on ctx): a transport error, a 429 or a 5xx is sent
// again — the same byte range of the same session, so nothing is doubled; a
// caller already gone is never retried. The body is fenced after each
// attempt: the transport may still read a request body after Do returns,
// and chunk is the caller's pooled buffer.
func (t *odTenant) putChunk(ctx context.Context, uploadURL string, chunk []byte, start, endInc, total int) error {
	attempts := odAttempts(ctx)
	var lastErr error
	for attempt := 0; attempt < attempts; attempt++ {
		if attempt > 0 {
			w, ok := retryWait(ctx, time.Duration(1<<uint(attempt-1))*time.Second)
			if !ok {
				if ctxErr := ctx.Err(); ctxErr != nil {
					return fmt.Errorf("onedrive chunk: %w (%w)", lastErr, ctxErr)
				}
				break
			}
			if err := t.sleep(ctx, w); err != nil {
				return fmt.Errorf("onedrive chunk: %w (waiting to retry: %w)", lastErr, err)
			}
		}
		body := &odFragment{b: chunk}
		req, err := http.NewRequestWithContext(ctx, http.MethodPut, uploadURL, body)
		if err != nil {
			return fmt.Errorf("onedrive chunk request: %w", err)
		}
		req.ContentLength = int64(len(chunk))
		req.Header.Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, endInc, total))
		// No Authorization header on chunk PUTs — the upload URL is pre-authenticated
		resp, err := t.uploadHTTP.Do(req)
		body.fence()
		t.totalReqs.Add(1)
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return fmt.Errorf("onedrive chunk: %w", ctxErr)
			}
			lastErr = err
			continue
		}
		odPooledDrain(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			return nil
		}
		if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500 {
			lastErr = fmt.Errorf("onedrive chunk HTTP %d", resp.StatusCode)
			continue
		}
		return fmt.Errorf("onedrive chunk HTTP %d", resp.StatusCode)
	}
	return fmt.Errorf("onedrive chunk exhausted retries: %w", lastErr)
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

// streamingChunkedUpload reads chunks directly from the io.Reader without
// buffering the entire body: the caller's ContentLength, or the size of the
// spool an unknown-length Put was materialised into. One pooled fragment
// buffer per upload.
func (t *odTenant) streamingChunkedUpload(ctx context.Context, driveID, parentID, path string, data io.Reader, totalSize int64) error {
	sessionURL := fmt.Sprintf("%s/drives/%s/items/%s:/%s:/createUploadSession",
		graphBase, driveID, parentID, odEscapePath(path))
	sb, _ := json.Marshal(map[string]any{
		"item": map[string]any{"@microsoft.graph.conflictBehavior": "replace"},
	})
	req, _ := http.NewRequestWithContext(ctx, "POST", sessionURL, bytes.NewReader(sb))
	req.Header.Set("Content-Type", "application/json")
	req.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(sb)), nil }
	resp, err := t.graphDo(ctx, req)
	if err != nil {
		return fmt.Errorf("onedrive create upload session: %w", err)
	}
	var session struct {
		UploadURL string `json:"uploadUrl"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&session)
	_ = resp.Body.Close()

	if session.UploadURL == "" {
		return fmt.Errorf("onedrive: empty upload session URL")
	}

	bp := odChunkPool.Get().(*[]byte)
	defer odChunkPool.Put(bp)
	buf := *bp
	offset := int64(0)
	for offset < totalSize {
		chunkEnd := offset + int64(odChunkSize)
		if chunkEnd > totalSize {
			chunkEnd = totalSize
		}
		toRead := chunkEnd - offset

		n, err := io.ReadFull(data, buf[:toRead])
		if err != nil {
			// A body shorter than its length is an error, never a shorter file.
			return fmt.Errorf("onedrive read chunk at offset %d: %w", offset, err)
		}

		if err := t.putChunk(ctx, session.UploadURL, buf[:n], int(offset), int(offset)+n-1, int(totalSize)); err != nil {
			return fmt.Errorf("onedrive streaming chunk at offset %d: %w", offset, err)
		}
		offset += int64(n)
	}
	return nil
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
