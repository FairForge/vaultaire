package drivers

import (
	"bytes"
	"context"
	"crypto/rand"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/FairForge/vaultaire/internal/common"
	"github.com/FairForge/vaultaire/internal/engine"
)

// The Swift driver's behaviour is tested twice with the same functions:
// against the real Blomp account when BLOMP_USER / BLOMP_PASSWORD are set
// (`swift_README.md` § Live tests: ~50 objects, cleaned up), and against
// fakeSwift always (CI has no Blomp credentials). The failure shapes Blomp
// cannot be made to produce on demand — a revoked token, a 503 — run on the
// fake only.

type swiftTarget struct {
	name string
	drv  *SwiftDriver
	live bool
}

// swiftTargets is the fake, plus Blomp when its credentials are set. Each
// gets a fresh tenant id so runs never meet.
func swiftTargets(t *testing.T) []swiftTarget {
	t.Helper()
	f := newFakeSwift(t)
	fake, err := NewSwiftDriver("swift-fake", f.config(), zap.NewNop())
	require.NoError(t, err)
	out := []swiftTarget{{name: "fake", drv: fake}}
	if cfg, ok, err := BlompConfigFromEnv(os.Getenv); ok && err == nil {
		cfg.SegmentSize = 1 << 20 // Swift 2.17's min_segment_size: every piece but the last ≥ 1 MiB
		live, lerr := NewSwiftDriver("blomp-test", cfg, zap.NewNop())
		require.NoError(t, lerr)
		out = append(out, swiftTarget{name: "blomp", drv: live, live: true})
	} else if err != nil {
		t.Fatalf("BLOMP_* set but invalid: %v", err)
	}
	return out
}

func swiftTenant(t *testing.T) string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return fmt.Sprintf("_test-%x", b)
}

func swiftCtx(tenant string) context.Context {
	return common.WithTenantID(context.Background(), tenant)
}

func randomBytes(t *testing.T, n int) []byte {
	b := make([]byte, n)
	_, err := rand.Read(b)
	require.NoError(t, err)
	return b
}

// cleanupTenant removes everything a test left under its tenant.
func cleanupTenant(t *testing.T, d *SwiftDriver, tenant string) {
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		_ = d.WalkTenant(ctx, tenant, func(o engine.TenantObject) error { return o.Remove(ctx) })
	})
}

func readAll(t *testing.T, rc io.ReadCloser, err error) []byte {
	t.Helper()
	require.NoError(t, err)
	defer func() { _ = rc.Close() }()
	b, err := io.ReadAll(rc)
	require.NoError(t, err)
	return b
}

func TestSwift_RoundTrip(t *testing.T) {
	for _, tg := range swiftTargets(t) {
		t.Run(tg.name, func(t *testing.T) {
			tenant := swiftTenant(t)
			cleanupTenant(t, tg.drv, tenant)
			ctx := swiftCtx(tenant)
			body := randomBytes(t, 4096)

			require.NoError(t, tg.drv.Put(ctx, "b1", "dir/hello world.bin", bytes.NewReader(body), engine.WithContentLength(int64(len(body)))))

			ok, err := tg.drv.Exists(ctx, "b1", "dir/hello world.bin")
			require.NoError(t, err)
			assert.True(t, ok)
			rc, err := tg.drv.Get(ctx, "b1", "dir/hello world.bin")
			assert.Equal(t, body, readAll(t, rc, err))

			rc, err = tg.drv.GetRange(ctx, "b1", "dir/hello world.bin", 100, 50)
			assert.Equal(t, body[100:150], readAll(t, rc, err))

			require.NoError(t, tg.drv.Delete(ctx, "b1", "dir/hello world.bin"))
			ok, err = tg.drv.Exists(ctx, "b1", "dir/hello world.bin")
			require.NoError(t, err)
			assert.False(t, ok)
		})
	}
}

func TestSwift_MissesAreTypedMisses(t *testing.T) {
	for _, tg := range swiftTargets(t) {
		t.Run(tg.name, func(t *testing.T) {
			ctx := swiftCtx(swiftTenant(t))
			_, err := tg.drv.Get(ctx, "b1", "nope")
			assert.True(t, engine.IsObjectMiss(err), "Get: %v", err)
			_, err = tg.drv.GetRange(ctx, "b1", "nope", 0, 10)
			assert.True(t, engine.IsObjectMiss(err), "GetRange: %v", err)
			err = tg.drv.Delete(ctx, "b1", "nope")
			assert.True(t, engine.IsObjectMiss(err), "Delete: %v", err)
		})
	}
}

