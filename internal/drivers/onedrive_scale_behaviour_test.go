package drivers

import (
	"context"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/FairForge/vaultaire/internal/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// WP-FLEET-11 (docs/reviews/WP-FLEET-NEXT-PROMPT.md Part 5), on a 300-drive
// fake fleet: a miss on a settled fleet costs one request (it walked every
// account), an unsettled one walks at most 8, and a probe's work per
// interval is bounded (it asked every account).

// countingRT answers every Graph request 404 itemNotFound (or 200 for a
// quota probe) and counts them.
type countingRT struct{ n atomic.Int64 }

func (c *countingRT) RoundTrip(req *http.Request) (*http.Response, error) {
	c.n.Add(1)
	if strings.Contains(req.URL.RawQuery, "quota") {
		return odResp(req, 200, `{"quota":{"total":5497558138880,"used":1,"deleted":0,"remaining":5497558138879,"state":"normal"}}`, nil), nil
	}
	return odResp(req, 404, `{"error":{"code":"itemNotFound"}}`, nil), nil
}

func bigFleet(n int) (*OneDriveDriver, *countingRT) {
	rt := &countingRT{}
	d, _ := stubFleet(n, zap.NewNop())
	for _, t := range d.tenants {
		t.graphHTTP = &http.Client{Transport: rt}
		t.cdnHTTP = &http.Client{Transport: rt}
		t.wait = (&waitLog{}).wait
	}
	return d, rt
}

func TestFleetAtScale_AMissOnASettledFleetCostsOneRequest(t *testing.T) {
	ctx := common.WithTenantID(context.Background(), "x")
	d, rt := bigFleet(300)
	d.fleetState = FleetMatch

	ok, err := d.Exists(ctx, "bkt", "nothing-here")
	require.NoError(t, err)
	assert.False(t, ok)
	assert.Equal(t, int64(1), rt.n.Load(), "the home account only (it asked all 300)")

	rt.n.Store(0)
	_, err = d.Get(ctx, "bkt", "nothing-here")
	require.Error(t, err)
	assert.Equal(t, int64(1), rt.n.Load())

	rt.n.Store(0)
	_ = d.Delete(ctx, "bkt", "nothing-here")
	assert.Equal(t, int64(1), rt.n.Load())
}

func TestFleetAtScale_AnUnsettledFleetWalksAtMostEightAccounts(t *testing.T) {
	ctx := common.WithTenantID(context.Background(), "x")
	d, rt := bigFleet(300)
	d.fleetState = FleetMismatch

	ok, err := d.Exists(ctx, "bkt", "nothing-here")

	require.NoError(t, err)
	assert.False(t, ok)
	assert.Equal(t, int64(8), rt.n.Load())
}

func TestFleetAtScale_AProbesWorkPerIntervalIsBounded(t *testing.T) {
	d, rt := bigFleet(300)
	d.fleetState = FleetMatch
	d.fleetCheckedAt = time.Now() // no fingerprint re-check inside this probe

	require.NoError(t, d.HealthCheck(context.Background()))

	assert.LessOrEqual(t, rt.n.Load(), int64(16), "a sample of the fleet, not 300 calls inside the probe's 15 s")
	assert.Greater(t, rt.n.Load(), int64(0))
}
