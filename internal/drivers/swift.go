package drivers

import (
	"bufio"
	"bytes"
	"context"
	"crypto/md5" // #nosec G501 -- Swift's ETag of an object or piece IS the MD5 of its bytes; compared, never a security primitive
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"go.uber.org/zap"
	"golang.org/x/sync/errgroup"
	"golang.org/x/sync/semaphore"

	"github.com/FairForge/vaultaire/internal/engine"
)

// SwiftDriver stores objects on OpenStack Swift behind Keystone v2 — written
// for Blomp (registered as `blomp`, swift_README.md), generic otherwise.
//
// Layout: ONE container (Blomp gives an account exactly one, named after its
// e-mail; creating another is 403) holding `t-<tenant>/<container>/<artifact>`
// like every fixed-bucket driver, the tenant from the call's context
// (ErrNoTenant without one). An object larger than SegmentSize (or of
// unknown length past swiftPeekBytes) is a static large object: its pieces
// at `t-<tenant>/_segments/<digest>/<generation>/<n>` IN THE SAME container
// (rclone puts them in `<container>_segments`, which Blomp refuses — that is
// the "Permission denied" its users report), then the manifest at the key.
// The pieces go up PutConcurrency at a time, each held in memory (so a 5xx
// on one is retried) under a process-wide budget of PutBudget bytes: one
// stream to Blomp carries 37–62 MB/s, sixteen 275 MB/s (prod, 2026-10-21).
//
// What Blomp measured on 2026-10-21 shaped the rest: a token lives one hour
// (re-authenticated 5 min before expiry and once on a 401); the storage URL
// is plain HTTP (CleartextTransport: callers never send it bytes that are
// not already sealed); writes are rate-limited per container (~3 PUT/s), so
// a delete HEADs first and issues ONE write; a container listing lags.
type SwiftDriver struct {
	name   string
	cfg    SwiftConfig
	client *http.Client
	logger *zap.Logger
	now    func() time.Time

	// putBudget holds the bytes of the pieces in memory, process-wide;
	// pieceBuf recycles the buffers of SegmentSize pieces.
	putBudget      *semaphore.Weighted
	pieceBuf       sync.Pool
	segmentBackoff func(attempt int) time.Duration
	// cleanups counts the background removals of a deleted large object's
	// pieces (waitCleanups, for tests).
	cleanups sync.WaitGroup

	mu         sync.Mutex
	token      string
	storageURL string
	expires    time.Time

	listPageSize int
}

// SwiftConfig is one Swift account.
type SwiftConfig struct {
	AuthURL     string // Keystone v2 base, e.g. https://authenticate.blomp.com/v2.0
	User        string
	Password    string // never logged
	Tenant      string // Keystone tenant (project) name: `storage` on Blomp
	Container   string // the account's container: the user's e-mail on Blomp
	SegmentSize int64  // objects above this are stored as SLO pieces of this size
	// PutConcurrency is how many pieces of one large object go up at once
	// (0 = swiftDefaultPutConcurrency); PutBudget bounds the bytes of the
	// pieces every upload of the process holds (0 = swiftDefaultPutBudget).
	PutConcurrency int
	PutBudget      int64
	// ListingSettle is how long the container's listing may lag a write
	// (engine.ListingLagger): 0 = swiftDefaultListingSettle, < 0 = no lag.
	ListingSettle time.Duration
}

const (
	// BlompDefaultAuthURL is the Keystone v2 endpoint Blomp's dashboard names.
	BlompDefaultAuthURL = "https://authenticate.blomp.com/v2.0"
	// BlompDefaultTenant is the tenant every Blomp account authenticates to.
	BlompDefaultTenant = "storage"
	// swiftMaxObjectSize is Swift's max_file_size (5 GiB + 2 on Blomp's /info).
	swiftMaxObjectSize = 5 << 30
	// swiftDefaultSegmentSize: whole up to 64 MiB, SLO above — the size the
	// parallel 64 MiB PUTs reached 275 MB/s at.
	swiftDefaultSegmentSize = 64 << 20
	// swiftDefaultPutConcurrency pieces of one object in flight.
	swiftDefaultPutConcurrency = 4
	swiftMaxPutConcurrency     = 16
	// swiftDefaultPutBudget bounds the pieces held in memory by every
	// upload of the process (16 pieces of 64 MiB).
	swiftDefaultPutBudget = 1 << 30
	// swiftMaxManifestSegments is Swift's max_manifest_segments (Blomp's
	// /info: 1000).
	swiftMaxManifestSegments = 1000
	// swiftMinSegmentSize is Swift 2.17's min_segment_size: every piece of a
	// large object but the last must be at least this.
	swiftMinSegmentSize = 1 << 20
	// swiftMaxNameBytes is Swift's max_object_name_length.
	swiftMaxNameBytes = 1024
	// swiftPeekBytes is how much of a body of unknown length is held to
	// decide between one PUT and a large object (and to retry that PUT).
	swiftPeekBytes = 8 << 20
	// swiftTokenMargin re-authenticates this long before the token expires.
	swiftTokenMargin = 5 * time.Minute
	// Bounds (internal/drivers/CLAUDE.md, the bound invariant): a request
	// waits 25 s for its first byte after its body (the transport's
	// ResponseHeaderTimeout: a GET, a PUT's answer); HEAD, DELETE, a listing
	// page and the token request are bounded whole at 15 s.
	swiftAnswerTimeout = 25 * time.Second
	swiftMetaTimeout   = 15 * time.Second
	// swiftWalkPageTimeout bounds one listing page of a job's walk.
	swiftWalkPageTimeout = 60 * time.Second
	// swiftCleanupTimeout bounds the bulk delete of a failed upload's or a
	// deleted large object's pieces, plus swiftCleanupPerPiece for each
	// (Blomp removes ~3 objects a second), at most swiftCleanupMax.
	swiftCleanupTimeout  = 2 * time.Minute
	swiftCleanupPerPiece = 500 * time.Millisecond
	swiftCleanupMax      = 15 * time.Minute
	swiftAttempts        = 3
	// swiftSegmentAttempts: a piece of a large object is held in memory and
	// sent up to 5 times, 1/2/4/8 s apart (+ up to half again of jitter).
	// Prod 2026-10-21 11:05 UTC: Blomp answered 503 to a streamed 1 GiB
	// piece three uploads running — once after 633 MB of it — and a
	// streamed piece cannot be sent again, so the 6 GiB upload failed.
	swiftSegmentAttempts = 5
	swiftListPage        = 1000
	// swiftDefaultListingSettle is how long a container listing may lag a
	// write: Blomp's hid ~30 existing objects for over 90 minutes on
	// 2026-10-21, so twice that. swiftMinListingSettle and
	// swiftMaxListingSettle bound BLOMP_LISTING_SETTLE.
	swiftDefaultListingSettle = 3 * time.Hour
	swiftMinListingSettle     = time.Minute
	swiftMaxListingSettle     = 48 * time.Hour
)

