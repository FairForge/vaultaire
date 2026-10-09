package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/FairForge/vaultaire/internal/common"
	"github.com/FairForge/vaultaire/internal/drivers"
	"github.com/FairForge/vaultaire/internal/engine"
	"github.com/lib/pq"
	"go.uber.org/zap"
)

// The reconcile pass of the vault_parity job (Prompt 2a PR 5; bounded and
// made safe on Sync by Prompt 2a.2 G3): shard folders no vault_parity row
// names are erased after two sightings OrphanGrace apart, and the empty
// folders the deletes leave are removed.
//
// The walk is folder by folder on a leg that can list one (ListDir: webdav,
// local, the OneDrive fleet — whose List sees only the container's direct
// children, so a List-based walk found nothing there): the container's
// `<digest>` folders in order, each one's `<etag>` folders, and the files of
// every `<etag>` folder no row names (a named folder is never listed). Each
// listing is one call against the leg — on Sync one PROPFIND against the
// account's ~14 ops/s, which also serves customers — so a run spends at
// most MaxReconcileListingsPerRun per leg and resumes where it stopped: the
// cursor (tenant, digest) is in job_runs.result, which survives a deploy.
// A leg without ListDir (S3) is listed flat, once per tenant.
//
// A candidate is an `<etag>` folder no row names: one with shard files, or
// an empty one on a leg that can remove folders (an overwrite or delete
// whose folder removal a lagging bridge refused). Both get a sighting and
// are acted on only past the grace. A `<digest>` folder is removed when
// nothing is left in it and no row names a folder under it. Every folder
// removal goes through RemoveEmptyDir, which on Sync deletes only what every
// bridge lists empty (a collection DELETE is recursive).

const (
	// defaultReconcileEvery: the reconcile runs at most this often
	// (VAULT_PARITY_RECONCILE_EVERY).
	defaultReconcileEvery = 30 * time.Minute
	// defaultReconcileSlice: the share of a run the reconcile keeps.
	defaultReconcileSlice = 10 * time.Minute
	// defaultMaxReconcileListingsPerRun: folder listings per leg per run.
	defaultMaxReconcileListingsPerRun = 1000
	// sightingPruneAge: a sighting of an erased tenant or of a leg no longer
	// registered is dropped once it has not been seen for this long.
	sightingPruneAge = 7 * 24 * time.Hour
)

// sightingErased is the first_seen a sighting gets once its files have been
// erased (its folders not yet): a later pass that meets the files again (a
// lagging listing) deletes them again but does not count them again. Older
// than any grace, so the folders are retried at once.
var sightingErased = time.Unix(0, 0).UTC()

