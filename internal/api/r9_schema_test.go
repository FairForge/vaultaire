// r9_schema_test.go — Review R9: queries that had drifted from the migrated
// schema (object_head_cache.size_bytes/updated_at, bandwidth_usage_daily.
// requests_count). These run against the real migrated test database because
// sqlmock cannot tell a phantom column from a real one (R9-01, R9-02).
package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/FairForge/vaultaire/internal/testutil"
	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

func r9DB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("postgres", testutil.DSN())
	require.NoError(t, err)
	if err := db.Ping(); err != nil {
		_ = db.Close()
		t.Skipf("test database unreachable (%v) — create it with `make test-db`", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// r9Tenant creates a unique tenant with two head-cache rows and one bandwidth
// row, and removes them all on cleanup.
func r9Tenant(t *testing.T, db *sql.DB, prefix string) string {
	t.Helper()
	tid := fmt.Sprintf("%s-%d", prefix, time.Now().UnixNano())
	ctx := context.Background()
	_, err := db.ExecContext(ctx,
		`INSERT INTO tenants (id, name, email, access_key, secret_key) VALUES ($1, $2, $3, $4, 's')`,
		tid, tid, tid+"@r9.test", tid)
	require.NoError(t, err)
	for i, key := range []string{"photos/a.jpg", "photos/b.jpg"} {
		_, err = db.ExecContext(ctx,
			`INSERT INTO object_head_cache (tenant_id, bucket, object_key, size_bytes, etag, content_type, updated_at)
			 VALUES ($1, 'b1', $2, $3, 'etag', 'image/jpeg', NOW())`, tid, key, int64(100*(i+1)))
		require.NoError(t, err)
	}
	_, err = db.ExecContext(ctx,
		`INSERT INTO bandwidth_usage_daily (tenant_id, date, ingress_bytes, egress_bytes, requests_count)
		 VALUES ($1, CURRENT_DATE, 10, 20, 3)`, tid)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM object_head_cache WHERE tenant_id = $1`, tid)
		_, _ = db.Exec(`DELETE FROM bandwidth_usage_daily WHERE tenant_id = $1`, tid)
		_, _ = db.Exec(`DELETE FROM tenants WHERE id = $1`, tid)
	})
	return tid
}

// R9-01: GET /api/v1/manage/buckets/{name}/objects selected `size` and
// `last_modified` — columns object_head_cache never had — and 500'd on every
// call since the endpoint shipped.
func TestMgmtListObjects_MigratedColumns(t *testing.T) {
	db := r9DB(t)
	tid := r9Tenant(t, db, "test-r9-mgmt")
	s := &Server{db: db, logger: zap.NewNop()}

	call := func(query string) (int, []mgmtObject) {
		req := httptest.NewRequest("GET", "/buckets/b1/objects"+query, nil)
		rctx := chi.NewRouteContext()
		rctx.URLParams.Add("name", "b1")
		ctx := context.WithValue(req.Context(), chi.RouteCtxKey, rctx)
		ctx = context.WithValue(ctx, tenantIDKey, tid)
		w := httptest.NewRecorder()
		s.handleMgmtListObjects(w, req.WithContext(ctx))
		var body struct {
			Data []mgmtObject `json:"data"`
		}
		_ = json.Unmarshal(w.Body.Bytes(), &body)
		return w.Code, body.Data
	}

	for _, q := range []string{"", "?prefix=photos/", "?starting_after=photos/a.jpg", "?prefix=photos/&starting_after=photos/a.jpg"} {
		code, objs := call(q)
		require.Equal(t, http.StatusOK, code, "query %q must not 500", q)
		require.NotEmpty(t, objs, "query %q must list objects", q)
		assert.Greater(t, objs[0].Size, int64(0), "size must come from size_bytes")
		assert.False(t, objs[0].LastModified.IsZero(), "last_modified must come from updated_at")
	}
}

// R9-02: the GDPR export swallowed the same phantom-column errors and shipped
// an export with empty objects/bandwidth sections.
func TestAccountExporter_ObjectsAndBandwidth_MigratedColumns(t *testing.T) {
	db := r9DB(t)
	tid := r9Tenant(t, db, "test-r9-export")
	e := NewAccountExporter(db, zap.NewNop())

	objects := e.collectObjects(context.Background(), tid)
	require.Len(t, objects, 2, "export must list the tenant's objects")
	assert.EqualValues(t, 100, objects[0]["size"])

	bw := e.collectBandwidth(context.Background(), tid)
	require.Len(t, bw, 1, "export must include bandwidth usage")
	assert.EqualValues(t, 3, bw[0]["requests"])
}
