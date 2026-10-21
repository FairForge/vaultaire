package api

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/FairForge/vaultaire/internal/config"
	"github.com/FairForge/vaultaire/internal/drivers"
	"github.com/FairForge/vaultaire/internal/engine"
	"github.com/FairForge/vaultaire/internal/idrive"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/lib/pq"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// WP-CAP-1 (Stage 1a Prompt 21): the backend_plans record, the daily
// backend_capacity job's sums, the vendor view, the gauges from
// job_runs.result, the rule file.

// capFixture isolates its rows under backend names of its own (the sums
// are global on the shared test database: a second tenant's rows on OTHER
// backends must stay untouched, and ours must see nothing else's).
type capFixture struct {
	t       *testing.T
	db      *sql.DB
	tenant  string
	tenant2 string
	backend string // this fixture's own backend name
	leg     string // and its parity leg
}

func setupCapFixture(t *testing.T) *capFixture {
	t.Helper()
	jf := setupJobsFixture(t)
	f := &capFixture{t: t, db: jf.db,
		tenant:  "cap-" + uuid.New().String()[:12],
		tenant2: "cap2-" + uuid.New().String()[:12],
		backend: "capbk-" + uuid.New().String()[:8],
		leg:     "capleg-" + uuid.New().String()[:8]}
	for _, tn := range []string{f.tenant, f.tenant2} {
		_, err := f.db.Exec(`INSERT INTO tenants (id, name, email) VALUES ($1, 'C', $2)`, tn, tn+"@test.local")
		require.NoError(t, err)
	}
	t.Cleanup(func() {
		for _, tn := range []string{f.tenant, f.tenant2} {
			_, _ = f.db.Exec(`DELETE FROM object_versions WHERE tenant_id = $1`, tn)
			_, _ = f.db.Exec(`DELETE FROM object_head_cache WHERE tenant_id = $1`, tn)
			_, _ = f.db.Exec(`DELETE FROM multipart_uploads WHERE tenant_id = $1`, tn)
			_, _ = f.db.Exec(`DELETE FROM vault_parity WHERE tenant_id = $1`, tn)
			_, _ = f.db.Exec(`DELETE FROM retired_blobs WHERE tenant_id = $1`, tn)
			_, _ = f.db.Exec(`DELETE FROM tenants WHERE id = $1`, tn)
		}
		_, _ = f.db.Exec(`DELETE FROM global_content_index WHERE backend_id IN ($1, $2)`, f.backend, f.leg)
		_, _ = f.db.Exec(`DELETE FROM packs WHERE backend IN ($1, $2)`, f.backend, f.leg)
	})
	return f
}

func (f *capFixture) exec(q string, args ...any) {
	f.t.Helper()
	_, err := f.db.Exec(q, args...)
	require.NoError(f.t, err)
}

