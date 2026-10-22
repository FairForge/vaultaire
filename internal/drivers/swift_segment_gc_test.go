package drivers

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/FairForge/vaultaire/internal/engine"
)

const testSegmentGrace = 6 * time.Hour

// segmentNames are the stored names under t-<tenant>/_segments/.
func segmentNames(f *fakeSwift, tenant string) []string {
	var out []string
	for _, n := range f.names() {
		if strings.HasPrefix(n, "t-"+tenant+"/_segments/") {
			out = append(out, n)
		}
	}
	return out
}

// generations groups segment names by their generation prefix.
func generations(names []string) map[string][]string {
	out := map[string][]string{}
	for _, n := range names {
		i := strings.LastIndex(n, "/")
		out[n[:i+1]] = append(out[n[:i+1]], n[i+1:])
	}
	return out
}

func newGCSwift(t *testing.T) (*SwiftDriver, *fakeSwift) {
	t.Helper()
	f := newFakeSwift(t)
	d, err := NewSwiftDriver("swift-gc", f.config(), zap.NewNop()) // segments of 1 KiB
	require.NoError(t, err)
	return d, f
}

// A large upload writes a marker naming its manifest beside its pieces:
// what lets the reaper find a generation's manifest without a listing.
func TestSwift_ALargeUploadWritesItsGenerationMarker(t *testing.T) {
	d, f := newGCSwift(t)
	ctx := swiftCtx("t1")
	body := randomBytes(t, 3000)
	require.NoError(t, d.Put(ctx, "b1", "big", bytes.NewReader(body), engine.WithContentLength(int64(len(body)))))

	gens := generations(segmentNames(f, "t1"))
	require.Len(t, gens, 1)
	for prefix, files := range gens {
		assert.Contains(t, files, swiftGenerationMarker)
		m, ok := f.object(prefix + swiftGenerationMarker)
		require.True(t, ok)
		assert.Equal(t, "t-t1/b1/big", string(m.data))
	}

	// A delete takes the marker with the pieces.
	require.NoError(t, d.Delete(ctx, "b1", "big"))
	d.waitCleanups()
	assert.Empty(t, f.names())
}

// The reaper: a crashed upload's generation (marker + pieces, its manifest
// never written) is deleted once older than the grace; the live object's
// generation is kept and still reads.
func TestSwiftSegmentGC_ACrashedUploadIsReapedTheLiveOneKept(t *testing.T) {
	d, f := newGCSwift(t)
	ctx := swiftCtx("t1")
	body := randomBytes(t, 3000)
	require.NoError(t, d.Put(ctx, "b1", "big", bytes.NewReader(body), engine.WithContentLength(int64(len(body)))))
	live := generations(segmentNames(f, "t1"))

	crashed := swiftSegmentPrefix("t1", "t-t1/b1/big", "00000000000000aa")
	f.put(crashed+swiftGenerationMarker, []byte("t-t1/b1/big"))
	f.put(crashed+"00000000", randomBytes(t, 1024))
	f.put(crashed+"00000001", randomBytes(t, 1024))
	gone := swiftSegmentPrefix("t1", "t-t1/b1/gone", "00000000000000bb") // its manifest never existed
	f.put(gone+swiftGenerationMarker, []byte("t-t1/b1/gone"))
	f.put(gone+"00000000", randomBytes(t, 1024))
	f.age("t-t1/", 7*time.Hour)

	res, err := d.ReapOrphanSegments(context.Background(), testSegmentGrace)

	require.NoError(t, err)
	assert.Equal(t, 3, res.Generations)
	assert.Equal(t, 1, res.Live)
	assert.Equal(t, 2, res.Reaped)
	assert.Equal(t, 3, res.PiecesDeleted, "the pieces first; a marker goes once its generation lists nothing else")
	left := generations(segmentNames(f, "t1"))
	for prefix := range live {
		assert.Equal(t, live[prefix], left[prefix], "the live generation is untouched")
	}
	assert.Equal(t, []string{swiftGenerationMarker}, left[crashed])
	assert.Equal(t, []string{swiftGenerationMarker}, left[gone])

	// The next run takes the markers left alone.
	res, err = d.ReapOrphanSegments(context.Background(), testSegmentGrace)
	require.NoError(t, err)
	assert.Equal(t, 2, res.Reaped)
	assert.Equal(t, 2, res.MarkersDeleted)
	assert.Equal(t, live, generations(segmentNames(f, "t1")))

	rc, err := d.Get(ctx, "b1", "big")
	require.NoError(t, err)
	got, _ := io.ReadAll(rc)
	_ = rc.Close()
	assert.Equal(t, body, got)
}

