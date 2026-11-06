package drivers

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/FairForge/vaultaire/internal/common"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// WP-FLEET-2 (D-F1-5): a Graph DELETE parks the file in the user's recycle
// bin for 93 days, and the bin counts against the drive's quota — after one
// bench day the three fleet drives held 68 / 23 / 21 GiB of deleted shards.
// Every delete the driver makes is now POST …/permanentDelete: gone at once,
// no bin entry. A 404 there is "already gone".

func permaURL(tn *odTenant, itemID string) string {
	return graphBase + "/drives/" + tn.driveID + "/items/" + itemID + "/permanentDelete"
}

func TestPermanentDelete_DeleteIsAPermanentDeleteOnEveryAccountThatHasIt(t *testing.T) {
	ctx := common.WithTenantID(context.Background(), "x")
	d, stubs := stubFleet(2, zap.NewNop())
	path := d.ObjectKey(ctx, "bkt", "obj")
	for i, tn := range d.tenants {
		stubs[i].routes["GET "+itemURL(tn, path)] = odRoute{200, `{"id":"F` + tn.name + `"}`}
		stubs[i].routes["POST "+permaURL(tn, "F"+tn.name)] = odRoute{204, ""}
	}

	require.NoError(t, d.Delete(ctx, "bkt", "obj"))

	for i, tn := range d.tenants {
		assert.Contains(t, stubs[i].calls, "POST "+permaURL(tn, "F"+tn.name))
		assert.False(t, stubs[i].sawMethod("DELETE"), "no recycle-bin DELETE on %s", tn.name)
	}
}

func TestPermanentDelete_A404IsAlreadyGone(t *testing.T) {
	// The item resolved, then went (another delete raced it): success.
	ctx := common.WithTenantID(context.Background(), "x")
	d, stubs := stubFleet(1, zap.NewNop())
	tn := d.tenants[0]
	path := d.ObjectKey(ctx, "bkt", "obj")
	stubs[0].routes["GET "+itemURL(tn, path)] = odRoute{200, `{"id":"F1"}`}
	stubs[0].routes["POST "+permaURL(tn, "F1")] = odRoute{404, `{"error":{"code":"itemNotFound"}}`}

	assert.NoError(t, d.Delete(ctx, "bkt", "obj"))
}

func TestPermanentDelete_A403IsAnError(t *testing.T) {
	ctx := common.WithTenantID(context.Background(), "x")
	d, stubs := stubFleet(1, zap.NewNop())
	tn := d.tenants[0]
	path := d.ObjectKey(ctx, "bkt", "obj")
	stubs[0].routes["GET "+itemURL(tn, path)] = odRoute{200, `{"id":"F1"}`}
	stubs[0].routes["POST "+permaURL(tn, "F1")] = odRoute{403, `{"error":{"code":"accessDenied"}}`}

	err := d.Delete(ctx, "bkt", "obj")

	require.Error(t, err)
	assert.False(t, odIsNotFound(err), "never a miss")
}

func TestPermanentDelete_RemoveContainerTreeAndRemoveEmptyDir(t *testing.T) {
	ctx := common.WithTenantID(context.Background(), "x")
	d, stubs := stubFleet(2, zap.NewNop())
	children := func(tn *odTenant, rel string) string {
		return graphBase + "/drives/" + tn.driveID + "/items/root:/" + odEscapePath(odRootFolder+"/t-x/"+rel) + ":/children?$top=999"
	}
	for i, tn := range d.tenants {
		stubs[i].routes["GET "+children(tn, "c")] = odRoute{200, `{"value":[]}`}
		stubs[i].routes["GET "+itemURL(tn, "t-x/c")] = odRoute{200, `{"id":"C` + tn.name + `"}`}
		stubs[i].routes["POST "+permaURL(tn, "C"+tn.name)] = odRoute{204, ""}
		stubs[i].routes["GET "+children(tn, "p__parity/d1")] = odRoute{200, `{"value":[]}`}
		stubs[i].routes["GET "+itemURL(tn, "t-x/p__parity/d1")] = odRoute{200, `{"id":"D` + tn.name + `"}`}
		stubs[i].routes["POST "+permaURL(tn, "D"+tn.name)] = odRoute{204, ""}
	}

	require.NoError(t, d.RemoveContainerTree(ctx, "c"))
	require.NoError(t, d.RemoveEmptyDir(ctx, "p__parity", "d1"))

	for i, tn := range d.tenants {
		assert.Contains(t, stubs[i].calls, "POST "+permaURL(tn, "C"+tn.name), "container tree on %s", tn.name)
		assert.Contains(t, stubs[i].calls, "POST "+permaURL(tn, "D"+tn.name), "empty dir on %s", tn.name)
		assert.False(t, stubs[i].sawMethod("DELETE"))
	}
}

// No Graph DELETE of an item remains anywhere in the driver (D-F1-5): every
// delete goes through deleteItem.
func TestPermanentDelete_NoGraphDeleteRequestInTheDriver(t *testing.T) {
	files, err := filepath.Glob("onedrive*.go")
	require.NoError(t, err)
	require.NotEmpty(t, files)
	method := regexp.MustCompile(`http\.MethodDelete|"DELETE"`)
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		b, err := os.ReadFile(f) // #nosec G304 -- the package's own sources
		require.NoError(t, err)
		for i, line := range strings.Split(string(b), "\n") {
			assert.False(t, method.MatchString(line), "%s:%d sends a recycle-bin DELETE: %s", f, i+1, strings.TrimSpace(line))
		}
	}
}

// D-F1-6: the probe reads each account's drive quota (what the data app can
// see) into per-account gauges. Graph's `used` excludes the recycle bin;
// `deleted` is the bin; `remaining` = total − used − deleted (measured on
// prod 2026-11-05: 5120 − 20.61 − 68.37 = 5031.02 GiB).
func TestProbe_ExportsTheAccountsQuota(t *testing.T) {
	d, stubs := stubFleet(1, zap.NewNop())
	tn := d.tenants[0]
	stubs[0].routes["GET "+graphBase+"/drives/"+tn.driveID+"?$select=quota"] = odRoute{200,
		`{"quota":{"total":5497558138880,"used":22130000000,"remaining":5402000000000,"deleted":73410000000,"state":"nearing"}}`}

	require.NoError(t, d.HealthCheck(context.Background()))

	q := func(kind string) float64 {
		return testutil.ToFloat64(permafrostQuotaBytes.WithLabelValues(tn.name, kind))
	}
	assert.Equal(t, 5497558138880.0, q("total"))
	assert.Equal(t, 22130000000.0, q("used"))
	assert.Equal(t, 5402000000000.0, q("remaining"))
	assert.Equal(t, 73410000000.0, q("deleted"))
	for _, s := range []string{"normal", "nearing", "critical", "exceeded"} {
		want := 0.0
		if s == "nearing" {
			want = 1
		}
		assert.Equal(t, want, testutil.ToFloat64(permafrostQuotaState.WithLabelValues(tn.name, s)), s)
	}
}

func TestProbe_TheQuotaGaugesAreCollected(t *testing.T) {
	var found int
	for _, c := range Collectors() {
		if c == permafrostQuotaBytes || c == permafrostQuotaState {
			found++
		}
	}
	assert.Equal(t, 2, found)
}
