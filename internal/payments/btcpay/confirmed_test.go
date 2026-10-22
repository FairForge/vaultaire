package btcpay

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestConfirmedCents(t *testing.T) {
	// The formula on the lab's payment methods (rate USD/BTC):
	// confirmed = min(paidAmount − Σ unconfirmed value × rate,
	//                 Σ settled value × rate), never below 0.
	load := func(name string) []PaymentMethod {
		var pms []PaymentMethod
		require.NoError(t, json.Unmarshal(companion(t, name, "payment-methods"), &pms))
		return pms
	}
	for _, c := range []struct {
		name string
		paid int64
		want int64
	}{
		{"InvoiceProcessing_exact", 1000, 0},             // 0.0001181 Processing
		{"InvoiceSettled_exact", 1000, 1000},             // 0.0001181 × 84675 = 10.0001
		{"InvoiceSettled_overpaid", 1500, 1500},          // 0.00017715 × 84675 = 15.0002
		{"InvoiceExpired_partial", 500, 500},             // 0.00005903 × 84699 = 4.99978
		{"InvoiceReceivedPayment_late_partial", 500, 0},  // Processing
		{"InvoiceReceivedPayment_late_full", 1000, 0},    // Processing
		{"InvoicePaymentSettled_late_full", 1000, 1000},  // Settled
		{"InvoiceInvalid_marked", 0, 0},                  // no payment
		{"InvoiceSettled_marked_unpaid", 0, 0},           // marked by hand, nothing paid
		{"InvoiceExpired_double_spend_noinvalid", 0, 0},  // the payment double-spent away
		{"InvoiceSettled_speed_LowSpeed", 1000, 1000},    // settled at 6 confirmations
		{"InvoiceExpired_late_over", 1300, 0},            // Processing
		{"InvoicePaymentSettled_late_over", 1300, 1300},  // Settled
		{"InvoiceSettled_late_full_marked", 1000, 1000},  // Settled
		{"InvoicePaymentSettled_exact", 1000, 1000},      // Settled
		{"InvoiceReceivedPayment_overpaid", 1500, 0},     // Processing
		{"InvoicePaymentSettled_late_partial", 500, 500}, // Settled
	} {
		got, err := confirmedCents(c.paid, load(c.name))
		require.NoError(t, err, c.name)
		assert.Equal(t, c.want, got, c.name)
	}
	// Never more than the Settled payments are worth, whatever paidAmount
	// says (the second term).
	got, err := confirmedCents(1500, load("InvoiceSettled_exact"))
	require.NoError(t, err)
	assert.Equal(t, int64(1000), got)
	// An Invalid (double-spent) payment counts for nothing either way.
	got, err = confirmedCents(1000, []PaymentMethod{{Rate: "84675", Payments: []Payment{
		{Value: "0.0001181", Status: PaymentInvalid}, {Value: "0.0001181", Status: PaymentSettled}}}})
	require.NoError(t, err)
	assert.Equal(t, int64(1000), got)
	// An undecodable rate is an error, never a guess.
	_, err = confirmedCents(1000, []PaymentMethod{{Rate: "n/a", Payments: []Payment{{Value: "1", Status: PaymentSettled}}}})
	assert.Error(t, err)
}
