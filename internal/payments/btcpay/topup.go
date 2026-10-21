package btcpay

import (
	"errors"
	"math"
	"time"

	"github.com/FairForge/vaultaire/internal/payments"
)

// TopUp is a mid-period resize up priced the docs/PAYMENTS.md § 3.5 way —
// prorations are ours, not a processor's, for crypto: the period charge's
// difference (monthly: the month's; annual: the year's) times the days
// left over the days of the current period. Every input is printed in the
// invoice's metadata.
type TopUp struct {
	OldChargeCents, NewChargeCents int64
	PeriodStart, PaidThrough       time.Time
	Now                            time.Time
	RemainingDays, PeriodDays      int
	Cents                          int64
}

// ErrPeriodSwitch: a resize may not change the period (monthly ↔ annual is
// at renewal only — § 3.5).
var ErrPeriodSwitch = errors.New("the period changes at renewal only")

// PriceTopUp prices moving the house from old to new with paidThrough
// left, at now. Cents ≤ 0 = no money moves (a resize down, or the last
// day): the house changes in place.
func PriceTopUp(old, new payments.Order, paidThrough, now time.Time) (TopUp, error) {
	if old.Period != new.Period {
		return TopUp{}, ErrPeriodSwitch
	}
	t := TopUp{
		OldChargeCents: payments.QuoteHouse(old.Std, old.Vault, old.PinHot, old.Period).ChargeCents,
		NewChargeCents: payments.QuoteHouse(new.Std, new.Vault, new.PinHot, new.Period).ChargeCents,
		PaidThrough:    paidThrough.UTC(), Now: now.UTC(),
	}
	if new.Period == payments.PeriodAnnual {
		t.PeriodStart = t.PaidThrough.AddDate(-1, 0, 0)
	} else {
		t.PeriodStart = t.PaidThrough.AddDate(0, -1, 0)
	}
	const day = 24 * time.Hour
	t.PeriodDays = int(math.Round(float64(t.PaidThrough.Sub(t.PeriodStart)) / float64(day)))
	if left := t.PaidThrough.Sub(t.Now); left > 0 {
		t.RemainingDays = int(math.Ceil(float64(left) / float64(day)))
	}
	if t.RemainingDays > t.PeriodDays {
		t.RemainingDays = t.PeriodDays
	}
	diff := t.NewChargeCents - t.OldChargeCents
	if diff <= 0 || t.RemainingDays == 0 || t.PeriodDays == 0 {
		return t, nil
	}
	t.Cents = int64(math.Round(float64(diff) * float64(t.RemainingDays) / float64(t.PeriodDays)))
	return t, nil
}

// Metadata is the formula's inputs as the invoice prints them.
func (t TopUp) Metadata() map[string]any {
	return map[string]any{
		"old_charge_cents": t.OldChargeCents, "new_charge_cents": t.NewChargeCents,
		"period_start": t.PeriodStart.Format(time.RFC3339), "paid_through": t.PaidThrough.Format(time.RFC3339),
		"remaining_days": t.RemainingDays, "period_days": t.PeriodDays, "top_up_cents": t.Cents,
		"formula": "(new_charge_cents - old_charge_cents) * remaining_days / period_days",
	}
}
