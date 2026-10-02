package api

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/FairForge/vaultaire/internal/crypto"
	"github.com/FairForge/vaultaire/internal/drivers"
	"github.com/FairForge/vaultaire/internal/engine"
	"github.com/FairForge/vaultaire/internal/flags"
	"github.com/FairForge/vaultaire/internal/tenant"
	"github.com/FairForge/vaultaire/internal/testutil"
	"github.com/FairForge/vaultaire/internal/usage"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	promtest "github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// Egress allowance enforcement end to end (WP-R10-9): a real HTTP server in
// front of handleS3Request and the /cdn route, the real database, one tenant
// of its own per test. The egress_throttle flag is a fixture switch — a test
// never writes the global flag row on the shared database.

const (
	mib = int64(1) << 20

	egressTestBucket = "pub" // public-read, so the same object is on /cdn
)

type egressFixture struct {
	t        *testing.T
	db       *sql.DB
	srv      *Server
	meter    *egressMeter
	eng      *engine.CoreEngine
	ts       *httptest.Server
	tn       *tenant.Tenant
	tenantID string
	slug     string
	tempDir  string
	enforced atomic.Bool
	// finished holds the ids of requests whose server handler has returned
	// (see settle).
	finished sync.Map
}

// egressTestRequestID marks a fixture request so the test can wait for its
// server handler to return.
const egressTestRequestID = "X-Egress-Test-Request"

// setupEgressFixture builds a tenant with quotaBytes of storage (so an
// allowance of half that) behind a throttle of the given shape. wrap, when
// set, replaces the local driver with a wrapper around it.
func setupEgressFixture(t *testing.T, quotaBytes int64, cfg usage.EgressThrottle, wrap func(engine.Driver) engine.Driver) *egressFixture {
	t.Helper()
	db, err := sql.Open("postgres", testutil.DSN())
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	require.NoError(t, db.Ping())

	logger := zap.NewNop()
	tempDir, err := os.MkdirTemp("", "vaultaire-egress-*")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(tempDir) })

	eng := engine.NewEngine(nil, logger, nil)
	var driver engine.Driver = drivers.NewLocalDriver(tempDir, logger)
	if wrap != nil {
		driver = wrap(driver)
	}
	eng.AddDriver("local", driver)
	eng.SetPrimary("local")

	tenantID := uuid.New().String()
	f := &egressFixture{t: t, db: db, eng: eng, tenantID: tenantID, slug: "eg-" + tenantID[:8], tempDir: tempDir,
		tn: &tenant.Tenant{ID: tenantID, Namespace: "tenant/" + tenantID + "/"}}

	_, err = db.Exec(`INSERT INTO tenants (id, name, email, access_key, secret_key, slug, slug_locked)
		VALUES ($1, 'Egress Test', $2, $3, $4, $5, true)`,
		tenantID, "egress-"+tenantID[:8]+"@test.local", "AK-"+tenantID[:8], "SK-"+tenantID[:8], f.slug)
	require.NoError(t, err)
	qm := usage.NewQuotaManager(db)
	require.NoError(t, qm.CreateTenant(context.Background(), tenantID, "starter", quotaBytes))
	_, err = db.Exec(`INSERT INTO buckets (tenant_id, name, visibility) VALUES ($1, $2, 'public-read')`,
		tenantID, egressTestBucket)
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(filepath.Join(tempDir, f.tn.NamespaceContainer(egressTestBucket)), 0o755))

	f.meter = newEgressMeter(db, cfg, func(string) bool { return f.enforced.Load() }, logger)
	f.srv = &Server{
		logger:           logger,
		router:           chi.NewRouter(),
		engine:           eng,
		quotaManager:     qm,
		db:               db,
		testMode:         true,
		bandwidthTracker: NewBandwidthTracker(db),
		egress:           f.meter,
	}
	f.srv.router.Get("/cdn/{slug}/{bucket}/*", f.srv.handleCDNRequest)
	f.srv.router.Head("/cdn/{slug}/{bucket}/*", f.srv.handleCDNRequest)
	f.ts = httptest.NewServer(f.handler())

	t.Cleanup(func() {
		f.ts.CloseClientConnections()
		f.ts.Close()
		time.Sleep(50 * time.Millisecond) // emitEvent's webhook lookup runs detached
		for _, q := range []string{
			`DELETE FROM bandwidth_alerts WHERE tenant_id = $1`,
			`DELETE FROM webhook_deliveries WHERE endpoint_id IN (SELECT id FROM webhook_endpoints WHERE tenant_id = $1)`,
			`DELETE FROM events WHERE tenant_id = $1`,
			`DELETE FROM feature_flags WHERE tenant_id = $1`,
			`DELETE FROM audit_logs WHERE tenant_id = $1`,
			`DELETE FROM tenant_encryption_keys WHERE tenant_id = $1`,
			`DELETE FROM quota_usage_events WHERE tenant_id = $1`,
			`DELETE FROM tenant_floor_quotas WHERE tenant_id = $1`,
			`DELETE FROM tenant_quotas WHERE tenant_id = $1`,
			`DELETE FROM object_head_cache WHERE tenant_id = $1`,
			`DELETE FROM object_locations WHERE tenant_id = $1`,
			`DELETE FROM buckets WHERE tenant_id = $1`,
			`DELETE FROM bandwidth_usage_daily WHERE tenant_id = $1`,
		} {
			_, _ = db.Exec(q, tenantID)
		}
		cleanupTenantChunkRows(db, tenantID, tenantID)
		_, _ = db.Exec(`DELETE FROM tenants WHERE id = $1`, tenantID)
	})
	return f
}

