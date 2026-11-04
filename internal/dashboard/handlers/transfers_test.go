package handlers

import (
	"context"
	"database/sql"
	"html/template"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	dashauth "github.com/FairForge/vaultaire/internal/dashboard/auth"
)

type stubTransfers struct {
	gotSecret string
	gotForm   TransferForm
	problem   string
}

func (s *stubTransfers) CreateTransfer(_ context.Context, _, _ string, f TransferForm, secret string) (string, string, error) {
	s.gotForm, s.gotSecret = f, secret
	return "id-1", s.problem, nil
}
func (s *stubTransfers) CancelTransfer(context.Context, string, string, string) (bool, error) {
	return true, nil
}
func (s *stubTransfers) ResumeTransfer(context.Context, string, string, string) (bool, error) {
	return true, nil
}

func transfersTemplate(t *testing.T) *template.Template {
	t.Helper()
	page, err := os.ReadFile("../templates/customer/transfers.html")
	require.NoError(t, err)
	tmpl := template.Must(template.New("base").Parse(`{{define "base"}}{{template "content" .}}{{end}}`))
	return template.Must(tmpl.Parse(string(page)))
}

func transfersDB(t *testing.T) *sqlmockDB {
	t.Helper()
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	mock.MatchExpectationsInOrder(false)
	for i := 0; i < 4; i++ {
		mock.ExpectQuery(`FROM transfer_jobs`).WillReturnRows(sqlmock.NewRows([]string{"id"}))
		mock.ExpectQuery(`SELECT name FROM buckets`).WillReturnRows(sqlmock.NewRows([]string{"name"}).AddRow("photos"))
	}
	return &sqlmockDB{db: db}
}

// WP-XFER-1 PR B: the secret is a password input and is never echoed back —
// a refused start re-renders the form with every field but the secret; a
// member who may not start one sees no form.
func TestTransfersPage_TheSecretIsNeverEchoed(t *testing.T) {
	svc := &stubTransfers{problem: "the source refused this key: InvalidAccessKeyId"}
	m := transfersDB(t)
	h := HandleTransfersPage(transfersTemplate(t), TransferDeps{DB: m.db, Service: svc, Logger: zap.NewNop()})
	form := url.Values{"endpoint": {"https://s3.us-west-004.backblazeb2.com"}, "region": {"us-west-004"}, "source_bucket": {"b2b"},
		"access_key": {"004abc"}, "secret_key": {"K004-very-secret"}, "bucket": {"photos"}}
	req := httptest.NewRequest(http.MethodPost, "/dashboard/transfers", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req = req.WithContext(dashauth.WithSession(req.Context(), &dashauth.SessionData{UserID: "u", TenantID: "t", MemberRole: "owner"}))
	w := httptest.NewRecorder()
	h(w, req)
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Equal(t, "K004-very-secret", svc.gotSecret, "the secret reaches the service")
	assert.Equal(t, "b2b", svc.gotForm.SourceBucket)
	body := w.Body.String()
	assert.Contains(t, body, "InvalidAccessKeyId")
	assert.Contains(t, body, `value="https://s3.us-west-004.backblazeb2.com"`)
	assert.NotContains(t, body, "K004-very-secret", "never echoed back")
	assert.Contains(t, body, `type="password" name="secret_key"`)

	// A viewer: the list, no form.
	req = httptest.NewRequest(http.MethodGet, "/dashboard/transfers", nil)
	req = req.WithContext(dashauth.WithSession(req.Context(), &dashauth.SessionData{UserID: "u", TenantID: "t", MemberRole: "viewer"}))
	w = httptest.NewRecorder()
	h(w, req)
	assert.Equal(t, http.StatusOK, w.Code)
	assert.NotContains(t, w.Body.String(), `name="secret_key"`)
}

type sqlmockDB struct{ db *sql.DB }
