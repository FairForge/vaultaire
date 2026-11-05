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

// WP-FLEET-1 (D-F1-1, D-F1-2): getDriveID took /users/{upn}/drives[0]. Two of
// the three prod fleet users list a hidden PersonalCacheLibrary before their
// OneDrive library, so the fleet's vaultaire/ tree and its fingerprint sit in
// the cache library on those accounts — and nothing pinned the order: a flip
// would make every object there an off-home read and the fingerprint unknown
// (bench-results/PERMAFROST-FLEET-2026-11-05.md). Now the drive named
// "OneDrive" is chosen in any order, TENANT_N_DRIVE_ID wins without a listing,
// and the marker records the drive it was written on.

// driveFleetStore is one account whose user has several drives, each holding
// files by path under vaultaire/ (the marker).
type driveFleetStore struct {
	mu      sync.Mutex
	upn     string
	drives  []odDrive                    // in the order Graph lists them
	files   map[string]map[string]string // drive id → path → content
	listing int                          // GETs of /users/{upn}/drives
	puts    map[string]int               // drive id → PUTs
}

func newDriveFleetStore(upn string, drives ...odDrive) *driveFleetStore {
	s := &driveFleetStore{upn: upn, drives: drives, files: map[string]map[string]string{}, puts: map[string]int{}}
	for _, d := range drives {
		s.files[d.ID] = map[string]string{}
	}
	return s
}

func (s *driveFleetStore) RoundTrip(req *http.Request) (*http.Response, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	u := req.URL.String()
	if req.Method == http.MethodGet && strings.HasPrefix(u, graphBase+"/users/") && strings.Contains(u, "/drives") {
		s.listing++
		var out struct {
			Value []map[string]string `json:"value"`
		}
		for _, d := range s.drives {
			out.Value = append(out.Value, map[string]string{"id": d.ID, "name": d.Name, "driveType": d.DriveType})
		}
		b, _ := json.Marshal(out)
		return odResp(req, 200, string(b), nil), nil
	}
	for id, files := range s.files {
		meta := graphBase + "/drives/" + id + "/items/root:/" + odRootFolder + "/"
		cdn := "https://cdn.example.test/" + id + "/"
		switch {
		case req.Method == http.MethodGet && strings.HasPrefix(u, meta):
			p := strings.TrimPrefix(u, meta)
			if _, ok := files[p]; !ok {
				return odResp(req, 404, `{"error":{"code":"itemNotFound"}}`, nil), nil
			}
			return odResp(req, 200, `{"@microsoft.graph.downloadUrl":"`+cdn+p+`","size":1}`, nil), nil
		case req.Method == http.MethodGet && strings.HasPrefix(u, cdn):
			return odResp(req, 200, files[strings.TrimPrefix(u, cdn)], nil), nil
		case req.Method == http.MethodPut && strings.HasPrefix(u, graphBase+"/drives/"+id+"/items/ROOT:/"):
			p := strings.TrimSuffix(strings.TrimPrefix(u, graphBase+"/drives/"+id+"/items/ROOT:/"), ":/content")
			b, _ := io.ReadAll(req.Body)
			files[p] = string(b)
			s.puts[id]++
			return odResp(req, 201, `{}`, nil), nil
		}
	}
	return odResp(req, 404, `{"error":{"code":"itemNotFound"}}`, nil), nil
}

func (s *driveFleetStore) listings() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.listing
}

func (s *driveFleetStore) putsOn(driveID string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.puts[driveID]
}

func (s *driveFleetStore) markerOn(t *testing.T, driveID string) fleetMarker {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	var m fleetMarker
	require.NoError(t, json.Unmarshal([]byte(s.files[driveID][fleetMarkerPath]), &m))
	return m
}

// driveTenant is an account with no drive chosen yet.
func driveTenant(name string, s *driveFleetStore) *odTenant {
	tn := stubTenant(name, s)
	tn.driveID = ""
	tn.userUPN = s.upn
	tn.tenantID = "tid-" + name
	tn.rootFolderID = "ROOT"
	return tn
}

var (
	cacheLib  = odDrive{ID: "b!CACHE", Name: "PersonalCacheLibrary", DriveType: "business"}
	oneDrive  = odDrive{ID: "b!ONEDRIVE", Name: "OneDrive", DriveType: "business"}
	documents = odDrive{ID: "b!DOCS", Name: "Documents", DriveType: "business"}
)

func TestDriveSelect_TheOneDriveLibraryInEitherOrder(t *testing.T) {
	for _, order := range [][]odDrive{{cacheLib, oneDrive}, {oneDrive, cacheLib}} {
		s := newDriveFleetStore("u@fleet.test", order...)
		tn := driveTenant("a", s)

		id, err := tn.getDriveID(context.Background())

		require.NoError(t, err)
		assert.Equal(t, oneDrive.ID, id, "listed %s first", order[0].Name)
	}
}

