package payments

import (
	"errors"
	"fmt"
	"math"

	"github.com/FairForge/vaultaire/internal/api/landing"
)

// The house's price, from internal/api/landing/prices.json only — the one
// sheet the site, the dashboard and every provider charge from. Moved here
// from internal/billing in WP-PAY-2 so the dashboard prices a house without
// importing a provider SDK (TestDashboardImportsNoStripe).

// Line identifies a house line.
type Line string

const (
	LineStandard Line = "standard"
	LineVault    Line = "vault"
	LinePinHot   Line = "pin_hot"
)

// ValidateHouseOrder applies the product rules: at least one floor, the
// builder's bound per floor, pin-hot only on (and within) downstairs, a
// known period. Attic-only houses are sold (decision 2026-09-27).
func ValidateHouseOrder(o Order) error {
	if o.Period != PeriodAnnual && o.Period != PeriodMonthly {
		return fmt.Errorf("unknown billing period %q", o.Period)
	}
	if o.Std < 0 || o.Vault < 0 || o.PinHot < 0 {
		return errors.New("a floor cannot be negative")
	}
	if o.Std > landing.MaxIntentTB || o.Vault > landing.MaxIntentTB {
		return fmt.Errorf("a floor holds at most %d TB", landing.MaxIntentTB)
	}
	if o.Std == 0 && o.Vault == 0 {
		return errors.New("the house is empty: add at least one box")
	}
	if o.PinHot > o.Std {
		return errors.New("pin-hot covers Standard only, so it cannot exceed the Standard TB")
	}
	return nil
}

// --- Quote (what the page prints; what Stripe charges) ---

// QuoteLine is one receipt line.
type QuoteLine struct {
	Label  string // "Standard", "Vault", "Pin-hot"
	TB     int
	Cents  int64 // per month
	Detail string
}

// Quote is a house priced from prices.json.
type Quote struct {
	Lines        []QuoteLine
	MonthlyCents int64 // per month, what the site's receipt shows
	ChargeCents  int64 // per billing period (×12 when annual)
	Period       Period
}

func money(cents int64) string { return fmt.Sprintf("$%d.%02d", cents/100, cents%100) }

// Money formats the line's per-month amount ("$26.94").
func (l QuoteLine) Money() string { return money(l.Cents) }

// Monthly formats the per-month total.
func (q Quote) Monthly() string { return money(q.MonthlyCents) }

// Charge formats the per-period charge.
func (q Quote) Charge() string { return money(q.ChargeCents) }

// Cents rounds dollars to whole cents.
func Cents(usd float64) int64 { return int64(math.Round(usd * 100)) }

// UnitCents returns the per-TB-per-month unit for a line/period in whole
// cents, as Stripe stores it — so the sum of lines is exactly the invoice.
func UnitCents(line Line, period Period) int64 {
	p := landing.Get()
	switch line {
	case LineStandard:
		if period == PeriodMonthly {
			return Cents(p.Standard.Monthly)
		}
		return Cents(p.Standard.Annual)
	case LineVault:
		if period == PeriodMonthly {
			return Cents(p.Vault.Monthly)
		}
		return Cents(p.Vault.Annual)
	case LinePinHot:
		return Cents(p.PinHot)
	}
	return 0
}

// vaultMonthlyCents applies the site's "$4.99 monthly minimum" to the
// attic line: it is a volume-tiered price in Stripe (1 TB = the minimum
// flat, 2+ TB = per unit), so the maths here mirrors that shape.
func vaultMonthlyCents(tb int) int64 {
	if tb <= 0 {
		return 0
	}
	if tb == 1 {
		return Cents(landing.Get().Vault.MonthlyMinimum)
	}
	return int64(tb) * UnitCents(LineVault, PeriodMonthly)
}

// QuoteHouse prices an order. Annual rates are per TB per month billed
// yearly (ChargeCents = 12 × monthly); monthly is charged as shown.
func QuoteHouse(std, vault, pinHot int, period Period) Quote {
	q := Quote{Period: period}
	add := func(label string, tb int, c int64, detail string) {
		if tb <= 0 {
			return
		}
		q.Lines = append(q.Lines, QuoteLine{Label: label, TB: tb, Cents: c, Detail: detail})
		q.MonthlyCents += c
	}
	add("Standard", std, int64(std)*UnitCents(LineStandard, period), "opens instantly")
	if period == PeriodMonthly {
		add("Vault", vault, vaultMonthlyCents(vault), "on tape, back in minutes")
	} else {
		add("Vault", vault, int64(vault)*UnitCents(LineVault, period), "on tape, back in minutes")
	}
	add("Pin-hot", pinHot, int64(pinHot)*UnitCents(LinePinHot, period), "never moves to tape")
	q.ChargeCents = q.MonthlyCents
	if period == PeriodAnnual {
		q.ChargeCents = q.MonthlyCents * 12
	}
	return q
}
