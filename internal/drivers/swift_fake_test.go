package drivers

import (
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeSwift is an in-process OpenStack Swift behind Keystone v2 with just
// what the swift driver uses, shaped on Blomp as probed live on 2026-10-21:
// one container per account (creating another is 403), static large
// objects whose segments live in the same container, `?multipart-manifest`
// put/get/delete (delete on a plain object answers 200 with "Not an SLO
// manifest" in its body), `?bulk-delete`, and 401 for a token it does not
// know.
type fakeSwift struct {
	t         *testing.T
	srv       *httptest.Server
	user      string
	password  string
	tenant    string
	container string

	mu        sync.Mutex
	objects   map[string]fakeSwiftObject // object name → object
	tokens    map[string]bool
	tokenN    int
	authCalls int
	expiresIn time.Duration
	calls     map[string]int // "METHOD kind" → count
	// fail[method] = statuses answered (in order) before the call is served.
	fail        map[string][]int
	ignoreRange bool
	// quotaBytes > 0: a PUT that would take the container past it answers
	// 413 "Upload exceeds quota." (Swift's container_quotas; Blomp's 40 GiB).
	quotaBytes int64
	// dropUsageHeaders: the container HEAD answers without
	// X-Container-Bytes-Used (a proxy that strips it).
	dropUsageHeaders bool
	// putDelay holds every segment PUT this long; inflight/maxInflight count
	// the segment PUTs being served at once.
	putDelay    time.Duration
	inflight    int
	maxInflight int
	// lagging: names the container listing does not show yet although they
	// exist (Blomp's listing hid ~30 objects for 90+ minutes, 2026-10-21).
	lagging map[string]bool
	// stall[method] = how many of the next calls of method are held — no
	// headers — until the client cancels them or release is closed (a
	// Blomp proxy's ~5 s first byte); stallCancelled counts the held calls
	// whose client went away.
	stall          map[string]int
	release        chan struct{}
	releaseOnce    sync.Once
	stallCancelled int
}

type fakeSwiftObject struct {
	data        []byte
	contentType string
	manifest    []fakeSwiftSegment // non-nil = a static large object
}

type fakeSwiftSegment struct {
	Path      string `json:"path"`
	Etag      string `json:"etag"`
	SizeBytes int64  `json:"size_bytes"`
}

func newFakeSwift(t *testing.T) *fakeSwift {
	f := &fakeSwift{t: t, user: "u@example.com", password: "pw", tenant: "storage", container: "u@example.com",
		objects: map[string]fakeSwiftObject{}, tokens: map[string]bool{}, calls: map[string]int{},
		fail: map[string][]int{}, expiresIn: time.Hour, stall: map[string]int{}, release: make(chan struct{})}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	t.Cleanup(f.releaseStalls) // runs first: a held call never blocks Close
	return f
}

// releaseStalls lets every held call go on.
func (f *fakeSwift) releaseStalls() { f.releaseOnce.Do(func() { close(f.release) }) }

func (f *fakeSwift) authURL() string { return f.srv.URL + "/v2.0" }

func (f *fakeSwift) config() SwiftConfig {
	return SwiftConfig{AuthURL: f.authURL(), User: f.user, Password: f.password, Tenant: f.tenant,
		Container: f.container, SegmentSize: 1024}
}

// failNext makes the next calls of method answer these statuses first.
func (f *fakeSwift) failNext(method string, statuses ...int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fail[method] = append(f.fail[method], statuses...)
}

// stallNext holds the next n calls of method (stall).
func (f *fakeSwift) stallNext(method string, n int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stall[method] += n
}

func (f *fakeSwift) cancelledStalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.stallCancelled
}

func (f *fakeSwift) peakSegmentPuts() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.maxInflight
}

func (f *fakeSwift) revokeTokens() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tokens = map[string]bool{}
}

func (f *fakeSwift) count(key string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[key]
}

func (f *fakeSwift) names() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, 0, len(f.objects))
	for n := range f.objects {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// overQuota: would a PUT of n bytes at name take the container past
// quotaBytes (the object it replaces is not counted; a manifest's bytes
// are its pieces', already stored)?
func (f *fakeSwift) overQuota(name string, n int64) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.quotaBytes <= 0 {
		return false
	}
	var used int64
	for k, o := range f.objects {
		if k != name && o.manifest == nil {
			used += int64(len(o.data))
		}
	}
	return used+n > f.quotaBytes
}

// lagListing makes the container listing leave name out (true) or show it.
func (f *fakeSwift) lagListing(name string, lags bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.lagging == nil {
		f.lagging = map[string]bool{}
	}
	f.lagging[name] = lags
}

func (f *fakeSwift) listingLags(name string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lagging[name]
}

func (f *fakeSwift) object(name string) (fakeSwiftObject, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	o, ok := f.objects[name]
	return o, ok
}

