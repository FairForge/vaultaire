package drivers

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// The bound invariant (RP8-9, WP-R6-3; written in internal/drivers/CLAUDE.md):
// nothing on the data path waits more than 30 s for a backend's first byte
// or answer. On 2026-10-17 three 60 s timers raced — the transport's
// response-header timeout, the PUT deadline of a small body and aws-cli's
// default read timeout — so a stalled backend surfaced as the client giving
// up, never as the backend's own failure. The exceptions, each with its
// reason:
//
//   - CompleteMultipartUpload (and a server-side copy): 10 min — the vendor
//     assembles the object before it answers, and a re-sent Complete whose
//     first copy committed answers NoSuchUpload (Prompt 2b.5 F3.8);
//   - Geyser's GET first byte and PUT answer: 5 min — tape (Vail);
//   - the per-size PUT deadline (s3PutDeadline: 10 s floor, 15 s for a
//     16 MiB part, 60 s per 64 MiB) — the PUT's own bound, under which the
//     transport's PUT header bound never fires first;
//   - the Sync bridge (webdav.go): its own bounds (a PUT 2 min per 64 MiB,
//     an answer 5 min, metadata 60 s, a listing 10 min, the idle watchdog
//     60 s) — a bridge answers a PUT only after its own upload to Sync's
//     cloud (residual RP9 in docs/reviews/WP-P9.md).
const dataPathBound = 30 * time.Second

// headerBounds reports the response-header bound of every request class a
// client's transport applies (0 = none: unbounded).
func headerBounds(t *testing.T, hc *http.Client) map[string]time.Duration {
	t.Helper()
	switch tr := hc.Transport.(type) {
	case *opSplitTransport:
		return map[string]time.Duration{
			"meta":     tr.byClass[classMeta].ResponseHeaderTimeout,
			"get":      tr.byClass[classGet].ResponseHeaderTimeout,
			"put":      tr.byClass[classPut].ResponseHeaderTimeout,
			"complete": tr.byClass[classComplete].ResponseHeaderTimeout,
		}
	case *http.Transport:
		d := tr.ResponseHeaderTimeout
		return map[string]time.Duration{"meta": d, "get": d, "put": d, "complete": d}
	default:
		t.Fatalf("unknown transport %T: the invariant cannot read its bounds", hc.Transport)
		return nil
	}
}

func sdkHTTPClient(t *testing.T, c *s3.Client) *http.Client {
	t.Helper()
	hc, ok := c.Options().HTTPClient.(*http.Client)
	require.True(t, ok, "client: %T", c.Options().HTTPClient)
	return hc
}

// assertBounded checks one client against the invariant; allow names the
// classes with an exception and its limit.
func assertBounded(t *testing.T, name string, hc *http.Client, allow map[string]time.Duration) {
	t.Helper()
	for class, d := range headerBounds(t, hc) {
		limit := dataPathBound
		if a, ok := allow[class]; ok {
			limit = a
		}
		assert.True(t, d > 0 && d <= limit, "%s: %s waits %s for its first byte (want 0 < bound <= %s)", name, class, d, limit)
	}
}

var completeException = map[string]time.Duration{"complete": 10 * time.Minute}

func TestDataPathBounds_NothingWaitsMoreThanThirtySeconds(t *testing.T) {
	t.Setenv("FIXED_BUCKET_PUT_TIMEOUT", "")
	nop := zap.NewNop()

	fixed, err := NewFixedBucketS3Driver("idrive", "ak", "sk", "https://s3.example.invalid", "us-west-1", "b", nop)
	require.NoError(t, err)
	assertBounded(t, "fixed-bucket (idrive, idrive-<region>, wasabi)", sdkHTTPClient(t, fixed.client), completeException)
	assertBounded(t, "fixed-bucket fresh (PUT retry, hedge)", fixed.putClient.fresh, completeException)

	lyve, err := NewLyveDriver("ak", "sk", "", "us-east-1", nop)
	require.NoError(t, err)
	assertBounded(t, "lyve", sdkHTTPClient(t, lyve.client), completeException)
	lp, ok := lyve.putClient.(*deadlinePutClient)
	require.True(t, ok)
	assertBounded(t, "lyve fresh (PUT retry, hedge)", lp.fresh, completeException)

	r2, err := NewR2Driver("acct", "ak", "sk", "", "", nop)
	require.NoError(t, err)
	assertBounded(t, "r2", sdkHTTPClient(t, r2.client), completeException)

	compat, err := NewS3CompatDriver("ak", "sk", nop)
	require.NoError(t, err)
	assertBounded(t, "s3compat (`s3`)", sdkHTTPClient(t, compat.client), completeException)

	plain, err := NewS3Driver("https://s3.example.invalid", "ak", "sk", "", nop)
	require.NoError(t, err)
	assertBounded(t, "s3 (quotaless's base)", sdkHTTPClient(t, plain.client), completeException)

	geyser, err := NewGeyserDriver("ak", "sk", "b", "", nop)
	require.NoError(t, err)
	assertBounded(t, "geyser", sdkHTTPClient(t, geyser.client), map[string]time.Duration{
		"get": 5 * time.Minute, "put": 5 * time.Minute, "complete": 10 * time.Minute,
	})

	// The PUT deadline is the PUT's bound, and never above the transport's
	// PUT bound for any part a driver sends (≤ 16 MiB: 15 s at the default).
	assert.LessOrEqual(t, s3PutDeadline(fixedBucketPutTimeoutDefault, s3UploadPartSize), dataPathBound)
	assert.LessOrEqual(t, s3PutDeadline(fixedBucketPutTimeoutDefault, 2<<20), dataPathBound)
}

