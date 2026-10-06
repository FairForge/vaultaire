package drivers

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/FairForge/vaultaire/internal/engine"
)

// WebDAVDriver stores objects on a WebDAV server (RFC 4918) — generic, so the
// same driver serves Sync.com's local encrypted bridge (`sync-webdav`, the
// first instance, registered as "sync"), Hetzner Storage Box, Koofr, pCloud
// and Nextcloud. See webdav_README.md.
//
// Layout is the fixed-bucket shape of iDrive/R2/Geyser under a configured
// root folder: `<root>/t-<tenant>/<container>/<artifact…>`, the tenant taken
// from the call's context (a call without one is refused, tenant_ctx.go).
// Every key segment is a WebDAV path segment, so S3 keys that cannot be one
// are mapped reversibly (davName): "" (a trailing `/`, an S3 folder marker,
// or `a//b`), "." and ".." would otherwise be normalised away by the server —
// ".." would climb out of the tenant's folder — and a segment that starts
// with "~" gets one more, which keeps the mapping one-to-one.
//
// Known limit: WebDAV cannot hold a file `a` and a folder `a` side by side,
// so an S3 key `a` and a key `a/b` in the same container conflict (the second
// PUT fails); S3 allows both.
type WebDAVDriver struct {
	name     string
	origin   string   // scheme://host[:port]
	baseSegs []string // decoded path segments of the URL's own path
	rootSegs []string // the configured root folder, below the URL's path
	username string
	password string
	client   *http.Client
	logger   *zap.Logger

	// known holds collection paths (escaped, no trailing slash) this process
	// has created or seen, so a PUT into a known folder costs no MKCOL.
	known sync.Map
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
)

// WebDAVConfig is one WebDAV backend's settings.
type WebDAVConfig struct {
	URL, User, Password, Root string
}

// Defaults of the `sync` instance: Sync.com's `sync-webdav` bridge listens on
// localhost with the user `sync` and a generated password.
const (
	SyncWebDAVDefaultURL  = "http://127.0.0.1:4918"
	SyncWebDAVDefaultUser = "sync"
	SyncWebDAVDefaultRoot = "vaultaire"
)

// SyncWebDAVConfigFromEnv reads the `sync` instance's settings:
// SYNC_WEBDAV_PASSWORD (required — ok is false without it), SYNC_WEBDAV_URL,
// SYNC_WEBDAV_USER, SYNC_WEBDAV_ROOT.
func SyncWebDAVConfigFromEnv(getenv func(string) string) (WebDAVConfig, bool) {
	c := WebDAVConfig{
		URL:      getenv("SYNC_WEBDAV_URL"),
		User:     getenv("SYNC_WEBDAV_USER"),
		Password: getenv("SYNC_WEBDAV_PASSWORD"),
		Root:     getenv("SYNC_WEBDAV_ROOT"),
	}
	if c.Password == "" {
		return WebDAVConfig{}, false
	}
	if c.URL == "" {
		c.URL = SyncWebDAVDefaultURL
	}
	if c.User == "" {
		c.User = SyncWebDAVDefaultUser
	}
	if c.Root == "" {
		c.Root = SyncWebDAVDefaultRoot
	}
	return c, true
}

