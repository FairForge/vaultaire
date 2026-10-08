package api

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/FairForge/vaultaire/internal/drivers"
	"github.com/FairForge/vaultaire/internal/engine"
	"github.com/FairForge/vaultaire/internal/tenant"
	"github.com/FairForge/vaultaire/internal/testutil"
	"github.com/google/uuid"
	"github.com/lib/pq"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// Post-merge review of #631 (Prompt 1b, 2026-10-07): DeleteObjects runs the
// one aftermath per key, and the aftermath fires the bucket notification and
// the object.deleted webhooks — each a goroutine with its own query. A
// 1,000-key batch started 2,000 goroutines against a 50-connection pool to
// find, 2,000 times, that the bucket has no targets and the tenant no
// webhooks. The batch now resolves both ONCE and skips the per-key dispatch
// when there are none; when there are, it still fires per key (AWS fires
// ObjectRemoved per key) with no query.
//
// The test counts the queries the batch makes, through a database/sql
// driver that records every statement text (the only deterministic way to
// see what goroutines that outlive the response do).

// countingSQLDriver wraps lib/pq and records every statement's text.
type countingSQLDriver struct {
	inner driver.Driver
	log   *queryLog
}

type queryLog struct {
	mu       sync.Mutex
	queries  []string
	inFlight map[string]int // statements running now, by text
	peaks    map[string]int // the most that ran at once, by text
	running  int
	peak     int
}

func (l *queryLog) add(q string) {
	l.mu.Lock()
	l.queries = append(l.queries, q)
	l.mu.Unlock()
}

// enter/leave bracket a statement's execution so peaks can be read.
func (l *queryLog) enter(q string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.inFlight == nil {
		l.inFlight, l.peaks = map[string]int{}, map[string]int{}
	}
	l.queries = append(l.queries, q)
	l.inFlight[q]++
	if l.inFlight[q] > l.peaks[q] {
		l.peaks[q] = l.inFlight[q]
	}
	l.running++
	if l.running > l.peak {
		l.peak = l.running
	}
}

func (l *queryLog) leave(q string) {
	l.mu.Lock()
	l.inFlight[q]--
	l.running--
	l.mu.Unlock()
}

func (l *queryLog) resetPeaks() {
	l.mu.Lock()
	l.peaks, l.peak = map[string]int{}, 0
	l.mu.Unlock()
}

// peakOf is the most statements naming table that ran at once.
func (l *queryLog) peakOf(table string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for q, p := range l.peaks {
		if strings.Contains(q, table) && p > n {
			n = p
		}
	}
	return n
}

func (l *queryLog) peakTotal() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.peak
}

func (l *queryLog) reset() {
	l.mu.Lock()
	l.queries = nil
	l.mu.Unlock()
}

// count is the number of recorded statements that name the table.
func (l *queryLog) count(table string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for _, q := range l.queries {
		if strings.Contains(q, table) {
			n++
		}
	}
	return n
}

func (l *queryLog) total() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.queries)
}

func (d *countingSQLDriver) Open(name string) (driver.Conn, error) {
	c, err := d.inner.Open(name)
	if err != nil {
		return nil, err
	}
	return &countingConn{Conn: c, log: d.log}, nil
}

// countingConn forwards every context method database/sql prefers, so no
// statement can bypass the count through a legacy path.
type countingConn struct {
	driver.Conn
	log *queryLog
}

func (c *countingConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	c.log.enter(query)
	defer c.log.leave(query)
	return c.Conn.(driver.QueryerContext).QueryContext(ctx, query, args)
}

func (c *countingConn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	c.log.enter(query)
	defer c.log.leave(query)
	return c.Conn.(driver.ExecerContext).ExecContext(ctx, query, args)
}

func (c *countingConn) PrepareContext(ctx context.Context, query string) (driver.Stmt, error) {
	c.log.add(query)
	return c.Conn.(driver.ConnPrepareContext).PrepareContext(ctx, query)
}

func (c *countingConn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	return c.Conn.(driver.ConnBeginTx).BeginTx(ctx, opts)
}

