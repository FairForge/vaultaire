package drivers

import (
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"unicode/utf16"

	"github.com/FairForge/vaultaire/internal/engine"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// Sync.com's bridge name rules as measured on prod's bridges (2026-10-07:
// raw PUT through one bridge, GET through the four others 150 s later).
// This is an independent statement of the rules — not of the mapping — so
// the tests below prove the mapping never sends a name the bridge refuses
// or silently drops.
var syncDeviceNames = map[string]bool{"CON": true, "PRN": true, "AUX": true, "NUL": true}

func init() {
	for i := 0; i <= 9; i++ {
		syncDeviceNames[fmt.Sprintf("COM%d", i)] = true
		syncDeviceNames[fmt.Sprintf("LPT%d", i)] = true
	}
}

// syncRefuses: the bridge answers 400.
func syncRefuses(name string) bool {
	if len(utf16.Encode([]rune(name))) > 248 {
		return true
	}
	if strings.ContainsAny(name, `:?*"<>|\`) {
		return true
	}
	for _, r := range name {
		if (r < 0x20 && r != '\t') || r == 0x7f {
			return true
		}
	}
	if strings.HasPrefix(name, " ") || strings.HasPrefix(name, "~") ||
		strings.HasSuffix(name, ".") || strings.HasSuffix(name, " ") {
		return true
	}
	switch strings.ToLower(name) {
	case "desktop.ini", "thumbs.db":
		return true
	}
	stem, _, _ := strings.Cut(name, ".")
	return syncDeviceNames[strings.ToUpper(strings.TrimRight(stem, " "))]
}

// syncDiscards: the bridge answers 201 and keeps nothing.
func syncDiscards(name string) bool { return strings.EqualFold(name, ".DS_Store") }

// syncRules wraps the fixture's handler with the bridge's rules on every
// path segment below the server root; counts what it had to refuse/drop.
func syncRules(refused, discarded *atomic.Int32) func(http.Handler) http.Handler {
	return func(h http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			segs := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
			for _, s := range segs {
				if s != "" && syncRefuses(s) {
					refused.Add(1)
					http.Error(w, "", http.StatusBadRequest)
					return
				}
			}
			if r.Method == http.MethodPut && syncDiscards(segs[len(segs)-1]) {
				discarded.Add(1)
				_, _ = io.Copy(io.Discard, r.Body)
				w.WriteHeader(http.StatusCreated)
				return
			}
			h.ServeHTTP(w, r)
		})
	}
}

// The measured names, plus the shapes the old '~' scheme was for.
var syncHostileSegments = []string{
	"", ".", "..", "~", "~tilde", "~$office.docx", " lead", "trail.", "trail ", ". ",
	"colon:a", "q?x", "star*", `quote"q`, "lt<gt", "gt>x", "pipe|p", `back\slash`, "icon\r", "del\x7f",
	"tab\tx", "100%", "%41", "%", "%%", "%2E", "%7E",
	".DS_Store", ".ds_store", "desktop.ini", "Desktop.INI", "Thumbs.db", "thumbs.db.x",
	"CON", "con", "con.txt", "NUL", "nul.txt", "COM1", "LPT9", "aux.c", "PRN", "CON .txt",
	"case", "CASE", "üñíçødé 😀", ".hidden", "plain.jpg",
}

func TestDavName_MapsSyncHostileNames(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"plain.jpg", "plain.jpg"},
		{"photos", "photos"},
		{"t-tenant-14a623b16b3f7012", "t-tenant-14a623b16b3f7012"},
		{"tenant-14a623b16b3f7012_sync-lab", "tenant-14a623b16b3f7012_sync-lab"},
		{"üñíçødé 😀", "üñíçødé 😀"},
		{"tab\tx", "tab\tx"},
		{".hidden", ".hidden"},
		{"thumbs.db.x", "thumbs.db.x"},
		{"inner~tilde", "inner~tilde"},
		{"", "%"},
		{".", "%2E"},
		{"..", ".%2E"},
		{"~", "%7E"},
		{"~tilde", "%7Etilde"},
		{"100%", "100%25"},
		{"%", "%25"},
		{"colon:a", "colon%3Aa"},
		{`a\b`, "a%5Cb"},
		{"icon\r", "icon%0D"},
		{" lead", "%20lead"},
		{"trail.", "trail%2E"},
		{"trail ", "trail%20"},
		{".DS_Store", "%2EDS_Store"},
		{"desktop.ini", "%64esktop.ini"},
		{"Thumbs.db", "%54humbs.db"},
		{"CON", "%43ON"},
		{"con.txt", "%63on.txt"},
		{"LPT9", "%4CPT9"},
	} {
		assert.Equal(t, tc.want, davName(tc.in), "%q", tc.in)
		assert.Equal(t, tc.in, keySegment(tc.want), "%q", tc.want)
	}
}

