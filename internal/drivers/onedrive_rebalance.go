package drivers

// WP-FLEET-5 PR B: the rebalance behind cmd/tools/permafrost-rebalance.
//
// A fleet whose markers record a placement the data is laid out under (RB3-1
// placed_under, D-F5-2 placement_version) is moved to the current placement
// in three operator steps, each a method here so the tool stays a thin loop
// and every byte goes through the driver's own pacing, retries, hedges and
// quickXorHash checks:
//
//   - RebalancePlan walks every account's vaultaire/ tree and names every
//     file not at the home the TARGET placement gives it (D-F5-5 -dry-run).
//   - RebalanceSetTarget writes placement_target on every marker: the live
//     drivers write to the target placement and read both from their next
//     check (the hourly re-check, or a restart). A run before that is caught
//     by the next -run: nothing is lost, -commit refuses while anything
//     remains.
//   - RebalanceMove copies one file to its home (the driver's upload, hashed
//     as it streams; the source's quickXorHash, the bytes' and the target's
//     must agree), then permanentDeletes the source. A target already holding
//     the same bytes is left and only the source deleted; one holding other
//     bytes refuses. Resumable: a cut run leaves every file readable (the
//     second look, D-F5-3).
//   - RebalanceCommit walks again and, only when nothing is left to move,
//     rewrites every marker: placed_under = the digest, placement_version =
//     the target, no target — the moment reads become authoritative.
//
// The data moves through the box (download, then upload), never a Graph
// server-side copy: one code path, every byte hashed, cross-tenant the same
// as within a tenant. For prod's three accounts (bench leftovers and the
// markers) that is minutes; a 40 TiB tenant is a day at its 400 GB/h — the
// pacing holds it there.

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"

	"go.uber.org/zap"
)

// ErrRebalanceIncomplete: a commit was asked while files are still off their
// home.
var ErrRebalanceIncomplete = errors.New("permafrost rebalance: files remain to move")

// ErrRebalanceTargetDiffers: the home already holds another object under the
// path (never overwritten).
var ErrRebalanceTargetDiffers = errors.New("permafrost rebalance: the home holds other bytes under the path")

// RebalanceAccount is one fleet account as the plan names it.
type RebalanceAccount struct {
	Index    int // the fleet position
	Name     string
	Identity string
	Files    int
	Bytes    int64
	MovesOut int
	MovesIn  int
}

// RebalanceMove is one file to move: its storage path (under vaultaire/),
// size, the source's quickXorHash ("" when Graph reported none), and the
// fleet positions it leaves and reaches.
type RebalanceMove struct {
	Path     string
	Size     int64
	Hash     string
	From, To int
}

// RebalancePlan is what a walk found.
type RebalancePlan struct {
	Digest   string
	State    FleetState
	Laid     int  // the placement the markers say the data is laid out under
	Target   int  // the markers' placement_target (0 = none set)
	Version  int  // the placement this plan moves to
	Placed   bool // the markers are placed under Digest with no target
	Accounts []RebalanceAccount
	Files    int
	Bytes    int64
	Moves    []RebalanceMove
	// Strays are files found where neither the target nor the recorded
	// placement puts them (an earlier cut run, a slot migration's remnant):
	// moved like the others, named so the operator sees them.
	Strays []string
}

// MoveBytes is the size of every move.
func (p *RebalancePlan) MoveBytes() (n int64) {
	for _, m := range p.Moves {
		n += m.Size
	}
	return n
}

// rebalanceVerdict reads the markers (the fingerprint check, which writes
// nothing on a settled fleet) and refuses a fleet whose order is not settled.
func (d *OneDriveDriver) rebalanceVerdict(ctx context.Context) (FleetFingerprint, placementVerdict, error) {
	fp, err := d.CheckFleetFingerprint(ctx, "")
	if err != nil {
		return fp, placementVerdict{}, err
	}
	switch fp.State {
	case FleetMismatch:
		return fp, placementVerdict{}, fmt.Errorf("permafrost rebalance: the fleet's order is not the one its markers record (%s) — accept it first (PERMAFROST_FLEET_ACCEPT=%s, a restart), then rebalance", strings.Join(fp.Diffs, "; "), fp.Digest)
	case FleetUnknown:
		return fp, placementVerdict{}, errors.New("permafrost rebalance: a marker could not be read — no verdict")
	}
	markers := make([]*fleetMarker, len(d.tenants))
	for i, t := range d.tenants {
		m, _, err := t.readFleetMarkerAnyDrive(ctx)
		if err != nil {
			return fp, placementVerdict{}, fmt.Errorf("permafrost rebalance: read marker %s: %w", t.name, err)
		}
		if m == nil {
			return fp, placementVerdict{}, fmt.Errorf("permafrost rebalance: account %s has no marker after the check", t.name)
		}
		markers[i] = m
	}
	return fp, placementOfMarkers(markers, fp.Digest), nil
}