// Young generations — an upload still running, or one whose manifest a
// lagging listing has not shown — are never judged.
func TestSwiftSegmentGC_AYoungGenerationIsKept(t *testing.T) {
	d, f := newGCSwift(t)
	g := swiftSegmentPrefix("t1", "t-t1/b1/k", "00000000000000cc")
	f.put(g+swiftGenerationMarker, []byte("t-t1/b1/k"))
	f.put(g+"00000000", randomBytes(t, 1024))
	f.age(g, 7*time.Hour)
	f.put(g+"00000001", randomBytes(t, 1024)) // the newest piece is recent

	res, err := d.ReapOrphanSegments(context.Background(), testSegmentGrace)

	require.NoError(t, err)
	assert.Equal(t, 1, res.Young)
	assert.Len(t, segmentNames(f, "t1"), 3)
}

// A generation with no marker (written before markers existed) or one whose
// marker does not name a manifest of its own digest cannot be identified:
// kept and counted.
func TestSwiftSegmentGC_AnUnidentifiedGenerationIsKept(t *testing.T) {
	d, f := newGCSwift(t)
	noMarker := swiftSegmentPrefix("t1", "t-t1/b1/old", "00000000000000dd")
	f.put(noMarker+"00000000", randomBytes(t, 1024))
	lying := swiftSegmentPrefix("t1", "t-t1/b1/x", "00000000000000ee")
	f.put(lying+swiftGenerationMarker, []byte("t-t1/b1/another-key")) // not this digest's name
	f.put(lying+"00000000", randomBytes(t, 1024))
	foreign := swiftSegmentPrefix("t1", "t-t2/b1/x", "00000000000000ff")
	f.put(foreign+swiftGenerationMarker, []byte("t-t2/b1/x")) // another tenant's name
	f.put(foreign+"00000000", randomBytes(t, 1024))
	f.age("t-t1/", 7*time.Hour)

	res, err := d.ReapOrphanSegments(context.Background(), testSegmentGrace)

	require.NoError(t, err)
	assert.Equal(t, 3, res.Unidentified)
	assert.Equal(t, 0, res.Reaped)
	assert.Len(t, segmentNames(f, "t1"), 5)
}

// A failed upload removes its pieces at once and leaves its marker (a
// piece cancelled mid-flight may still land after the removal); the reaper
// takes what is left after the grace.
func TestSwiftSegmentGC_AFailedUploadsMarkerIsReapedAfterTheGrace(t *testing.T) {
	f := newFakeSwift(t)
	cfg := f.config()
	cfg.PutConcurrency = 1
	d, err := NewSwiftDriver("swift-gc", cfg, zap.NewNop())
	require.NoError(t, err)
	d.client.Transport = &failPiece{rt: d.client.Transport, suffix: "/00000002", times: -1}
	d.segmentBackoff = func(int) time.Duration { return time.Millisecond }
	ctx := swiftCtx("t1")
	body := randomBytes(t, 5000)
	require.Error(t, d.Put(ctx, "b1", "big", bytes.NewReader(body), engine.WithContentLength(int64(len(body)))))
	gens := generations(segmentNames(f, "t1"))
	require.Len(t, gens, 1)
	for _, files := range gens {
		assert.Equal(t, []string{swiftGenerationMarker}, files, "the pieces went at once, the marker stays for the reaper")
	}

	res, err := d.ReapOrphanSegments(context.Background(), testSegmentGrace)
	require.NoError(t, err)
	assert.Equal(t, 1, res.Young, "not before the grace")

	f.age("t-t1/", 7*time.Hour)
	res, err = d.ReapOrphanSegments(context.Background(), testSegmentGrace)
	require.NoError(t, err)
	assert.Equal(t, 1, res.MarkersDeleted)
	assert.Empty(t, f.names())
}