// reconcileEveryFromEnv is VAULT_PARITY_RECONCILE_EVERY (1m–24h), else the
// default; a rejected value is logged at Warn and the default kept.
func reconcileEveryFromEnv(getenv func(string) string, logger *zap.Logger) time.Duration {
	raw := strings.TrimSpace(getenv("VAULT_PARITY_RECONCILE_EVERY"))
	if raw == "" {
		return defaultReconcileEvery
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d < time.Minute || d > 24*time.Hour {
		logger.Warn("VAULT_PARITY_RECONCILE_EVERY rejected; default kept", zap.String("value", raw), zap.Duration("default", defaultReconcileEvery))
		return defaultReconcileEvery
	}
	return d
}

// emptyDirRemover is the optional driver surface that removes an empty
// folder (local, webdav; nothing on an S3 leg). On Sync it deletes only
// what every bridge lists empty (drivers.ErrDirNotEmpty otherwise).
type emptyDirRemover interface {
	RemoveEmptyDir(ctx context.Context, container, dir string) error
}

// folderLister is the optional driver surface that lists one folder's
// direct members (webdav, local, the OneDrive fleet).
type folderLister interface {
	ListDir(ctx context.Context, container, dir string) (dirs, files []string, err error)
}

// reconcileCursor is where a leg's walk resumes: tenants up to After are
// done; Tenant (when set) is in progress, its digests up to Digest done.
type reconcileCursor struct {
	After  string `json:"after,omitempty"`
	Tenant string `json:"tenant,omitempty"`
	Digest string `json:"digest,omitempty"`
}

type reconcileState struct {
	At      time.Time
	Cursors map[string]reconcileCursor
}

func (s reconcileState) cursors() map[string]reconcileCursor {
	out := make(map[string]reconcileCursor, len(s.Cursors))
	for k, v := range s.Cursors {
		out[k] = v
	}
	return out
}

func (p *VaultParity) reconcileEvery() time.Duration {
	if p.ReconcileEvery == 0 {
		return defaultReconcileEvery
	}
	return p.ReconcileEvery
}

func (p *VaultParity) reconcileSlice() time.Duration {
	if p.ReconcileSlice <= 0 {
		return defaultReconcileSlice
	}
	return p.ReconcileSlice
}

// loadReconcileState is the reconcile state of the last run: job_runs.result
// (written by the scheduler — it survives a deploy), or the in-memory copy
// when it is newer (a run whose result was not recorded) or the table has
// none.
func (p *VaultParity) loadReconcileState(ctx context.Context) reconcileState {
	st := p.reconcileMem
	var raw []byte
	if err := p.db.QueryRowContext(ctx, `SELECT result FROM job_runs WHERE job = $1`, p.JobName).Scan(&raw); err != nil || len(raw) == 0 {
		return st
	}
	var last VaultParityResult
	if json.Unmarshal(raw, &last) != nil || last.ReconcileAt.Before(st.At) || last.ReconcileAt.IsZero() && len(last.ReconcileCursors) == 0 {
		return st
	}
	return reconcileState{At: last.ReconcileAt, Cursors: last.ReconcileCursors}
}

// reconcileOrphans is the pass over `<tenant>__parity/` on every registered
// leg of Legs whose breaker is closed. Nothing here fails the run: a leg
// that cannot be listed or a delete that fails is a note; the walk is
// bounded per leg (listings, candidates, tenants) and by its time slice.
func (p *VaultParity) reconcileOrphans(ctx context.Context, res *VaultParityResult, st reconcileState) {
	status := p.eng.GetFailoverStatus()
	names := p.Legs
	if len(names) == 0 {
		names = vaultParityLegs
	}
	cursors := st.cursors()
	var registered []string
	type leg struct {
		name string
		drv  engine.Driver
	}
	var legs []leg
	for _, name := range names {
		drv, ok := p.eng.GetDriver(name)
		if !ok {
			res.SkippedLegs = append(res.SkippedLegs, name+": not registered")
			continue
		}
		registered = append(registered, name)
		if status[name] == engine.StateOpen.String() {
			res.SkippedLegs = append(res.SkippedLegs, name+": circuit breaker open")
			continue
		}
		legs = append(legs, leg{name, drv})
		res.ReconciledLegs = append(res.ReconciledLegs, name)
	}
	p.pruneSightings(ctx, res, registered)
	for _, l := range legs {
		if ctx.Err() != nil {
			break
		}
		cur := cursors[l.name]
		p.reconcileLeg(ctx, res, l.name, l.drv, &cur)
		cursors[l.name] = cur
	}
	if ctx.Err() != nil && res.ReconcileStopped == "" {
		res.ReconcileStopped = "time slice"
	}
	p.reconcileMem = reconcileState{At: p.now(), Cursors: cursors}
	res.ReconcileAt, res.ReconcileCursors = p.reconcileMem.At, cursors
}

// pruneSightings drops sightings nobody will revisit: of a tenant that no
// longer exists (a reconcile racing the deletion runner wrote it after
// EraseRows) or of a leg no longer registered, once not seen for a week.
func (p *VaultParity) pruneSightings(ctx context.Context, res *VaultParityResult, registered []string) {
	r, err := p.db.ExecContext(ctx, `
		DELETE FROM vault_parity_orphans o
		WHERE o.last_seen < $1
		  AND (o.leg <> ALL($2) OR NOT EXISTS (SELECT 1 FROM tenants t WHERE t.id = o.tenant_id))
		  AND ($3 = '' OR o.tenant_id = $3)`,
		p.now().Add(-sightingPruneAge), pq.Array(registered), p.scopeTenant)
	if err != nil {
		res.Errors = append(res.Errors, "reconcile: prune sightings: "+err.Error())
		return
	}
	n, _ := r.RowsAffected()
	res.SightingsPruned += int(n)
}

// reconcileLeg walks one leg from its cursor until the tenants, the
// listing budget or the time slice run out, and moves the cursor.
func (p *VaultParity) reconcileLeg(ctx context.Context, res *VaultParityResult, legName string, drv engine.Driver, cur *reconcileCursor) {
	w := &parityWalk{p: p, res: res, leg: legName, drv: drv,
		listings: p.MaxReconcileListingsPerRun, candidates: p.MaxOrphanCandidatesPerRun}
	if w.listings <= 0 {
		w.listings = defaultMaxReconcileListingsPerRun
	}
	if w.candidates <= 0 {
		w.candidates = defaultMaxOrphanCandidatesPerRun
	}
	tenants, full, err := p.reconcileTenants(ctx, *cur)
	if err != nil {
		res.Errors = append(res.Errors, "reconcile: tenants: "+err.Error())
		return
	}
	for _, tenantID := range tenants {
		after := ""
		if tenantID == cur.Tenant {
			after = cur.Digest
		}
		res.ReconcileTenants++
		done, last := w.tenant(ctx, tenantID, after)
		if !done {
			cur.Tenant, cur.Digest = tenantID, last
			return
		}
		if tenantID > cur.After {
			cur.After = tenantID
		}
		cur.Tenant, cur.Digest = "", ""
	}
	if !full || p.scopeTenant != "" {
		*cur = reconcileCursor{} // wrap: the next walk starts over
	}
}

// reconcileTenants is the tenants this run walks: the scoped one, or the
// tenant in progress then the next page after the cursor. full is true when
// the page was cut by MaxReconcileTenantsPerRun (more tenants follow).
func (p *VaultParity) reconcileTenants(ctx context.Context, cur reconcileCursor) ([]string, bool, error) {
	if p.scopeTenant != "" {
		return []string{p.scopeTenant}, false, nil
	}
	limit := p.MaxReconcileTenantsPerRun
	if limit <= 0 {
		limit = defaultMaxReconcileTenantsPerRun
	}
	var ids []string
	from := cur.After
	if cur.Tenant != "" {
		ids = append(ids, cur.Tenant)
		from = max(from, cur.Tenant)
	}
	rows, err := p.db.QueryContext(ctx, `SELECT id FROM tenants WHERE id > $1 ORDER BY id LIMIT $2`, from, limit)
	if err != nil {
		return nil, false, err
	}
	defer func() { _ = rows.Close() }()
	n := 0
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, false, err
		}
		ids = append(ids, id)
		n++
	}
	return ids, n == limit, rows.Err()
}

