package email

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// WP-PAY-3: the three payment templates render with their data and escape
// what came from a row.

func TestRenderPaymentReceipt(t *testing.T) {
	html, text, err := RenderPaymentReceipt(ReceiptMail{BaseURL: "https://stored.test", Email: "a@b.test", InvoiceNumber: "ST-2026-000001",
		Amount: "$53.88", PaidAt: "20 Oct 2026", House: "1 TB downstairs", Period: "20 Oct 2026 – 20 Oct 2027",
		ReceiptURL: "https://stored.test/dashboard/billing/receipts/abc", Issuer: "FairForge LLC <x>",
		Lines: []ReceiptLine{{Label: "Downstairs", TB: 1, Money: "$4.49"}}})
	require.NoError(t, err)
	for _, want := range []string{"ST-2026-000001", "$53.88", "Downstairs", "1 TB", "/dashboard/billing/receipts/abc", "FairForge LLC &lt;x&gt;"} {
		assert.Contains(t, html, want)
	}
	assert.Contains(t, text, "ST-2026-000001")
	assert.NotContains(t, html, "<x>", "escaped")
}

func TestRenderRenewalReminder_StepCopy(t *testing.T) {
	base := ReminderMail{BaseURL: "https://stored.test", Email: "a@b.test", House: "2 TB downstairs", PaidThrough: "1 Nov 2026",
		GraceEnds: "15 Nov 2026", RenewURL: "https://stored.test/dashboard/billing", DaysLeft: 13}
	html, _, err := RenderRenewalReminder(base)
	require.NoError(t, err)
	assert.Contains(t, html, "paid for 13 more days")
	assert.Contains(t, html, "Renew now")
	g := base
	g.InGrace, g.DaysLeft = true, 12
	html, _, err = RenderRenewalReminder(g)
	require.NoError(t, err)
	assert.Contains(t, html, "grace period")
	assert.Contains(t, html, "12 more days")
	l := base
	l.LastDay = true
	html, _, err = RenderRenewalReminder(l)
	require.NoError(t, err)
	assert.Contains(t, html, "Last day")
	assert.True(t, strings.Contains(html, "Nothing is deleted"), "every step says nothing is deleted")
}

func TestRenderHouseLapsed(t *testing.T) {
	html, text, err := RenderHouseLapsed(LapsedMail{BaseURL: "https://stored.test", Email: "a@b.test", House: "1 TB Vault",
		PaidThrough: "1 Oct 2026", RenewURL: "https://stored.test/dashboard/billing"})
	require.NoError(t, err)
	assert.Contains(t, html, "Nothing was deleted")
	assert.Contains(t, html, "1 TB Vault")
	assert.Contains(t, text, "Renew your plan")
}

// WP-PAY-5: a customer who asked to switch to bitcoin is reminded with the
// switch offer — the next period paid the new way, from the paid date.
func TestRenderRenewalReminder_SwitchOffer(t *testing.T) {
	d := ReminderMail{BaseURL: "https://stored.test", Email: "a@b.test", House: "2 TB downstairs", PaidThrough: "1 Nov 2026",
		GraceEnds: "15 Nov 2026", RenewURL: "https://stored.test/dashboard/billing", DaysLeft: 13, SwitchTo: "bitcoin"}
	html, text, err := RenderRenewalReminder(d)
	require.NoError(t, err)
	assert.Contains(t, html, "Pay the next period in bitcoin")
	assert.Contains(t, html, "you chose to pay in bitcoin")
	assert.Contains(t, text, "bitcoin")
}
