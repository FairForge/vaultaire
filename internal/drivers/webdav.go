package drivers

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf16"

	"go.uber.org/zap"

	"github.com/FairForge/vaultaire/internal/engine"
)

// WebDAVDriver stores objects on a WebDAV server (RFC 4918) — generic, so the
// same driver serves Sync.com's local encrypted bridge (`sync-webdav`, the
// first instance, registered as "sync"), Hetzner Storage Box, Koofr, pCloud
// and Nextcloud. See webdav_README.md.
//
// Layout is the fixed-bucket shape of iDrive/R2/Geyser under a configured
// root folder: `<root>/t-<tenant>/<container>/<artifact…>%o`, the tenant taken
// from the call's context (a call without one is refused, tenant_ctx.go).
// Every key segment is a WebDAV path segment, mapped reversibly (davName) so
// that every S3 key is one Sync.com's bridge stores — the characters and
// names it refuses or silently drops are percent-encoded; "" (a trailing
// `/`), "." and ".." never reach the server as such.
//
// Every object's leaf carries WebDAVLeafMarker (`%o`) and no folder does,
// so S3 keys `a` and `a/b` coexist — on one server and across bridges that
// cannot see each other's writes yet.
type WebDAVDriver struct {
	name string
	// base is the configured server — Scheme and Host only, parsed and
	// checked once in the constructor. Every request URL is a copy of it
	// with only the path set (requestURL): scheme and host never derive
	// from a key.
	base     url.URL
	basePath string   // decoded path of the URL's own folder ("/" at the top)
	rootPath string   // decoded path of the root folder (= basePath without a root)
	baseSegs []string // decoded path segments of the URL's own path
	rootSegs []string // the configured root folder, below the URL's path
	username string
	password string
	client   *http.Client
	logger   *zap.Logger

	// known holds collection paths (escaped, no trailing slash) this process
	// has created or seen, so a PUT into a known folder costs no MKCOL.
	known sync.Map
	// mkcolLocks serialises the MKCOL of one path (path → *sync.Mutex).
	mkcolLocks sync.Map

	// limits, sem and counts bound a server that stops talking
	// (webdav_resilience.go): idle and PUT deadlines, retries, and a slot
	// per request in flight.
	limits webdavSettings
	sem    chan struct{}
	counts webdavCounters

	// folders counts files per folder against Sync's 50,000 limit
	// (webdav_folder_limit.go); folderFileLimit overrides it (tests).
	folders         folderTracker
	folderFileLimit int

	// bodyGate (optional; the multi-bridge driver's large-transfer cap) runs
	// when a GET answer's body is about to be handed out, with its
	// Content-Length (-1 = unknown); it may wait under ctx, and its release
	// runs at the body's Close.
	bodyGate func(ctx context.Context, size int64) (release func(), err error)
}

const (
	// webdavResponseHeaderTimeout bounds the wait for an answer once a
	// request is sent. Generous: a bridge (Sync's encrypts and uploads)
	// may answer a PUT only after its own upload finished.
	webdavResponseHeaderTimeout = 5 * time.Minute
	// webdavMetaTimeout bounds one PROPFIND/MKCOL/DELETE.
	webdavMetaTimeout = 60 * time.Second
	// webdavMaxMultistatus caps one PROPFIND answer we parse.
	webdavMaxMultistatus = 64 << 20
	// webdavLockedRetries / webdavLockedBackoff: a MKCOL answered 423 Locked
	// is retried this many times, waiting attempt × backoff in between.
	webdavLockedRetries = 5
	webdavLockedBackoff = 100 * time.Millisecond
)

// WebDAVConfig is one WebDAV backend's settings. Bridges are the servers it
// spreads over (the `sync` instance: one per Sync bridge process,
// webdav_multi.go); URL and Password are the first bridge's, for the
// single-server constructor. MaxConcurrency, LargeConcurrency and
// IdleTimeout are 0 for the defaults (Options turns the per-bridge ones into
// options); Warnings name env values that were rejected (the caller logs
// them).
type WebDAVConfig struct {
	URL, User, Password, Root string
	Bridges                   []WebDAVBridge
	MaxConcurrency            int
	LargeConcurrency          int
	IdleTimeout               time.Duration
	// StripeMin (0 = WebDAVDefaultStripeMin, < 0 = never), StripePiece (0 =
	// WebDAVDefaultStripePiece) and StagingDir ("" = <tmp>/vaultaire-stripes)
	// configure the multi-bridge driver's striping (webdav_stripe.go).
	StripeMin   int64
	StripePiece int64
	StagingDir  string
	Warnings    []string
}

// WebDAVBridge is one server of a backend: its URL and its own password.
type WebDAVBridge struct {
	URL, Password string
}

// Defaults of the `sync` instance: Sync.com's `sync-webdav` bridge listens on
// localhost with the user `sync` and a generated password.
const (
	SyncWebDAVDefaultURL  = "http://127.0.0.1:4918"
	SyncWebDAVDefaultUser = "sync"
	SyncWebDAVDefaultRoot = "vaultaire"
	// SyncWebDAVMaxBridges bounds SYNC_WEBDAV_URLS.
	SyncWebDAVMaxBridges = 16
)

// SyncWebDAVConfigFromEnv reads the `sync` instance's settings. The bridges
// are either one — SYNC_WEBDAV_PASSWORD (+ SYNC_WEBDAV_URL, default
// 127.0.0.1:4918) — or several: SYNC_WEBDAV_URLS and SYNC_WEBDAV_PASSWORDS,
// comma-separated, same order, 1..16, distinct URLs (the list wins over the
// single form, with a warning). Then SYNC_WEBDAV_USER, SYNC_WEBDAV_ROOT and
// the limits SYNC_WEBDAV_MAX_CONCURRENCY, SYNC_WEBDAV_LARGE_CONCURRENCY and
// SYNC_WEBDAV_IDLE_TIMEOUT (a rejected limit is a Warning and the default
// is kept). ok is false when no password is set at all (no sync driver);
// err is a bridge list that cannot be used — it never quotes a password.
func SyncWebDAVConfigFromEnv(getenv func(string) string) (_ WebDAVConfig, ok bool, _ error) {
	c := WebDAVConfig{
		URL:      strings.TrimSpace(getenv("SYNC_WEBDAV_URL")),
		User:     getenv("SYNC_WEBDAV_USER"),
		Password: getenv("SYNC_WEBDAV_PASSWORD"),
		Root:     getenv("SYNC_WEBDAV_ROOT"),
	}
	urls, passwords := strings.TrimSpace(getenv("SYNC_WEBDAV_URLS")), getenv("SYNC_WEBDAV_PASSWORDS")
	if c.Password == "" && urls == "" && passwords == "" {
		return WebDAVConfig{}, false, nil
	}
	if c.User == "" {
		c.User = SyncWebDAVDefaultUser
	}
	if c.Root == "" {
		c.Root = SyncWebDAVDefaultRoot
	}
	parseWebDAVLimits(&c, getenv)

	if urls == "" && passwords == "" {
		if c.URL == "" {
			c.URL = SyncWebDAVDefaultURL
		}
		c.Bridges = []WebDAVBridge{{URL: c.URL, Password: c.Password}}
		return c, true, nil
	}
	if urls == "" {
		return c, true, errors.New("SYNC_WEBDAV_PASSWORDS is set without SYNC_WEBDAV_URLS")
	}
	if passwords == "" {
		return c, true, errors.New("SYNC_WEBDAV_URLS is set without SYNC_WEBDAV_PASSWORDS (one password per bridge, same order)")
	}
	if c.URL != "" || c.Password != "" {
		c.Warnings = append(c.Warnings, "SYNC_WEBDAV_URLS is set: SYNC_WEBDAV_URL and SYNC_WEBDAV_PASSWORD are ignored")
	}
	ul, pl := strings.Split(urls, ","), strings.Split(passwords, ",")
	if len(ul) != len(pl) {
		return c, true, fmt.Errorf("SYNC_WEBDAV_URLS names %d bridges, SYNC_WEBDAV_PASSWORDS has %d passwords", len(ul), len(pl))
	}
	bridges := make([]WebDAVBridge, len(ul))
	for i := range ul {
		bridges[i] = WebDAVBridge{URL: strings.TrimSpace(ul[i]), Password: strings.TrimSpace(pl[i])}
	}
	if err := checkWebDAVBridges("sync", bridges); err != nil {
		return c, true, fmt.Errorf("SYNC_WEBDAV_URLS: %w", err)
	}
	c.Bridges = bridges
	c.URL, c.Password = bridges[0].URL, bridges[0].Password
	return c, true, nil
}