// A manifest that cannot be read (5xx) keeps its generation: only a 404 or
// an object that is not a manifest makes it an orphan.
func TestSwiftSegmentGC_AnUnreadableManifestKeepsTheGeneration(t *testing.T) {
	d, f := newGCSwift(t)
	g := swiftSegmentPrefix("t1", "t-t1/b1/k", "0000000000000011")
	f.put(g+swiftGenerationMarker, []byte("t-t1/b1/k"))
	f.put(g+"00000000", randomBytes(t, 1024))
	f.put("t-t1/b1/k", []byte("a plain object, not a manifest"))
	g2 := swiftSegmentPrefix("t1", "t-t1/b1/j", "0000000000000022")
	f.put(g2+swiftGenerationMarker, []byte("t-t1/b1/j"))
	f.put(g2+"00000000", randomBytes(t, 1024))
	f.age("t-t1/", 7*time.Hour)
	// The markers are read first (GET), then the manifests: fail every
	// GET after the two markers' — the manifest of the second generation.
	d.client.Transport = &failNthGet{rt: d.client.Transport, match: "t-t1/b1/j?", status: http.StatusServiceUnavailable}

	res, err := d.ReapOrphanSegments(context.Background(), testSegmentGrace)

	require.NoError(t, err)
	assert.Equal(t, 1, res.Reaped, "a plain object at the name references nothing")
	assert.Len(t, res.Errors, 1)
	assert.Contains(t, segmentNames(f, "t1"), g2+"00000000", "kept on the error")
}

type failNthGet struct {
	rt     http.RoundTripper
	match  string
	status int
}

func (f *failNthGet) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.Method == http.MethodGet && strings.Contains(r.URL.String(), f.match) {
		return &http.Response{StatusCode: f.status, Body: io.NopCloser(strings.NewReader("")), Header: http.Header{}, Request: r}, nil
	}
	return f.rt.RoundTrip(r)
}

// Every tenant is walked; the piece budget stops a run and says so.
func TestSwiftSegmentGC_EveryTenantWithinTheBudget(t *testing.T) {
	d, f := newGCSwift(t)
	for _, tenant := range []string{"a", "b", "c"} {
		g := swiftSegmentPrefix(tenant, "t-"+tenant+"/b/k", "0000000000000033")
		f.put(g+swiftGenerationMarker, []byte("t-"+tenant+"/b/k"))
		f.put(g+"00000000", randomBytes(t, 1024))
		f.put(g+"00000001", randomBytes(t, 1024))
	}
	f.put("t-a/b/plain", []byte("x"))
	f.age("t-", 7*time.Hour)
	d.segmentGCMaxPieces = 4

	res, err := d.ReapOrphanSegments(context.Background(), testSegmentGrace)

	require.NoError(t, err)
	assert.True(t, res.BudgetStopped)
	assert.Equal(t, 2, res.Reaped, "the third generation would pass the budget")
	assert.Equal(t, 4, res.PiecesDeleted)

	d.segmentGCMaxPieces = 1000
	res, err = d.ReapOrphanSegments(context.Background(), testSegmentGrace)
	require.NoError(t, err)
	assert.False(t, res.BudgetStopped)
	assert.Equal(t, 3, res.Tenants)
}
