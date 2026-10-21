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
	"hash"
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
}

const (
	// BlompDefaultAuthURL is the Keystone v2 endpoint Blomp's dashboard names.
	BlompDefaultAuthURL = "https://authenticate.blomp.com/v2.0"
	// BlompDefaultTenant is the tenant every Blomp account authenticates to.
	BlompDefaultTenant = "storage"
	// swiftMaxObjectSize is Swift's max_file_size (5 GiB + 2 on Blomp's /info).
	swiftMaxObjectSize = 5 << 30
	// swiftDefaultSegmentSize: whole up to 1 GiB, SLO above.
	swiftDefaultSegmentSize = 1 << 30
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
	// swiftCleanupTimeout bounds the bulk delete of a failed upload's pieces.
	swiftCleanupTimeout = 2 * time.Minute
	swiftAttempts       = 3
	swiftListPage       = 1000
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
		Tenant: BlompDefaultTenant, SegmentSize: swiftDefaultSegmentSize}
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
	for _, m := range []string{http.MethodGet, http.MethodPut, http.MethodHead, http.MethodDelete, http.MethodPost} {
		for _, o := range []string{"ok", "client_error", "server_error", "unauthorized", "transport"} {
			swiftRequests.WithLabelValues(name, m, o)
		}
	}
	InitTenantlessSeries(name)
	return &SwiftDriver{name: name, cfg: cfg, logger: logger, now: time.Now, listPageSize: swiftListPage,
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
}

func (r swiftRequest) replayable() bool { return r.body == nil }

