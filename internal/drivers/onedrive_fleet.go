// internal/drivers/onedrive_fleet.go
//
// The fleet fingerprint (WP-R7-4, R7-09). An object's home account is
// hash(path) % fleet size over the order the fleet is built in — TENANT_1..15,
// gaps skipped — so removing TENANT_2, reordering the slots or appending one
// moves the home of most objects. Reads survive (the fallback walk finds an
// object on any account) but every remapped read pays a walk of the fleet, and
// nothing recorded the order to notice.
//
// Every account now carries a marker, vaultaire/_fleet/fingerprint.json: its
// index and the ordered fleet (one short hash per account), written at first
// boot and compared on every boot. The marker lives with the data it describes
// — a database restore cannot lose it, a fleet moved to another box keeps it.
// A mismatch is one Error line naming the diff, and
// vaultaire_permafrost_fleet_mismatch 1 (rule PermafrostFleetMismatch). Boot is
// never blocked. Accepting a new order: PERMAFROST_FLEET_ACCEPT=<the
// fingerprint the Error line names> rewrites the markers at the next boot; a
// value left set does nothing once they match (onedrive_README.md § Fleet
// fingerprint).
//
// The order and the DATA are two facts (RB3-1, the review of WP-FLEET-11): a
// marker also records `placed_under`, the digest of the order the account's
// objects are laid out for. A first boot writes the current digest; an accept
// carries each old marker's value forward (a brand-new account gets the
// current digest — it holds nothing); a marker written before the field reads
// as placed under its own digest. Get / Exists / Delete ask the home account
// only (WP-FLEET-11 D-F11-3) while EVERY marker is placed under the current
// order; after an accept they walk until a rebalance rewrites the field —
// the hourly re-check and the next deploy find `match` and change nothing.
package drivers

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"go.uber.org/zap"
)

// fleetMarkerPath is the marker under the fleet's root folder (outside every
// t-<tenant>/ tree).
const fleetMarkerPath = "_fleet/fingerprint.json"

// permafrostDriveMismatch is the per-account drive verdict (WP-FLEET-1,
// D-F1-2): 1 when the fleet's tree is not on the drive the account uses —
// the marker there records another drive_id, or the chosen drive holds no
// marker while another drive of the same user does.
var permafrostDriveMismatch = prometheus.NewGaugeVec(prometheus.GaugeOpts{
	Name: "vaultaire_permafrost_drive_mismatch",
	Help: "1 when a permafrost account's fleet marker names another drive than the one the account uses (TENANT_N_DRIVE_ID, else the user's drive named OneDrive), or sits on another drive of the same user: every object of that account is off its drive. Never switched automatically (WP-FLEET-1; onedrive_README.md § Drive selection). 0 otherwise.",
}, []string{"account"})

// InitPermafrostAccountSeries creates an account's per-account permafrost
// series at 0 (the driver calls it for every account it keeps), so the
// first 1 is a change a rule can see.
func InitPermafrostAccountSeries(account string) {
	permafrostDriveMismatch.WithLabelValues(account).Set(0)
	// The quota's state series at 0 = no probe has read it yet; the byte
	// series appear with the first probe (a 0 total would be a lie).
	for _, st := range odQuotaStates {
		permafrostQuotaState.WithLabelValues(account, st)
	}
}

var permafrostFleetMismatch = prometheus.NewGauge(prometheus.GaugeOpts{
	Name: "vaultaire_permafrost_fleet_mismatch",
	Help: "1 when the permafrost fleet's order (TENANT_1..15, gaps skipped) differs from the order its accounts' markers recorded (read at boot): placement is hash % fleet size over that order, so remapped objects are read through the fleet walk. 0 otherwise, also when the markers could not be read (WP-R7-4, R7-09).",
})

