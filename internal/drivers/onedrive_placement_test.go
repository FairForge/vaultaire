package drivers

import (
	"context"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"io"
	"net/http"
	"sort"
	"strings"
	"testing"

	"github.com/FairForge/vaultaire/internal/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// WP-FLEET-5 (docs/reviews/WP-FLEET-5-PROMPT.md): placement over (tenant,
// drive). Measured: placement was hash(full path) % slots, so the fourth
// drive remaps three quarters of every home; one object's four parity shards
// landed wherever their names hashed — two on one tenant on 2026-11-06's
// soak4, where the 400 GB/h per-app-per-tenant budget bit first; ListDir was
// the fleet's union because no drive could be named for a folder.
//
//   - D-F5-1 placement from the key alone: the key's GROUP (its parent
//     folder) and its SHARD index j (a leaf `p<j>`, else 0). Each shard ranks
//     the fleet's tenants by rendezvous on (tenant key, group, j) and takes
//     the first whose shard count is under ⌈max(spread, j+1)/n⌉; the drive
//     inside the tenant is rendezvous on (drive identity, group) — every file
//     of one group that lands on a tenant lands on ONE drive of it.
//   - D-F5-2 the marker records placement_version (absent = 1 = hash % slots
//     over the recorded order; 2 = this) and the tenant grouping, so a reader
//     can compute the OLD placement exactly.
//   - D-F5-3 the fallback is the previous placement, computed — never a walk.
//   - D-F5-4 ListDir of a group folder asks the group's spread set.
//
// The fleet's version is what its markers say: prod's three accounts, whose
// markers predate the field, stay version 1 and authoritative with nothing
// rewritten; a first boot records version 2; the rebalance tool moves a
// fleet from 1 to 2 (placement_target while it runs, placement_version last).

// ── the pure placement ──────────────────────────────────────────────────────

// synthFleet is n tenants of d drives each, identities "t<i>/d<k>".
func synthFleet(tenants, drives int) []odPlacedTenant {
	out := make([]odPlacedTenant, tenants)
	idx := 0
	for i := range out {
		out[i].key = fmt.Sprintf("tenant-%d", i)
		for k := 0; k < drives; k++ {
			idx++
			out[i].drives = append(out[i].drives, odPlaced{index: idx, identity: fmt.Sprintf("t%d/d%d", i, k)})
		}
	}
	return out
}

func tenantOf(p *odPlacement, path string) string {
	home := p.home(path)
	for _, t := range p.tenants {
		for _, dr := range t.drives {
			if dr.identity == home.identity {
				return t.key
			}
		}
	}
	return "?"
}

func parityKey(group int, shard int) string {
	return fmt.Sprintf("t-cust__parity/%08x/etag%d/p%d", group*2654435761, group, shard)
}

func TestPlacement_KeyGroupAndShard(t *testing.T) {
	cases := []struct {
		path  string
		group string
		shard int
	}{
		{"t-x__parity/ab12/etag/p3", "t-x__parity/ab12/etag", 3},
		{"t-x__parity/ab12/etag/p0", "t-x__parity/ab12/etag", 0},
		{"t-x/bkt/file.bin", "t-x/bkt", 0},
		{"t-x/bkt/sub/p", "t-x/bkt/sub", 0},
		{"t-x/bkt/sub/p12x", "t-x/bkt/sub", 0},
		{"t-x/bkt/sub/p-1", "t-x/bkt/sub", 0},
		{"t-x/bkt/sub/p007", "t-x/bkt/sub", 7},
	}
	for _, c := range cases {
		g, j := odKeyGroup(c.path)
		assert.Equal(t, c.group, g, c.path)
		assert.Equal(t, c.shard, j, c.path)
	}
}

func TestPlacement_IsDeterministicAndIndependentOfListingOrder(t *testing.T) {
	a := newPlacementV2(synthFleet(5, 3), 4)
	shuffled := synthFleet(5, 3)
	for i := range shuffled {
		dr := shuffled[i].drives
		dr[0], dr[2] = dr[2], dr[0]
	}
	shuffled[0], shuffled[4] = shuffled[4], shuffled[0]
	b := newPlacementV2(shuffled, 4)
	for g := 0; g < 500; g++ {
		for j := 0; j < 4; j++ {
			k := parityKey(g, j)
			assert.Equal(t, a.home(k).identity, b.home(k).identity, k)
		}
	}
}

func TestPlacement_FourShardsOfOneObjectSitOnFourTenants(t *testing.T) {
	p := newPlacementV2(synthFleet(5, 3), 4)
	for g := 0; g < 2000; g++ {
		seen := map[string]bool{}
		for j := 0; j < 4; j++ {
			seen[tenantOf(p, parityKey(g, j))] = true
		}
		require.Len(t, seen, 4, "group %d: four shards, four tenants", g)
	}

	// Fewer tenants than shards: at most ⌈4/n⌉ per tenant.
	for n, cap := range map[int]int{3: 2, 2: 2, 1: 4} {
		p := newPlacementV2(synthFleet(n, 2), 4)
		for g := 0; g < 500; g++ {
			per := map[string]int{}
			for j := 0; j < 4; j++ {
				per[tenantOf(p, parityKey(g, j))]++
			}
			for tk, c := range per {
				require.LessOrEqual(t, c, cap, "%d tenants, group %d, tenant %s", n, g, tk)
			}
		}
	}
}

func TestPlacement_AGroupsFilesOnOneTenantShareOneDrive(t *testing.T) {
	p := newPlacementV2(synthFleet(3, 4), 4)
	for g := 0; g < 500; g++ {
		driveOf := map[string]string{} // tenant → drive
		leaves := []string{"p0", "p1", "p2", "p3", "p4", "p5", "manifest", "x.bin"}
		for _, leaf := range leaves {
			k := fmt.Sprintf("t-cust__parity/%08x/etag/%s", g*7919, leaf)
			home := p.home(k)
			tk := tenantOf(p, k)
			if prev, ok := driveOf[tk]; ok {
				require.Equal(t, prev, home.identity, "group %d: %s landed on another drive of %s", g, leaf, tk)
			}
			driveOf[tk] = home.identity
		}
	}
}

// movedFraction is the share of (group, shard) placements that differ
// between two placements over the same 4,000 synthetic parity groups.
func movedFraction(t *testing.T, before, after *odPlacement, onlyTenant string) (moved float64) {
	t.Helper()
	var total, diff float64
	for g := 0; g < 4000; g++ {
		for j := 0; j < 4; j++ {
			k := parityKey(g, j)
			if onlyTenant != "" && tenantOf(before, k) != onlyTenant {
				continue
			}
			total++
			if before.home(k).identity != after.home(k).identity {
				diff++
			}
		}
	}
	require.Positive(t, total)
	return diff / total
}

func TestPlacement_AnAppendedTenantMovesAboutOneShardInN(t *testing.T) {
	before := newPlacementV2(synthFleet(4, 2), 4)
	after := newPlacementV2(synthFleet(5, 2), 4)
	moved := movedFraction(t, before, after, "")
	// One tenant in five takes about a fifth of the shards; the exclusion
	// (two shards may not share a tenant) cascades a little. Never the
	// three quarters of hash % slots, never the half of a shifted ranking.
	assert.InDelta(t, 0.20, moved, 0.06, "moved %.3f", moved)

	// Nothing changes when nothing changes.
	assert.Zero(t, movedFraction(t, before, newPlacementV2(synthFleet(4, 2), 4), ""))
}

func TestPlacement_AnAppendedDriveMovesOnlyInsideItsTenant(t *testing.T) {
	before := newPlacementV2(synthFleet(4, 2), 4)
	grown := synthFleet(4, 2)
	grown[2].drives = append(grown[2].drives, odPlaced{index: 99, identity: "t2/d2"})
	after := newPlacementV2(grown, 4)

	for _, other := range []string{"tenant-0", "tenant-1", "tenant-3"} {
		assert.Zero(t, movedFraction(t, before, after, other), "tenant %s", other)
	}
	moved := movedFraction(t, before, after, "tenant-2")
	assert.InDelta(t, 1.0/3, moved, 0.06, "a third of tenant-2's files take the new drive; moved %.3f", moved)
}

func TestPlacement_VersionOneIsHashModSlotsOverTheRecordedOrder(t *testing.T) {
	order := []odPlaced{{1, "a"}, {2, "b"}, {3, "c"}, {5, "e"}}
	p := newPlacementV1(order)
	for g := 0; g < 300; g++ {
		k := parityKey(g, g%4)
		h := fnv.New32a()
		_, _ = h.Write([]byte(k))
		want := order[int(h.Sum32()%uint32(len(order)))]
		assert.Equal(t, want.identity, p.home(k).identity, k)
	}
	// And it is what the driver did before this WP.
	acc := newAccounts("a", "b", "c")
	d := markerFleet(zap.NewNop(), acc...)
	v1 := newPlacementV1(d.placedOrder())
	for g := 0; g < 100; g++ {
		k := parityKey(g, 1)
		assert.Equal(t, d.tenants[d.homeTenantIndex(k)].identity(), v1.home(k).identity)
	}
}

func TestPlacement_TheSpreadSetIsOneDrivePerShard(t *testing.T) {
	p := newPlacementV2(synthFleet(5, 3), 4)
	for g := 0; g < 300; g++ {
		group := fmt.Sprintf("t-cust__parity/%08x/etag", g*104729)
		set := p.spreadSet(group)
		require.Len(t, set, 4)
		want := map[string]bool{}
		for j := 0; j < 4; j++ {
			want[p.home(group+"/p"+fmt.Sprint(j)).identity] = true
		}
		got := map[string]bool{}
		for _, dr := range set {
			got[dr.identity] = true
		}
		assert.Equal(t, want, got, group)
	}
	// With fewer tenants than the spread, the set is smaller and still
	// covers every shard's drive.
	p3 := newPlacementV2(synthFleet(3, 1), 4)
	set := p3.spreadSet("t-cust__parity/x/etag")
	require.Len(t, set, 3)
}

// ── the driver: markers, versions, the computed fallback ────────────────────

// tenantedFleet builds a fleet whose accounts share directories: every store
// of one group has the same tenant id.
func tenantedFleet(logger *zap.Logger, groups ...[]*markerStore) *OneDriveDriver {
	d := &OneDriveDriver{logger: logger}
	for gi, g := range groups {
		for _, a := range g {
			tn := stubTenant(a.name, a)
			tn.tenantID = fmt.Sprintf("tid-group-%d", gi)
			tn.userUPN = a.name + "@fleet.test"
			tn.rootFolderID = "ROOT"
			d.tenants = append(d.tenants, tn)
		}
	}
	return d
}

func totalGets(accounts ...*markerStore) (n int) {
	for _, a := range accounts {
		n += a.getCount()
	}
	return n
}

func storeOf(d *OneDriveDriver, accounts []*markerStore, identity string) *markerStore {
	for i, tn := range d.tenants {
		if tn.identity() == identity {
			return accounts[i]
		}
	}
	return nil
}

func TestFleetPlacement_AMarkerWithoutTheVersionIsVersionOneAndStaysAuthoritative(t *testing.T) {
	// Prod's three accounts: markers written before placement_version.
	acc := newAccounts("a", "b", "c")
	d := markerFleet(zap.NewNop(), acc...)
	_, err := d.CheckFleetFingerprint(context.Background(), "")
	require.NoError(t, err)
	for _, a := range acc {
		rewriteMarker(t, a, func(m *fleetMarker) { m.PlacementVersion = 0; m.Tenants = nil })
		var raw map[string]any
		require.NoError(t, json.Unmarshal([]byte(a.files[fleetMarkerPath]), &raw))
		require.NotContains(t, raw, "placement_version", "the fixture is a pre-field marker")
	}

	fresh := markerFleet(zap.NewNop(), acc...)
	fp, err := fresh.CheckFleetFingerprint(context.Background(), "")
	require.NoError(t, err)
	assert.Equal(t, FleetMatch, fp.State)
	assert.True(t, fresh.authoritative(), "version 1 everywhere = the data is where version 1 says")
	assert.Equal(t, 1, fresh.placement().version)
	for _, a := range acc {
		assert.Equal(t, 1, a.putCount(), "nothing rewritten")
	}

	// A miss costs one request, at the version-1 home.
	ctx := common.WithTenantID(context.Background(), "x")
	before := totalGets(acc...)
	ok, err := fresh.Exists(ctx, "bkt", "nothing")
	require.NoError(t, err)
	assert.False(t, ok)
	assert.Equal(t, 1, totalGets(acc...)-before)
	home := fresh.tenants[fresh.homeTenantIndex(tenantKey("x", "bkt", "nothing"))]
	assert.Equal(t, 1, storeOf(fresh, acc, home.identity()).getCount()-0, "asked at hash %% slots")
}

func TestFleetPlacement_AFirstBootRecordsVersionTwoAndTheTenants(t *testing.T) {
	g0, g1 := newAccounts("a", "b"), newAccounts("c", "d")
	d := tenantedFleet(zap.NewNop(), g0, g1)
	fp, err := d.CheckFleetFingerprint(context.Background(), "")
	require.NoError(t, err)
	require.Equal(t, FleetRecorded, fp.State)
	require.True(t, d.authoritative())
	assert.Equal(t, odPlacementCurrent, d.placement().version)

	m := g0[0].marker(t)
	assert.Equal(t, odPlacementCurrent, m.PlacementVersion)
	require.Len(t, m.Tenants, 2, "the grouping is recorded")
	assert.ElementsMatch(t, []int{0, 1}, m.Tenants[0].Drives)
	assert.ElementsMatch(t, []int{2, 3}, m.Tenants[1].Drives)
	assert.Zero(t, m.PlacementTarget)

	// A Put lands on the version-2 home.
	ctx := common.WithTenantID(context.Background(), "cust")
	require.NoError(t, d.Put(ctx, "parity", "ab/etag/p1", strings.NewReader("shard")))
	home := d.placement().home(tenantKey("cust", "parity", "ab/etag/p1"))
	st := storeOf(d, append(g0, g1...), home.identity)
	require.NotNil(t, st)
	st.mu.Lock()
	_, there := st.files["t-cust/parity/ab/etag/p1"]
	st.mu.Unlock()
	assert.True(t, there, "written where version 2 places it")
}

func TestFleetPlacement_AnUnplacedMissAsksTheOldHomeAndNoMore(t *testing.T) {
	// 39 drives in 5 tenants, placed; then a 40th appended and accepted.
	names := make([]string, 40)
	for i := range names {
		names[i] = fmt.Sprintf("d%02d", i)
	}
	all := newAccounts(names...)
	groups := func(n int) [][]*markerStore {
		var gs [][]*markerStore
		for i := 0; i < 5; i++ {
			var g []*markerStore
			for k := i; k < n; k += 5 {
				g = append(g, all[k])
			}
			gs = append(gs, g)
		}
		return gs
	}
	old := tenantedFleet(zap.NewNop(), groups(39)...)
	_, err := old.CheckFleetFingerprint(context.Background(), "")
	require.NoError(t, err)
	require.True(t, old.authoritative())
	ctx := common.WithTenantID(context.Background(), "cust")
	require.NoError(t, old.Put(ctx, "parity", "ab/etag/p2", strings.NewReader("shard-2")))

	grown := tenantedFleet(zap.NewNop(), groups(40)...)
	mm, err := grown.CheckFleetFingerprint(context.Background(), "")
	require.NoError(t, err)
	require.Equal(t, FleetMismatch, mm.State)
	acc, err := grown.CheckFleetFingerprint(context.Background(), mm.Digest)
	require.NoError(t, err)
	require.Equal(t, FleetAccepted, acc.State)
	require.False(t, grown.authoritative())
	require.NotNil(t, grown.previousPlacement(), "the markers say where the data was laid out")
	assert.Equal(t, 39, len(grown.previousPlacement().order))

	// A miss: the new home, then the old home — two requests at 40 drives.
	before := totalGets(all...)
	ok, err := grown.Exists(ctx, "parity", "nothing/here/p0")
	require.NoError(t, err)
	assert.False(t, ok)
	assert.Equal(t, 2, totalGets(all...)-before, "never a walk")

	// A hit placed under the old order is found at the old home.
	before = totalGets(all...)
	ok, err = grown.Exists(ctx, "parity", "ab/etag/p2")
	require.NoError(t, err)
	assert.True(t, ok)
	assert.LessOrEqual(t, totalGets(all...)-before, 2)
	rc, err := grown.Get(ctx, "parity", "ab/etag/p2")
	require.NoError(t, err)
	body, err := io.ReadAll(rc)
	require.NoError(t, err)
	assert.Equal(t, "shard-2", string(body))
}

func TestFleetPlacement_ARebalanceTargetWritesNewAndReadsBoth(t *testing.T) {
	// A version-1 fleet mid-rebalance: the tool set placement_target 2.
	acc := newAccounts("a", "b", "c")
	d := markerFleet(zap.NewNop(), acc...)
	_, err := d.CheckFleetFingerprint(context.Background(), "")
	require.NoError(t, err)
	ctx := common.WithTenantID(context.Background(), "cust")
	for _, a := range acc {
		rewriteMarker(t, a, func(m *fleetMarker) { m.PlacementVersion = 0; m.Tenants = nil })
	}
	v1 := markerFleet(zap.NewNop(), acc...)
	_, err = v1.CheckFleetFingerprint(context.Background(), "")
	require.NoError(t, err)
	require.Equal(t, 1, v1.placement().version)
	require.NoError(t, v1.Put(ctx, "parity", "old/etag/p0", strings.NewReader("old")))

	for _, a := range acc {
		rewriteMarker(t, a, func(m *fleetMarker) { m.PlacementTarget = odPlacementCurrent })
	}
	mid := markerFleet(zap.NewNop(), acc...)
	fp, err := mid.CheckFleetFingerprint(context.Background(), "")
	require.NoError(t, err)
	assert.Equal(t, FleetMatch, fp.State)
	assert.False(t, mid.authoritative(), "a rebalance is running")
	assert.Equal(t, odPlacementCurrent, mid.placement().version, "writes go where the target places them")
	require.NotNil(t, mid.previousPlacement())
	assert.Equal(t, 1, mid.previousPlacement().version)

	require.NoError(t, mid.Put(ctx, "parity", "new/etag/p0", strings.NewReader("new")))
	newHome := mid.placement().home(tenantKey("cust", "parity", "new/etag/p0"))
	st := storeOf(mid, acc, newHome.identity)
	st.mu.Lock()
	_, there := st.files["t-cust/parity/new/etag/p0"]
	st.mu.Unlock()
	assert.True(t, there)

	ok, err := mid.Exists(ctx, "parity", "old/etag/p0")
	require.NoError(t, err)
	assert.True(t, ok, "the old object is read at its version-1 home")

	// -commit: placement_version 2, no target → authoritative.
	for _, a := range acc {
		rewriteMarker(t, a, func(m *fleetMarker) { m.PlacementVersion = odPlacementCurrent; m.PlacementTarget = 0 })
	}
	done := markerFleet(zap.NewNop(), acc...)
	_, err = done.CheckFleetFingerprint(context.Background(), "")
	require.NoError(t, err)
	assert.True(t, done.authoritative())
	assert.Nil(t, done.previousPlacement())
}

// ── ListDir over the spread set ─────────────────────────────────────────────

// listingStore answers children listings with what it holds under a folder.
type listingStore struct {
	*markerStore
	listings int
}

func (l *listingStore) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Method == http.MethodGet && strings.Contains(req.URL.Path, ":/children") {
		l.mu.Lock()
		l.listings++
		folder := req.URL.Path[strings.Index(req.URL.Path, "/items/root:/")+len("/items/root:/"):]
		folder = strings.TrimSuffix(folder, ":/children")
		folder = strings.TrimPrefix(folder, odRootFolder+"/")
		var items []string
		seen := map[string]bool{}
		for p := range l.files {
			if !strings.HasPrefix(p, folder+"/") {
				continue
			}
			rest := strings.TrimPrefix(p, folder+"/")
			name, _, isDir := strings.Cut(rest, "/")
			if seen[name] {
				continue
			}
			seen[name] = true
			if isDir {
				items = append(items, fmt.Sprintf(`{"name":%q,"folder":{}}`, name))
			} else {
				items = append(items, fmt.Sprintf(`{"name":%q,"size":1}`, name))
			}
		}
		l.mu.Unlock()
		sort.Strings(items)
		return odResp(req, 200, `{"value":[`+strings.Join(items, ",")+`]}`, nil), nil
	}
	return l.markerStore.RoundTrip(req)
}

