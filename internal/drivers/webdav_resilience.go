package drivers

import (
	"context"
	"errors"
	"fmt"
	"io"
	mrand "math/rand/v2"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"go.uber.org/zap"

	"github.com/FairForge/vaultaire/internal/engine"
)

// Bounding a WebDAV server that stops talking (live bench against Sync's
// bridge, 2026-10-06, SLC): every op is a 0.5–1 s round trip, and the bridge
//
//   - answered some GETs and DELETEs 500 at 32 concurrent requests (once at
//     8 on DELETE) — transient;
//   - with 4 concurrent 256 MiB PUTs stopped reading one body after 62 KB
//     and never answered, while serving everything else: the client sat in
//     io.Copy writing that body for 45+ minutes.
//
// So every request to the server goes through send:
//
//   - a slot of a per-driver semaphore (WithWebDAVMaxConcurrency, default 8,
//     SYNC_WEBDAV_MAX_CONCURRENCY), waited for under the caller's context. A
//     GET gives its slot back when the answer's headers arrive — the body
//     streams to a caller who may be copying it into this same driver, and
//     a held slot would let 8 such copies deadlock the driver;
//   - an idle watchdog (WithWebDAVIdleTimeout, default 60 s,
//     SYNC_WEBDAV_IDLE_TIMEOUT): a PUT whose body the server consumes no
//     byte of for that long, a GET with no answer for that long, and a GET
//     body that yields no byte for that long while the caller is reading
//     are cancelled with ErrWebDAVStalled. The caller's own pace is never a
//     stall: the upload timer runs only while the transport holds bytes it
//     has not handed to the server, the download timer only inside Read;
//   - an overall deadline for a PUT of known length (WithWebDAVPutTimeout,
//     default 2 min per started 64 MiB — the fixed-bucket putDeadline
//     scaling, ≈ 0.5 MB/s floor — capped at 6 h): the bridge may answer
//     only after its own upload to Sync, so the wait after the last body
//     byte is bounded by this, not by the idle timer;
//   - retries (WithWebDAVRetries, default 3 attempts, jittered exponential
//     backoff from 250 ms) of a transient failure — 500/502/503/504, 423
//     Locked, a transport error (reset, EOF, refused), a stall or a timeout
//     — for PROPFIND, MKCOL, DELETE and a GET before its body reached the
//     caller. A PUT is retried only when its body can be sent again: a
//     seekable body is rewound, a non-seekable one of known length ≤ 8 MiB
//     is held in memory for it, anything larger is never buffered — a
//     transient failure after its bytes were consumed is returned with
//     engine.ErrNoFailover. A 2xx is never retried; a caller whose context
//     is done is never retried.
//
// Counted in vaultaire_webdav_requests_total{backend,method,outcome} (one
// per attempt), vaultaire_webdav_stalls_total{backend,direction} and
// vaultaire_webdav_retries_total{backend,method}, every series at 0 from
// the constructor on.

const (
	// WebDAVDefaultIdleTimeout is how long a transfer may make no progress.
	WebDAVDefaultIdleTimeout = 60 * time.Second
	// WebDAVDefaultPutTimeout is the PUT deadline per started 64 MiB.
	WebDAVDefaultPutTimeout = 2 * time.Minute
	// WebDAVDefaultMaxConcurrency is the cap of requests in flight.
	WebDAVDefaultMaxConcurrency = 8
	// WebDAVDefaultAttempts is how many times a transient failure is tried.
	WebDAVDefaultAttempts = 3

	webdavDefaultBackoff = 250 * time.Millisecond
	// webdavPutDeadlineCap bounds the scaled PUT deadline.
	webdavPutDeadlineCap = 6 * time.Hour
	// webdavRetryBufferMax is the largest non-seekable PUT body held in
	// memory so that it can be sent again.
	webdavRetryBufferMax = 8 << 20
	// webdavMaxConcurrencyLimit bounds SYNC_WEBDAV_MAX_CONCURRENCY.
	webdavMaxConcurrencyLimit = 256
)

// ErrWebDAVStalled is a transfer the server stopped making progress on. It
// wraps engine.ErrTimeout.
var ErrWebDAVStalled = fmt.Errorf("%w: the WebDAV server made no progress", engine.ErrTimeout)

// errAttemptOver ends a body read that the transport makes after its
// request has been given up (a retry is about to rewind the source).
var errAttemptOver = errors.New("webdav: request attempt is over")