// fleetMarker is one account's marker. DriveID (WP-FLEET-1, additive) is the
// drive the account's tree lives on; a marker written before reads as
// before. MovedTo is set on the OLD drive's marker by the migration tool's
// -switch (cmd/tools/permafrost-migrate-drive): the tree's new drive.
type fleetMarker struct {
	Version   int       `json:"version"`
	Index     int       `json:"index"`
	Fleet     []string  `json:"fleet"`
	Digest    string    `json:"digest"`
	WrittenAt time.Time `json:"written_at"`
	DriveID   string    `json:"drive_id,omitempty"`
	MovedTo   string    `json:"moved_to,omitempty"`
	// PlacedUnder is the digest of the fleet order the account's data is laid
	// out for (RB3-1; additive): "" = Digest (a marker written before).
	PlacedUnder string `json:"placed_under,omitempty"`
	// PlacementVersion is the placement the account's data is laid out
	// under (WP-FLEET-5; additive): 0 = 1 = hash % slots over Fleet; 2 =
	// rendezvous over Tenants then drives. PlacementTarget is set by the
	// rebalance tool while it runs (writes go there, reads look there first);
	// Tenants is the fleet's grouping as written, so the old placement can
	// be computed exactly.
	PlacementVersion int                 `json:"placement_version,omitempty"`
	PlacementTarget  int                 `json:"placement_target,omitempty"`
	Tenants          []fleetMarkerTenant `json:"tenants,omitempty"`

	foundOn string // the drive the marker was read on, when not the account's (never encoded)
}

// fleetMarkerTenant is one directory in the marker: its key (a hash, never
// the id) and the fleet positions of its drives.
type fleetMarkerTenant struct {
	Key    string `json:"key"`
	Drives []int  `json:"drives"`
}

// FleetMarkerPath is the marker's path under vaultaire/ (the migration tool
// reads and rewrites it).
const FleetMarkerPath = fleetMarkerPath

// RewriteFleetMarker is a marker with its drive_id (and moved_to, "" = left
// as it is) set, every other field kept — the migration tool's -switch.
func RewriteFleetMarker(b []byte, driveID, movedTo string) ([]byte, error) {
	var m fleetMarker
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("marker decode: %w", err)
	}
	if m.Version == 0 || len(m.Fleet) == 0 {
		return nil, fmt.Errorf("marker decode: not a fleet marker")
	}
	m.DriveID = driveID
	if movedTo != "" {
		m.MovedTo = movedTo
	}
	m.WrittenAt = time.Now().UTC()
	return json.Marshal(m)
}

// placedUnder is the order the marker's data is laid out for: PlacedUnder,
// else the marker's own Digest (written before the field existed).
func (m *fleetMarker) placedUnder() string {
	if m.PlacedUnder != "" {
		return m.PlacedUnder
	}
	return m.Digest
}

// FleetState is the outcome of a fingerprint check.
type FleetState string

const (
	FleetRecorded FleetState = "recorded" // no account had a marker: written on every one
	FleetMatch    FleetState = "match"    // every marker records this fleet
	FleetRepaired FleetState = "repaired" // the markers present match; the missing ones written
	FleetMismatch FleetState = "mismatch" // a marker records another order or size
	FleetAccepted FleetState = "accepted" // a mismatch accepted by PERMAFROST_FLEET_ACCEPT: rewritten
	FleetUnknown  FleetState = "unknown"  // a marker could not be read: no verdict, nothing written
)

// FleetFingerprint is what CheckFleetFingerprint found.
type FleetFingerprint struct {
	State  FleetState
	Digest string   // the current fleet's fingerprint
	Diffs  []string // what differs, for a mismatch
	// Placed: every marker records its data laid out for THIS order
	// (placed_under = Digest) — reads may be authoritative. False after an
	// accept until a rebalance, and on a mismatch or no verdict.
	Placed bool
}

// identity is the account's short hash in the fingerprint (its directory and
// user, never a secret).
func (t *odTenant) identity() string {
	sum := sha256.Sum256([]byte(t.tenantID + "\x00" + t.userUPN))
	return hex.EncodeToString(sum[:8])
}

// fleetIdentities is the fleet in placement order.
func (d *OneDriveDriver) fleetIdentities() []string {
	ids := make([]string, len(d.tenants))
	for i, t := range d.tenants {
		ids[i] = t.identity()
	}
	return ids
}