func TestFleetPlacement_ListDirOfAGroupFolderAsksTheSpreadSet(t *testing.T) {
	// 12 drives in 4 tenants, version 2.
	stores := make([]*listingStore, 12)
	groups := make([][]*markerStore, 4)
	for i := range stores {
		stores[i] = &listingStore{markerStore: &markerStore{name: fmt.Sprintf("d%02d", i), files: map[string]string{}}}
		groups[i%4] = append(groups[i%4], stores[i].markerStore)
	}
	d := &OneDriveDriver{logger: zap.NewNop()}
	for gi, g := range groups {
		for k, a := range g {
			tn := stubTenant(a.name, stores[gi+4*k])
			tn.tenantID = fmt.Sprintf("tid-group-%d", gi)
			tn.userUPN = a.name + "@fleet.test"
			tn.rootFolderID = "ROOT"
			d.tenants = append(d.tenants, tn)
		}
	}
	_, err := d.CheckFleetFingerprint(context.Background(), "")
	require.NoError(t, err)
	require.True(t, d.authoritative())
	ctx := common.WithTenantID(context.Background(), "cust")
	for j := 0; j < 4; j++ {
		require.NoError(t, d.Put(ctx, "parity", fmt.Sprintf("ab/etag/p%d", j), strings.NewReader("s")))
	}
	reset := func() {
		for _, s := range stores {
			s.mu.Lock()
			s.listings = 0
			s.mu.Unlock()
		}
	}
	count := func() (n int) {
		for _, s := range stores {
			s.mu.Lock()
			n += s.listings
			s.mu.Unlock()
		}
		return n
	}

	// The group folder: four listings, every shard seen.
	reset()
	dirs, files, err := d.ListDir(ctx, "parity", "ab/etag")
	require.NoError(t, err)
	assert.Empty(t, dirs)
	assert.ElementsMatch(t, []string{"p0", "p1", "p2", "p3"}, files)
	assert.Equal(t, 4, count(), "the spread set, not the fleet")

	// Above it, the union still (sub-folders may be anywhere).
	reset()
	dirs, _, err = d.ListDir(ctx, "parity", "ab")
	require.NoError(t, err)
	assert.Equal(t, []string{"etag"}, dirs)
	assert.Equal(t, 12, count())

	// RemoveEmptyDir of the group folder: refused while a shard is there,
	// and it asked the spread set.
	reset()
	err = d.RemoveEmptyDir(ctx, "parity", "ab/etag")
	require.ErrorIs(t, err, ErrDirNotEmpty)
	assert.Equal(t, 4, count())
}
