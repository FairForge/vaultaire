package billing

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/FairForge/vaultaire/internal/payments"
	"github.com/FairForge/vaultaire/internal/usage"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/stripe/stripe-go/v75"
)

// WP-PAY-7 PR 3: the webhook writes the entitlement record and nothing of
// tenants but stripe_customer_id; the current-subscription guard reads the
// entitlement only; charge.refunded lands in the ledger.

// droppedColumns are the tenants billing columns migration 100 drops.
var droppedColumns = regexp.MustCompile(`\b(stripe_subscription_id|subscription_status|house_period|deletion_stripe_cancelled_at)\b`)

// sqlLiterals returns every string literal of the package's non-test Go
// files in dir, by file.
func sqlLiterals(t *testing.T, dir string) map[string][]string {
	t.Helper()
	out := map[string][]string{}
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	fset := token.NewFileSet()
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, 0)
		require.NoError(t, err)
		ast.Inspect(f, func(n ast.Node) bool {
			if lit, ok := n.(*ast.BasicLit); ok && lit.Kind == token.STRING {
				if s, err := strconv.Unquote(lit.Value); err == nil {
					out[name] = append(out[name], s)
				}
			}
			return true
		})
	}
	return out
}

// TestWebhook_WritesNoTenantsBillingColumns is a source guard (sqlmock
// would have to model every query Grant runs): every `UPDATE tenants` the
// billing package sends sets stripe_customer_id alone, and no literal names
// a dropped column — applyHouse, the deletion and the invoice handlers
// touch only the entitlement tables (+ the customer id).
func TestWebhook_WritesNoTenantsBillingColumns(t *testing.T) {
	updateTenants := regexp.MustCompile(`(?is)UPDATE\s+tenants\s+SET\s+(.*?)\s+WHERE`)
	for file, lits := range sqlLiterals(t, ".") {
		for _, s := range lits {
			assert.False(t, droppedColumns.MatchString(s), "%s names a dropped tenants column: %q", file, s)
			for _, m := range updateTenants.FindAllStringSubmatch(s, -1) {
				set := strings.Join(strings.Fields(m[1]), " ")
				assert.Equal(t, "stripe_customer_id = $1", set, "%s writes tenants beyond the customer id: %q", file, s)
			}
		}
	}
}

// A clearing event for a tenant with NO entitlement row fails the guard
// closed: rows exist for every granted house since 097, so there is nothing
// of ours to clear — the event is ignored with an Info line, never a Revoke
// of floors the record does not describe.
func TestIsCurrentSubscription_NoRow_IgnoresClearingEvent(t *testing.T) {
	// Arrange: floors with no entitlement row (an operator's SetHouse).
	f := setupHouseFixture(t)
	require.NoError(t, f.qm.SetHouse(context.Background(), f.tenantID, usage.HouseFromTB(2, 1, 0)))
	t.Cleanup(func() { _, _ = f.db.Exec(`DELETE FROM tenant_floor_quotas WHERE tenant_id = $1`, f.tenantID) })
	start, end := time.Now().Add(-31*24*time.Hour), time.Now().Add(-time.Hour)

	// Act: canceled (updated) and deleted.
	f.post(t, "canceled", "customer.subscription.updated", f.stripeHas(f.houseSub("canceled", start, end)))
	f.post(t, "deleted", "customer.subscription.deleted", f.houseSub("canceled", start, end))

	// Assert
	assert.True(t, f.hasHouse(t), "no row names the subscription: nothing is cleared")
	_, err := payments.Get(context.Background(), f.db, f.tenantID)
	assert.ErrorIs(t, err, payments.ErrNoEntitlement)
}

// paidInvoice grants the fixture's house and settles one paid invoice; it
// returns the invoice.
func (f *houseFixture) paidInvoice(t *testing.T, cents int64) stripe.Invoice {
	t.Helper()
	start := time.Now().Add(-time.Hour).Truncate(time.Second)
	end := start.Add(30 * 24 * time.Hour)
	f.post(t, "created", "customer.subscription.created", f.stripeHas(f.houseSub("active", start, end)))
	inv := invoiceFor("in_r_"+f.tenantID, "cus_"+f.tenantID, "sub_"+f.tenantID)
	inv.AmountPaid = cents
	inv.Currency = "usd"
	inv.BillingReason = stripe.InvoiceBillingReasonSubscriptionCreate
	f.post(t, "paid", "invoice.payment_succeeded", inv)
	return inv
}

// refundedCharge is the charge.refunded payload: the charge of invoice with
// its refunds listed.
func (f *houseFixture) refundedCharge(invoiceID string, refunds ...*stripe.Refund) *stripe.Charge {
	var total int64
	for _, r := range refunds {
		total += r.Amount
	}
	return &stripe.Charge{ID: "ch_" + f.tenantID, Customer: &stripe.Customer{ID: "cus_" + f.tenantID},
		Invoice: &stripe.Invoice{ID: invoiceID}, Refunded: true, AmountRefunded: total,
		Refunds: &stripe.RefundList{Data: refunds}}
}

