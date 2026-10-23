package dashboard

import (
	"bytes"
	"html/template"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/FairForge/vaultaire/internal/dashboard/handlers"
	"github.com/FairForge/vaultaire/internal/modes"
)

// Keystone K3 PR 3: the privacy page's per-mode table and the bucket labels
// are generated from modes.json, behind modes_copy.

func withModesCopy(t *testing.T, on bool, masterKey bool) {
	t.Helper()
	handlers.SetModesCopy(func(string) bool { return on })
	modes.SetMasterKeyLoaded(masterKey)
	t.Cleanup(func() {
		handlers.SetModesCopy(nil)
		modes.SetMasterKeyLoaded(false)
	})
}

func privacyPage(t *testing.T) string {
	t.Helper()
	r, _, _ := setupTestRouter(t)
	req := httptest.NewRequest("GET", "/legal/privacy", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code)
	return w.Body.String()
}

func TestPrivacyModesTable_NoMasterKey_NeverSaysKeysWeHold(t *testing.T) {
	// Arrange: the flag on globally, NO master key (prod today —
	// docs/reviews/K3-PROBE.md).
	r, _, _ := setupTestRouter(t)
	withModesCopy(t, true, false)

	// Act
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("GET", "/legal/privacy", nil))
	body := w.Body.String()

	// Assert: every mode's row, from modes.json; Standard says the pre-key
	// truth, never "keys we hold".
	require.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, body, `id="modes"`)
	for _, m := range modes.All().Modes {
		assert.Contains(t, body, "<strong>"+m.Name+"</strong>")
		assert.Contains(t, body, template.HTMLEscapeString(m.ServerHolds.Render(false)))
	}
	std, _ := modes.Get(modes.Standard)
	assert.Contains(t, body, template.HTMLEscapeString(std.Customer.WithoutMasterKey))
	assert.NotContains(t, strings.ToLower(body), "keys we hold")
	assert.NotContains(t, strings.ToLower(body), "zero-knowledge")
}

func TestPrivacyModesTable_MasterKeyLoaded_SaysKeysWeHold(t *testing.T) {
	r, _, _ := setupTestRouter(t)
	withModesCopy(t, true, true)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("GET", "/legal/privacy", nil))
	assert.Contains(t, w.Body.String(), "keys we hold")
}

func TestPrivacyModesTable_FlagOff_NoTable(t *testing.T) {
	body := privacyPage(t) // the router wires modes_copy from deps.Flags: nil = off
	assert.NotContains(t, body, `id="modes"`)
}

// renderCustomer renders a customer template the way the router does.
func renderCustomer(t *testing.T, page string, data map[string]any) string {
	t.Helper()
	tmpl := template.Must(template.New("").Funcs(handlers.TemplateFuncs()).ParseFS(Templates, "templates/layouts/base.html"))
	template.Must(tmpl.ParseFS(Templates, page))
	var b bytes.Buffer
	require.NoError(t, tmpl.ExecuteTemplate(&b, "base", data))
	return b.String()
}

func TestBucketLabels_ModesCopy_StandardBucketIsNeverCalledPrivate(t *testing.T) {
	std := handlers.BucketRow{Name: "photos", Visibility: modes.VisibilityPrivate, Mode: modes.Standard}
	pub := handlers.BucketRow{Name: "site", Visibility: modes.VisibilityPublic, Mode: modes.Public}
	std.ModeLabel, pub.ModeLabel = handlers.ModeLabelOf(modes.Standard), handlers.ModeLabelOf(modes.Public)
	page := func(on bool) string {
		return renderCustomer(t, "templates/customer/buckets.html", map[string]any{"Page": "buckets", "ModesCopy": on,
			"Buckets": []handlers.BucketRow{std, pub}, "BucketCount": 2})
	}

	on, off := page(true), page(false)

	// On: the names from modes.json — the 'private' visibility bucket is
	// Standard (the name trap).
	assert.Contains(t, on, ">"+modes.Name(modes.Standard)+"</span>")
	assert.Contains(t, on, ">"+modes.Name(modes.Public)+"</span>")
	assert.NotContains(t, on, ">Private</span>", "a Standard bucket is never labelled Private")
	// Off: the legacy labels, unchanged until the flag is flipped.
	assert.Contains(t, off, ">Private</span>")
}

func TestBucketSettings_ModesCopy_ShowsTheModeAndItsSentence(t *testing.T) {
	withModesCopy(t, true, false)
	data := map[string]any{"Page": "buckets", "BucketName": "photos", "Visibility": modes.VisibilityPrivate,
		"ModesCopy": true, "Mode": handlers.ModeLabelOf(modes.Standard), "ModeStandard": handlers.ModeLabelOf(modes.Standard),
		"ModePublic": handlers.ModeLabelOf(modes.Public), "ModePrivate": handlers.ModeLabelOf(modes.Private)}

	body := renderCustomer(t, "templates/customer/bucket_settings.html", data)

	std, _ := modes.Get(modes.Standard)
	assert.Contains(t, body, template.HTMLEscapeString(std.Customer.Render(false)))
	assert.NotContains(t, strings.ToLower(body), "keys we hold")
	assert.NotContains(t, body, `toggle-active">Private</span>`)
}

// RP37-8: the retention list says what happens to a deleted file's bytes —
// from modes.json, behind modes_copy like the per-mode table.
func TestPrivacyRetention_ModesCopy_SaysDeletedBytesOutliveTheDelete(t *testing.T) {
	r, _, _ := setupTestRouter(t)
	withModesCopy(t, true, false)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("GET", "/legal/privacy", nil))
	assert.Contains(t, w.Body.String(), "<strong>Deleted and replaced files</strong>, "+template.HTMLEscapeString(modes.All().DeletedBytes.Text))
}

func TestPrivacyRetention_FlagOff_NoDeletedBytesSentence(t *testing.T) {
	assert.NotContains(t, privacyPage(t), "Deleted and replaced files")
}