var (
	swiftRequests = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "vaultaire_swift_requests_total",
		Help: "Requests the Swift driver sent, by backend, method and outcome (ok, client_error, server_error, unauthorized, transport).",
	}, []string{"backend", "method", "outcome"})
	swiftRetries = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "vaultaire_swift_retries_total",
		Help: "Swift requests sent again after a 5xx or a transport error, by backend and method.",
	}, []string{"backend", "method"})
	swiftAuths = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "vaultaire_swift_auths_total",
		Help: "Keystone token requests of the Swift driver, by backend and reason (initial, refresh, unauthorized) and outcome.",
	}, []string{"backend", "reason", "outcome"})
)

// BlompConfigFromEnv reads BLOMP_* (swift_README.md). ok is false when
// BLOMP_PASSWORD is unset: the driver is not registered. A value that is set
// and wrong is an error, never a silent default.
func BlompConfigFromEnv(getenv func(string) string) (SwiftConfig, bool, error) {
	pw := getenv("BLOMP_PASSWORD")
	if pw == "" {
		return SwiftConfig{}, false, nil
	}
	cfg := SwiftConfig{AuthURL: BlompDefaultAuthURL, User: strings.TrimSpace(getenv("BLOMP_USER")), Password: pw,
		Tenant: BlompDefaultTenant, SegmentSize: swiftDefaultSegmentSize,
		PutConcurrency: swiftDefaultPutConcurrency, PutBudget: swiftDefaultPutBudget}
	if cfg.User == "" {
		return SwiftConfig{}, false, errors.New("BLOMP_PASSWORD is set without BLOMP_USER (the account's e-mail)")
	}
	cfg.Container = cfg.User
	if v := strings.TrimSpace(getenv("BLOMP_AUTH_URL")); v != "" {
		cfg.AuthURL = v
	}
	if v := strings.TrimSpace(getenv("BLOMP_TENANT")); v != "" {
		cfg.Tenant = v
	}
	if v := strings.TrimSpace(getenv("BLOMP_CONTAINER")); v != "" {
		cfg.Container = v
	}
	if v := strings.TrimSpace(getenv("BLOMP_SEGMENT_SIZE")); v != "" {
		n, err := parseByteSize(v)
		if err != nil {
			return SwiftConfig{}, false, fmt.Errorf("BLOMP_SEGMENT_SIZE: %w", err)
		}
		if n < swiftMinSegmentSize {
			return SwiftConfig{}, false, fmt.Errorf("BLOMP_SEGMENT_SIZE %s: below Swift's 1 MiB min_segment_size", v)
		}
		cfg.SegmentSize = n
	}
	if v := strings.TrimSpace(getenv("BLOMP_PUT_CONCURRENCY")); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > swiftMaxPutConcurrency {
			return SwiftConfig{}, false, fmt.Errorf("BLOMP_PUT_CONCURRENCY %q: want 1…%d", v, swiftMaxPutConcurrency)
		}
		cfg.PutConcurrency = n
	}
	if v := strings.TrimSpace(getenv("BLOMP_PUT_BUDGET")); v != "" {
		n, err := parseByteSize(v)
		if err != nil {
			return SwiftConfig{}, false, fmt.Errorf("BLOMP_PUT_BUDGET: %w", err)
		}
		cfg.PutBudget = n
	}
	if v := strings.TrimSpace(getenv("BLOMP_LISTING_SETTLE")); v != "" {
		if strings.EqualFold(v, "off") {
			cfg.ListingSettle = -1
		} else {
			d, err := time.ParseDuration(v)
			if err != nil || d < swiftMinListingSettle || d > swiftMaxListingSettle {
				return SwiftConfig{}, false, fmt.Errorf("BLOMP_LISTING_SETTLE %q: want a duration %s…%s, or off",
					v, swiftMinListingSettle, swiftMaxListingSettle)
			}
			cfg.ListingSettle = d
		}
	}
	if err := cfg.validate(); err != nil {
		return SwiftConfig{}, false, err
	}
	return cfg, true, nil
}

func (c SwiftConfig) validate() error {
	u, err := url.Parse(c.AuthURL)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return fmt.Errorf("swift auth URL %q: want http(s)://host/…", c.AuthURL)
	}
	if c.User == "" || c.Password == "" || c.Tenant == "" || c.Container == "" {
		return errors.New("swift: user, password, tenant and container are all required")
	}
	if strings.Contains(c.Container, "/") {
		return fmt.Errorf("swift container %q contains '/'", c.Container)
	}
	if c.SegmentSize < 1 || c.SegmentSize > swiftMaxObjectSize {
		return fmt.Errorf("swift segment size %d: want 1 B … 5 GiB (Swift's max_file_size)", c.SegmentSize)
	}
	if c.PutConcurrency < 0 || c.PutConcurrency > swiftMaxPutConcurrency {
		return fmt.Errorf("swift put concurrency %d: want 1…%d", c.PutConcurrency, swiftMaxPutConcurrency)
	}
	if c.PutBudget != 0 && c.PutBudget < c.SegmentSize {
		return fmt.Errorf("swift put budget %d: below one segment (%d)", c.PutBudget, c.SegmentSize)
	}
	return nil
}

// NewSwiftDriver builds the driver; nothing is sent until the first call.
func NewSwiftDriver(name string, cfg SwiftConfig, logger *zap.Logger) (*SwiftDriver, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	if logger == nil {
		logger = zap.NewNop()
	}
	cfg.AuthURL = strings.TrimSuffix(cfg.AuthURL, "/")
	if cfg.PutConcurrency == 0 {
		cfg.PutConcurrency = swiftDefaultPutConcurrency
	}
	if cfg.PutBudget == 0 {
		cfg.PutBudget = max(swiftDefaultPutBudget, cfg.SegmentSize)
	}
	for _, m := range []string{http.MethodGet, http.MethodPut, http.MethodHead, http.MethodDelete, http.MethodPost} {
		for _, o := range []string{"ok", "client_error", "server_error", "unauthorized", "transport"} {
			swiftRequests.WithLabelValues(name, m, o)
		}
	}
	InitTenantlessSeries(name)
	return &SwiftDriver{name: name, cfg: cfg, logger: logger, now: time.Now, listPageSize: swiftListPage,
		putBudget: semaphore.NewWeighted(cfg.PutBudget), segmentBackoff: swiftSegmentBackoff,
		client: TunedHTTPClient(WithResponseHeaderTimeout(swiftAnswerTimeout), WithHTTP1Only())}, nil
}

// Name is the registration name.
func (d *SwiftDriver) Name() string { return d.name }

// StoreID (engine.StoreIdentifier): the account's container, from the
// configuration (no request needed at boot).
func (d *SwiftDriver) StoreID() string {
	return "swift://" + hostOf(d.cfg.AuthURL) + "/" + d.cfg.Tenant + "/" + d.cfg.User + "/" + d.cfg.Container
}