func fleetDigest(ids []string) string {
	sum := sha256.Sum256([]byte(strings.Join(ids, ",")))
	return hex.EncodeToString(sum[:8])
}

// fleetRecheckEvery is how often the probe re-reads the markers after a
// verdict; an unknown verdict is re-checked at the next probe (RP18-1).
const fleetRecheckEvery = time.Hour

// fleetRecheckBudget bounds one re-check inside the probe (the probe itself
// has 15 s; a marker is two Graph calls per account).
const fleetRecheckBudget = 10 * time.Second

// FleetState is the last verdict of CheckFleetFingerprint ("" before any).
func (d *OneDriveDriver) FleetState() FleetState {
	d.fleetMu.Lock()
	defer d.fleetMu.Unlock()
	return d.fleetState
}

// recheckFleet runs the fingerprint check from the probe path when it is
// due: at the next probe after an unknown verdict (a marker unreadable in
// the boot budget — Graph down at deploy time — left the gauge at 0 and
// nothing re-checked for the slot's lifetime, RP18-1), then every
// fleetRecheckEvery. The accept value is the one boot gave; one check at a
// time; its outcome is the driver's log line, never the probe's verdict.
func (d *OneDriveDriver) recheckFleet(ctx context.Context) {
	d.fleetMu.Lock()
	due := d.fleetState != "" && !d.fleetChecking &&
		(d.fleetState == FleetUnknown || time.Since(d.fleetCheckedAt) >= fleetRecheckEvery)
	accept := d.fleetAccept
	if due {
		d.fleetChecking = true
	}
	d.fleetMu.Unlock()
	if !due {
		return
	}
	defer func() {
		d.fleetMu.Lock()
		d.fleetChecking = false
		d.fleetMu.Unlock()
	}()
	ctx, cancel := context.WithTimeout(ctx, fleetRecheckBudget)
	defer cancel()
	_, _ = d.CheckFleetFingerprint(ctx, accept) // logged by the check, on a change of verdict
}

// CheckFleetFingerprint reads every account's marker and compares it with the
// fleet as built (cmd/vaultaire calls it once at boot, bounded; the probe
// re-runs it after an unknown verdict and hourly — recheckFleet). accept is
// PERMAFROST_FLEET_ACCEPT: equal to the current digest, a mismatch is
// accepted and the markers rewritten. An error is returned with
// FleetUnknown (a marker unreadable) or with the state whose writes failed.
// The verdict is logged once per CHANGE of verdict (the first always): an
// hourly re-check that finds the same state writes nothing.
func (d *OneDriveDriver) CheckFleetFingerprint(ctx context.Context, accept string) (FleetFingerprint, error) {
	out, err := d.checkFleetFingerprint(ctx, accept)
	d.fleetMu.Lock()
	d.fleetState, d.fleetAccept, d.fleetCheckedAt = out.State, accept, time.Now()
	d.fleetUnplaced = !out.Placed
	d.fleetMu.Unlock()
	return out, err
}

// fleetVerdictChanged reports whether state differs from the last verdict
// (true before any).
func (d *OneDriveDriver) fleetVerdictChanged(state FleetState) bool {
	d.fleetMu.Lock()
	defer d.fleetMu.Unlock()
	return d.fleetState != state
}