func TestBackendCapacity_SumsEveryHolderOnce(t *testing.T) {
	// Arrange: one of everything the backend can hold, plus a second
	// tenant's rows on the same database (on another backend name).
	f := setupCapFixture(t)
	bk, leg := f.backend, f.leg
	// A whole head row (100 B) and a chunked one (the sentinel: its bytes
	// are the chunk blobs, never the head row's logical size).
	f.exec(`INSERT INTO object_head_cache (tenant_id, bucket, object_key, size_bytes, etag, content_type, backend_name, is_chunked, floor)
	        VALUES ($1, 'b', 'whole.bin', 100, 'e1', 'text/plain', $2, FALSE, 'standard')`, f.tenant, bk)
	f.exec(`INSERT INTO object_head_cache (tenant_id, bucket, object_key, size_bytes, etag, content_type, backend_name, is_chunked, floor, storage_key)
	        VALUES ($1, 'b', 'big.bin', 999999, 'e2-3', 'text/plain', 'chunked', TRUE, 'standard', 'big.bin@MANIFESTADDR')`, f.tenant)
	// Two chunk blobs of it on the backend: the vendor bills the compressed
	// size (7 + 40 where compressed_size is NULL).
	f.exec(`INSERT INTO global_content_index (dedup_scope, plaintext_hash, backend_id, storage_key, size_bytes, compressed_size, ref_count, state)
	        VALUES ('_global', $1, $2, '_chunks/h1', 10, 7, 1, 'ready')`, "caph1-"+f.tenant, bk)
	f.exec(`INSERT INTO global_content_index (dedup_scope, plaintext_hash, backend_id, storage_key, size_bytes, ref_count, state)
	        VALUES ('_global', $1, $2, '_chunks/h2', 40, 1, 'ready')`, "caph2-"+f.tenant, bk)
	// A pending chunk claim holds no blob yet: never counted.
	f.exec(`INSERT INTO global_content_index (dedup_scope, plaintext_hash, backend_id, storage_key, size_bytes, ref_count, state, pending_since)
	        VALUES ('_global', $1, $2, '_chunks/h3', 500, 0, 'pending', NOW())`, "caph3-"+f.tenant, bk)
	// A retained version sharing the head row's blob (not counted — the
	// head row's charge) and one with its own blob (counted, 25 B).
	f.exec(`INSERT INTO object_versions (tenant_id, bucket, object_key, version_id, size_bytes, etag, is_latest, is_delete_marker, backend_name, storage_key)
	        VALUES ($1, 'b', 'whole.bin', 'v-latest', 100, 'e1', TRUE, FALSE, $2, 'whole.bin')`, f.tenant, bk)
	f.exec(`INSERT INTO object_versions (tenant_id, bucket, object_key, version_id, size_bytes, etag, is_latest, is_delete_marker, backend_name, storage_key)
	        VALUES ($1, 'b', 'whole.bin', 'v-old', 25, 'e0', FALSE, FALSE, $2, 'whole.bin@OLDADDR')`, f.tenant, bk)
	// An active upload's reservation (attributed to the engine primary).
	f.exec(`INSERT INTO multipart_uploads (upload_id, tenant_id, bucket, object_key, status, reserved_bytes, reserved_floor)
	        VALUES ($1, $2, 'b', 'up.bin', 'active', 11, 'standard')`, "upload-"+uuid.New().String()[:8], f.tenant)
	// A parity row: 4 shards of 6 B on the leg.
	f.exec(`INSERT INTO vault_parity (tenant_id, bucket, object_key, etag, size_bytes, stripe_bytes, shard_bytes, shard_prefix, legs, state)
	        VALUES ($1, 'b', 'vault.bin', 'e9', 100, 64, 6, 'pfx', $2, 'complete')`, f.tenant, pq.Array([]string{leg, leg, leg, leg}))
	// A live pack (50 B) and a retired one (never counted).
	f.exec(`INSERT INTO packs (backend, name, folder, size, sha256, member_count) VALUES ($1, 'aa/p1.pack', 'aa', 50, 's', 1)`, leg)
	f.exec(`INSERT INTO packs (backend, name, folder, size, sha256, member_count, retired_at) VALUES ($1, 'aa/p2.pack', 'aa', 70, 's', 1, NOW())`, leg)
	// A displaced blob awaiting the collector: a ROW count, not bytes.
	f.exec(`INSERT INTO retired_blobs (backend_id, container, storage_key, tenant_id, reason, retire_after)
	        VALUES ($1, $2, 'whole.bin@GONE', $3, 'overwritten', NOW())`, bk, f.tenant+"_b", f.tenant)
	// The second tenant's rows, on its own backend: untouched by ours.
	f.exec(`INSERT INTO object_head_cache (tenant_id, bucket, object_key, size_bytes, etag, content_type, backend_name, is_chunked, floor)
	        VALUES ($1, 'b', 'other.bin', 7777, 'e3', 'text/plain', $2, FALSE, 'standard')`, f.tenant2, f.leg)
	eng := engine.NewEngine(nil, zap.NewNop(), nil)
	eng.AddDriver(bk, drivers.NewLocalDriver(t.TempDir(), zap.NewNop()))
	eng.SetPrimary(bk)
	r := NewBackendCapacityRunner(f.db, eng, zap.NewNop())
	r.JobName = "test_backend_capacity_" + uuid.New().String()[:8]

	// Act.
	res, err := r.RunOnce(context.Background())
	require.NoError(t, err)

	// Assert: every holder once, by kind, on the recorded backend.
	b := res.Backends[bk]
	require.NotNil(t, b)
	assert.Equal(t, int64(100), b.Objects, "the whole head row; the chunked sentinel never counts as an object")
	assert.Equal(t, int64(25), b.Versions, "the version sharing the head's blob is the head row's charge")
	assert.Equal(t, int64(47), b.Chunks, "compressed where known (7), stored size where not (40); a pending claim holds nothing")
	assert.Equal(t, int64(11), b.Multipart, "the active upload's holding, on the primary")
	assert.Equal(t, int64(100+25+47+11), b.Total)
	legSums := res.Backends[leg]
	require.NotNil(t, legSums)
	assert.Equal(t, int64(24), legSums.Parity, "4 shards x 6 B on the recorded leg")
	assert.Equal(t, int64(50), legSums.Packs, "live packs only")
	assert.Equal(t, int64(7777), legSums.Objects, "the second tenant's rows count on THEIR backend")
	assert.Equal(t, int64(1), res.RetiredBlobRows[bk], "retired_blobs has no size column: rows, not bytes")
	assert.Contains(t, res.Notes[0], "vendor view off", "no reseller key: a note on an ok run")
}