func TestSwift_RefusesACallWithoutATenant(t *testing.T) {
	tg := swiftTargets(t)[0]
	err := tg.drv.Put(context.Background(), "b1", "k", strings.NewReader("x"))
	assert.ErrorIs(t, err, ErrNoTenant)
	assert.ErrorIs(t, err, engine.ErrInvalidInput)
}

func TestSwift_RefusesNamesSwiftCannotHold(t *testing.T) {
	tg := swiftTargets(t)[0]
	ctx := swiftCtx("t1")
	assert.ErrorIs(t, tg.drv.Put(ctx, "b1", "a/../b", strings.NewReader("x")), engine.ErrInvalidInput)
	assert.ErrorIs(t, tg.drv.Put(ctx, "b1", strings.Repeat("k", 1020), strings.NewReader("x")), engine.ErrInvalidInput)
}

// A body larger than a segment is a static large object: pieces in the same
// container, read back whole and by range, deleted with its pieces.
func TestSwift_LargeObject(t *testing.T) {
	for _, tg := range swiftTargets(t) {
		t.Run(tg.name, func(t *testing.T) {
			tenant := swiftTenant(t)
			cleanupTenant(t, tg.drv, tenant)
			ctx := swiftCtx(tenant)
			seg := int(tg.drv.cfg.SegmentSize)
			body := randomBytes(t, seg*2+seg/2)

			require.NoError(t, tg.drv.Put(ctx, "b1", "big", bytes.NewReader(body), engine.WithContentLength(int64(len(body)))))

			rc, err := tg.drv.Get(ctx, "b1", "big")
			assert.Equal(t, body, readAll(t, rc, err))
			rc, err = tg.drv.GetRange(ctx, "b1", "big", int64(seg-10), 20) // across a piece boundary
			assert.Equal(t, body[seg-10:seg+10], readAll(t, rc, err))

			if !tg.live { // a container listing lags on Blomp
				assert.Equal(t, 3, countWalk(t, tg.drv, tenant, "_segments"), "three pieces under _segments")
				keys, err := tg.drv.List(ctx, "b1", "")
				require.NoError(t, err)
				assert.Equal(t, []string{"big"}, keys, "the pieces are not in the object's container")
			}

			require.NoError(t, tg.drv.Delete(ctx, "b1", "big"))
			ok, err := tg.drv.Exists(ctx, "b1", "big")
			require.NoError(t, err)
			assert.False(t, ok)
			tg.drv.waitCleanups()
			if !tg.live {
				assert.Equal(t, 0, countWalk(t, tg.drv, tenant, "_segments"), "the pieces went with the manifest")
			}
		})
	}
}

// A body of unknown length: one PUT when it ends within the peek, a large
// object when it does not.
func TestSwift_UnknownLength(t *testing.T) {
	for _, tg := range swiftTargets(t) {
		t.Run(tg.name, func(t *testing.T) {
			tenant := swiftTenant(t)
			cleanupTenant(t, tg.drv, tenant)
			ctx := swiftCtx(tenant)

			small := randomBytes(t, 300)
			require.NoError(t, tg.drv.Put(ctx, "b1", "small", io.NopCloser(bytes.NewReader(small))))
			rc, err := tg.drv.Get(ctx, "b1", "small")
			assert.Equal(t, small, readAll(t, rc, err))

			empty := []byte{}
			require.NoError(t, tg.drv.Put(ctx, "b1", "empty", bytes.NewReader(empty)))
			rc, err = tg.drv.Get(ctx, "b1", "empty")
			assert.Empty(t, readAll(t, rc, err))
		})
	}
}

func TestSwift_UnknownLengthPastThePeekIsALargeObject(t *testing.T) {
	f := newFakeSwift(t)
	cfg := f.config()
	cfg.SegmentSize = 64 << 10
	d, err := NewSwiftDriver("swift-fake", cfg, zap.NewNop())
	require.NoError(t, err)
	ctx := swiftCtx("t1")
	body := randomBytes(t, swiftPeekBytes+1500)

	require.NoError(t, d.Put(ctx, "b1", "k", struct{ io.Reader }{bytes.NewReader(body)}))

	o, ok := f.object("t-t1/b1/k")
	require.True(t, ok)
	assert.NotNil(t, o.manifest)
	assert.Equal(t, body, o.data)
}

