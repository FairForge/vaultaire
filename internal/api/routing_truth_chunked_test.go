package api

// A chunked object's head row is not routing truth: its bytes are the
// content index's (chunk rows carry their own backend_id, and the job checks
// them chunk by chunk). The row's backend_name is 'chunked' when every chunk
// of a PUT was a dedup hit, and '' on a manifest copy — names no driver has.
// The boot check and the job counted them as "rows on no registered backend"
// and the read counter fired on every GET of one (plan-driver review of
// WP-R7-5, #560).

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

func TestRoutingTruth_ChunkedRowsAreNeverUnknownBackendRows(t *testing.T) {
	f := setupChunkedCopyFixture(t)
	content := generateTestData(8 * 1024)
	f.seedChunked(t, "first.bin", content)
	f.seedChunked(t, "again.bin", content)                       // every chunk a dedup hit → backend_name 'chunked'
	w := f.copyObject(t, "first.bin", "dest-bucket", "copy.bin") // a manifest copy → backend_name ''
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	var names []string
	rows, err := f.db.Query(`SELECT DISTINCT COALESCE(backend_name, '<NULL>') FROM object_head_cache WHERE tenant_id = $1 AND is_chunked ORDER BY 1`, f.tenantID)
	require.NoError(t, err)
	for rows.Next() {
		var n string
		require.NoError(t, rows.Scan(&n))
		names = append(names, n)
	}
	require.NoError(t, rows.Err())
	require.NoError(t, rows.Close())
	require.Contains(t, names, "chunked", "the all-dedup PUT stamps the sentinel (precondition of this test)")
	require.Contains(t, names, "", "the manifest copy stamps '' (precondition of this test)")

	c := NewRoutingTruthChecker(f.db, f.server.engine, f.adapter.gci, zap.NewNop())
	require.NotNil(t, c)
	c.scopeTenant = f.tenantID

	unknown, err := c.unknownBackendRows(context.Background())
	require.NoError(t, err)
	assert.Empty(t, unknown, "a chunked row's backend_name is not routing truth: %+v", unknown)

	res, err := c.RunOnce(context.Background())
	require.NoError(t, err)
	assert.Empty(t, res.Unknown, "the job reports no unknown backend for chunked rows: %+v", res.Unknown)
	assert.Equal(t, 3, res.Chunks.Objects, "the chunked rows are checked chunk by chunk instead")

	// A GET of the all-dedup object: served, and not counted as a read of an
	// unknown backend.
	before := testutil.ToFloat64(routingUnknownReads.WithLabelValues("get", "chunked"))
	req := httptest.NewRequest("GET", "/test-bucket/again.bin", nil)
	req = req.WithContext(s3Ctx(req.Context(), f.tenant))
	rec := httptest.NewRecorder()
	f.adapter.HandleGet(rec, req, "test-bucket", "again.bin")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, content, rec.Body.Bytes())
	assert.Equal(t, before, testutil.ToFloat64(routingUnknownReads.WithLabelValues("get", "chunked")), "no unknown-backend read counted for a chunked row")
}
