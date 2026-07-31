package drivers

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/FairForge/vaultaire/internal/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

func fleetOf(n int) *OneDriveDriver {
	d := &OneDriveDriver{logger: zap.NewNop()}
	for i := 0; i < n; i++ {
		d.tenants = append(d.tenants, &odTenant{name: string(rune('a' + i))})
	}
	return d
}

// The 2026-07-30 read-after-write failure: Put and Get picked fleet tenants
// independently (round-robin), so reads asked accounts that never held the
// object. Placement must be a pure function of the path.
func TestOneDrivePlacement_Deterministic(t *testing.T) {
	d := fleetOf(3)
	paths := []string{
		"t-tenant-a/_global/_chunks/aabbcc",
		"t-tenant-a/bucket/key.bin",
		"t-tenant-b/bucket/key.bin", // different tenant prefix → may differ
	}
	for _, p := range paths {
		first := d.homeTenantIndex(p)
		for i := 0; i < 50; i++ {
			require.Equal(t, first, d.homeTenantIndex(p),
				"placement for %q must never vary between calls", p)
		}
	}
}

// tenantsFor puts the home tenant first and every other tenant after it —
// reads probe the whole fleet so pre-placement (round-robin era) data stays
// reachable.
func TestOneDrivePlacement_ProbeOrderCoversFleet(t *testing.T) {
	d := fleetOf(5)
	p := "t-x/bucket/some-object"
	ordered := d.tenantsFor(p)
	require.Len(t, ordered, 5)
	assert.Same(t, d.tenants[d.homeTenantIndex(p)], ordered[0], "home tenant must be probed first")

	seen := map[*odTenant]bool{}
	for _, tn := range ordered {
		assert.False(t, seen[tn], "no tenant may appear twice")
		seen[tn] = true
	}
	assert.Len(t, seen, 5, "every fleet tenant must be reachable by the probe")
}

// Distribution sanity: many keys should spread across the fleet, not pile on
// one account.
func TestOneDrivePlacement_Spreads(t *testing.T) {
	d := fleetOf(3)
	counts := map[int]int{}
	for i := 0; i < 3000; i++ {
		counts[d.homeTenantIndex(string(rune('a'+i%26))+string(rune('0'+i%10))+"/obj"+string(rune(i)))]++
	}
	for idx, c := range counts {
		assert.Greater(t, c, 300, "tenant %d starved: %d of 3000", idx, c)
	}
}

// odStubRoundTripper serves canned Graph responses so List can be driven
// without credentials or network.
type odStubRoundTripper struct {
	mu    sync.Mutex
	calls []string
	pages map[string]string // request URL -> JSON body
}

func (rt *odStubRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	rt.mu.Lock()
	rt.calls = append(rt.calls, req.URL.String())
	rt.mu.Unlock()
	body, ok := rt.pages[req.URL.String()]
	if !ok {
		return &http.Response{StatusCode: 404, Body: io.NopCloser(strings.NewReader(`{"error":"not found"}`)),
			Header: make(http.Header), Request: req}, nil
	}
	return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body)),
		Header: make(http.Header), Request: req}, nil
}

// Graph pages children (default 200/page). Listing only the first page
// silently truncated every container with more objects than one page.
func TestOneDriveList_FollowsPagination(t *testing.T) {
	page1 := graphBase + "/drives/DRIVE1/items/root:/vaultaire/t-tenant-x/bucket:/children?$top=999"
	page2 := graphBase + "/drives/DRIVE1/items/root:/next-page-token"
	rt := &odStubRoundTripper{pages: map[string]string{
		page1: `{"value":[{"name":"a.bin"},{"name":"b.bin"}],"@odata.nextLink":"` + page2 + `"}`,
		page2: `{"value":[{"name":"c.bin"}]}`,
	}}

	tn := &odTenant{
		name:      "tenant-1",
		driveID:   "DRIVE1",
		cachedTok: "stub-token",
		tokExpiry: time.Now().Add(time.Hour),
		graphHTTP: &http.Client{Transport: rt},
	}
	d := &OneDriveDriver{tenants: []*odTenant{tn}, logger: zap.NewNop()}

	ctx := common.WithTenantID(context.Background(), "tenant-x")
	got, err := d.List(ctx, "bucket", "")
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"a.bin", "b.bin", "c.bin"}, got,
		"List must return objects from every page, not just the first")
	assert.Len(t, rt.calls, 2, "should have followed exactly one nextLink")
}