// handler is the server's two object surfaces: /cdn/* through the router,
// everything else as an authenticated S3 request of the fixture's tenant.
func (f *egressFixture) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if id := r.Header.Get(egressTestRequestID); id != "" {
			defer f.finished.Store(id, struct{}{})
		}
		if strings.HasPrefix(r.URL.Path, "/cdn/") {
			f.srv.router.ServeHTTP(w, r)
			return
		}
		f.srv.handleS3Request(w, r.WithContext(s3Ctx(r.Context(), f.tn)))
	})
}

func (f *egressFixture) allowance() int64 {
	a, err := usage.NewQuotaManager(f.db).EgressAllowance(context.Background(), f.tenantID)
	require.NoError(f.t, err)
	return a.Bytes
}

// mark tags a request so settle can wait for it.
func (f *egressFixture) mark(req *http.Request) string {
	id := uuid.New().String()
	req.Header.Set(egressTestRequestID, id)
	return id
}

// settle returns once the server handler of a marked request has returned.
// A client has the last byte of a response before the server has counted and
// recorded it (the count follows the write), so an assertion on the counter
// straight after a download raced the handler: main went red once on a
// counter exactly one 16 KiB paced slice short (CI run 36953508850).
func (f *egressFixture) settle(id string) {
	f.t.Helper()
	require.Eventually(f.t, func() bool {
		_, done := f.finished.LoadAndDelete(id)
		return done
	}, 10*time.Second, time.Millisecond, "the server handler did not return")
}

// used is the live counter.
func (f *egressFixture) used() int64 {
	n, err := f.meter.usedBytes(context.Background(), f.tenantID, time.Now())
	require.NoError(f.t, err)
	return n
}

// recorded is the month's egress as the database holds it.
func (f *egressFixture) recorded() int64 {
	f.t.Helper()
	n, err := usage.MonthEgressBytes(context.Background(), f.db, f.tenantID, usage.EgressMonthStart(time.Now()))
	require.NoError(f.t, err)
	return n
}

// requireRecordedEqualsLive flushes the tracker until the database holds
// exactly what the live counter holds. (A handler records after its last
// byte is on the wire, so the client can be a few milliseconds ahead of it;
// a double count would leave the database above the counter for good.)
func (f *egressFixture) requireRecordedEqualsLive() {
	f.t.Helper()
	require.Eventually(f.t, func() bool {
		f.srv.bandwidthTracker.Flush()
		return f.recorded() == f.used()
	}, 5*time.Second, 20*time.Millisecond, "database %d vs live counter %d", f.recorded(), f.used())
}

// setUsed puts the tenant's month counter at n, as if it had downloaded it.
func (f *egressFixture) setUsed(n int64) {
	f.meter.tenant(context.Background(), f.tenantID).used.Store(n)
}

func (f *egressFixture) put(key string, size int64) []byte {
	f.t.Helper()
	body := make([]byte, size)
	_, _ = rand.Read(body)
	req, err := http.NewRequest(http.MethodPut, f.ts.URL+"/"+egressTestBucket+"/"+key, bytes.NewReader(body))
	require.NoError(f.t, err)
	req.ContentLength = size
	id := f.mark(req)
	resp, err := f.ts.Client().Do(req)
	require.NoError(f.t, err)
	defer func() { _ = resp.Body.Close() }()
	msg, _ := io.ReadAll(resp.Body)
	require.Equal(f.t, http.StatusOK, resp.StatusCode, string(msg))
	f.settle(id)
	return body
}

type egressResult struct {
	status  int
	header  http.Header
	body    []byte
	elapsed time.Duration
}

// do sends one request and reads the whole response.
func (f *egressFixture) do(method, path string, headers map[string]string) egressResult {
	f.t.Helper()
	req, err := http.NewRequest(method, f.ts.URL+path, nil)
	require.NoError(f.t, err)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	id := f.mark(req)
	start := time.Now()
	resp, err := f.ts.Client().Do(req)
	require.NoError(f.t, err)
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	require.NoError(f.t, err)
	elapsed := time.Since(start)
	f.settle(id)
	return egressResult{status: resp.StatusCode, header: resp.Header, body: body, elapsed: elapsed}
}

func (f *egressFixture) s3Path(key string) string { return "/" + egressTestBucket + "/" + key }
func (f *egressFixture) cdnPath(key string) string {
	return "/cdn/" + f.slug + "/" + egressTestBucket + "/" + key
}

// hold opens a GET and returns once the response headers are in, leaving
// the body unread; the returned func ends it.
func (f *egressFixture) hold(path string) (status int, header http.Header, release func()) {
	f.t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, f.ts.URL+path, nil)
	require.NoError(f.t, err)
	resp, err := f.ts.Client().Do(req)
	require.NoError(f.t, err)
	return resp.StatusCode, resp.Header, func() {
		cancel()
		_ = resp.Body.Close()
	}
}

func engagedCount(surface string) float64 {
	return promtest.ToFloat64(egressEngaged.WithLabelValues(surface))
}
func wouldCount(surface string) float64 {
	return promtest.ToFloat64(egressWouldThrottle.WithLabelValues(surface))
}
func rejectedCount(surface string) float64 {
	return promtest.ToFloat64(egressRejected.WithLabelValues(surface))
}

// egressFastBound is what "not slowed" means in these tests: under the
// 1.4 s an 8 MiB body takes at the test rate, with room for a slow CI
// runner under -race.
const egressFastBound = 1250 * time.Millisecond

// testThrottle is a fast shape for timing tests: 4 MiB/s (a 1 MiB burst).
func testThrottle() usage.EgressThrottle {
	return usage.EgressThrottle{MinBytesPerSec: 4 * mib, Factor: 1, MaxStreams: 16}
}