func (d *OneDriveDriver) checkFleetFingerprint(ctx context.Context, accept string) (FleetFingerprint, error) {
	ids := d.fleetIdentities()
	out := FleetFingerprint{Digest: fleetDigest(ids)}

	markers := make([]*fleetMarker, len(d.tenants))
	elsewhere := make([]bool, len(d.tenants)) // the marker read is on another drive of the user
	readErrs := make([]error, len(d.tenants))
	sem := make(chan struct{}, odBootConcurrency) // WP-FLEET-11: 900 markers in a boot
	var wg sync.WaitGroup
	for i, t := range d.tenants {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			markers[i], elsewhere[i], readErrs[i] = t.readFleetMarkerAnyDrive(ctx)
		}()
	}
	wg.Wait()
	for i, t := range d.tenants {
		if err := readErrs[i]; err != nil {
			out.State = FleetUnknown
			if d.fleetVerdictChanged(out.State) {
				d.logger.Warn("permafrost fleet fingerprint: a marker could not be read — no verdict; re-checked at the next probe",
					zap.String("account", t.name), zap.Error(err))
			}
			return out, fmt.Errorf("permafrost fleet fingerprint: read %s: %w", t.name, err)
		}
	}
	d.checkDrives(ctx, markers, elsewhere)

	present := 0
	recorded := map[string][]string{} // recorded digest → recorded fleet
	for i, m := range markers {
		if m == nil {
			continue
		}
		present++
		if m.Index != i {
			out.Diffs = append(out.Diffs, fmt.Sprintf("account %s (%s) is index %d, recorded index %d",
				d.tenants[i].name, ids[i], i, m.Index))
		}
		if !slices.Equal(m.Fleet, ids) {
			recorded[fleetDigest(m.Fleet)] = m.Fleet
		}
	}
	for _, fleet := range recorded {
		out.Diffs = append(out.Diffs, fleetDiff(fleet, ids)...)
	}
	slices.Sort(out.Diffs)
	out.Diffs = slices.Compact(out.Diffs)

	// placed: what each (re)written marker records as placed_under, and
	// whether every marker's data is laid out for THIS order (RB3-1). A
	// marker present keeps its own value: an accept carries the old order
	// forward — the data did not move. One absent takes the fleet's: the
	// value the present markers agree on, else the first one that is not the
	// current digest (the conservative one: it keeps the walk), else the
	// current digest (a first boot, or an account appended to a placed fleet
	// — it holds nothing).
	placedFor := out.Digest
	out.Placed = true
	for _, m := range markers {
		if m == nil {
			continue
		}
		if p := m.placedUnder(); p != out.Digest {
			out.Placed = false
			if placedFor == out.Digest {
				placedFor = p
			}
		}
	}
	placedUnderFor := func(i int) string {
		switch {
		case markers[i] != nil:
			return markers[i].placedUnder() // carried forward (an accept): the data stays where it is
		case out.State == FleetRepaired:
			return placedFor // the fleet's
		default:
			return out.Digest // a first boot, or an account appended at an accept: it holds nothing
		}
	}

	var write []int // the accounts whose marker is (re)written
	switch {
	case present == 0:
		out.State = FleetRecorded
		write = allIndexes(len(markers))
	case len(out.Diffs) == 0 && present == len(markers):
		out.State = FleetMatch
	case len(out.Diffs) == 0:
		out.State = FleetRepaired
		for i, m := range markers {
			if m == nil {
				write = append(write, i)
			}
		}
	case accept != "" && accept == out.Digest:
		out.State = FleetAccepted
		write = allIndexes(len(markers))
	default:
		out.Placed = false
		out.State = FleetMismatch
		permafrostFleetMismatch.Set(1)
		if d.fleetVerdictChanged(out.State) {
			d.logger.Error("permafrost fleet fingerprint mismatch: the fleet's order is not the one its accounts recorded — every object whose home moved (hash % fleet size) is read through the fleet walk. Restore the TENANT_N order, or rebalance and set PERMAFROST_FLEET_ACCEPT to this fingerprint (onedrive_README.md § Fleet fingerprint)",
				zap.Strings("diffs", out.Diffs), zap.String("fingerprint", out.Digest),
				zap.Int("accounts", len(ids)), zap.Int("markers", present))
		}
		return out, nil
	}
	permafrostFleetMismatch.Set(0)

	var (
		failed []string
		fmu    sync.Mutex
	)
	wsem := make(chan struct{}, odBootConcurrency) // WP-FLEET-11: 900 markers in a boot
	var wwg sync.WaitGroup
	for _, i := range write {
		if elsewhere[i] {
			continue // the tree is on another drive: never a fresh marker on this one
		}
		wwg.Add(1)
		go func() {
			defer wwg.Done()
			wsem <- struct{}{}
			defer func() { <-wsem }()
			if err := d.tenants[i].writeFleetMarker(ctx, i, ids, out.Digest, placedUnderFor(i)); err != nil {
				fmu.Lock()
				failed = append(failed, fmt.Sprintf("%s: %v", d.tenants[i].name, err))
				fmu.Unlock()
			}
		}()
	}
	wwg.Wait()
	slices.Sort(failed)
	fields := []zap.Field{zap.String("state", string(out.State)), zap.String("fingerprint", out.Digest),
		zap.Int("accounts", len(ids)), zap.Int("written", len(write)-len(failed)), zap.Bool("placed", out.Placed)}
	switch {
	case len(failed) > 0:
		d.logger.Warn("permafrost fleet fingerprint: markers not written", append(fields, zap.Strings("failed", failed))...)
		return out, fmt.Errorf("permafrost fleet fingerprint: write: %s", strings.Join(failed, "; "))
	case out.State == FleetAccepted:
		d.logger.Warn("permafrost fleet fingerprint: a new fleet order accepted (PERMAFROST_FLEET_ACCEPT) — markers rewritten; the data stays laid out for the previous order (placed_under), so reads walk the fleet until a rebalance rewrites it", fields...)
	case d.fleetVerdictChanged(out.State):
		d.logger.Info("permafrost fleet fingerprint", fields...)
	}
	return out, nil
}

