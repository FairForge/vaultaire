package payments

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// addPeriod must add a period exactly as Postgres adds the interval Extend
// and PaidThroughAfter use (RP88): the day clamped to the target month's
// last day. Go's AddDate normalises an overflow forward (Oct 31 + 1 month =
// Dec 1), so a manual grant made on the 29th–31st used to run up to three
// days past the extension that followed it, and this package's tests went
// red on those days of the month.
func TestAddPeriod_IsPostgresRule(t *testing.T) {
	// Arrange
	f := newFixture(t)
	cases := []struct {
		in string
		p  Period
	}{
		{"2026-10-31T01:53:56Z", PeriodMonthly}, // → 2026-11-30
		{"2026-01-31T12:00:00Z", PeriodMonthly}, // → 2026-02-28
		{"2028-01-30T00:00:00Z", PeriodMonthly}, // → 2028-02-29 (leap)
		{"2028-02-29T23:59:59Z", PeriodAnnual},  // → 2029-02-28
		{"2026-03-31T00:00:00Z", PeriodMonthly}, // → 2026-04-30
		{"2026-12-31T00:00:00Z", PeriodMonthly}, // → 2027-01-31 (the year rolls)
		{"2026-10-15T08:00:00Z", PeriodAnnual},  // → 2027-10-15 (unchanged day)
		{"2026-10-15T08:00:00+02:00", PeriodMonthly},
	}
	for _, c := range cases {
		in, err := time.Parse(time.RFC3339, c.in)
		require.NoError(t, err)

		// Act
		var pg time.Time
		require.NoError(t, f.db.QueryRow(`SELECT (($1::timestamptz AT TIME ZONE 'UTC') + $2::interval) AT TIME ZONE 'UTC'`,
			in, c.p.interval()).Scan(&pg))
		got := addPeriod(in, c.p)

		// Assert
		assert.True(t, got.Equal(pg), "%s + %s: Go %s, Postgres %s", c.in, c.p, got.UTC(), pg.UTC())
		assert.Equal(t, time.UTC, got.Location(), "addPeriod answers in UTC")
	}
}