func TestSwift_ShortOrLongBodyIsRefused(t *testing.T) {
	f := newFakeSwift(t)
	d, err := NewSwiftDriver("swift-fake", f.config(), zap.NewNop())
	require.NoError(t, err)
	ctx := swiftCtx("t1")

	err = d.Put(ctx, "b1", "short", bytes.NewReader(make([]byte, 100)), engine.WithContentLength(200))
	assert.ErrorIs(t, err, io.ErrUnexpectedEOF)
	err = d.Put(ctx, "b1", "long", bytes.NewReader(make([]byte, 300)), engine.WithContentLength(200))
	assert.ErrorIs(t, err, engine.ErrInvalidInput)
	err = d.Put(ctx, "b1", "shortbig", bytes.NewReader(make([]byte, 2000)), engine.WithContentLength(3000))
	assert.ErrorIs(t, err, io.ErrUnexpectedEOF)
	assert.Empty(t, f.names(), "nothing stored, the pieces of the large one removed")
}

func TestSwift_FailedManifestRemovesItsPieces(t *testing.T) {
	f := newFakeSwift(t)
	d, err := NewSwiftDriver("swift-fake", f.config(), zap.NewNop())
	require.NoError(t, err)
	ctx := swiftCtx("t1")
	body := randomBytes(t, 2500)
	// three segment PUTs pass, the manifest PUT (and its two retries) 503
	d.client.Transport = &failNthPut{rt: d.client.Transport, n: 4, f: f}

	err = d.Put(ctx, "b1", "k", bytes.NewReader(body), engine.WithContentLength(int64(len(body))))
	require.Error(t, err)
	assert.Empty(t, f.names(), "the pieces were bulk-deleted")
	assert.Equal(t, 1, f.count("POST bulk-delete"))
}

// failNthPut makes the n-th PUT and every later one answer 503.
type failNthPut struct {
	rt   http.RoundTripper
	n    int
	mu   sync.Mutex
	puts int
	f    *fakeSwift
}

func (p *failNthPut) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.Method == http.MethodPut {
		p.mu.Lock()
		defer p.mu.Unlock()
		p.puts++
		if p.puts >= p.n {
			p.f.failNext(http.MethodPut, http.StatusServiceUnavailable)
		}
	}
	return p.rt.RoundTrip(r)
}

func TestSwift_ListAndWalk(t *testing.T) {
	for _, tg := range swiftTargets(t) {
		t.Run(tg.name, func(t *testing.T) {
			if tg.live {
				t.Skip("a Blomp container listing lags writes by tens of seconds (measured 2026-10-21)")
			}
			tg.drv.listPageSize = 2
			ab := swiftTenant(t)
			abc := ab + "c" // shares ab's prefix
			for _, k := range []string{"a", "b/c", "d"} {
				require.NoError(t, tg.drv.Put(swiftCtx(ab), "b1", k, strings.NewReader(k)))
			}
			require.NoError(t, tg.drv.Put(swiftCtx(ab), "b2", "x", strings.NewReader("x")))
			require.NoError(t, tg.drv.Put(swiftCtx(abc), "b1", "neighbour", strings.NewReader("n")))

			keys, err := tg.drv.List(swiftCtx(ab), "b1", "")
			require.NoError(t, err)
			assert.Equal(t, []string{"a", "b/c", "d"}, keys)
			keys, err = tg.drv.List(swiftCtx(ab), "b1", "b/")
			require.NoError(t, err)
			assert.Equal(t, []string{"b/c"}, keys)

			var seen []string
			require.NoError(t, tg.drv.WalkTenant(context.Background(), ab, func(o engine.TenantObject) error {
				seen = append(seen, o.Container+"|"+o.Artifact)
				return o.Remove(context.Background())
			}))
			sort.Strings(seen)
			assert.Equal(t, []string{"b1|a", "b1|b/c", "b1|d", "b2|x"}, seen, "never the neighbour tenant")
			ok, err := tg.drv.Exists(swiftCtx(abc), "b1", "neighbour")
			require.NoError(t, err)
			assert.True(t, ok)

			assert.ErrorIs(t, tg.drv.WalkTenant(context.Background(), "", nil), ErrWalkTenantID)
			assert.ErrorIs(t, tg.drv.WalkTenant(context.Background(), "a/b", nil), ErrWalkTenantID)
		})
	}
}