// fleetDiff names what differs between a recorded fleet and the current one.
func fleetDiff(was, now []string) []string {
	var out []string
	if len(was) != len(now) {
		out = append(out, fmt.Sprintf("size %d → %d", len(was), len(now)))
	}
	for k := 0; k < max(len(was), len(now)); k++ {
		w, n := "none", "none"
		if k < len(was) {
			w = was[k]
		}
		if k < len(now) {
			n = now[k]
		}
		if w != n {
			out = append(out, fmt.Sprintf("index %d: was %s, now %s", k, w, n))
		}
	}
	return out
}

func allIndexes(n int) []int {
	out := make([]int, n)
	for i := range out {
		out[i] = i
	}
	return out
}

// checkDrives is the per-account drive verdict (D-F1-2): a marker that
// names another drive, or one found only on another drive of the user, is
// one Error line per account (on a change of verdict) and the gauge 1. Never
// a switch, never a block of boot: the pin, or the migration tool, moves it.
func (d *OneDriveDriver) checkDrives(ctx context.Context, markers []*fleetMarker, elsewhere []bool) {
	for i, t := range d.tenants {
		m := markers[i]
		chosen, _ := t.getDriveID(ctx) // cached: the marker was read on it
		mismatch := m != nil && (elsewhere[i] || (m.DriveID != "" && m.DriveID != chosen))
		state := int32(1)
		if mismatch {
			state = 2
		}
		changed := t.driveMismatch.Swap(state) != state
		if !mismatch {
			permafrostDriveMismatch.WithLabelValues(t.name).Set(0)
			continue
		}
		permafrostDriveMismatch.WithLabelValues(t.name).Set(1)
		if !changed {
			continue
		}
		markerDrive := m.DriveID
		if elsewhere[i] && markerDrive == "" {
			markerDrive = m.foundOn
		}
		d.logger.Error("permafrost account drive mismatch: the fleet's tree is not on the drive this account uses — every object of the account is off its drive. Pin TENANT_N_DRIVE_ID to the marker's drive, or move the tree with cmd/tools/permafrost-migrate-drive (onedrive_README.md § Drive selection)",
			zap.String("account", t.name), zap.String("drive_id", chosen),
			zap.String("marker_drive_id", markerDrive), zap.String("moved_to", m.MovedTo),
			zap.Bool("marker_on_other_drive", elsewhere[i]))
	}
}

// readFleetMarkerAnyDrive is the account's marker on its drive; when that
// drive has none and the listing named other drives of the user (no pin),
// the marker on one of those — onOther true. A deploy that chose a new
// drive for an account would otherwise find no marker, write a fresh one
// and call the fleet repaired.
func (t *odTenant) readFleetMarkerAnyDrive(ctx context.Context) (m *fleetMarker, onOther bool, err error) {
	driveID, err := t.getDriveID(ctx)
	if err != nil {
		return nil, false, err
	}
	m, err = t.readFleetMarkerOn(ctx, driveID)
	if err != nil || m != nil {
		return m, false, err
	}
	t.mu.Lock()
	others := append([]odDrive(nil), t.otherDrives...)
	t.mu.Unlock()
	for _, o := range others {
		om, err := t.readFleetMarkerOn(ctx, o.ID)
		if err != nil {
			return nil, false, fmt.Errorf("marker on drive %s: %w", o.Name, err)
		}
		if om != nil {
			om.foundOn = o.ID
			return om, true, nil
		}
	}
	return nil, false, nil
}

