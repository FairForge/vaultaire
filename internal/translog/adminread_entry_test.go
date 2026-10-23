package translog

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// K4c-1 (Stage 1a Prompt 33): an admin read of customer metadata is a public,
// permanent entry. Its body is the scope, the role, a nonce and an opaque
// commitment — byte for byte the golden below — and nothing that names a
// tenant, an operator, an object or an address can be written in it.

func TestAdminRead_BodyHasNoIdentity(t *testing.T) {
	// Golden: the exact bytes hashed.
	at := time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)
	body, err := Encode(validAdminRead(), at)
	require.NoError(t, err)
	assert.Equal(t, `{"kind":"admin_read","v":1,"at":"2026-10-23T12:00:00Z","entry":{"scope":"tenant_detail","actor_role":"admin",`+
		`"nonce":"0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a","commitment":"`+strings.Repeat("c3", 32)+`"}}`, string(body))
	d, err := Decode(body)
	require.NoError(t, err)
	assert.Equal(t, Entry(validAdminRead()), d.Entry)

	list, err := Encode(AdminRead{Scope: ScopeTenantList, ActorRole: ActorRoleAdmin}, at)
	require.NoError(t, err)
	assert.Equal(t, `{"kind":"admin_read","v":1,"at":"2026-10-23T12:00:00Z","entry":{"scope":"tenant_list","actor_role":"admin","nonce":"","commitment":""}}`, string(list))

	// Each forbidden field is refused on the way back in (strict decode): a
	// body carrying one is not an admin_read entry.
	for _, f := range []string{`"tenant_id":"7c9e6679"`, `"email":"alice@example.com"`, `"object_key":"t-acme/photos/a.jpg"`,
		`"ip":"203.0.113.7"`, `"actor":"storedge@teamviera.com"`, `"subject":"7c9e6679"`, `"user_id":"u1"`} {
		forged := strings.Replace(string(body), `"scope":`, f+`,"scope":`, 1)
		_, err := Decode([]byte(forged))
		assert.Error(t, err, "a body with %s", f)
	}

	// The shapes: a role that is a person, a scope off the list, a subject
	// scope without its commitment, a list scope with one.
	bad := map[string]AdminRead{
		"actor is a person":      {Scope: ScopeTenantDetail, ActorRole: "storedge@teamviera.com", Nonce: strings.Repeat("0a", 16), Commitment: strings.Repeat("c3", 32)},
		"scope off the list":     {Scope: "tenant 7c9e6679", ActorRole: ActorRoleAdmin},
		"no commitment":          {Scope: ScopeTenantPayments, ActorRole: ActorRoleAdmin},
		"a list with a subject":  {Scope: ScopeAuditList, ActorRole: ActorRoleAdmin, Nonce: strings.Repeat("0a", 16), Commitment: strings.Repeat("c3", 32)},
		"upper-case commitment":  {Scope: ScopeTenantDetail, ActorRole: ActorRoleAdmin, Nonce: strings.Repeat("0a", 16), Commitment: strings.Repeat("C3", 32)},
		"a tenant id as a nonce": {Scope: ScopeTenantDetail, ActorRole: ActorRoleAdmin, Nonce: "7c9e6679-7425-40de-944b-e07fc1f9", Commitment: strings.Repeat("c3", 32)},
	}
	for name, a := range bad {
		_, err := Encode(a, at)
		assert.Error(t, err, name)
	}
	for _, s := range AdminReadScopes() {
		assert.Regexp(t, `^[a-z_]+$`, s)
		assert.NotContains(t, s, "@")
	}
}

func TestCoalescedAdminRead_KeyOutsideTheBody(t *testing.T) {
	e := CoalescedAdminRead(validAdminRead(), strings.Repeat("ab", 32))
	assert.Equal(t, "admin_read:"+strings.Repeat("ab", 32), e.DedupKey())
	body, err := Encode(e, time.Now())
	require.NoError(t, err)
	assert.NotContains(t, string(body), "abab", "the coalescing key is never in the public body")
	assert.Empty(t, CoalescedAdminRead(validAdminRead(), "").DedupKey())
}