func TestSwift_HealthCheck(t *testing.T) {
	for _, tg := range swiftTargets(t) {
		t.Run(tg.name, func(t *testing.T) {
			require.NoError(t, tg.drv.HealthCheck(context.Background()))
			cfg := tg.drv.cfg
			cfg.Password = "wrong"
			bad, err := NewSwiftDriver("swift-bad", cfg, zap.NewNop())
			require.NoError(t, err)
			err = bad.HealthCheck(context.Background())
			var se *SwiftError
			require.ErrorAs(t, err, &se)
			assert.Equal(t, http.StatusUnauthorized, se.Status)
		})
	}
}

func TestSwift_ReauthenticatesOnceOnA401(t *testing.T) {
	f := newFakeSwift(t)
	d, err := NewSwiftDriver("swift-fake", f.config(), zap.NewNop())
	require.NoError(t, err)
	ctx := swiftCtx("t1")
	require.NoError(t, d.Put(ctx, "b1", "k", strings.NewReader("v")))

	f.revokeTokens()
	rc, err := d.Get(ctx, "b1", "k")
	assert.Equal(t, []byte("v"), readAll(t, rc, err))
	assert.Equal(t, 2, f.authCalls)

	f.password = "rotated" // the account's password changed: one re-auth, then the error
	f.revokeTokens()
	_, err = d.Get(ctx, "b1", "k")
	var se *SwiftError
	require.ErrorAs(t, err, &se)
	assert.Equal(t, http.StatusUnauthorized, se.Status)
}

func TestSwift_RefreshesTheTokenBeforeItExpires(t *testing.T) {
	f := newFakeSwift(t)
	d, err := NewSwiftDriver("swift-fake", f.config(), zap.NewNop())
	require.NoError(t, err)
	now := time.Now()
	d.now = func() time.Time { return now }
	ctx := swiftCtx("t1")
	_, _ = d.Exists(ctx, "b1", "k")
	_, _ = d.Exists(ctx, "b1", "k")
	assert.Equal(t, 1, f.authCalls)

	now = now.Add(56 * time.Minute) // inside the 5 min margin of the hour
	_, _ = d.Exists(ctx, "b1", "k")
	assert.Equal(t, 2, f.authCalls)
}

func TestSwift_RetriesA5xxWhenTheBodyCanBeResent(t *testing.T) {
	f := newFakeSwift(t)
	d, err := NewSwiftDriver("swift-fake", f.config(), zap.NewNop())
	require.NoError(t, err)
	ctx := swiftCtx("t1")

	f.failNext(http.MethodPut, http.StatusServiceUnavailable)
	require.NoError(t, d.Put(ctx, "b1", "k", strings.NewReader("small")))
	f.failNext(http.MethodGet, http.StatusServiceUnavailable, http.StatusBadGateway)
	rc, err := d.Get(ctx, "b1", "k")
	assert.Equal(t, []byte("small"), readAll(t, rc, err))

	f.failNext(http.MethodGet, 503, 503, 503)
	_, err = d.Get(ctx, "b1", "k")
	var se *SwiftError
	require.ErrorAs(t, err, &se)
	assert.Equal(t, 503, se.Status)
	assert.False(t, engine.IsObjectMiss(err))
}

func TestSwift_RangeIgnoredByTheBackendIsCut(t *testing.T) {
	f := newFakeSwift(t)
	d, err := NewSwiftDriver("swift-fake", f.config(), zap.NewNop())
	require.NoError(t, err)
	ctx := swiftCtx("t1")
	require.NoError(t, d.Put(ctx, "b1", "k", strings.NewReader("0123456789")))
	f.ignoreRange = true
	rc, err := d.GetRange(ctx, "b1", "k", 3, 4)
	assert.Equal(t, []byte("3456"), readAll(t, rc, err))
}