// RebalancePlan walks the fleet and names what moves to be laid out under
// placement version target over the current fleet.
func (d *OneDriveDriver) RebalancePlan(ctx context.Context, target int) (*RebalancePlan, error) {
	if target < 1 || target > odPlacementCurrent {
		return nil, fmt.Errorf("permafrost rebalance: placement version %d does not exist (1..%d)", target, odPlacementCurrent)
	}
	fp, pv, err := d.rebalanceVerdict(ctx)
	if err != nil {
		return nil, err
	}
	plan := &RebalancePlan{Digest: fp.Digest, State: fp.State, Laid: pv.laid, Target: pv.target, Version: target, Placed: fp.Placed}
	want := d.placementOver(target)
	prev := d.previousPlacement()
	if prev == nil && fp.PlacementVersion != target {
		prev = d.placementOver(fp.PlacementVersion)
	}
	plan.Accounts = make([]RebalanceAccount, len(d.tenants))
	for i, t := range d.tenants {
		plan.Accounts[i] = RebalanceAccount{Index: i, Name: t.name, Identity: t.identity()}
	}

	type found struct {
		at   int
		path string
		size int64
		hash string
	}
	var (
		mu    sync.Mutex
		files []found
		werr  error
	)
	sem := make(chan struct{}, odBootConcurrency)
	var wg sync.WaitGroup
	for i, t := range d.tenants {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			err := d.walkAccount(ctx, t, func(p string, size int64, hash string) {
				mu.Lock()
				files = append(files, found{at: i, path: p, size: size, hash: hash})
				mu.Unlock()
			})
			if err != nil {
				mu.Lock()
				werr = errors.Join(werr, fmt.Errorf("walk %s: %w", t.name, err))
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if werr != nil {
		return nil, fmt.Errorf("permafrost rebalance: %w", werr)
	}
	sort.Slice(files, func(a, b int) bool {
		if files[a].path != files[b].path {
			return files[a].path < files[b].path
		}
		return files[a].at < files[b].at
	})
	for _, f := range files {
		plan.Files++
		plan.Bytes += f.size
		plan.Accounts[f.at].Files++
		plan.Accounts[f.at].Bytes += f.size
		home := d.tenantByIdentity(want.home(f.path).identity)
		if home == nil || home == d.tenants[f.at] {
			continue
		}
		to := d.positionOf(home)
		plan.Moves = append(plan.Moves, RebalanceMove{Path: f.path, Size: f.size, Hash: f.hash, From: f.at, To: to})
		plan.Accounts[f.at].MovesOut++
		plan.Accounts[to].MovesIn++
		if prev != nil {
			if old := d.tenantByIdentity(prev.home(f.path).identity); old != nil && old != d.tenants[f.at] {
				plan.Strays = append(plan.Strays, d.tenants[f.at].name+": "+f.path)
			}
		}
	}
	return plan, nil
}

// positionOf is the account's fleet position (-1 when it is not in it).
func (d *OneDriveDriver) positionOf(t *odTenant) int {
	for i, x := range d.tenants {
		if x == t {
			return i
		}
	}
	return -1
}

// walkAccount calls fn with every file under the account's vaultaire/ tree
// (path relative to it) but the fleet's own folder.
func (d *OneDriveDriver) walkAccount(ctx context.Context, t *odTenant, fn func(path string, size int64, hash string)) error {
	driveID, err := t.getDriveID(ctx)
	if err != nil {
		return fmt.Errorf("onedrive get drive: %w", err)
	}
	var walk func(rel string) error
	walk = func(rel string) error {
		folder := odRootFolder
		if rel != "" {
			folder += "/" + rel
		}
		var subs []string
		err := t.listChildren(ctx, driveID, folder, func(c odChild) {
			p := c.name
			if rel != "" {
				p = rel + "/" + c.name
			}
			if c.folder {
				subs = append(subs, p)
				return
			}
			fn(p, c.size, c.hash)
		})
		if err != nil {
			if odIsNotFound(err) {
				return nil // nothing under the folder on this account
			}
			return err
		}
		for _, s := range subs {
			if s == odFleetFolder {
				continue // the markers' folder
			}
			if err := walk(s); err != nil {
				return err
			}
		}
		return nil
	}
	return walk("")
}

// RebalanceSetTarget writes placement_target = target on every marker (every
// other field kept). The live drivers follow it at their next check.
func (d *OneDriveDriver) RebalanceSetTarget(ctx context.Context, target int) error {
	if target < 1 || target > odPlacementCurrent {
		return fmt.Errorf("permafrost rebalance: placement version %d does not exist (1..%d)", target, odPlacementCurrent)
	}
	fp, _, err := d.rebalanceVerdict(ctx)
	if err != nil {
		return err
	}
	return d.rewriteMarkers(ctx, fp.Digest, func(m *fleetMarker) {
		if m.PlacementVersion == target || (m.PlacementVersion == 0 && target == 1) {
			m.PlacementTarget = 0 // already there
			return
		}
		m.PlacementTarget = target
	})
}

// RebalanceCommit walks again and, when nothing is left to move, rewrites
// every marker as placed under the current order and laid out by target.
func (d *OneDriveDriver) RebalanceCommit(ctx context.Context, target int) (*RebalancePlan, error) {
	plan, err := d.RebalancePlan(ctx, target)
	if err != nil {
		return nil, err
	}
	if len(plan.Moves) > 0 {
		return plan, fmt.Errorf("%w: %d files, %d bytes", ErrRebalanceIncomplete, len(plan.Moves), plan.MoveBytes())
	}
	tenants := d.markerTenants()
	err = d.rewriteMarkers(ctx, plan.Digest, func(m *fleetMarker) {
		m.PlacedUnder = plan.Digest
		m.PlacementVersion = target
		m.PlacementTarget = 0
		m.Tenants = tenants
	})
	if err != nil {
		return plan, err
	}
	// This driver's own verdict follows at once (the live slots at their
	// next check).
	if _, err := d.CheckFleetFingerprint(ctx, ""); err != nil {
		return plan, err
	}
	return plan, nil
}

// rewriteMarkers edits every account's marker in place.
func (d *OneDriveDriver) rewriteMarkers(ctx context.Context, digest string, edit func(*fleetMarker)) error {
	ids := d.fleetIdentities()
	var failed []string
	for i, t := range d.tenants {
		m, _, err := t.readFleetMarkerAnyDrive(ctx)
		if err != nil || m == nil {
			failed = append(failed, fmt.Sprintf("%s: read: %v", t.name, err))
			continue
		}
		edit(m)
		m.Fleet, m.Digest, m.Index = ids, digest, i
		if err := t.writeFleetMarker(ctx, i, ids, digest, m.placedUnder(), m.PlacementVersion, m.PlacementTarget, m.Tenants); err != nil {
			failed = append(failed, fmt.Sprintf("%s: write: %v", t.name, err))
		}
	}
	if len(failed) > 0 {
		return fmt.Errorf("permafrost rebalance: markers: %s", strings.Join(failed, "; "))
	}
	return nil
}

// RebalanceMove moves one file to its home: the home's copy is checked first
// (the same bytes = only the source goes; other bytes = refused), else the
// source is streamed through the driver's upload — hashed as it goes; the
// source's hash, the bytes' and the stored item's must agree — and then
// permanentDeleted. A source already gone is done.
func (d *OneDriveDriver) RebalanceMove(ctx context.Context, m RebalanceMove) error {
	if m.From < 0 || m.From >= len(d.tenants) || m.To < 0 || m.To >= len(d.tenants) || m.From == m.To {
		return fmt.Errorf("permafrost rebalance: move %s: accounts %d → %d", m.Path, m.From, m.To)
	}
	from, to := d.tenants[m.From], d.tenants[m.To]
	fromDrive, err := from.getDriveID(ctx)
	if err != nil {
		return fmt.Errorf("permafrost rebalance: %s: get drive: %w", from.name, err)
	}
	toDrive, err := to.getDriveID(ctx)
	if err != nil {
		return fmt.Errorf("permafrost rebalance: %s: get drive: %w", to.name, err)
	}
	toRoot, err := to.ensureRootFolder(ctx, toDrive)
	if err != nil {
		return fmt.Errorf("permafrost rebalance: %s: ensure folder: %w", to.name, err)
	}

	src, err := from.getItem(ctx, fromDrive, m.Path)
	if err != nil {
		if odIsNotFound(err) {
			d.logger.Info("permafrost rebalance: source gone, nothing to move", zap.String("path", m.Path), zap.String("from", from.name))
			return nil
		}
		return fmt.Errorf("permafrost rebalance: %s: read %s: %w", from.name, m.Path, err)
	}
	srcHash := src.File.Hashes.QuickXorHash
	if m.Hash != "" && srcHash != "" && m.Hash != srcHash {
		return fmt.Errorf("permafrost rebalance: %s: %s changed since the plan (quickXorHash %s, planned %s)", from.name, m.Path, srcHash, m.Hash)
	}

	dst, err := to.getItem(ctx, toDrive, m.Path)
	switch {
	case err != nil && !odIsNotFound(err):
		return fmt.Errorf("permafrost rebalance: %s: read %s: %w", to.name, m.Path, err)
	case err == nil && dst.Size == src.Size && (dst.File.Hashes.QuickXorHash == srcHash || srcHash == "" || dst.File.Hashes.QuickXorHash == ""):
		// The home already holds the bytes (a cut earlier run): the source goes.
		d.logger.Info("permafrost rebalance: the home already holds the file; the source goes",
			zap.String("path", m.Path), zap.String("from", from.name), zap.String("to", to.name))
	case err == nil:
		return fmt.Errorf("%w: %s on %s (%d bytes, %s) against the source on %s (%d bytes, %s)", ErrRebalanceTargetDiffers,
			m.Path, to.name, dst.Size, dst.File.Hashes.QuickXorHash, from.name, src.Size, srcHash)
	default:
		if err := d.rebalanceTransfer(ctx, from, fromDrive, to, toDrive, toRoot, m.Path, src); err != nil {
			return err
		}
	}
	// The source goes, for good (#1168).
	if err := from.deleteItem(ctx, fromDrive, src.ID); err != nil && !odIsNotFound(err) {
		return fmt.Errorf("permafrost rebalance: %s: delete %s: %w", from.name, m.Path, err)
	}
	return nil
}

// rebalanceTransfer streams the source into the home and checks the hashes.
func (d *OneDriveDriver) rebalanceTransfer(ctx context.Context, from *odTenant, fromDrive string, to *odTenant, toDrive, toRoot, p string, src *odItem) error {
	var body io.ReadCloser
	if src.Size == 0 {
		body = io.NopCloser(strings.NewReader(""))
	} else {
		dlURL, size, err := from.getDownloadURL(ctx, fromDrive, p)
		if err != nil {
			return fmt.Errorf("permafrost rebalance: %s: download url %s: %w", from.name, p, err)
		}
		if size != src.Size {
			return fmt.Errorf("permafrost rebalance: %s: %s is %d bytes, the item said %d", from.name, p, size, src.Size)
		}
		body, err = from.downloadRanges(ctx, dlURL, size, odAdaptiveStreams(size))
		if err != nil {
			return fmt.Errorf("permafrost rebalance: %s: download %s: %w", from.name, p, err)
		}
	}
	defer func() { _ = body.Close() }()
	h := NewQuickXorHash()
	if err := to.upload(ctx, toDrive, toRoot, p, io.TeeReader(body, h), src.Size); err != nil {
		return fmt.Errorf("permafrost rebalance: %s: upload %s: %w", to.name, p, err)
	}
	got := base64.StdEncoding.EncodeToString(h.Sum(nil))
	srcHash := src.File.Hashes.QuickXorHash
	if srcHash != "" && got != srcHash {
		// The bytes read are not the source's: the copy goes, the source stays.
		if id, err := to.getItemID(ctx, toDrive, p); err == nil {
			_ = to.deleteItem(ctx, toDrive, id)
		}
		return fmt.Errorf("permafrost rebalance: %s: %s read as quickXorHash %s, the source says %s", from.name, p, got, srcHash)
	}
	return nil
}