func TestEgress_UnderTheAllowanceIsFullSpeed(t *testing.T) {
	// Arrange: 64 MiB of quota, so 32 MiB of egress a month.
	f := setupEgressFixture(t, 64*mib, testThrottle(), nil)
	f.enforced.Store(true)
	want := f.put("obj.bin", 8*mib)
	engaged := engagedCount(egressSurfaceS3)

	// Act
	got := f.do(http.MethodGet, f.s3Path("obj.bin"), nil)

	// Assert
	require.Equal(t, http.StatusOK, got.status)
	assert.Equal(t, want, got.body)
	assert.Less(t, got.elapsed, egressFastBound)
	assert.Equal(t, engaged, engagedCount(egressSurfaceS3))
	assert.Equal(t, 8*mib, f.used(), "the counter has exactly the bytes sent")
	assert.Equal(t, 32*mib, f.allowance())
}

func TestEgress_PastTheAllowance_GetIsPaced_PutListHeadAreNot(t *testing.T) {
	// Arrange: the tenant downloads its whole 32 MiB allowance, for real.
	f := setupEgressFixture(t, 64*mib, testThrottle(), nil)
	f.enforced.Store(true)
	want := f.put("obj.bin", 8*mib)
	for i := 0; i < 4; i++ {
		res := f.do(http.MethodGet, f.s3Path("obj.bin"), nil)
		require.Equal(t, http.StatusOK, res.status)
		require.Less(t, res.elapsed, egressFastBound, "download %d is still inside the allowance", i+1)
	}
	require.Equal(t, 32*mib, f.used())
	engaged := engagedCount(egressSurfaceS3)

	// Act: one more download, an upload, a listing and a HEAD.
	get := f.do(http.MethodGet, f.s3Path("obj.bin"), nil)
	putStart := time.Now()
	f.put("second.bin", 8*mib)
	putElapsed := time.Since(putStart)
	list := f.do(http.MethodGet, "/"+egressTestBucket+"?list-type=2", nil)
	head := f.do(http.MethodHead, f.s3Path("obj.bin"), nil)

	// Assert: the GET arrives whole at the capped pace — (8 − 1 burst) MiB
	// at 4 MiB/s = 1.75 s — and nothing else is slowed or refused.
	require.Equal(t, http.StatusOK, get.status)
	assert.Equal(t, want, get.body)
	assert.GreaterOrEqual(t, get.elapsed, 1400*time.Millisecond)
	assert.Less(t, get.elapsed, 10*time.Second)
	assert.Equal(t, engaged+1, engagedCount(egressSurfaceS3))
	assert.Less(t, putElapsed, egressFastBound, "uploads are never slowed for bandwidth")
	assert.Equal(t, http.StatusOK, list.status)
	assert.Contains(t, string(list.body), "second.bin")
	assert.Less(t, list.elapsed, egressFastBound)
	assert.Equal(t, http.StatusOK, head.status)
	assert.Less(t, head.elapsed, egressFastBound)
}

func TestEgress_FlagOff_NothingIsSlowed_TheDecisionIsCounted(t *testing.T) {
	// Arrange: a tenant at three times its allowance; the flag is off.
	f := setupEgressFixture(t, 64*mib, testThrottle(), nil)
	want := f.put("obj.bin", 8*mib)
	f.setUsed(96 * mib)
	would, engaged := wouldCount(egressSurfaceS3), engagedCount(egressSurfaceS3)
	wouldCDN := wouldCount(egressSurfaceCDN)

	// Act
	s3 := f.do(http.MethodGet, f.s3Path("obj.bin"), nil)
	cdn := f.do(http.MethodGet, f.cdnPath("obj.bin"), nil)

	// Assert
	require.Equal(t, http.StatusOK, s3.status)
	require.Equal(t, http.StatusOK, cdn.status)
	assert.Equal(t, want, s3.body)
	assert.Less(t, s3.elapsed, egressFastBound)
	assert.Less(t, cdn.elapsed, egressFastBound)
	assert.Equal(t, would+1, wouldCount(egressSurfaceS3), "the same decision is counted")
	assert.Equal(t, wouldCDN+1, wouldCount(egressSurfaceCDN))
	assert.Equal(t, engaged, engagedCount(egressSurfaceS3))
}

func TestEgress_PerTenantFlagRowIsTheExemption(t *testing.T) {
	// Arrange: the flag service as the server wires it. The global "on" is
	// the registered default here — a test must not write the '*' row on
	// the shared database — and the tenant's own row turns it off.
	f := setupEgressFixture(t, 64*mib, testThrottle(), nil)
	svc := flags.New(f.db, zap.NewNop())
	svc.Register(flagEgressThrottle, true)
	require.NoError(t, svc.Refresh(context.Background()))
	f.meter.enforced = func(id string) bool { return svc.Enabled(flagEgressThrottle, id) }
	f.put("obj.bin", 8*mib)
	f.setUsed(96 * mib)
	would := wouldCount(egressSurfaceS3)

	// Act 1: exempt.
	require.NoError(t, svc.Set(context.Background(), flagEgressThrottle, f.tenantID, false, "egress-test"))
	exempt := f.do(http.MethodGet, f.s3Path("obj.bin"), nil)

	// Assert 1
	require.Equal(t, http.StatusOK, exempt.status)
	assert.Less(t, exempt.elapsed, egressFastBound)
	assert.Equal(t, would+1, wouldCount(egressSurfaceS3))

	// Act 2: the exemption is removed.
	require.NoError(t, svc.Unset(context.Background(), flagEgressThrottle, f.tenantID))
	paced := f.do(http.MethodGet, f.s3Path("obj.bin"), nil)

	// Assert 2
	require.Equal(t, http.StatusOK, paced.status)
	assert.GreaterOrEqual(t, paced.elapsed, 1400*time.Millisecond)
}