// A delete of a plain object is ONE write (Blomp rate-limits writes per
// container): a HEAD, then the DELETE that fits.
func TestSwift_DeleteIsOneWrite(t *testing.T) {
	f := newFakeSwift(t)
	d, err := NewSwiftDriver("swift-fake", f.config(), zap.NewNop())
	require.NoError(t, err)
	ctx := swiftCtx("t1")
	require.NoError(t, d.Put(ctx, "b1", "k", strings.NewReader("v")))
	require.NoError(t, d.Delete(ctx, "b1", "k"))
	assert.Equal(t, 1, f.count("DELETE object"))
	assert.Equal(t, 0, f.count("DELETE manifest-delete"))
}

// A large object's delete is the manifest alone (one write) and its pieces
// go in the background: Blomp deletes ~3 objects a second, so the
// manifest-delete of a 6 GiB object in 64 MiB pieces (97 writes) outran the
// 15 s bound on prod 2026-10-21 and every DELETE answered 503.
func TestSwift_LargeDeleteRemovesThePiecesInTheBackground(t *testing.T) {
	f := newFakeSwift(t)
	cfg := f.config()
	cfg.SegmentSize = 1 << 10
	d, err := NewSwiftDriver("swift-fake", cfg, zap.NewNop())
	require.NoError(t, err)
	ctx := swiftCtx("t1")
	body := randomBytes(t, 10<<10)
	require.NoError(t, d.Put(ctx, "b1", "big", bytes.NewReader(body), engine.WithContentLength(int64(len(body)))))
	require.NoError(t, d.Put(ctx, "b1", "other", strings.NewReader("keep")))

	require.NoError(t, d.Delete(ctx, "b1", "big"))
	d.waitCleanups()

	assert.Equal(t, 0, f.count("DELETE manifest-delete"), "never Swift's piece-by-piece manifest delete")
	assert.Equal(t, 1, f.count("GET manifest-get"))
	assert.Equal(t, 1, f.count("DELETE object"), "the manifest alone")
	assert.Equal(t, 1, f.count("POST bulk-delete"), "every piece in one request")
	assert.Equal(t, []string{"t-t1/b1/other"}, f.names())
}

// A full account is ErrBackendFull, not a failure to retry: Blomp's Swift
// answered 413 "Upload exceeds quota." past its 40 GiB on prod 2026-10-21
// and every client saw 503 "please retry". A large object refused midway
// leaves no pieces behind.
func TestSwift_FullAccountIsBackendFull(t *testing.T) {
	f := newFakeSwift(t)
	cfg := f.config()
	cfg.SegmentSize = 1 << 10
	d, err := NewSwiftDriver("swift-fake", cfg, zap.NewNop())
	require.NoError(t, err)
	ctx := swiftCtx("t1")
	require.NoError(t, d.Put(ctx, "b1", "first", bytes.NewReader(make([]byte, 3000))))
	f.quotaBytes = 4000
	before := f.names()

	err = d.Put(ctx, "b1", "small", bytes.NewReader(make([]byte, 1500)))
	assert.ErrorIs(t, err, engine.ErrBackendFull)
	body := randomBytes(t, 4<<10)
	err = d.Put(ctx, "b1", "big", bytes.NewReader(body), engine.WithContentLength(int64(len(body))))
	assert.ErrorIs(t, err, engine.ErrBackendFull)
	d.waitCleanups()
	assert.Equal(t, before, f.names(), "the refused uploads' pieces were removed")

	rc, err := d.Get(ctx, "b1", "first")
	assert.Len(t, readAll(t, rc, err), 3000, "reads still work on a full account")
}

func TestSwift_StoreIDAndCleartext(t *testing.T) {
	f := newFakeSwift(t)
	d, err := NewSwiftDriver("swift-fake", f.config(), zap.NewNop())
	require.NoError(t, err)
	assert.Contains(t, d.StoreID(), "swift://")
	assert.Contains(t, d.StoreID(), "/storage/u@example.com/u@example.com")
	assert.True(t, d.CleartextTransport(), "unknown before the first token = cleartext")
	require.NoError(t, d.HealthCheck(context.Background()))
	assert.True(t, d.CleartextTransport(), "the fake's storage URL is http://")
	assert.True(t, engine.IsCleartextTransport(d))
}

