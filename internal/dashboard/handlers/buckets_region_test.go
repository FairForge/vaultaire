package handlers

import (
	"html/template"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/FairForge/vaultaire/internal/drivers"
)

// WP-R7-1: the region picker is generated from the account's region table
// (the template used to hard-code eight regions, three of which never
// existed) and only regions this deployment can store in are selectable.

// realBucketsTemplate renders the shipped buckets.html (from disk, relative to
// this package) inside a minimal base so the picker markup under test is the
// real one, not a stub.
func realBucketsTemplate(t *testing.T) *template.Template {
	t.Helper()
	tmpl := template.Must(template.New("base").Parse(`{{define "base"}}{{block "content" .}}{{end}}{{end}}`))
	return template.Must(tmpl.ParseFiles("../templates/customer/buckets.html"))
}

func TestHandleBuckets_RegionPickerFromTable(t *testing.T) {
	drivers.SetAvailableIDriveRegions(drivers.IDriveDefaultRegion(os.Getenv), []string{"eu-west-1"})
	t.Cleanup(drivers.ResetAvailableIDriveRegions)

	handler := HandleBuckets(realBucketsTemplate(t), nil, t.TempDir(), zap.NewNop())
	req := injectSessionWithTenant(httptest.NewRequest("GET", "/dashboard/buckets", nil), "test-dash-rp")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code)
	body := w.Body.String()

	assert.Contains(t, body, `<option value="us-central-1" selected>US Central (Dallas)</option>`, "default region preselected")
	assert.Contains(t, body, `<option value="eu-west-1">EU West (Ireland)</option>`, "enabled region selectable")
	assert.Contains(t, body, `<option value="ap-northeast-1" disabled>Asia Pacific (Tokyo) (not enabled)</option>`, "region without a driver is visible but disabled")
	for _, gone := range []string{`value="us-west-1"`, `value="eu-central-2"`, `value="eu-west-2"`} {
		assert.NotContains(t, body, gone, "regions the account does not have are gone")
	}
	assert.Contains(t, body, `<optgroup label="European Union">`)
}

// The region gate itself lives in the API layer's registry
// (createBucketRegistry — TestManagementCreateBucket_UnavailableRegion); the
// dashboard only renders the verdict, naming the region the customer picked.
func TestHandleCreateBucket_RegionNotEnabledIsRefused(t *testing.T) {
	fc := &fakeCreator{result: BucketCreateResult{State: BucketCreateRegionUnavailable}}
	handler := HandleCreateBucket(testBucketsTemplate(t), nil, fc.fn(), zap.NewNop())

	form := url.Values{"name": {"tokyo-bucket"}, "region": {"ap-northeast-1"}}
	req := injectSessionWithTenant(httptest.NewRequest("POST", "/dashboard/buckets",
		strings.NewReader(form.Encode())), "test-dash-r7")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code, "re-rendered form")
	assert.Contains(t, w.Body.String(), "Tokyo")
	assert.Contains(t, w.Body.String(), "not enabled on this deployment")
	assert.Equal(t, []string{"test-dash-r7/tokyo-bucket/ap-northeast-1"}, fc.calls)
}
