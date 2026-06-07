package handlers

import (
	"html/template"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

func testDedupTemplate(t *testing.T) *template.Template {
	t.Helper()
	tmpl := template.Must(template.New("admin").Parse(
		`{{define "admin"}}{{block "content" .}}{{end}}{{end}}`))
	template.Must(tmpl.Parse(
		`{{define "title"}}Dedup{{end}}` +
			`{{define "content"}}` +
			`<span class="ratio">{{.DedupRatio}}</span>` +
			`<span class="saved">{{.StorageSaved}}</span>` +
			`<span class="logical">{{.LogicalBytes}}</span>` +
			`<span class="physical">{{.PhysicalBytes}}</span>` +
			`<span class="chunks">{{.ChunksProcessed}}</span>` +
			`<span class="comp-ratio">{{.CompressionRatio}}</span>` +
			`<span class="comp-saved">{{.CompressionSaved}}</span>` +
			`<span class="comp-chunks">{{.ChunksCompressed}}</span>` +
			`<span class="comp-skipped">{{.ChunksUncompressed}}</span>` +
			`{{range .TenantTable}}<span class="tenant">{{.Email}} {{.LogicalFmt}} {{.PhysicalFmt}} {{.SavedFmt}} {{.SavedPct}} {{.DedupRatio}}</span>{{end}}` +
			`{{if not .TenantTable}}<p>No tenants</p>{{end}}` +
			`{{end}}`))
	return tmpl
}

func TestHandleAdminDedup_RendersGlobalStats(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	// Global stats query: COUNT(*), SUM(size_bytes), SUM(saved)
	mock.ExpectQuery(`SELECT\s+COUNT\(\*\) as total_chunks`).
		WillReturnRows(sqlmock.NewRows([]string{"total_chunks", "total_bytes", "bytes_saved"}).
			AddRow(int64(100), int64(1024*1024*1024), int64(512*1024*1024)))

	// Compression stats query
	mock.ExpectQuery(`COUNT\(\*\) FILTER \(WHERE compression_algo IS NOT NULL\)`).
		WillReturnRows(sqlmock.NewRows([]string{"chunks_compressed", "chunks_uncompressed", "total_plaintext", "total_stored", "bytes_saved"}).
			AddRow(int64(60), int64(40), int64(1024*1024*1024), int64(800*1024*1024), int64(224*1024*1024)))

	// Per-tenant query: no tenants with chunks
	mock.ExpectQuery(`SELECT DISTINCT t.id, t.email`).
		WillReturnRows(sqlmock.NewRows([]string{"id", "email"}))

	handler := HandleAdminDedup(testDedupTemplate(t), db, zap.NewNop())
	req := httptest.NewRequest("GET", "/admin/dedup", nil)
	req = req.WithContext(adminCtx(t))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	body := w.Body.String()
	// Logical = 1 GB + 512 MB = 1.5 GB, Physical = 1 GB, Ratio = 1.5x
	assert.Contains(t, body, `<span class="ratio">1.5x</span>`)
	assert.Contains(t, body, `<span class="saved">512 MB</span>`)
	assert.Contains(t, body, `<span class="chunks">100</span>`)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestHandleAdminDedup_PerTenantTable(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	// Global stats
	mock.ExpectQuery(`SELECT\s+COUNT\(\*\) as total_chunks`).
		WillReturnRows(sqlmock.NewRows([]string{"total_chunks", "total_bytes", "bytes_saved"}).
			AddRow(int64(200), int64(2*1024*1024*1024), int64(1024*1024*1024)))

	// Compression stats query
	mock.ExpectQuery(`COUNT\(\*\) FILTER \(WHERE compression_algo IS NOT NULL\)`).
		WillReturnRows(sqlmock.NewRows([]string{"chunks_compressed", "chunks_uncompressed", "total_plaintext", "total_stored", "bytes_saved"}).
			AddRow(int64(0), int64(200), int64(2*1024*1024*1024), int64(2*1024*1024*1024), int64(0)))

	// Tenants with chunked objects
	mock.ExpectQuery(`SELECT DISTINCT t.id, t.email`).
		WillReturnRows(sqlmock.NewRows([]string{"id", "email"}).
			AddRow("a0eebc99-9c0b-4ef8-bb6d-6bb9bd380a11", "alice@example.com").
			AddRow("b0eebc99-9c0b-4ef8-bb6d-6bb9bd380a22", "bob@example.com"))

	// Tenant dedup stats for alice (get_tenant_dedup_ratio)
	mock.ExpectQuery(`SELECT \* FROM get_tenant_dedup_ratio`).
		WillReturnRows(sqlmock.NewRows([]string{"logical", "physical", "ratio"}).
			AddRow(int64(1024*1024*1024), int64(768*1024*1024), float64(1.33)))

	// Tenant dedup stats for bob
	mock.ExpectQuery(`SELECT \* FROM get_tenant_dedup_ratio`).
		WillReturnRows(sqlmock.NewRows([]string{"logical", "physical", "ratio"}).
			AddRow(int64(2*1024*1024*1024), int64(1024*1024*1024), float64(2.0)))

	handler := HandleAdminDedup(testDedupTemplate(t), db, zap.NewNop())
	req := httptest.NewRequest("GET", "/admin/dedup", nil)
	req = req.WithContext(adminCtx(t))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	body := w.Body.String()
	assert.Contains(t, body, "alice@example.com")
	assert.Contains(t, body, "bob@example.com")
	assert.Contains(t, body, "1.3x") // alice's ratio
	assert.Contains(t, body, "2.0x") // bob's ratio
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestHandleAdminDedup_EmptyNoChunks(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	// Global stats: no chunks
	mock.ExpectQuery(`SELECT\s+COUNT\(\*\) as total_chunks`).
		WillReturnRows(sqlmock.NewRows([]string{"total_chunks", "total_bytes", "bytes_saved"}).
			AddRow(int64(0), int64(0), int64(0)))

	// Compression stats: empty
	mock.ExpectQuery(`COUNT\(\*\) FILTER \(WHERE compression_algo IS NOT NULL\)`).
		WillReturnRows(sqlmock.NewRows([]string{"chunks_compressed", "chunks_uncompressed", "total_plaintext", "total_stored", "bytes_saved"}).
			AddRow(int64(0), int64(0), int64(0), int64(0), int64(0)))

	// No tenants
	mock.ExpectQuery(`SELECT DISTINCT t.id, t.email`).
		WillReturnRows(sqlmock.NewRows([]string{"id", "email"}))

	handler := HandleAdminDedup(testDedupTemplate(t), db, zap.NewNop())
	req := httptest.NewRequest("GET", "/admin/dedup", nil)
	req = req.WithContext(adminCtx(t))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	body := w.Body.String()
	assert.Contains(t, body, `<span class="ratio">1.0x</span>`)
	assert.Contains(t, body, `<span class="saved">0 B</span>`)
	assert.Contains(t, body, `<span class="chunks">0</span>`)
	assert.Contains(t, body, "No tenants")
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestHandleAdminDedup_NilDB(t *testing.T) {
	handler := HandleAdminDedup(testDedupTemplate(t), nil, zap.NewNop())
	req := httptest.NewRequest("GET", "/admin/dedup", nil)
	req = req.WithContext(adminCtx(t))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	body := w.Body.String()
	assert.Contains(t, body, `<span class="ratio">1.0x</span>`)
	assert.Contains(t, body, `<span class="saved">0 B</span>`)
	assert.Contains(t, body, `<span class="chunks">0</span>`)
}

func TestHandleAdminDedup_NoSession(t *testing.T) {
	handler := HandleAdminDedup(testDedupTemplate(t), nil, zap.NewNop())
	req := httptest.NewRequest("GET", "/admin/dedup", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusSeeOther, w.Code)
	assert.Equal(t, "/login", w.Header().Get("Location"))
}

func TestHandleAdminDedup_DBErrorDegrades(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	// Global stats query fails
	mock.ExpectQuery(`SELECT\s+COUNT\(\*\) as total_chunks`).
		WillReturnError(assert.AnError)

	handler := HandleAdminDedup(testDedupTemplate(t), db, zap.NewNop())
	req := httptest.NewRequest("GET", "/admin/dedup", nil)
	req = req.WithContext(adminCtx(t))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	body := w.Body.String()
	assert.Contains(t, body, `<span class="ratio">1.0x</span>`)
	assert.Contains(t, body, `<span class="saved">0 B</span>`)
}

func TestHandleAdminDedup_CompressionStats(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	// Global dedup stats
	mock.ExpectQuery(`SELECT\s+COUNT\(\*\) as total_chunks`).
		WillReturnRows(sqlmock.NewRows([]string{"total_chunks", "total_bytes", "bytes_saved"}).
			AddRow(int64(50), int64(500*1024*1024), int64(0)))

	// Compression stats: 30 compressed, 20 uncompressed, good ratio
	mock.ExpectQuery(`COUNT\(\*\) FILTER \(WHERE compression_algo IS NOT NULL\)`).
		WillReturnRows(sqlmock.NewRows([]string{"chunks_compressed", "chunks_uncompressed", "total_plaintext", "total_stored", "bytes_saved"}).
			AddRow(int64(30), int64(20), int64(500*1024*1024), int64(300*1024*1024), int64(200*1024*1024)))

	// No tenants
	mock.ExpectQuery(`SELECT DISTINCT t.id, t.email`).
		WillReturnRows(sqlmock.NewRows([]string{"id", "email"}))

	handler := HandleAdminDedup(testDedupTemplate(t), db, zap.NewNop())
	req := httptest.NewRequest("GET", "/admin/dedup", nil)
	req = req.WithContext(adminCtx(t))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	body := w.Body.String()
	// 500 MB / 300 MB = 1.67x
	assert.Contains(t, body, `<span class="comp-ratio">1.7x</span>`)
	assert.Contains(t, body, `<span class="comp-saved">200 MB</span>`)
	assert.Contains(t, body, `<span class="comp-chunks">30</span>`)
	assert.Contains(t, body, `<span class="comp-skipped">20</span>`)
	require.NoError(t, mock.ExpectationsWereMet())
}
