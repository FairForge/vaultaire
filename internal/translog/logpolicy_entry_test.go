package translog

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Stage 1a Prompt 34a (RP44-2): a write of a translog_* flag row is a public,
// permanent entry. Its body is the flag, the action, the value, the scope and
// — for a tenant's row — a nonce and an opaque commitment, byte for byte the
// golden below; nothing that names a tenant or an operator can be written.

func validLogPolicy() LogPolicy {
	return LogPolicy{Flag: FlagTranslogAdminReads, Action: PolicyActionSet, Enabled: false, Scope: PolicyScopeTenant,
		Nonce: strings.Repeat("0a", 16), Commitment: strings.Repeat("c3", 32)}
}

func TestLogPolicy_BodyHasNoIdentity(t *testing.T) {
	// Golden: the exact bytes hashed, a tenant row and the global row.
	at := time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)
	body, err := Encode(validLogPolicy(), at)
	require.NoError(t, err)
	assert.Equal(t, `{"kind":"log_policy","v":1,"at":"2026-10-23T12:00:00Z","entry":{"flag":"translog_admin_reads","action":"set",`+
		`"enabled":false,"scope":"tenant","nonce":"0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a","commitment":"`+strings.Repeat("c3", 32)+`"}}`, string(body))
	d, err := Decode(body)
	require.NoError(t, err)
	assert.Equal(t, Entry(validLogPolicy()), d.Entry)

	global, err := Encode(LogPolicy{Flag: FlagTranslogAdminReads, Action: PolicyActionClear, Scope: PolicyScopeGlobal}, at)
	require.NoError(t, err)
	assert.Equal(t, `{"kind":"log_policy","v":1,"at":"2026-10-23T12:00:00Z","entry":{"flag":"translog_admin_reads","action":"clear",`+
		`"enabled":false,"scope":"global","nonce":"","commitment":""}}`, string(global))
}

func TestLogPolicy_ForbiddenFieldsAndShapes(t *testing.T) {
	at := time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)
	body, err := Encode(validLogPolicy(), at)
	require.NoError(t, err)

	// Each forbidden field is refused on the way back in (strict decode).
	for _, f := range []string{`"tenant_id":"7c9e6679"`, `"tenant":"7c9e6679"`, `"email":"alice@example.com"`, `"actor":"storedge@teamviera.com"`,
		`"updated_by":"storedge@teamviera.com"`, `"reason":"testing"`, `"subject":"7c9e6679"`, `"ip":"203.0.113.7"`} {
		forged := strings.Replace(string(body), `"flag":`, f+`,"flag":`, 1)
		_, err := Decode([]byte(forged))
		assert.Error(t, err, "a body with %s", f)
	}

	// The shapes: closed lists, a clear with a value, a tenant row without
	// its commitment, the global row with one.
	bad := map[string]LogPolicy{
		"a flag off the list":      {Flag: "signups", Action: PolicyActionSet, Scope: PolicyScopeGlobal},
		"a tenant id as the flag":  {Flag: "tenant-7c9e6679", Action: PolicyActionSet, Scope: PolicyScopeGlobal},
		"an action off the list":   {Flag: FlagTranslogPublic, Action: "flip", Scope: PolicyScopeGlobal},
		"a clear with a value":     {Flag: FlagTranslogPublic, Action: PolicyActionClear, Enabled: true, Scope: PolicyScopeGlobal},
		"a scope off the list":     {Flag: FlagTranslogPublic, Action: PolicyActionSet, Scope: "tenant-7c9e6679"},
		"a tenant row, no nonce":   {Flag: FlagTranslogAdminReads, Action: PolicyActionSet, Enabled: true, Scope: PolicyScopeTenant},
		"the global row, a nonce":  {Flag: FlagTranslogAdminReads, Action: PolicyActionSet, Scope: PolicyScopeGlobal, Nonce: strings.Repeat("0a", 16), Commitment: strings.Repeat("c3", 32)},
		"a tenant id as the nonce": {Flag: FlagTranslogAdminReads, Action: PolicyActionSet, Scope: PolicyScopeTenant, Nonce: "7c9e6679-7425-40de-944b-e07fc1f9", Commitment: strings.Repeat("c3", 32)},
	}
	for name, p := range bad {
		_, err := Encode(p, at)
		assert.Error(t, err, name)
	}
	for _, f := range PolicyFlags() {
		assert.True(t, strings.HasPrefix(f, "translog_"), f)
	}
}