func TestEgressThrottleFlag_RegisteredDefaultOff(t *testing.T) {
	s := &Server{flags: flags.New(nil, zap.NewNop())}
	s.flags.Register(flagEgressThrottle, false)

	assert.Equal(t, "egress_throttle", flagEgressThrottle)
	assert.False(t, s.flags.Enabled(flagEgressThrottle, "any-tenant"))
}

func TestEgress_CDNSharesTheTenantBucket(t *testing.T) {
	// Arrange: over the allowance; the same object is public on /cdn.
	f := setupEgressFixture(t, 64*mib, testThrottle(), nil)
	f.enforced.Store(true)
	want := f.put("obj.bin", 8*mib)
	f.setUsed(96 * mib)
	engaged := engagedCount(egressSurfaceCDN)

	// Act: a full download and a 4 MiB range.
	full := f.do(http.MethodGet, f.cdnPath("obj.bin"), nil)
	ranged := f.do(http.MethodGet, f.cdnPath("obj.bin"), map[string]string{"Range": "bytes=0-4194303"})

	// Assert
	require.Equal(t, http.StatusOK, full.status)
	assert.Equal(t, want, full.body)
	assert.GreaterOrEqual(t, full.elapsed, 1400*time.Millisecond)
	require.Equal(t, http.StatusPartialContent, ranged.status)
	assert.Equal(t, want[:4*mib], ranged.body)
	assert.GreaterOrEqual(t, ranged.elapsed, 700*time.Millisecond, "4 MiB at 4 MiB/s after the bucket was drained")
	assert.Equal(t, engaged+2, engagedCount(egressSurfaceCDN))
	assert.Equal(t, 96*mib+12*mib, f.used(), "CDN bytes land on the same counter")
}

func TestEgress_CrossingTheAllowanceMidDownloadSlowsDown(t *testing.T) {
	// Arrange: an 8 MiB allowance and one 16 MiB object.
	f := setupEgressFixture(t, 16*mib, testThrottle(), nil)
	f.enforced.Store(true)
	want := f.put("big.bin", 16*mib)
	require.Equal(t, int64(0), f.used())
	engaged := engagedCount(egressSurfaceS3)

	// Act
	got := f.do(http.MethodGet, f.s3Path("big.bin"), nil)

	// Assert: the first 8 MiB at full speed, the rest at 4 MiB/s — about
	// (8 − 1 burst) / 4 = 1.75 s. Without the mid-stream re-evaluation it
	// would finish in milliseconds.
	require.Equal(t, http.StatusOK, got.status)
	assert.Equal(t, want, got.body)
	assert.GreaterOrEqual(t, got.elapsed, 1300*time.Millisecond)
	assert.Equal(t, engaged+1, engagedCount(egressSurfaceS3))
}

func TestEgress_ParallelDownloadsCannotRunPastTheLineAtFullSpeed(t *testing.T) {
	// Arrange: 64 KiB short of a 64 MiB allowance; eight 16 MiB downloads
	// start together. Counted at request end (as before WP-R10-9) all 128 MiB
	// would leave at full speed.
	const streams = 8
	cfg := usage.EgressThrottle{MinBytesPerSec: 1 * mib, Factor: 1, MaxStreams: 16}
	f := setupEgressFixture(t, 128*mib, cfg, nil)
	f.enforced.Store(true)
	f.put("big.bin", 16*mib)
	f.setUsed(f.allowance() - 64<<10)
	engaged := engagedCount(egressSurfaceS3)

	// Act: run them for 1.5 s.
	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()
	var received atomic.Int64
	var wg sync.WaitGroup
	for i := 0; i < streams; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req, _ := http.NewRequestWithContext(ctx, http.MethodGet, f.ts.URL+f.s3Path("big.bin"), nil)
			resp, err := f.ts.Client().Do(req)
			if err != nil {
				return
			}
			defer func() { _ = resp.Body.Close() }()
			buf := make([]byte, 64<<10)
			for {
				n, err := resp.Body.Read(buf)
				received.Add(int64(n))
				if err != nil {
					return
				}
			}
		}()
	}
	wg.Wait()

	// Assert: what got out is the 64 KiB that was left, at most one
	// re-evaluation interval (1 MiB) per stream, and 1.5 s at 1 MiB/s plus
	// the burst — not 128 MiB.
	slack := int64(64<<10) + streams*egressReevalBytes + 2*mib + mib
	assert.LessOrEqual(t, received.Load(), slack)
	assert.LessOrEqual(t, f.used()-f.allowance(), slack-int64(64<<10))
	assert.Equal(t, engaged+streams, engagedCount(egressSurfaceS3), "every stream joined the paced set")
}