// parityWalk is one leg's walk in one run, with its budgets.
type parityWalk struct {
	p          *VaultParity
	res        *VaultParityResult
	leg        string
	drv        engine.Driver
	listings   int
	candidates int
}

// digestView is what the walk saw of one `<digest>` folder.
type digestView struct {
	name  string
	etags map[string][]string // unnamed `<etag>` folder → its files
	// other: something under the digest the pass does not judge (a file
	// directly in it, a deeper folder, a folder it could not list) — the
	// digest folder stays.
	other bool
}

// spend takes one listing from the budget; false when there is none left.
func (w *parityWalk) spend() bool {
	if w.listings <= 0 {
		w.res.ReconcileStopped = "listing budget"
		return false
	}
	w.listings--
	w.res.ReconcileListings++
	return true
}

// tenant reconciles one tenant's parity container on the leg, digests
// after `after`; done is false when a budget or the time slice ran out,
// last is then the last digest fully handled.
func (w *parityWalk) tenant(ctx context.Context, tenantID, after string) (done bool, last string) {
	p := w.p
	tctx := common.WithTenantID(ctx, tenantID)
	container := parityContainer(tenantID)
	before, err := p.namedPrefixes(ctx, tenantID)
	if err != nil {
		w.res.Errors = append(w.res.Errors, "reconcile: rows of "+tenantID+": "+err.Error())
		return ctx.Err() == nil, after
	}
	views, done, err := w.list(tctx, container, after, before)
	if err != nil {
		w.res.Errors = append(w.res.Errors, fmt.Sprintf("reconcile: list %s on %s: %v", container, w.leg, err))
		return ctx.Err() == nil, after // a broken container never wedges the cursor
	}
	// The rows are read again AFTER the listing: a row is inserted before
	// its folder is created, so a folder the listing saw without a row in
	// this read has none.
	named, err := p.namedPrefixes(ctx, tenantID)
	if err != nil {
		w.res.Errors = append(w.res.Errors, "reconcile: rows of "+tenantID+": "+err.Error())
		return false, after
	}
	sightings, err := p.sightings(ctx, tenantID, w.leg)
	if err != nil {
		w.res.Errors = append(w.res.Errors, "reconcile: sightings: "+err.Error())
		return false, after
	}
	last = after
	met := map[string]bool{}
	for _, v := range views {
		if ctx.Err() != nil {
			return false, last
		}
		if !w.digest(ctx, tctx, tenantID, container, v, named, met) {
			w.res.ReconcileTruncated = true
			done = false
			break
		}
		last = v.name
	}
	// Sightings in the walked range the pass did not meet: the row appeared
	// or the folder is gone — forgotten.
	var forget []string
	for prefix := range sightings {
		d, _, _ := strings.Cut(prefix, "/")
		if met[prefix] || d <= after || (!done && d > last) {
			continue
		}
		forget = append(forget, prefix)
	}
	p.forget(ctx, w.res, tenantID, w.leg, forget)
	return done, last
}

