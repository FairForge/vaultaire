package adminread

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/FairForge/vaultaire/internal/translog"
)

// "Access to your account" says every entry a tenant can find in words from
// ONE table: every subject scope, and every write of a tenant row of a
// translog_* flag that can have one, has its sentence; anything else is
// Unknown, never the raw value.
func TestWords_EveryEntryATenantCanFindHasWords(t *testing.T) {
	seen := map[string]bool{}
	for _, scope := range translog.AdminReadScopes() {
		if !translog.ScopeHasSubject(scope) {
			continue // a list names no tenant: Find never returns it
		}
		w := Words(Found{Kind: translog.KindAdminRead, Scope: scope})
		assert.NotEqual(t, Unknown, w, "scope %s has no words", scope)
		assert.False(t, seen[w], "two scopes read the same sentence: %q", w)
		seen[w] = true
		assert.NotContains(t, w, "_", "words, not the scope's name")
	}
	on, off := true, false
	for _, flag := range translog.PolicyFlags() {
		if flag == translog.FlagTranslogPublic {
			continue // global only (D-P34a-4): a tenant row is refused, so no tenant has one
		}
		for _, f := range []Found{
			{Kind: translog.KindLogPolicy, Flag: flag, Action: translog.PolicyActionSet, Enabled: &on},
			{Kind: translog.KindLogPolicy, Flag: flag, Action: translog.PolicyActionSet, Enabled: &off},
			{Kind: translog.KindLogPolicy, Flag: flag, Action: translog.PolicyActionClear, Enabled: &off},
		} {
			w := Words(f)
			assert.NotEqual(t, Unknown, w, "%s %s %v has no words", flag, f.Action, *f.Enabled)
			assert.False(t, seen[w], "a repeated sentence: %q", w)
			seen[w] = true
			assert.False(t, strings.Contains(w, "translog"), "words, not the flag's key")
		}
	}
	// What the table does not know is never printed as it came.
	assert.Equal(t, Unknown, Words(Found{Kind: translog.KindAdminRead, Scope: "<script>alert(1)</script>"}))
	assert.Equal(t, Unknown, Words(Found{Kind: translog.KindLogPolicy, Flag: "x", Action: "set", Enabled: &on}))
	assert.Equal(t, Unknown, Words(Found{Kind: "server_build"}))
}