func TestEgress_StreamGuard_503OnS3_429OnCDN_PerSurface(t *testing.T) {
	// Arrange: at most two paced responses per surface; the tenant is over.
	cfg := usage.EgressThrottle{MinBytesPerSec: 65536, Factor: 1, MaxStreams: 2}
	f := setupEgressFixture(t, 64*mib, cfg, nil)
	f.enforced.Store(true)
	f.put("obj.bin", 8*mib)
	f.setUsed(96 * mib)
	rejS3, rejCDN := rejectedCount(egressSurfaceS3), rejectedCount(egressSurfaceCDN)

	// Act 1: two S3 downloads are in progress; a third arrives.
	st1, _, release1 := f.hold(f.s3Path("obj.bin"))
	st2, _, release2 := f.hold(f.s3Path("obj.bin"))
	defer release2()
	third := f.do(http.MethodGet, f.s3Path("obj.bin"), nil)

	// Assert 1: 503 SlowDown + Retry-After — the SDKs' retryable shape.
	require.Equal(t, http.StatusOK, st1)
	require.Equal(t, http.StatusOK, st2)
	assert.Equal(t, http.StatusServiceUnavailable, third.status)
	assert.Contains(t, string(third.body), "<Code>SlowDown</Code>")
	assert.Equal(t, egressRetryAfter, third.header.Get("Retry-After"))
	assert.Equal(t, rejS3+1, rejectedCount(egressSurfaceS3))

	// Act 2: the S3 slots are full; the public bucket is read on /cdn.
	c1, _, releaseC1 := f.hold(f.cdnPath("obj.bin"))
	defer releaseC1()
	c2, _, releaseC2 := f.hold(f.cdnPath("obj.bin"))
	defer releaseC2()
	c3 := f.do(http.MethodGet, f.cdnPath("obj.bin"), nil)

	// Assert 2: the guard is per surface — anonymous readers holding every
	// CDN slot do not lock the owner out of S3, and the reverse.
	assert.Equal(t, http.StatusOK, c1)
	assert.Equal(t, http.StatusOK, c2)
	assert.Equal(t, http.StatusTooManyRequests, c3.status)
	assert.Equal(t, egressRetryAfter, c3.header.Get("Retry-After"))
	assert.Equal(t, rejCDN+1, rejectedCount(egressSurfaceCDN))

	// Act 3: uploads, listings, HEAD and a miss while both guards are full.
	putStart := time.Now()
	f.put("during.bin", mib)
	list := f.do(http.MethodGet, "/"+egressTestBucket+"?list-type=2", nil)
	head := f.do(http.MethodHead, f.s3Path("obj.bin"), nil)
	cdnHead := f.do(http.MethodHead, f.cdnPath("obj.bin"), nil)

	// Assert 3: never refused for bandwidth.
	assert.Less(t, time.Since(putStart), 2*time.Second)
	assert.Equal(t, http.StatusOK, list.status)
	assert.Equal(t, http.StatusOK, head.status)
	assert.Equal(t, http.StatusOK, cdnHead.status)

	// Act 4: one S3 download ends; the next request takes its slot.
	release1()
	var st4 int
	require.Eventually(t, func() bool {
		var rel func()
		st4, _, rel = f.hold(f.s3Path("obj.bin"))
		rel()
		return st4 == http.StatusOK
	}, 3*time.Second, 50*time.Millisecond, "a finished response must give its slot back")
}

// An attacker opening 1,000 connections against a tenant that is past its
// allowance gets sixteen slow streams and 984 refusals; the map holds one
// entry and no response is left pacing after the clients go away.
func TestEgress_AThousandConnections(t *testing.T) {
	cfg := usage.EgressThrottle{MinBytesPerSec: 65536, Factor: 1, MaxStreams: 16}
	f := setupEgressFixture(t, 64*mib, cfg, nil)
	f.enforced.Store(true)
	f.put("obj.bin", 8*mib)
	f.setUsed(96 * mib)
	const conns = 1000
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h := f.handler()

	// Act: straight into the handler (1,000 real sockets would exhaust the
	// test's file descriptors; the handler is what an attacker reaches).
	var refused, served, other atomic.Int64
	var peak atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < conns; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, f.s3Path("obj.bin"), nil).WithContext(ctx)
			h.ServeHTTP(rec, req)
			switch rec.Code {
			case http.StatusServiceUnavailable:
				refused.Add(1)
			case http.StatusOK:
				served.Add(1)
			default:
				other.Add(1)
			}
		}()
	}
	tn := f.meter.tenant(context.Background(), f.tenantID)
	require.Eventually(t, func() bool {
		if n := tn.throttledStreams[0].Load(); n > peak.Load() {
			peak.Store(n)
		}
		return refused.Load() == conns-16
	}, 20*time.Second, 5*time.Millisecond)
	held := tn.throttledStreams[0].Load()
	cancel() // the sixteen clients disconnect
	wg.Wait()

	// Assert
	assert.Equal(t, int64(conns-16), refused.Load())
	assert.Equal(t, int64(16), served.Load())
	assert.Equal(t, int64(0), other.Load())
	assert.Equal(t, int32(16), held)
	assert.LessOrEqual(t, peak.Load(), int32(17), "the count never runs past the guard (one racing Add at most)")
	assert.Equal(t, int32(0), tn.throttledStreams[0].Load(), "every slot is given back")
	assert.Equal(t, int32(0), tn.streams.Load())
	f.meter.mu.Lock()
	assert.Len(t, f.meter.tenants, 1)
	f.meter.mu.Unlock()
}

func TestEgress_NotModifiedHeadAndUnsatisfiableRangeCountNoBody(t *testing.T) {
	// Arrange
	f := setupEgressFixture(t, 64*mib, testThrottle(), nil)
	f.enforced.Store(true)
	f.put("obj.bin", 2*mib)
	first := f.do(http.MethodGet, f.s3Path("obj.bin"), nil)
	require.Equal(t, http.StatusOK, first.status)
	etag := first.header.Get("ETag")
	require.NotEmpty(t, etag)
	before := f.used()

	// Act: five answers that carry no object bytes.
	notModified := f.do(http.MethodGet, f.s3Path("obj.bin"), map[string]string{"If-None-Match": etag})
	head := f.do(http.MethodHead, f.s3Path("obj.bin"), nil)
	headMiss := f.do(http.MethodHead, f.s3Path("nope.bin"), nil)
	unsatisfiable := f.do(http.MethodGet, f.s3Path("obj.bin"), map[string]string{"Range": "bytes=99999999-"})
	cdnNotModified := f.do(http.MethodGet, f.cdnPath("obj.bin"), map[string]string{"If-None-Match": etag})
	cdnHead := f.do(http.MethodHead, f.cdnPath("obj.bin"), nil)

	// Assert: the object is 2 MiB; none of them counted it.
	assert.Equal(t, http.StatusNotModified, notModified.status)
	assert.Equal(t, http.StatusOK, head.status)
	assert.Equal(t, http.StatusNotFound, headMiss.status)
	assert.Equal(t, http.StatusRequestedRangeNotSatisfiable, unsatisfiable.status)
	assert.Equal(t, http.StatusNotModified, cdnNotModified.status)
	assert.Equal(t, http.StatusOK, cdnHead.status)
	assert.Equal(t, before, f.used(), "a 304, a HEAD (hit or miss) and a 416 send no body and count none")

	// And the database agrees with the counter to the byte.
	f.requireRecordedEqualsLive()
}

