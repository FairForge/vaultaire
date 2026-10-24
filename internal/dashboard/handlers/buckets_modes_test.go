package handlers

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/FairForge/vaultaire/internal/modes"
)

// WP-P39 Part B (the tenant-zero proof of modes_copy): with the flag on for
// the tenant, GET /dashboard/buckets labels a Standard bucket "Standard" (the
// name from modes.json) — never "Private", the name trap. HandleBuckets
// rendered the list without ModesCopy, so the page kept the legacy labels with
// the flag on; the template test (dashboard/modes_p26_test.go) set ModesCopy
// itself and never ran the handler.
func TestHandleBuckets_ModesCopyOn_StandardBucketIsStandard(t *testing.T) {
	if testing.Short() {
		t.Skip("requires database")
	}
	db := testDashDB(t)
	defer func() { _ = db.Close() }()
	const tenant = "test-dash-modes"
	cleanupDashBucketData(t, db, tenant)
	defer cleanupDashBucketData(t, db, tenant)
	_, err := db.Exec(`INSERT INTO tenants (id, name, email, access_key, secret_key) VALUES ($1, 'M', 'testdashmodes@test.com', 'VK-dm', 'SK-dm') ON CONFLICT DO NOTHING`, tenant)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO buckets (tenant_id, name) VALUES ($1, 'photos') ON CONFLICT DO NOTHING`, tenant)
	require.NoError(t, err)

	page := func(on bool) string {
		SetModesCopy(func(id string) bool { return on && id == tenant })
		defer SetModesCopy(nil)
		w := httptest.NewRecorder()
		HandleBuckets(realBucketsTemplate(t), db, "", nil, zap.NewNop()).
			ServeHTTP(w, injectSessionWithTenant(httptest.NewRequest("GET", "/dashboard/buckets", nil), tenant))
		require.Equal(t, http.StatusOK, w.Code)
		return w.Body.String()
	}

	on, off := page(true), page(false)

	assert.Contains(t, on, ">"+modes.Name(modes.Standard)+"</span>", "the flag on: the mode's name from modes.json")
	assert.NotContains(t, on, ">Private</span>", "a Standard bucket is never labelled Private")
	assert.Contains(t, off, ">Private</span>", "the flag off: the legacy label, until the flip")
}
