package auth

import (
	"context"
	"testing"

	"github.com/FairForge/vaultaire/internal/usage"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Review R12 (P1): registration writes the tenant's primary key pair into
// api_keys, so a fresh free-tier account already held MaxAPIKeys (=1) rows
// and the dashboard refused to mint any key at all — while the credentials
// page tells customers who lost the secret to "generate a new key from the
// dashboard". The cap counts ACTIVE keys beyond the primary, and it is
// enforced here so the dashboard, the management API and the user API cannot
// disagree (R11-16 / WP-R11-6).
func TestGenerateAPIKey_FreeTierCapCountsActiveExtraKeys(t *testing.T) {
	db := setupTestDB(t)
	svc := NewAuthService(nil, db)
	user, _, primary, err := svc.CreateUserWithTenant(context.Background(), "cap@stored.ge", "Str0ngPassw0rd!", "cap")
	require.NoError(t, err)
	require.NotNil(t, primary)
	// tenant_quotas defaults tier to 'free' (migration 034).

	first, err := svc.GenerateAPIKey(context.Background(), user.ID, "laptop", nil)
	require.NoError(t, err, "the first dashboard key on a fresh free account must succeed")

	_, err = svc.GenerateAPIKey(context.Background(), user.ID, "second", nil)
	require.ErrorIs(t, err, ErrKeyLimitReached, "free tier: %d extra key(s)", usage.FreeTierLimits.MaxAPIKeys)

	require.NoError(t, svc.RevokeAPIKey(context.Background(), user.ID, first.ID))
	_, err = svc.GenerateAPIKey(context.Background(), user.ID, "replacement", nil)
	assert.NoError(t, err, "revoking frees the slot")

	// A paid tier is not capped here (the management API keeps its 50/account).
	_, err = db.Exec(`UPDATE tenant_quotas SET tier = 'standard' WHERE tenant_id = $1`, user.TenantID)
	require.NoError(t, err)
	_, err = svc.GenerateAPIKey(context.Background(), user.ID, "third", nil)
	assert.NoError(t, err)
}
