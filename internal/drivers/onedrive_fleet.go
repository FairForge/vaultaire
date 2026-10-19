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
// never blocked. Accepting a new order after a rebalance:
// PERMAFROST_FLEET_ACCEPT=<the fingerprint the Error line names> rewrites the
// markers at the next boot; a value left set does nothing once they match
// (onedrive_README.md § Fleet fingerprint).
package drivers

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"go.uber.org/zap"
)

// fleetMarkerPath is the marker under the fleet's root folder (outside every
// t-<tenant>/ tree).
const fleetMarkerPath = "_fleet/fingerprint.json"

var permafrostFleetMismatch = prometheus.NewGauge(prometheus.GaugeOpts{
	Name: "vaultaire_permafrost_fleet_mismatch",
	Help: "1 when the permafrost fleet's order (TENANT_1..15, gaps skipped) differs from the order its accounts' markers recorded (read at boot): placement is hash % fleet size over that order, so remapped objects are read through the fleet walk. 0 otherwise, also when the markers could not be read (WP-R7-4, R7-09).",
})

// fleetMarker is one account's marker.
type fleetMarker struct {
	Version   int       `json:"version"`
	Index     int       `json:"index"`
	Fleet     []string  `json:"fleet"`
	Digest    string    `json:"digest"`
	WrittenAt time.Time `json:"written_at"`
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

// CheckFleetFingerprint reads every account's marker and compares it with the
// fleet as built (cmd/vaultaire calls it once at boot, bounded). accept is
// PERMAFROST_FLEET_ACCEPT: equal to the current digest, a mismatch is
// accepted and the markers rewritten. An error is returned with
// FleetUnknown (a marker unreadable) or with the state whose writes failed.
func (d *OneDriveDriver) CheckFleetFingerprint(ctx context.Context, accept string) (FleetFingerprint, error) {
	ids := d.fleetIdentities()
	out := FleetFingerprint{Digest: fleetDigest(ids)}

	markers := make([]*fleetMarker, len(d.tenants))
	for i, t := range d.tenants {
		m, err := t.readFleetMarker(ctx)
		if err != nil {
			out.State = FleetUnknown
			d.logger.Warn("permafrost fleet fingerprint: a marker could not be read — no verdict this boot",
				zap.String("account", t.name), zap.Error(err))
			return out, fmt.Errorf("permafrost fleet fingerprint: read %s: %w", t.name, err)
		}
		markers[i] = m
	}

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
		out.State = FleetMismatch
		permafrostFleetMismatch.Set(1)
		d.logger.Error("permafrost fleet fingerprint mismatch: the fleet's order is not the one its accounts recorded — every object whose home moved (hash % fleet size) is read through the fleet walk. Restore the TENANT_N order, or rebalance and set PERMAFROST_FLEET_ACCEPT to this fingerprint (onedrive_README.md § Fleet fingerprint)",
			zap.Strings("diffs", out.Diffs), zap.String("fingerprint", out.Digest),
			zap.Int("accounts", len(ids)), zap.Int("markers", present))
		return out, nil
	}
	permafrostFleetMismatch.Set(0)

	var failed []string
	for _, i := range write {
		if err := d.tenants[i].writeFleetMarker(ctx, i, ids, out.Digest); err != nil {
			failed = append(failed, fmt.Sprintf("%s: %v", d.tenants[i].name, err))
		}
	}
	fields := []zap.Field{zap.String("state", string(out.State)), zap.String("fingerprint", out.Digest),
		zap.Int("accounts", len(ids)), zap.Int("written", len(write)-len(failed))}
	switch {
	case len(failed) > 0:
		d.logger.Warn("permafrost fleet fingerprint: markers not written", append(fields, zap.Strings("failed", failed))...)
		return out, fmt.Errorf("permafrost fleet fingerprint: write: %s", strings.Join(failed, "; "))
	case out.State == FleetAccepted:
		d.logger.Warn("permafrost fleet fingerprint: a new fleet order accepted (PERMAFROST_FLEET_ACCEPT) — markers rewritten", fields...)
	default:
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

// readFleetMarker is the account's marker, nil when it has none.
func (t *odTenant) readFleetMarker(ctx context.Context) (*fleetMarker, error) {
	driveID, err := t.getDriveID(ctx)
	if err != nil {
		return nil, err
	}
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

// writeFleetMarker writes the account's marker for index i of fleet ids.
func (t *odTenant) writeFleetMarker(ctx context.Context, i int, ids []string, digest string) error {
	driveID, err := t.getDriveID(ctx)
	if err != nil {
		return err
	}
	rootID, err := t.ensureRootFolder(ctx, driveID)
	if err != nil {
		return err
	}
	b, err := json.Marshal(fleetMarker{Version: 1, Index: i, Fleet: ids, Digest: digest, WrittenAt: time.Now().UTC()})
	if err != nil {
		return fmt.Errorf("marker encode: %w", err)
	}
	return t.simpleUpload(ctx, driveID, rootID, fleetMarkerPath, b)
}
