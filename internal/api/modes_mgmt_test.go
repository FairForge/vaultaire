package api

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/FairForge/vaultaire/internal/hosts"
	"github.com/FairForge/vaultaire/internal/modes"
)

// K3 PR 3: the management API's bucket JSON names the bucket's mode
// (buckets.mode, migration 102) — additive, on every answer.
func TestManagementBucket_ReportsMode(t *testing.T) {
	f := setupVersioningFixture(t)
	path := "/api/v1/manage/buckets/" + f.bucket
	get := func() string {
		t.Helper()
		w := f.mgmt(t, "GET", path, "")
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		var got map[string]any
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &got))
		return got["mode"].(string)
	}

	// A bucket is born Standard ('private' visibility — the name trap).
	assert.Equal(t, modes.Standard, get())

	// Requiring a customer key makes it Private — said by the PATCH itself.
	w := f.mgmt(t, "PATCH", path, `{"ssec_required": true}`)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var patched map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &patched))
	assert.Equal(t, modes.Private, patched["mode"])
	assert.Equal(t, modes.Private, get())

	// A metadata-only PATCH reports the mode it did not change.
	w = f.mgmt(t, "PATCH", path, `{"metadata": {"k": "v"}}`)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &patched))
	assert.Equal(t, modes.Private, patched["mode"])
}

// K3-PROBE R12: llms.txt no longer promises ?acl public-read (the server
// refuses it) and lists the bucket modes from modes.json.
func TestLLMSTxt_ModesFromModesJSON_NoPublicReadACL(t *testing.T) {
	body := llmsTxtBody(hosts.Get())
	assert.NotContains(t, body, "?acl (public-read)")
	for _, m := range modes.All().Modes {
		assert.Contains(t, body, m.ID+" ("+m.Name+": ")
	}
}
