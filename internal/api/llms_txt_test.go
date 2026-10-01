package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Review R14-04 / R14-11: /llms.txt quotes the price file, answers HEAD and
// is cacheable; it must never carry a retired price or the moved Swagger path.
func TestLlmsTxt_PricesFromPriceFileAndHEAD(t *testing.T) {
	s := newTestServerWithHealthChecker(t)

	req := httptest.NewRequest(http.MethodGet, "/llms.txt", nil)
	rec := httptest.NewRecorder()
	s.handleLlmsTxt(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	body := rec.Body.String()

	assert.Contains(t, body, "$4.49/TB/mo annual ($4.99 monthly)")
	assert.Contains(t, body, "$2/TB/mo annual\n> ($2.55 monthly, $4.99 monthly minimum)")
	assert.Contains(t, body, "Free egress per month: 0.5× the Standard quota + 1× the Vault quota.")
	assert.NotContains(t, body, "3.99")
	assert.NotContains(t, body, "GET /docs — interactive Swagger UI")
	assert.Contains(t, body, "GET /docs/api")
	assert.Contains(t, body, "/api/v1/user")
	assert.Contains(t, body, "/api/v1/sts/token")
	assert.Contains(t, body, "--aws-sigv4")
	assert.Contains(t, body, `{"accessKeyId","secretAccessKey","endpoint"}`, "register response shape (live-verified R14)")
	assert.Equal(t, "text/plain; charset=utf-8", rec.Header().Get("Content-Type"))
	assert.Equal(t, "public, max-age=300", rec.Header().Get("Cache-Control"))

	req = httptest.NewRequest(http.MethodHead, "/llms.txt", nil)
	rec = httptest.NewRecorder()
	s.handleLlmsTxt(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Empty(t, rec.Body.String())
	assert.NotEmpty(t, rec.Header().Get("Content-Length"))
}