// Every segment round-trips, the mapping is one-to-one, and no mapped name
// is one Sync refuses or drops.
func TestDavName_RoundTripsAndIsAlwaysSyncSafe(t *testing.T) {
	alphabet := []rune("aZ09.-_ ~%:?*\"<>|\\\t\r\x7f$#&+;=@^éü😀")
	words := []string{"con", "CON", "aux", "lpt1", "desktop.ini", "thumbs.db", ".ds_store", ".DS_Store", "nul"}
	rnd := rand.New(rand.NewSource(1))
	segs := append([]string(nil), syncHostileSegments...)
	for i := 0; i < 20000; i++ {
		var b strings.Builder
		if rnd.Intn(4) == 0 {
			b.WriteString(words[rnd.Intn(len(words))])
		}
		for n := rnd.Intn(8); n > 0; n-- {
			b.WriteRune(alphabet[rnd.Intn(len(alphabet))])
		}
		segs = append(segs, b.String())
	}
	seen := map[string]string{}
	for _, s := range segs {
		m := davName(s)
		require.Equal(t, s, keySegment(m), "round trip of %q via %q", s, m)
		assert.False(t, syncRefuses(m), "%q maps to %q, which Sync refuses", s, m)
		assert.False(t, syncDiscards(m), "%q maps to %q, which Sync drops", s, m)
		assert.NotContains(t, []string{"", ".", ".."}, m, "%q", s)
		if prev, ok := seen[m]; ok {
			require.Equal(t, prev, s, "%q and %q both map to %q", prev, s, m)
		}
		seen[m] = s
	}
}

// Through the driver against a server that behaves like the bridge: every
// hostile key is stored, read back, listed and walked under its own name,
// and the bridge never had to refuse or drop a request.
func TestWebDAVDriver_SyncNameRules(t *testing.T) {
	var refused, discarded atomic.Int32
	f := newDAVFixture(t, syncRules(&refused, &discarded))
	ctx := davCtx("tenant-a")
	var keys []string
	for i, s := range syncHostileSegments {
		keys = append(keys, fmt.Sprintf("d%02d/%s", i, s), fmt.Sprintf("%s/f%02d", s, i))
	}
	keys = append(keys, "photos/", "photos/x.jpg", "a//b", strings.Repeat("é", 246), strings.Repeat("é", 248)+"/f")
	for i, k := range keys {
		body := fmt.Sprintf("body-%d", i)
		require.NoError(t, f.drv.Put(ctx, "c", k, strings.NewReader(body), engine.WithContentLength(int64(len(body)))), "%q", k)
	}
	for i, k := range keys {
		assert.Equal(t, fmt.Sprintf("body-%d", i), string(readAllClose(t, mustGet(ctx, t, f.drv, "c", k))), "%q", k)
	}
	listed, err := f.drv.List(ctx, "c", "")
	require.NoError(t, err)
	want := append([]string(nil), keys...)
	sort.Strings(want)
	assert.Equal(t, want, listed)

	var walked []string
	require.NoError(t, f.drv.WalkTenant(ctx, "tenant-a", func(o engine.TenantObject) error {
		walked = append(walked, o.Artifact)
		return nil
	}))
	sort.Strings(walked)
	assert.Equal(t, want, walked)
	assert.Zero(t, refused.Load(), "the bridge refused a name the driver sent")
	assert.Zero(t, discarded.Load(), "the bridge dropped a file the driver sent")
}