// webdavSettings are a driver's limits (WebDAVOption).
type webdavSettings struct {
	idleTimeout    time.Duration // 0 = no idle watchdog
	putTimeout     time.Duration // per 64 MiB; 0 = no PUT deadline
	maxConcurrency int
	attempts       int
	backoff        time.Duration
	bridge         string // the metrics' bridge label ("0" for a single server)
}

func defaultWebDAVSettings() webdavSettings {
	return webdavSettings{
		idleTimeout:    WebDAVDefaultIdleTimeout,
		putTimeout:     WebDAVDefaultPutTimeout,
		maxConcurrency: WebDAVDefaultMaxConcurrency,
		attempts:       WebDAVDefaultAttempts,
		backoff:        webdavDefaultBackoff,
		bridge:         "0",
	}
}

// WebDAVOption tunes a WebDAV driver's limits.
type WebDAVOption func(*webdavSettings)

// WithWebDAVIdleTimeout sets how long a transfer may make no progress
// (0 disables the watchdog; negative keeps the default).
func WithWebDAVIdleTimeout(d time.Duration) WebDAVOption {
	return func(s *webdavSettings) {
		if d >= 0 {
			s.idleTimeout = d
		}
	}
}

// WithWebDAVPutTimeout sets the PUT deadline per started 64 MiB of a body
// of known length (0 disables it; negative keeps the default).
func WithWebDAVPutTimeout(d time.Duration) WebDAVOption {
	return func(s *webdavSettings) {
		if d >= 0 {
			s.putTimeout = d
		}
	}
}

// WithWebDAVMaxConcurrency caps the requests in flight to the server (n < 1
// keeps the default).
func WithWebDAVMaxConcurrency(n int) WebDAVOption {
	return func(s *webdavSettings) {
		if n >= 1 {
			s.maxConcurrency = n
		}
	}
}

// WithWebDAVRetries sets how many attempts a transient failure gets (1 = no
// retry; < 1 keeps the default) and the first backoff (doubled per attempt,
// jittered; negative keeps the default).
func WithWebDAVRetries(attempts int, backoff time.Duration) WebDAVOption {
	return func(s *webdavSettings) {
		if attempts >= 1 {
			s.attempts = attempts
		}
		if backoff >= 0 {
			s.backoff = backoff
		}
	}
}

// Options are the limits a WebDAVConfig carries (zero fields = defaults).
func (c WebDAVConfig) Options() []WebDAVOption {
	var out []WebDAVOption
	if c.MaxConcurrency > 0 {
		out = append(out, WithWebDAVMaxConcurrency(c.MaxConcurrency))
	}
	if c.IdleTimeout > 0 {
		out = append(out, WithWebDAVIdleTimeout(c.IdleTimeout))
	}
	return out
}

// parseWebDAVLimits reads SYNC_WEBDAV_MAX_CONCURRENCY (1..256, per bridge),
// SYNC_WEBDAV_LARGE_CONCURRENCY (1..256, per bridge and direction) and
// SYNC_WEBDAV_IDLE_TIMEOUT (a duration 1s..1h, or 0/off). A rejected value
// is a warning and the default is kept (the R13-19 rule).
func parseWebDAVLimits(c *WebDAVConfig, getenv func(string) string) {
	if v := strings.TrimSpace(getenv("SYNC_WEBDAV_MAX_CONCURRENCY")); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > webdavMaxConcurrencyLimit {
			c.Warnings = append(c.Warnings, fmt.Sprintf("invalid SYNC_WEBDAV_MAX_CONCURRENCY %q (need 1..%d), keeping %d",
				v, webdavMaxConcurrencyLimit, WebDAVDefaultMaxConcurrency))
		} else {
			c.MaxConcurrency = n
		}
	}
	if v := strings.TrimSpace(getenv("SYNC_WEBDAV_LARGE_CONCURRENCY")); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > webdavMaxConcurrencyLimit {
			c.Warnings = append(c.Warnings, fmt.Sprintf("invalid SYNC_WEBDAV_LARGE_CONCURRENCY %q (need 1..%d), keeping %d",
				v, webdavMaxConcurrencyLimit, WebDAVDefaultLargeConcurrency))
		} else {
			c.LargeConcurrency = n
		}
	}
	parseWebDAVStripes(c, getenv)
	if v := strings.TrimSpace(getenv("SYNC_WEBDAV_IDLE_TIMEOUT")); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d < time.Second || d > time.Hour {
			c.Warnings = append(c.Warnings, fmt.Sprintf("invalid SYNC_WEBDAV_IDLE_TIMEOUT %q (need a duration 1s..1h such as 60s), keeping %s",
				v, WebDAVDefaultIdleTimeout))
		} else {
			c.IdleTimeout = d
		}
	}
}

