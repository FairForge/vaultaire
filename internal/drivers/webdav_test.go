package drivers

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"sort"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"golang.org/x/net/webdav"

	"github.com/FairForge/vaultaire/internal/common"
	"github.com/FairForge/vaultaire/internal/engine"
)

const (
	davTestUser = "sync"
	davTestPass = "generated-s3cret"
)

var (
	_ engine.Driver       = (*WebDAVDriver)(nil)
	_ engine.RangeGetter  = (*WebDAVDriver)(nil)
	_ engine.TenantWalker = (*WebDAVDriver)(nil)
	_ engine.KeyAddresser = (*WebDAVDriver)(nil)
)

// davFixture is an in-memory WebDAV server (golang.org/x/net/webdav) behind
// HTTP Basic auth — the shape of the Sync.com bridge — and a driver on it.
type davFixture struct {
	srv     *httptest.Server
	fs      webdav.FileSystem
	drv     *WebDAVDriver
	methods sync32 // requests seen, by method
}

type sync32 struct {
	mkcol, put, propfind, get, del atomic.Int32
}

// newDAVFixture starts the server. wrap (optional) sits between the auth
// check and the WebDAV handler, to misbehave on purpose.
func newDAVFixture(t *testing.T, wrap func(http.Handler) http.Handler) *davFixture {
	t.Helper()
	f := &davFixture{fs: webdav.NewMemFS()}
	var h http.Handler = &webdav.Handler{FileSystem: f.fs, LockSystem: webdav.NewMemLS()}
	if wrap != nil {
		h = wrap(h)
	}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, p, ok := r.BasicAuth()
		if !ok || u != davTestUser || p != davTestPass {
			w.Header().Set("WWW-Authenticate", `Basic realm="sync"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		switch r.Method {
		case "MKCOL":
			f.methods.mkcol.Add(1)
		case http.MethodPut:
			f.methods.put.Add(1)
		case "PROPFIND":
			f.methods.propfind.Add(1)
		case http.MethodGet:
			f.methods.get.Add(1)
		case http.MethodDelete:
			f.methods.del.Add(1)
		}
		h.ServeHTTP(w, r)
	}))
	t.Cleanup(f.srv.Close)
	drv, err := NewWebDAVDriver("sync", f.srv.URL, davTestUser, davTestPass, "vaultaire", zap.NewNop())
	require.NoError(t, err)
	f.drv = drv
	return f
}

// memFiles lists every file of an in-memory WebDAV file system.
func memFiles(t *testing.T, fs webdav.FileSystem, dir string) []string {
	t.Helper()
	f, err := fs.OpenFile(context.Background(), dir, os.O_RDONLY, 0)
	require.NoError(t, err)
	infos, err := f.Readdir(-1)
	mustClose(f)
	require.NoError(t, err)
	var out []string
	for _, fi := range infos {
		p := strings.TrimSuffix(dir, "/") + "/" + fi.Name()
		if fi.IsDir() {
			out = append(out, memFiles(t, fs, p)...)
		} else {
			out = append(out, p)
		}
	}
	return out
}

func davCtx(tenant string) context.Context {
	return common.WithTenantID(context.Background(), tenant)
}

// onlyReader hides every interface but io.Reader (no Seeker, no WriterTo,
// no Len): the driver must stream it.
type onlyReader struct{ r io.Reader }

func (o onlyReader) Read(p []byte) (int, error) { return o.r.Read(p) }

// patternReader produces n deterministic bytes without holding them.
type patternReader struct{ n, off int64 }

func (p *patternReader) Read(b []byte) (int, error) {
	if p.off >= p.n {
		return 0, io.EOF
	}
	if int64(len(b)) > p.n-p.off {
		b = b[:p.n-p.off]
	}
	for i := range b {
		b[i] = byte((p.off + int64(i)) * 31 % 251)
	}
	p.off += int64(len(b))
	return len(b), nil
}

func readAllClose(t *testing.T, rc io.ReadCloser) []byte {
	t.Helper()
	defer mustClose(rc)
	b, err := io.ReadAll(rc)
	require.NoError(t, err)
	return b
}

func TestNewWebDAVDriver_Validates(t *testing.T) {
	_, err := NewWebDAVDriver("", "http://127.0.0.1:4918", "u", "p", "", zap.NewNop())
	assert.Error(t, err, "a name is required")
	_, err = NewWebDAVDriver("sync", "", "u", "p", "", zap.NewNop())
	assert.Error(t, err, "a URL is required")
	_, err = NewWebDAVDriver("sync", "ftp://host/", "u", "p", "", zap.NewNop())
	assert.Error(t, err, "http(s) only")
	_, err = NewWebDAVDriver("sync", "http://u:pw@127.0.0.1:4918", "u", "p", "", zap.NewNop())
	assert.Error(t, err, "credentials in the URL would end up in error messages")
	_, err = NewWebDAVDriver("sync", "http://127.0.0.1:4918", "u", "", "", zap.NewNop())
	assert.Error(t, err, "a password is required")
	d, err := NewWebDAVDriver("sync", "http://127.0.0.1:4918/", "u", "p", "/vaultaire/", zap.NewNop())
	require.NoError(t, err)
	assert.Equal(t, "sync", d.Name())
}

func TestWebDAVDriver_PutGetRoundTrip(t *testing.T) {
	f := newDAVFixture(t, nil)
	ctx := davCtx("tenant-a")

	body := []byte("hello sync")
	require.NoError(t, f.drv.Put(ctx, "tenant-a_photos", "a.txt", bytes.NewReader(body),
		engine.WithContentLength(int64(len(body)))))

	got := readAllClose(t, mustGet(ctx, t, f.drv, "tenant-a_photos", "a.txt"))
	assert.Equal(t, body, got)

	// The object sits at <root>/t-<tenant>/<container>/<artifact>.
	fi, err := f.fs.Stat(context.Background(), "/vaultaire/t-tenant-a/tenant-a_photos/a.txt")
	require.NoError(t, err)
	assert.Equal(t, int64(len(body)), fi.Size())
}

func mustGet(ctx context.Context, t *testing.T, d *WebDAVDriver, c, a string) io.ReadCloser {
	t.Helper()
	rc, err := d.Get(ctx, c, a)
	require.NoError(t, err)
	return rc
}

func TestWebDAVDriver_PutStreamsLargeBody(t *testing.T) {
	f := newDAVFixture(t, nil)
	ctx := davCtx("tenant-a")
	const size = 6<<20 + 123

	hasher := sha256.New()
	src := io.TeeReader(&patternReader{n: size}, hasher)
	require.NoError(t, f.drv.Put(ctx, "c", "big.bin", onlyReader{src}, engine.WithContentLength(size)))
	want := hasher.Sum(nil)

	rc := mustGet(ctx, t, f.drv, "c", "big.bin")
	defer mustClose(rc)
	h2 := sha256.New()
	n, err := io.Copy(h2, rc)
	require.NoError(t, err)
	assert.Equal(t, int64(size), n)
	assert.Equal(t, want, h2.Sum(nil))

	// Unknown length (chunked transfer) streams too.
	hasher3 := sha256.New()
	src3 := io.TeeReader(&patternReader{n: 3 << 20}, hasher3)
	require.NoError(t, f.drv.Put(ctx, "c", "big-unknown.bin", onlyReader{src3}))
	got := readAllClose(t, mustGet(ctx, t, f.drv, "c", "big-unknown.bin"))
	sum := sha256.Sum256(got)
	assert.Equal(t, hasher3.Sum(nil), sum[:])
}

func TestWebDAVDriver_Overwrite(t *testing.T) {
	f := newDAVFixture(t, nil)
	ctx := davCtx("tenant-a")
	require.NoError(t, f.drv.Put(ctx, "c", "k", strings.NewReader("first version, longer")))
	require.NoError(t, f.drv.Put(ctx, "c", "k", strings.NewReader("second"), engine.WithContentLength(6)))
	assert.Equal(t, "second", string(readAllClose(t, mustGet(ctx, t, f.drv, "c", "k"))))
}

func TestWebDAVDriver_NestedKeysCreateCollectionsOnce(t *testing.T) {
	f := newDAVFixture(t, nil)
	ctx := davCtx("tenant-a")

	require.NoError(t, f.drv.Put(ctx, "c", "a/b/c/d.txt", strings.NewReader("x"), engine.WithContentLength(1)))
	assert.Equal(t, "x", string(readAllClose(t, mustGet(ctx, t, f.drv, "c", "a/b/c/d.txt"))))
	fi, err := f.fs.Stat(context.Background(), "/vaultaire/t-tenant-a/c/a/b")
	require.NoError(t, err)
	assert.True(t, fi.IsDir())

	// A second object in a known folder costs no MKCOL.
	before := f.methods.mkcol.Load()
	require.NoError(t, f.drv.Put(ctx, "c", "a/b/c/e.txt", strings.NewReader("y"), engine.WithContentLength(1)))
	assert.Equal(t, before, f.methods.mkcol.Load(), "known collections are cached")

	// A new leaf under a known parent costs one MKCOL.
	before = f.methods.mkcol.Load()
	require.NoError(t, f.drv.Put(ctx, "c", "a/b/z/f.txt", strings.NewReader("z"), engine.WithContentLength(1)))
	assert.Equal(t, before+1, f.methods.mkcol.Load())
}

func TestWebDAVDriver_RecreatesCollectionDeletedBehindItsBack(t *testing.T) {
	f := newDAVFixture(t, nil)
	ctx := davCtx("tenant-a")
	require.NoError(t, f.drv.Put(ctx, "c", "dir/one", strings.NewReader("1"), engine.WithContentLength(1)))
	// Someone removes the folder on the server; the cache still says it exists.
	require.NoError(t, f.fs.RemoveAll(context.Background(), "/vaultaire/t-tenant-a/c"))

	// A rewindable body is retried once after the MKCOLs.
	require.NoError(t, f.drv.Put(ctx, "c", "dir/two", strings.NewReader("2"), engine.WithContentLength(1)))
	assert.Equal(t, "2", string(readAllClose(t, mustGet(ctx, t, f.drv, "c", "dir/two"))))

	// A stream that was already sent cannot be retried: an error, never a
	// silently short object; the collections are recreated for the next call.
	require.NoError(t, f.fs.RemoveAll(context.Background(), "/vaultaire/t-tenant-a/c"))
	err := f.drv.Put(ctx, "c", "dir/three", onlyReader{strings.NewReader("3")}, engine.WithContentLength(1))
	require.Error(t, err)
	assert.ErrorIs(t, err, engine.ErrNoFailover)
	require.NoError(t, f.drv.Put(ctx, "c", "dir/three", onlyReader{strings.NewReader("3")}, engine.WithContentLength(1)))
}

func TestWebDAVDriver_SpecialCharacterKeys(t *testing.T) {
	f := newDAVFixture(t, nil)
	ctx := davCtx("tenant-a")
	keys := []string{
		"dir with space/ünïcødé ✓.txt",
		"100%+sure#frag?q=1&x=y.txt",
		"pct%2Fslash",
		"folder/",     // an S3 "folder" marker
		"folder/file", // and a file inside the same folder
		"a//b",        // empty segment
		"./dot",
		"x/../escape", // must not escape anything
		"..",
		"~tilde/~",
		"semi;colon,comma=eq@at$dollar",
	}
	for i, k := range keys {
		body := fmt.Sprintf("body-%d", i)
		require.NoError(t, f.drv.Put(ctx, "c", k, strings.NewReader(body), engine.WithContentLength(int64(len(body)))), k)
	}
	for i, k := range keys {
		assert.Equal(t, fmt.Sprintf("body-%d", i), string(readAllClose(t, mustGet(ctx, t, f.drv, "c", k))), k)
		ok, err := f.drv.Exists(ctx, "c", k)
		require.NoError(t, err)
		assert.True(t, ok, k)
	}
	listed, err := f.drv.List(ctx, "c", "")
	require.NoError(t, err)
	want := append([]string(nil), keys...)
	sort.Strings(want)
	assert.Equal(t, want, listed)

	// Nothing landed outside the tenant's container.
	var outside []string
	for _, p := range memFiles(t, f.fs, "/") {
		if !strings.HasPrefix(p, "/vaultaire/t-tenant-a/c/") {
			outside = append(outside, p)
		}
	}
	assert.Empty(t, outside)
}

func TestWebDAVDriver_GetRange(t *testing.T) {
	data := []byte("0123456789abcdefghij")
	for _, tc := range []struct {
		name        string
		ignoreRange bool
	}{
		{"server answers 206", false},
		{"server ignores Range and answers 200", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var sawRange atomic.Int32
			f := newDAVFixture(t, func(h http.Handler) http.Handler {
				return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.Method == http.MethodGet && r.Header.Get("Range") != "" {
						sawRange.Add(1)
						if tc.ignoreRange {
							r.Header.Del("Range")
						}
					}
					h.ServeHTTP(w, r)
				})
			})
			ctx := davCtx("tenant-a")
			require.NoError(t, f.drv.Put(ctx, "c", "r", bytes.NewReader(data), engine.WithContentLength(int64(len(data)))))

			rc, err := f.drv.GetRange(ctx, "c", "r", 5, 4)
			require.NoError(t, err)
			assert.Equal(t, "5678", string(readAllClose(t, rc)))

			rc, err = f.drv.GetRange(ctx, "c", "r", 15, 0) // to the end
			require.NoError(t, err)
			assert.Equal(t, "fghij", string(readAllClose(t, rc)))

			rc, err = f.drv.GetRange(ctx, "c", "r", 0, 100) // past the end = what there is
			require.NoError(t, err)
			assert.Equal(t, string(data), string(readAllClose(t, rc)))
			assert.Positive(t, sawRange.Load())
		})
	}
}

func TestWebDAVDriver_GetRangeShortObjectFromIgnoringServer(t *testing.T) {
	f := newDAVFixture(t, func(h http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			r.Header.Del("Range")
			h.ServeHTTP(w, r)
		})
	})
	ctx := davCtx("tenant-a")
	require.NoError(t, f.drv.Put(ctx, "c", "r", strings.NewReader("abc"), engine.WithContentLength(3)))
	rc, err := f.drv.GetRange(ctx, "c", "r", 10, 2)
	if err == nil {
		b, rerr := io.ReadAll(rc)
		mustClose(rc)
		assert.Empty(t, b, "never bytes from before the offset")
		_ = rerr
	}
}

func TestWebDAVDriver_NotFound(t *testing.T) {
	f := newDAVFixture(t, nil)
	ctx := davCtx("tenant-a")

	_, err := f.drv.Get(ctx, "c", "missing")
	require.Error(t, err)
	var nf engine.NotFoundError
	assert.True(t, errors.As(err, &nf), "a miss is the engine's NotFoundError: %v", err)

	_, err = f.drv.GetRange(ctx, "c", "missing", 0, 1)
	assert.True(t, errors.As(err, &nf), "GetRange too: %v", err)

	ok, err := f.drv.Exists(ctx, "c", "missing")
	require.NoError(t, err)
	assert.False(t, ok)
}

func TestWebDAVDriver_Delete(t *testing.T) {
	f := newDAVFixture(t, nil)
	ctx := davCtx("tenant-a")
	require.NoError(t, f.drv.Put(ctx, "c", "gone", strings.NewReader("x"), engine.WithContentLength(1)))
	require.NoError(t, f.drv.Delete(ctx, "c", "gone"))
	ok, err := f.drv.Exists(ctx, "c", "gone")
	require.NoError(t, err)
	assert.False(t, ok)

	assert.NoError(t, f.drv.Delete(ctx, "c", "gone"), "a delete of a missing object is not an error")
	assert.NoError(t, f.drv.Delete(ctx, "nope", "never"), "nor in a missing container")
}

// A WebDAV DELETE of a collection is recursive. S3's DeleteObject("a") must
// never take "a/b" with it.
func TestWebDAVDriver_DeleteNeverRemovesACollection(t *testing.T) {
	f := newDAVFixture(t, nil)
	ctx := davCtx("tenant-a")
	require.NoError(t, f.drv.Put(ctx, "c", "a/b", strings.NewReader("x"), engine.WithContentLength(1)))

	require.NoError(t, f.drv.Delete(ctx, "c", "a"))
	ok, err := f.drv.Exists(ctx, "c", "a/b")
	require.NoError(t, err)
	assert.True(t, ok, "a/b survives a delete of a")

	ok, err = f.drv.Exists(ctx, "c", "a")
	require.NoError(t, err)
	assert.False(t, ok, "a collection is not an object")
}

func TestWebDAVDriver_List(t *testing.T) {
	f := newDAVFixture(t, nil)
	ctx := davCtx("tenant-a")
	for _, k := range []string{"logs/2026/a.log", "logs/2026/b.log", "logs/2025/z.log", "logo.png", "readme"} {
		require.NoError(t, f.drv.Put(ctx, "c", k, strings.NewReader("x"), engine.WithContentLength(1)))
	}
	// An empty folder is not an object.
	require.NoError(t, f.fs.Mkdir(context.Background(), "/vaultaire/t-tenant-a/c/empty", 0o755))

	all, err := f.drv.List(ctx, "c", "")
	require.NoError(t, err)
	assert.Equal(t, []string{"logo.png", "logs/2025/z.log", "logs/2026/a.log", "logs/2026/b.log", "readme"}, all)

	logs, err := f.drv.List(ctx, "c", "logs/2026/")
	require.NoError(t, err)
	assert.Equal(t, []string{"logs/2026/a.log", "logs/2026/b.log"}, logs)

	lo, err := f.drv.List(ctx, "c", "lo")
	require.NoError(t, err)
	assert.Equal(t, []string{"logo.png", "logs/2025/z.log", "logs/2026/a.log", "logs/2026/b.log"}, lo)

	none, err := f.drv.List(ctx, "missing-container", "")
	require.NoError(t, err)
	assert.Empty(t, none)

	// Another tenant's container of the same name is another folder.
	other, err := f.drv.List(davCtx("tenant-b"), "c", "")
	require.NoError(t, err)
	assert.Empty(t, other)
}

// Servers answer hrefs in different shapes: absolute URLs, absolute paths,
// percent-encoding of characters that need none, collections without the
// trailing slash. The listing must not care.
func TestWebDAVDriver_ListToleratesHrefShapes(t *testing.T) {
	var base string
	f := newDAVFixture(t, func(h http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != "PROPFIND" {
				h.ServeHTTP(w, r)
				return
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, r)
			out := rec.Body.String()
			// Absolute URLs, "t" escaped needlessly, trailing slashes dropped.
			out = strings.ReplaceAll(out, "<D:href>/", "<D:href>"+base+"/")
			out = strings.ReplaceAll(out, "/<", "<")
			out = strings.ReplaceAll(out, "/vaultaire", "/vaul%74aire")
			for k, v := range rec.Header() {
				w.Header()[k] = v
			}
			w.Header().Del("Content-Length")
			w.WriteHeader(rec.Code)
			_, _ = io.WriteString(w, out)
		})
	})
	base = f.srv.URL
	ctx := davCtx("tenant-a")
	for _, k := range []string{"d/x y.txt", "top"} {
		require.NoError(t, f.drv.Put(ctx, "c", k, strings.NewReader("x"), engine.WithContentLength(1)))
	}
	all, err := f.drv.List(ctx, "c", "")
	require.NoError(t, err)
	assert.Equal(t, []string{"d/x y.txt", "top"}, all)
}

func TestWebDAVDriver_ListFailsOnForeignHref(t *testing.T) {
	f := newDAVFixture(t, func(h http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == "PROPFIND" && r.Header.Get("Depth") == "1" {
				w.Header().Set("Content-Type", "application/xml")
				w.WriteHeader(http.StatusMultiStatus)
				_, _ = io.WriteString(w, `<?xml version="1.0"?><D:multistatus xmlns:D="DAV:">`+
					`<D:response><D:href>/somewhere/else.txt</D:href><D:propstat><D:prop><D:getcontentlength>1</D:getcontentlength></D:prop><D:status>HTTP/1.1 200 OK</D:status></D:propstat></D:response>`+
					`</D:multistatus>`)
				return
			}
			h.ServeHTTP(w, r)
		})
	})
	_, err := f.drv.List(davCtx("tenant-a"), "c", "")
	assert.Error(t, err, "a listing that names something outside the folder asked for is an error")
}

func TestWebDAVDriver_WalkTenant(t *testing.T) {
	f := newDAVFixture(t, nil)
	for _, tc := range []struct{ tenant, container, key string }{
		{"tenant-a", "tenant-a_one", "x/1.txt"},
		{"tenant-a", "tenant-a_two", "2.txt"},
		{"tenant-a", "_global", "_chunks/abc"},
		{"tenant-ab", "tenant-ab_one", "nope.txt"},
		{"tenant-b", "tenant-b_one", "nope.txt"},
	} {
		require.NoError(t, f.drv.Put(davCtx(tc.tenant), tc.container, tc.key, strings.NewReader("x"), engine.WithContentLength(1)))
	}

	var seen []string
	var objs []engine.TenantObject
	require.NoError(t, f.drv.WalkTenant(context.Background(), "tenant-a", func(o engine.TenantObject) error {
		seen = append(seen, o.Container+"|"+o.Artifact)
		objs = append(objs, o)
		return nil
	}))
	sort.Strings(seen)
	assert.Equal(t, []string{"_global|_chunks/abc", "tenant-a_one|x/1.txt", "tenant-a_two|2.txt"}, seen)

	for _, o := range objs {
		require.NoError(t, o.Remove(context.Background()))
		require.NoError(t, o.Remove(context.Background()), "a second remove is a miss, not an error")
	}
	var after []string
	require.NoError(t, f.drv.WalkTenant(context.Background(), "tenant-a", func(o engine.TenantObject) error {
		after = append(after, o.Artifact)
		return nil
	}))
	assert.Empty(t, after)

	// The neighbours are untouched.
	ok, err := f.drv.Exists(davCtx("tenant-ab"), "tenant-ab_one", "nope.txt")
	require.NoError(t, err)
	assert.True(t, ok)

	// A tenant with nothing on the backend is an empty walk.
	require.NoError(t, f.drv.WalkTenant(context.Background(), "tenant-zzz", func(engine.TenantObject) error {
		t.Fatal("nothing to see")
		return nil
	}))

	// Bad ids are refused before any request.
	for _, bad := range []string{"", "a/b"} {
		err := f.drv.WalkTenant(context.Background(), bad, func(engine.TenantObject) error { return nil })
		assert.ErrorIs(t, err, ErrWalkTenantID)
	}

	// An error from fn stops the walk and is returned.
	stop := errors.New("stop")
	err = f.drv.WalkTenant(context.Background(), "tenant-b", func(engine.TenantObject) error { return stop })
	assert.ErrorIs(t, err, stop)
}

func TestWebDAVDriver_RefusesCallsWithoutTenant(t *testing.T) {
	f := newDAVFixture(t, nil)
	ctx := context.Background()

	err := f.drv.Put(ctx, "c", "k", strings.NewReader("x"))
	assert.ErrorIs(t, err, ErrNoTenant)
	_, err = f.drv.Get(ctx, "c", "k")
	assert.ErrorIs(t, err, ErrNoTenant)
	_, err = f.drv.GetRange(ctx, "c", "k", 0, 1)
	assert.ErrorIs(t, err, ErrNoTenant)
	assert.ErrorIs(t, f.drv.Delete(ctx, "c", "k"), ErrNoTenant)
	_, err = f.drv.Exists(ctx, "c", "k")
	assert.ErrorIs(t, err, ErrNoTenant)
	_, err = f.drv.List(ctx, "c", "")
	assert.ErrorIs(t, err, ErrNoTenant)
	assert.Zero(t, f.methods.put.Load()+f.methods.get.Load()+f.methods.del.Load()+f.methods.propfind.Load())

	// The chunk context addresses the one chunk prefix.
	cctx := engine.ChunkContext(ctx)
	require.NoError(t, f.drv.Put(cctx, engine.ChunkContainer, "_chunks/h1", strings.NewReader("c"), engine.WithContentLength(1)))
	_, statErr := f.fs.Stat(context.Background(), "/vaultaire/t-_global/_global/_chunks/h1")
	assert.NoError(t, statErr)
	assert.Equal(t, "t-_global/_global/_chunks/h1", f.drv.ObjectKey(cctx, engine.ChunkContainer, "_chunks/h1"))
}

func TestWebDAVDriver_HealthCheck(t *testing.T) {
	f := newDAVFixture(t, nil)
	assert.NoError(t, f.drv.HealthCheck(context.Background()), "the root folder need not exist yet")
	require.NoError(t, f.drv.Put(davCtx("t"), "c", "k", strings.NewReader("x"), engine.WithContentLength(1)))
	assert.NoError(t, f.drv.HealthCheck(context.Background()))
	assert.Positive(t, f.methods.propfind.Load(), "an authenticated PROPFIND, not a bare GET")

	wrong, err := NewWebDAVDriver("sync", f.srv.URL, davTestUser, "wrong-password", "vaultaire", zap.NewNop())
	require.NoError(t, err)
	err = wrong.HealthCheck(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "401")
	assert.NotContains(t, err.Error(), "wrong-password", "the password never appears in an error")

	down, err := NewWebDAVDriver("sync", "http://127.0.0.1:1", davTestUser, davTestPass, "vaultaire", zap.NewNop())
	require.NoError(t, err)
	assert.Error(t, down.HealthCheck(context.Background()))
}

func TestWebDAVDriver_DetectsTruncatedPut(t *testing.T) {
	f := newDAVFixture(t, func(h http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodPut && r.ContentLength > 1 {
				// The server keeps half of what it was sent and says 201.
				r.Body = io.NopCloser(io.LimitReader(r.Body, r.ContentLength/2))
			}
			h.ServeHTTP(w, r)
		})
	})
	ctx := davCtx("tenant-a")
	err := f.drv.Put(ctx, "c", "half", strings.NewReader("0123456789"), engine.WithContentLength(10))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "size")
}

func TestWebDAVDriver_ServerErrorsAreReturned(t *testing.T) {
	f := newDAVFixture(t, func(h http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "upstream gone", http.StatusBadGateway)
		})
	})
	ctx := davCtx("tenant-a")
	_, err := f.drv.Get(ctx, "c", "k")
	require.Error(t, err)
	var nf engine.NotFoundError
	assert.False(t, errors.As(err, &nf), "a 5xx is a backend failure, never a miss")
	assert.Contains(t, err.Error(), "502")
	assert.Error(t, f.drv.Delete(ctx, "c", "k"))
	_, err = f.drv.Exists(ctx, "c", "k")
	assert.Error(t, err)
	assert.Error(t, f.drv.Put(ctx, "c", "k", strings.NewReader("x"), engine.WithContentLength(1)))
}
