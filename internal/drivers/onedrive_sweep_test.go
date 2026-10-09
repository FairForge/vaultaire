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
	"go.uber.org/zap/zaptest/observer"
)

// odRouteStub routes "METHOD url" to a canned response; unrouted requests
// get a Graph-style 404. Every request is recorded.
type odRouteStub struct {
	mu     sync.Mutex
	calls  []string
	routes map[string]odRoute
}

type odRoute struct {
	status int
	body   string
}

func newODRouteStub() *odRouteStub {
	return &odRouteStub{routes: map[string]odRoute{}}
}

func (s *odRouteStub) RoundTrip(req *http.Request) (*http.Response, error) {
	key := req.Method + " " + req.URL.String()
	s.mu.Lock()
	s.calls = append(s.calls, key)
	s.mu.Unlock()
	r, ok := s.routes[key]
	if !ok {
		r = odRoute{status: 404, body: `{"error":{"code":"itemNotFound"}}`}
	}
	return &http.Response{
		StatusCode: r.status,
		Body:       io.NopCloser(strings.NewReader(r.body)),
		Header:     make(http.Header),
		Request:    req,
	}, nil
}

func (s *odRouteStub) sawMethod(method string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, c := range s.calls {
		if strings.HasPrefix(c, method+" ") {
			return true
		}
	}
	return false
}

func stubTenant(name string, rt http.RoundTripper) *odTenant {
	return &odTenant{
		name:      name,
		driveID:   "DRIVE-" + name,
		cachedTok: "stub-token",
		tokExpiry: time.Now().Add(time.Hour),
		graphHTTP: &http.Client{Transport: rt},
		cdnHTTP:   &http.Client{Transport: rt},
	}
}

// stubFleet builds an n-account fleet where every account serves canned
// Graph responses from its own route table.
func stubFleet(n int, logger *zap.Logger) (*OneDriveDriver, []*odRouteStub) {
	d := &OneDriveDriver{logger: logger}
	stubs := make([]*odRouteStub, n)
	for i := 0; i < n; i++ {
		stubs[i] = newODRouteStub()
		d.tenants = append(d.tenants, stubTenant(string(rune('a'+i)), stubs[i]))
	}
	return d, stubs
}

func itemURL(tenant *odTenant, path string) string {
	return graphBase + "/drives/" + tenant.driveID + "/items/root:/" + odRootFolder + "/" + odEscapePath(path)
}

// The old round-robin Put could leave copies of the SAME path on several
// accounts (every overwrite landed wherever the rotation pointed). Delete
// must sweep the whole fleet: removing only the first copy found leaves a
// stale duplicate that the next Get's fleet probe resurrects.
func TestOneDriveDelete_SweepsAllFleetCopies(t *testing.T) {
	ctx := common.WithTenantID(context.Background(), "x")
	d, stubs := stubFleet(3, zap.NewNop())
	path := d.ObjectKey(ctx, "bkt", "obj")
	home := d.homeTenantIndex(path)
	dup := (home + 1) % 3

	for idx, id := range map[int]string{home: "ID-HOME", dup: "ID-DUP"} {
		tn := d.tenants[idx]
		stubs[idx].routes["GET "+itemURL(tn, path)] = odRoute{200, `{"id":"` + id + `"}`}
		stubs[idx].routes["DELETE "+graphBase+"/drives/"+tn.driveID+"/items/"+id] = odRoute{204, ""}
	}

	require.NoError(t, d.Delete(ctx, "bkt", "obj"))
	assert.True(t, stubs[home].sawMethod("DELETE"), "home copy must be deleted")
	assert.True(t, stubs[dup].sawMethod("DELETE"),
		"duplicate on account %q survived — next Get resurrects the object", d.tenants[dup].name)
}

// Deleting an object that exists nowhere must still surface an error
// (the engine relies on it to detect double-deletes / bad paths).
func TestOneDriveDelete_MissingEverywhereErrors(t *testing.T) {
	ctx := common.WithTenantID(context.Background(), "x")
	d, _ := stubFleet(3, zap.NewNop())
	require.Error(t, d.Delete(ctx, "bkt", "nope"))
}

// One dead account must not abort the probe: pre-placement data reachable on
// a healthy account should still be found. (Get already continues past
// drive-lookup failures; Exists must behave the same.)
func TestOneDriveExists_SurvivesDownAccount(t *testing.T) {
	ctx := common.WithTenantID(context.Background(), "x")
	d, stubs := stubFleet(2, zap.NewNop())
	path := d.ObjectKey(ctx, "bkt", "obj")
	home := d.homeTenantIndex(path)
	other := (home + 1) % 2

	// Home account is down: no cached driveID and its drive lookup fails.
	d.tenants[home].driveID = ""
	d.tenants[home].userUPN = "brokenupn"

	stubs[other].routes["GET "+itemURL(d.tenants[other], path)] = odRoute{200, `{"id":"ID-1"}`}

	found, err := d.Exists(ctx, "bkt", "obj")
	require.NoError(t, err, "a down account must not abort the fleet probe")
	assert.True(t, found)
}