// parseWebDAVStripes reads SYNC_WEBDAV_STRIPE_MIN (a size such as 512MiB,
// or 0/off = never stripe), SYNC_WEBDAV_STRIPE_PIECE (16MiB..4GiB) and
// SYNC_WEBDAV_STAGING_DIR (an absolute path). A rejected value is a warning
// and the default is kept; a minimum below the piece size becomes the piece
// size.
func parseWebDAVStripes(c *WebDAVConfig, getenv func(string) string) {
	if v := strings.TrimSpace(getenv("SYNC_WEBDAV_STRIPE_PIECE")); v != "" {
		n, err := parseByteSize(v)
		if err != nil || n < webdavStripePieceMin || n > webdavStripePieceMax {
			c.Warnings = append(c.Warnings, fmt.Sprintf("invalid SYNC_WEBDAV_STRIPE_PIECE %q (need 16MiB..4GiB), keeping %d", v, WebDAVDefaultStripePiece))
		} else {
			c.StripePiece = n
		}
	}
	if v := strings.TrimSpace(getenv("SYNC_WEBDAV_STRIPE_MIN")); v != "" {
		if strings.EqualFold(v, "off") || v == "0" {
			c.StripeMin = -1
		} else if n, err := parseByteSize(v); err != nil || n <= 0 {
			c.Warnings = append(c.Warnings, fmt.Sprintf("invalid SYNC_WEBDAV_STRIPE_MIN %q (need a size such as 512MiB, or off), keeping %d", v, WebDAVDefaultStripeMin))
		} else {
			piece := c.StripePiece
			if piece <= 0 {
				piece = WebDAVDefaultStripePiece
			}
			if n < piece {
				c.Warnings = append(c.Warnings, fmt.Sprintf("SYNC_WEBDAV_STRIPE_MIN %q is below the piece size, using %d", v, piece))
				n = piece
			}
			c.StripeMin = n
		}
	}
	if v := strings.TrimSpace(getenv("SYNC_WEBDAV_STAGING_DIR")); v != "" {
		if !filepath.IsAbs(v) {
			c.Warnings = append(c.Warnings, fmt.Sprintf("invalid SYNC_WEBDAV_STAGING_DIR %q (need an absolute path), keeping %s", v, defaultStagingDir()))
		} else {
			c.StagingDir = filepath.Clean(v)
		}
	}
}

// parseByteSize reads a byte count with an optional binary suffix (KiB,
// MiB, GiB, TiB; K, M, G, T are the same).
func parseByteSize(v string) (int64, error) {
	s := strings.TrimSpace(v)
	mult := int64(1)
	for _, u := range []struct {
		suf string
		m   int64
	}{{"TiB", 1 << 40}, {"GiB", 1 << 30}, {"MiB", 1 << 20}, {"KiB", 1 << 10}, {"T", 1 << 40}, {"G", 1 << 30}, {"M", 1 << 20}, {"K", 1 << 10}} {
		if strings.HasSuffix(s, u.suf) {
			s, mult = strings.TrimSpace(strings.TrimSuffix(s, u.suf)), u.m
			break
		}
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n < 0 || n > (1<<62)/mult {
		return 0, fmt.Errorf("not a size: %q", v)
	}
	return n * mult, nil
}

// webdavPutDeadline is the deadline of one PUT of size bytes: base per
// started 64 MiB (putDeadline, the fixed-bucket driver's scaling), capped.
// 0 = none (unknown size, or disabled).
func webdavPutDeadline(base time.Duration, size int64) time.Duration {
	if base <= 0 || size <= 0 {
		return 0
	}
	if size/putDeadlineUnit >= int64(webdavPutDeadlineCap/base) {
		return webdavPutDeadlineCap
	}
	return min(putDeadline(base, size), webdavPutDeadlineCap)
}

// --- metrics ----------------------------------------------------------------

var (
	webdavRequests = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "vaultaire_webdav_requests_total",
		Help: "Requests (attempts) a WebDAV driver sent, by backend, method and outcome (ok, http_4xx, locked, http_5xx, transport_error, stall, timeout, canceled).",
	}, []string{"backend", "bridge", "method", "outcome"})
	webdavStalls = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "vaultaire_webdav_stalls_total",
		Help: "WebDAV transfers cancelled because the server made no progress for the idle timeout, by backend and direction (upload, download).",
	}, []string{"backend", "bridge", "direction"})
	webdavRetries = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "vaultaire_webdav_retries_total",
		Help: "WebDAV requests sent again after a transient failure (5xx, 423, transport error, stall, timeout), by backend and method.",
	}, []string{"backend", "bridge", "method"})
)

