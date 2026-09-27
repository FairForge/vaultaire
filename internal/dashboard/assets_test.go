package dashboard

import (
	"html/template"
	"regexp"
	"strings"
	"testing"

	"github.com/FairForge/vaultaire/internal/dashboard/handlers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Static asset URLs carry a version derived from the embedded files, so a
// deploy never pairs new HTML with a stale edge-cached stylesheet.
func TestAssetVersion_StampsEveryAssetURL(t *testing.T) {
	require.Regexp(t, `^[0-9a-f]{12}$`, handlers.AssetVersion, "set from the static tree at init")
	assert.Equal(t, "/static/css/style.css?v="+handlers.AssetVersion, handlers.AssetURL("/static/css/style.css"))

	for _, layout := range []string{"templates/layouts/base.html", "templates/layouts/admin.html"} {
		src, err := Templates.ReadFile(layout)
		require.NoError(t, err)
		bare := regexp.MustCompile(`(href|src)="/static/`).FindAllString(string(src), -1)
		assert.Empty(t, bare, "%s references /static/ without {{asset}}", layout)
	}

	tmpl := template.Must(template.New("").Funcs(handlers.TemplateFuncs()).ParseFS(Templates, "templates/layouts/base.html"))
	template.Must(tmpl.Parse(`{{define "content"}}x{{end}}`))
	var sb strings.Builder
	require.NoError(t, tmpl.ExecuteTemplate(&sb, "base", map[string]any{}))
	assert.Contains(t, sb.String(), `href="/static/css/style.css?v=`+handlers.AssetVersion+`"`)
	assert.Contains(t, sb.String(), `src="/static/js/dashboard.js?v=`+handlers.AssetVersion+`"`)
}
