package api

// WP-R10-8 Part D (R10-25, R10-19). quota_usage_events got one keyless
// 'RESERVE' row per reservation — on every write's hot path — and
// GetUsageHistory summed 'PUT'/'DELETE' rows nobody wrote. And a floor row
// SetHouse created while a PUT was in flight was seeded without it.

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/FairForge/vaultaire/internal/usage"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWrites_RecordNoUsageEventRow(t *testing.T) {
	// Before: 3 rows (one 'RESERVE' per reservation, no key).
	f := setupQuotaAccountingFixture(t, 1<<30)
	require.Equal(t, http.StatusOK, f.put(t, "a", testBytes(1000)))
	require.Equal(t, http.StatusOK, f.put(t, "b", testBytes(2000)))
	require.Equal(t, http.StatusOK, f.put(t, "a", testBytes(3000)))
	var n int
	require.NoError(t, f.db.QueryRow(`SELECT COUNT(*) FROM quota_usage_events WHERE tenant_id = $1`, f.tenantID).Scan(&n))
	assert.Zero(t, n)
}

func TestUsageHistory_IsTheMeteredDailyTraffic(t *testing.T) {
	// Before: one row per day of events with uploaded = deleted = 0 and
	// peak_usage = the largest reservation; nothing for a day with traffic
	// but no reservation.
	f := setupQuotaAccountingFixture(t, 1<<30)
	_, err := f.db.Exec(`INSERT INTO bandwidth_usage_daily (tenant_id, date, ingress_bytes, egress_bytes, requests_count) VALUES
		($1, (NOW() AT TIME ZONE 'UTC')::date, 5000, 7000, 12),
		($1, (NOW() AT TIME ZONE 'UTC')::date - 1, 100, 0, 1),
		($1, (NOW() AT TIME ZONE 'UTC')::date - 40, 9, 9, 9)`, f.tenantID)
	require.NoError(t, err)
	t.Cleanup(func() { _, _ = f.db.Exec(`DELETE FROM bandwidth_usage_daily WHERE tenant_id = $1`, f.tenantID) })

	h, err := f.qm.GetUsageHistory(context.Background(), f.tenantID, 30)

	require.NoError(t, err)
	require.Len(t, h, 2, "30 days, newest first")
	assert.Equal(t, int64(5000), h[0]["uploaded"])
	assert.Equal(t, int64(7000), h[0]["downloaded"])
	assert.Equal(t, int64(12), h[0]["requests"])
	assert.Equal(t, int64(100), h[1]["uploaded"])
}

func TestSetHouse_DuringAPut_TheNewFloorRowCountsTheWrite(t *testing.T) {
	// R10-19. The PUT reserves on a tenant with no floor rows; the house is
	// bought while its body streams (the floor row seeded from the billed
	// bytes, which do not have it yet); the PUT commits. Before: the floor
	// row stayed 4000 bytes short of the total until a reconcile.
	f := setupQuotaAccountingFixture(t, 1<<30)
	require.Equal(t, http.StatusOK, f.put(t, "first", testBytes(1000)))
	body := &raceBody{Reader: bytes.NewReader(testBytes(4000)), act: func() {
		require.NoError(t, f.qm.SetHouse(context.Background(), f.tenantID, usage.House{StdBytes: 1 << 30}))
	}}
	t.Cleanup(func() { _, _ = f.db.Exec(`DELETE FROM tenant_floor_quotas WHERE tenant_id = $1`, f.tenantID) })
	req := httptest.NewRequest("PUT", "/test-bucket/k", body)
	req.ContentLength = 4000
	req = req.WithContext(f.ctx(req.Context()))
	w := httptest.NewRecorder()

	f.server.handlePutObject(w, req, f.s3Req("test-bucket", "k"))

	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	std, _ := f.floors(t)
	assert.Equal(t, int64(5000), std, "the floor row counts the write in flight")
	f.ledgerAgrees(t, "after the purchase mid-write")
}