// checkWebDAVBridges validates a bridge list: 1..16 bridges, each URL one
// NewWebDAVDriver accepts, a password each, no server twice. Errors name a
// bridge by index and never quote a password.
func checkWebDAVBridges(name string, bridges []WebDAVBridge) error {
	if len(bridges) == 0 {
		return fmt.Errorf("webdav %s: no bridge configured", name)
	}
	if len(bridges) > SyncWebDAVMaxBridges {
		return fmt.Errorf("webdav %s: %d bridges, at most %d", name, len(bridges), SyncWebDAVMaxBridges)
	}
	seen := map[string]int{}
	for i, b := range bridges {
		u, err := parseWebDAVURL(name, b.URL)
		if err != nil {
			return fmt.Errorf("bridge %d: %w", i, err)
		}
		if b.Password == "" {
			return fmt.Errorf("webdav %s: bridge %d has no password", name, i)
		}
		id := bridgeID(u)
		if j, dup := seen[id]; dup {
			return fmt.Errorf("webdav %s: bridges %d and %d are the same server %s", name, j, i, id)
		}
		seen[id] = i
	}
	return nil
}

// parseWebDAVURL checks a configured server URL: http or https, a host, no
// credentials (a URL lands in error messages), no query or fragment. An
// error never quotes the URL (it may hold userinfo).
func parseWebDAVURL(name, raw string) (*url.URL, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, fmt.Errorf("webdav %s: a URL is required", name)
	}
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return nil, fmt.Errorf("webdav %s: the URL does not parse", name)
	}
	if u.User != nil {
		return nil, fmt.Errorf("webdav %s: put the credentials in the username/password settings, not in the URL", name)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("webdav %s: URL scheme must be http or https, got %q", name, u.Scheme)
	}
	if u.Host == "" {
		return nil, fmt.Errorf("webdav %s: URL has no host", name)
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return nil, fmt.Errorf("webdav %s: URL must not carry a query or fragment", name)
	}
	return u, nil
}

// bridgeID is a server's identity (routing seed, duplicate check):
// scheme://host + the path without its trailing slash.
func bridgeID(u *url.URL) string {
	return strings.ToLower(u.Scheme) + "://" + strings.ToLower(u.Host) + strings.TrimSuffix(u.EscapedPath(), "/")
}

// NewWebDAVDriver builds a driver. name is the engine's backend name (the
// metrics and errors carry it); baseURL is the WebDAV root the server
// exposes (e.g. http://127.0.0.1:4918 for the Sync bridge, or
// https://uXXXX.your-storagebox.de); root is the folder under it objects
// live in ("" = the URL's own path). Credentials go in username/password,
// never in the URL: a URL lands in error messages. opts tune the limits
// (webdav_resilience.go); the defaults suit Sync's bridge.
func NewWebDAVDriver(name, baseURL, username, password, root string, logger *zap.Logger, opts ...WebDAVOption) (*WebDAVDriver, error) {
	if strings.TrimSpace(name) == "" {
		return nil, errors.New("webdav: a backend name is required")
	}
	u, err := parseWebDAVURL(name, baseURL)
	if err != nil {
		return nil, err
	}
	if password == "" {
		return nil, fmt.Errorf("webdav %s: a password is required", name)
	}
	rootSegs, err := plainSegments(root)
	if err != nil {
		return nil, fmt.Errorf("webdav %s: root %q: %w", name, root, err)
	}
	baseSegs, err := plainSegments(u.Path)
	if err != nil {
		return nil, fmt.Errorf("webdav %s: URL path: %w", name, err)
	}
	if logger == nil {
		logger = zap.NewNop()
	}
	InitTenantlessSeries(name)
	limits := defaultWebDAVSettings()
	for _, o := range opts {
		o(&limits)
	}
	InitWebDAVSeries(name, limits.bridge)

	d := &WebDAVDriver{
		name:     name,
		base:     url.URL{Scheme: u.Scheme, Host: u.Host},
		basePath: "/" + strings.Join(baseSegs, "/"),
		rootPath: "/" + strings.Join(append(append([]string(nil), baseSegs...), rootSegs...), "/"),
		baseSegs: baseSegs,
		rootSegs: rootSegs,
		username: username,
		password: password,
		// HTTP/1.1 like the other drivers: one h2 connection funnels every
		// upload to a remote server (Storage Box, Koofr) through one TCP
		// stream. Honours VAULTAIRE_TUNED_TRANSPORT.
		client: TunedHTTPClient(WithHTTP1Only(), WithResponseHeaderTimeout(webdavResponseHeaderTimeout)),
		logger: logger,
		limits: limits,
		sem:    make(chan struct{}, limits.maxConcurrency),
	}
	logger.Info("WebDAV driver initialized",
		zap.String("backend", name),
		zap.String("url", d.origin()+d.escapedPath(nil, false)),
		zap.String("user", username),
		zap.String("bridge", limits.bridge),
		zap.Int("max_concurrency", limits.maxConcurrency),
		zap.Duration("idle_timeout", limits.idleTimeout),
		zap.Duration("put_timeout_per_64MiB", limits.putTimeout),
		zap.Int("attempts", limits.attempts))
	return d, nil
}

// plainSegments splits an operator-configured path into segments, refusing
// "." and "..".
func plainSegments(p string) ([]string, error) {
	var segs []string
	for _, s := range strings.Split(p, "/") {
		switch s {
		case "":
			continue
		case ".", "..":
			return nil, errors.New(`"." and ".." are not allowed`)
		}
		segs = append(segs, s)
	}
	return segs, nil
}

// Name returns the engine backend name the driver was built with.
func (d *WebDAVDriver) Name() string { return d.name }

