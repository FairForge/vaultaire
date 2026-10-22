package btcpay

import (
	"fmt"
	"math/big"
	"strings"
)

// Money at the edges: Greenfield sends amounts as decimal strings; the
// ledger keeps USD cents. Conversions are exact (big.Rat), rounded half up
// to the cent once, at the end.

// usdString formats cents as Greenfield's decimal amount ("12.30").
func usdString(cents int64) string { return fmt.Sprintf("%d.%02d", cents/100, cents%100) }

// parseDecimal reads a decimal string ("" = 0).
func parseDecimal(s string) (*big.Rat, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return new(big.Rat), nil
	}
	r, ok := new(big.Rat).SetString(s)
	if !ok {
		return nil, fmt.Errorf("btcpay: not a decimal: %q", s)
	}
	return r, nil
}

// usdToCents reads a USD decimal ("12.3") as cents.
func usdToCents(s string) (int64, error) {
	v, err := parseDecimal(s)
	if err != nil {
		return 0, err
	}
	v.Mul(v, big.NewRat(100, 1))
	return roundRat(v), nil
}

// roundRat rounds half away from zero to an integer.
func roundRat(v *big.Rat) int64 {
	neg := v.Sign() < 0
	a := new(big.Rat).Abs(v)
	a.Add(a, big.NewRat(1, 2))
	q := new(big.Int).Quo(a.Num(), a.Denom())
	if neg {
		q.Neg(q)
	}
	return q.Int64()
}

// confirmedCents is how much of an invoice's paidAmount (cents) is
// CONFIRMED bitcoin (RP33-2): only a payment whose status is Settled is
// value — Processing is seen, not confirmed (and a payment double-spent
// before it confirms vanishes from the invoice). Over every payment method
// of the invoice, each payment converted at its method's rate (USD/BTC):
//
//	confirmed = max(0, min(paidAmount − Σ value×rate of the payments not Settled (Processing),
//	                       Σ value×rate of the Settled payments))
//
// paidAmount is BTCPay's own net figure (a second payment's network fee —
// networkFeeMode MultiplePaymentsOnly — counts toward nothing), so the
// first term is never more than the confirmed part of it and the second
// never more than the Settled payments are worth; when every payment is
// Settled the first term is paidAmount itself. Invalid payments are not in
// paidAmount and are skipped. Rounded half up once per sum. The lab:
// 0.0001181 BTC × 84675 = 10.0001 → 1000 cents Settled, 0 while it was
// Processing.
func confirmedCents(paid int64, pms []PaymentMethod) (int64, error) {
	settled, unconfirmed := new(big.Rat), new(big.Rat)
	for _, pm := range pms {
		rate, err := parseDecimal(pm.Rate)
		if err != nil {
			return 0, fmt.Errorf("rate of %s: %w", pm.PaymentMethodID, err)
		}
		for _, pay := range pm.Payments {
			v, err := parseDecimal(pay.Value)
			if err != nil {
				return 0, fmt.Errorf("payment %s value: %w", pay.ID, err)
			}
			v.Mul(v, rate)
			switch pay.Status {
			case PaymentSettled:
				settled.Add(settled, v)
			case PaymentInvalid:
			default:
				unconfirmed.Add(unconfirmed, v)
			}
		}
	}
	hundred := big.NewRat(100, 1)
	c := paid - roundRat(unconfirmed.Mul(unconfirmed, hundred))
	if s := roundRat(settled.Mul(settled, hundred)); s < c {
		c = s
	}
	if c < 0 {
		c = 0
	}
	return c, nil
}
