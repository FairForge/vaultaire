package auth

// WP-KEY-1 PR B (Stage 1a Prompt 54; D-RP88-1): the key's identity reaches
// the request. The verifier answers (tenant, *KeyScope); KeyScope.KeyID is
// the api_keys.id of the credential that signed (an STS token: its parent
// key's), so a per-key cap can be enforced, metered and named. The primary
// pair has a KeyID and is never capped (F-39 (c)); a management key's
// ceiling is no S3 credential and has none.

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestKeyScope_NamesItsKey(t *testing.T) {
	a := newLifecycleAccount(t, "keyid")
	ctx := context.Background()

	scoped, err := a.svc.GenerateAPIKey(ctx, a.user.ID, "ci", &KeyCreateOptions{Permissions: []string{"GetObject", "ListObjects"}})
	require.NoError(t, err)
	_, scope, err := a.authenticate(scoped.Key, scoped.Secret)
	require.NoError(t, err)
	assert.Equal(t, scoped.ID, scope.KeyID, "a scoped key names itself")
	assert.False(t, scope.Primary)

	_, scope, err = a.authenticate(a.key.Key, a.key.Secret)
	require.NoError(t, err)
	assert.Equal(t, a.key.ID, scope.KeyID, "the primary pair has a KeyID")
	assert.True(t, scope.Primary, "and is the primary: never capped (F-39 (c))")
	assert.Zero(t, scope.EgressCap)

	tok, err := GenerateSTSToken(ctx, a.db, nil, a.tenant.ID, scoped.Key,
		&KeyScope{Permissions: scoped.Permissions}, STSRequest{Permissions: []string{"GetObject"}, TTL: 600})
	require.NoError(t, err)
	_, scope, err = a.authenticate(tok.AccessKey, tok.SecretKey)
	require.NoError(t, err)
	assert.True(t, scope.Temporary)
	assert.Equal(t, scoped.ID, scope.KeyID, "an STS token names its parent key")
	assert.False(t, scope.Primary)

	ptok, err := GenerateSTSToken(ctx, a.db, nil, a.tenant.ID, a.key.Key,
		&KeyScope{Permissions: []string{"*"}}, STSRequest{Permissions: []string{"GetObject"}, TTL: 600})
	require.NoError(t, err)
	_, scope, err = a.authenticate(ptok.AccessKey, ptok.SecretKey)
	require.NoError(t, err)
	assert.Equal(t, a.key.ID, scope.KeyID)
	assert.True(t, scope.Primary, "a token of the primary is the primary's")

	// The key's egress cap and this UTC month's counter ride the lookup
	// (no second query on a GET); another month's counter reads as 0.
	month := time.Now().UTC().Format("2006-01") + "-01"
	_, err = a.db.Exec(`UPDATE api_keys SET egress_limit_bytes = 1000, egress_used_bytes = 400, egress_month = $2 WHERE id = $1`, scoped.ID, month)
	require.NoError(t, err)
	_, scope, err = a.authenticate(scoped.Key, scoped.Secret)
	require.NoError(t, err)
	assert.EqualValues(t, 1000, scope.EgressCap)
	assert.EqualValues(t, 400, scope.EgressUsed)
	_, scope, err = a.authenticate(tok.AccessKey, tok.SecretKey)
	require.NoError(t, err)
	assert.EqualValues(t, 1000, scope.EgressCap, "a token carries its parent's cap")
	_, err = a.db.Exec(`UPDATE api_keys SET egress_month = DATE '2001-01-01' WHERE id = $1`, scoped.ID)
	require.NoError(t, err)
	_, scope, err = a.authenticate(scoped.Key, scoped.Secret)
	require.NoError(t, err)
	assert.Zero(t, scope.EgressUsed, "last month's bytes are not this month's")

	// A management key's ceiling is no S3 credential.
	assert.Empty(t, (&S3Ceiling{Permissions: []string{"GetObject"}}).KeyScope(time.Now()).KeyID)
}
