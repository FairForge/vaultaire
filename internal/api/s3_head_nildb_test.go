package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/FairForge/vaultaire/internal/tenant"
)

// R1-05 / R2-18: HEAD is served from object_head_cache only; without a
// database it must answer cleanly instead of dereferencing a nil *sql.DB.
func TestHeadObject_NilDB_NoPanic(t *testing.T) {
	s := &Server{logger: zap.NewNop()}
	req := httptest.NewRequest("HEAD", "/b/k", nil)
	req = req.WithContext(tenant.WithTenant(req.Context(), &tenant.Tenant{ID: "t"}))
	w := httptest.NewRecorder()

	require.NotPanics(t, func() { s.handleHeadObject(w, req, &S3Request{Bucket: "b", Object: "k"}) })
	assert.Equal(t, http.StatusServiceUnavailable, w.Code)
}
