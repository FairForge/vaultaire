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
