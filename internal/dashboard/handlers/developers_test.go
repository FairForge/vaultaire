package handlers

// WP-LOG-1 (Stage 1a Prompt 49): the Developers page — the tenant's S3
// requests of the last 7 days, the error and its suggestion inline,
// filtered by key and status, searchable by request id.

import (
	"html/template"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

func developersPage(t *testing.T) *template.Template {
	t.Helper()
	base := template.Must(template.New("").Funcs(TemplateFuncs()).ParseFS(os.DirFS("../templates"), "layouts/base.html"))
	tmpl := template.Must(base.Clone())
	return template.Must(tmpl.ParseFS(os.DirFS("../templates"), "customer/developers.html"))
}

func TestDevelopers_ShowsTheTenantsRequestsWithTheSuggestion(t *testing.T) {
	db := testDashDB(t)
	t.Cleanup(func() { _ = db.Close() })
	id := houseTenant(t, db, 0, 0)
	other := houseTenant(t, db, 0, 0)
	t.Cleanup(func() { _, _ = db.Exec(`DELETE FROM request_log WHERE tenant_id IN ($1, $2)`, id, other) })
	_, err := db.Exec(`INSERT INTO request_suggestions (error_code, suggestion) VALUES ('AccessDenied', 'Check the key under API Keys.')
		ON CONFLICT (error_code) DO NOTHING`)
	require.NoError(t, err)
	var suggestion string
	require.NoError(t, db.QueryRow(`SELECT suggestion FROM request_suggestions WHERE error_code = 'AccessDenied'`).Scan(&suggestion))
	_, err = db.Exec(`INSERT INTO request_log (tenant_id, key_id, op, bucket, object_key, status, error_code, request_id, host) VALUES
		($1, 'VKREAD', 'PutObject', 'photos', 'denied.jpg', 403, 'AccessDenied', 'REQ-DENIED-1', 'origin'),
		($1, 'VKMAIN', 'GetObject', 'photos', 'cat.jpg', 200, '', 'REQ-OK-1', 's3.stored.cloud'),
		($2, 'VKOTHER', 'GetObject', 'theirs', 'secret.txt', 200, '', 'REQ-OTHER', 'origin')`, id, other)
	require.NoError(t, err)

	get := func(query string) string {
		w := httptest.NewRecorder()
		HandleDevelopers(developersPage(t), db, zap.NewNop()).ServeHTTP(w,
			injectSessionWithTenant(httptest.NewRequest("GET", "/dashboard/developers"+query, nil), id))
		require.Equal(t, 200, w.Code)
		return w.Body.String()
	}
	body := get("")
	assert.Contains(t, body, "REQ-DENIED-1")
	assert.Contains(t, body, "REQ-OK-1")
	assert.Contains(t, body, "AccessDenied")
	assert.Contains(t, body, template.HTMLEscapeString(suggestion), "the suggestion inline")
	assert.NotContains(t, body, "REQ-OTHER", "never another tenant's")
	assert.NotContains(t, body, "secret.txt")

	errs := get("?status=error")
	assert.Contains(t, errs, "REQ-DENIED-1")
	assert.NotContains(t, errs, "REQ-OK-1")

	byID := get("?request_id=REQ-OK-1")
	assert.Contains(t, byID, "REQ-OK-1")
	assert.NotContains(t, byID, "REQ-DENIED-1")

	byKey := get("?key=VKREAD")
	assert.Contains(t, byKey, "REQ-DENIED-1")
	assert.NotContains(t, byKey, "REQ-OK-1")
}
