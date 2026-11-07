package drivers

// WP-FLEET-5: placement over (tenant, drive) — docs/reviews/WP-FLEET-5-PROMPT.md.
//
// Measured: placement was hash(full path) % slots (version 1), so the fourth
// drive remaps three quarters of every home; one object's four parity shards
// landed wherever their names hashed — two on one tenant on 2026-11-06's
// soak4, where the 400 GB/h per-app-per-tenant budget ("no license bound",
// Microsoft Learn 2026-08-10) bit first; and no drive could be named for a
// folder, so ListDir was the fleet's union (D-F11-4).
//
//   - D-F5-1 placement from the key alone, in two steps. A key's GROUP is its
//     parent folder; its SHARD index j is the integer of a leaf `p<j>` (the
//     parity layout <tenant>__parity/<digest>/<etag>/p<j>), else 0. The
//     tenants stand in COLUMNS (tenant k of the fleet's order of first
//     appearance in column k mod spread); shard j takes a tenant of column
//     j mod spread by rendezvous on (tenant key, group), so shards 0..3 sit
//     on four tenants whenever four exist (an empty column — fewer tenants
//     than the spread — falls back to every tenant under a cap of
//     ⌈max(spread, j+1)/n⌉ shards each). The drive inside the tenant is
//     rendezvous on (drive identity, group): every file of one group that
//     lands on a tenant lands on ONE drive of it. Adding a tenant fills one
//     column and moves about one shard in (its size + 1) of that column's
//     (an eighth of all shards on a 4 → 5 step); adding a drive moves about
//     one file in d of ITS tenant and nothing elsewhere; nothing moves
//     otherwise. A ranking shared across shards, or an exclusion of earlier
//     shards' tenants, moved half of every shard on 4 → 5 (the red test).
//   - D-F5-2 the marker records placement_version (absent = 1), the tenant
//     grouping as written, and placement_target while a rebalance runs.
//   - D-F5-3 the fallback is the previous placement, COMPUTED from the
//     markers — two requests at any fleet size, never a walk.
//   - D-F5-4 ListDir of a group folder (odGroupDepth segments under a PARITY
//     container, whose every file is a shard) asks the group's spread set;
//     every other folder and List stay the union.
//
// The fleet's version is what its markers say: prod's three accounts, whose
// markers predate the field, stay version 1 and authoritative with nothing
// rewritten; a first boot records odPlacementCurrent; the rebalance tool
// (cmd/tools/permafrost-rebalance) sets placement_target while it moves the
// data and placement_version last.

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"hash/fnv"
	"os"
	"path"
	"sort"
	"strconv"
	"strings"

	"github.com/prometheus/client_golang/prometheus"
	"go.uber.org/zap"
)

const (
	// odPlacementCurrent is the placement a fleet with no history records.
	odPlacementCurrent = 2
	// odPlacementSpreadDefault is PERMAFROST_PLACEMENT_SPREAD's default: RS
	// 4+4's four parity shards.
	odPlacementSpreadDefault = 4
	// odGroupDepth is how many folders under the container a group folder
	// sits at: the parity layout's <digest>/<etag>.
	odGroupDepth = 2
)

var permafrostPlacementVersion = prometheus.NewGauge(prometheus.GaugeOpts{
	Name: "vaultaire_permafrost_placement_version",
	Help: "The placement the permafrost fleet's reads are authoritative under (1 = hash % slots, 2 = rendezvous over tenants then drives, WP-FLEET-5); 0 while the data is not placed under the current fleet (an accepted order change, or a rebalance running) and reads look at the previous placement too.",
})

// odPlaced is one drive (account) as placement sees it: its position in the
// fleet and the marker's identity hash.
type odPlaced struct {
	index    int
	identity string
}

// odPlacedTenant is one directory and the drives in it.
type odPlacedTenant struct {
	key    string
	drives []odPlaced
}

// odPlacement computes an object's home from its key alone.
type odPlacement struct {
	version int
	spread  int
	order   []odPlaced       // version 1: the fleet in order (hash % slots)
	tenants []odPlacedTenant // version 2: rendezvous over tenants, then drives
}