func TestEgress_RestartMidMonthReloadsTheCounter(t *testing.T) {
	// Arrange: a tenant downloads 24 MiB of its 32 MiB; the process stops
	// (Shutdown flushes the tracker — H-3) and a new one starts.
	f := setupEgressFixture(t, 64*mib, testThrottle(), nil)
	f.enforced.Store(true)
	f.put("obj.bin", 8*mib)
	for i := 0; i < 3; i++ {
		require.Equal(t, http.StatusOK, f.do(http.MethodGet, f.s3Path("obj.bin"), nil).status)
	}
	live := f.used()
	require.Equal(t, 24*mib, live)
	f.requireRecordedEqualsLive()

	// Act: a fresh counter, as after a deploy.
	restarted := newEgressMeter(f.db, testThrottle(), func(string) bool { return true }, zap.NewNop())
	reloaded, err := restarted.usedBytes(context.Background(), f.tenantID, time.Now())
	require.NoError(t, err)
	tn := restarted.tenant(context.Background(), f.tenantID)

	// Assert: the same number, dated on the UTC day, and the allowance with it.
	assert.Equal(t, live, reloaded)
	assert.Equal(t, live, tn.used.Load())
	assert.Equal(t, 32*mib, tn.allowance.Load())
	var day string
	require.NoError(t, f.db.QueryRow(
		`SELECT to_char(date, 'YYYY-MM-DD') FROM bandwidth_usage_daily WHERE tenant_id = $1`, f.tenantID).Scan(&day))
	assert.Equal(t, time.Now().UTC().Format("2006-01-02"), day)

	// One more download crosses the line on the restarted process.
	f.srv.egress = restarted
	f.meter = restarted
	require.Equal(t, http.StatusOK, f.do(http.MethodGet, f.s3Path("obj.bin"), nil).status)
	paced := f.do(http.MethodGet, f.s3Path("obj.bin"), nil)
	assert.GreaterOrEqual(t, paced.elapsed, 1400*time.Millisecond)
}

func TestEgress_AnAbortedCDNDownloadIsStillCounted(t *testing.T) {
	// Arrange: before WP-R10-9 the CDN recorded bytes only when the copy
	// finished; a reader that hung up early cost the tenant nothing.
	f := setupEgressFixture(t, 256*mib, testThrottle(), nil)
	f.put("big.bin", 64*mib)
	before := f.used()

	// Act: read 1 MiB of 64 and hang up.
	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, f.ts.URL+f.cdnPath("big.bin"), nil)
	require.NoError(t, err)
	resp, err := f.ts.Client().Do(req)
	require.NoError(t, err)
	_, err = io.CopyN(io.Discard, resp.Body, mib)
	require.NoError(t, err)
	cancel()
	_ = resp.Body.Close()

	// Assert: the counter has what was sent, and so does the flushed row.
	tn := f.meter.tenant(context.Background(), f.tenantID)
	require.Eventually(t, func() bool { return tn.streams.Load() == 0 }, 5*time.Second, 20*time.Millisecond)
	sent := f.used() - before
	assert.GreaterOrEqual(t, sent, mib)
	f.requireRecordedEqualsLive()
}

// resetDriver hands out readers that fail partway, like a backend that
// resets the connection of a reader it finds too slow.
type resetDriver struct {
	engine.Driver
	gets    atomic.Int64
	failGet atomic.Bool
}

func (d *resetDriver) Get(ctx context.Context, container, artifact string) (io.ReadCloser, error) {
	d.gets.Add(1)
	if d.failGet.Load() {
		return nil, fmt.Errorf("dial tcp: connection refused")
	}
	rc, err := d.Driver.Get(ctx, container, artifact)
	if err != nil {
		return nil, err
	}
	return &resetReader{rc: rc, left: 256 << 10}, nil
}

type resetReader struct {
	rc   io.ReadCloser
	left int
}

func (r *resetReader) Read(p []byte) (int, error) {
	if r.left <= 0 {
		return 0, fmt.Errorf("read tcp: connection reset by peer")
	}
	if len(p) > r.left {
		p = p[:r.left]
	}
	n, err := r.rc.Read(p)
	r.left -= n
	return n, err
}
func (r *resetReader) Close() error { return r.rc.Close() }