func (c *countingConn) Ping(ctx context.Context) error { return c.Conn.(driver.Pinger).Ping(ctx) }

func (c *countingConn) CheckNamedValue(nv *driver.NamedValue) error {
	return c.Conn.(driver.NamedValueChecker).CheckNamedValue(nv)
}

func (c *countingConn) ResetSession(ctx context.Context) error {
	return c.Conn.(driver.SessionResetter).ResetSession(ctx)
}

func (c *countingConn) IsValid() bool { return c.Conn.(driver.Validator).IsValid() }

var (
	countingDriverOnce sync.Once
	countingDriverLog  = &queryLog{}
)

// openCountingDB opens the test database through the counting driver with
// prod's pool size.
func openCountingDB(t *testing.T) (*sql.DB, *queryLog) {
	t.Helper()
	countingDriverOnce.Do(func() {
		sql.Register("postgres-counting", &countingSQLDriver{inner: &pq.Driver{}, log: countingDriverLog})
	})
	db, err := sql.Open("postgres-counting", testutil.DSN())
	require.NoError(t, err)
	db.SetMaxOpenConns(50)
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Ping(); err != nil {
		t.Skipf("test database unreachable (%v) — create it with `make test-db`", err)
	}
	return db, countingDriverLog
}

// settled waits until the log has not grown for a while: the per-key
// dispatch goroutines outlive the response.
func (l *queryLog) settled(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	last := l.total()
	quiet := time.Now()
	for time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
		if n := l.total(); n != last {
			last, quiet = n, time.Now()
			continue
		}
		if time.Since(quiet) > 750*time.Millisecond {
			return
		}
	}
	t.Fatal("the queries never settled")
}

type batchFanoutFixture struct {
	t        *testing.T
	db       *sql.DB
	log      *queryLog
	srv      *Server
	tn       *tenant.Tenant
	tenantID string
	bucket   string
}