var (
	webdavMethods  = []string{http.MethodGet, http.MethodPut, "PROPFIND", "MKCOL", http.MethodDelete}
	webdavOutcomes = []string{"ok", "http_4xx", "locked", "http_5xx", "transport_error", "stall", "timeout", "canceled"}
)

// InitWebDAVSeries creates a backend bridge's WebDAV series at 0, so the
// first stall or retry is an increase a rule can see. bridge is the index
// of the bridge ("0" for a single-server driver).
func InitWebDAVSeries(backend, bridge string) {
	for _, m := range webdavMethods {
		webdavRetries.WithLabelValues(backend, bridge, m)
		for _, o := range webdavOutcomes {
			webdavRequests.WithLabelValues(backend, bridge, m, o)
		}
	}
	for _, d := range []string{"upload", "download"} {
		webdavStalls.WithLabelValues(backend, bridge, d)
	}
}

// WebDAVStats are one driver's own counts (the bench reports them).
type WebDAVStats struct {
	UploadStalls, DownloadStalls, Retries int64
}

type webdavCounters struct {
	uploadStalls, downloadStalls, retries atomic.Int64
}

// Stats returns the driver's stall and retry counts since construction.
func (d *WebDAVDriver) Stats() WebDAVStats {
	return WebDAVStats{
		UploadStalls:   d.counts.uploadStalls.Load(),
		DownloadStalls: d.counts.downloadStalls.Load(),
		Retries:        d.counts.retries.Load(),
	}
}

func (d *WebDAVDriver) stalled(direction string) {
	webdavStalls.WithLabelValues(d.name, d.limits.bridge, direction).Inc()
	if direction == "upload" {
		d.counts.uploadStalls.Add(1)
	} else {
		d.counts.downloadStalls.Add(1)
	}
}

func (d *WebDAVDriver) retried(method string) {
	webdavRetries.WithLabelValues(d.name, d.limits.bridge, method).Inc()
	d.counts.retries.Add(1)
}

// --- watchdog ---------------------------------------------------------------

// watchdog cancels a request when it stays armed for idle. A nil watchdog
// (idle timeout disabled) does nothing.
type watchdog struct {
	idle  time.Duration
	timer *time.Timer
	fired atomic.Bool
}

func newWatchdog(idle time.Duration, cancel context.CancelFunc) *watchdog {
	if idle <= 0 {
		return nil
	}
	w := &watchdog{idle: idle}
	w.timer = time.AfterFunc(time.Hour, func() {
		w.fired.Store(true)
		cancel()
	})
	w.timer.Stop()
	return w
}

func (w *watchdog) arm() {
	if w != nil {
		w.timer.Reset(w.idle)
	}
}

func (w *watchdog) disarm() {
	if w != nil {
		w.timer.Stop()
	}
}

func (w *watchdog) hasFired() bool { return w != nil && w.fired.Load() }

// uploadBody is one PUT attempt's view of the body: it counts the bytes the
// transport took, keeps the watchdog armed while the transport holds bytes
// the server has not asked for yet, and is fenced off once the attempt is
// given up (the transport may read on in another goroutine after RoundTrip
// returned; a retry must not share the source with it).
type uploadBody struct {
	mu     sync.Mutex
	r      io.Reader
	n      int64
	wd     *watchdog
	fenced bool
}

func (u *uploadBody) Read(p []byte) (int, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.fenced {
		return 0, errAttemptOver
	}
	u.wd.disarm()
	n, err := u.r.Read(p)
	u.n += int64(n)
	if err == nil {
		u.wd.arm()
	}
	return n, err
}

