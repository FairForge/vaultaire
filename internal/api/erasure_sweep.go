package api

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/FairForge/vaultaire/internal/account"
	"github.com/FairForge/vaultaire/internal/common"
	"github.com/FairForge/vaultaire/internal/engine"
	"github.com/FairForge/vaultaire/internal/tenant"
	"github.com/prometheus/client_golang/prometheus"
	"go.uber.org/zap"
)

// The erasure sweep (WP-R10-3c; post-merge finding PM-3 of the deletion
// runner): the sweep stage of an account erasure, after the head-row walk and the
// multipart abort, before account.EraseRows.
//
// The walk deletes what object_head_cache lists. Bytes with no head row are
// out of its reach, and EraseRows then removes the last rows that named them:
//
//   - a blob behind a delete marker on a versioning-enabled bucket (DELETE
//     writes the marker, drops the head row and leaves the blob — it is what
//     GET ?versionId serves);
//   - a blob whose version row went with DELETE ?versionId;
//   - a displaced blob whose delete failed or was skipped (WP-R13-2 leaves an
//     orphan rather than hold a request on a dead backend);
//   - a cold or hot copy with no ledger row;
//   - bytes on a backend the row did not name.
//
// So the sweep does not ask the tables where the bytes are. It asks every
// REGISTERED backend what it holds for the tenant and deletes it:
//
//   - a driver that implements engine.TenantWalker lists everything under
//     the tenant's own prefix in one walk — every container, including ones
//     no table remembers — and each object is removed by the exact key the
//     listing returned;
//   - any other driver (the plain `s3` driver, permafrost) is asked, bucket
//     by bucket, for List(<tenant>_<bucket>) and each name is deleted through
//     the driver (deleteCopyOn — never engine.Delete, which falls back to the
//     primary).
//
// What it never touches: the shared chunk container. On a fixed-bucket
// backend the chunk blobs written during a tenant's requests sit under that
// tenant's prefix (`t-<tenant>/_global/…`); they are the content index's,
// released by the manifest path and dedup GC's to collect. The walk reports
// them, the sweep counts them (ChunkBlobsLeft) and leaves them. (That GC
// does not find them under a tenant's prefix is WP-R8-7, not the sweep's to
// paper over: another tenant's manifest may name the same chunk.)
//
// What it cannot reach: a backend with no registered driver. Rows that name
// one are listed in the erasure record (UnsweptBackends) and the log.
//
// Failure is not partial success: a listing or a delete that fails (a miss
// is not a failure), a backend whose circuit breaker is open, a run cut by a
// shutdown or the tenant deadline — each DEFERS the tenant. The account is
// not reported erased while bytes may remain; the job retries hourly and
// the sweep starts over (it keeps no cursor: what is gone is not listed).
//
// It runs in the job, never in a request: these calls go straight to a
// backend's driver, with no circuit breaker in front of them, and are bounded
// by a deadline per call.

// The sweep's bounds (fields of the runner so a test can shrink them).
const (
	// defaultSweepDeleteTimeout bounds one delete: a backend that hangs
	// fails the call, the failure is counted, and SweepMaxFailures ends the
	// backend.
	defaultSweepDeleteTimeout = 30 * time.Second
	// defaultSweepListTimeout bounds one List of one container on a driver
	// without a tenant walk (the walk bounds each of its own pages).
	defaultSweepListTimeout = 5 * time.Minute
	// defaultSweepMaxFailures stops a backend after this many failed
	// deletes: a dead backend must not cost the run one timeout per object.
	defaultSweepMaxFailures = 25
	// defaultSweepCancelEvery is the number of objects between two re-reads
	// of the user row (a cancel wins between them).
	defaultSweepCancelEvery = 500
)

// accountDeletionSwept counts what the sweep found. `shared_left` is a chunk
// blob under the tenant's prefix it did not touch.
var accountDeletionSwept = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "vaultaire_account_deletion_swept_total",
	Help: "Blobs the account-deletion sweep found with no head row, by backend and result (deleted, failed, shared_left).",
}, []string{"backend", "result"})