// Every TunedHTTPClient call and every context.WithTimeout in the drivers
// is listed here with what bounds it, so a new one is a decision, not an
// accident: a site this table does not know fails the test.
var boundSites = map[string]string{
	// TunedHTTPClient
	"TunedHTTPClient idrive.go:s3ObjectHTTPClient":         "s3OpTimeouts (checked above)",
	"TunedHTTPClient idrive.go:s3FreshHTTPClient":          "s3OpTimeouts, keep-alives off (checked above)",
	"TunedHTTPClient r2.go:NewR2Driver":                    "s3OpTimeouts (checked above)",
	"TunedHTTPClient s3compat.go:NewS3CompatDriver":        "s3OpTimeouts (checked above)",
	"TunedHTTPClient s3.go:NewS3Driver":                    "s3OpTimeouts (checked above)",
	"TunedHTTPClient geyser.go:NewGeyserDriver":            "geyserOpTimeouts: tape 5 min for GET/PUT (checked above)",
	"TunedHTTPClient webdav.go:NewWebDAVDriver":            "the Sync bridge exception: 5 min answer, its own watchdogs",
	"TunedHTTPClient swift.go:NewSwiftDriver":              "Blomp/Swift: 25 s to the first byte after the body (ResponseHeaderTimeout); metadata 15 s via meta; the hedge's fresh client the same bound, keep-alives off",
	"TunedHTTPClient lyve_console.go:NewLyveConsoleClient": "management plane (the root-key console probe), bounded by the probe's 45 s context",
	// context.WithTimeout
	"WithTimeout put_deadline.go:attempt":            "the per-size PUT deadline (s3PutDeadline)",
	"WithTimeout dns_cache.go:entry":                 "one DNS lookup of the drivers' cache, 5 s (dnsLookupTimeout); a dial waits on it only within its own context",
	"WithTimeout dns_cache.go:dialer":                "the dial's address loop: ONE budget, the dialer's Timeout (10 s), across every address",
	"WithTimeout dns_cache.go:attemptContext":        "one address's share of the loop's budget: the time left over the addresses left, ≥ 2 s",
	"WithTimeout onedrive.go:NewOneDriveFleetDriver": "boot only: the fleet's first token and drive ids, 30 s",
	"WithTimeout onedrive_fleet.go:recheckFleet":     "the probe path: one fingerprint re-check after an unknown verdict or an hour, 10 s inside the probe's 15 s (RP18-1)",
	"WithTimeout quotaless.go:HealthCheck":           "the quotaless probe, 10 s",
	"WithTimeout tenant_walk.go:s3WalkPrefix":        "a job (the erasure sweep): one listing page, 60 s over a 15 s transport bound",
	"WithTimeout list_page.go:s3ListPage":            "a job (the chunk orphan sweep, WP-R8-2 B.2): one listing page, 60 s over a 15 s transport bound",
	"WithTimeout swift.go:authenticate":              "one Keystone token request, 15 s (swiftMetaTimeout)",
	"WithTimeout swift.go:meta":                      "a Swift HEAD / DELETE / small POST, 15 s whole (swiftMetaTimeout)",
	"WithTimeout swift_segment_gc.go:listEntries":    "one Swift listing page: 15 s on a request path, 60 s in a job's walk (swiftWalkPageTimeout)",
	"WithTimeout swift.go:bulkDelete":                "a bulk delete of pieces (a failed upload — twice when a piece may land late —, a deleted large object, the segment reaper): 2 min + 0.5 s a piece, ≤ 15 min",
	"WithTimeout webdav_resilience.go:send":          "the Sync bridge exception: the caller's per-request timeout",
	"WithTimeout webdav_folder_limit.go:notePut":     "the Sync bridge exception: a folder count, 2 × 60 s",
	"WithTimeout webdav_stripe.go:dropGeneration":    "the Sync bridge exception: a failed striped upload's cleanup, detached, 5 min",
}

func TestDataPathBounds_EveryClientAndTimeoutIsListed(t *testing.T) {
	fset := token.NewFileSet()
	files, err := filepath.Glob("*.go")
	require.NoError(t, err)
	found := map[string]bool{}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, err := os.ReadFile(f) // #nosec G304 -- this package's own sources
		require.NoError(t, err)
		file, err := parser.ParseFile(fset, f, src, 0)
		require.NoError(t, err)
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				switch fun := call.Fun.(type) {
				case *ast.Ident:
					if fun.Name == "TunedHTTPClient" {
						found[fmt.Sprintf("TunedHTTPClient %s:%s", f, fn.Name.Name)] = true
					}
				case *ast.SelectorExpr:
					if x, ok := fun.X.(*ast.Ident); ok && x.Name == "context" && fun.Sel.Name == "WithTimeout" {
						found[fmt.Sprintf("WithTimeout %s:%s", f, fn.Name.Name)] = true
					}
				}
				return true
			})
		}
	}
	var unknown, stale []string
	for site := range found {
		if _, ok := boundSites[site]; !ok {
			unknown = append(unknown, site)
		}
	}
	for site := range boundSites {
		if !found[site] {
			stale = append(stale, site)
		}
	}
	sort.Strings(unknown)
	sort.Strings(stale)
	assert.Empty(t, unknown, "a client or timeout the bound invariant does not know: say what bounds it in boundSites (and CLAUDE.md)")
	assert.Empty(t, stale, "boundSites names a site that is gone: drop it")
}