func TestEgress_BackendResetMidBodyDoesNotChargeTheBreaker(t *testing.T) {
	// Arrange: a paced tenant reading through a backend that resets every
	// stream after 256 KiB.
	var drv *resetDriver
	f := setupEgressFixture(t, 64*mib, testThrottle(), func(d engine.Driver) engine.Driver {
		drv = &resetDriver{Driver: d}
		return drv
	})
	f.enforced.Store(true)
	f.put("obj.bin", 4*mib)
	f.setUsed(96 * mib)
	getsBefore := drv.gets.Load()

	// Act: twenty downloads die mid-body.
	for i := 0; i < 20; i++ {
		req, err := http.NewRequest(http.MethodGet, f.ts.URL+f.s3Path("obj.bin"), nil)
		require.NoError(t, err)
		resp, err := f.ts.Client().Do(req)
		require.NoError(t, err)
		n, _ := io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		require.Equal(t, http.StatusOK, resp.StatusCode)
		require.Less(t, n, 4*mib, "the body is cut short")
	}

	// Assert: the breaker never saw them — it charges a failed Get, not a
	// failed Read — and the backend is still being asked.
	assert.Equal(t, "closed", f.eng.GetFailoverStatus()["local"])
	assert.Equal(t, getsBefore+20, drv.gets.Load())

	// Control: the same number of failed Gets does open it.
	drv.failGet.Store(true)
	for i := 0; i < 20; i++ {
		_ = f.do(http.MethodGet, f.s3Path("obj.bin"), nil)
	}
	assert.NotEqual(t, "closed", f.eng.GetFailoverStatus()["local"], "control: the breaker does trip on failed Gets")
}

// Every path that sends object bytes writes through the throttle: plain,
// native range, range over a decrypted SSE object, SSE whole, chunked whole
// and chunked range (the adapter), and the CDN's two (TestEgress_CDN…).
func TestEgress_EveryObjectBodyPathGoesThroughTheThrottle(t *testing.T) {
	// Arrange: a fast cap, so the test measures paced bytes, not time.
	cfg := usage.EgressThrottle{MinBytesPerSec: 256 * mib, Factor: 1, MaxStreams: 16}
	f := setupEgressFixture(t, 1024*mib, cfg, nil)
	f.enforced.Store(true)
	sse, err := crypto.NewSSEService(f.db, strings.Repeat("ab", 32))
	require.NoError(t, err)
	gci := crypto.NewGlobalContentIndex(f.db)

	put := func(key string, body []byte, encrypt, chunk bool) {
		a := NewS3ToEngine(f.eng, f.db, zap.NewNop())
		a.sseService = sse
		if chunk {
			a.gci = gci
			a.chunkingThreshold = 1024
		}
		req := httptest.NewRequest(http.MethodPut, f.s3Path(key), bytes.NewReader(body))
		req.ContentLength = int64(len(body))
		if encrypt {
			req.Header.Set("x-amz-server-side-encryption", "AES256")
		}
		rec := httptest.NewRecorder()
		a.HandlePut(rec, req.WithContext(s3Ctx(req.Context(), f.tn)), egressTestBucket, key)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	}
	plain := make([]byte, 3*mib)
	_, _ = rand.Read(plain)
	put("plain.bin", plain, false, false)
	put("sse.bin", plain, true, false)
	put("chunked.bin", plain, false, true)
	var chunked, encAlgo string
	require.NoError(t, f.db.QueryRow(`SELECT is_chunked::text FROM object_head_cache WHERE tenant_id = $1 AND object_key = 'chunked.bin'`, f.tenantID).Scan(&chunked))
	require.Equal(t, "true", chunked)
	require.NoError(t, f.db.QueryRow(`SELECT COALESCE(encryption_algorithm, '') FROM object_head_cache WHERE tenant_id = $1 AND object_key = 'sse.bin'`, f.tenantID).Scan(&encAlgo))
	require.NotEmpty(t, encAlgo)
	f.setUsed(4096 * mib)

	cases := []struct {
		name, key, rng string
		status         int
		want           []byte
	}{
		{"plain whole", "plain.bin", "", 200, plain},
		{"plain range (backend-native)", "plain.bin", "bytes=1048576-2097151", 206, plain[mib : 2*mib]},
		{"SSE whole (one Write of the decrypted object)", "sse.bin", "", 200, plain},
		{"SSE range (sliced from the decrypted bytes)", "sse.bin", "bytes=100-1048675", 206, plain[100 : mib+100]},
		{"chunked whole", "chunked.bin", "", 200, plain},
		{"chunked range", "chunked.bin", "bytes=5-2097156", 206, plain[5 : 2*mib+5]},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := NewS3ToEngine(f.eng, f.db, zap.NewNop())
			a.sseService = sse
			a.gci = gci
			req := httptest.NewRequest(http.MethodGet, f.s3Path(tc.key), nil)
			if tc.rng != "" {
				req.Header.Set("Range", tc.rng)
			}
			req = req.WithContext(s3Ctx(req.Context(), f.tn))
			under := &sliceRecorder{}
			var body bytes.Buffer
			tee := &teeWriter{sliceRecorder: under, buf: &body}
			w, ok := f.meter.admit(tee, req, f.tenantID, egressSurfaceS3)
			require.True(t, ok)
			pacedBefore := promtest.ToFloat64(egressThrottledBytes)

			// Act
			a.HandleGet(w, req, egressTestBucket, tc.key)
			w.close()

			// Assert: the bytes are right and every one was paced, in
			// slices no larger than 16 KiB.
			require.Equal(t, tc.status, statusOr200(under.status))
			assert.Equal(t, tc.want, body.Bytes())
			assert.Equal(t, float64(len(tc.want)), promtest.ToFloat64(egressThrottledBytes)-pacedBefore)
			assert.LessOrEqual(t, under.maxWrite, egressSliceBytes)
		})
	}
}

type teeWriter struct {
	*sliceRecorder
	buf *bytes.Buffer
}

func (w *teeWriter) Write(p []byte) (int, error) {
	w.buf.Write(p)
	return w.sliceRecorder.Write(p)
}

func statusOr200(code int) int {
	if code == 0 {
		return http.StatusOK
	}
	return code
}