// davName maps one key segment onto a WebDAV resource name, reversibly, so
// that every S3 key is one Sync.com's bridge stores (rules measured on
// prod's bridges 2026-10-07, webdav_README.md "Key mapping"): '%' and the
// characters Sync refuses (`: ? * " < > | \`, control characters but tab)
// are percent-encoded wherever they are; so is a leading space or '~', a
// trailing '.' or space, and the first character of a Windows device name
// (CON, PRN, AUX, NUL, COM0-9, LPT0-9 — also with an extension), of
// desktop.ini / Thumbs.db (refused) and of .DS_Store (accepted and silently
// dropped). "" (a trailing `/` or `a//b`) is "%" — never a real name, since a
// real '%' is "%25". "." and ".." come out as "%2E" and ".%2E": the server
// never sees an empty, "." or ".." segment. Plain names map to themselves.
func davName(seg string) string {
	if seg == "" {
		return "%"
	}
	first := seg[0] == ' ' || seg[0] == '~' || syncReservedName(seg)
	last := len(seg) - 1
	var b strings.Builder
	for i := 0; i < len(seg); i++ {
		c := seg[i]
		if c == '%' || strings.IndexByte(`:?*"<>|\`, c) >= 0 || (c < 0x20 && c != '\t') || c == 0x7f ||
			(i == 0 && first) || (i == last && (c == '.' || c == ' ')) {
			fmt.Fprintf(&b, "%%%02X", c)
			continue
		}
		b.WriteByte(c)
	}
	return b.String()
}

// syncReservedName: a name Sync refuses or drops because of what it is, not
// of a character in it (case-insensitive).
func syncReservedName(seg string) bool {
	switch strings.ToLower(seg) {
	case "desktop.ini", "thumbs.db", ".ds_store":
		return true
	}
	stem, _, _ := strings.Cut(seg, ".")
	stem = strings.ToUpper(strings.TrimRight(stem, " "))
	switch stem {
	case "CON", "PRN", "AUX", "NUL":
		return true
	}
	return len(stem) == 4 && (strings.HasPrefix(stem, "COM") || strings.HasPrefix(stem, "LPT")) &&
		stem[3] >= '0' && stem[3] <= '9'
}

// keySegment is davName's inverse ("%XX" decoded; a '%' not followed by two
// hex digits is kept as it is).
func keySegment(name string) string {
	if name == "%" {
		return ""
	}
	if !strings.Contains(name, "%") {
		return name
	}
	var b strings.Builder
	for i := 0; i < len(name); i++ {
		if name[i] == '%' && i+2 < len(name) && isHex(name[i+1]) && isHex(name[i+2]) {
			b.WriteByte(unhex(name[i+1])<<4 | unhex(name[i+2]))
			i += 2
			continue
		}
		b.WriteByte(name[i])
	}
	return b.String()
}

func isHex(c byte) bool {
	return ('0' <= c && c <= '9') || ('a' <= c && c <= 'f') || ('A' <= c && c <= 'F')
}

func unhex(c byte) byte {
	switch {
	case c >= 'a':
		return c - 'a' + 10
	case c >= 'A':
		return c - 'A' + 10
	}
	return c - '0'
}

// webdavMaxNameUnits is the longest resource name Sync's bridge takes: 248
// characters (measured 2026-10-07 — 248 created, 249 refused; 248 × 'é' =
// 496 bytes accepted, so characters, not bytes). Counted in UTF-16 units to
// be safe for characters outside the BMP (unmeasured).
const webdavMaxNameUnits = 248

// errNameTooLong: a mapped name longer than the bridge takes. A PUT of
// such a key is the caller's error (it wraps engine.ErrInvalidInput); a
// read, Exists or Delete of one is a miss — it can never have been stored.
var errNameTooLong = fmt.Errorf("%w: key segment too long for WebDAV", engine.ErrInvalidInput)

// unstorableMiss turns errNameTooLong into the read's answer: not found.
func unstorableMiss(err error, container, artifact string) error {
	if errors.Is(err, errNameTooLong) {
		return engine.ErrNotFound(container, artifact)
	}
	return err
}

// checkNames refuses a mapped name the bridge would refuse for its length —
// before any request.
func checkNames(names []string) error {
	for _, n := range names {
		units := 0
		for _, r := range n {
			units += utf16.RuneLen(r)
		}
		if units > webdavMaxNameUnits {
			return fmt.Errorf("%w: %d characters once mapped (max %d)", errNameTooLong, units, webdavMaxNameUnits)
		}
	}
	return nil
}

// escapedPath is the escaped URL path of base + root + names (names are
// resource names, already davName-mapped). dir appends the trailing slash.
func (d *WebDAVDriver) escapedPath(names []string, dir bool) string {
	var b strings.Builder
	for _, group := range [][]string{d.baseSegs, d.rootSegs, names} {
		for _, s := range group {
			b.WriteByte('/')
			b.WriteString(url.PathEscape(s))
		}
	}
	if b.Len() == 0 || dir {
		b.WriteByte('/')
	}
	return b.String()
}

// prefixSegs is base + root: what every href of ours starts with.
func (d *WebDAVDriver) prefixSegs() []string {
	out := make([]string, 0, len(d.baseSegs)+len(d.rootSegs))
	out = append(out, d.baseSegs...)
	return append(out, d.rootSegs...)
}

// tenantNames are the resource names of a tenant's folder and a container.
func tenantNames(tenantID, container string) ([]string, error) {
	if strings.Contains(tenantID, "/") || strings.Contains(container, "/") {
		return nil, fmt.Errorf("%w: tenant or container contains '/'", engine.ErrInvalidInput)
	}
	return []string{davName("t-" + tenantID), davName(container)}, nil
}

// WebDAVLeafMarker ends the resource name of every object (the leaf of
// its path); folder names never carry it. S3 allows a key `x` and a key
// `x/y`; WebDAV cannot hold a file and a folder of one name — and with
// several bridges the check cannot even be made: `x` and `x/y` route to
// different bridges, the second one has not seen `x` yet (cross-bridge
// staleness, 1.5 s – 5 min), creates the folder, and Sync's cloud keeps the
// folder and loses the file (prod, 2026-10-07). With the marker `x` is the
// file `x%o` and `x/y` the folder `x` holding `y%o`. Unambiguous: davName
// writes a literal '%' as "%25", so a mapped name holds '%' only before two
// hex digits or as the empty-segment name "%" — never before 'o'.
const WebDAVLeafMarker = "%o"

// leafName is the resource name of an object's last key segment.
func leafName(seg string) string { return davName(seg) + WebDAVLeafMarker }

// leafSegment is leafName's inverse; ok is false for a file without the
// marker (written before it, or by something else) — not an object.
func leafSegment(name string) (string, bool) {
	if !strings.HasSuffix(name, WebDAVLeafMarker) {
		return "", false
	}
	return keySegment(strings.TrimSuffix(name, WebDAVLeafMarker)), true
}

// objectNames are the resource names of an object, root excluded: folders
// davName-mapped, the leaf leafName-mapped. The length is not checked here
// (object does it: a PUT refuses, a read misses).
func objectNames(tenantID, container, artifact string) ([]string, error) {
	names, err := tenantNames(tenantID, container)
	if err != nil {
		return nil, err
	}
	segs := strings.Split(artifact, "/")
	for _, s := range segs[:len(segs)-1] {
		names = append(names, davName(s))
	}
	return append(names, leafName(segs[len(segs)-1])), nil
}

// object resolves a call to its tenant key (for errors) and resource names.
func (d *WebDAVDriver) object(ctx context.Context, op, container, artifact string) (string, []string, error) {
	tenantID, err := requireTenant(ctx, d.name, op, "", d.logger)
	if err != nil {
		return "", nil, err
	}
	names, err := objectNames(tenantID, container, artifact)
	if err == nil {
		err = checkNames(names)
	}
	if err != nil {
		return "", nil, fmt.Errorf("%s %s %s/%s: %w", d.name, op, container, artifact, err)
	}
	return tenantKey(tenantID, container, artifact), names, nil
}

// ObjectKey implements engine.KeyAddresser: the fixed-bucket key a call
// addresses (below the root folder).
func (d *WebDAVDriver) ObjectKey(ctx context.Context, container, artifact string) string {
	return tenantKey(contextTenant(ctx), container, artifact)
}

// origin is scheme://host[:port] of the configured server.
func (d *WebDAVDriver) origin() string { return d.base.Scheme + "://" + d.base.Host }

// webdavEscapedPath is what escapedPath can produce: an absolute path of
// url.PathEscape'd segments — unreserved and sub-delim characters and %XX
// escapes; never '?', '#', '\', a scheme or a raw space.
var webdavEscapedPath = regexp.MustCompile(`^/(?:[A-Za-z0-9\-._~!$&'()*+,;=:@/]|%[0-9A-Fa-f]{2})*$`)

// requestURL turns an escaped path into the URL a request goes to: a copy of
// the configured base (scheme and host from the constructor, never from the
// path) with only Path/RawPath set. The path must be what escapedPath makes
// — absolute, no '?'/'#', no empty, "." or ".." segment (also between
// backslashes, which some servers treat as separators), no encoded '/' or
// NUL — and must be the root folder or below it, or exactly the server's own
// folder (the health check's fallback). Anything else is refused with
// engine.ErrInvalidInput before a request is built.
func (d *WebDAVDriver) requestURL(escaped string) (*url.URL, error) {
	if !webdavEscapedPath.MatchString(escaped) {
		return nil, fmt.Errorf("%w: webdav request path %q is not an escaped absolute path", engine.ErrInvalidInput, escaped)
	}
	decoded, err := url.PathUnescape(escaped)
	if err != nil {
		return nil, fmt.Errorf("%w: webdav request path %q: %w", engine.ErrInvalidInput, escaped, err)
	}
	if escaped != "/" {
		for _, raw := range strings.Split(strings.TrimSuffix(escaped, "/"), "/")[1:] {
			seg, err := url.PathUnescape(raw)
			if err != nil {
				return nil, fmt.Errorf("%w: webdav request path %q: %w", engine.ErrInvalidInput, escaped, err)
			}
			if seg == "" || strings.ContainsAny(seg, "/\x00") {
				return nil, fmt.Errorf("%w: webdav request path %q has an empty or invalid segment", engine.ErrInvalidInput, escaped)
			}
			for _, part := range strings.Split(seg, `\`) {
				if part == "." || part == ".." {
					return nil, fmt.Errorf("%w: webdav request path %q has a dot segment", engine.ErrInvalidInput, escaped)
				}
			}
		}
	}
	clean := path.Clean(decoded)
	underRoot := clean == d.rootPath || strings.HasPrefix(clean, strings.TrimSuffix(d.rootPath, "/")+"/")
	if !underRoot && clean != d.basePath {
		return nil, fmt.Errorf("%w: webdav request path %q is outside the root %q", engine.ErrInvalidInput, escaped, d.rootPath)
	}
	u := d.base // a copy: Scheme and Host come from the configuration only
	u.Path = decoded
	u.RawPath = escaped
	if u.Scheme != d.base.Scheme || u.Host != d.base.Host || u.User != nil || u.Opaque != "" || u.RawQuery != "" || u.Fragment != "" {
		return nil, fmt.Errorf("%w: webdav request URL left the configured server %s", engine.ErrInvalidInput, d.origin())
	}
	return &u, nil
}

// drainClose reads a little of an unwanted body so the connection can be
// reused, then closes it.
func drainClose(resp *http.Response) {
	_, _ = io.CopyN(io.Discard, resp.Body, 64<<10)
	_ = resp.Body.Close()
}

// statusError describes a response we did not want, with a short excerpt of
// its body. It never carries the request's credentials.
func statusError(resp *http.Response) error {
	excerpt, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
	_ = resp.Body.Close()
	return &webdavStatusError{code: resp.StatusCode, status: resp.Status, excerpt: strings.TrimSpace(string(excerpt))}
}

// refusedNameStatus: an answer to a PUT or MKCOL that refuses the name
// itself (Sync's bridge: 400 for a name it does not take; 414 for a path too
// long) — the caller's error (engine.ErrInvalidInput), never a sign the
// server is unwell, so no breaker is charged for it.
func refusedNameStatus(code int) bool {
	return code == http.StatusBadRequest || code == http.StatusRequestURITooLong
}

// webdavStatusError is an answer we did not want (statusError). The
// multi-bridge driver reads the code: a 5xx left after the retries is a
// bridge in trouble (a read may go to another bridge), a 4xx is not.
type webdavStatusError struct {
	code            int
	status, excerpt string
}

func (e *webdavStatusError) Error() string {
	if e.excerpt != "" {
		return "unexpected status " + e.status + ": " + e.excerpt
	}
	return "unexpected status " + e.status
}

// --- PROPFIND -------------------------------------------------------------

// propfindBody asks for the type and the size only — never getlastmodified
// or creationdate: Sync's bridge reports a wrong modification time for
// every file (live, 2026-10-07: a file uploaded now lists as 1970-01-21;
// rclone --min-age retention deleted fresh files because of it). No
// decision in this driver — listing, walk, health, fallback — rests on a
// server's modtime.

const propfindBody = `<?xml version="1.0" encoding="utf-8"?>` +
	`<D:propfind xmlns:D="DAV:"><D:prop><D:resourcetype/><D:getcontentlength/></D:prop></D:propfind>`

type davMultistatus struct {
	Responses []davResponse `xml:"DAV: response"`
}

type davResponse struct {
	Href      string        `xml:"DAV: href"`
	Propstats []davPropstat `xml:"DAV: propstat"`
}

type davPropstat struct {
	Status string  `xml:"DAV: status"`
	Prop   davProp `xml:"DAV: prop"`
}

type davProp struct {
	ResourceType *struct {
		Collection *struct{} `xml:"DAV: collection"`
	} `xml:"DAV: resourcetype"`
	ContentLength string `xml:"DAV: getcontentlength"`
}

// davEntry is one resource of a PROPFIND answer.
type davEntry struct {
	segs []string // decoded path segments of the href
	dir  bool
	size int64 // -1 = not reported
}

// propfind asks for resourcetype + getcontentlength at depth "0" or "1".
// found is false on a 404.
func (d *WebDAVDriver) propfind(ctx context.Context, path, depth string) (entries []davEntry, found bool, err error) {
	call, err := d.do(ctx, davSpec{method: "PROPFIND", path: path, body: propfindBody, timeout: webdavMetaTimeout,
		header: map[string]string{"Depth": depth, "Content-Type": "application/xml; charset=utf-8"}})
	if err != nil {
		return nil, false, err
	}
	defer call.finish()
	resp := call.resp
	switch resp.StatusCode {
	case http.StatusMultiStatus:
	case http.StatusNotFound:
		drainClose(resp)
		return nil, false, nil
	default:
		return nil, false, statusError(resp)
	}
	defer func() { _ = resp.Body.Close() }()
	var ms davMultistatus
	if err := xml.NewDecoder(io.LimitReader(resp.Body, webdavMaxMultistatus)).Decode(&ms); err != nil {
		return nil, true, fmt.Errorf("parse multistatus: %w", err)
	}
	files := 0
	for _, r := range ms.Responses {
		e, err := parseDavEntry(r)
		if err != nil {
			return nil, true, err
		}
		entries = append(entries, e)
		if !e.dir {
			files++
		}
	}
	if depth == "1" {
		d.recordFolder(path, files)
	}
	return entries, true, nil
}

func parseDavEntry(r davResponse) (davEntry, error) {
	href := strings.TrimSpace(r.Href)
	u, err := url.Parse(href)
	if err != nil {
		return davEntry{}, fmt.Errorf("parse href %q: %w", href, err)
	}
	e := davEntry{size: -1, dir: strings.HasSuffix(u.EscapedPath(), "/")}
	for _, raw := range strings.Split(u.EscapedPath(), "/") {
		if raw == "" {
			continue
		}
		s, err := url.PathUnescape(raw)
		if err != nil {
			return davEntry{}, fmt.Errorf("unescape href %q: %w", href, err)
		}
		e.segs = append(e.segs, s)
	}
	for _, ps := range r.Propstats {
		if !propstatOK(ps.Status) {
			continue
		}
		if ps.Prop.ResourceType != nil {
			e.dir = ps.Prop.ResourceType.Collection != nil
		}
		if v := strings.TrimSpace(ps.Prop.ContentLength); v != "" {
			if n, err := strconv.ParseInt(v, 10, 64); err == nil {
				e.size = n
			}
		}
	}
	return e, nil
}

// propstatOK: a missing status is taken as success (some servers omit it).
func propstatOK(status string) bool {
	f := strings.Fields(status)
	return len(f) < 2 || f[1] == "200"
}

// relSegments returns e's segments below parent (decoded), and whether e is
// inside parent at all.
func relSegments(e davEntry, parent []string) ([]string, bool) {
	if len(e.segs) < len(parent) {
		return nil, false
	}
	for i, s := range parent {
		if e.segs[i] != s {
			return nil, false
		}
	}
	return e.segs[len(parent):], true
}

// stat is a Depth 0 PROPFIND of one resource.
func (d *WebDAVDriver) stat(ctx context.Context, names []string) (davEntry, bool, error) {
	entries, found, err := d.propfind(ctx, d.escapedPath(names, false), "0")
	if err != nil || !found {
		return davEntry{}, found, err
	}
	if len(entries) == 0 {
		return davEntry{}, false, errors.New("PROPFIND answered no resource")
	}
	return entries[0], true, nil
}

// walk visits every file below the collection `names` (resource names),
// one Depth 1 PROPFIND per collection (Depth infinity is commonly disabled).
// fn gets the file's path below `names` as resource names and its href
// path. descend (may be nil) prunes subtrees. A missing collection is an
// empty walk; an href from outside the collection is an error.
func (d *WebDAVDriver) walk(ctx context.Context, names []string, descend func(rel []string) bool, fn func(rel []string, path string) error) error {
	top := append(d.prefixSegs(), names...)
	queue := [][]string{nil}
	for len(queue) > 0 {
		rel := queue[0]
		queue = queue[1:]
		dirNames := append(append([]string(nil), names...), rel...)
		path := d.escapedPath(dirNames, true)
		entries, found, err := d.propfind(ctx, path, "1")
		if err != nil {
			return fmt.Errorf("list %s: %w", path, err)
		}
		if !found {
			if rel == nil {
				return nil
			}
			return fmt.Errorf("list %s: the folder vanished during the listing", path)
		}
		for _, e := range entries {
			sub, ok := relSegments(e, top)
			if !ok {
				return fmt.Errorf("list %s: the server returned %q from outside the folder", path, "/"+strings.Join(e.segs, "/"))
			}
			if len(sub) <= len(rel) {
				continue // the collection itself
			}
			if len(sub) != len(rel)+1 {
				return fmt.Errorf("list %s: the server returned %q, not a direct member", path, "/"+strings.Join(e.segs, "/"))
			}
			if e.dir {
				if descend == nil || descend(sub) {
					queue = append(queue, sub)
				}
				continue
			}
			if err := fn(sub, d.escapedPath(append(append([]string(nil), names...), sub...), false)); err != nil {
				return err
			}
		}
	}
	return nil
}

// --- collections ------------------------------------------------------------

// parentPaths are the escaped paths of every folder an object needs, top
// down: the root folder's segments, then the object's parents.
func (d *WebDAVDriver) parentPaths(names []string) []string {
	var out []string
	var b strings.Builder
	for _, s := range d.baseSegs {
		b.WriteByte('/')
		b.WriteString(url.PathEscape(s))
	}
	all := append(append([]string(nil), d.rootSegs...), names[:len(names)-1]...)
	for _, s := range all {
		b.WriteByte('/')
		b.WriteString(url.PathEscape(s))
		out = append(out, b.String())
	}
	return out
}

// mkcol creates one collection. Concurrent PUTs into a new folder all want
// the same MKCOLs, and a server answers the ones that overlap 423 Locked
// (x/net/webdav does), so one MKCOL per path runs at a time in this process
// (the others then find it known), and a 423 — another process holding the
// folder for a moment — is retried a few times.
func (d *WebDAVDriver) mkcol(ctx context.Context, path string) (int, error) {
	mu, _ := d.mkcolLocks.LoadOrStore(path, &sync.Mutex{})
	m := mu.(*sync.Mutex)
	m.Lock()
	defer m.Unlock()
	if _, ok := d.known.Load(path); ok {
		return http.StatusMethodNotAllowed, nil // exists
	}
	for attempt := 1; ; attempt++ {
		code, err := d.mkcolOnce(ctx, path)
		if code != http.StatusLocked || attempt > webdavLockedRetries {
			return code, err
		}
		select {
		case <-ctx.Done():
			return code, fmt.Errorf("%w (waiting out a 423: %w)", err, ctx.Err())
		case <-time.After(time.Duration(attempt) * webdavLockedBackoff):
		}
	}
}

func (d *WebDAVDriver) mkcolOnce(ctx context.Context, path string) (int, error) {
	call, err := d.do(ctx, davSpec{method: "MKCOL", path: path + "/", timeout: webdavMetaTimeout, noRetry423: true})
	if err != nil {
		return 0, err
	}
	defer call.finish()
	resp := call.resp
	switch resp.StatusCode {
	case http.StatusCreated, http.StatusOK, http.StatusNoContent, http.StatusMethodNotAllowed:
		// 405 = it already exists (RFC 4918 §9.3.1).
		drainClose(resp)
		return resp.StatusCode, nil
	case http.StatusConflict, http.StatusNotFound:
		drainClose(resp)
		return resp.StatusCode, nil
	default:
		if refusedNameStatus(resp.StatusCode) {
			return resp.StatusCode, fmt.Errorf("%w: MKCOL %s: the server refused the name: %w", engine.ErrInvalidInput, path, statusError(resp))
		}
		return resp.StatusCode, fmt.Errorf("MKCOL %s: %w", path, statusError(resp))
	}
}

// ensureCollections makes sure every folder in dirs (top down) exists. The
// deepest unknown one is tried first — usually the only one missing; a 409
// (a parent is missing too) creates each from the top.
func (d *WebDAVDriver) ensureCollections(ctx context.Context, dirs []string) error {
	first := -1
	for i, p := range dirs {
		if _, ok := d.known.Load(p); !ok {
			first = i
			break
		}
	}
	if first < 0 {
		return nil
	}
	markAll := func() {
		for _, p := range dirs {
			d.known.Store(p, struct{}{})
		}
	}
	code, err := d.mkcol(ctx, dirs[len(dirs)-1])
	if err != nil {
		return err
	}
	if code != http.StatusConflict && code != http.StatusNotFound {
		markAll()
		return nil
	}
	for _, p := range dirs[first:] {
		code, err := d.mkcol(ctx, p)
		if err != nil {
			return err
		}
		if code == http.StatusConflict || code == http.StatusNotFound {
			return fmt.Errorf("%w: MKCOL %s: the server answered %d with every parent created (a file where a folder is needed?)", engine.ErrInvalidInput, p, code)
		}
		d.known.Store(p, struct{}{})
	}
	markAll()
	return nil
}

func (d *WebDAVDriver) forgetCollections(dirs []string) {
	for _, p := range dirs {
		d.known.Delete(p)
	}
}

// --- engine.Driver ----------------------------------------------------------

// Put streams the object to the server. The folders it needs are created
// first (cached). A PUT answered 404/409 — a folder removed behind the cache
// — recreates them and is sent once more; a transient failure (5xx, 423, a
// transport error, a stall, the deadline) is sent again up to the retry
// limit. Either needs the body again: a seekable body is rewound, a
// non-seekable one of known length ≤ 8 MiB is held in memory for it
// (webdavRetryBufferMax), a larger one is never buffered — a failure after
// its bytes went out is returned with engine.ErrNoFailover. With a known
// length the stored size is checked afterwards: a server that kept fewer
// bytes than it was sent (a full disk on a bridge's spill directory) is an
// error, never a silently short object. Overwrites replace.
func (d *WebDAVDriver) Put(ctx context.Context, container, artifact string, data io.Reader, opts ...engine.PutOption) error {
	key, names, err := d.object(ctx, "Put", container, artifact)
	if err != nil {
		return err
	}
	return d.putNames(ctx, key, names, data, opts...)
}

// putNames is Put at resource names below the root (key names the object
// in errors and logs).
func (d *WebDAVDriver) putNames(ctx context.Context, key string, names []string, data io.Reader, opts ...engine.PutOption) error {
	folder := d.escapedPath(names[:len(names)-1], true)
	err := d.putNamesOnce(ctx, key, names, data, opts...)
	if err != nil {
		return d.fullFolderErr(ctx, key, folder, err)
	}
	d.notePut(folder)
	return nil
}

func (d *WebDAVDriver) putNamesOnce(ctx context.Context, key string, names []string, data io.Reader, opts ...engine.PutOption) error {
	o := engine.ApplyPutOptions(opts...)
	dirs := d.parentPaths(names)
	path := d.escapedPath(names, false)
	if err := d.ensureCollections(ctx, dirs); err != nil {
		return fmt.Errorf("%s put %s: %w", d.name, key, err)
	}
	src, rewind, err := replayableBody(data, o.ContentLength)
	if err != nil {
		return fmt.Errorf("%s put %s: %w", d.name, key, err)
	}

	foldersRecreated := false
	transientTries := 0
	for {
		body := &uploadBody{r: src}
		code, transient, err := d.put(ctx, path, body, o)
		if err == nil && code != http.StatusConflict && code != http.StatusNotFound {
			break // 2xx
		}
		if err == nil { // 404/409: a folder went missing behind the cache
			if foldersRecreated {
				// A file where the key needs a folder (keys `a` and `a/b`;
				// WebDAV cannot hold both): the caller's key, not the server.
				return fmt.Errorf("%w: %s put %s: the server answered %d with every folder created (a file where a folder is needed?)", engine.ErrInvalidInput, d.name, key, code)
			}
			foldersRecreated = true
			d.forgetCollections(dirs)
			if err := d.ensureCollections(ctx, dirs); err != nil {
				return fmt.Errorf("%s put %s: recreate folders: %w", d.name, key, err)
			}
			if rerr := rewindFor(body, rewind); rerr != nil {
				return fmt.Errorf("%s put %s: the server answered %d after the body was sent and %w", d.name, key, code, rerr)
			}
			continue
		}
		transientTries++
		if !transient || transientTries >= d.limits.attempts || ctx.Err() != nil {
			if body.consumed() > 0 && rewind == nil {
				return fmt.Errorf("%s put %s: %w: %w", d.name, key, err, engine.ErrNoFailover)
			}
			return fmt.Errorf("%s put %s: %w", d.name, key, err)
		}
		if rerr := rewindFor(body, rewind); rerr != nil {
			return fmt.Errorf("%s put %s: %w; no retry: %w", d.name, key, err, rerr)
		}
		d.retried(http.MethodPut)
		d.logger.Warn("webdav PUT retried", zap.String("backend", d.name), zap.String("key", key),
			zap.Int("attempt", transientTries), zap.Error(err))
		if berr := d.backoff(ctx, transientTries); berr != nil {
			return fmt.Errorf("%s put %s: %w (giving up a retry: %w)", d.name, key, err, berr)
		}
	}

	if o.ContentLength > 0 {
		e, found, err := d.stat(ctx, names)
		if err != nil {
			return fmt.Errorf("%s put %s: verify size: %w", d.name, key, err)
		}
		if !found {
			return fmt.Errorf("%s put %s: verify size: the object is not there after a successful PUT", d.name, key)
		}
		if e.dir {
			return fmt.Errorf("%s put %s: verify size: the key is a folder", d.name, key)
		}
		if e.size >= 0 && e.size != o.ContentLength {
			return fmt.Errorf("%s put %s: stored size %d, sent %d (truncated upload)", d.name, key, e.size, o.ContentLength)
		}
	}
	return nil
}

// replayableBody returns the reader a PUT sends and how to start it over
// (nil = it cannot be): a seekable body is rewound to where it stands now; a
// non-seekable body of known length ≤ webdavRetryBufferMax is read into
// memory first; anything else streams once.
func replayableBody(data io.Reader, length int64) (io.Reader, func() error, error) {
	if s, ok := data.(io.Seeker); ok {
		if start, err := s.Seek(0, io.SeekCurrent); err == nil {
			return data, func() error {
				_, err := s.Seek(start, io.SeekStart)
				return err
			}, nil
		}
	}
	if length > 0 && length <= webdavRetryBufferMax {
		buf := make([]byte, length)
		if _, err := io.ReadFull(data, buf); err != nil {
			return nil, nil, fmt.Errorf("read the %d-byte body: %w", length, err)
		}
		r := bytes.NewReader(buf)
		return r, func() error {
			_, err := r.Seek(0, io.SeekStart)
			return err
		}, nil
	}
	return data, nil, nil
}

// rewindFor makes the source ready for another attempt after body's: the
// attempt is fenced off (the transport may still be reading it), then the
// source is rewound — or, when it cannot be, left as it is if the attempt
// took no byte of it.
func rewindFor(body *uploadBody, rewind func() error) error {
	n := body.fence()
	if rewind == nil {
		if n > 0 {
			return fmt.Errorf("the body cannot be rewound: %w", engine.ErrNoFailover)
		}
		return nil
	}
	if err := rewind(); err != nil {
		return fmt.Errorf("rewind: %w: %w", err, engine.ErrNoFailover)
	}
	return nil
}

// put sends one PUT attempt. A 2xx or 404/409 is returned as a code with a
// nil error (404/409 so the caller can create the folders); anything else
// is an error, transient when a retry may cure it.
func (d *WebDAVDriver) put(ctx context.Context, path string, body *uploadBody, o engine.PutOptions) (int, bool, error) {
	header := map[string]string{"Content-Type": "application/octet-stream"}
	if o.ContentType != "" {
		header["Content-Type"] = o.ContentType
	}
	// ContentLength 0 = unknown: chunked transfer encoding. (A known empty
	// body is indistinguishable from "unknown" in PutOptions; chunked
	// carries zero bytes just as well.)
	timeout := webdavPutDeadline(d.limits.putTimeout, o.ContentLength)
	call, transient, err := d.send(ctx, http.MethodPut, path, body, o.ContentLength, header, timeout, watchUpload)
	if err != nil {
		return 0, transient, err
	}
	defer call.finish()
	resp := call.resp
	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		drainClose(resp)
		return resp.StatusCode, false, nil
	case resp.StatusCode == http.StatusConflict || resp.StatusCode == http.StatusNotFound:
		drainClose(resp)
		return resp.StatusCode, false, nil
	case refusedNameStatus(resp.StatusCode):
		return resp.StatusCode, false, fmt.Errorf("%w: the server refused the name: %w", engine.ErrInvalidInput, statusError(resp))
	}
	return resp.StatusCode, transient, statusError(resp)
}

// get sends a GET (retried until an answer that is not transient) and hands
// back the answer with its body wrapped for the idle watchdog; Close of the
// body ends the call.
func (d *WebDAVDriver) get(ctx context.Context, path, what string, header map[string]string) (*http.Response, error) {
	call, err := d.do(ctx, davSpec{method: http.MethodGet, path: path, header: header, stream: true})
	if err != nil {
		return nil, err
	}
	resp := call.resp
	body := &downloadBody{rc: resp.Body, call: call, d: d, what: what}
	resp.Body = body
	if d.bodyGate != nil && (resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusPartialContent) {
		release, err := d.bodyGate(ctx, resp.ContentLength)
		if err != nil {
			_ = body.Close()
			return nil, err
		}
		body.release = release
	}
	return resp, nil
}

// Get streams the object. A 404 is the engine's NotFoundError.
func (d *WebDAVDriver) Get(ctx context.Context, container, artifact string) (io.ReadCloser, error) {
	key, names, err := d.object(ctx, "Get", container, artifact)
	if err != nil {
		return nil, unstorableMiss(err, container, artifact)
	}
	return d.getNames(ctx, key, names, engine.ErrNotFound(container, artifact))
}

// getNames is Get at resource names; a 404 is notFound (wrapped).
func (d *WebDAVDriver) getNames(ctx context.Context, key string, names []string, notFound error) (io.ReadCloser, error) {
	resp, err := d.get(ctx, d.escapedPath(names, false), d.name+" get "+key, nil)
	if err != nil {
		return nil, fmt.Errorf("%s get %s: %w", d.name, key, err)
	}
	switch resp.StatusCode {
	case http.StatusOK:
		return resp.Body, nil
	case http.StatusNotFound:
		drainClose(resp)
		return nil, fmt.Errorf("%s get %s: %w", d.name, key, notFound)
	}
	return nil, fmt.Errorf("%s get %s: %w", d.name, key, statusError(resp))
}

// GetRange implements engine.RangeGetter. length <= 0 = to the end. A server
// that ignores Range and answers 200 has the bytes before the offset
// discarded and the rest limited to length — never the wrong bytes.
func (d *WebDAVDriver) GetRange(ctx context.Context, container, artifact string, offset, length int64) (io.ReadCloser, error) {
	key, names, err := d.object(ctx, "GetRange", container, artifact)
	if err != nil {
		return nil, unstorableMiss(err, container, artifact)
	}
	return d.getRangeNames(ctx, key, names, offset, length, engine.ErrNotFound(container, artifact))
}

// getRangeNames is GetRange at resource names; a 404 is notFound (wrapped).
func (d *WebDAVDriver) getRangeNames(ctx context.Context, key string, names []string, offset, length int64, notFound error) (io.ReadCloser, error) {
	if offset < 0 {
		return nil, fmt.Errorf("%s get range %s: %w: negative offset", d.name, key, engine.ErrInvalidInput)
	}
	rng := fmt.Sprintf("bytes=%d-", offset)
	if length > 0 {
		rng = fmt.Sprintf("bytes=%d-%d", offset, offset+length-1)
	}
	resp, err := d.get(ctx, d.escapedPath(names, false), d.name+" get range "+key, map[string]string{"Range": rng})
	if err != nil {
		return nil, fmt.Errorf("%s get range %s: %w", d.name, key, err)
	}
	switch resp.StatusCode {
	case http.StatusPartialContent:
		if cr := resp.Header.Get("Content-Range"); cr != "" && !strings.HasPrefix(cr, fmt.Sprintf("bytes %d-", offset)) {
			_ = resp.Body.Close()
			return nil, fmt.Errorf("%s get range %s: asked for %s, the server sent %q", d.name, key, rng, cr)
		}
		if length > 0 {
			return limitedBody{io.LimitReader(resp.Body, length), resp.Body}, nil
		}
		return resp.Body, nil
	case http.StatusOK:
		if offset > 0 {
			if _, err := io.CopyN(io.Discard, resp.Body, offset); err != nil {
				_ = resp.Body.Close()
				if errors.Is(err, io.EOF) {
					return io.NopCloser(strings.NewReader("")), nil // offset past the end
				}
				return nil, fmt.Errorf("%s get range %s: skip to %d: %w", d.name, key, offset, err)
			}
		}
		if length > 0 {
			return limitedBody{io.LimitReader(resp.Body, length), resp.Body}, nil
		}
		return resp.Body, nil
	case http.StatusRequestedRangeNotSatisfiable:
		drainClose(resp)
		return io.NopCloser(strings.NewReader("")), nil
	case http.StatusNotFound:
		drainClose(resp)
		return nil, fmt.Errorf("%s get range %s: %w", d.name, key, notFound)
	}
	return nil, fmt.Errorf("%s get range %s: %w", d.name, key, statusError(resp))
}

type limitedBody struct {
	io.Reader
	io.Closer
}

// Delete removes the object; a miss is not an error. A WebDAV DELETE of a
// folder is recursive, and S3's DeleteObject("a") must never take "a/b" with
// it, so a key that is a folder on the server is left alone (it is not an
// object). That costs a PROPFIND per delete.
func (d *WebDAVDriver) Delete(ctx context.Context, container, artifact string) error {
	key, names, err := d.object(ctx, "Delete", container, artifact)
	if errors.Is(err, errNameTooLong) {
		return nil // never stored
	}
	if err != nil {
		return err
	}
	e, found, err := d.stat(ctx, names)
	if err != nil {
		return fmt.Errorf("%s delete %s: %w", d.name, key, err)
	}
	if !found || e.dir {
		return nil
	}
	if err := d.deletePath(ctx, d.escapedPath(names, false)); err != nil {
		return fmt.Errorf("%s delete %s: %w", d.name, key, err)
	}
	return nil
}

func (d *WebDAVDriver) deletePath(ctx context.Context, path string) error {
	call, err := d.do(ctx, davSpec{method: http.MethodDelete, path: path, timeout: webdavMetaTimeout})
	if err != nil {
		return err
	}
	defer call.finish()
	resp := call.resp
	if resp.StatusCode == http.StatusNotFound || (resp.StatusCode >= 200 && resp.StatusCode < 300) {
		drainClose(resp)
		return nil
	}
	return statusError(resp)
}

// Exists is a Depth 0 PROPFIND (not HEAD, which some bridges special-case).
// A folder is not an object.
func (d *WebDAVDriver) Exists(ctx context.Context, container, artifact string) (bool, error) {
	key, names, err := d.object(ctx, "Exists", container, artifact)
	if errors.Is(err, errNameTooLong) {
		return false, nil // never stored
	}
	if err != nil {
		return false, err
	}
	e, found, err := d.stat(ctx, names)
	if err != nil {
		return false, fmt.Errorf("%s exists %s: %w", d.name, key, err)
	}
	return found && !e.dir, nil
}

// List returns the artifact keys of a container that start with prefix,
// relative to the container, sorted. Folders are walked (pruned by the
// prefix) and are not keys themselves.
func (d *WebDAVDriver) List(ctx context.Context, container, prefix string) ([]string, error) {
	tenantID, err := requireTenant(ctx, d.name, "List", "", d.logger)
	if err != nil {
		return nil, err
	}
	names, err := tenantNames(tenantID, container)
	if err != nil {
		return nil, fmt.Errorf("%s list %s: %w", d.name, container, err)
	}
	dirKey := func(rel []string) string {
		segs := make([]string, len(rel))
		for i, s := range rel {
			segs[i] = keySegment(s)
		}
		return strings.Join(segs, "/")
	}
	var out []string
	seen := map[string]struct{}{}
	err = d.walk(ctx, names,
		func(rel []string) bool {
			dir := dirKey(rel) + "/"
			return strings.HasPrefix(dir, prefix) || strings.HasPrefix(prefix, dir)
		},
		func(rel []string, _ string) error {
			leaf, ok := objectLeafSegment(rel[len(rel)-1])
			if !ok {
				return nil // a file without a marker is not an object
			}
			k := leaf
			if len(rel) > 1 {
				k = dirKey(rel[:len(rel)-1]) + "/" + leaf
			}
			if _, dup := seen[k]; dup {
				return nil // a striped object's manifest beside a plain file (an interrupted overwrite)
			}
			seen[k] = struct{}{}
			if strings.HasPrefix(k, prefix) {
				out = append(out, k)
			}
			return nil
		})
	if err != nil {
		return nil, fmt.Errorf("%s list %s: %w", d.name, tenantKey(tenantID, container, prefix), err)
	}
	sort.Strings(out)
	return out, nil
}

// WalkTenant implements engine.TenantWalker: every file under
// `<root>/t-<tenant>/`, each with a Remove bound to the listed resource.
// The walk refuses bad ids before any request; a listing that cannot be
// completed, or names a resource outside the tenant's folder, is an error.
func (d *WebDAVDriver) WalkTenant(ctx context.Context, tenantID string, fn func(engine.TenantObject) error) error {
	if err := checkWalkTenant(tenantID); err != nil {
		return err
	}
	return d.walkTenantNames(ctx, tenantID, func(names []string) error {
		return fn(tenantObject(names, func(ctx context.Context) error { return d.removeNames(ctx, names) }))
	})
}

// walkTenantNames visits every file under `<root>/t-<tenant>/` with its
// resource names below the root (tenant folder first). The id must have
// passed checkWalkTenant.
func (d *WebDAVDriver) walkTenantNames(ctx context.Context, tenantID string, fn func(names []string) error) error {
	tenantDir := []string{davName("t-" + tenantID)}
	err := d.walk(ctx, tenantDir, nil, func(rel []string, _ string) error {
		return fn(append(append([]string(nil), tenantDir...), rel...))
	})
	if err != nil {
		return fmt.Errorf("%s walk t-%s/: %w", d.name, tenantID, err)
	}
	return nil
}

// children lists the direct members of the folder at resource names: its
// sub-folders and its files (resource names). A missing folder is empty.
func (d *WebDAVDriver) children(ctx context.Context, names []string) (dirs, files []string, err error) {
	path := d.escapedPath(names, true)
	entries, found, err := d.propfind(ctx, path, "1")
	if err != nil || !found {
		return nil, nil, err
	}
	top := append(d.prefixSegs(), names...)
	for _, e := range entries {
		sub, ok := relSegments(e, top)
		if !ok {
			return nil, nil, fmt.Errorf("list %s: the server returned %q from outside the folder", path, "/"+strings.Join(e.segs, "/"))
		}
		if len(sub) != 1 {
			continue // the folder itself
		}
		if e.dir {
			dirs = append(dirs, sub[0])
		} else {
			files = append(files, sub[0])
		}
	}
	return dirs, files, nil
}

// ErrDirNotEmpty: a folder removal refused because the folder holds
// something (RemoveEmptyDir).
var ErrDirNotEmpty = errors.New("folder not empty")

// dirNames are the resource names of the folder dir under container: each
// segment davName-mapped exactly like an object's folders. dir must name a
// folder below the container — never the container itself.
func dirNames(tenantID, container, dir string) ([]string, error) {
	trimmed := strings.Trim(dir, "/")
	if trimmed == "" {
		return nil, fmt.Errorf("%w: no folder below the container", engine.ErrInvalidInput)
	}
	names, err := tenantNames(tenantID, container)
	if err != nil {
		return nil, err
	}
	for _, seg := range strings.Split(trimmed, "/") {
		names = append(names, davName(seg))
	}
	return names, checkNames(names)
}

// RemoveEmptyDir removes the folder at dir under container when it holds
// nothing (the vault parity reconcile pass, after the shards under it are
// deleted); a missing folder is fine, a non-empty one is left and reported
// (ErrDirNotEmpty). One server: its view is the account's. Several bridges
// on one account: MultiWebDAVDriver.RemoveEmptyDir.
func (d *WebDAVDriver) RemoveEmptyDir(ctx context.Context, container, dir string) error {
	tenantID, err := requireTenant(ctx, d.name, "RemoveEmptyDir", "", d.logger)
	if err != nil {
		return err
	}
	names, err := dirNames(tenantID, container, dir)
	if err != nil {
		return fmt.Errorf("%s remove dir %s: %w", d.name, dir, err)
	}
	empty, err := d.emptyDir(ctx, names)
	if err != nil {
		return fmt.Errorf("%s remove dir %s: %w", d.name, dir, err)
	}
	if !empty {
		return fmt.Errorf("%s remove dir %s: %w", d.name, dir, ErrDirNotEmpty)
	}
	if err := d.removeDir(ctx, names); err != nil {
		return err
	}
	d.forgetCollections(d.parentPaths(append(names, "_")))
	return nil
}

// emptyDir: the folder at names holds nothing on this server (a missing
// folder is empty).
func (d *WebDAVDriver) emptyDir(ctx context.Context, names []string) (bool, error) {
	dirs, files, err := d.children(ctx, names)
	if err != nil {
		return false, err
	}
	return len(dirs)+len(files) == 0, nil
}

// ListDir lists the direct members of the folder dir under container ("" =
// the container): sub-folder names and object names, in key space (the
// inverse of the name mapping; a file without the leaf marker keeps its
// decoded name). A missing folder is empty. One PROPFIND.
func (d *WebDAVDriver) ListDir(ctx context.Context, container, dir string) (dirs, files []string, err error) {
	tenantID, err := requireTenant(ctx, d.name, "ListDir", "", d.logger)
	if err != nil {
		return nil, nil, err
	}
	names, err := listDirNames(tenantID, container, dir)
	if err != nil {
		return nil, nil, fmt.Errorf("%s list dir %s: %w", d.name, dir, err)
	}
	return d.listDirNames(ctx, names)
}

func listDirNames(tenantID, container, dir string) ([]string, error) {
	if strings.Trim(dir, "/") == "" {
		return tenantNames(tenantID, container)
	}
	return dirNames(tenantID, container, dir)
}

func (d *WebDAVDriver) listDirNames(ctx context.Context, names []string) (dirs, files []string, err error) {
	rd, rf, err := d.children(ctx, names)
	if err != nil {
		return nil, nil, fmt.Errorf("%s list dir: %w", d.name, err)
	}
	for _, n := range rd {
		dirs = append(dirs, keySegment(n))
	}
	for _, n := range rf {
		if leaf, ok := objectLeafSegment(n); ok {
			files = append(files, leaf)
			continue
		}
		files = append(files, keySegment(n))
	}
	return dirs, files, nil
}

// removeDir deletes the (empty or not) folder at resource names; a miss is
// not an error. Recursive on WebDAV: only for a folder every member of which
// is being deleted (a dead stripe generation), or once every bridge has
// listed it empty (RemoveEmptyDir, MultiWebDAVDriver.removeEmptyNames).
func (d *WebDAVDriver) removeDir(ctx context.Context, names []string) error {
	path := d.escapedPath(names, true)
	if err := d.deletePath(ctx, path); err != nil {
		return fmt.Errorf("%s delete %s: %w", d.name, path, err)
	}
	return nil
}

// removeNames deletes the file at resource names (below the root); a miss
// is not an error.
func (d *WebDAVDriver) removeNames(ctx context.Context, names []string) error {
	path := d.escapedPath(names, false)
	if err := d.deletePath(ctx, path); err != nil {
		return fmt.Errorf("%s delete %s: %w", d.name, path, err)
	}
	return nil
}

// tenantObject is the walk's view of a file at names (tenant folder first).
// It carries no modification time: a WebDAV server's (Sync's bridge's) is
// not to be trusted (see propfindBody).
func tenantObject(names []string, remove func(ctx context.Context) error) engine.TenantObject {
	rel := names[1:]
	obj := engine.TenantObject{Remove: remove}
	// The leaf: an object's carries the marker; a file without it (written
	// before the marker, or by something else) is still the tenant's and
	// is walked — the erasure sweep must remove it too.
	last := rel[len(rel)-1]
	leaf, ok := objectLeafSegment(last)
	if !ok {
		leaf = keySegment(last)
	}
	if len(rel) == 1 {
		obj.Artifact = leaf
		return obj
	}
	obj.Container = keySegment(rel[0])
	parts := make([]string, 0, len(rel)-1)
	for _, s := range rel[1 : len(rel)-1] {
		parts = append(parts, keySegment(s))
	}
	obj.Artifact = strings.Join(append(parts, leaf), "/")
	return obj
}

// HealthCheck is an authenticated Depth 0 PROPFIND of the root folder — a
// wrong password fails it with 401 (architecture decision 1: never a bare
// GET). A root folder not created yet (no object written) falls back to the
// server's own URL.
func (d *WebDAVDriver) HealthCheck(ctx context.Context) error {
	paths := []string{d.escapedPath(nil, true)}
	if len(d.rootSegs) > 0 {
		paths = append(paths, (&WebDAVDriver{baseSegs: d.baseSegs}).escapedPath(nil, true))
	}
	for _, p := range paths {
		_, found, err := d.propfind(ctx, p, "0")
		if err != nil {
			return fmt.Errorf("%s health check: %w", d.name, err)
		}
		if found {
			return nil
		}
	}
	return fmt.Errorf("%s health check: PROPFIND %s answered 404", d.name, paths[len(paths)-1])
}