// list walks the container: digests after `after`, in order, until a budget
// runs out (done false). Folders `before` names are not listed.
func (w *parityWalk) list(ctx context.Context, container, after string, before map[string]bool) ([]digestView, bool, error) {
	lister, ok := w.drv.(folderLister)
	if !ok {
		return w.listFlat(ctx, container, after)
	}
	if !w.spend() {
		return nil, false, nil
	}
	digests, _, err := lister.ListDir(ctx, container, "")
	if err != nil {
		return nil, false, err
	}
	sort.Strings(digests)
	var views []digestView
	for _, d := range digests {
		if d <= after {
			continue
		}
		if ctx.Err() != nil || !w.spend() {
			return views, false, nil
		}
		subs, files, err := lister.ListDir(ctx, container, d)
		v := digestView{name: d, etags: map[string][]string{}, other: len(files) > 0 || err != nil}
		if err != nil {
			w.res.Errors = append(w.res.Errors, fmt.Sprintf("reconcile: list %s/%s on %s: %v", container, d, w.leg, err))
		}
		for _, e := range subs {
			prefix := d + "/" + e
			if before[prefix] {
				continue // a row's folder: never listed, never touched
			}
			if ctx.Err() != nil || !w.spend() {
				return views, false, nil // this digest again next run
			}
			sub, shards, err := lister.ListDir(ctx, container, prefix)
			if err != nil {
				w.res.Errors = append(w.res.Errors, fmt.Sprintf("reconcile: list %s/%s on %s: %v", container, prefix, w.leg, err))
				v.other = true
				continue
			}
			if len(sub) > 0 {
				v.other = true // a deeper folder is not a shard folder: left alone
				continue
			}
			v.etags[e] = shards
		}
		views = append(views, v)
	}
	return views, true, nil
}