// fence ends the attempt's reads and returns the bytes it consumed.
func (u *uploadBody) fence() int64 {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.fenced = true
	u.wd.disarm()
	return u.n
}

// consumed is the bytes taken so far (for an attempt that is over).
func (u *uploadBody) consumed() int64 {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.n
}

// --- one request --------------------------------------------------------------

// davCall is one attempt in flight: its response, and what to undo when the
// caller is done with it (finish: the slot, the watchdog, the context).
type davCall struct {
	resp    *http.Response
	wd      *watchdog
	cancel  context.CancelFunc
	slot    sync.Once
	release func()
}

func (c *davCall) releaseSlot() { c.slot.Do(c.release) }

func (c *davCall) finish() {
	c.wd.disarm()
	c.releaseSlot()
	c.cancel()
}

// watchMode says which watchdog an attempt runs.
type watchMode int

const (
	watchNone     watchMode = iota
	watchUpload             // the body is an *uploadBody
	watchDownload           // the wait for headers; the body is wrapped by the caller
)

func (d *WebDAVDriver) acquire(ctx context.Context) (func(), error) {
	select {
	case d.sem <- struct{}{}:
		return func() { <-d.sem }, nil
	case <-ctx.Done():
		return nil, fmt.Errorf("waiting for one of %d request slots: %w", cap(d.sem), ctx.Err())
	}
}

func isTransientStatus(code int) bool {
	switch code {
	case http.StatusInternalServerError, http.StatusBadGateway, http.StatusServiceUnavailable,
		http.StatusGatewayTimeout, http.StatusLocked:
		return true
	}
	return false
}

func statusOutcome(code int) string {
	switch {
	case code == http.StatusLocked:
		return "locked"
	case code >= 500:
		return "http_5xx"
	case code >= 400:
		return "http_4xx"
	}
	return "ok"
}

// send makes one attempt: a slot, the request, the watchdog. On success the
// caller owns the call and must finish it once done with the response. On a
// transport failure the call is already finished; transient says whether
// the failure is one a retry may cure.
func (d *WebDAVDriver) send(ctx context.Context, method, path string, body io.Reader, length int64,
	header map[string]string, timeout time.Duration, mode watchMode) (_ *davCall, transient bool, _ error) {
	u, err := d.requestURL(path)
	if err != nil {
		return nil, false, err
	}
	release, err := d.acquire(ctx)
	if err != nil {
		return nil, false, err
	}
	var actx context.Context
	var cancel context.CancelFunc
	if timeout > 0 {
		actx, cancel = context.WithTimeout(ctx, timeout)
	} else {
		actx, cancel = context.WithCancel(ctx)
	}
	call := &davCall{cancel: cancel, release: release}
	if mode != watchNone {
		call.wd = newWatchdog(d.limits.idleTimeout, cancel)
	}
	if ub, ok := body.(*uploadBody); ok {
		ub.wd = call.wd
	}
	req, err := http.NewRequestWithContext(actx, method, u.String(), body)
	if err != nil {
		call.finish()
		return nil, false, fmt.Errorf("build %s request: %w", method, err)
	}
	if req.URL.Scheme != d.base.Scheme || req.URL.Host != d.base.Host {
		call.finish()
		return nil, false, fmt.Errorf("%w: webdav %s request to %s://%s, configured %s", engine.ErrInvalidInput, method, req.URL.Scheme, req.URL.Host, d.origin())
	}
	if length > 0 {
		req.ContentLength = length
	}
	req.SetBasicAuth(d.username, d.password)
	for k, v := range header {
		req.Header.Set(k, v)
	}

	// Upload: armed until the transport takes the first bytes (then the
	// body re-arms it); download: armed until the headers arrive.
	call.wd.arm()
	resp, err := d.client.Do(req) //nolint:bodyclose // the caller owns call.resp and closes it (drainClose / statusError / downloadBody.Close)
	call.wd.disarm()
	if err != nil {
		stalled := call.wd.hasFired()
		timedOut := errors.Is(actx.Err(), context.DeadlineExceeded)
		call.finish()
		switch {
		case ctx.Err() != nil:
			d.count(method, "canceled")
			return nil, false, fmt.Errorf("%s: %w", method, ctx.Err())
		case stalled:
			d.count(method, "stall")
			dir, what := "download", "no answer"
			if mode == watchUpload {
				dir, what = "upload", "the server read no body byte"
			}
			d.stalled(dir)
			return nil, true, fmt.Errorf("%s: %s for %s: %w (%s)", method, what, d.limits.idleTimeout, ErrWebDAVStalled, err.Error())
		case timedOut:
			d.count(method, "timeout")
			return nil, true, fmt.Errorf("%s: no answer within %s: %w (%s)", method, timeout, engine.ErrTimeout, err.Error())
		}
		d.count(method, "transport_error")
		return nil, true, fmt.Errorf("%s: %w", method, err)
	}
	call.resp = resp
	d.count(method, statusOutcome(resp.StatusCode))
	if mode == watchDownload {
		call.releaseSlot() // the body streams to the caller without a slot
	}
	return call, isTransientStatus(resp.StatusCode), nil
}