func TestBlompConfigFromEnv(t *testing.T) {
	env := func(kv map[string]string) func(string) string { return func(k string) string { return kv[k] } }

	_, ok, err := BlompConfigFromEnv(env(nil))
	assert.False(t, ok)
	assert.NoError(t, err)

	cfg, ok, err := BlompConfigFromEnv(env(map[string]string{"BLOMP_USER": "a@b.c", "BLOMP_PASSWORD": "p"}))
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, SwiftConfig{AuthURL: BlompDefaultAuthURL, User: "a@b.c", Password: "p", Tenant: "storage",
		Container: "a@b.c", SegmentSize: 64 << 20, PutConcurrency: 4, PutBudget: 1 << 30}, cfg)

	cfg, _, err = BlompConfigFromEnv(env(map[string]string{"BLOMP_USER": "a@b.c", "BLOMP_PASSWORD": "p",
		"BLOMP_PUT_CONCURRENCY": "8", "BLOMP_PUT_BUDGET": "2GiB"}))
	require.NoError(t, err)
	assert.Equal(t, 8, cfg.PutConcurrency)
	assert.Equal(t, int64(2<<30), cfg.PutBudget)

	cfg, _, err = BlompConfigFromEnv(env(map[string]string{"BLOMP_USER": "a@b.c", "BLOMP_PASSWORD": "p", "BLOMP_SEGMENT_SIZE": "256MiB"}))
	require.NoError(t, err)
	assert.Equal(t, int64(256<<20), cfg.SegmentSize)

	for _, bad := range []map[string]string{
		{"BLOMP_PASSWORD": "p"},
		{"BLOMP_USER": "a@b.c", "BLOMP_PASSWORD": "p", "BLOMP_SEGMENT_SIZE": "6GiB"},
		{"BLOMP_USER": "a@b.c", "BLOMP_PASSWORD": "p", "BLOMP_SEGMENT_SIZE": "512KiB"},
		{"BLOMP_USER": "a@b.c", "BLOMP_PASSWORD": "p", "BLOMP_SEGMENT_SIZE": "lots"},
		{"BLOMP_USER": "a@b.c", "BLOMP_PASSWORD": "p", "BLOMP_AUTH_URL": "ftp://x"},
		{"BLOMP_USER": "a@b.c", "BLOMP_PASSWORD": "p", "BLOMP_PUT_CONCURRENCY": "0"},
		{"BLOMP_USER": "a@b.c", "BLOMP_PASSWORD": "p", "BLOMP_PUT_CONCURRENCY": "17"},
		{"BLOMP_USER": "a@b.c", "BLOMP_PASSWORD": "p", "BLOMP_PUT_CONCURRENCY": "four"},
		{"BLOMP_USER": "a@b.c", "BLOMP_PASSWORD": "p", "BLOMP_PUT_BUDGET": "32MiB"}, // below one 64 MiB segment
	} {
		_, ok, err := BlompConfigFromEnv(env(bad))
		assert.Error(t, err, "%v", bad)
		assert.False(t, ok)
	}
}

func countWalk(t *testing.T, d *SwiftDriver, tenant, container string) int {
	t.Helper()
	n := 0
	err := d.WalkTenant(context.Background(), tenant, func(o engine.TenantObject) error {
		if o.Container == container {
			n++
		}
		return nil
	})
	require.NoError(t, err)
	return n
}

// The pieces of a large object go up concurrently, at most PutConcurrency
// at a time, and land in order in the manifest.
func TestSwift_LargeObjectPiecesGoUpInParallel(t *testing.T) {
	f := newFakeSwift(t)
	f.putDelay = 30 * time.Millisecond
	cfg := f.config()
	cfg.PutConcurrency = 4
	d, err := NewSwiftDriver("swift-fake", cfg, zap.NewNop())
	require.NoError(t, err)
	ctx := swiftCtx("t1")
	body := randomBytes(t, 10*1024+300)

	require.NoError(t, d.Put(ctx, "b1", "big", bytes.NewReader(body), engine.WithContentLength(int64(len(body)))))

	assert.Equal(t, 4, f.peakSegmentPuts(), "four pieces in flight at once")
	o, ok := f.object("t-t1/b1/big")
	require.True(t, ok)
	assert.Len(t, o.manifest, 11)
	assert.Equal(t, body, o.data, "the manifest names the pieces in order")
}