func newPlacementV1(order []odPlaced) *odPlacement {
	return &odPlacement{version: 1, order: order}
}

func newPlacementV2(tenants []odPlacedTenant, spread int) *odPlacement {
	if spread <= 0 {
		spread = odPlacementSpreadDefault
	}
	p := &odPlacement{version: 2, spread: spread, tenants: tenants}
	for _, t := range tenants {
		p.order = append(p.order, t.drives...)
	}
	sort.Slice(p.order, func(a, b int) bool { return p.order[a].index < p.order[b].index })
	return p
}

// odKeyGroup is the key's group (its parent folder) and shard index (a leaf
// `p<j>`, digits only, else 0).
func odKeyGroup(key string) (string, int) {
	g, j, _ := odKeyShard(key)
	return g, j
}

// odKeyShard is odKeyGroup with whether the leaf IS a shard (`p<j>`): a shard
// places by its group, any other file by its full path.
func odKeyShard(key string) (group string, j int, shard bool) {
	group, leaf := path.Split(key)
	group = strings.TrimSuffix(group, "/")
	if len(leaf) < 2 || leaf[0] != 'p' {
		return group, 0, false
	}
	for _, r := range leaf[1:] {
		if r < '0' || r > '9' {
			return group, 0, false
		}
	}
	j, err := strconv.Atoi(leaf[1:])
	if err != nil || j < 0 {
		return group, 0, false
	}
	return group, j, true
}

// odRendezvous is the rendezvous weight of parts (FNV-1a 64 over the parts
// joined with NUL): the highest weight wins.
func odRendezvous(parts ...string) uint64 {
	h := fnv.New64a()
	for i, p := range parts {
		if i > 0 {
			_, _ = h.Write([]byte{0})
		}
		_, _ = h.Write([]byte(p))
	}
	return h.Sum64()
}

// home is the drive the key lives on (the zero odPlaced on an empty fleet).
func (p *odPlacement) home(key string) odPlaced {
	switch p.version {
	case 1:
		if len(p.order) == 0 {
			return odPlaced{}
		}
		h := fnv.New32a()
		_, _ = h.Write([]byte(key))
		return p.order[int(h.Sum32()%uint32(len(p.order)))]
	default:
		group, j, shard := odKeyShard(key)
		if !shard {
			// Not a shard: the file itself is the unit — rendezvous over
			// every tenant, then every drive of it, on the FULL path, so a
			// folder of plain files spreads like version 1 did (prod's dry
			// run 2026-11-07: by folder, every bench file of three accounts
			// went to one drive of one tenant).
			t := p.tenantByPath(key)
			if t == nil {
				return odPlaced{}
			}
			return t.driveFor(key)
		}
		t := p.tenantFor(group, j)
		if t == nil {
			return odPlaced{}
		}
		return t.driveFor(group)
	}
}

// tenantByPath is the tenant a plain file lands on: rendezvous over every
// tenant on the path.
func (p *odPlacement) tenantByPath(key string) *odPlacedTenant {
	pick := -1
	var pickW uint64
	for k := range p.tenants {
		w := odRendezvous(p.tenants[k].key, key)
		if pick < 0 || w > pickW || (w == pickW && p.tenants[k].key < p.tenants[pick].key) {
			pick, pickW = k, w
		}
	}
	if pick < 0 {
		return nil
	}
	return &p.tenants[pick]
}