func (f *houseFixture) refundRows(t *testing.T) int {
	t.Helper()
	var n int
	require.NoError(t, f.db.QueryRow(
		`SELECT COUNT(*) FROM payments WHERE tenant_id = $1 AND kind = 'refund'`, f.tenantID).Scan(&n))
	return n
}

func TestChargeRefunded_RecordsOnce(t *testing.T) {
	// Arrange: a settled $11.53 charge.
	f := setupHouseFixture(t)
	inv := f.paidInvoice(t, 1153)
	ch := f.refundedCharge(inv.ID, &stripe.Refund{ID: "re_1_" + f.tenantID, Amount: 500, Status: stripe.RefundStatusSucceeded})

	// Act: delivered, then redelivered under a new event id.
	f.post(t, "refunded", "charge.refunded", ch)
	f.post(t, "refunded-again", "charge.refunded", ch)

	// Assert: one refund row linked to the charge, one audit row by the webhook.
	assert.Equal(t, 1, f.refundRows(t))
	var refundOf, chargeStatus string
	require.NoError(t, f.db.QueryRow(`
		SELECT r.refund_of::text, c.status FROM payments r JOIN payments c ON c.id = r.refund_of
		 WHERE r.tenant_id = $1 AND r.kind = 'refund' AND r.provider_id = $2`, f.tenantID, "re_1_"+f.tenantID).
		Scan(&refundOf, &chargeStatus))
	assert.NotEmpty(t, refundOf)
	assert.Equal(t, payments.StatusSettled, chargeStatus, "a partial refund leaves the charge settled")
	assert.Equal(t, 1, f.audits(t, "billing.refund_recorded"))
	var actor string
	require.NoError(t, f.db.QueryRow(`SELECT metadata->>'actor' FROM audit_logs WHERE tenant_id = $1 AND action = 'billing.refund_recorded'`,
		f.tenantID).Scan(&actor))
	assert.Equal(t, webhookActor, actor)

	// A second refund of the rest marks the charge refunded.
	ch = f.refundedCharge(inv.ID,
		&stripe.Refund{ID: "re_1_" + f.tenantID, Amount: 500, Status: stripe.RefundStatusSucceeded},
		&stripe.Refund{ID: "re_2_" + f.tenantID, Amount: 653, Status: stripe.RefundStatusSucceeded})
	f.post(t, "refunded-rest", "charge.refunded", ch)
	assert.Equal(t, 2, f.refundRows(t))
	require.NoError(t, f.db.QueryRow(`SELECT status FROM payments WHERE provider = 'stripe' AND provider_id = $1`, inv.ID).Scan(&chargeStatus))
	assert.Equal(t, payments.StatusRefunded, chargeStatus)
}

func TestChargeRefunded_UnknownCharge200NoApply(t *testing.T) {
	f := setupHouseFixture(t)
	f.paidInvoice(t, 1153)
	ch := f.refundedCharge("in_nobody_"+f.tenantID, &stripe.Refund{ID: "re_x_" + f.tenantID, Amount: 100, Status: stripe.RefundStatusSucceeded})

	code := f.postCode(t, "refunded-unknown", "charge.refunded", ch)

	assert.Equal(t, http.StatusOK, code, "an unknown charge is acknowledged, never a 500 storm")
	assert.Zero(t, f.refundRows(t))
	assert.True(t, f.eventRecorded(t, "refunded-unknown"))
}

// paymentsCount reads vaultaire_payments_total{provider,kind,outcome}.
func paymentsCount(t *testing.T, provider, kind, outcome string) float64 {
	t.Helper()
	reg := prometheus.NewRegistry()
	reg.MustRegister(payments.Collectors()...)
	mfs, err := reg.Gather()
	require.NoError(t, err)
	for _, mf := range mfs {
		if mf.GetName() != "vaultaire_payments_total" {
			continue
		}
		for _, m := range mf.GetMetric() {
			l := map[string]string{}
			for _, p := range m.GetLabel() {
				l[p.GetName()] = p.GetValue()
			}
			if l["provider"] == provider && l["kind"] == kind && l["outcome"] == outcome {
				return m.GetCounter().GetValue()
			}
		}
	}
	return 0
}

func TestChargeRefunded_CountedByChargesProvider(t *testing.T) {
	f := setupHouseFixture(t)
	inv := f.paidInvoice(t, 1153)
	before := paymentsCount(t, payments.ProviderStripe, payments.KindRefund, payments.OutcomeSettled)
	manual := paymentsCount(t, payments.ProviderManual, payments.KindRefund, payments.OutcomeSettled)

	f.post(t, "refunded", "charge.refunded",
		f.refundedCharge(inv.ID, &stripe.Refund{ID: "re_c_" + f.tenantID, Amount: 200, Status: stripe.RefundStatusSucceeded}))

	assert.Equal(t, before+1, paymentsCount(t, payments.ProviderStripe, payments.KindRefund, payments.OutcomeSettled))
	assert.Equal(t, manual, paymentsCount(t, payments.ProviderManual, payments.KindRefund, payments.OutcomeSettled))
}