func TestBackendCapacity_VendorFailureIsANote(t *testing.T) {
	// Arrange: a reseller endpoint that answers 500 (the account-admin API
	// has done that on dead regions) — the run must stay ok.
	f := setupCapFixture(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":{"type":"server_error","code":"boom","message":"region down"}}`))
	}))
	t.Cleanup(srv.Close)
	r := NewBackendCapacityRunner(f.db, nil, zap.NewNop())
	r.JobName = "test_backend_capacity_" + uuid.New().String()[:8]
	r.Reseller = idrive.NewResellerClient(srv.URL, "rk")
	r.ResellerUser = "prod@stored.ge"

	res, err := r.RunOnce(context.Background())

	require.NoError(t, err, "a vendor call that fails is a note, never a failed run")
	require.NotEmpty(t, res.Notes)
	assert.Contains(t, res.Notes[len(res.Notes)-1], "vendor users call failed")
	assert.Empty(t, res.Vendor)

	// And a working vendor fills the view: month-to-date downloads summed.
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/users":
			_, _ = w.Write([]byte(`{"data":[{"email":"prod@stored.ge","storage_used":2.0,"enabled":true}]}`))
		case "/usage_stats":
			_, _ = w.Write([]byte(`[{"date":"d1","uploads":5,"downloads":100,"disk_used":1},{"date":"d2","uploads":0,"downloads":200,"disk_used":2}]`))
		}
	}))
	t.Cleanup(good.Close)
	r.Reseller = idrive.NewResellerClient(good.URL, "rk")
	res, err = r.RunOnce(context.Background())
	require.NoError(t, err)
	v := res.Vendor["idrive"]
	assert.Equal(t, int64(2_000_000_000), v.UsedBytes, "storage_used read as decimal GB")
	assert.Equal(t, int64(300), v.EgressMonthBytes)
	assert.False(t, v.UnitChecked, "nothing of ours to compare against: the unit stays a note")
	assert.Contains(t, v.Note, "unit unverified")
}