// sweepTenantIDPattern is what a tenant id must look like before anything is
// listed under it. Registration mints `tenant-<hex>`; tests use UUIDs. The
// characters refused are the ones that are separators somewhere: '/' ends a
// key segment (tenant `a/b` would be container `b` of tenant `a`), '_' ends
// the tenant part of a container name (`a_b` + bucket `c` is the container of
// tenant `a` + bucket `b_c`), and "" collapses every prefix.
var sweepTenantIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]*$`)

// errSweepCancelled is returned through a walk when the user cancelled.
var errSweepCancelled = errors.New("deletion cancelled during the sweep")

// sweepBucketSources is every table that records a bucket name of the
// tenant. The union is the list of containers swept on a driver that cannot
// walk a tenant. TestSweepBucketSourcesCoverSchema walks the schema: a new
// tenant table with a bucket column must be listed here or in
// sweepBucketTablesExcluded.
var sweepBucketSources = []struct{ table, query string }{
	{"buckets", `SELECT name FROM buckets WHERE tenant_id = $1`},
	{"object_head_cache", `SELECT DISTINCT bucket FROM object_head_cache WHERE tenant_id = $1`},
	{"object_versions", `SELECT DISTINCT bucket FROM object_versions WHERE tenant_id = $1`},
	{"multipart_uploads", `SELECT DISTINCT bucket FROM multipart_uploads WHERE tenant_id = $1`},
	{"smart_demotions", `SELECT DISTINCT bucket FROM smart_demotions WHERE tenant_id = $1`},
	{"object_locks", `SELECT DISTINCT bucket FROM object_locks WHERE tenant_id = $1`},
	{"object_metadata", `SELECT DISTINCT bucket_name FROM object_metadata WHERE tenant_id::text = $1`},
	{"tenant_chunk_refs", `SELECT DISTINCT bucket_name FROM tenant_chunk_refs WHERE tenant_id::text = $1`},
	{"bucket_notifications", `SELECT DISTINCT bucket FROM bucket_notifications WHERE tenant_id = $1`},
	// object_locations.bucket is the CONTAINER the engine wrote to
	// (`<tenant>_<bucket>`, or `_global` for a chunk): only the tenant's own
	// containers count, with the tenant part taken off.
	{"object_locations", `SELECT DISTINCT substr(bucket, length($1) + 2) FROM object_locations
	                       WHERE tenant_id = $1 AND left(bucket, length($1) + 1) = $1 || '_'`},
}

// sweepBucketTablesExcluded are tenant tables with a bucket-like column that
// are not a source, and why.
var sweepBucketTablesExcluded = map[string]string{
	"s3_access_log":      "a log of what requests named — any string, existing or not",
	"cdn_access_log":     "a log, purged after 2 days",
	"cdn_stats_daily":    "a rollup of the log",
	"abuse_reports":      "the reporter's claim, not the tenant's bucket list",
	"access_patterns":    "no writer (R6: the tiering path is inert)",
	"artifacts":          "no writer (R9 orphan table, D-12)",
	"change_history":     "no writer (D-12)",
	"retention_policies": "no writer (D-12)",
	"tiering_policies":   "no writer (R6: inert)",
}

// sweepPlan is what one tenant's sweep runs over, read before the rows are
// erased (the pass after the erase has no rows left to ask).
type sweepPlan struct {
	buckets      []string // every bucket name the tenant's rows remember
	backends     []string // registered drivers, sorted
	unregistered []string // backends the tenant's rows name that have no driver
}

// checkSweepTenant refuses a tenant id that cannot be swept safely, and one
// whose id is the start of ANOTHER tenant's id followed by a separator — the
// prefix of the first would then be a prefix of the second's containers.
func (r *AccountDeletionRunner) checkSweepTenant(ctx context.Context, tenantID string) error {
	if !sweepTenantIDPattern.MatchString(tenantID) {
		return fmt.Errorf("tenant id %q is not sweepable (only letters, digits and '-'): nothing is listed under it", tenantID)
	}
	var collides int
	err := r.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM tenants
		 WHERE id <> $1 AND left(id, length($1) + 1) IN ($1 || '_', $1 || '/')`, tenantID).Scan(&collides)
	if err != nil {
		return fmt.Errorf("check tenant id collisions: %w", err)
	}
	if collides > 0 {
		return fmt.Errorf("%d other tenant id(s) start with %q and a separator: a listing under this tenant could reach theirs", collides, tenantID)
	}
	return nil
}