func TestDriveSelect_APinnedDriveWinsWithoutAListing(t *testing.T) {
	s := newDriveFleetStore("u@fleet.test", cacheLib, oneDrive)
	tn := driveTenant("a", s)
	tn.pinnedDriveID = cacheLib.ID

	id, err := tn.getDriveID(context.Background())

	require.NoError(t, err)
	assert.Equal(t, cacheLib.ID, id)
	assert.Equal(t, 0, s.listings(), "a pin is used without a listing")
}

func TestDriveSelect_OneDriveListedIsThatDrive(t *testing.T) {
	s := newDriveFleetStore("u@fleet.test", documents)
	tn := driveTenant("a", s)

	id, err := tn.getDriveID(context.Background())

	require.NoError(t, err)
	assert.Equal(t, documents.ID, id)
}

func TestDriveSelect_NoOneDriveAndNoPinSkipsTheSlotWithAnError(t *testing.T) {
	// Arrange: account a has two drives, neither named OneDrive; b is normal.
	core, logs := observer.New(zap.InfoLevel)
	a := driveTenant("a", newDriveFleetStore("a@fleet.test", cacheLib, documents))
	b := driveTenant("b", newDriveFleetStore("b@fleet.test", cacheLib, oneDrive))

	// Act
	kept := initFleetDrives(context.Background(), zap.New(core), []*odTenant{a, b})

	// Assert: a skipped (as an incomplete slot is), named with the drives found.
	require.Len(t, kept, 1)
	assert.Equal(t, "b", kept[0].name)
	errs := logs.FilterLevelExact(zapcore.ErrorLevel).All()
	require.Len(t, errs, 1)
	assert.Equal(t, "a", errs[0].ContextMap()["account"])
	assert.Contains(t, errs[0].ContextMap()["drives"], "PersonalCacheLibrary")
	assert.Contains(t, errs[0].ContextMap()["drives"], "Documents")
	info := logs.FilterMessage("permafrost account drive").All()
	require.Len(t, info, 1, "one Info line per kept slot")
	assert.Equal(t, oneDrive.ID, info[0].ContextMap()["drive_id"])
	assert.Equal(t, []any{"PersonalCacheLibrary"}, info[0].ContextMap()["other_drives"])
}

func TestDriveSelect_AGraphFailureAtBootKeepsTheSlot(t *testing.T) {
	// A listing that fails (Graph down at boot) is retried on first use, as
	// before — only a listing that names no OneDrive skips the slot.
	core, _ := observer.New(zap.InfoLevel)
	a := stubTenant("a", newODRouteStub())
	a.driveID, a.userUPN = "", "a@fleet.test"
	b := driveTenant("b", newDriveFleetStore("b@fleet.test", oneDrive))

	kept := initFleetDrives(context.Background(), zap.New(core), []*odTenant{a, b})

	assert.Len(t, kept, 2)
}

func TestFleetMarker_RecordsTheDriveItWasWrittenOn(t *testing.T) {
	s := newDriveFleetStore("a@fleet.test", cacheLib, oneDrive)
	d := &OneDriveDriver{logger: zap.NewNop(), tenants: []*odTenant{driveTenant("a", s)}}

	got, err := d.CheckFleetFingerprint(context.Background(), "")

	require.NoError(t, err)
	assert.Equal(t, FleetRecorded, got.State)
	assert.Equal(t, oneDrive.ID, s.markerOn(t, oneDrive.ID).DriveID)
	assert.Equal(t, 0, s.putsOn(cacheLib.ID))
}

// recordedFleet writes markers for the accounts as a first boot would, every
// account's marker on drive (the cache library, as prod's slots 1 and 3 are).
func recordedFleet(t *testing.T, drive odDrive, stores ...*driveFleetStore) {
	t.Helper()
	d := &OneDriveDriver{logger: zap.NewNop()}
	for i, s := range stores {
		tn := driveTenant(string(rune('a'+i)), s)
		tn.pinnedDriveID = drive.ID
		d.tenants = append(d.tenants, tn)
	}
	_, err := d.CheckFleetFingerprint(context.Background(), "")
	require.NoError(t, err)
}

func TestFleetMarker_ADriveIDOtherThanTheChosenIsOneErrorPerAccountAndTheGauge(t *testing.T) {
	// Arrange: markers recorded on the cache library; the marker of account
	// a is then copied to its OneDrive library (as the migration's copy
	// does) and the slot boots on that drive.
	sa := newDriveFleetStore("a@fleet.test", cacheLib, oneDrive)
	sb := newDriveFleetStore("b@fleet.test", cacheLib, oneDrive)
	recordedFleet(t, cacheLib, sa, sb)
	sa.files[oneDrive.ID][fleetMarkerPath] = sa.files[cacheLib.ID][fleetMarkerPath]
	core, logs := observer.New(zap.InfoLevel)
	a, b := driveTenant("a", sa), driveTenant("b", sb)
	a.pinnedDriveID = oneDrive.ID
	b.pinnedDriveID = cacheLib.ID
	d := &OneDriveDriver{logger: zap.New(core), tenants: []*odTenant{a, b}}

	// Act
	got, err := d.CheckFleetFingerprint(context.Background(), "")

	// Assert: the order matches; account a's drive does not; never a switch.
	require.NoError(t, err)
	assert.Equal(t, FleetMatch, got.State)
	assert.Equal(t, 1.0, testutil.ToFloat64(permafrostDriveMismatch.WithLabelValues("a")))
	assert.Equal(t, 0.0, testutil.ToFloat64(permafrostDriveMismatch.WithLabelValues("b")))
	errs := logs.FilterLevelExact(zapcore.ErrorLevel).All()
	require.Len(t, errs, 1)
	assert.Equal(t, "a", errs[0].ContextMap()["account"])
	assert.Equal(t, cacheLib.ID, errs[0].ContextMap()["marker_drive_id"])
	assert.Equal(t, oneDrive.ID, errs[0].ContextMap()["drive_id"])
	assert.Equal(t, 0, sa.putsOn(oneDrive.ID), "nothing rewritten")

	// A re-check with the same verdict logs nothing more.
	_, err = d.CheckFleetFingerprint(context.Background(), "")
	require.NoError(t, err)
	assert.Len(t, logs.FilterLevelExact(zapcore.ErrorLevel).All(), 1)
	permafrostDriveMismatch.WithLabelValues("a").Set(0)
}