// NewWebDAVDriver builds a driver. name is the engine's backend name (the
// metrics and errors carry it); baseURL is the WebDAV root the server
// exposes (e.g. http://127.0.0.1:4918 for the Sync bridge, or
// https://uXXXX.your-storagebox.de); root is the folder under it objects
// live in ("" = the URL's own path). Credentials go in username/password,
// never in the URL: a URL lands in error messages.
func NewWebDAVDriver(name, baseURL, username, password, root string, logger *zap.Logger) (*WebDAVDriver, error) {
	if strings.TrimSpace(name) == "" {
		return nil, errors.New("webdav: a backend name is required")
	}
	if strings.TrimSpace(baseURL) == "" {
		return nil, fmt.Errorf("webdav %s: a URL is required", name)
	}
	u, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil {
		return nil, fmt.Errorf("webdav %s: parse URL: %w", name, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("webdav %s: URL scheme must be http or https, got %q", name, u.Scheme)
	}
	if u.Host == "" {
		return nil, fmt.Errorf("webdav %s: URL has no host", name)
	}
	if u.User != nil {
		return nil, fmt.Errorf("webdav %s: put the credentials in the username/password settings, not in the URL", name)
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return nil, fmt.Errorf("webdav %s: URL must not carry a query or fragment", name)
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

	d := &WebDAVDriver{
		name:     name,
		origin:   u.Scheme + "://" + u.Host,
		baseSegs: baseSegs,
		rootSegs: rootSegs,
		username: username,
		password: password,
		// HTTP/1.1 like the other drivers: one h2 connection funnels every
		// upload to a remote server (Storage Box, Koofr) through one TCP
		// stream. Honours VAULTAIRE_TUNED_TRANSPORT.
		client: TunedHTTPClient(WithHTTP1Only(), WithResponseHeaderTimeout(webdavResponseHeaderTimeout)),
		logger: logger,
	}
	logger.Info("WebDAV driver initialized",
		zap.String("backend", name),
		zap.String("url", d.origin+d.escapedPath(nil, false)),
		zap.String("user", username))
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

// davName maps one key segment onto a WebDAV resource name, reversibly:
// "" → "~", "." → "~.", ".." → "~..", "~x" → "~~x"; everything else is
// itself. The server never sees an empty, "." or ".." segment.
func davName(seg string) string {
	switch {
	case seg == "":
		return "~"
	case seg == "." || seg == "..":
		return "~" + seg
	case strings.HasPrefix(seg, "~"):
		return "~" + seg
	}
	return seg
}

// keySegment is davName's inverse.
func keySegment(name string) string {
	if name == "~" {
		return ""
	}
	return strings.TrimPrefix(name, "~")
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

// objectNames are the resource names of an object, root excluded.
func objectNames(tenantID, container, artifact string) ([]string, error) {
	names, err := tenantNames(tenantID, container)
	if err != nil {
		return nil, err
	}
	for _, s := range strings.Split(artifact, "/") {
		names = append(names, davName(s))
	}
	return names, nil
}

// object resolves a call to its tenant key (for errors) and resource names.
func (d *WebDAVDriver) object(ctx context.Context, op, container, artifact string) (string, []string, error) {
	tenantID, err := requireTenant(ctx, d.name, op, "", d.logger)
	if err != nil {
		return "", nil, err
	}
	names, err := objectNames(tenantID, container, artifact)
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

// request sends one authenticated request to an escaped path.
func (d *WebDAVDriver) request(ctx context.Context, method, path string, body io.Reader, length int64, header map[string]string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, d.origin+path, body)
	if err != nil {
		return nil, fmt.Errorf("build %s request: %w", method, err)
	}
	if length > 0 {
		req.ContentLength = length
	}
	req.SetBasicAuth(d.username, d.password)
	for k, v := range header {
		req.Header.Set(k, v)
	}
	return d.client.Do(req)
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
	msg := strings.TrimSpace(string(excerpt))
	if msg != "" {
		return fmt.Errorf("unexpected status %s: %s", resp.Status, msg)
	}
	return fmt.Errorf("unexpected status %s", resp.Status)
}

// --- PROPFIND -------------------------------------------------------------

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
	ctx, cancel := context.WithTimeout(ctx, webdavMetaTimeout)
	defer cancel()
	resp, err := d.request(ctx, "PROPFIND", path, strings.NewReader(propfindBody), 0, map[string]string{
		"Depth":        depth,
		"Content-Type": "application/xml; charset=utf-8",
	})
	if err != nil {
		return nil, false, err
	}
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
	for _, r := range ms.Responses {
		e, err := parseDavEntry(r)
		if err != nil {
			return nil, true, err
		}
		entries = append(entries, e)
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

func (d *WebDAVDriver) mkcol(ctx context.Context, path string) (int, error) {
	ctx, cancel := context.WithTimeout(ctx, webdavMetaTimeout)
	defer cancel()
	resp, err := d.request(ctx, "MKCOL", path+"/", nil, 0, nil)
	if err != nil {
		return 0, err
	}
	switch resp.StatusCode {
	case http.StatusCreated, http.StatusOK, http.StatusNoContent, http.StatusMethodNotAllowed:
		// 405 = it already exists (RFC 4918 §9.3.1).
		drainClose(resp)
		return resp.StatusCode, nil
	case http.StatusConflict, http.StatusNotFound:
		drainClose(resp)
		return resp.StatusCode, nil
	default:
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
			return fmt.Errorf("MKCOL %s: the server answered %d with every parent created", p, code)
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

// Put streams the object to the server (never buffered). The folders it
// needs are created first (cached). A PUT answered 404/409 — a folder
// removed behind the cache — recreates them and is retried once when the
// body can be rewound (or was not read at all). With a known length the
// stored size is checked afterwards: a server that kept fewer bytes than it
// was sent (a full disk on a bridge's spill directory) is an error, never a
// silently short object. Overwrites replace.
func (d *WebDAVDriver) Put(ctx context.Context, container, artifact string, data io.Reader, opts ...engine.PutOption) error {
	key, names, err := d.object(ctx, "Put", container, artifact)
	if err != nil {
		return err
	}
	o := engine.ApplyPutOptions(opts...)
	dirs := d.parentPaths(names)
	path := d.escapedPath(names, false)
	if err := d.ensureCollections(ctx, dirs); err != nil {
		return fmt.Errorf("%s put %s: %w", d.name, key, err)
	}

	var start int64 = -1
	if s, ok := data.(io.Seeker); ok {
		if pos, serr := s.Seek(0, io.SeekCurrent); serr == nil {
			start = pos
		}
	}
	body := &countingReader{Reader: data}
	code, err := d.put(ctx, path, body, o)
	if err != nil {
		return fmt.Errorf("%s put %s: %w", d.name, key, err)
	}
	if code == http.StatusConflict || code == http.StatusNotFound {
		d.forgetCollections(dirs)
		if err := d.ensureCollections(ctx, dirs); err != nil {
			return fmt.Errorf("%s put %s: recreate folders: %w", d.name, key, err)
		}
		if body.n > 0 {
			s, ok := data.(io.Seeker)
			if !ok || start < 0 {
				return fmt.Errorf("%s put %s: the server answered %d after the body was sent and it cannot be rewound: %w",
					d.name, key, code, engine.ErrNoFailover)
			}
			if _, serr := s.Seek(start, io.SeekStart); serr != nil {
				return fmt.Errorf("%s put %s: rewind: %w: %w", d.name, key, serr, engine.ErrNoFailover)
			}
		}
		body = &countingReader{Reader: data}
		code, err = d.put(ctx, path, body, o)
		if err != nil {
			return fmt.Errorf("%s put %s (retry): %w", d.name, key, err)
		}
		if code == http.StatusConflict || code == http.StatusNotFound {
			return fmt.Errorf("%s put %s: the server answered %d with every folder created", d.name, key, code)
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

// put sends one PUT. A 404/409 is returned as a code, not an error, so the
// caller can create the folders; any other non-2xx is an error.
func (d *WebDAVDriver) put(ctx context.Context, path string, body *countingReader, o engine.PutOptions) (int, error) {
	header := map[string]string{"Content-Type": "application/octet-stream"}
	if o.ContentType != "" {
		header["Content-Type"] = o.ContentType
	}
	// ContentLength 0 = unknown: chunked transfer encoding. (A known empty
	// body is indistinguishable from "unknown" in PutOptions; chunked
	// carries zero bytes just as well.)
	resp, err := d.request(ctx, http.MethodPut, path, body, o.ContentLength, header)
	if err != nil {
		return 0, err
	}
	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		drainClose(resp)
		return resp.StatusCode, nil
	case resp.StatusCode == http.StatusConflict || resp.StatusCode == http.StatusNotFound:
		drainClose(resp)
		return resp.StatusCode, nil
	}
	return resp.StatusCode, statusError(resp)
}

// Get streams the object. A 404 is the engine's NotFoundError.
func (d *WebDAVDriver) Get(ctx context.Context, container, artifact string) (io.ReadCloser, error) {
	key, names, err := d.object(ctx, "Get", container, artifact)
	if err != nil {
		return nil, err
	}
	resp, err := d.request(ctx, http.MethodGet, d.escapedPath(names, false), nil, 0, nil)
	if err != nil {
		return nil, fmt.Errorf("%s get %s: %w", d.name, key, err)
	}
	switch resp.StatusCode {
	case http.StatusOK:
		return resp.Body, nil
	case http.StatusNotFound:
		drainClose(resp)
		return nil, fmt.Errorf("%s get %s: %w", d.name, key, engine.ErrNotFound(container, artifact))
	}
	return nil, fmt.Errorf("%s get %s: %w", d.name, key, statusError(resp))
}

// GetRange implements engine.RangeGetter. length <= 0 = to the end. A server
// that ignores Range and answers 200 has the bytes before the offset
// discarded and the rest limited to length — never the wrong bytes.
func (d *WebDAVDriver) GetRange(ctx context.Context, container, artifact string, offset, length int64) (io.ReadCloser, error) {
	key, names, err := d.object(ctx, "GetRange", container, artifact)
	if err != nil {
		return nil, err
	}
	if offset < 0 {
		return nil, fmt.Errorf("%s get range %s: %w: negative offset", d.name, key, engine.ErrInvalidInput)
	}
	rng := fmt.Sprintf("bytes=%d-", offset)
	if length > 0 {
		rng = fmt.Sprintf("bytes=%d-%d", offset, offset+length-1)
	}
	resp, err := d.request(ctx, http.MethodGet, d.escapedPath(names, false), nil, 0, map[string]string{"Range": rng})
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
		return nil, fmt.Errorf("%s get range %s: %w", d.name, key, engine.ErrNotFound(container, artifact))
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
	ctx, cancel := context.WithTimeout(ctx, webdavMetaTimeout)
	defer cancel()
	resp, err := d.request(ctx, http.MethodDelete, path, nil, 0, nil)
	if err != nil {
		return err
	}
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
	keyOf := func(rel []string) string {
		segs := make([]string, len(rel))
		for i, s := range rel {
			segs[i] = keySegment(s)
		}
		return strings.Join(segs, "/")
	}
	var out []string
	err = d.walk(ctx, names,
		func(rel []string) bool {
			dir := keyOf(rel) + "/"
			return strings.HasPrefix(dir, prefix) || strings.HasPrefix(prefix, dir)
		},
		func(rel []string, _ string) error {
			if k := keyOf(rel); strings.HasPrefix(k, prefix) {
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
	tenantDir := []string{davName("t-" + tenantID)}
	err := d.walk(ctx, tenantDir, nil, func(rel []string, path string) error {
		obj := engine.TenantObject{
			Remove: func(ctx context.Context) error {
				if err := d.deletePath(ctx, path); err != nil {
					return fmt.Errorf("%s delete %s: %w", d.name, path, err)
				}
				return nil
			},
		}
		if len(rel) == 1 {
			obj.Artifact = keySegment(rel[0])
		} else {
			obj.Container = keySegment(rel[0])
			parts := make([]string, len(rel)-1)
			for i, s := range rel[1:] {
				parts[i] = keySegment(s)
			}
			obj.Artifact = strings.Join(parts, "/")
		}
		return fn(obj)
	})
	if err != nil {
		return fmt.Errorf("%s walk t-%s/: %w", d.name, tenantID, err)
	}
	return nil
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
