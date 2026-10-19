package drivers

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// WP-R7-4 R7-09: the fleet is built from TENANT_1..15 skipping gaps and an
// object's home is hash % fleet size over that order — removing TENANT_2
// silently renumbered every later account and remapped placement (reads
// survived through the fallback walk, at a fleet-walk per read). Nothing
// recorded the order. Now every account carries a marker
// (vaultaire/_fleet/fingerprint.json: its index and the ordered fleet), written
// at first boot and compared on every boot: a mismatch is one Error line
// naming the diff and vaultaire_permafrost_fleet_mismatch 1. Boot is never
// blocked.

// markerStore is one fleet account holding files by path under vaultaire/.
type markerStore struct {
	mu      sync.Mutex
	name    string
	files   map[string]string
	puts    int
	failGet bool
}

func (m *markerStore) RoundTrip(req *http.Request) (*http.Response, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	u := req.URL.String()
	const meta = "/items/root:/" + odRootFolder + "/"
	switch {
	case req.Method == http.MethodGet && strings.Contains(u, meta):
		if m.failGet {
			return odResp(req, 500, "", nil), nil
		}
		p := u[strings.Index(u, meta)+len(meta):]
		if _, ok := m.files[p]; !ok {
			return odResp(req, 404, `{"error":{"code":"itemNotFound"}}`, nil), nil
		}
		return odResp(req, 200, `{"@microsoft.graph.downloadUrl":"https://cdn.example.test/`+m.name+`/`+p+`","size":1}`, nil), nil
	case req.Method == http.MethodGet && strings.HasPrefix(u, "https://cdn.example.test/"+m.name+"/"):
		p := strings.TrimPrefix(u, "https://cdn.example.test/"+m.name+"/")
		return odResp(req, 200, m.files[p], nil), nil
	case req.Method == http.MethodPut && strings.HasSuffix(u, ":/content"):
		i := strings.Index(u, "/items/ROOT:/")
		p := strings.TrimSuffix(u[i+len("/items/ROOT:/"):], ":/content")
		b, _ := io.ReadAll(req.Body)
		m.files[p] = string(b)
		m.puts++
		return odResp(req, 201, `{}`, nil), nil
	}
	return odResp(req, 404, `{"error":{"code":"itemNotFound"}}`, nil), nil
}

func (m *markerStore) marker(t *testing.T) fleetMarker {
	t.Helper()
	m.mu.Lock()
	defer m.mu.Unlock()
	var fm fleetMarker
	require.NoError(t, json.Unmarshal([]byte(m.files[fleetMarkerPath]), &fm))
	return fm
}

func (m *markerStore) putCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.puts
}

// markerFleet builds a driver over the given accounts, in order.
func markerFleet(logger *zap.Logger, accounts ...*markerStore) *OneDriveDriver {
	d := &OneDriveDriver{logger: logger}
	for _, a := range accounts {
		tn := stubTenant(a.name, a)
		tn.tenantID = "tid-" + a.name
		tn.userUPN = a.name + "@fleet.test"
		tn.rootFolderID = "ROOT"
		d.tenants = append(d.tenants, tn)
	}
	return d
}

func newAccounts(names ...string) []*markerStore {
	out := make([]*markerStore, len(names))
	for i, n := range names {
		out[i] = &markerStore{name: n, files: map[string]string{}}
	}
	return out
}

func TestFleetFingerprint_FirstBootRecordsAMarkerOnEveryAccount(t *testing.T) {
	acc := newAccounts("a", "b", "c")
	d := markerFleet(zap.NewNop(), acc...)

	got, err := d.CheckFleetFingerprint(context.Background(), "")

	require.NoError(t, err)
	assert.Equal(t, FleetRecorded, got.State)
	for i, a := range acc {
		fm := a.marker(t)
		assert.Equal(t, i, fm.Index)
		assert.Equal(t, d.fleetIdentities(), fm.Fleet)
		assert.Equal(t, got.Digest, fm.Digest)
	}
	assert.Equal(t, 0.0, testutil.ToFloat64(permafrostFleetMismatch))
}

func TestFleetFingerprint_TheSameFleetMatchesAndWritesNothing(t *testing.T) {
	acc := newAccounts("a", "b", "c")
	_, err := markerFleet(zap.NewNop(), acc...).CheckFleetFingerprint(context.Background(), "")
	require.NoError(t, err)

	got, err := markerFleet(zap.NewNop(), acc...).CheckFleetFingerprint(context.Background(), "")

	require.NoError(t, err)
	assert.Equal(t, FleetMatch, got.State)
	for _, a := range acc {
		assert.Equal(t, 1, a.putCount(), "the first boot's write only")
	}
	assert.Equal(t, 0.0, testutil.ToFloat64(permafrostFleetMismatch))
}

