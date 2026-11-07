package drivers

// WP-FLEET-5 placement over (tenant, drive) — docs/reviews/WP-FLEET-5-PROMPT.md.
// PR 0: the shapes the red tests name. PR A fills them.

// odPlacementCurrent is the placement a fleet with no history records.
const odPlacementCurrent = 2

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
	return &odPlacement{version: 2, spread: spread, tenants: tenants}
}

// odKeyGroup is the key's group (its parent folder) and shard index (a leaf
// `p<j>`, else 0).
func odKeyGroup(_ string) (string, int) { return "", 0 }

// home is the drive the key lives on.
func (p *odPlacement) home(_ string) odPlaced { return odPlaced{} }

// spreadSet is the drives a group folder's files can be on.
func (p *odPlacement) spreadSet(_ string) []odPlaced { return nil }

// placedOrder is the fleet as version 1 places over it.
func (d *OneDriveDriver) placedOrder() []odPlaced {
	out := make([]odPlaced, len(d.tenants))
	for i, t := range d.tenants {
		out[i] = odPlaced{index: i, identity: t.identity()}
	}
	return out
}

// placement is where writes go and reads look first.
func (d *OneDriveDriver) placement() *odPlacement { return newPlacementV1(d.placedOrder()) }

// previousPlacement is where the data was laid out while it differs from
// placement(); nil when the fleet is placed.
func (d *OneDriveDriver) previousPlacement() *odPlacement { return nil }