// tenantFor is the tenant shard j of group lands on.
//
// The tenants stand in COLUMNS: tenant k (its position in the fleet's order
// of first appearance — append-only, so a tenant keeps its column for life)
// is in column k mod spread, and shard j takes a tenant of column j mod
// spread by rendezvous on (tenant key, group). Shards 0..spread−1 are then on
// distinct tenants whenever every column has one, and a tenant appended to
// the fleet joins ONE column: only that column's shards can move, about one
// in (its size + 1) of them — a ranking shared across shards, or an
// exclusion of the tenants earlier shards took, moved half of every shard on
// a 4 → 5 step (measured in the red test), because with as many tenants as
// shards the assignment is forced and every change cascades. The price is
// balance: a column with fewer tenants than the others gives each of its
// tenants a larger share (5 tenants at spread 4: three hold a quarter, two
// an eighth) — keep the tenant count a multiple of the spread
// (docs/FLEET.md § Sizing a tenant).
//
// An empty column (fewer tenants than the spread) falls back to every
// tenant, each of them at most ⌈max(spread, j+1)/n⌉ shards of the group, by
// rendezvous on (tenant key, group, shard).
func (p *odPlacement) tenantFor(group string, j int) *odPlacedTenant {
	n := len(p.tenants)
	if n == 0 {
		return nil
	}
	spread := max(p.spread, 1)
	// Column members, in fleet order.
	column := func(c int) (members []int) {
		for k := range p.tenants {
			if k%spread == c {
				members = append(members, k)
			}
		}
		return members
	}
	best := func(candidates []int, salt string) int {
		pick, pickW := -1, uint64(0)
		for _, k := range candidates {
			w := odRendezvous(p.tenants[k].key, group, salt)
			if pick < 0 || w > pickW || (w == pickW && p.tenants[k].key < p.tenants[pick].key) {
				pick, pickW = k, w
			}
		}
		return pick
	}
	// inColumn is shard s's tenant when its column has members: shards c,
	// c+spread, c+2·spread … of one group share the column (RS 4+5 and
	// beyond), each later one avoiding the tenants the earlier ones took
	// while the column has a free one.
	inColumn := func(s int, members []int) int {
		taken := map[int]bool{}
		pick := members[0]
		for q := 0; q*spread+s%spread <= s; q++ {
			var free []int
			for _, k := range members {
				if !taken[k] {
					free = append(free, k)
				}
			}
			if len(free) == 0 {
				free = members
			}
			pick = best(free, strconv.Itoa(q))
			taken[pick] = true
		}
		return pick
	}
	members := column(j % spread)
	if len(members) > 0 {
		return &p.tenants[inColumn(j, members)]
	}
	// Shard j's column is empty (fewer tenants than the spread): every
	// tenant is a candidate, each holding at most cap shards of the group —
	// counting the shards the columns placed.
	cap := (max(spread, j+1) + n - 1) / n
	counts := make([]int, n)
	all := make([]int, n)
	for k := range all {
		all[k] = k
	}
	pick := 0
	for s := 0; s <= j; s++ {
		if m := column(s % spread); len(m) > 0 {
			pick = inColumn(s, m)
		} else {
			var free []int
			for _, k := range all {
				if counts[k] < cap {
					free = append(free, k)
				}
			}
			if len(free) == 0 {
				free = all
			}
			pick = best(free, strconv.Itoa(s))
		}
		counts[pick]++
	}
	return &p.tenants[pick]
}

// driveFor is the tenant's drive for a group: rendezvous over its drives.
func (t *odPlacedTenant) driveFor(group string) odPlaced {
	var best odPlaced
	var bestW uint64
	for i, d := range t.drives {
		w := odRendezvous(d.identity, group)
		if i == 0 || w > bestW || (w == bestW && d.identity < best.identity) {
			best, bestW = d, w
		}
	}
	return best
}

// spreadSet is the drives a group folder's files can be on: shards
// 0..spread−1's homes, each once, in shard order.
func (p *odPlacement) spreadSet(group string) []odPlaced {
	if p.version == 1 {
		return p.order
	}
	seen := map[string]bool{}
	var out []odPlaced
	for j := 0; j < max(p.spread, 1); j++ {
		d := p.home(group + "/p" + strconv.Itoa(j))
		if d.identity == "" || seen[d.identity] {
			continue
		}
		seen[d.identity] = true
		out = append(out, d)
	}
	return out
}

// ── the driver's side ───────────────────────────────────────────────────────

// odTenantKey is a directory's key in the marker and in placement (a hash,
// never the id).
func odTenantKey(tenantID string) string {
	sum := sha256.Sum256([]byte("tenant\x00" + tenantID))
	return hex.EncodeToString(sum[:8])
}

// placedOrder is the fleet as version 1 places over it.
func (d *OneDriveDriver) placedOrder() []odPlaced {
	out := make([]odPlaced, len(d.tenants))
	for i, t := range d.tenants {
		out[i] = odPlaced{index: i, identity: t.identity()}
	}
	return out
}