func TestFleetMarker_AMarkerWithoutADriveIDReadsAsBefore(t *testing.T) {
	// The markers prod holds today were written before drive_id: no verdict
	// on the drive, the fleet's as before.
	s := newDriveFleetStore("a@fleet.test", cacheLib, oneDrive)
	tn := driveTenant("a", s)
	tn.pinnedDriveID = cacheLib.ID
	old, _ := json.Marshal(map[string]any{"version": 1, "index": 0, "fleet": []string{tn.identity()}, "digest": fleetDigest([]string{tn.identity()})})
	s.files[cacheLib.ID][fleetMarkerPath] = string(old)
	d := &OneDriveDriver{logger: zap.NewNop(), tenants: []*odTenant{tn}}

	got, err := d.CheckFleetFingerprint(context.Background(), "")

	require.NoError(t, err)
	assert.Equal(t, FleetMatch, got.State)
	assert.Equal(t, 0.0, testutil.ToFloat64(permafrostDriveMismatch.WithLabelValues("a")))
}

func TestFleetMarker_TheTreeOnAnotherDriveOfTheUserIsAMismatchNeverAFreshMarker(t *testing.T) {
	// Arrange: the markers sit on the cache library (prod's slots 1 and 3);
	// a deploy without the pin lets the selection choose the OneDrive
	// library, which holds no vaultaire/ at all. The check must not write a
	// fresh marker there and call the fleet repaired: every object of that
	// account would be an off-home read.
	sa := newDriveFleetStore("a@fleet.test", cacheLib, oneDrive)
	sb := newDriveFleetStore("b@fleet.test", cacheLib, oneDrive)
	recordedFleet(t, cacheLib, sa, sb)
	core, logs := observer.New(zap.InfoLevel)
	a, b := driveTenant("a", sa), driveTenant("b", sb)
	b.pinnedDriveID = cacheLib.ID
	d := &OneDriveDriver{logger: zap.New(core), tenants: []*odTenant{a, b}}

	// Act
	got, err := d.CheckFleetFingerprint(context.Background(), "")

	// Assert
	require.NoError(t, err)
	assert.Equal(t, FleetMatch, got.State, "the order is read from the marker where the tree is")
	assert.Equal(t, 0, sa.putsOn(oneDrive.ID), "no fresh marker on the empty drive")
	assert.Equal(t, 1.0, testutil.ToFloat64(permafrostDriveMismatch.WithLabelValues("a")))
	errs := logs.FilterLevelExact(zapcore.ErrorLevel).All()
	require.Len(t, errs, 1)
	assert.Equal(t, cacheLib.ID, errs[0].ContextMap()["marker_drive_id"])
	permafrostDriveMismatch.WithLabelValues("a").Set(0)
}

func TestFleetMarker_TheDriveMismatchGaugeIsCollected(t *testing.T) {
	found := false
	for _, c := range Collectors() {
		if c == permafrostDriveMismatch {
			found = true
		}
	}
	assert.True(t, found, "on drivers.Collectors() — declared is not registered (be9d044d)")
}

func TestRewriteFleetMarker_SetsTheDriveAndKeepsTheRest(t *testing.T) {
	in, _ := json.Marshal(fleetMarker{Version: 1, Index: 2, Fleet: []string{"x", "y", "z"}, Digest: "dg"})

	out, err := RewriteFleetMarker(in, "b!NEW", "b!NEW")

	require.NoError(t, err)
	var m fleetMarker
	require.NoError(t, json.Unmarshal(out, &m))
	assert.Equal(t, 2, m.Index)
	assert.Equal(t, []string{"x", "y", "z"}, m.Fleet)
	assert.Equal(t, "dg", m.Digest)
	assert.Equal(t, "b!NEW", m.DriveID)
	assert.Equal(t, "b!NEW", m.MovedTo)
	assert.Contains(t, string(out), `"drive_id":"b!NEW"`)
	assert.Contains(t, string(out), `"moved_to":"b!NEW"`)
}