// The process-wide budget bounds the pieces held in memory whatever the
// concurrency: an upload waits for it, never fails.
func TestSwift_PutBudgetBoundsPiecesInFlight(t *testing.T) {
	f := newFakeSwift(t)
	f.putDelay = 20 * time.Millisecond
	cfg := f.config()
	cfg.PutConcurrency = 8
	cfg.PutBudget = 2 * cfg.SegmentSize
	d, err := NewSwiftDriver("swift-fake", cfg, zap.NewNop())
	require.NoError(t, err)
	ctx := swiftCtx("t1")
	var wg sync.WaitGroup
	for i := range 3 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			body := randomBytes(t, 6*1024)
			assert.NoError(t, d.Put(ctx, "b1", fmt.Sprintf("big%d", i), bytes.NewReader(body), engine.WithContentLength(int64(len(body)))))
		}()
	}
	wg.Wait()
	assert.LessOrEqual(t, f.peakSegmentPuts(), 2)
	assert.Len(t, f.names(), 3*6+3)
}

// A piece is held in memory, so a 5xx on it is retried.
func TestSwift_PieceRetriedOnA5xx(t *testing.T) {
	f := newFakeSwift(t)
	d, err := NewSwiftDriver("swift-fake", f.config(), zap.NewNop())
	require.NoError(t, err)
	ctx := swiftCtx("t1")
	d.segmentBackoff = func(int) time.Duration { return time.Millisecond }
	body := randomBytes(t, 3000)
	f.failNext(http.MethodPut, http.StatusServiceUnavailable, http.StatusBadGateway)

	require.NoError(t, d.Put(ctx, "b1", "big", bytes.NewReader(body), engine.WithContentLength(int64(len(body)))))
	o, ok := f.object("t-t1/b1/big")
	require.True(t, ok)
	assert.Equal(t, body, o.data)
}

// One piece that keeps failing fails the upload: the others are stopped
// and every piece written is removed.
func TestSwift_FailedPieceRemovesTheOthers(t *testing.T) {
	f := newFakeSwift(t)
	cfg := f.config()
	cfg.PutConcurrency = 3
	d, err := NewSwiftDriver("swift-fake", cfg, zap.NewNop())
	require.NoError(t, err)
	d.client.Transport = &failPiece{rt: d.client.Transport, suffix: "/00000004", times: -1}
	d.segmentBackoff = func(int) time.Duration { return time.Millisecond }
	ctx := swiftCtx("t1")
	body := randomBytes(t, 9*1024)

	err = d.Put(ctx, "b1", "big", bytes.NewReader(body), engine.WithContentLength(int64(len(body))))
	require.Error(t, err)
	var se *SwiftError
	require.ErrorAs(t, err, &se)
	assert.Equal(t, http.StatusServiceUnavailable, se.Status)
	assert.Empty(t, f.names(), "every piece written was bulk-deleted")
	assert.Equal(t, 1, f.count("POST bulk-delete"))
}

// Blomp 503s a piece now and then (prod 2026-10-21: three 6 GiB uploads in
// a row). A piece is sent up to 5 times, so four 503s on one still land it.
func TestSwift_PieceSurvivesFour503s(t *testing.T) {
	f := newFakeSwift(t)
	d, err := NewSwiftDriver("swift-fake", f.config(), zap.NewNop())
	require.NoError(t, err)
	fp := &failPiece{rt: d.client.Transport, suffix: "/00000001", times: 4}
	d.client.Transport = fp
	var waits []time.Duration
	var mu sync.Mutex
	d.segmentBackoff = func(a int) time.Duration {
		mu.Lock()
		waits = append(waits, swiftSegmentBackoff(a))
		mu.Unlock()
		return time.Millisecond
	}
	ctx := swiftCtx("t1")
	body := randomBytes(t, 3000)

	require.NoError(t, d.Put(ctx, "b1", "big", bytes.NewReader(body), engine.WithContentLength(int64(len(body)))))
	o, ok := f.object("t-t1/b1/big")
	require.True(t, ok)
	assert.Equal(t, body, o.data)
	require.Len(t, waits, 4)
	for i, w := range waits {
		base := time.Second << i
		assert.GreaterOrEqual(t, w, base)
		assert.LessOrEqual(t, w, base+base/2)
	}
}

// failPiece answers 503 to the first `times` PUTs (-1 = every one) of a
// piece whose name ends in suffix.
type failPiece struct {
	rt     http.RoundTripper
	suffix string
	mu     sync.Mutex
	times  int
}