// placedTenants is the fleet grouped by directory, in order of first
// appearance, each drive at its fleet position.
func (d *OneDriveDriver) placedTenants() []odPlacedTenant {
	var out []odPlacedTenant
	at := map[string]int{}
	for i, t := range d.tenants {
		key := odTenantKey(t.tenantID)
		k, ok := at[key]
		if !ok {
			k = len(out)
			at[key] = k
			out = append(out, odPlacedTenant{key: key})
		}
		out[k].drives = append(out[k].drives, odPlaced{index: i, identity: t.identity()})
	}
	return out
}

// markerTenants is placedTenants as the marker records it.
func (d *OneDriveDriver) markerTenants() []fleetMarkerTenant {
	pts := d.placedTenants()
	out := make([]fleetMarkerTenant, len(pts))
	for i, t := range pts {
		out[i].Key = t.key
		for _, dr := range t.drives {
			out[i].Drives = append(out[i].Drives, dr.index)
		}
	}
	return out
}

// placementOver builds the placement of version over the current fleet.
func (d *OneDriveDriver) placementOver(version int) *odPlacement {
	if version <= 1 {
		return newPlacementV1(d.placedOrder())
	}
	return newPlacementV2(d.placedTenants(), d.placementSpread)
}

// placementFromMarker is the placement a marker's data is laid out under:
// its version over the fleet and grouping it recorded. A version-2 marker
// without a grouping (never written by this code) groups each drive alone.
func (d *OneDriveDriver) placementFromMarker(m *fleetMarker) *odPlacement {
	order := make([]odPlaced, len(m.Fleet))
	for i, id := range m.Fleet {
		order[i] = odPlaced{index: i, identity: id}
	}
	if m.PlacementVersion <= 1 {
		return newPlacementV1(order)
	}
	var tenants []odPlacedTenant
	for _, mt := range m.Tenants {
		t := odPlacedTenant{key: mt.Key}
		for _, k := range mt.Drives {
			if k >= 0 && k < len(order) {
				t.drives = append(t.drives, order[k])
			}
		}
		if len(t.drives) > 0 {
			tenants = append(tenants, t)
		}
	}
	if len(tenants) == 0 {
		for _, dr := range order {
			tenants = append(tenants, odPlacedTenant{key: dr.identity, drives: []odPlaced{dr}})
		}
	}
	p := newPlacementV2(tenants, d.placementSpread)
	p.order = order
	return p
}

// placement is where writes go and reads look first: the fleet's recorded
// (or targeted) version over the current fleet; version 1 before any check
// (what the driver always did — a boot with the markers unreadable must not
// write where no marker says).
func (d *OneDriveDriver) placement() *odPlacement {
	d.fleetMu.Lock()
	p := d.fleetPlacement
	d.fleetMu.Unlock()
	if p != nil {
		return p
	}
	return d.placementOver(1)
}

// previousPlacement is where the data was laid out while it differs from
// placement(): nil when the fleet is placed. With no verdict yet (or an
// unknown one) it is the other version over the current fleet, so a miss
// still looks in both places a marker could name.
func (d *OneDriveDriver) previousPlacement() *odPlacement {
	if d.authoritative() {
		return nil
	}
	d.fleetMu.Lock()
	prev, cur, state := d.fleetPrevious, d.fleetPlacement, d.fleetState
	d.fleetMu.Unlock()
	if prev != nil {
		return prev
	}
	if state == FleetMismatch || state == FleetAccepted {
		return nil // the markers named no other layout (they agree with the data)
	}
	other := odPlacementCurrent
	if cur != nil && cur.version == odPlacementCurrent {
		other = 1
	}
	return d.placementOver(other)
}

// tenantByIdentity is the account of an identity, nil when it is not in the
// fleet (a drive the markers remember that is gone).
func (d *OneDriveDriver) tenantByIdentity(identity string) *odTenant {
	if identity == "" {
		return nil
	}
	d.identityMu.Lock()
	if d.identityIndex == nil || len(d.identityIndex) != len(d.tenants) {
		d.identityIndex = make(map[string]*odTenant, len(d.tenants))
		for _, t := range d.tenants {
			d.identityIndex[t.identity()] = t
		}
	}
	t := d.identityIndex[identity]
	d.identityMu.Unlock()
	return t
}