// CleartextTransport (engine.CleartextTransportDeclarer) is true unless the
// storage URL the token came with is https: before the first token it is
// unknown and reported as cleartext (Blomp's is http://…:8080).
func (d *SwiftDriver) CleartextTransport() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return !strings.HasPrefix(d.storageURL, "https://")
}

// PreferredRangeBytes (engine.RangePartSizer): a parallel GET reads Swift in
// parts of SegmentSize. Every range pays the proxy's slow first byte
// (~650 ms on Blomp), so 16 MiB parts spent most of their time waiting:
// 8 × 64 MiB read a 1 GiB object at 123–369 MB/s, 8 × 16 MiB at 58–157
// (SLC lab 2026-10-21, sha-verified). A part then also matches one SLO piece.
func (d *SwiftDriver) PreferredRangeBytes() int64 { return d.cfg.SegmentSize }

// ObjectKey (engine.KeyAddresser).
func (d *SwiftDriver) ObjectKey(ctx context.Context, container, artifact string) string {
	return tenantKey(contextTenant(ctx), container, artifact)
}

// ---- authentication -------------------------------------------------------

// session returns a token and the storage URL, asking Keystone when there
// is none, it expires within swiftTokenMargin, or force (a 401).
func (d *SwiftDriver) session(ctx context.Context, force bool) (token, storageURL string, err error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	reason := "initial"
	if d.token != "" {
		if !force && d.now().Before(d.expires.Add(-swiftTokenMargin)) {
			return d.token, d.storageURL, nil
		}
		reason = "refresh"
		if force {
			reason = "unauthorized"
		}
	}
	tok, su, exp, err := d.authenticate(ctx)
	if err != nil {
		swiftAuths.WithLabelValues(d.name, reason, "failed").Inc()
		return "", "", err
	}
	swiftAuths.WithLabelValues(d.name, reason, "ok").Inc()
	d.token, d.storageURL, d.expires = tok, su, exp
	return tok, su, nil
}