// If an account was unreachable and the object was found nowhere else,
// Exists cannot honestly answer "false, nil" — the object may live on the
// unreachable account.
func TestOneDriveExists_DownAccountNotFoundIsError(t *testing.T) {
	ctx := common.WithTenantID(context.Background(), "x")
	d, _ := stubFleet(2, zap.NewNop())
	home := d.homeTenantIndex(d.ObjectKey(ctx, "bkt", "obj"))
	d.tenants[home].driveID = ""
	d.tenants[home].userUPN = "brokenupn"

	_, err := d.Exists(ctx, "bkt", "obj")
	require.Error(t, err, "cannot claim absence while an account is unreachable")
}

// Fallback hits (object served from a non-home account) are the signal that
// pre-placement data still exists. They must be logged: when they stop, the
// N-account probe on misses can be retired.
func TestOneDriveGet_LogsFallbackHit(t *testing.T) {
	core, logs := observer.New(zap.InfoLevel)
	ctx := common.WithTenantID(context.Background(), "x")
	d, stubs := stubFleet(2, zap.New(core))
	path := d.ObjectKey(ctx, "bkt", "obj")
	other := (d.homeTenantIndex(path) + 1) % 2

	dlURL := "https://cdn.example.test/obj"
	stubs[other].routes["GET "+itemURL(d.tenants[other], path)] =
		odRoute{200, `{"@microsoft.graph.downloadUrl":"` + dlURL + `","size":5}`}
	stubs[other].routes["GET "+dlURL] = odRoute{200, "hello"}

	rc, err := d.Get(ctx, "bkt", "obj")
	require.NoError(t, err)
	body, err := io.ReadAll(rc)
	require.NoError(t, err)
	_ = rc.Close()
	assert.Equal(t, "hello", string(body))

	hits := logs.FilterMessage("onedrive fallback hit")
	require.Equal(t, 1, hits.Len(), "a non-home read must log a fallback hit")
	assert.Equal(t, d.tenants[other].name, hits.All()[0].ContextMap()["tenant"])
}

// WP-R10-3c: permafrost cannot walk a tenant (a folder tree on every fleet
// account), so the account-erasure sweep uses List + Delete per bucket on it.
// List returns the container's DIRECT children on every account — a key with
// a '/' shows up as its first folder — and Delete of that name removes the
// folder item on each account that has it (Graph deletes a folder with its
// subtree). Every request stays under `t-<tenant>/<container>`.
func TestOneDriveSweepShape_ListsDirectChildrenAndDeletesEachItemOnEveryAccount(t *testing.T) {
	// Arrange
	ctx := common.WithTenantID(context.Background(), "x")
	d, stubs := stubFleet(3, zap.NewNop())
	childrenURL := func(tn *odTenant, container string) string {
		return graphBase + "/drives/" + tn.driveID + "/items/root:/" + odEscapePath(odRootFolder+"/t-x/"+container) + ":/children?$top=999"
	}
	a, b := d.tenants[0], d.tenants[1]
	stubs[0].routes["GET "+childrenURL(a, "c")] = odRoute{200, `{"value":[{"name":"photos"},{"name":"k.txt"}]}`}
	stubs[1].routes["GET "+childrenURL(b, "c")] = odRoute{200, `{"value":[{"name":"photos"}]}`}
	for i, tn := range []*odTenant{a, b} {
		id := "FOLDER-" + tn.name
		stubs[i].routes["GET "+itemURL(tn, "t-x/c/photos")] = odRoute{200, `{"id":"` + id + `"}`}
		stubs[i].routes["DELETE "+graphBase+"/drives/"+tn.driveID+"/items/"+id] = odRoute{204, ""}
	}

	// Act
	names, err := d.List(ctx, "c", "")
	require.NoError(t, err)
	delErr := d.Delete(ctx, "c", "photos")

	// Assert
	assert.ElementsMatch(t, []string{"photos", "k.txt"}, names, "direct children of the container, the union of the fleet")
	require.NoError(t, delErr)
	for i, tn := range []*odTenant{a, b} {
		assert.Contains(t, stubs[i].calls, "DELETE "+graphBase+"/drives/"+tn.driveID+"/items/FOLDER-"+tn.name,
			"the folder item is deleted on account %s", tn.name)
	}
	for i := range stubs {
		for _, call := range stubs[i].calls {
			inContainer := strings.Contains(call, "/t-x/c:/children") || strings.Contains(call, "/t-x/c/photos")
			assert.True(t, inContainer || strings.Contains(call, "/items/FOLDER-"),
				"request outside the tenant's container: %s", call)
		}
	}
}