// listFlat is the walk of a leg without folders (S3): one List of the
// container, grouped by `<digest>/<etag>/<file>`.
func (w *parityWalk) listFlat(ctx context.Context, container, after string) ([]digestView, bool, error) {
	if !w.spend() {
		return nil, false, nil
	}
	names, err := w.drv.List(ctx, container, "")
	if err != nil {
		return nil, false, err
	}
	byDigest := map[string]*digestView{}
	for _, name := range names {
		segs := strings.Split(name, "/")
		if segs[0] <= after {
			continue
		}
		v := byDigest[segs[0]]
		if v == nil {
			v = &digestView{name: segs[0], etags: map[string][]string{}}
			byDigest[segs[0]] = v
		}
		if len(segs) != 3 {
			v.other = true
			continue
		}
		v.etags[segs[1]] = append(v.etags[segs[1]], segs[1]+"/"+segs[2])
	}
	views := make([]digestView, 0, len(byDigest))
	for _, v := range byDigest {
		for e, files := range v.etags {
			for i, f := range files {
				_, files[i], _ = strings.Cut(f, "/")
			}
			v.etags[e] = files
		}
		views = append(views, *v)
	}
	sort.Slice(views, func(i, j int) bool { return views[i].name < views[j].name })
	return views, true, nil
}

// digest settles one digest folder: its unnamed `<etag>` folders as
// candidates, then the digest folder itself when nothing is left in it.
// false when the candidate budget ran out (the digest is redone next run).
func (w *parityWalk) digest(ctx, tctx context.Context, tenantID, container string, v digestView,
	named map[string]bool, met map[string]bool) bool {
	rm, canRemove := w.drv.(emptyDirRemover)
	left := v.other || namedUnder(named, v.name) // a row's folder sits under the digest
	etags := make([]string, 0, len(v.etags))
	for e := range v.etags {
		etags = append(etags, e)
	}
	sort.Strings(etags)
	for _, e := range etags {
		prefix := v.name + "/" + e
		files := v.etags[e]
		if named[prefix] {
			left = true
			continue
		}
		if len(files) == 0 && !canRemove {
			continue // an empty folder this leg cannot remove (or does not have)
		}
		if w.candidates <= 0 {
			return false
		}
		w.candidates--
		met[prefix] = true
		if !w.candidate(ctx, tctx, tenantID, container, prefix, files, rm) {
			left = true
		}
	}
	if canRemove && !left {
		if err := rm.RemoveEmptyDir(tctx, container, v.name); err != nil {
			w.p.logger.Info("vault parity: digest folder not removed yet", zap.String("leg", w.leg),
				zap.String("folder", v.name), zap.Error(err))
		} else {
			w.res.OrphanFoldersRemoved++
		}
	}
	return true
}

func namedUnder(named map[string]bool, digest string) bool {
	for prefix := range named {
		if strings.HasPrefix(prefix, digest+"/") {
			return true
		}
	}
	return false
}