// A deploy drains HTTP and then flushes the trackers (H-3). A download still
// open at that moment has not "ended", so before WP-R10-9's drain its bytes
// never reached the database and the restarted process reloaded a counter
// that was short by every response in flight.
func TestEgress_ShutdownRecordsBytesOfResponsesStillInFlight(t *testing.T) {
	// Arrange: a reader takes 1 MiB of a 64 MiB object and stalls.
	f := setupEgressFixture(t, 256*mib, testThrottle(), nil)
	f.put("big.bin", 64*mib)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, f.ts.URL+f.s3Path("big.bin"), nil)
	require.NoError(t, err)
	resp, err := f.ts.Client().Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	_, err = io.CopyN(io.Discard, resp.Body, mib)
	require.NoError(t, err)
	var live int64
	require.Eventually(t, func() bool { // the server fills the socket buffers, then blocks
		n := f.used()
		stable := n == live && n >= mib
		live = n
		return stable
	}, 5*time.Second, 100*time.Millisecond)
	f.srv.bandwidthTracker.Flush()
	require.Equal(t, int64(0), f.recorded(), "nothing is recorded while the response is open")

	// Act 1: the shutdown flush.
	f.srv.flushTrackers(context.Background())

	// Assert 1: the database has what the live counter has.
	assert.Equal(t, live, f.recorded())

	// Act 2: the response then ends (the drain timed out and the handler
	// finished late) and the tracker flushes again.
	cancel()
	tn := f.meter.tenant(context.Background(), f.tenantID)
	require.Eventually(t, func() bool { return tn.streams.Load() == 0 }, 5*time.Second, 20*time.Millisecond)

	// Assert 2: counted once — to the byte, and it stays that way.
	f.requireRecordedEqualsLive()
	time.Sleep(100 * time.Millisecond)
	f.srv.bandwidthTracker.Flush()
	assert.Equal(t, f.used(), f.recorded())
}

// Anyone on the internet can read a public bucket, and what they read is the
// owner's egress: it counts on the owner's month counter and can spend the
// allowance. What that costs the owner is speed, never money and never
// access: the owner's own S3 downloads are then paced, not refused. (The
// owner's levers are the bucket's CDN budget and its visibility.)
func TestEgress_PublicBucketReadersSpendTheOwnersAllowance(t *testing.T) {
	// Arrange: a 32 MiB allowance; strangers fetch a public 8 MiB object.
	f := setupEgressFixture(t, 64*mib, testThrottle(), nil)
	f.enforced.Store(true)
	want := f.put("obj.bin", 8*mib)

	// Act 1: five anonymous downloads.
	var cdn []egressResult
	for i := 0; i < 5; i++ {
		cdn = append(cdn, f.do(http.MethodGet, f.cdnPath("obj.bin"), nil))
	}

	// Assert 1: four at full speed spend the allowance, the fifth is paced.
	for i, res := range cdn {
		require.Equal(t, http.StatusOK, res.status, "download %d", i+1)
	}
	assert.Less(t, cdn[3].elapsed, egressFastBound)
	assert.GreaterOrEqual(t, cdn[4].elapsed, 1400*time.Millisecond)
	assert.Equal(t, 40*mib, f.used())

	// Act 2: the owner downloads over the S3 API.
	own := f.do(http.MethodGet, f.s3Path("obj.bin"), nil)

	// Assert 2: slowed like any download of the account — and whole.
	require.Equal(t, http.StatusOK, own.status)
	assert.Equal(t, want, own.body)
	assert.GreaterOrEqual(t, own.elapsed, 1400*time.Millisecond)
	f.requireRecordedEqualsLive()
}

// Post-merge review: the stream guard's 503 SlowDown is a refusal of one
// client that is past its allowance, not a server failure. The logging
// middleware counted every 5xx into vaultaire_errors_total, which the
// VaultaireServerErrorRatio rule pages on — so any tenant past its allowance
// could page the operator by opening more than MaxStreams downloads. A real
// 5xx still counts.
func TestEgress_StreamGuardRefusalIsNotAServerError(t *testing.T) {
	// Arrange: one paced stream allowed; the tenant is over; the fixture's
	// S3 surface behind the server's logging middleware.
	cfg := usage.EgressThrottle{MinBytesPerSec: 65536, Factor: 1, MaxStreams: 1}
	f := setupEgressFixture(t, 64*mib, cfg, nil)
	f.enforced.Store(true)
	f.put("obj.bin", 8*mib)
	f.setUsed(96 * mib)
	logged := httptest.NewServer(f.srv.loggingMiddleware(f.handler()))
	defer logged.Close()
	get := func() int {
		resp, err := logged.Client().Get(logged.URL + f.s3Path("obj.bin"))
		require.NoError(t, err)
		defer func() { _ = resp.Body.Close() }()
		return resp.StatusCode
	}
	_, _, release := f.hold(f.s3Path("obj.bin"))
	defer release()
	errsBefore, reqsBefore := atomic.LoadInt64(&f.srv.errorCount), atomic.LoadInt64(&f.srv.requestCount)

	// Act: ten more downloads are refused by the guard.
	for i := 0; i < 10; i++ {
		require.Equal(t, http.StatusServiceUnavailable, get())
	}

	// Assert: ten requests, no server error.
	assert.Equal(t, reqsBefore+10, atomic.LoadInt64(&f.srv.requestCount))
	assert.Equal(t, errsBefore, atomic.LoadInt64(&f.srv.errorCount), "a throttle refusal must not feed the 5xx alert")

	// A real server failure on the same path still counts.
	failing := httptest.NewServer(f.srv.loggingMiddleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	})))
	defer failing.Close()
	resp, err := failing.Client().Get(failing.URL)
	require.NoError(t, err)
	_ = resp.Body.Close()
	// The middleware counts after the handler returns; the client is ahead.
	require.Eventually(t, func() bool { return atomic.LoadInt64(&f.srv.errorCount) == errsBefore+1 },
		5*time.Second, time.Millisecond)
}