// readFleetMarkerOn is the marker on driveID, nil when it has none.
func (t *odTenant) readFleetMarkerOn(ctx context.Context, driveID string) (*fleetMarker, error) {
	dlURL, _, err := t.getDownloadURL(ctx, driveID, fleetMarkerPath)
	if odIsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, dlURL, nil)
	if err != nil {
		return nil, fmt.Errorf("marker request: %w", err)
	}
	resp, err := t.cdnHTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("marker download: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("marker download: HTTP %d", resp.StatusCode)
	}
	var m fleetMarker
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&m); err != nil {
		return nil, fmt.Errorf("marker decode: %w", err)
	}
	return &m, nil
}

// writeFleetMarker writes the account's marker for index i of fleet ids,
// its data placed under placedUnder (RB3-1).
func (t *odTenant) writeFleetMarker(ctx context.Context, i int, ids []string, digest, placedUnder string) error {
	driveID, err := t.getDriveID(ctx)
	if err != nil {
		return err
	}
	rootID, err := t.ensureRootFolder(ctx, driveID)
	if err != nil {
		return err
	}
	b, err := json.Marshal(fleetMarker{Version: 1, Index: i, Fleet: ids, Digest: digest, WrittenAt: time.Now().UTC(), DriveID: driveID, PlacedUnder: placedUnder})
	if err != nil {
		return fmt.Errorf("marker encode: %w", err)
	}
	return t.simpleUploadAt(ctx, driveID, rootID, fleetMarkerPath, bytes.NewReader(b), 0, int64(len(b)), false)
}

// The account's drive quota as Graph reports it (WP-FLEET-2, D-F1-6), read by
// every probe. `used` EXCLUDES the recycle bin; `deleted` is the bin;
// `remaining` = total − used − deleted, so the bin counts against it (prod
// 2026-11-05: 5120 − 20.61 − 68.37 = 5031.02 GiB on tenant-1).
var (
	permafrostQuotaBytes = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "vaultaire_permafrost_quota_bytes",
		Help: "A permafrost account's drive quota from Graph drive.quota, read by every probe: kind = total | used (live files, the recycle bin excluded) | deleted (the recycle bin) | remaining (total − used − deleted). WP-FLEET-2; onedrive_README.md § OneDrive Gotchas.",
	}, []string{"account", "kind"})
	permafrostQuotaState = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "vaultaire_permafrost_quota_state",
		Help: "1 for a permafrost account's current Graph drive.quota.state (normal | nearing | critical | exceeded), 0 for the others. WP-FLEET-2.",
	}, []string{"account", "state"})
)

// odQuotaStates are Graph's drive.quota.state values.
var odQuotaStates = []string{"normal", "nearing", "critical", "exceeded"}

// odQuota is Graph's drive.quota.
type odQuota struct {
	Total     int64  `json:"total"`
	Used      int64  `json:"used"`
	Remaining int64  `json:"remaining"`
	Deleted   int64  `json:"deleted"`
	State     string `json:"state"`
}

func (q *odQuota) export(account string) {
	permafrostQuotaBytes.WithLabelValues(account, "total").Set(float64(q.Total))
	permafrostQuotaBytes.WithLabelValues(account, "used").Set(float64(q.Used))
	permafrostQuotaBytes.WithLabelValues(account, "remaining").Set(float64(q.Remaining))
	permafrostQuotaBytes.WithLabelValues(account, "deleted").Set(float64(q.Deleted))
	for _, s := range odQuotaStates {
		v := 0.0
		if s == q.State {
			v = 1
		}
		permafrostQuotaState.WithLabelValues(account, s).Set(v)
	}
}
