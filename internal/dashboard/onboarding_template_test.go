package dashboard

import (
	"html/template"
	"strings"
	"testing"

	"github.com/FairForge/vaultaire/internal/dashboard/handlers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// B4 (5.15.6): the onboarding quickstart must be copy-paste RUNNABLE —
// real access key injected, rclone + s3cmd tabs present, and a copy button
// per snippet. The secret is never in dashboard HTML (reveal-once at signup
// per B2) — snippets carry the YOUR_SECRET_KEY placeholder.
func renderOverview(t *testing.T, data map[string]any) string {
	t.Helper()
	tmpl := template.Must(template.ParseFS(Templates,
		"templates/layouts/base.html",
		"templates/customer/dashboard.html",
	))
	var sb strings.Builder
	require.NoError(t, tmpl.ExecuteTemplate(&sb, "base", data))
	return sb.String()
}

func TestOnboardingTabs_RunnableConfig(t *testing.T) {
	body := renderOverview(t, map[string]any{
		"Email": "b4@stored.ge",
		"Page":  "overview",
		"Onboarding": &handlers.OnboardingStatus{
			AccessKey: "VKb4testkey123",
		},
	})

	// Real access key is injected into every snippet family.
	assert.Contains(t, body, "VKb4testkey123", "the user's real access key must be injected")
	// The secret NEVER appears in dashboard HTML — placeholder only.
	assert.Contains(t, body, "YOUR_SECRET_KEY", "snippets must carry the secret placeholder")

	// rclone tab (the tool the LET audience actually uses).
	assert.Contains(t, body, "rclone config create", "must ship an rclone quickstart")
	// s3cmd tab.
	assert.Contains(t, body, "s3cmd", "must ship an s3cmd quickstart")

	// Copy buttons: btn-copy with a data-copy-target per snippet panel.
	assert.GreaterOrEqual(t, strings.Count(body, "data-copy-target"), 4,
		"each snippet panel needs a copy button wired to the shared btn-copy handler")
}
