package dashboard

import (
	"html/template"
	"strings"
	"testing"

	"github.com/FairForge/vaultaire/internal/dashboard/handlers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The admin layout's bar on small screens: a Menu button that controls the
// nav (aria-expanded / aria-controls), the theme toggle beside the brand,
// and the nav landmark named for screen readers.
func TestAdminLayout_MobileMenu(t *testing.T) {
	tmpl := template.Must(template.New("").Funcs(handlers.TemplateFuncs()).ParseFS(Templates, "templates/layouts/admin.html"))
	template.Must(tmpl.Parse(`{{define "content"}}x{{end}}`))
	var sb strings.Builder
	require.NoError(t, tmpl.ExecuteTemplate(&sb, "admin", map[string]any{"Page": "admin-flags"}))
	body := sb.String()

	assert.Contains(t, body, `<button class="menu-toggle" type="button" aria-expanded="false" aria-controls="admin-nav"`)
	assert.Contains(t, body, `<nav class="sidebar-nav" id="admin-nav" aria-label="Admin">`)
	assert.Equal(t, 1, strings.Count(body, `class="theme-toggle"`), "one theme toggle, in the bar")
	assert.Contains(t, body, `classList.add("js")`, "the no-JS fallback keys off html.js")
	assert.Contains(t, body, `sidebar-link sidebar-link--active">Flags</a>`, "the current page is marked")
}