// candidate handles one unnamed `<etag>` folder: a sighting (upserted), and
// once past the grace its files are deleted, then the folder. true when
// the folder is gone (its sighting forgotten).
func (w *parityWalk) candidate(ctx, tctx context.Context, tenantID, container, prefix string, files []string,
	rm emptyDirRemover) bool {
	p := w.p
	now := p.now()
	var firstSeen time.Time
	var inserted bool
	err := p.db.QueryRowContext(ctx, `
		INSERT INTO vault_parity_orphans (tenant_id, leg, prefix, first_seen, last_seen) VALUES ($1, $2, $3, $4, $4)
		ON CONFLICT (tenant_id, leg, prefix) DO UPDATE SET last_seen = EXCLUDED.last_seen
		RETURNING first_seen, (xmax = 0)`, tenantID, w.leg, prefix, now).Scan(&firstSeen, &inserted)
	if err != nil {
		w.res.Errors = append(w.res.Errors, "reconcile: sighting of "+prefix+": "+err.Error())
		return false
	}
	if inserted {
		w.res.OrphansFound++
		vaultParityOrphans.WithLabelValues("found").Inc()
	}
	grace := p.OrphanGrace
	if grace <= 0 {
		grace = defaultOrphanGrace
	}
	if now.Sub(firstSeen) < grace {
		return false
	}
	alreadyErased := firstSeen.Equal(sightingErased)
	for _, f := range files {
		if err := w.drv.Delete(tctx, container, prefix+"/"+f); err != nil && !isObjectMissingErr(err) {
			w.res.OrphansFailed++
			vaultParityOrphans.WithLabelValues("failed").Inc()
			w.res.Errors = append(w.res.Errors, fmt.Sprintf("reconcile: erase %s on %s: delete %s: %v", prefix, w.leg, f, err))
			return false
		}
	}
	if len(files) > 0 && !alreadyErased {
		w.res.OrphansErased++
		vaultParityOrphans.WithLabelValues("erased").Inc()
		p.logger.Warn("vault parity: orphan shards erased (no row named them)",
			zap.String("tenant_id", tenantID), zap.String("leg", w.leg), zap.String("folder", prefix),
			zap.Int("files", len(files)), zap.Duration("first_seen_ago", now.Sub(firstSeen)))
	}
	if rm != nil {
		if err := rm.RemoveEmptyDir(tctx, container, prefix); err != nil {
			// The files are gone but the folder stayed (on Sync a bridge
			// that still lists them answers "not empty"): the sighting stays,
			// marked erased, and a later pass removes the folder.
			if !errors.Is(err, drivers.ErrDirNotEmpty) {
				w.res.Errors = append(w.res.Errors, fmt.Sprintf("reconcile: remove %s on %s: %v", prefix, w.leg, err))
			}
			if len(files) > 0 && !alreadyErased {
				if _, err := p.db.ExecContext(ctx, `UPDATE vault_parity_orphans SET first_seen = $4 WHERE tenant_id = $1 AND leg = $2 AND prefix = $3`,
					tenantID, w.leg, prefix, sightingErased); err != nil {
					w.res.Errors = append(w.res.Errors, "reconcile: sighting of "+prefix+": "+err.Error())
				}
			}
			return false
		}
		w.res.OrphanFoldersRemoved++
	}
	p.forget(ctx, w.res, tenantID, w.leg, []string{prefix})
	return true
}

// namedPrefixes is the shard folders the tenant's rows name.
func (p *VaultParity) namedPrefixes(ctx context.Context, tenantID string) (map[string]bool, error) {
	rows, err := p.db.QueryContext(ctx, `SELECT shard_prefix FROM vault_parity WHERE tenant_id = $1`, tenantID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	named := map[string]bool{}
	for rows.Next() {
		var prefix string
		if err := rows.Scan(&prefix); err != nil {
			return nil, err
		}
		named[prefix] = true
	}
	return named, rows.Err()
}

// sightings is the tenant's sightings on the leg (prefix → first_seen).
func (p *VaultParity) sightings(ctx context.Context, tenantID, leg string) (map[string]time.Time, error) {
	rows, err := p.db.QueryContext(ctx, `SELECT prefix, first_seen FROM vault_parity_orphans WHERE tenant_id = $1 AND leg = $2`, tenantID, leg)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := map[string]time.Time{}
	for rows.Next() {
		var prefix string
		var first time.Time
		if err := rows.Scan(&prefix, &first); err != nil {
			return nil, err
		}
		out[prefix] = first
	}
	return out, rows.Err()
}

func (p *VaultParity) forget(ctx context.Context, res *VaultParityResult, tenantID, leg string, prefixes []string) {
	if len(prefixes) == 0 {
		return
	}
	if _, err := p.db.ExecContext(ctx, `
		DELETE FROM vault_parity_orphans WHERE tenant_id = $1 AND leg = $2 AND prefix = ANY($3)`,
		tenantID, leg, pq.Array(prefixes)); err != nil {
		res.Errors = append(res.Errors, "reconcile: forget sightings: "+err.Error())
	}
}