func (d *WebDAVDriver) count(method, outcome string) {
	webdavRequests.WithLabelValues(d.name, d.limits.bridge, method, outcome).Inc()
}

// backoff waits before attempt+1: backoff × 2^(attempt-1), jittered to
// between half and all of it.
func (d *WebDAVDriver) backoff(ctx context.Context, attempt int) error {
	wait := d.limits.backoff << (attempt - 1)
	if wait <= 0 {
		return ctx.Err()
	}
	wait = wait/2 + time.Duration(mrand.Int64N(int64(wait/2)+1)) // #nosec G404 -- jitter for backoff, not security
	t := time.NewTimer(wait)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// davSpec is an idempotent request (PROPFIND, MKCOL, DELETE, GET).
type davSpec struct {
	method, path string
	header       map[string]string
	body         string        // a fixed request body, sent again on a retry
	timeout      time.Duration // per attempt; 0 = none
	stream       bool          // GET: watch the header wait; the slot goes at the headers
	noRetry423   bool          // the caller handles 423 itself (MKCOL)
}

// do sends an idempotent request, retrying transient failures. The last
// answer is returned even when it is a transient status (the caller turns it
// into its error); the caller must finish the call.
func (d *WebDAVDriver) do(ctx context.Context, s davSpec) (*davCall, error) {
	mode := watchNone
	if s.stream {
		mode = watchDownload
	}
	for attempt := 1; ; attempt++ {
		var body io.Reader
		if s.body != "" {
			body = strings.NewReader(s.body)
		}
		call, transient, err := d.send(ctx, s.method, s.path, body, int64(len(s.body)), s.header, s.timeout, mode)
		if call != nil && s.noRetry423 && call.resp.StatusCode == http.StatusLocked {
			transient = false
		}
		if !transient || attempt >= d.limits.attempts || ctx.Err() != nil {
			return call, err
		}
		if call != nil {
			drainClose(call.resp)
			call.finish()
		}
		d.retried(s.method)
		d.logger.Debug("webdav request retried", zap.String("backend", d.name), zap.String("method", s.method),
			zap.Int("attempt", attempt), zap.Error(err))
		if berr := d.backoff(ctx, attempt); berr != nil {
			if err == nil {
				err = errors.New("transient answer")
			}
			return nil, fmt.Errorf("%s: %w (giving up a retry: %w)", s.method, err, berr)
		}
	}
}

// --- streamed GET bodies ------------------------------------------------------

// downloadBody is a GET body handed to the caller: the watchdog runs while
// the caller waits in Read (never between reads, which is the caller's
// pace), and Close ends the attempt.
type downloadBody struct {
	rc      io.ReadCloser
	call    *davCall
	d       *WebDAVDriver
	what    string
	counted atomic.Bool
	once    sync.Once
	release func() // a multi-bridge large-transfer slot (nil = none)
}

func (b *downloadBody) Read(p []byte) (int, error) {
	b.call.wd.arm()
	n, err := b.rc.Read(p)
	b.call.wd.disarm()
	if err != nil && !errors.Is(err, io.EOF) && b.call.wd.hasFired() {
		if b.counted.CompareAndSwap(false, true) {
			b.d.stalled("download")
		}
		return n, fmt.Errorf("%s: no byte from the server for %s: %w (%s)", b.what, b.d.limits.idleTimeout, ErrWebDAVStalled, err.Error())
	}
	return n, err
}

func (b *downloadBody) Close() error {
	var err error
	b.once.Do(func() {
		err = b.rc.Close()
		b.call.finish()
		if b.release != nil {
			b.release()
		}
	})
	return err
}