// The gauges are read from job_runs.result and backend_plans on scrape —
// right after a fresh Server with no run in process memory (the R13 lesson).
func TestBackendCapacity_GaugesReadFromJobRuns(t *testing.T) {
	jf := setupJobsFixture(t)
	jobName := "test_cap_gauges_" + uuid.New().String()[:8]
	bk := "capgauge-" + uuid.New().String()[:8]
	res := BackendCapacityResult{Backends: map[string]*BackendBytes{
		bk: {Objects: 1000, Chunks: 47, Total: 1047}},
		Vendor: map[string]VendorView{bk: {UsedBytes: 2000, EgressMonthBytes: 300}}}
	raw, err := json.Marshal(res)
	require.NoError(t, err)
	_, err = jf.db.Exec(`INSERT INTO job_runs (job, last_outcome, result) VALUES ($1, 'ok', $2)
	                     ON CONFLICT (job) DO UPDATE SET result = EXCLUDED.result`, jobName, raw)
	require.NoError(t, err)
	mult := 3.0
	require.NoError(t, upsertBackendPlan(context.Background(), jf.db, BackendPlan{
		Backend: bk, CapacityBytes: 1 << 40, EgressFreeMultiple: &mult, PaymentOnFile: true,
		TermStart: time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC), TermEnd: time.Date(2027, 10, 20, 0, 0, 0, 0, time.UTC)}))
	t.Cleanup(func() {
		_, _ = jf.db.Exec(`DELETE FROM job_runs WHERE job = $1`, jobName)
		_, _ = jf.db.Exec(`DELETE FROM backend_plans WHERE backend = $1`, bk)
	})

	// A collector with nothing in process memory but the names.
	c := newCapacityCollector(jf.db, jobName, zap.NewNop())
	series := collectCapacity(t, c)

	assert.Equal(t, 1000.0, series[`vaultaire_backend_stored_bytes{backend="`+bk+`",kind="objects"}`])
	assert.Equal(t, 47.0, series[`vaultaire_backend_stored_bytes{backend="`+bk+`",kind="chunks"}`])
	assert.Equal(t, 2000.0, series[`vaultaire_backend_vendor_used_bytes{backend="`+bk+`"}`])
	assert.Equal(t, 300.0, series[`vaultaire_backend_vendor_egress_month_bytes{backend="`+bk+`"}`])
	assert.Equal(t, float64(1<<40), series[`vaultaire_backend_plan_bytes{backend="`+bk+`"}`])
	assert.Equal(t, float64(time.Date(2027, 10, 20, 0, 0, 0, 0, time.UTC).Unix()),
		series[`vaultaire_backend_plan_term_end_timestamp_seconds{backend="`+bk+`"}`])
	assert.Equal(t, 1.0, series[`vaultaire_backend_plan_payment_on_file{backend="`+bk+`"}`])
	assert.Equal(t, 3.0*1047, series[`vaultaire_backend_egress_free_bytes{backend="`+bk+`"}`],
		"free egress = the plan's multiple x the stored total")
}

// collectCapacity renders a collector's series as name{labels} -> value
// (labels in the registry's alphabetical order).
func collectCapacity(t *testing.T, c *capacityCollector) map[string]float64 {
	t.Helper()
	reg := prometheus.NewRegistry()
	require.NoError(t, reg.Register(c))
	mfs, err := reg.Gather()
	require.NoError(t, err)
	out := map[string]float64{}
	for _, mf := range mfs {
		for _, m := range mf.GetMetric() {
			key := mf.GetName() + "{"
			for i, l := range m.GetLabel() {
				if i > 0 {
					key += ","
				}
				key += l.GetName() + `="` + l.GetValue() + `"`
			}
			key += "}"
			out[key] = m.GetGauge().GetValue()
		}
	}
	return out
}