// A segment longer than the bridge takes (248 UTF-16 units once mapped,
// the leaf marker included) is invalid input before any request.
func TestWebDAVDriver_SegmentLongerThanSyncTakesIsRefusedUpFront(t *testing.T) {
	var refused, discarded atomic.Int32
	f := newDAVFixture(t, syncRules(&refused, &discarded))
	ctx := davCtx("tenant-a")
	require.NoError(t, f.drv.Put(ctx, "c", strings.Repeat("D", 248)+"/"+strings.Repeat("L", 246), strings.NewReader("x"), engine.WithContentLength(1)))
	puts, mkcols := f.methods.put.Load(), f.methods.mkcol.Load()
	for _, k := range []string{
		strings.Repeat("L", 249),
		"dir/" + strings.Repeat(":", 83), // 83 → 249 once mapped
		strings.Repeat("😀", 125),         // 250 UTF-16 units
	} {
		err := f.drv.Put(ctx, "c", k, strings.NewReader("x"), engine.WithContentLength(1))
		require.Error(t, err, "%q", k)
		assert.ErrorIs(t, err, engine.ErrInvalidInput)
	}
	assert.Equal(t, puts, f.methods.put.Load())
	assert.Equal(t, mkcols, f.methods.mkcol.Load())
	assert.Zero(t, refused.Load())
}

// A name the server refuses (400 / 414) is the caller's error — engine.ErrInvalidInput, which
// never charges a breaker (the sync breaker opened on five bad names and
// sent every tenant's sync-tier writes to the primary). A 5xx is not.
func TestWebDAVDriver_RefusedNamesAreInvalidInput(t *testing.T) {
	ctx := davCtx("tenant-a")
	f := newDAVFixture(t, func(h http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch {
			case strings.Contains(r.URL.Path, "refuse-me"):
				http.Error(w, "", http.StatusBadRequest)
			case strings.Contains(r.URL.Path, "too-long-uri"):
				http.Error(w, "", http.StatusRequestURITooLong)
			case strings.Contains(r.URL.Path, "broken") && r.Method == http.MethodPut:
				http.Error(w, "", http.StatusInternalServerError)
			default:
				h.ServeHTTP(w, r)
			}
		})
	}, WithWebDAVRetries(1, 0))
	for _, k := range []string{"refuse-me", "dir/refuse-me", "refuse-me/x", "too-long-uri"} {
		err := f.drv.Put(ctx, "c", k, strings.NewReader("x"), engine.WithContentLength(1))
		require.Error(t, err, k)
		assert.ErrorIs(t, err, engine.ErrInvalidInput, k)
	}

	err := f.drv.Put(ctx, "c", "broken", strings.NewReader("x"), engine.WithContentLength(1))
	require.Error(t, err)
	assert.False(t, errors.Is(err, engine.ErrInvalidInput), "a 5xx is the server's trouble: %v", err)
}

// The multi-bridge driver passes the classification through.
func TestMultiWebDAVDriver_RefusedNameIsInvalidInput(t *testing.T) {
	var refused, discarded atomic.Int32
	f := newDAVFixture(t, syncRules(&refused, &discarded))
	m, err := NewMultiWebDAVDriver("sync", WebDAVConfig{
		Bridges: []WebDAVBridge{{URL: f.srv.URL, Password: davTestPass}},
		User:    davTestUser, Root: "vaultaire",
	}, zap.NewNop())
	require.NoError(t, err)
	err = m.Put(davCtx("tenant-a"), "c", strings.Repeat("L", 300), strings.NewReader("x"), engine.WithContentLength(1))
	require.Error(t, err)
	assert.ErrorIs(t, err, engine.ErrInvalidInput)
	require.NoError(t, m.Put(davCtx("tenant-a"), "c", "CON", strings.NewReader("x"), engine.WithContentLength(1)))
	assert.Zero(t, refused.Load())
}