// Prompt 2a.2 G3: the vault parity reconcile walks `<digest>/<etag>/` on the
// permafrost leg; List sees only the container's direct children (a digest
// folder), so the walk was a silent no-op there. ListDir lists any folder's
// direct children, sub-folders told from files, the union of the fleet.
func TestOneDriveListDir_ListsAFoldersChildrenOnEveryAccount(t *testing.T) {
	ctx := common.WithTenantID(context.Background(), "x")
	d, stubs := stubFleet(2, zap.NewNop())
	childrenURL := func(tn *odTenant, path string) string {
		return graphBase + "/drives/" + tn.driveID + "/items/root:/" + odEscapePath(odRootFolder+"/t-x/"+path) + ":/children?$top=999"
	}
	a, b := d.tenants[0], d.tenants[1]
	stubs[0].routes["GET "+childrenURL(a, "c__parity/d1")] = odRoute{200, `{"value":[{"name":"e1","folder":{"childCount":4}}]}`}
	stubs[1].routes["GET "+childrenURL(b, "c__parity/d1")] = odRoute{200, `{"value":[{"name":"e2","folder":{"childCount":0}},{"name":"stray"}]}`}
	stubs[0].routes["GET "+childrenURL(a, "c__parity/d1/e1")] = odRoute{200, `{"value":[{"name":"p0","file":{}},{"name":"p1","file":{}}]}`}

	dirs, files, err := d.ListDir(ctx, "c__parity", "d1")
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"e1", "e2"}, dirs)
	assert.Equal(t, []string{"stray"}, files)

	dirs, files, err = d.ListDir(ctx, "c__parity", "d1/e1")
	require.NoError(t, err)
	assert.Empty(t, dirs)
	assert.ElementsMatch(t, []string{"p0", "p1"}, files)
}

// Prompt 2a.3 H1: the parity reconcile walks permafrost with ListDir, but the
// fleet could not remove a folder — every emptied `<etag>`/`<digest>` folder
// was listed again on every walk, forever. RemoveEmptyDir removes a folder
// only when no account of the fleet holds anything in it.
func TestOneDriveRemoveEmptyDir_RemovesAFolderEmptyOnEveryAccount(t *testing.T) {
	ctx := common.WithTenantID(context.Background(), "x")
	d, stubs := stubFleet(2, zap.NewNop())
	path := d.ObjectKey(ctx, "c__parity", "d1/e1")
	for i, tn := range d.tenants {
		stubs[i].routes["GET "+graphBase+"/drives/"+tn.driveID+"/items/root:/"+odEscapePath(odRootFolder+"/"+path)+":/children?$top=999"] = odRoute{200, `{"value":[]}`}
		stubs[i].routes["GET "+itemURL(tn, path)] = odRoute{200, `{"id":"F` + tn.name + `"}`}
		stubs[i].routes["DELETE "+graphBase+"/drives/"+tn.driveID+"/items/F"+tn.name] = odRoute{204, ""}
	}

	require.NoError(t, d.RemoveEmptyDir(ctx, "c__parity", "d1/e1"))
	assert.True(t, stubs[0].sawMethod("DELETE"))
	assert.True(t, stubs[1].sawMethod("DELETE"))
}

func TestOneDriveRemoveEmptyDir_NeverDeletesWhatOneAccountHolds(t *testing.T) {
	ctx := common.WithTenantID(context.Background(), "x")
	d, stubs := stubFleet(2, zap.NewNop())
	path := d.ObjectKey(ctx, "c__parity", "d1/e1")
	b := d.tenants[1]
	stubs[1].routes["GET "+graphBase+"/drives/"+b.driveID+"/items/root:/"+odEscapePath(odRootFolder+"/"+path)+":/children?$top=999"] = odRoute{200, `{"value":[{"name":"p0","file":{}}]}`}

	err := d.RemoveEmptyDir(ctx, "c__parity", "d1/e1")

	require.ErrorIs(t, err, ErrDirNotEmpty)
	assert.False(t, stubs[0].sawMethod("DELETE"))
	assert.False(t, stubs[1].sawMethod("DELETE"))
}

func TestOneDriveRemoveEmptyDir_MissingIsFineAndNeverTheContainer(t *testing.T) {
	ctx := common.WithTenantID(context.Background(), "x")
	d, _ := stubFleet(2, zap.NewNop())
	require.NoError(t, d.RemoveEmptyDir(ctx, "c__parity", "nope/none"))
	assert.Error(t, d.RemoveEmptyDir(ctx, "c__parity", ""))
	assert.Error(t, d.RemoveEmptyDir(ctx, "c__parity", "/"))
}
