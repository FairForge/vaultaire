package billing

// RP94-14: payments.Revoke → usage.ClearHouseTx, reached by the Stripe
// terminal branches and by entitlement_lapse's lapse branch, set a sponsored
// member's limit to the free tier — fits() then capped the member INSIDE the
// pool. ClearHouseTx now skips a sponsored member: the entitlement row is
// lapsed, no quota is touched (only old rows: an entitlement written before
// RP92-1's guards).

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/FairForge/vaultaire/internal/payments"
	"github.com/FairForge/vaultaire/internal/usage"
)

// oldEntitlement writes a live entitlement as one written before RP92-1's
// guards would be: on a sponsored member.
func (f *houseFixture) oldEntitlement(t *testing.T) {
	t.Helper()
	_, err := f.db.Exec(`INSERT INTO entitlements (tenant_id, std_tb, period, paid_through, renewal, provider)
		VALUES ($1, 1, 'monthly', NOW() + interval '20 days', 'auto', 'stripe')`, f.tenantID)
	require.NoError(t, err)
}

func TestFamily_RevokeOfASponsoredMemberLapsesTheRowTouchesNoQuota(t *testing.T) {
	f := setupHouseFixture(t)
	f.sponsor(t)
	f.oldEntitlement(t)
	ctx := context.Background()

	require.NoError(t, payments.InTx(ctx, f.db, func(tx *sql.Tx) error { return payments.Revoke(ctx, tx, f.tenantID) }))
	var lapsed sql.NullTime
	require.NoError(t, f.db.QueryRow(`SELECT lapsed_at FROM entitlements WHERE tenant_id = $1`, f.tenantID).Scan(&lapsed))
	assert.True(t, lapsed.Valid, "the entitlement row is lapsed")
	limit, tier, floors := f.quotaRow(t)
	assert.Zero(t, limit, "the pool still decides — never the free tier's limit inside the pool")
	assert.Equal(t, "free", tier)
	assert.Zero(t, floors)

	// ClearHouse itself, the same.
	require.NoError(t, f.qm.ClearHouse(ctx, f.tenantID))
	limit, _, _ = f.quotaRow(t)
	assert.Zero(t, limit)
	_ = time.Second
	_ = usage.TB
}
