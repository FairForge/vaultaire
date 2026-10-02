package dashboard

import (
	"html/template"
	"strings"
	"testing"

	"github.com/FairForge/vaultaire/internal/auth"
	"github.com/FairForge/vaultaire/internal/dashboard/handlers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// WP-R4-1: the served key form offers the GOVERNANCE bypass as a permission
// and says what it does. Every value the form can post is a permission the
// API accepts — a checkbox the validator refuses would fail the whole form.
func TestAPIKeysPage_OffersTheGovernanceBypassPermission(t *testing.T) {
	// Arrange
	tmpl := template.Must(template.New("").Funcs(handlers.TemplateFuncs()).ParseFS(Templates,
		"templates/layouts/base.html",
		"templates/customer/apikeys.html",
	))

	// Act
	var sb strings.Builder
	require.NoError(t, tmpl.ExecuteTemplate(&sb, "base", map[string]any{"Email": "keys@stored.ge", "Page": "apikeys"}))
	body := sb.String()

	// Assert
	// (strings.Contains, not assert.Contains: a failure must not print the page.)
	assert.True(t, strings.Contains(body, `name="permissions" value="`+auth.PermBypassGovernanceRetention+`"`), "the form offers the permission")
	assert.True(t, strings.Contains(body, "GOVERNANCE retention"), "the form says what the permission does")
	assert.True(t, strings.Contains(body, "COMPLIANCE"), "and what it never does")
	for _, part := range strings.Split(body, `name="permissions" value="`)[1:] {
		value := part[:strings.Index(part, `"`)]
		assert.True(t, auth.ValidPermissions[value], "the form offers %q, which the validator refuses", value)
	}
}