// The job is registered with the scheduler, daily at 08:30 UTC.
func TestJobsWiring_BackendCapacity0830(t *testing.T) {
	jf := setupJobsFixture(t)
	eng := engine.NewEngine(nil, zap.NewNop(), nil)
	s := NewServer(&config.Config{Server: config.ServerConfig{Port: 8000}}, zap.NewNop(), eng, nil, jf.db)
	j := s.jobs.job(backendCapacityJobName)
	require.NotNil(t, j, "backend_capacity is a job of the scheduler")
	assert.True(t, j.spec.daily())
	assert.Equal(t, 8, j.spec.Hour)
	assert.Equal(t, 30, j.spec.Minute)
	assert.Equal(t, 10*time.Minute, j.spec.MaxRunTime)
}

// --- the plan record (WP-CAP-1 A) ---

type planFixture struct {
	server  *Server
	db      *sql.DB
	adminID string
	userID  string
	backend string
}

func setupPlanFixture(t *testing.T) *planFixture {
	t.Helper()
	jf := setupJobsFixture(t)
	db := jf.db
	adminID, userID := uuid.New().String(), uuid.New().String()
	for id, role := range map[string]string{adminID: "admin", userID: "user"} {
		_, err := db.Exec(`INSERT INTO users (id, email, password_hash, role) VALUES ($1, $2, 'x', $3)`,
			id, fmt.Sprintf("plan-%s-%s@test.local", role, uuid.New().String()[:8]), role)
		require.NoError(t, err)
	}
	backend := "planbk-" + uuid.New().String()[:8]
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM users WHERE id IN ($1, $2)`, adminID, userID)
		_, _ = db.Exec(`DELETE FROM backend_plans WHERE backend = $1`, backend)
		_, _ = db.Exec(`DELETE FROM audit_logs WHERE action = 'backend.plan_set' AND resource = $1`, "backend:"+backend)
	})
	eng := engine.NewEngine(nil, zap.NewNop(), nil)
	eng.AddDriver(backend, drivers.NewLocalDriver(t.TempDir(), zap.NewNop()))
	s := &Server{logger: zap.NewNop(), router: chi.NewRouter(), db: db, engine: eng,
		config: &config.Config{Server: config.ServerConfig{Port: 8000}}}
	s.router.Route("/api/v1/admin", func(r chi.Router) {
		r.Use(func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				ctx := context.WithValue(req.Context(), userIDKey, req.Header.Get("X-Test-User"))
				next.ServeHTTP(w, req.WithContext(ctx))
			})
		})
		s.registerAdminBackendPlanRoutes(r)
	})
	return &planFixture{server: s, db: db, adminID: adminID, userID: userID, backend: backend}
}

func (f *planFixture) call(t *testing.T, as, method, path string, body any) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		require.NoError(t, json.NewEncoder(&buf).Encode(body))
	}
	req := httptest.NewRequest(method, path, &buf)
	req.Header.Set("X-Test-User", as)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	f.server.router.ServeHTTP(w, req)
	var out map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return w, out
}

func validPlanBody(invoice string) map[string]any {
	return map[string]any{
		"vendor": "iDrive e2", "plan_name": "Yearly (1 TB)", "capacity_bytes": int64(1_000_000_000_000),
		"egress_free_multiple": 3, "overage_usd_per_tb_month": 5.12, "egress_usd_per_gb": 0.01,
		"term_start": "2026-10-20T00:00:00Z", "term_end": "2027-10-20T00:00:00Z",
		"auto_renew": true, "payment_on_file": true, "invoice_ref": invoice}
}

func TestBackendPlan_PutValidatesTermAndCapacity(t *testing.T) {
	f := setupPlanFixture(t)
	path := "/api/v1/admin/backends/" + f.backend + "/plan"

	// No row yet: GET is an honest 404.
	w, _ := f.call(t, f.adminID, "GET", path, nil)
	assert.Equal(t, http.StatusNotFound, w.Code)

	// The refusals: capacity, the term order.
	bad := validPlanBody("inv-1")
	bad["capacity_bytes"] = 0
	w, _ = f.call(t, f.adminID, "PUT", path, bad)
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "capacity_bytes")
	bad = validPlanBody("inv-1")
	bad["term_end"] = "2026-10-19T00:00:00Z"
	w, _ = f.call(t, f.adminID, "PUT", path, bad)
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "term_end")

	// A valid row round-trips; a second PUT updates in place.
	w, out := f.call(t, f.adminID, "PUT", path, validPlanBody("invoice 105170"))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Equal(t, "invoice 105170", out["invoice_ref"])
	assert.Equal(t, 3.0, out["egress_free_multiple"])
	w, out = f.call(t, f.adminID, "GET", path, nil)
	require.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, 1.0e12, out["capacity_bytes"])
	assert.Equal(t, f.adminID, out["updated_by"])
	update := validPlanBody("invoice 105171")
	w, out = f.call(t, f.adminID, "PUT", path, update)
	require.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "invoice 105171", out["invoice_ref"])
	var n int
	require.NoError(t, f.db.QueryRow(`SELECT COUNT(*) FROM backend_plans WHERE backend = $1`, f.backend).Scan(&n))
	assert.Equal(t, 1, n, "one row per backend")

	// A non-admin is refused before anything is read.
	w, _ = f.call(t, f.userID, "PUT", path, validPlanBody("x"))
	assert.Equal(t, http.StatusForbidden, w.Code)
}

func TestBackendPlan_UnknownBackend404(t *testing.T) {
	f := setupPlanFixture(t)
	for _, method := range []string{"GET", "PUT"} {
		w, _ := f.call(t, f.adminID, method, "/api/v1/admin/backends/nosuchbackend/plan", validPlanBody("x"))
		assert.Equal(t, http.StatusNotFound, w.Code, method)
		assert.Contains(t, w.Body.String(), "not registered")
	}
	var n int
	require.NoError(t, f.db.QueryRow(`SELECT COUNT(*) FROM backend_plans WHERE backend = 'nosuchbackend'`).Scan(&n))
	assert.Zero(t, n)
}

func TestBackendPlan_AuditRow(t *testing.T) {
	f := setupPlanFixture(t)
	w, _ := f.call(t, f.adminID, "PUT", "/api/v1/admin/backends/"+f.backend+"/plan", validPlanBody("invoice 105170"))
	require.Equal(t, http.StatusOK, w.Code)

	var actor, invoice string
	require.NoError(t, f.db.QueryRow(`
		SELECT COALESCE(performed_by::text, ''), metadata->>'invoice_ref' FROM audit_logs
		 WHERE action = 'backend.plan_set' AND resource = $1`, "backend:"+f.backend).Scan(&actor, &invoice))
	assert.Equal(t, f.adminID, actor)
	assert.Equal(t, "invoice 105170", invoice)
}

// RP26-2: the reseller API may return the managed user's e-mail in another
// case than IDRIVE_RESELLER_USER spells it — the same mailbox.
func TestVendorView_ResellerUserCaseInsensitive(t *testing.T) {
	f := setupCapFixture(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/users":
			_, _ = w.Write([]byte(`{"data":[{"email":"Prod@Stored.GE","storage_used":2.0,"enabled":true}]}`))
		case "/usage_stats":
			_, _ = w.Write([]byte(`[{"date":"d1","uploads":5,"downloads":100,"disk_used":1}]`))
		}
	}))
	t.Cleanup(srv.Close)
	r := NewBackendCapacityRunner(f.db, nil, zap.NewNop())
	r.JobName = "test_backend_capacity_" + uuid.New().String()[:8]
	r.Reseller = idrive.NewResellerClient(srv.URL, "rk")
	r.ResellerUser = "prod@stored.ge"

	res, err := r.RunOnce(context.Background())

	require.NoError(t, err)
	v, ok := res.Vendor["idrive"]
	require.True(t, ok, "the user is found whatever case iDrive returns it in; notes: %v", res.Notes)
	assert.Equal(t, int64(2_000_000_000), v.UsedBytes)
	for _, n := range res.Notes {
		assert.NotContains(t, n, "not in /users")
	}
}