func (p *failPiece) fail() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.times == 0 {
		return false
	}
	if p.times > 0 {
		p.times--
	}
	return true
}

func (p *failPiece) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.Method == http.MethodPut && strings.HasSuffix(r.URL.Path, p.suffix) && p.fail() {
		if r.Body != nil {
			_, _ = io.Copy(io.Discard, r.Body)
			_ = r.Body.Close()
		}
		return &http.Response{StatusCode: http.StatusServiceUnavailable, Body: io.NopCloser(strings.NewReader("")),
			Header: http.Header{}, Request: r}, nil
	}
	return p.rt.RoundTrip(r)
}

// Swift's max_manifest_segments is 1000: a known size grows the piece to
// fit; an unknown one past 1000 pieces is refused and its pieces removed.
func TestSwift_ThousandPieceLimit(t *testing.T) {
	f := newFakeSwift(t)
	d, err := NewSwiftDriver("swift-fake", f.config(), zap.NewNop())
	require.NoError(t, err)
	ctx := swiftCtx("t1")

	body := randomBytes(t, 1000*1024+5000)
	require.NoError(t, d.Put(ctx, "b1", "known", bytes.NewReader(body), engine.WithContentLength(int64(len(body)))))
	o, ok := f.object("t-t1/b1/known")
	require.True(t, ok)
	assert.LessOrEqual(t, len(o.manifest), 1000)
	assert.Equal(t, body, o.data)
	require.NoError(t, d.Delete(ctx, "b1", "known"))

	big := randomBytes(t, swiftPeekBytes+1500) // 8 MiB of 1 KiB pieces
	err = d.Put(ctx, "b1", "unknown", struct{ io.Reader }{bytes.NewReader(big)})
	assert.ErrorIs(t, err, engine.ErrInvalidInput)
	assert.Empty(t, f.names(), "the pieces of the refused upload were removed")
}

// GetRangeInfo (engine.VersionedRangeGetter): every range of one object
// reports the same identity — the parallel ranged GET compares them.
func TestSwift_GetRangeInfo(t *testing.T) {
	for _, tg := range swiftTargets(t) {
		t.Run(tg.name, func(t *testing.T) {
			tenant := swiftTenant(t)
			cleanupTenant(t, tg.drv, tenant)
			ctx := swiftCtx(tenant)
			seg := int(tg.drv.cfg.SegmentSize)
			for _, n := range []int{5000, seg*2 + 100} { // a plain object, a large one
				key := fmt.Sprintf("obj-%d", n)
				body := randomBytes(t, n)
				require.NoError(t, tg.drv.Put(ctx, "b1", key, bytes.NewReader(body), engine.WithContentLength(int64(n))))

				rc, a, err := tg.drv.GetRangeInfo(ctx, "b1", key, 0, 100)
				assert.Equal(t, body[:100], readAll(t, rc, err))
				rc, b, err := tg.drv.GetRangeInfo(ctx, "b1", key, int64(n-50), 50)
				assert.Equal(t, body[n-50:], readAll(t, rc, err))
				assert.Equal(t, int64(n), a.Size)
				assert.Equal(t, a, b, "one identity for every range")
				assert.NotEmpty(t, a.ETag)
				assert.NotContains(t, a.ETag, `"`)
			}
			_, _, err := tg.drv.GetRangeInfo(ctx, "b1", "absent", 0, 10)
			assert.True(t, engine.IsObjectMiss(err), "%v", err)
		})
	}
}

func TestSwift_GetRangeInfoWhenTheRangeIsIgnored(t *testing.T) {
	f := newFakeSwift(t)
	d, err := NewSwiftDriver("swift-fake", f.config(), zap.NewNop())
	require.NoError(t, err)
	ctx := swiftCtx("t1")
	require.NoError(t, d.Put(ctx, "b1", "k", strings.NewReader("0123456789")))
	f.ignoreRange = true
	rc, info, err := d.GetRangeInfo(ctx, "b1", "k", 3, 4)
	assert.Equal(t, []byte("3456"), readAll(t, rc, err))
	assert.Equal(t, int64(10), info.Size)
	assert.Equal(t, md5hex([]byte("0123456789")), info.ETag)
}