func TestFleetFingerprint_ARemovedAccountIsAMismatchNamedInOneErrorLine(t *testing.T) {
	// Arrange: a three-account fleet recorded; then TENANT_2 is removed and
	// the third account becomes index 1.
	acc := newAccounts("a", "b", "c")
	_, err := markerFleet(zap.NewNop(), acc...).CheckFleetFingerprint(context.Background(), "")
	require.NoError(t, err)
	core, logs := observer.New(zap.InfoLevel)

	// Act
	got, err := markerFleet(zap.New(core), acc[0], acc[2]).CheckFleetFingerprint(context.Background(), "")

	// Assert
	require.NoError(t, err)
	assert.Equal(t, FleetMismatch, got.State)
	assert.Equal(t, 1.0, testutil.ToFloat64(permafrostFleetMismatch))
	errs := logs.FilterLevelExact(zapcore.ErrorLevel).All()
	require.Len(t, errs, 1)
	diff := strings.Join(got.Diffs, "; ")
	assert.Contains(t, diff, "index 1")
	assert.Contains(t, diff, "size 3 → 2")
	assert.Equal(t, got.Digest, errs[0].ContextMap()["fingerprint"], "the digest to accept is in the line")
	for _, a := range acc {
		assert.Equal(t, 1, a.putCount(), "a mismatch rewrites nothing")
	}
	permafrostFleetMismatch.Set(0)
}

func TestFleetFingerprint_AnAppendedAccountIsAMismatch(t *testing.T) {
	acc := newAccounts("a", "b", "c")
	_, err := markerFleet(zap.NewNop(), acc[0], acc[1]).CheckFleetFingerprint(context.Background(), "")
	require.NoError(t, err)

	got, err := markerFleet(zap.NewNop(), acc...).CheckFleetFingerprint(context.Background(), "")

	require.NoError(t, err)
	assert.Equal(t, FleetMismatch, got.State, "appending changes the modulus: every home moves")
	assert.Equal(t, 0, acc[2].putCount())
	permafrostFleetMismatch.Set(0)
}

func TestFleetFingerprint_AcceptingTheDigestRewritesTheMarkers(t *testing.T) {
	acc := newAccounts("a", "b", "c")
	_, err := markerFleet(zap.NewNop(), acc...).CheckFleetFingerprint(context.Background(), "")
	require.NoError(t, err)
	shrunk := markerFleet(zap.NewNop(), acc[0], acc[2])
	first, err := shrunk.CheckFleetFingerprint(context.Background(), "")
	require.NoError(t, err)
	require.Equal(t, FleetMismatch, first.State)

	wrong, err := shrunk.CheckFleetFingerprint(context.Background(), "not-the-digest")
	require.NoError(t, err)
	assert.Equal(t, FleetMismatch, wrong.State, "only the current digest accepts")

	got, err := shrunk.CheckFleetFingerprint(context.Background(), first.Digest)
	require.NoError(t, err)
	assert.Equal(t, FleetAccepted, got.State)
	assert.Equal(t, 0.0, testutil.ToFloat64(permafrostFleetMismatch))
	assert.Equal(t, 1, acc[2].marker(t).Index)

	again, err := shrunk.CheckFleetFingerprint(context.Background(), first.Digest)
	require.NoError(t, err)
	assert.Equal(t, FleetMatch, again.State, "an accept left set does nothing once the markers match")
}

func TestFleetFingerprint_AnUnreadableMarkerIsNoVerdict(t *testing.T) {
	acc := newAccounts("a", "b")
	acc[1].failGet = true
	core, logs := observer.New(zap.InfoLevel)

	got, err := markerFleet(zap.New(core), acc...).CheckFleetFingerprint(context.Background(), "")

	require.Error(t, err)
	assert.Equal(t, FleetUnknown, got.State)
	assert.Equal(t, 0, acc[0].putCount(), "no first-boot write while an account cannot be read")
	assert.Equal(t, 0.0, testutil.ToFloat64(permafrostFleetMismatch))
	assert.Empty(t, logs.FilterLevelExact(zapcore.ErrorLevel).All())
}

func TestFleetFingerprint_AMissingMarkerOnAMatchingFleetIsRewritten(t *testing.T) {
	acc := newAccounts("a", "b")
	_, err := markerFleet(zap.NewNop(), acc...).CheckFleetFingerprint(context.Background(), "")
	require.NoError(t, err)
	delete(acc[1].files, fleetMarkerPath)

	got, err := markerFleet(zap.NewNop(), acc...).CheckFleetFingerprint(context.Background(), "")

	require.NoError(t, err)
	assert.Equal(t, FleetRepaired, got.State)
	assert.Equal(t, 1, acc[1].marker(t).Index)
}

func TestFleetFingerprint_TheMismatchGaugeExistsAtZero(t *testing.T) {
	found := false
	for _, c := range Collectors() {
		if c == permafrostFleetMismatch {
			found = true
		}
	}
	assert.True(t, found, "on drivers.Collectors(): exported at 0 from registration")
}
