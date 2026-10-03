package handlers

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// WP-R10-3b: the dashboard asks the one export service; nothing is rendered
// in the request. The fake records the calls.

type fakeExports struct {
	requestErr error
	requested  []string
	latest     *ExportStatus
	latestErr  error
	url        string
	urlErr     error
}

func (f *fakeExports) Request(_ context.Context, userID, tenantID string) (string, error) {
	f.requested = append(f.requested, userID+"/"+tenantID)
	if f.requestErr != nil {
		return "", f.requestErr
	}
	return "exp-1", nil
}
func (f *fakeExports) Latest(context.Context, string) (*ExportStatus, error) {
	return f.latest, f.latestErr
}
func (f *fakeExports) DownloadURL(context.Context, string, string) (string, time.Time, error) {
	return f.url, time.Now().Add(time.Hour), f.urlErr
}

func TestHandleExportData_NoSessionRedirectsToLogin(t *testing.T) {
	w := httptest.NewRecorder()
	HandleExportData(&fakeExports{}, nil, zap.NewNop()).ServeHTTP(w, httptest.NewRequest("POST", "/dashboard/settings/export", nil))
	assert.Equal(t, http.StatusSeeOther, w.Code)
	assert.Equal(t, "/login", w.Header().Get("Location"))
}

func TestHandleExportData_RequestsThroughTheServiceAndFlashes(t *testing.T) {
	f := &fakeExports{}
	w := httptest.NewRecorder()
	HandleExportData(f, nil, zap.NewNop()).ServeHTTP(w, injectAccountSession(httptest.NewRequest("POST", "/dashboard/settings/export", nil)))

	assert.Equal(t, http.StatusSeeOther, w.Code)
	assert.Equal(t, "/dashboard/settings", w.Header().Get("Location"))
	assert.Equal(t, []string{"user-123/tenant-456"}, f.requested, "the session's user and tenant")
	kind, msg := flashOf(t, w)
	assert.Equal(t, "success", kind)
	assert.Equal(t, ExportRequestedMessage, msg)
	assert.Empty(t, w.Body.String(), "nothing is rendered in the request")
}

func TestHandleExportData_OneInFlightIsAFlash(t *testing.T) {
	f := &fakeExports{requestErr: ErrExportInFlight}
	w := httptest.NewRecorder()
	HandleExportData(f, nil, zap.NewNop()).ServeHTTP(w, injectAccountSession(httptest.NewRequest("POST", "/dashboard/settings/export", nil)))
	assert.Equal(t, http.StatusSeeOther, w.Code)
	kind, msg := flashOf(t, w)
	assert.Equal(t, "error", kind)
	assert.Contains(t, msg, "already being prepared")
}

func TestHandleExportData_NoServiceIsAFlash(t *testing.T) {
	w := httptest.NewRecorder()
	HandleExportData(nil, nil, zap.NewNop()).ServeHTTP(w, injectAccountSession(httptest.NewRequest("POST", "/dashboard/settings/export", nil)))
	assert.Equal(t, http.StatusSeeOther, w.Code)
	kind, _ := flashOf(t, w)
	assert.Equal(t, "error", kind)
}

func TestHandleExportDownload_RedirectsToThePresignedURLOnce(t *testing.T) {
	ready := &ExportStatus{ID: "exp-1", Status: "completed", ExpiresAt: time.Now().Add(time.Hour)}
	f := &fakeExports{latest: ready, url: "http://s3.test.local/_exports/account-export-exp-1.json?X-Amz-Signature=abc"}
	w := httptest.NewRecorder()
	HandleExportDownload(f, zap.NewNop()).ServeHTTP(w, injectAccountSession(httptest.NewRequest("GET", "/dashboard/settings/export/download", nil)))

	assert.Equal(t, http.StatusFound, w.Code)
	assert.Equal(t, f.url, w.Header().Get("Location"), "the URL lives in this one Location header")
	assert.Equal(t, "no-store", w.Header().Get("Cache-Control"))
	assert.NotContains(t, w.Body.String(), "X-Amz-Signature", "never in a body")
}

func TestHandleExportDownload_NothingReadyIsAFlash(t *testing.T) {
	for name, f := range map[string]*fakeExports{
		"none":     {},
		"pending":  {latest: &ExportStatus{ID: "exp-1", Status: "pending"}},
		"expired":  {latest: &ExportStatus{ID: "exp-1", Status: "completed", Expired: true}},
		"failed":   {latest: &ExportStatus{ID: "exp-1", Status: "failed"}},
		"url gone": {latest: &ExportStatus{ID: "exp-1", Status: "completed"}, urlErr: ErrExportExpired},
		"db error": {latestErr: errors.New("db down")},
	} {
		t.Run(name, func(t *testing.T) {
			w := httptest.NewRecorder()
			HandleExportDownload(f, zap.NewNop()).ServeHTTP(w, injectAccountSession(httptest.NewRequest("GET", "/dashboard/settings/export/download", nil)))
			assert.Equal(t, http.StatusSeeOther, w.Code)
			assert.Equal(t, "/dashboard/settings", w.Header().Get("Location"))
			kind, _ := flashOf(t, w)
			assert.Equal(t, "error", kind)
		})
	}
}

func TestPopulateExportStatus_TheThreeStates(t *testing.T) {
	at := time.Date(2026, 10, 3, 9, 0, 0, 0, time.UTC)
	cases := map[string]struct {
		latest *ExportStatus
		want   exportView
	}{
		"pending":   {&ExportStatus{Status: "pending", CreatedAt: at}, exportView{Pending: true, Requested: "October 3, 2026 09:00 UTC"}},
		"ready":     {&ExportStatus{Status: "completed", CreatedAt: at, ExpiresAt: at.Add(7 * 24 * time.Hour), SizeBytes: 3 << 20}, exportView{Ready: true, Requested: "October 3, 2026 09:00 UTC", ReadyUntil: "October 10, 2026 09:00 UTC", Size: "3.0 MB"}},
		"expired":   {&ExportStatus{Status: "expired", CreatedAt: at}, exportView{Expired: true, Requested: "October 3, 2026 09:00 UTC"}},
		"past date": {&ExportStatus{Status: "completed", CreatedAt: at, Expired: true}, exportView{Expired: true, Requested: "October 3, 2026 09:00 UTC"}},
		"failed":    {&ExportStatus{Status: "failed", CreatedAt: at, Error: "boom"}, exportView{Failed: true, Error: "boom", Requested: "October 3, 2026 09:00 UTC"}},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			data := map[string]any{}
			populateExportStatus(context.Background(), &fakeExports{latest: c.latest}, "user-123", data)
			require.Equal(t, true, data["ExportAvailable"])
			assert.Equal(t, c.want, data["Export"])
		})
	}
	data := map[string]any{}
	populateExportStatus(context.Background(), nil, "user-123", data)
	assert.Equal(t, false, data["ExportAvailable"])
	assert.NotContains(t, data, "Export")
}