func (f *fakeSwift) serve(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/v2.0/tokens" && r.Method == http.MethodPost {
		f.serveAuth(w, r)
		return
	}
	acct := "/v1/AUTH_test"
	if !strings.HasPrefix(r.URL.Path, acct) {
		http.NotFound(w, r)
		return
	}
	f.mu.Lock()
	ok := f.tokens[r.Header.Get("X-Auth-Token")]
	f.mu.Unlock()
	if !ok {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	f.mu.Lock()
	if f.stall[r.Method] > 0 {
		f.stall[r.Method]--
		f.mu.Unlock()
		select {
		case <-r.Context().Done():
			f.mu.Lock()
			f.stallCancelled++
			f.mu.Unlock()
			return
		case <-f.release:
		}
		f.mu.Lock()
	}
	if q := f.fail[r.Method]; len(q) > 0 {
		f.fail[r.Method] = q[1:]
		f.mu.Unlock()
		_, _ = io.Copy(io.Discard, r.Body)
		w.WriteHeader(q[0])
		return
	}
	f.mu.Unlock()

	rest := strings.TrimPrefix(r.URL.Path, acct)
	if rest == "" || rest == "/" {
		if r.Method == http.MethodPost && r.URL.Query().Has("bulk-delete") {
			f.serveBulkDelete(w, r)
			return
		}
		w.WriteHeader(http.StatusForbidden) // account listing: Blomp refuses it
		return
	}
	rest = strings.TrimPrefix(rest, "/")
	container, name, _ := strings.Cut(rest, "/")
	if container != f.container {
		w.WriteHeader(http.StatusForbidden)
		return
	}
	if name == "" {
		f.serveContainer(w, r)
		return
	}
	f.serveObject(w, r, name)
}

func (f *fakeSwift) serveAuth(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Auth struct {
			TenantName          string `json:"tenantName"`
			PasswordCredentials struct {
				Username string `json:"username"`
				Password string `json:"password"`
			} `json:"passwordCredentials"`
		} `json:"auth"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.authCalls++
	c := req.Auth.PasswordCredentials
	if c.Username != f.user || c.Password != f.password || req.Auth.TenantName != f.tenant {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error": {"message": "The request you have made requires authentication.", "code": 401}}`)
		return
	}
	f.tokenN++
	tok := fmt.Sprintf("tok-%d", f.tokenN)
	f.tokens[tok] = true
	resp := map[string]any{"access": map[string]any{
		"token": map[string]any{"id": tok, "expires": time.Now().Add(f.expiresIn).UTC().Format("2006-01-02T15:04:05.000000Z")},
		"serviceCatalog": []any{
			map[string]any{"type": "compute", "name": "nova", "endpoints": []any{map[string]any{"publicURL": "http://compute.invalid/v2"}}},
			map[string]any{"type": "object-store", "name": "swift", "endpoints": []any{map[string]any{"region": "RegionOne", "publicURL": f.srv.URL + "/v1/AUTH_test"}}},
		},
	}}
	_ = json.NewEncoder(w).Encode(resp)
}

