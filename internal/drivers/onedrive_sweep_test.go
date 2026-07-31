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
	path := d.buildPath(ctx, "bkt", "obj")
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
	path := d.buildPath(ctx, "bkt", "obj")
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
	home := d.homeTenantIndex(d.buildPath(ctx, "bkt", "obj"))
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
	path := d.buildPath(ctx, "bkt", "obj")
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
