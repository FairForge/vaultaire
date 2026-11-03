package billing

// RP92-1 (Stage 1a Prompt 57 Part 0; D-STRIPE-96's recommended half): a
// sponsored family member never buys a house of its own. The organizer's
// pool pays for it; a house inside the pool would be thrown away when the
// member detaches. The checkout, every grant and the Stripe webhook refuse
// it — the webhook by logging and skipping (a 500 would have Stripe retry
// for three days).

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/FairForge/vaultaire/internal/payments"
	"github.com/FairForge/vaultaire/internal/usage"
)

// sponsor makes the fixture's tenant a sponsored member of a new organizer.
func (f *houseFixture) sponsor(t *testing.T) string {
	t.Helper()
	org := fmt.Sprintf("house-org-%d", time.Now().UnixNano())
	_, err := f.db.Exec(`INSERT INTO tenants (id, name, email) VALUES ($1, 'Org', $2)`, org, org+"@test.local")
	require.NoError(t, err)
	require.NoError(t, f.qm.CreateTenant(context.Background(), org, "free", usage.FreeTierLimits.StorageBytes))
	t.Cleanup(func() {
		_, _ = f.db.Exec(`UPDATE tenants SET parent_tenant_id = NULL, child_kind = NULL, pool_root_id = id WHERE id = $1`, f.tenantID)
		_, _ = f.db.Exec(`DELETE FROM entitlements WHERE tenant_id = $1`, f.tenantID)
		_, _ = f.db.Exec(`DELETE FROM tenant_floor_quotas WHERE tenant_id = $1`, f.tenantID)
		_, _ = f.db.Exec(`DELETE FROM tenant_quotas WHERE tenant_id = $1`, org)
		_, _ = f.db.Exec(`DELETE FROM tenants WHERE id = $1`, org)
	})
	_, err = f.db.Exec(`UPDATE tenants SET parent_tenant_id = $2, child_kind = 'sponsored', pool_root_id = $2 WHERE id = $1`, f.tenantID, org)
	require.NoError(t, err)
	// A member's own limit is 0: the pool decides (usage/pool.go fits).
	_, err = f.db.Exec(`UPDATE tenant_quotas SET storage_limit_bytes = 0 WHERE tenant_id = $1`, f.tenantID)
	require.NoError(t, err)
	return org
}

func (f *houseFixture) quotaRow(t *testing.T) (limit int64, tier string, floors int) {
	t.Helper()
	require.NoError(t, f.db.QueryRow(`SELECT storage_limit_bytes, tier FROM tenant_quotas WHERE tenant_id = $1`, f.tenantID).Scan(&limit, &tier))
	require.NoError(t, f.db.QueryRow(`SELECT COUNT(*) FROM tenant_floor_quotas WHERE tenant_id = $1`, f.tenantID).Scan(&floors))
	return limit, tier, floors
}

// familyCheckoutProvider is a provider that would hand out a checkout URL.
type familyCheckoutProvider struct{ started int }

func (p *familyCheckoutProvider) Name() string              { return "famtest" }
func (p *familyCheckoutProvider) Renewal() payments.Renewal { return payments.RenewalAuto }
func (p *familyCheckoutProvider) Ready(context.Context) (payments.Readiness, string) {
	return payments.Ready, ""
}
func (p *familyCheckoutProvider) StartCheckout(context.Context, payments.Checkout) (string, error) {
	p.started++
	return "https://checkout.example/x", nil
}
func (p *familyCheckoutProvider) Resize(context.Context, string, payments.Order) (payments.ResizeOutcome, error) {
	return payments.ResizeOutcome{}, nil
}
func (p *familyCheckoutProvider) StopRenewal(context.Context, string) error   { return nil }
func (p *familyCheckoutProvider) ResumeRenewal(context.Context, string) error { return nil }
func (p *familyCheckoutProvider) Cancel(context.Context, string) error        { return nil }
func (p *familyCheckoutProvider) ManageURL(context.Context, string, string) (string, error) {
	return "", nil
}
func (p *familyCheckoutProvider) Webhook() http.Handler { return nil }

type everyFlagOn struct{}

func (everyFlagOn) Enabled(string, string) bool { return true }

func TestFamily_ASponsoredMemberCannotBuyAHouse(t *testing.T) {
	f := setupHouseFixture(t)
	f.sponsor(t)
	ctx := context.Background()
	order := payments.Order{Std: 1, Period: payments.PeriodMonthly}

	// The checkout: refused before the provider is called.
	reg := payments.NewRegistry(f.db, everyFlagOn{}, 0, nil)
	p := &familyCheckoutProvider{}
	reg.Register(p)
	url, err := reg.StartCheckout(ctx, p.Name(), payments.Checkout{TenantID: f.tenantID, Order: order, Purpose: payments.PurposeNew})
	assert.ErrorIs(t, err, payments.ErrInAFamily, "a sponsored member's checkout")
	assert.Empty(t, url)
	assert.Zero(t, p.started, "no checkout was started at the provider")

	// Grant, Extend and Resize: refused, nothing written.
	for name, write := range map[string]func(*sql.Tx) error{
		"grant": func(tx *sql.Tx) error {
			return payments.Grant(ctx, tx, f.tenantID, order, time.Now().Add(30*24*time.Hour), payments.ProviderManual, "t", payments.RenewalPrepaid)
		},
		"resize":    func(tx *sql.Tx) error { return payments.Resize(ctx, tx, f.tenantID, order.House()) },
		"set_house": func(tx *sql.Tx) error { return usage.SetHouseTx(ctx, tx, f.tenantID, order.House()) },
	} {
		err := payments.InTx(ctx, f.db, write)
		assert.ErrorIs(t, err, usage.ErrInAFamily, name)
	}
	assert.ErrorIs(t, f.qm.SetHouse(ctx, f.tenantID, order.House()), usage.ErrInAFamily, "QuotaManager.SetHouse")
	_, state, _, _ := f.record(t)
	assert.Equal(t, "none", state, "no entitlement row")
	limit, tier, floors := f.quotaRow(t)
	assert.Zero(t, limit, "the pool still decides")
	assert.Equal(t, "free", tier)
	assert.Zero(t, floors, "no floor rows")

	// A replayed subscription event (Stripe retries for days): 200, skipped.
	start := time.Now().Add(-time.Hour).Truncate(time.Second)
	sub := f.houseSub("active", start, start.Add(30*24*time.Hour))
	f.post(t, "replayed", "customer.subscription.updated", f.stripeHas(sub)) // requires 200
	_, state, _, _ = f.record(t)
	assert.Equal(t, "none", state, "the webhook skipped the sponsored member")
	limit, tier, floors = f.quotaRow(t)
	assert.Zero(t, limit)
	assert.Equal(t, "free", tier)
	assert.Zero(t, floors)
}