func setupBatchFanoutFixture(t *testing.T) *batchFanoutFixture {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping database test")
	}
	db, log := openCountingDB(t)
	logger := zap.NewNop()
	eng := engine.NewEngine(nil, logger, nil)
	eng.AddDriver("local", drivers.NewLocalDriver(t.TempDir(), logger))
	eng.SetPrimary("local")

	f := &batchFanoutFixture{t: t, db: db, log: log, tenantID: uuid.New().String(), bucket: "crate"}
	f.tn = &tenant.Tenant{ID: f.tenantID}
	_, err := db.Exec(`INSERT INTO tenants (id, name, email, access_key, secret_key) VALUES ($1,$2,$3,$4,$5)`,
		f.tenantID, "batch fanout", "fanout-"+f.tenantID[:8]+"@test.local", "AK-"+f.tenantID[:8], "SK-"+f.tenantID[:8])
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO buckets (tenant_id, name) VALUES ($1, $2)`, f.tenantID, f.bucket)
	require.NoError(t, err)
	t.Cleanup(func() {
		for _, q := range []string{
			`DELETE FROM webhook_deliveries WHERE webhook_id IN (SELECT id FROM webhook_endpoints WHERE tenant_id = $1)`,
			`DELETE FROM webhook_endpoints WHERE tenant_id = $1`, `DELETE FROM bucket_notifications WHERE tenant_id = $1`,
			`DELETE FROM events WHERE tenant_id = $1`, `DELETE FROM object_head_cache WHERE tenant_id = $1`,
			`DELETE FROM buckets WHERE tenant_id = $1`, `DELETE FROM tenant_quotas WHERE tenant_id = $1`,
			`DELETE FROM tenants WHERE id = $1`,
		} {
			_, _ = db.Exec(q, f.tenantID)
		}
	})
	f.srv = &Server{engine: eng, db: db, logger: logger, smartPromoter: NewSmartPromoter(db, eng, logger)}
	return f
}

// objects plants n head rows (the bytes are not needed: a backend miss is an
// idempotent delete) and returns the DeleteObjects body naming them all.
func (f *batchFanoutFixture) objects(n int) string {
	f.t.Helper()
	_, err := f.db.Exec(`
		INSERT INTO object_head_cache (tenant_id, bucket, object_key, size_bytes, etag, backend_name, floor, is_chunked, content_type)
		SELECT $1, $2, 'k/' || i, 1, 'etag', 'local', 'standard', false, 'application/octet-stream'
		FROM generate_series(1, $3) AS i`, f.tenantID, f.bucket, n)
	require.NoError(f.t, err)
	var b strings.Builder
	b.WriteString("<Delete>")
	for i := 1; i <= n; i++ {
		fmt.Fprintf(&b, "<Object><Key>k/%d</Key></Object>", i)
	}
	b.WriteString("</Delete>")
	return b.String()
}

func (f *batchFanoutFixture) deleteObjects(body string) string {
	f.t.Helper()
	req := httptest.NewRequest("POST", "/"+f.bucket+"?delete", strings.NewReader(body))
	req = req.WithContext(s3Ctx(req.Context(), f.tn))
	w := httptest.NewRecorder()
	f.srv.handleDeleteObjects(w, req, &S3Request{Bucket: f.bucket, TenantID: f.tenantID})
	require.Equal(f.t, http.StatusOK, w.Code, w.Body.String())
	require.NotContains(f.t, w.Body.String(), "<Error>")
	return w.Body.String()
}

func TestDeleteObjects_ABatchWithNoTargetsResolvesThemOnceAndDispatchesNothing(t *testing.T) {
	f := setupBatchFanoutFixture(t)
	body := f.objects(maxBatchDeleteKeys)

	f.log.reset()
	f.deleteObjects(body)
	f.log.settled(t)

	assert.Equal(t, 1, f.log.count("bucket_notifications"), "the bucket's targets are read once per batch, not once per key")
	assert.Equal(t, 1, f.log.count("webhook_endpoints"), "the tenant's webhooks are read once per batch, not once per key")
	assert.Equal(t, maxBatchDeleteKeys, f.log.count("INSERT INTO events"), "the event log still has one row per key")
	var left int
	require.NoError(t, f.db.QueryRow(`SELECT COUNT(*) FROM object_head_cache WHERE tenant_id = $1`, f.tenantID).Scan(&left))
	assert.Equal(t, 0, left)
}

func TestDeleteObjects_ABatchWithTargetsFiresPerKeyFromOneLoad(t *testing.T) {
	f := setupBatchFanoutFixture(t)
	webhookAllowPrivateTargets.Store(true)
	t.Cleanup(func() { webhookAllowPrivateTargets.Store(false) })

	var notifications, webhooks atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/s3":
			notifications.Add(1)
		case "/hook":
			webhooks.Add(1)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(target.Close)
	_, err := f.db.Exec(`INSERT INTO bucket_notifications (tenant_id, bucket, event_filter, target_type, target_url)
		VALUES ($1, $2, 's3:ObjectRemoved:*', 'webhook', $3)`, f.tenantID, f.bucket, target.URL+"/s3")
	require.NoError(t, err)
	_, err = f.db.Exec(`INSERT INTO webhook_endpoints (id, tenant_id, url, event_filter, secret)
		VALUES ($1, $2, $3, '{object.deleted}', 'whsec_test')`, uuid.New().String(), f.tenantID, target.URL+"/hook")
	require.NoError(t, err)

	const n = 200
	body := f.objects(n)
	f.log.reset()
	f.deleteObjects(body)
	f.log.settled(t)

	assert.Equal(t, 1, f.log.count("bucket_notifications"), "one load of the targets")
	assert.Equal(t, 1, f.log.count("webhook_endpoints"), "one load of the webhooks")
	assert.Equal(t, int32(n), notifications.Load(), "s3:ObjectRemoved:Delete is fired per key, as AWS does")
	assert.Equal(t, int32(n), webhooks.Load(), "object.deleted is delivered per key")
	assert.Equal(t, n, f.log.count("INSERT INTO webhook_deliveries"))
}
