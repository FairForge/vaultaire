package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// RP33-6 (Stage 1a Prompt 27): docs/BACKEND_ENCRYPTION.md has one row per
// driver this binary can register — transport, the vendor's at-rest
// statement, and whether a Standard object can land there. A driver added
// to main.go without a row fails here; so does a row for a driver that no
// longer exists.

var (
	addDriverCall = regexp.MustCompile(`addDriver\("([a-z0-9-]+)"(\+region)?`)
	docRow        = regexp.MustCompile("(?m)^\\| `([a-z0-9<>-]+)` \\|")
)

func TestBackendEncryptionDoc_EveryDriverHasARow(t *testing.T) {
	src, err := os.ReadFile("main.go")
	require.NoError(t, err)
	registered := map[string]bool{}
	for _, m := range addDriverCall.FindAllStringSubmatch(string(src), -1) {
		name := m[1]
		if m[2] != "" {
			name += "<region>" // addDriver("idrive-"+region, …)
		}
		registered[name] = true
	}
	require.NotEmpty(t, registered)

	doc, err := os.ReadFile(filepath.Join("..", "..", "docs", "BACKEND_ENCRYPTION.md"))
	require.NoError(t, err)
	rows := map[string]bool{}
	for _, m := range docRow.FindAllStringSubmatch(string(doc), -1) {
		rows[m[1]] = true
	}
	for name := range registered {
		assert.True(t, rows[name], "driver %q has no row in docs/BACKEND_ENCRYPTION.md", name)
	}
	for name := range rows {
		assert.True(t, registered[name], "docs/BACKEND_ENCRYPTION.md has a row for %q, which main.go no longer registers", name)
	}
	assert.Contains(t, string(doc), "RB-3", "the blomp row names the owner's gate")
	assert.True(t, strings.Contains(string(doc), "BLOMP_ALLOW_UNSEALED"))
}