// do sends req with the session's token: once more after a 401 (a fresh
// token), and up to swiftAttempts in all after a 5xx or a transport error
// when the body can be sent again. The caller closes the body.
func (d *SwiftDriver) do(ctx context.Context, req swiftRequest) (*http.Response, error) {
	force := false
	reauthed := false
	var lastErr error
	for attempt := 1; attempt <= swiftAttempts; attempt++ {
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
		if attempt < swiftAttempts {
			swiftRetries.WithLabelValues(d.name, req.method).Inc()
			if err := sleepCtx(ctx, swiftBackoff(attempt)); err != nil {
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
	if offset < 0 || length <= 0 {
		return nil, fmt.Errorf("%w: range offset %d length %d", engine.ErrInvalidInput, offset, length)
	}
	name, err := d.objectName(ctx, "GetRange", container, artifact)
	if err != nil {
		return nil, err
	}
	h := http.Header{}
	h.Set("Range", fmt.Sprintf("bytes=%d-%d", offset, offset+length-1))
	resp, err := d.do(ctx, swiftRequest{method: http.MethodGet, name: name, header: h})
	if err != nil {
		return nil, err
	}
	switch resp.StatusCode {
	case http.StatusPartialContent:
		return resp.Body, nil
	case http.StatusOK:
		if _, err := io.CopyN(io.Discard, resp.Body, offset); err != nil {
			_ = resp.Body.Close()
			return nil, fmt.Errorf("%s GetRange %s: skip to %d: %w", d.name, name, offset, err)
		}
		return struct {
			io.Reader
			io.Closer
		}{io.LimitReader(resp.Body, length), resp.Body}, nil
	case http.StatusNotFound:
		drain(resp)
		return nil, engine.ErrNotFound(container, artifact)
	}
	drain(resp)
	return nil, &SwiftError{Op: "GET (range) " + name, Status: resp.StatusCode}
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

// deleteName deletes one stored name with ONE write: a HEAD tells a large
// object (deleted with `?multipart-manifest=delete`, which takes its pieces)
// from a plain one (a plain DELETE) — Swift answers 200 with "Not an SLO
// manifest" in the body to the first on a plain object, so asking it
// blindly would cost a second write. found is false for a miss.
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
	h := http.Header{}
	h.Set("Accept", "application/json")
	status, _, body, err := d.meta(ctx, swiftRequest{method: http.MethodDelete, name: name,
		query: url.Values{"multipart-manifest": {"delete"}}, header: h})
	if err != nil {
		return false, err
	}
	if status != http.StatusOK {
		return false, &SwiftError{Op: "DELETE (large object) " + name, Status: status, Body: string(body)}
	}
	res, err := parseSwiftBulk(body)
	if err != nil {
		return false, fmt.Errorf("%s delete %s: %w", d.name, name, err)
	}
	if err := res.err("delete " + name); err != nil {
		return false, err
	}
	return res.Deleted > 0, nil
}

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

// putLarge streams body as pieces of SegmentSize into the same container,
// then writes the manifest at name. size -1 = unknown. A failure removes
// the pieces already written (one bulk delete).
func (d *SwiftDriver) putLarge(ctx context.Context, name, contentType string, body io.Reader, size int64) (err error) {
	tenantID := contextTenant(ctx)
	digest := sha256.Sum256([]byte(name))
	gen := make([]byte, 8)
	if _, err := rand.Read(gen); err != nil {
		return fmt.Errorf("%s put %s: generation: %w", d.name, name, err)
	}
	segPrefix := fmt.Sprintf("t-%s/_segments/%s/%x/", tenantID, hex.EncodeToString(digest[:16]), gen)
	var written []string
	defer func() {
		if err != nil && len(written) > 0 {
			d.removeSegments(ctx, name, written)
		}
	}()

	br := bufio.NewReaderSize(body, 64<<10)
	var manifest []swiftSegment
	var total int64
	for n := 0; ; n++ {
		if size >= 0 && total == size {
			break
		}
		if _, perr := br.Peek(1); perr == io.EOF {
			if size >= 0 {
				return fmt.Errorf("%s put %s: body is %d bytes, %d declared: %w", d.name, name, total, size, io.ErrUnexpectedEOF)
			}
			break
		} else if perr != nil {
			return fmt.Errorf("%s put %s: read body: %w", d.name, name, perr)
		}
		segLen := d.cfg.SegmentSize
		if size >= 0 && size-total < segLen {
			segLen = size - total
		}
		segName := fmt.Sprintf("%s%08d", segPrefix, n)
		h := md5.New() // #nosec G401 -- see the import
		cr := &countingReader{Reader: io.LimitReader(br, segLen)}
		req := swiftRequest{method: http.MethodPut, name: segName, body: io.TeeReader(cr, h), size: segLen}
		if size < 0 {
			req.size = -1 // chunked: the piece ends where the body or the segment does
		}
		written = append(written, segName)
		if err := d.putSegment(ctx, req, h, cr); err != nil {
			if size >= 0 && cr.n < segLen {
				return fmt.Errorf("%s put %s: body ended after %d bytes, %d declared: %w (%w)", d.name, name, total+cr.n, size, io.ErrUnexpectedEOF, err)
			}
			return err
		}
		if size >= 0 && cr.n != segLen {
			return fmt.Errorf("%s put %s: body ended after %d bytes, %d declared: %w", d.name, name, total+cr.n, size, io.ErrUnexpectedEOF)
		}
		manifest = append(manifest, swiftSegment{Path: "/" + url.PathEscape(d.cfg.Container) + "/" + escapeSwiftName(segName),
			Etag: hex.EncodeToString(h.Sum(nil)), SizeBytes: cr.n})
		total += cr.n
	}
	if size >= 0 {
		if _, perr := br.Peek(1); perr == nil {
			return fmt.Errorf("%w: %s put %s: body is longer than its declared %d bytes", engine.ErrInvalidInput, d.name, name, size)
		}
	}
	if len(manifest) == 0 {
		return d.putOne(ctx, name, contentType, swiftRequest{bodyBytes: []byte{}})
	}
	mbody, err := json.Marshal(manifest)
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

func (d *SwiftDriver) putSegment(ctx context.Context, req swiftRequest, h hash.Hash, cr *countingReader) error {
	resp, err := d.do(ctx, req)
	if err != nil {
		return err
	}
	defer drain(resp)
	if resp.StatusCode != http.StatusCreated {
		return &SwiftError{Op: "PUT (segment) " + req.name, Status: resp.StatusCode}
	}
	if got, want := strings.Trim(resp.Header.Get("Etag"), `"`), hex.EncodeToString(h.Sum(nil)); got != "" && got != want {
		return fmt.Errorf("%s put %s: the backend stored MD5 %s for %d bytes, we sent %s", d.name, req.name, got, cr.n, want)
	}
	return nil
}

// removeSegments bulk-deletes a failed upload's pieces (one write for all
// of them), detached from a caller that may have gone.
func (d *SwiftDriver) removeSegments(ctx context.Context, name string, segs []string) {
	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), swiftCleanupTimeout)
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
		d.logger.Warn("swift: a failed upload's pieces were not all removed — the erasure walk lists them under _segments",
			zap.String("backend", d.name), zap.String("object", name), zap.Int("pieces", len(segs)), zap.Error(err))
	}
}

var (
	_ engine.Driver       = (*SwiftDriver)(nil)
	_ engine.RangeGetter  = (*SwiftDriver)(nil)
	_ engine.TenantWalker = (*SwiftDriver)(nil)
	_ engine.KeyAddresser = (*SwiftDriver)(nil)
)