func (d *SwiftDriver) authenticate(ctx context.Context) (token, storageURL string, expires time.Time, err error) {
	ctx, cancel := context.WithTimeout(ctx, swiftMetaTimeout)
	defer cancel()
	var body bytes.Buffer
	req := map[string]any{"auth": map[string]any{"tenantName": d.cfg.Tenant,
		"passwordCredentials": map[string]string{"username": d.cfg.User, "password": d.cfg.Password}}}
	if err := json.NewEncoder(&body).Encode(req); err != nil {
		return "", "", time.Time{}, fmt.Errorf("%s auth: %w", d.name, err)
	}
	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, d.cfg.AuthURL+"/tokens", &body)
	if err != nil {
		return "", "", time.Time{}, fmt.Errorf("%s auth: %w", d.name, err)
	}
	hreq.Header.Set("Content-Type", "application/json")
	hreq.Header.Set("Accept", "application/json")
	resp, err := d.client.Do(hreq)
	if err != nil {
		return "", "", time.Time{}, fmt.Errorf("%s auth: %w", d.name, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return "", "", time.Time{}, &SwiftError{Op: "auth", Status: resp.StatusCode, Body: strings.TrimSpace(string(msg))}
	}
	var out struct {
		Access struct {
			Token struct {
				ID      string `json:"id"`
				Expires string `json:"expires"`
			} `json:"token"`
			ServiceCatalog []struct {
				Type      string `json:"type"`
				Endpoints []struct {
					PublicURL string `json:"publicURL"`
				} `json:"endpoints"`
			} `json:"serviceCatalog"`
		} `json:"access"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&out); err != nil {
		return "", "", time.Time{}, fmt.Errorf("%s auth: decode: %w", d.name, err)
	}
	for _, s := range out.Access.ServiceCatalog {
		if s.Type == "object-store" && len(s.Endpoints) > 0 {
			storageURL = strings.TrimSuffix(s.Endpoints[0].PublicURL, "/")
		}
	}
	if out.Access.Token.ID == "" || storageURL == "" {
		return "", "", time.Time{}, fmt.Errorf("%s auth: no token or no object-store endpoint in the answer", d.name)
	}
	expires = d.now().Add(time.Hour)
	for _, layout := range []string{"2006-01-02T15:04:05.999999Z", time.RFC3339Nano, "2006-01-02T15:04:05Z"} {
		if t, perr := time.Parse(layout, out.Access.Token.Expires); perr == nil {
			expires = t
			break
		}
	}
	return out.Access.Token.ID, storageURL, expires, nil
}

// ---- requests -------------------------------------------------------------

// SwiftError is an answer the driver does not turn into a value.
type SwiftError struct {
	Op     string
	Status int
	Body   string
}

// Unwrap names a full account: Swift's container quota answers 413 "Upload
// exceeds quota." (Blomp's 40 GiB, prod 2026-10-21) and some proxies 507.
// engine.ErrBackendFull is a verdict, not a failure — no retry, no breaker
// charge, a 507 to the client.
func (e *SwiftError) Unwrap() error {
	if e.Status == http.StatusRequestEntityTooLarge || e.Status == http.StatusInsufficientStorage {
		return engine.ErrBackendFull
	}
	return nil
}

func (e *SwiftError) Error() string {
	if e.Body != "" {
		return fmt.Sprintf("swift %s: HTTP %d: %s", e.Op, e.Status, e.Body)
	}
	return fmt.Sprintf("swift %s: HTTP %d", e.Op, e.Status)
}

// swiftRequest is one request; body is re-sent from bodyBytes on a retry
// (a streamed body is sent once).
type swiftRequest struct {
	method    string
	name      string // object name in the container; "" = the container
	account   bool   // the account URL (bulk delete)
	query     url.Values
	header    http.Header
	body      io.Reader
	bodyBytes []byte
	size      int64 // Content-Length; -1 = chunked
	attempts  int   // 0 = swiftAttempts; more = a piece, backed off by segmentBackoff
}

func (r swiftRequest) replayable() bool { return r.body == nil }

// do sends req with the session's token: once more after a 401 (a fresh
// token), and up to swiftAttempts in all after a 5xx or a transport error
// when the body can be sent again. The caller closes the body.
func (d *SwiftDriver) do(ctx context.Context, req swiftRequest) (*http.Response, error) {
	force := false
	reauthed := false
	var lastErr error
	attempts, backoff := swiftAttempts, swiftBackoff
	if req.attempts > 0 {
		attempts, backoff = req.attempts, d.segmentBackoff
	}
	for attempt := 1; attempt <= attempts; attempt++ {
		token, storageURL, err := d.session(ctx, force)
		if err != nil {
			return nil, err
		}
		force = false
		hreq, err := d.newRequest(ctx, storageURL, token, req)
		if err != nil {
			return nil, err
		}
		resp, err := d.client.Do(hreq)
		switch {
		case err != nil:
			swiftRequests.WithLabelValues(d.name, req.method, "transport").Inc()
			lastErr = fmt.Errorf("%s %s %s: %w", d.name, req.method, req.name, err)
			if ctx.Err() != nil || !req.replayable() {
				return nil, lastErr
			}
		case resp.StatusCode == http.StatusUnauthorized:
			swiftRequests.WithLabelValues(d.name, req.method, "unauthorized").Inc()
			drain(resp)
			lastErr = &SwiftError{Op: req.method + " " + req.name, Status: resp.StatusCode}
			if reauthed || !req.replayable() {
				return nil, lastErr
			}
			reauthed, force = true, true
			attempt-- // a fresh token is not a retry
			continue
		case resp.StatusCode >= 500:
			swiftRequests.WithLabelValues(d.name, req.method, "server_error").Inc()
			msg, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
			drain(resp)
			lastErr = &SwiftError{Op: req.method + " " + req.name, Status: resp.StatusCode, Body: strings.TrimSpace(string(msg))}
			if !req.replayable() {
				return nil, lastErr
			}
		default:
			outcome := "ok"
			if resp.StatusCode >= 400 {
				outcome = "client_error"
			}
			swiftRequests.WithLabelValues(d.name, req.method, outcome).Inc()
			return resp, nil
		}
		if attempt < attempts {
			swiftRetries.WithLabelValues(d.name, req.method).Inc()
			if err := sleepCtx(ctx, backoff(attempt)); err != nil {
				return nil, lastErr
			}
		}
	}
	return nil, lastErr
}

func (d *SwiftDriver) newRequest(ctx context.Context, storageURL, token string, req swiftRequest) (*http.Request, error) {
	u := storageURL
	if !req.account {
		u += "/" + url.PathEscape(d.cfg.Container)
		if req.name != "" {
			u += "/" + escapeSwiftName(req.name)
		}
	}
	if len(req.query) > 0 {
		u += "?" + req.query.Encode()
	}
	body := req.body
	switch {
	case req.bodyBytes != nil && len(req.bodyBytes) == 0:
		body = http.NoBody
	case req.bodyBytes != nil:
		body = bytes.NewReader(req.bodyBytes)
	}
	hreq, err := http.NewRequestWithContext(ctx, req.method, u, body)
	if err != nil {
		return nil, fmt.Errorf("%s %s: %w", d.name, req.method, err)
	}
	for k, vs := range req.header {
		for _, v := range vs {
			hreq.Header.Add(k, v)
		}
	}
	hreq.Header.Set("X-Auth-Token", token)
	if body != nil {
		hreq.ContentLength = req.size
		if req.bodyBytes != nil {
			hreq.ContentLength = int64(len(req.bodyBytes))
		}
	}
	return hreq, nil
}

// escapeSwiftName escapes an object name segment by segment, keeping '/'.
func escapeSwiftName(name string) string {
	segs := strings.Split(name, "/")
	for i, s := range segs {
		segs[i] = url.PathEscape(s)
	}
	return strings.Join(segs, "/")
}

func swiftBackoff(attempt int) time.Duration {
	base := time.Duration(attempt*attempt) * 250 * time.Millisecond
	j, err := rand.Int(rand.Reader, big.NewInt(int64(base/2)+1))
	if err != nil {
		return base
	}
	return base + time.Duration(j.Int64())
}

// swiftSegmentBackoff waits 1, 2, 4, 8 s before a piece's next attempt,
// plus up to half again of jitter.
func swiftSegmentBackoff(attempt int) time.Duration {
	base := time.Second << (attempt - 1)
	j, err := rand.Int(rand.Reader, big.NewInt(int64(base/2)+1))
	if err != nil {
		return base
	}
	return base + time.Duration(j.Int64())
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func drain(resp *http.Response) {
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	_ = resp.Body.Close()
}

// meta runs a HEAD/DELETE/listing-page request under the 15 s bound and
// reads its body whole (≤ 16 MiB).
func (d *SwiftDriver) meta(ctx context.Context, req swiftRequest) (int, http.Header, []byte, error) {
	ctx, cancel := context.WithTimeout(ctx, swiftMetaTimeout)
	defer cancel()
	return d.metaWithin(ctx, req)
}

func (d *SwiftDriver) metaWithin(ctx context.Context, req swiftRequest) (int, http.Header, []byte, error) {
	resp, err := d.do(ctx, req)
	if err != nil {
		return 0, nil, nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return 0, nil, nil, fmt.Errorf("%s %s %s: read body: %w", d.name, req.method, req.name, err)
	}
	return resp.StatusCode, resp.Header, body, nil
}

// ---- names ----------------------------------------------------------------

// objectName is the stored name of (tenant from ctx, container, artifact).
func (d *SwiftDriver) objectName(ctx context.Context, op, container, artifact string) (string, error) {
	tenantID, err := requireTenant(ctx, d.name, op, "", d.logger)
	if err != nil {
		return "", err
	}
	name := tenantKey(tenantID, container, artifact)
	if err := checkSwiftName(name); err != nil {
		return "", fmt.Errorf("%s %s: %w", d.name, op, err)
	}
	return name, nil
}

// checkSwiftName refuses names Swift cannot hold or a proxy would rewrite.
func checkSwiftName(name string) error {
	if len(name) > swiftMaxNameBytes {
		return fmt.Errorf("%w: object name of %d bytes (Swift holds %d)", engine.ErrInvalidInput, len(name), swiftMaxNameBytes)
	}
	for _, s := range strings.Split(name, "/") {
		if s == "." || s == ".." {
			return fmt.Errorf("%w: object name with a %q segment", engine.ErrInvalidInput, s)
		}
	}
	if strings.ContainsRune(name, 0) {
		return fmt.Errorf("%w: object name with a NUL", engine.ErrInvalidInput)
	}
	return nil
}

// ---- engine.Driver --------------------------------------------------------

// Get reads an object (a large one is assembled by Swift).
func (d *SwiftDriver) Get(ctx context.Context, container, artifact string) (io.ReadCloser, error) {
	name, err := d.objectName(ctx, "Get", container, artifact)
	if err != nil {
		return nil, err
	}
	resp, err := d.do(ctx, swiftRequest{method: http.MethodGet, name: name})
	if err != nil {
		return nil, err
	}
	switch resp.StatusCode {
	case http.StatusOK:
		return resp.Body, nil
	case http.StatusNotFound:
		drain(resp)
		return nil, engine.ErrNotFound(container, artifact)
	}
	drain(resp)
	return nil, &SwiftError{Op: "GET " + name, Status: resp.StatusCode}
}

// GetRange reads length bytes from offset (engine.RangeGetter). A 200 to the
// Range (a proxy that ignores it) is skipped to offset and cut to length.
func (d *SwiftDriver) GetRange(ctx context.Context, container, artifact string, offset, length int64) (io.ReadCloser, error) {
	rc, _, err := d.GetRangeInfo(ctx, container, artifact, offset, length)
	return rc, err
}

// GetRangeInfo is GetRange reporting the object's identity
// (engine.VersionedRangeGetter): its ETag — for a large object, Swift's
// MD5 of the pieces' ETags, the same on every range of one manifest — and
// its total size from Content-Range (Content-Length on a 200). One ranged
// GET, so the parallel ranged GET may read a large object as many ranges:
// 8 ranges of a 256 MiB Blomp object read 163 MB/s, one stream 56–122.
func (d *SwiftDriver) GetRangeInfo(ctx context.Context, container, artifact string, offset, length int64) (io.ReadCloser, engine.RangeInfo, error) {
	if offset < 0 || length <= 0 {
		return nil, engine.RangeInfo{}, fmt.Errorf("%w: range offset %d length %d", engine.ErrInvalidInput, offset, length)
	}
	name, err := d.objectName(ctx, "GetRange", container, artifact)
	if err != nil {
		return nil, engine.RangeInfo{}, err
	}
	h := http.Header{}
	h.Set("Range", fmt.Sprintf("bytes=%d-%d", offset, offset+length-1))
	resp, err := d.do(ctx, swiftRequest{method: http.MethodGet, name: name, header: h})
	if err != nil {
		return nil, engine.RangeInfo{}, err
	}
	info := engine.RangeInfo{ETag: strings.Trim(resp.Header.Get("Etag"), `"`)}
	switch resp.StatusCode {
	case http.StatusPartialContent:
		total, ok := contentRangeTotal(resp.Header.Get("Content-Range"))
		if !ok {
			drain(resp)
			return nil, engine.RangeInfo{}, fmt.Errorf("%s GetRange %s: Content-Range %q names no total size", d.name, name, resp.Header.Get("Content-Range"))
		}
		info.Size = total
		return resp.Body, info, nil
	case http.StatusOK:
		info.Size = resp.ContentLength
		if _, err := io.CopyN(io.Discard, resp.Body, offset); err != nil {
			_ = resp.Body.Close()
			return nil, engine.RangeInfo{}, fmt.Errorf("%s GetRange %s: skip to %d: %w", d.name, name, offset, err)
		}
		return struct {
			io.Reader
			io.Closer
		}{io.LimitReader(resp.Body, length), resp.Body}, info, nil
	case http.StatusNotFound:
		drain(resp)
		return nil, engine.RangeInfo{}, engine.ErrNotFound(container, artifact)
	}
	drain(resp)
	return nil, engine.RangeInfo{}, &SwiftError{Op: "GET (range) " + name, Status: resp.StatusCode}
}

// contentRangeTotal reads the total of `bytes a-b/total`.
func contentRangeTotal(v string) (int64, bool) {
	_, total, ok := strings.Cut(v, "/")
	if !ok {
		return 0, false
	}
	n, err := strconv.ParseInt(strings.TrimSpace(total), 10, 64)
	return n, err == nil && n >= 0
}

// Exists HEADs the object.
func (d *SwiftDriver) Exists(ctx context.Context, container, artifact string) (bool, error) {
	name, err := d.objectName(ctx, "Exists", container, artifact)
	if err != nil {
		return false, err
	}
	status, _, _, err := d.meta(ctx, swiftRequest{method: http.MethodHead, name: name})
	if err != nil {
		return false, err
	}
	switch status {
	case http.StatusOK, http.StatusNoContent:
		return true, nil
	case http.StatusNotFound:
		return false, nil
	}
	return false, &SwiftError{Op: "HEAD " + name, Status: status}
}

// Delete removes an object — a large object with its pieces. A miss is
// engine.NotFoundError.
func (d *SwiftDriver) Delete(ctx context.Context, container, artifact string) error {
	name, err := d.objectName(ctx, "Delete", container, artifact)
	if err != nil {
		return err
	}
	found, err := d.deleteName(ctx, name)
	if err != nil {
		return err
	}
	if !found {
		return engine.ErrNotFound(container, artifact)
	}
	return nil
}

// deleteName deletes one stored name with ONE write in the caller's time: a
// HEAD tells a large object from a plain one (Swift answers 200 with "Not an
// SLO manifest" to `?multipart-manifest=delete` on a plain object, so asking
// blindly would cost a second write). A plain object is one DELETE; a large
// one is its manifest read, the manifest deleted, and its pieces removed by
// one bulk delete in the background. found is false for a miss.
func (d *SwiftDriver) deleteName(ctx context.Context, name string) (found bool, err error) {
	status, hdr, _, err := d.meta(ctx, swiftRequest{method: http.MethodHead, name: name})
	if err != nil {
		return false, err
	}
	switch status {
	case http.StatusNotFound:
		return false, nil
	case http.StatusOK, http.StatusNoContent:
	default:
		return false, &SwiftError{Op: "HEAD " + name, Status: status}
	}
	if !strings.EqualFold(hdr.Get("X-Static-Large-Object"), "true") {
		status, _, _, err := d.meta(ctx, swiftRequest{method: http.MethodDelete, name: name})
		if err != nil {
			return false, err
		}
		switch status {
		case http.StatusNoContent, http.StatusOK:
			return true, nil
		case http.StatusNotFound:
			return false, nil
		}
		return false, &SwiftError{Op: "DELETE " + name, Status: status}
	}
	// A large object: its manifest alone (one write), then its pieces in
	// the background. Swift's own ?multipart-manifest=delete removes the
	// pieces one by one inside the request — at Blomp's ~3 writes a second
	// a 6 GiB object of 96 pieces outran the 15 s bound (prod 2026-10-21).
	segs, err := d.manifestSegments(ctx, name)
	if err != nil {
		return false, err
	}
	status, _, _, err = d.meta(ctx, swiftRequest{method: http.MethodDelete, name: name})
	if err != nil {
		return false, err
	}
	switch status {
	case http.StatusNoContent, http.StatusOK:
	case http.StatusNotFound:
		return false, nil
	default:
		return false, &SwiftError{Op: "DELETE (manifest) " + name, Status: status}
	}
	if len(segs) > 0 {
		d.cleanups.Add(1)
		go func() {
			defer d.cleanups.Done()
			d.removeSegments(ctx, name, segs)
		}()
	}
	return true, nil
}

// manifestSegments names the pieces of a large object: its manifest read
// with ?multipart-manifest=get. Only names under a tenant's _segments/ are
// returned — a manifest never makes us delete anything else.
func (d *SwiftDriver) manifestSegments(ctx context.Context, name string) ([]string, error) {
	status, _, body, err := d.meta(ctx, swiftRequest{method: http.MethodGet, name: name,
		query: url.Values{"multipart-manifest": {"get"}}})
	if err != nil {
		return nil, err
	}
	if status == http.StatusNotFound {
		return nil, nil
	}
	if status != http.StatusOK {
		return nil, &SwiftError{Op: "GET (manifest) " + name, Status: status}
	}
	var entries []struct {
		Name string `json:"name"` // Swift's listing shape
		Path string `json:"path"` // format=raw
	}
	if err := json.Unmarshal(body, &entries); err != nil {
		return nil, fmt.Errorf("%s delete %s: manifest: %w", d.name, name, err)
	}
	prefix := "/" + d.cfg.Container + "/"
	var segs []string
	for _, e := range entries {
		p := e.Name
		if p == "" {
			p = e.Path
		}
		if u, err := url.PathUnescape(p); err == nil {
			p = u
		}
		seg, ok := strings.CutPrefix(p, prefix)
		if !ok || !strings.HasPrefix(seg, "t-") || !strings.Contains(seg, "/_segments/") {
			continue
		}
		segs = append(segs, seg)
	}
	return segs, nil
}

// waitCleanups waits for the background removals of pieces (tests).
func (d *SwiftDriver) waitCleanups() { d.cleanups.Wait() }

// swiftBulkResult is the JSON body of a bulk delete or a manifest delete.
type swiftBulkResult struct {
	Deleted  int        `json:"Number Deleted"`
	NotFound int        `json:"Number Not Found"`
	Status   string     `json:"Response Status"`
	Errors   [][]string `json:"Errors"`
}

func parseSwiftBulk(body []byte) (swiftBulkResult, error) {
	var r swiftBulkResult
	if err := json.Unmarshal(body, &r); err != nil {
		return r, fmt.Errorf("bulk answer %q: %w", truncate(string(body), 200), err)
	}
	return r, nil
}

func (r swiftBulkResult) err(op string) error {
	if len(r.Errors) == 0 && (r.Status == "" || strings.HasPrefix(r.Status, "2")) {
		return nil
	}
	return fmt.Errorf("swift %s: %s %v", op, r.Status, r.Errors)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// List returns the artifacts of container under prefix (every depth),
// relative to the container. Every page is read.
func (d *SwiftDriver) List(ctx context.Context, container, prefix string) ([]string, error) {
	tenantID, err := requireTenant(ctx, d.name, "List", "", d.logger)
	if err != nil {
		return nil, err
	}
	base := tenantKey(tenantID, container, "")
	var out []string
	err = d.listNames(ctx, base+prefix, swiftMetaTimeout, func(name string, _ int64) error {
		out = append(out, strings.TrimPrefix(name, base))
		return nil
	})
	return out, err
}

// listNames hands every stored name under prefix to fn, page by page (each
// page bounded by pageTimeout). A name outside the prefix aborts.
func (d *SwiftDriver) listNames(ctx context.Context, prefix string, pageTimeout time.Duration, fn func(name string, size int64) error) error {
	marker := ""
	for {
		q := url.Values{"format": {"json"}, "prefix": {prefix}, "limit": {strconv.Itoa(d.listPageSize)}}
		if marker != "" {
			q.Set("marker", marker)
		}
		pctx, cancel := context.WithTimeout(ctx, pageTimeout)
		status, _, body, err := d.metaWithin(pctx, swiftRequest{method: http.MethodGet, query: q})
		cancel()
		if err != nil {
			return fmt.Errorf("%s list %s: %w", d.name, prefix, err)
		}
		if status == http.StatusNoContent {
			return nil
		}
		if status != http.StatusOK {
			return &SwiftError{Op: "list " + prefix, Status: status, Body: truncate(string(body), 200)}
		}
		var page []struct {
			Name  string `json:"name"`
			Bytes int64  `json:"bytes"`
		}
		if err := json.Unmarshal(body, &page); err != nil {
			return fmt.Errorf("%s list %s: decode: %w", d.name, prefix, err)
		}
		for _, o := range page {
			if !strings.HasPrefix(o.Name, prefix) {
				return fmt.Errorf("%s list %s: the backend returned %q, outside the prefix", d.name, prefix, o.Name)
			}
			if err := fn(o.Name, o.Bytes); err != nil {
				return err
			}
		}
		if len(page) < d.listPageSize {
			return nil
		}
		marker = page[len(page)-1].Name
	}
}

// WalkTenant (engine.TenantWalker): everything the tenant holds, large-object
// pieces included (container `_segments`), each with a Remove bound to the
// listed name.
func (d *SwiftDriver) WalkTenant(ctx context.Context, tenantID string, fn func(engine.TenantObject) error) error {
	if err := checkWalkTenant(tenantID); err != nil {
		return err
	}
	prefix := "t-" + tenantID + "/"
	return d.listNames(ctx, prefix, swiftWalkPageTimeout, func(name string, _ int64) error {
		rest := strings.TrimPrefix(name, prefix)
		container, artifact, ok := strings.Cut(rest, "/")
		if !ok {
			container, artifact = "", rest
		}
		stored := name
		return fn(engine.TenantObject{Container: container, Artifact: artifact, Remove: func(ctx context.Context) error {
			_, err := d.deleteName(ctx, stored)
			return err
		}})
	})
}

// ListingLag (engine.ListingLagger): how long the container's listing may
// lag a write — what a walk that finds nothing waits for before it proves
// anything (the account-erasure sweep, api/erasure_settle.go).
func (d *SwiftDriver) ListingLag() time.Duration {
	switch {
	case d.cfg.ListingSettle < 0:
		return 0
	case d.cfg.ListingSettle == 0:
		return swiftDefaultListingSettle
	}
	return d.cfg.ListingSettle
}

// HealthCheck asks for a token (when none is current) and HEADs the
// container: a wrong password or a container the account cannot reach
// fails it.
func (d *SwiftDriver) HealthCheck(ctx context.Context) error {
	status, _, _, err := d.meta(ctx, swiftRequest{method: http.MethodHead})
	if err != nil {
		return err
	}
	if status != http.StatusNoContent && status != http.StatusOK {
		return &SwiftError{Op: "HEAD container " + d.cfg.Container, Status: status}
	}
	return nil
}

// VendorUsage reads the container HEAD (engine.VendorUsageReporter): its
// X-Container-Bytes-Used, and X-Container-Meta-Quota-Bytes when a quota is
// set (container_quotas — Blomp's 40 GiB free account). The account HEAD
// is not asked: Blomp answers it 403. Both are Swift's own counters, which
// lag writes and deletes by minutes (deleted bytes stayed counted after a
// bulk delete on 2026-10-21), and Swift lets a write in flight take the
// container past its quota (41.7 GiB accepted of 40 GiB) — the daily
// backend_capacity job reads it, never a write path.
func (d *SwiftDriver) VendorUsage(ctx context.Context) (engine.VendorUsage, error) {
	status, h, _, err := d.meta(ctx, swiftRequest{method: http.MethodHead})
	if err != nil {
		return engine.VendorUsage{}, err
	}
	if status != http.StatusNoContent && status != http.StatusOK {
		return engine.VendorUsage{}, &SwiftError{Op: "HEAD container " + d.cfg.Container, Status: status}
	}
	used, err := strconv.ParseInt(h.Get("X-Container-Bytes-Used"), 10, 64)
	if err != nil || used < 0 {
		return engine.VendorUsage{}, fmt.Errorf("%s: container HEAD gave no usable X-Container-Bytes-Used (%q)",
			d.name, h.Get("X-Container-Bytes-Used"))
	}
	u := engine.VendorUsage{UsedBytes: used}
	if q := h.Get("X-Container-Meta-Quota-Bytes"); q != "" {
		limit, err := strconv.ParseInt(q, 10, 64)
		if err != nil || limit < 0 {
			return engine.VendorUsage{}, fmt.Errorf("%s: container HEAD gave an unusable X-Container-Meta-Quota-Bytes (%q)", d.name, q)
		}
		u.LimitBytes = limit
	}
	return u, nil
}

// ---- writes ---------------------------------------------------------------

// Put stores an object. A known size up to SegmentSize, or an unknown one
// that ends within swiftPeekBytes, is one PUT; anything larger is a static
// large object (putLarge). A body shorter than its declared size fails.
func (d *SwiftDriver) Put(ctx context.Context, container, artifact string, data io.Reader, opts ...engine.PutOption) error {
	name, err := d.objectName(ctx, "Put", container, artifact)
	if err != nil {
		return err
	}
	o := engine.ApplyPutOptions(opts...)
	size := o.ContentLength
	if size > 0 && size > d.cfg.SegmentSize {
		return d.putLarge(ctx, name, o.ContentType, data, size)
	}
	if size > 0 && size > swiftPeekBytes {
		return d.putOne(ctx, name, o.ContentType, swiftRequest{body: data, size: size})
	}
	// Small or unknown: hold up to the peek (a retryable PUT).
	limit := int64(swiftPeekBytes)
	if size > 0 {
		limit = size
	}
	buf, err := io.ReadAll(io.LimitReader(data, limit+1))
	if err != nil {
		return fmt.Errorf("%s put %s: read body: %w", d.name, name, err)
	}
	if size > 0 && int64(len(buf)) < size {
		return fmt.Errorf("%s put %s: body is %d bytes, %d declared: %w", d.name, name, len(buf), size, io.ErrUnexpectedEOF)
	}
	if int64(len(buf)) > limit {
		if size > 0 {
			return fmt.Errorf("%w: %s put %s: body is longer than its declared %d bytes", engine.ErrInvalidInput, d.name, name, size)
		}
		return d.putLarge(ctx, name, o.ContentType, io.MultiReader(bytes.NewReader(buf), data), -1)
	}
	if int64(len(buf)) > d.cfg.SegmentSize {
		return d.putLarge(ctx, name, o.ContentType, bytes.NewReader(buf), int64(len(buf)))
	}
	if buf == nil {
		buf = []byte{}
	}
	return d.putOne(ctx, name, o.ContentType, swiftRequest{bodyBytes: buf})
}

// putOne is one object PUT, its ETag checked against the MD5 of what was
// sent.
func (d *SwiftDriver) putOne(ctx context.Context, name, contentType string, req swiftRequest) error {
	req.method, req.name = http.MethodPut, name
	req.header = http.Header{}
	if contentType != "" {
		req.header.Set("Content-Type", contentType)
	}
	h := md5.New() // #nosec G401 -- see the import
	cr := &countingReader{Reader: req.body}
	if req.body != nil {
		req.body = io.TeeReader(cr, h)
	} else {
		_, _ = h.Write(req.bodyBytes)
	}
	resp, err := d.do(ctx, req)
	if err != nil {
		if req.size > 0 && cr.n < req.size {
			return fmt.Errorf("%s put %s: body ended after %d bytes, %d declared: %w (%w)", d.name, name, cr.n, req.size, io.ErrUnexpectedEOF, err)
		}
		return err
	}
	defer drain(resp)
	if resp.StatusCode != http.StatusCreated {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
		return &SwiftError{Op: "PUT " + name, Status: resp.StatusCode, Body: strings.TrimSpace(string(msg))}
	}
	if got, want := strings.Trim(resp.Header.Get("Etag"), `"`), hex.EncodeToString(h.Sum(nil)); got != "" && got != want {
		return fmt.Errorf("%s put %s: the backend stored MD5 %s, we sent %s", d.name, name, got, want)
	}
	return nil
}

// putLarge cuts body into pieces of SegmentSize (grown for a known size
// so the manifest stays within Swift's 1000 pieces), uploads them
// PutConcurrency at a time, then writes the manifest at name. size -1 =
// unknown. Each piece is read into memory first — under the process-wide
// PutBudget, waited for, never refused — so a 5xx on it is retried. A
// failure stops the other pieces and removes every piece that may have been
// written (one bulk delete) once they have all stopped.
func (d *SwiftDriver) putLarge(ctx context.Context, name, contentType string, body io.Reader, size int64) (err error) {
	segSize := d.cfg.SegmentSize
	if size > 0 {
		segSize = max(segSize, (size+swiftMaxManifestSegments-1)/swiftMaxManifestSegments)
		if segSize > swiftMaxObjectSize {
			return fmt.Errorf("%w: %s put %s: %d bytes is more than %d pieces of 5 GiB", engine.ErrInvalidInput, d.name, name, size, swiftMaxManifestSegments)
		}
	}
	tenantID := contextTenant(ctx)
	digest := sha256.Sum256([]byte(name))
	gen := make([]byte, 8)
	if _, err := rand.Read(gen); err != nil {
		return fmt.Errorf("%s put %s: generation: %w", d.name, name, err)
	}
	segPrefix := fmt.Sprintf("t-%s/_segments/%s/%x/", tenantID, hex.EncodeToString(digest[:16]), gen)

	pctx, stop := context.WithCancel(ctx)
	defer stop()
	g, gctx := errgroup.WithContext(pctx)
	slots := make(chan struct{}, d.cfg.PutConcurrency)
	manifest := make([]swiftSegment, swiftMaxManifestSegments)
	var written []string
	defer func() {
		if err != nil {
			stop() // the pieces still going up stop before their removal
		}
		if werr := g.Wait(); err == nil {
			err = werr
		} else if werr != nil && errors.Is(err, context.Canceled) && ctx.Err() == nil {
			err = werr // the piece that failed, not the stop it caused
		}
		if err != nil && len(written) > 0 {
			d.removeSegments(ctx, name, written)
		}
	}()

	br := bufio.NewReaderSize(body, 64<<10)
	var total int64
	n := 0
	for ; size < 0 || total < size; n++ {
		segLen := segSize
		if size >= 0 {
			segLen = min(segSize, size-total)
		}
		if size < 0 {
			if _, perr := br.Peek(1); perr == io.EOF {
				break
			} else if perr != nil {
				return fmt.Errorf("%s put %s: read body: %w", d.name, name, perr)
			}
			if n == swiftMaxManifestSegments {
				return fmt.Errorf("%w: %s put %s: a body of unknown length past %d pieces of %d bytes", engine.ErrInvalidInput, d.name, name, swiftMaxManifestSegments, segSize)
			}
		}
		select {
		case slots <- struct{}{}:
		case <-gctx.Done():
			return gctx.Err()
		}
		weight := min(segLen, d.cfg.PutBudget)
		if err := d.putBudget.Acquire(gctx, weight); err != nil {
			<-slots
			return err
		}
		buf := d.pieceBuffer(segLen)
		got, rerr := io.ReadFull(br, buf)
		if rerr != nil && !(size < 0 && errors.Is(rerr, io.ErrUnexpectedEOF)) {
			d.releasePiece(buf, weight)
			<-slots
			if errors.Is(rerr, io.EOF) || errors.Is(rerr, io.ErrUnexpectedEOF) {
				return fmt.Errorf("%s put %s: body ended after %d bytes, %d declared: %w", d.name, name, total+int64(got), size, io.ErrUnexpectedEOF)
			}
			return fmt.Errorf("%s put %s: read body: %w", d.name, name, rerr)
		}
		segName := fmt.Sprintf("%s%08d", segPrefix, n)
		written = append(written, segName)
		piece, i := buf[:got], n
		g.Go(func() error {
			defer func() { d.releasePiece(buf, weight); <-slots }()
			etag, err := d.putSegment(gctx, segName, piece)
			if err != nil {
				return err
			}
			manifest[i] = swiftSegment{Path: "/" + url.PathEscape(d.cfg.Container) + "/" + escapeSwiftName(segName),
				Etag: etag, SizeBytes: int64(len(piece))}
			return nil
		})
		total += int64(got)
		if size < 0 && int64(got) < segLen {
			n++
			break
		}
	}
	if size >= 0 {
		if _, perr := br.Peek(1); perr == nil {
			return fmt.Errorf("%w: %s put %s: body is longer than its declared %d bytes", engine.ErrInvalidInput, d.name, name, size)
		}
	}
	if err := g.Wait(); err != nil {
		return err
	}
	if n == 0 {
		return d.putOne(ctx, name, contentType, swiftRequest{bodyBytes: []byte{}})
	}
	mbody, err := json.Marshal(manifest[:n])
	if err != nil {
		return fmt.Errorf("%s put %s: manifest: %w", d.name, name, err)
	}
	h := http.Header{}
	if contentType != "" {
		h.Set("Content-Type", contentType)
	}
	resp, err := d.do(ctx, swiftRequest{method: http.MethodPut, name: name, query: url.Values{"multipart-manifest": {"put"}},
		header: h, bodyBytes: mbody})
	if err != nil {
		return err
	}
	defer drain(resp)
	if resp.StatusCode != http.StatusCreated {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
		return &SwiftError{Op: "PUT (manifest) " + name, Status: resp.StatusCode, Body: strings.TrimSpace(string(msg))}
	}
	return nil
}

type swiftSegment struct {
	Path      string `json:"path"`
	Etag      string `json:"etag"`
	SizeBytes int64  `json:"size_bytes"`
}

// pieceBuffer is a buffer of n bytes, recycled when n is SegmentSize.
func (d *SwiftDriver) pieceBuffer(n int64) []byte {
	if n == d.cfg.SegmentSize {
		if b, ok := d.pieceBuf.Get().(*[]byte); ok {
			return *b
		}
	}
	return make([]byte, n)
}

// releasePiece returns a piece's budget and its buffer.
func (d *SwiftDriver) releasePiece(buf []byte, weight int64) {
	if int64(cap(buf)) == d.cfg.SegmentSize {
		b := buf[:cap(buf)]
		d.pieceBuf.Put(&b)
	}
	d.putBudget.Release(weight)
}

// putSegment uploads one piece from memory (replayable: a 5xx is retried)
// and checks the ETag Swift stored against its MD5.
func (d *SwiftDriver) putSegment(ctx context.Context, name string, data []byte) (string, error) {
	sum := md5.Sum(data) // #nosec G401 -- see the import
	want := hex.EncodeToString(sum[:])
	if data == nil {
		data = []byte{}
	}
	resp, err := d.do(ctx, swiftRequest{method: http.MethodPut, name: name, bodyBytes: data, attempts: swiftSegmentAttempts})
	if err != nil {
		return "", err
	}
	defer drain(resp)
	if resp.StatusCode != http.StatusCreated {
		return "", &SwiftError{Op: "PUT (segment) " + name, Status: resp.StatusCode}
	}
	if got := strings.Trim(resp.Header.Get("Etag"), `"`); got != "" && got != want {
		return "", fmt.Errorf("%s put %s: the backend stored MD5 %s for %d bytes, we sent %s", d.name, name, got, len(data), want)
	}
	return want, nil
}

// removeSegments bulk-deletes the pieces of a failed upload or a deleted
// large object (one request for all of them), detached from a caller that
// may have gone.
func (d *SwiftDriver) removeSegments(ctx context.Context, name string, segs []string) {
	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx),
		min(swiftCleanupTimeout+time.Duration(len(segs))*swiftCleanupPerPiece, swiftCleanupMax))
	defer cancel()
	var b strings.Builder
	for _, s := range segs {
		b.WriteString("/" + url.PathEscape(d.cfg.Container) + "/" + escapeSwiftName(s) + "\n")
	}
	h := http.Header{}
	h.Set("Content-Type", "text/plain")
	h.Set("Accept", "application/json")
	status, _, body, err := d.metaWithin(cctx, swiftRequest{method: http.MethodPost, account: true,
		query: url.Values{"bulk-delete": {""}}, header: h, bodyBytes: []byte(b.String())})
	if err == nil && status != http.StatusOK {
		err = &SwiftError{Op: "bulk delete", Status: status}
	}
	if err == nil {
		var res swiftBulkResult
		if res, err = parseSwiftBulk(body); err == nil {
			err = res.err("bulk delete")
		}
	}
	if err != nil {
		d.logger.Warn("swift: large-object pieces were not all removed — the erasure walk lists them under _segments",
			zap.String("backend", d.name), zap.String("object", name), zap.Int("pieces", len(segs)), zap.Error(err))
	}
}

var (
	_ engine.Driver               = (*SwiftDriver)(nil)
	_ engine.RangeGetter          = (*SwiftDriver)(nil)
	_ engine.VersionedRangeGetter = (*SwiftDriver)(nil)
	_ engine.TenantWalker         = (*SwiftDriver)(nil)
	_ engine.KeyAddresser         = (*SwiftDriver)(nil)
)