// planSweep reads the tenant's bucket names and the backends its rows name.
func (r *AccountDeletionRunner) planSweep(ctx context.Context, tenantID string) (sweepPlan, error) {
	var plan sweepPlan
	ce, ok := r.eng.(*engine.CoreEngine)
	if !ok {
		return plan, errors.New("the engine does not expose its drivers: no backend can be swept")
	}
	if err := r.checkSweepTenant(ctx, tenantID); err != nil {
		return plan, err
	}
	plan.backends = ce.GetDriverNames()
	registered := map[string]bool{}
	for _, b := range plan.backends {
		registered[b] = true
	}

	seen := map[string]bool{}
	for _, src := range sweepBucketSources {
		names, err := queryStrings(ctx, r.db, src.query, tenantID)
		if err != nil {
			return plan, fmt.Errorf("bucket names from %s: %w", src.table, err)
		}
		for _, n := range names {
			if n == "" {
				// An empty bucket name would make the container `<tenant>_`:
				// the prefix of every container of the tenant.
				return plan, fmt.Errorf("%s holds an empty bucket name for the tenant: refusing to sweep", src.table)
			}
			if !seen[n] {
				seen[n] = true
				plan.buckets = append(plan.buckets, n)
			}
		}
	}
	sort.Strings(plan.buckets)

	named, err := queryStrings(ctx, r.db, `
		SELECT DISTINCT backend_name FROM object_versions WHERE tenant_id = $1 AND COALESCE(backend_name, '') <> ''
		UNION SELECT DISTINCT backend_name FROM object_locations WHERE tenant_id = $1 AND COALESCE(backend_name, '') <> ''
		UNION SELECT hot_backend FROM smart_demotions WHERE tenant_id = $1
		UNION SELECT cold_backend FROM smart_demotions WHERE tenant_id = $1`, tenantID)
	if err != nil {
		return plan, fmt.Errorf("backends named by the tenant's rows: %w", err)
	}
	for _, b := range named {
		if b != "" && !registered[b] {
			plan.unregistered = append(plan.unregistered, b)
		}
	}
	sort.Strings(plan.unregistered)
	return plan, nil
}

func queryStrings(ctx context.Context, db *sql.DB, query string, args ...any) ([]string, error) {
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var s sql.NullString
		if err := rows.Scan(&s); err != nil {
			return nil, err
		}
		out = append(out, s.String)
	}
	return out, rows.Err()
}

// sweepTenant deletes what the tenant still holds on every registered
// backend. stop=true: the user cancelled (checked between backends and every
// SweepCancelEvery objects; never when final). A non-nil error means
// bytes may remain — the caller defers the tenant.
//
// final is the pass AFTER account.EraseRows: the rows are gone, so nothing
// re-reads the user and nothing can defer; it exists for bytes a write that
// was in flight during the first pass put down afterwards.
func (r *AccountDeletionRunner) sweepTenant(ctx context.Context, d account.Due, plan sweepPlan, te *TenantErasure, log *zap.Logger, final bool) (bool, error) {
	ce, ok := r.eng.(*engine.CoreEngine)
	if !ok {
		return false, errors.New("the engine does not expose its drivers: no backend can be swept")
	}
	// Checked again here: this function is what lists and deletes, whoever
	// built the plan.
	if !sweepTenantIDPattern.MatchString(d.TenantID) {
		return false, fmt.Errorf("tenant id %q is not sweepable", d.TenantID)
	}
	if te.Swept == nil {
		te.Swept = map[string]int{}
	}
	s := &tenantSweep{r: r, ce: ce, d: d, te: te, log: log, final: final}

	var failed []string
	for _, backend := range plan.backends {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		if stop, err := s.cancelled(ctx); err != nil {
			return false, err
		} else if stop {
			return true, nil
		}
		drv, ok := ce.GetDriver(backend)
		if !ok {
			continue // unregistered since the plan was read
		}
		// An open breaker is a backend that is failing right now: do not
		// spend a timeout per object on it. Half-open is "try".
		if st := ce.GetFailoverStatus()[backend]; st == engine.StateOpen.String() {
			failed = append(failed, backend+": unavailable (circuit breaker open)")
			continue
		}
		var err error
		if walker, ok := drv.(engine.TenantWalker); ok {
			err = s.walk(ctx, backend, walker)
		} else {
			err = s.containers(ctx, backend, drv, plan.buckets)
		}
		switch {
		case errors.Is(err, errSweepCancelled):
			return true, nil
		case err != nil && ctx.Err() != nil:
			return false, ctx.Err()
		case err != nil:
			failed = append(failed, backend+": "+err.Error())
		}
	}
	if len(failed) > 0 {
		return false, errors.New(strings.Join(failed, "; "))
	}
	return false, nil
}