// homeTenant is the account placement names for path (the first of the fleet
// when placement names none — an empty marker fleet).
func (d *OneDriveDriver) homeTenant(path string) *odTenant {
	if t := d.tenantByIdentity(d.placement().home(path).identity); t != nil {
		return t
	}
	return d.tenants[0]
}

// spreadTenants is the accounts a group folder's files can be on under the
// current placement (D-F5-4): the group's spread set, de-duplicated and in
// fleet order; the whole fleet under version 1.
func (d *OneDriveDriver) spreadTenants(group string) []*odTenant {
	set := d.placement().spreadSet(group)
	if len(set) == 0 {
		return d.tenants
	}
	want := make(map[string]bool, len(set))
	for _, dr := range set {
		want[dr.identity] = true
	}
	var out []*odTenant
	for _, t := range d.tenants {
		if want[t.identity()] {
			out = append(out, t)
		}
	}
	if len(out) == 0 {
		return d.tenants
	}
	return out
}

// odParityContainerSuffix names the containers whose every file is a shard:
// the Vault parity layout's `<tenant>__parity` (internal/api/vault_parity.go)
// — the one layout a folder's drives can be named for.
const odParityContainerSuffix = "__parity"

// odGroupFolder reports whether a folder (dir, "/" separated, no leading
// slash) of the container is a group folder — a parity container's
// `<digest>/<etag>`, whose files are shards and sit on the group's spread
// set. Any other folder may hold plain files, placed by their full path, so
// it is listed on every account.
func odGroupFolder(container, dir string) bool {
	dir = strings.Trim(dir, "/")
	return strings.HasSuffix(container, odParityContainerSuffix) && dir != "" && strings.Count(dir, "/")+1 >= odGroupDepth
}

// odPlacementSpreadFromEnv reads PERMAFROST_PLACEMENT_SPREAD (default 4; a
// rejected value is logged at Warn and the default kept).
func odPlacementSpreadFromEnv(getenv func(string) string, logger *zap.Logger) int {
	v := strings.TrimSpace(getenv("PERMAFROST_PLACEMENT_SPREAD"))
	if v == "" {
		return odPlacementSpreadDefault
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 || n > 64 {
		logger.Warn("invalid PERMAFROST_PLACEMENT_SPREAD (need 1..64), keeping default",
			zap.String("value", v), zap.Int("default", odPlacementSpreadDefault))
		return odPlacementSpreadDefault
	}
	return n
}

// placementVerdict is what the markers say about placement: the version the
// data is laid out under, a rebalance target, and the marker to compute the
// old placement from.
type placementVerdict struct {
	laid    int          // the data's version (markers agree; 0 = none present)
	target  int          // placement_target, 0 = none
	agree   bool         // every present marker names the same version and target
	ref     *fleetMarker // an old-layout marker (placed_under ≠ digest), else any present
	present bool
}

func placementOfMarkers(markers []*fleetMarker, digest string) placementVerdict {
	v := placementVerdict{agree: true}
	for _, m := range markers {
		if m == nil {
			continue
		}
		ver := max(m.PlacementVersion, 1)
		if !v.present {
			v.laid, v.target, v.present = ver, m.PlacementTarget, true
		} else if ver != v.laid || m.PlacementTarget != v.target {
			v.agree = false
			v.laid = max(v.laid, ver)
			v.target = max(v.target, m.PlacementTarget)
		}
		if v.ref == nil || (m.placedUnder() != digest && v.ref.placedUnder() == digest) {
			v.ref = m
		}
	}
	return v
}

// String names the verdict for a log line.
func (v placementVerdict) String() string {
	return fmt.Sprintf("laid=%d target=%d agree=%t", v.laid, v.target, v.agree)
}

func init() {
	// The gauge is 0 until a check says otherwise.
	permafrostPlacementVersion.Set(0)
}

// placementSpreadDefaultEnv is the spread the production constructor reads.
func placementSpreadDefaultEnv(logger *zap.Logger) int {
	return odPlacementSpreadFromEnv(os.Getenv, logger)
}