func (f *fakeSwift) serveContainer(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.calls[r.Method+" container"]++
	f.mu.Unlock()
	switch r.Method {
	case http.MethodHead:
		// Swift's container HEAD: the bytes and objects it holds, and
		// container_quotas' limit when one is set (Blomp: 40 GiB).
		f.mu.Lock()
		var used int64
		for _, o := range f.objects {
			used += int64(len(o.data))
		}
		w.Header().Set("X-Container-Object-Count", strconv.Itoa(len(f.objects)))
		w.Header().Set("X-Container-Bytes-Used", strconv.FormatInt(used, 10))
		if f.quotaBytes > 0 {
			w.Header().Set("X-Container-Meta-Quota-Bytes", strconv.FormatInt(f.quotaBytes, 10))
		}
		if f.dropUsageHeaders {
			w.Header().Del("X-Container-Bytes-Used")
		}
		f.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	case http.MethodGet:
		q := r.URL.Query()
		prefix, marker := q.Get("prefix"), q.Get("marker")
		limit := 10000
		if l := q.Get("limit"); l != "" {
			limit, _ = strconv.Atoi(l)
		}
		var out []map[string]any
		for _, n := range f.names() {
			if !strings.HasPrefix(n, prefix) || n <= marker || f.listingLags(n) {
				continue
			}
			if len(out) == limit {
				break
			}
			o, _ := f.object(n)
			out = append(out, map[string]any{"name": n, "bytes": len(o.data), "hash": md5hex(o.data)})
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(out)
	default:
		w.WriteHeader(http.StatusForbidden)
	}
}

func (f *fakeSwift) serveObject(w http.ResponseWriter, r *http.Request, name string) {
	q := r.URL.Query()
	mm := q.Get("multipart-manifest")
	f.mu.Lock()
	kind := "object"
	if mm != "" {
		kind = "manifest-" + mm
	}
	f.calls[r.Method+" "+kind]++
	f.mu.Unlock()
	switch r.Method {
	case http.MethodPut:
		if strings.Contains(name, "/_segments/") {
			f.mu.Lock()
			f.inflight++
			f.maxInflight = max(f.maxInflight, f.inflight)
			delay := f.putDelay
			f.mu.Unlock()
			defer func() {
				f.mu.Lock()
				f.inflight--
				f.mu.Unlock()
			}()
			time.Sleep(delay)
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if f.overQuota(name, int64(len(body))) {
			w.WriteHeader(http.StatusRequestEntityTooLarge)
			_, _ = io.WriteString(w, "Upload exceeds quota.")
			return
		}
		if mm == "put" {
			var segs []fakeSwiftSegment
			if err := json.Unmarshal(body, &segs); err != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			var all []byte
			for _, s := range segs {
				segName := strings.TrimPrefix(s.Path, "/"+url.PathEscape(f.container)+"/")
				seg, ok := f.object(segName)
				if !ok || md5hex(seg.data) != s.Etag || int64(len(seg.data)) != s.SizeBytes {
					w.WriteHeader(http.StatusBadRequest)
					_, _ = fmt.Fprintf(w, "bad segment %s", s.Path)
					return
				}
				all = append(all, seg.data...)
			}
			f.mu.Lock()
			f.objects[name] = fakeSwiftObject{data: all, contentType: r.Header.Get("Content-Type"), manifest: segs}
			f.mu.Unlock()
			w.Header().Set("Etag", `"`+md5hex(all)+`"`)
			w.WriteHeader(http.StatusCreated)
			return
		}
		if r.ContentLength >= 0 && int64(len(body)) != r.ContentLength {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		f.mu.Lock()
		f.objects[name] = fakeSwiftObject{data: body, contentType: r.Header.Get("Content-Type")}
		f.mu.Unlock()
		w.Header().Set("Etag", md5hex(body))
		w.WriteHeader(http.StatusCreated)
	case http.MethodHead, http.MethodGet:
		o, ok := f.object(name)
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if r.Method == http.MethodGet && mm == "get" && o.manifest != nil {
			// Swift's listing shape of a manifest (format=raw is the PUT shape).
			type entry struct {
				Name  string `json:"name"`
				Hash  string `json:"hash"`
				Bytes int64  `json:"bytes"`
			}
			out := make([]entry, 0, len(o.manifest))
			for _, s := range o.manifest {
				p, _ := url.PathUnescape(s.Path)
				out = append(out, entry{Name: p, Hash: s.Etag, Bytes: s.SizeBytes})
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(out)
			return
		}
		if o.manifest != nil {
			w.Header().Set("X-Static-Large-Object", "True")
		}
		w.Header().Set("Etag", md5hex(o.data))
		w.Header().Set("Accept-Ranges", "bytes")
		data := o.data
		status := http.StatusOK
		if rg := r.Header.Get("Range"); rg != "" && r.Method == http.MethodGet && !f.ignoreRange {
			var a, b int64
			if _, err := fmt.Sscanf(rg, "bytes=%d-%d", &a, &b); err != nil || a >= int64(len(data)) {
				w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
				return
			}
			if b >= int64(len(data)) {
				b = int64(len(data)) - 1
			}
			w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", a, b, len(data)))
			data, status = data[a:b+1], http.StatusPartialContent
		}
		w.Header().Set("Content-Length", strconv.Itoa(len(data)))
		w.WriteHeader(status)
		if r.Method == http.MethodGet {
			_, _ = w.Write(data)
		}
	case http.MethodDelete:
		o, ok := f.object(name)
		if mm == "delete" {
			w.Header().Set("Content-Type", "application/json")
			res := map[string]any{"Number Deleted": 0, "Number Not Found": 0, "Errors": []any{}, "Response Status": "200 OK", "Response Body": ""}
			switch {
			case !ok:
				res["Number Not Found"] = 1
			case o.manifest == nil:
				res["Response Status"] = "400 Bad Request"
				res["Errors"] = []any{[]any{"/" + f.container + "/" + name, "Not an SLO manifest"}}
			default:
				f.mu.Lock()
				for _, s := range o.manifest {
					delete(f.objects, strings.TrimPrefix(s.Path, "/"+url.PathEscape(f.container)+"/"))
				}
				delete(f.objects, name)
				f.mu.Unlock()
				res["Number Deleted"] = len(o.manifest) + 1
			}
			_ = json.NewEncoder(w).Encode(res)
			return
		}
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		f.mu.Lock()
		delete(f.objects, name)
		f.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (f *fakeSwift) serveBulkDelete(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.calls["POST bulk-delete"]++
	f.mu.Unlock()
	body, _ := io.ReadAll(r.Body)
	deleted, notFound := 0, 0
	for _, line := range strings.Split(strings.TrimSpace(string(body)), "\n") {
		if line == "" {
			continue
		}
		p, err := url.PathUnescape(strings.TrimPrefix(line, "/"))
		if err != nil {
			continue
		}
		_, name, _ := strings.Cut(p, "/")
		f.mu.Lock()
		if _, ok := f.objects[name]; ok {
			delete(f.objects, name)
			deleted++
		} else {
			notFound++
		}
		f.mu.Unlock()
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"Number Deleted": deleted, "Number Not Found": notFound,
		"Errors": []any{}, "Response Status": "200 OK"})
}

func md5hex(b []byte) string {
	s := md5.Sum(b)
	return hex.EncodeToString(s[:])
}
