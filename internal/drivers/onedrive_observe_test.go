package drivers

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// WP-FLEET-10 (docs/reviews/WP-FLEET-NEXT-PROMPT.md Part 6): the System page's
// fleet card reads one snapshot per account — its drive, the quota the last
// probe read (used, the bin, total), the last probe's outcome and the
// throttles of the last hour.

func TestFleetAccounts_TheSnapshotNamesDriveQuotaProbeAndThrottles(t *testing.T) {
	// Arrange: one account probed (quota known), throttled three times;
	// another whose probe failed; a third never probed.
	d, _ := bigFleet(3)
	d.fleetState = FleetMatch
	d.fleetCheckedAt = time.Now()
	require.NoError(t, d.tenants[0].probe(context.Background()))
	d.tenants[0].probeErr.Store(odProbeOutcome{})
	for i := 0; i < 3; i++ {
		d.tenants[0].hold("range", time.Millisecond)
	}
	d.tenants[1].probeErr.Store(odProbeOutcome{errors.New("onedrive throttled [b] (429)")})

	// Act
	got := d.FleetAccounts()

	// Assert
	require.Len(t, got, 3)
	a := got[0]
	assert.Equal(t, d.tenants[0].name, a.Account)
	assert.Equal(t, "DRIVE-a", a.DriveID)
	assert.True(t, a.Probed)
	assert.True(t, a.Up)
	assert.True(t, a.QuotaKnown)
	assert.Equal(t, int64(5497558138880), a.QuotaTotal)
	assert.Equal(t, int64(1), a.QuotaUsed)
	assert.Equal(t, int64(3), a.Throttles1h)
	assert.Empty(t, a.Licence, "the licence is WP-FLEET-6's")
	assert.False(t, got[1].Up)
	assert.Contains(t, got[1].LastError, "429")
	assert.False(t, got[2].Probed)
	assert.False(t, got[2].QuotaKnown)
}

func TestFleetAccounts_ThrottlesOlderThanAnHourDropOut(t *testing.T) {
	d, _ := bigFleet(1)
	tn := d.tenants[0]
	tn.countThrottleAt(time.Now().Add(-2 * time.Hour))
	tn.countThrottleAt(time.Now().Add(-30 * time.Minute))
	tn.countThrottleAt(time.Now())

	assert.Equal(t, int64(2), tn.throttlesSince(time.Now().Add(-time.Hour)))
}