// tenantSweep is one pass over the backends for one tenant.
type tenantSweep struct {
	r     *AccountDeletionRunner
	ce    *engine.CoreEngine
	d     account.Due
	te    *TenantErasure
	log   *zap.Logger
	final bool

	sinceCheck int
}

// cancelled re-reads the user row: a cancel wins until EraseRows.
func (s *tenantSweep) cancelled(ctx context.Context) (bool, error) {
	if s.final {
		return false, nil
	}
	s.sinceCheck = 0
	still, err := s.r.account.StillDue(ctx, s.d.UserID, s.r.now())
	if err != nil {
		return false, fmt.Errorf("re-read user: %w", err)
	}
	return !still, nil
}

// tick is called once per object; every SweepCancelEvery it re-reads the
// user row.
func (s *tenantSweep) tick(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.sinceCheck++
	if s.sinceCheck < s.r.SweepCancelEvery {
		return nil
	}
	stop, err := s.cancelled(ctx)
	if err != nil {
		return err
	}
	if stop {
		return errSweepCancelled
	}
	return nil
}

// remove runs one delete under its own deadline and counts it. It returns an
// error only when the backend has failed too often to go on.
func (s *tenantSweep) remove(ctx context.Context, backend, container, artifact string, failures *int, rm func(context.Context) error) error {
	dctx, cancel := context.WithTimeout(ctx, s.r.SweepDeleteTimeout)
	err := rm(dctx)
	cancel()
	if err == nil {
		s.te.Swept[backend]++
		if s.final {
			s.te.SweptAfterErase++
		}
		accountDeletionSwept.WithLabelValues(backend, "deleted").Inc()
		return nil
	}
	*failures++
	s.te.SweepFailures++
	accountDeletionSwept.WithLabelValues(backend, "failed").Inc()
	s.log.Error("account deletion sweep: blob left in place", zap.String("backend", backend),
		zap.String("container", container), zap.String("key", artifact), zap.Error(err))
	if *failures >= s.r.SweepMaxFailures {
		return fmt.Errorf("%d deletes failed, the last: %w", *failures, err)
	}
	return nil
}

// walk sweeps a backend that can list the tenant: everything under the
// tenant's prefix except the shared chunk container.
func (s *tenantSweep) walk(ctx context.Context, backend string, walker engine.TenantWalker) error {
	failures := 0
	err := walker.WalkTenant(ctx, s.d.TenantID, func(o engine.TenantObject) error {
		if err := s.tick(ctx); err != nil {
			return err
		}
		if o.Container == chunkContainer {
			// Not the tenant's container: the content index owns these blobs
			// (another manifest may reference the same chunk); collecting
			// them is dedup GC's job.
			if !s.final { // the pass after the erase sees the same blobs again
				s.te.ChunkBlobsLeft++
				accountDeletionSwept.WithLabelValues(backend, "shared_left").Inc()
			}
			return nil
		}
		return s.remove(ctx, backend, o.Container, o.Artifact, &failures, o.Remove)
	})
	if err != nil {
		return err
	}
	if failures > 0 {
		return fmt.Errorf("%d deletes failed", failures)
	}
	return nil
}

// containers sweeps a backend that cannot list a tenant: List of each of the
// tenant's containers, Delete of each name it returned.
func (s *tenantSweep) containers(ctx context.Context, backend string, drv engine.Driver, buckets []string) error {
	t := &tenant.Tenant{ID: s.d.TenantID}
	tctx := common.WithTenantID(ctx, s.d.TenantID)
	failures := 0
	for _, bucket := range buckets {
		if bucket == "" {
			return errors.New("empty bucket name: refusing to list")
		}
		container := t.NamespaceContainer(bucket)
		if container == chunkContainer {
			return errors.New("a container resolved to the shared chunk container: refusing to list")
		}
		lctx, cancel := context.WithTimeout(tctx, s.r.SweepListTimeout)
		names, err := drv.List(lctx, container, "")
		cancel()
		if err != nil {
			// Never read as "nothing there": every driver answers an absent
			// container with an empty listing.
			return fmt.Errorf("list bucket %q: %w", bucket, err)
		}
		for _, name := range names {
			if err := s.tick(ctx); err != nil {
				return err
			}
			if name == "" {
				continue
			}
			err := s.remove(ctx, backend, container, name, &failures, func(dctx context.Context) error {
				return deleteCopyOn(dctx, s.ce, backend, s.d.TenantID, container, name)
			})
			if err != nil {
				return err
			}
		}
	}
	if failures > 0 {
		return fmt.Errorf("%d deletes failed", failures)
	}
	return nil
}
