package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/FairForge/vaultaire/internal/common"
	"github.com/FairForge/vaultaire/internal/crypto"
	"github.com/FairForge/vaultaire/internal/drivers"
	"github.com/FairForge/vaultaire/internal/engine"
	"github.com/lib/pq"
	"github.com/prometheus/client_golang/prometheus"
	"go.uber.org/zap"
)

// Routing truth (WP-R7-5; Review R7-10, R7-11, R3-03, R7-27).
//
// object_head_cache.backend_name is where an object's bytes are: GET and
// DELETE hint the engine with it, the demotion ledger, the deletion runner
// and the erasure sweep act on it. Nothing checked it. On prod (2026-10-02)
// 2,613 rows named `onedrive` — a registration key no driver has answered to
// since the fleet became `permafrost`; 451 named `local` for a DATA_PATH that
// holds ten files; 62 had no backend at all (multipart before R3-03); and
// 2,007 of the 2,037 `idrive` rows pointed at the reseller account that was
// replaced on 2026-09-21. HEAD answered 200 for every one of them.
//
// Three things, none of which writes:
//
//   - the BOOT CHECK (BootCheck): every distinct backend_name in
//     object_head_cache, smart_demotions and object_versions must be a
//     registered driver name — one Error line per unknown name, with the row
//     count — and two registered names must never share a store (the
//     invariant WP-R13-2 left as a comment: a stale copy is deleted "on the
//     backend the key does not route to", which must not be the same bucket
//     under another name). It never blocks or fails boot.
//   - the SAMPLED JOB (`routing_truth`, daily 05:30 UTC on the one scheduler):
//     N head rows per registered backend, each asked of the RECORDED backend
//     only (engine.ExistsOn — never the fan-out, which would find the bytes
//     wherever they are and hide the drift). A chunked row is checked chunk
//     by chunk through the chunk store at the one address. Counted per
//     outcome: present, missing (bytes gone, row still there), changed (the
//     row was overwritten, moved or deleted while we looked — re-read on
//     every miss, so an erasure or a demotion mid-run is not a miss), error
//     (the backend could not be asked; an open breaker skips the backend
//     with ONE error, never N misses), unknown_backend (rows on a name no
//     driver has — never missing: nobody was asked). The result is written
//     to job_runs.result (migration 074) and read back from there by the
//     collector, the admin API and the dashboard — right after a restart too.
//   - the READ COUNTER (noteRecordedBackend): a GET, DELETE, copy, restore or
//     CDN read of a row whose backend is not registered is counted; the
//     request itself keeps today's behaviour (the engine's fan-out, WP-R6-1
//     removes it).
//
// What the operator does with the findings is a plan, not a job:
// docs/reviews/WP-R7-5.md and cmd/tools/routing-truth.

const routingTruthJobName = "routing_truth"

// routingExistsTimeout bounds one Exists call of the job.
const routingExistsTimeout = 30 * time.Second

// routingErrorCutoff: a backend whose calls fail this many times in a row is
// given up for the run (the rest of its sample is not asked) — the job must
// not thunder at a sick backend.
const routingErrorCutoff = 10

// routingChunkCap bounds the chunk HEADs of one run.
const routingChunkCap = 2000

var (
	routingTruthChecks = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "vaultaire_routing_truth_checks_total",
		Help: "Head rows the routing_truth job classified, by recorded backend and result: present, missing (bytes gone), changed (row changed mid-check), error (backend could not be asked), unknown_backend (no driver has the name; counted, never asked).",
	}, []string{"backend", "result"})
	routingChunkChecks = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "vaultaire_routing_truth_chunk_checks_total",
		Help: "Chunks of sampled chunked objects the routing_truth job checked at the one address, by backend and result: present, legacy (found only under the uploader's prefix — run the chunk move), missing, error, unknown_backend.",
	}, []string{"backend", "result"})
	routingUnknownReads = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "vaultaire_routing_unknown_backend_reads_total",
		Help: "Requests that read a head row whose recorded backend is not a registered driver (backend=\"\" is a row with no backend): the request fell back to the engine's fan-out.",
	}, []string{"op", "backend"})
)

// routingResults are the counter's result labels, created at 0 per backend.
var routingResults = []string{"present", "missing", "changed", "error"}

// RoutingTruthChecker is the boot check and the sampled job.
type RoutingTruthChecker struct {
	db     *sql.DB
	eng    *engine.CoreEngine
	gci    *crypto.GlobalContentIndex
	chunks *chunkStore
	logger *zap.Logger

	// Sample is the number of whole-object head rows asked per backend and
	// run (ROUTING_TRUTH_SAMPLE, default 300).
	Sample int
	// PermafrostSample is the sample on the OneDrive fleet, whose Exists
	// probes every account (ROUTING_TRUTH_PERMAFROST_SAMPLE, default 20).
	PermafrostSample int
	// ChunkedSample is the number of chunked objects whose chunks are
	// checked per run (ROUTING_TRUTH_CHUNKED_SAMPLE, default 20).
	ChunkedSample int
	// JobName is the job_runs name (tests use their own: the table is shared).
	JobName string

	// scopeTenant confines every query to one tenant (tests: the database
	// is shared and the job walks every tenant's rows).
	scopeTenant string
	// beforeExists is a test hook called before each whole-object check.
	beforeExists func(headRef)

	bootMu sync.Mutex
	boot   *RoutingBootReport
}

// NewRoutingTruthChecker builds the checker; nil without a database or an
// engine (nothing to check).
func NewRoutingTruthChecker(db *sql.DB, eng *engine.CoreEngine, gci *crypto.GlobalContentIndex, logger *zap.Logger) *RoutingTruthChecker {
	if db == nil || eng == nil {
		return nil
	}
	return &RoutingTruthChecker{
		db: db, eng: eng, gci: gci, logger: logger,
		chunks:           newChunkStore(eng, db, logger),
		Sample:           300,
		PermafrostSample: 20,
		ChunkedSample:    20,
		JobName:          routingTruthJobName,
	}
}

// headRef names one whole-object head row.
type headRef struct {
	tenantID, bucket, key, etag string
}

// --- the result ---------------------------------------------------------------

// BackendTruth is one registered backend's part of a run.
type BackendTruth struct {
	// Rows is how many whole-object head rows name the backend.
	Rows int64 `json:"rows"`
	// Sampled is how many of them were asked.
	Sampled int `json:"sampled"`
	Present int `json:"present"`
	// Missing: the backend says the bytes are not there and the row is
	// still exactly as read (same backend, same etag).
	Missing int `json:"missing"`
	// Changed: the backend missed, and the row had been overwritten, moved
	// or deleted meanwhile — not a miss.
	Changed int `json:"changed"`
	// Errors: the backend could not be asked (driver error, open breaker,
	// deadline). Never a miss.
	Errors int `json:"errors"`
	// MissingRatio is Missing over the rows with a verdict (present +
	// missing); 0 when none.
	MissingRatio float64 `json:"missing_ratio"`
	// Skipped says why the backend was not (fully) sampled: an open breaker,
	// too many consecutive errors.
	Skipped string `json:"skipped,omitempty"`
}

// ChunkTruth is the chunked part of a run.
type ChunkTruth struct {
	Objects int `json:"objects"`
	Chunks  int `json:"chunks"`
	Present int `json:"present"`
	// Legacy chunks are present — under the uploader's prefix, where blobs
	// were written before WP-R8-7; the chunk move brings them over.
	Legacy  int `json:"legacy"`
	Missing int `json:"missing"`
	Errors  int `json:"errors"`
	// UnknownBackend: the index row names a backend no driver has.
	UnknownBackend int `json:"unknown_backend"`
}

// RoutingTruthResult is one run, as written to job_runs.result.
type RoutingTruthResult struct {
	StartedAt  time.Time `json:"started_at"`
	FinishedAt time.Time `json:"finished_at"`
	// Backends: every registered backend, by name.
	Backends map[string]*BackendTruth `json:"backends"`
	// Unknown: head rows on a name no driver has ("" = NULL), by name.
	Unknown map[string]int64 `json:"unknown"`
	Chunks  ChunkTruth       `json:"chunks"`
	// Checks is the number of calls made (whole rows + chunks).
	Checks int64 `json:"checks"`
	// Summary is the one-line note (job_runs.last_error on an ok run, the
	// System page).
	Summary string `json:"summary"`
}

// unknownRows is one (table, backend) pair with no registered driver.
type unknownRows struct {
	Table   string `json:"table"`
	Backend string `json:"backend"` // "" for NULL
	Rows    int64  `json:"rows"`
}

// RoutingBootReport is what the boot check found.
type RoutingBootReport struct {
	At           time.Time            `json:"at"`
	Backends     []string             `json:"registered_backends"`
	Unknown      []unknownRows        `json:"unknown_backend_rows"`
	SharedStores []engine.SharedStore `json:"shared_stores"`
}

// --- the boot check ------------------------------------------------------------

// BootCheck reads the three routing tables and the engine's registry and
// reports what does not match. It logs one Error per finding; it never
// fails boot (an error here is a database that could not be read).
func (c *RoutingTruthChecker) BootCheck(ctx context.Context) (RoutingBootReport, error) {
	rep := RoutingBootReport{At: time.Now().UTC(), Backends: c.eng.GetDriverNames()}
	unknown, err := c.unknownBackendRows(ctx)
	if err != nil {
		return rep, err
	}
	rep.Unknown = unknown
	rep.SharedStores = c.eng.SharedStores()
	if rep.Unknown == nil {
		rep.Unknown = []unknownRows{}
	}
	if rep.SharedStores == nil {
		rep.SharedStores = []engine.SharedStore{}
	}

	for _, u := range unknown {
		name := u.Backend
		if name == "" {
			name = "<NULL>"
		}
		c.logger.Error("routing truth: rows name a backend no driver is registered under — their objects are served only by the engine's fan-out (WP-R6-1 removes it); see docs/reviews/WP-R7-5.md",
			zap.String("table", u.Table), zap.String("backend", name), zap.Int64("rows", u.Rows),
			zap.Strings("registered", rep.Backends))
	}
	for _, s := range rep.SharedStores {
		c.logger.Error("routing truth: two registered backends share one store — a stale-copy delete on one removes the other's live object (WP-R13-2 invariant)",
			zap.Strings("backends", s.Backends), zap.String("store", s.Store))
	}
	c.bootMu.Lock()
	c.boot = &rep
	c.bootMu.Unlock()
	return rep, nil
}

// bootReport is the last boot check (nil before it ran).
func (c *RoutingTruthChecker) bootReport() *RoutingBootReport {
	c.bootMu.Lock()
	defer c.bootMu.Unlock()
	return c.boot
}

// unknownBackendRows counts, per table, the rows whose backend is not a
// registered name (NULL included, as ""). smart_demotions counts its live
// rows (hot copy not yet reclaimed) on either column; object_versions its
// non-marker rows.
func (c *RoutingTruthChecker) unknownBackendRows(ctx context.Context) ([]unknownRows, error) {
	registered := pq.Array(c.eng.GetDriverNames())
	scope := c.scopeTenant
	type q struct{ table, sql string }
	queries := []q{
		{"object_head_cache", `SELECT COALESCE(backend_name, ''), COUNT(*) FROM object_head_cache
			WHERE ($2 = '' OR tenant_id = $2) AND (backend_name IS NULL OR NOT (backend_name = ANY($1::text[]))) GROUP BY 1`},
		{"smart_demotions", `SELECT b, COUNT(*) FROM (
			SELECT hot_backend AS b FROM smart_demotions WHERE hot_deleted_at IS NULL AND ($2 = '' OR tenant_id = $2)
			UNION ALL
			SELECT cold_backend FROM smart_demotions WHERE hot_deleted_at IS NULL AND ($2 = '' OR tenant_id = $2)) x
			WHERE NOT (b = ANY($1::text[])) GROUP BY 1`},
		{"object_versions", `SELECT COALESCE(backend_name, ''), COUNT(*) FROM object_versions
			WHERE NOT is_delete_marker AND ($2 = '' OR tenant_id = $2)
			  AND (backend_name IS NULL OR NOT (backend_name = ANY($1::text[]))) GROUP BY 1`},
	}
	var out []unknownRows
	for _, qq := range queries {
		rows, err := c.db.QueryContext(ctx, qq.sql, registered, scope)
		if err != nil {
			return nil, fmt.Errorf("routing truth: %s: %w", qq.table, err)
		}
		for rows.Next() {
			u := unknownRows{Table: qq.table}
			if err := rows.Scan(&u.Backend, &u.Rows); err != nil {
				_ = rows.Close()
				return nil, fmt.Errorf("routing truth: scan %s: %w", qq.table, err)
			}
			out = append(out, u)
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("routing truth: %s: %w", qq.table, err)
		}
		_ = rows.Close()
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Table != out[j].Table {
			return out[i].Table < out[j].Table
		}
		return out[i].Backend < out[j].Backend
	})
	return out, nil
}

// --- the job --------------------------------------------------------------------

// spec is the job: daily 05:30 UTC (between account_deletion 04:30 and
// smart_demotion 06:30), a catch-up six minutes after boot, one hour at most.
// What fails a run: a query of the head table failed — nothing was checked.
// A backend that could not be asked is a note.
func (c *RoutingTruthChecker) spec() jobSpec {
	return jobSpec{
		Name: c.JobName, Hour: 5, Minute: 30,
		BootDelay: 6 * time.Minute, MaxRunTime: time.Hour,
		Run: func(ctx context.Context) (jobReport, error) {
			res, err := c.RunOnce(ctx)
			if err != nil {
				return jobReport{}, err
			}
			return jobReport{Rows: res.Checks, Note: res.Summary, Result: res}, nil
		},
	}
}

// initSeries creates every result series at 0 for every registered backend,
// so the first miss after a boot is an increase a rule can see.
func (c *RoutingTruthChecker) initSeries() {
	for _, b := range c.eng.GetDriverNames() {
		for _, r := range routingResults {
			routingTruthChecks.WithLabelValues(b, r)
			routingChunkChecks.WithLabelValues(b, r)
		}
		routingChunkChecks.WithLabelValues(b, "legacy")
	}
}

// RunOnce is one sampled pass. It reads; it never writes.
func (c *RoutingTruthChecker) RunOnce(ctx context.Context) (RoutingTruthResult, error) {
	res := RoutingTruthResult{StartedAt: time.Now().UTC(), Backends: map[string]*BackendTruth{}, Unknown: map[string]int64{}}

	// Rows on names no driver has: counted, never asked.
	unknown, err := c.unknownBackendRows(ctx)
	if err != nil {
		return res, err
	}
	for _, u := range unknown {
		if u.Table != "object_head_cache" {
			continue
		}
		res.Unknown[u.Backend] = u.Rows
		routingTruthChecks.WithLabelValues(u.Backend, "unknown_backend").Add(float64(u.Rows))
	}

	for _, backend := range c.eng.GetDriverNames() {
		bt := &BackendTruth{}
		res.Backends[backend] = bt
		if err := c.sampleBackend(ctx, backend, bt); err != nil {
			return res, err
		}
		res.Checks += int64(bt.Sampled)
	}

	if err := c.sampleChunked(ctx, &res); err != nil {
		return res, err
	}

	res.FinishedAt = time.Now().UTC()
	res.Summary = summarizeRoutingTruth(&res)
	return res, nil
}

// sampleBackend asks one backend about a random window of its rows.
func (c *RoutingTruthChecker) sampleBackend(ctx context.Context, backend string, bt *BackendTruth) error {
	if err := c.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM object_head_cache
		WHERE backend_name = $1 AND NOT is_chunked AND ($2 = '' OR tenant_id = $2)`, backend, c.scopeTenant).Scan(&bt.Rows); err != nil {
		return fmt.Errorf("routing truth: count %s: %w", backend, err)
	}
	if bt.Rows == 0 {
		return nil
	}
	// An open breaker: the backend is down for everyone; one error for the
	// run, no HEADs (the failover would answer every one "unavailable" and a
	// bare reading of that would be a thousand misses).
	if st := c.eng.GetFailoverStatus()[backend]; st == engine.StateOpen.String() {
		bt.Errors = 1
		bt.Skipped = "circuit breaker open: not asked this run"
		routingTruthChecks.WithLabelValues(backend, "error").Inc()
		return nil
	}

	n := c.sampleSize(backend)
	offset := int64(0)
	if bt.Rows > int64(n) {
		offset = rand.Int64N(bt.Rows - int64(n) + 1) // #nosec G404 — a sampling window, not a secret
	}
	rows, err := c.db.QueryContext(ctx, `SELECT tenant_id, bucket, object_key, COALESCE(etag, '') FROM object_head_cache
		WHERE backend_name = $1 AND NOT is_chunked AND ($2 = '' OR tenant_id = $2)
		ORDER BY tenant_id, bucket, object_key OFFSET $3 LIMIT $4`, backend, c.scopeTenant, offset, n)
	if err != nil {
		return fmt.Errorf("routing truth: sample %s: %w", backend, err)
	}
	var refs []headRef
	for rows.Next() {
		var h headRef
		if err := rows.Scan(&h.tenantID, &h.bucket, &h.key, &h.etag); err != nil {
			_ = rows.Close()
			return fmt.Errorf("routing truth: scan %s: %w", backend, err)
		}
		refs = append(refs, h)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return fmt.Errorf("routing truth: sample %s: %w", backend, err)
	}
	_ = rows.Close()

	consecutive := 0
	for _, h := range refs {
		if ctx.Err() != nil {
			bt.Skipped = "run cut short: " + ctx.Err().Error()
			break
		}
		if consecutive >= routingErrorCutoff {
			bt.Skipped = fmt.Sprintf("%d consecutive errors: the rest of the sample was not asked", consecutive)
			break
		}
		if c.beforeExists != nil {
			c.beforeExists(h)
		}
		bt.Sampled++
		result := c.checkWhole(ctx, backend, h)
		routingTruthChecks.WithLabelValues(backend, result).Inc()
		switch result {
		case "present":
			bt.Present++
			consecutive = 0
		case "missing":
			bt.Missing++
			consecutive = 0
		case "changed":
			bt.Changed++
			consecutive = 0
		default:
			bt.Errors++
			consecutive++
		}
	}
	if verdicts := bt.Present + bt.Missing; verdicts > 0 {
		bt.MissingRatio = float64(bt.Missing) / float64(verdicts)
	}
	return nil
}

// sampleSize is the rows asked of one backend: fewer on the fleet.
func (c *RoutingTruthChecker) sampleSize(backend string) int {
	if backend == "permafrost" {
		return c.PermafrostSample
	}
	if d, ok := c.eng.GetDriver(backend); ok {
		if _, fleet := d.(*drivers.OneDriveDriver); fleet {
			return c.PermafrostSample
		}
	}
	return c.Sample
}

// checkWhole asks the recorded backend, with the row's tenant in the
// context, whether the object's bytes are there.
//
// The driver is called directly, not through the engine's failover: the job
// OBSERVES the backend's circuit breaker (an open one skips the backend) but
// never charges it. Through engine.ExistsOn five failing HEADs would open the
// primary's breaker for 30 s, and every customer PUT in that window would
// fail over to the next durable backend — a diagnostic must not move
// customer bytes (prod's primary key answers 403 to every GET/HEAD today,
// see docs/reviews/WP-R7-5.md). The sample is bounded by routingErrorCutoff.
func (c *RoutingTruthChecker) checkWhole(ctx context.Context, backend string, h headRef) string {
	d, ok := c.eng.GetDriver(backend)
	if !ok {
		return "error"
	}
	if st := c.eng.GetFailoverStatus()[backend]; st == engine.StateOpen.String() {
		return "error"
	}
	cctx, cancel := context.WithTimeout(common.WithTenantID(ctx, h.tenantID), routingExistsTimeout)
	defer cancel()
	container := h.tenantID + "_" + h.bucket
	ok, err := d.Exists(cctx, container, h.key)
	if err != nil {
		c.logger.Warn("routing truth: backend could not be asked", zap.String("backend", backend),
			zap.String("tenant_id", h.tenantID), zap.String("bucket", h.bucket), zap.String("key", h.key), zap.Error(err))
		return "error"
	}
	if ok {
		return "present"
	}
	// A miss: is the row still what we read? An overwrite, a demotion, a
	// delete or an erasure that landed meanwhile is not drift.
	var one int
	rerr := c.db.QueryRowContext(ctx, `SELECT 1 FROM object_head_cache
		WHERE tenant_id = $1 AND bucket = $2 AND object_key = $3 AND backend_name = $4 AND COALESCE(etag, '') = $5 AND NOT is_chunked`,
		h.tenantID, h.bucket, h.key, backend, h.etag).Scan(&one)
	if errors.Is(rerr, sql.ErrNoRows) {
		return "changed"
	}
	if rerr != nil {
		c.logger.Warn("routing truth: re-read after a miss failed", zap.Error(rerr))
		return "error"
	}
	c.logger.Warn("routing truth: head row points at bytes the backend does not have",
		zap.String("backend", backend), zap.String("tenant_id", h.tenantID), zap.String("bucket", h.bucket), zap.String("key", h.key))
	return "missing"
}

// sampleChunked checks the chunks of a few chunked objects at the one
// address (WP-R8-7), through the chunk store — the same path a GET takes,
// legacy fallback included.
func (c *RoutingTruthChecker) sampleChunked(ctx context.Context, res *RoutingTruthResult) error {
	if c.gci == nil || c.ChunkedSample <= 0 {
		return nil
	}
	var total int64
	if err := c.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM object_head_cache WHERE is_chunked AND ($1 = '' OR tenant_id = $1)`, c.scopeTenant).Scan(&total); err != nil {
		return fmt.Errorf("routing truth: count chunked: %w", err)
	}
	if total == 0 {
		return nil
	}
	offset := int64(0)
	if total > int64(c.ChunkedSample) {
		offset = rand.Int64N(total - int64(c.ChunkedSample) + 1) // #nosec G404 — a sampling window
	}
	rows, err := c.db.QueryContext(ctx, `SELECT tenant_id, bucket, object_key FROM object_head_cache
		WHERE is_chunked AND ($1 = '' OR tenant_id = $1) ORDER BY tenant_id, bucket, object_key OFFSET $2 LIMIT $3`,
		c.scopeTenant, offset, c.ChunkedSample)
	if err != nil {
		return fmt.Errorf("routing truth: sample chunked: %w", err)
	}
	var objs []headRef
	for rows.Next() {
		var h headRef
		if err := rows.Scan(&h.tenantID, &h.bucket, &h.key); err != nil {
			_ = rows.Close()
			return fmt.Errorf("routing truth: scan chunked: %w", err)
		}
		objs = append(objs, h)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return fmt.Errorf("routing truth: sample chunked: %w", err)
	}
	_ = rows.Close()

	ct := &res.Chunks
	registered := map[string]bool{}
	for _, b := range c.eng.GetDriverNames() {
		registered[b] = true
	}
	consecutive := 0 // errors in a row: the chunk store goes through the engine's failover, so a dead backend is given up early
	for _, h := range objs {
		if ctx.Err() != nil || ct.Chunks >= routingChunkCap || consecutive >= routingErrorCutoff {
			break
		}
		ct.Objects++
		refs, err := c.gci.GetObjectChunks(ctx, h.tenantID, h.bucket, h.key)
		if err != nil {
			ct.Errors++
			c.logger.Warn("routing truth: manifest could not be read", zap.String("key", h.key), zap.Error(err))
			continue
		}
		for _, ref := range refs {
			if ctx.Err() != nil || ct.Chunks >= routingChunkCap || consecutive >= routingErrorCutoff {
				break
			}
			scope := ref.DedupScope
			if scope == "" {
				scope = crypto.GlobalDedupScope
			}
			lookup, lerr := c.gci.LookupChunk(ctx, scope, ref.PlaintextHash)
			if lerr != nil || lookup == nil || lookup.Entry == nil {
				ct.Chunks++
				ct.Missing++
				routingChunkChecks.WithLabelValues("", "missing").Inc()
				c.logger.Warn("routing truth: chunk missing from the index", zap.String("key", h.key), zap.String("hash", shortHash(ref.PlaintextHash)), zap.Error(lerr))
				continue
			}
			backend := lookup.Entry.BackendID
			key := lookup.Entry.StorageKey
			if key == "" {
				key = "_chunks/" + ref.PlaintextHash
			}
			ct.Chunks++
			res.Checks++
			if !registered[backend] {
				ct.UnknownBackend++
				routingChunkChecks.WithLabelValues(backend, "unknown_backend").Inc()
				continue
			}
			if st := c.eng.GetFailoverStatus()[backend]; st == engine.StateOpen.String() {
				ct.Errors++
				routingChunkChecks.WithLabelValues(backend, "error").Inc()
				continue
			}
			result := c.checkChunk(ctx, chunkAddr{scope: scope, hash: ref.PlaintextHash, backend: backend, key: key})
			routingChunkChecks.WithLabelValues(backend, result).Inc()
			switch result {
			case "present":
				ct.Present++
				consecutive = 0
			case "legacy":
				ct.Present++
				ct.Legacy++
				consecutive = 0
			case "missing":
				ct.Missing++
				consecutive = 0
				c.logger.Warn("routing truth: chunk blob is at no address", zap.String("backend", backend), zap.String("key", h.key), zap.String("hash", shortHash(ref.PlaintextHash)))
			default:
				ct.Errors++
				consecutive++
			}
		}
	}
	return nil
}

// checkChunk: the one address first, then the legacy address (a blob
// written before WP-R8-7 under its uploader's prefix is present — and the
// chunk move's work).
func (c *RoutingTruthChecker) checkChunk(ctx context.Context, a chunkAddr) string {
	cctx, cancel := context.WithTimeout(ctx, routingExistsTimeout)
	defer cancel()
	ok, err := c.chunks.exists(cctx, a)
	if err != nil {
		return "error"
	}
	if ok {
		return "present"
	}
	legacy, err := c.chunks.legacyCopyExists(cctx, a)
	if err != nil {
		return "error"
	}
	if legacy {
		return "legacy"
	}
	return "missing"
}

// summarizeRoutingTruth is the one-line note.
func summarizeRoutingTruth(res *RoutingTruthResult) string {
	var parts []string
	names := make([]string, 0, len(res.Backends))
	for b := range res.Backends {
		names = append(names, b)
	}
	sort.Strings(names)
	for _, b := range names {
		bt := res.Backends[b]
		if bt.Rows == 0 {
			continue
		}
		p := fmt.Sprintf("%s: %d of %d rows sampled, %d present, %d missing", b, bt.Sampled, bt.Rows, bt.Present, bt.Missing)
		if bt.Changed > 0 {
			p += fmt.Sprintf(", %d changed", bt.Changed)
		}
		if bt.Errors > 0 {
			p += fmt.Sprintf(", %d error", bt.Errors)
		}
		if bt.Skipped != "" {
			p += " (" + bt.Skipped + ")"
		}
		parts = append(parts, p)
	}
	if len(res.Unknown) > 0 {
		keys := make([]string, 0, len(res.Unknown))
		for k := range res.Unknown {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		var u []string
		for _, k := range keys {
			name := k
			if name == "" {
				name = "(NULL)"
			}
			u = append(u, fmt.Sprintf("%s %d", name, res.Unknown[k]))
		}
		parts = append(parts, "rows on no registered backend: "+strings.Join(u, ", "))
	}
	if ct := res.Chunks; ct.Objects > 0 {
		p := fmt.Sprintf("chunks: %d object(s), %d chunk(s), %d present", ct.Objects, ct.Chunks, ct.Present)
		if ct.Legacy > 0 {
			p += fmt.Sprintf(" (%d at a legacy address: run the chunk move)", ct.Legacy)
		}
		p += fmt.Sprintf(", %d missing", ct.Missing)
		if ct.Errors > 0 {
			p += fmt.Sprintf(", %d error", ct.Errors)
		}
		if ct.UnknownBackend > 0 {
			p += fmt.Sprintf(", %d on an unregistered backend", ct.UnknownBackend)
		}
		parts = append(parts, p)
	}
	if len(parts) == 0 {
		return "no head rows"
	}
	return strings.Join(parts, "; ")
}

// --- the collector: everything read from the table -------------------------------

// routingLastRun is the last run as the collector exports it.
type routingLastRun struct {
	finishedAt   time.Time
	missingRatio map[string]float64
}

// lastRunGauges reads the last run from job_runs.result. Nothing in memory:
// a process that has just started reports the run of the process before it.
func (c *RoutingTruthChecker) lastRunGauges(ctx context.Context) routingLastRun {
	out := routingLastRun{missingRatio: map[string]float64{}}
	var raw []byte
	err := c.db.QueryRowContext(ctx, `SELECT result FROM job_runs WHERE job = $1`, c.JobName).Scan(&raw)
	if err != nil || len(raw) == 0 {
		return out
	}
	var res RoutingTruthResult
	if json.Unmarshal(raw, &res) != nil {
		return out
	}
	out.finishedAt = res.FinishedAt
	for b, bt := range res.Backends {
		if bt.Sampled > 0 && bt.Present+bt.Missing > 0 {
			out.missingRatio[b] = bt.MissingRatio
		}
	}
	return out
}

// routingCollectorTTL is how long the collector trusts one read of the
// tables.
const routingCollectorTTL = 5 * time.Minute

// routingCollector exports the routing gauges from the TABLES, never from a
// value set in this process:
//
//	vaultaire_routing_unknown_backend_rows{table,backend}   rows on a name no driver has ("" = NULL)
//	vaultaire_routing_shared_store_backends                   registered names that share a store with another
//	vaultaire_routing_truth_last_run_missing_ratio{backend} the last run's missing ratio (job_runs.result)
//	vaultaire_routing_truth_last_run_timestamp_seconds        when the last run finished
type routingCollector struct {
	c *RoutingTruthChecker

	unknownDesc, sharedDesc, ratioDesc, lastDesc *prometheus.Desc

	mu      sync.Mutex
	readAt  time.Time
	unknown []unknownRows
	last    routingLastRun
	ok      bool
}

func newRoutingCollector(c *RoutingTruthChecker) *routingCollector {
	return &routingCollector{
		c: c,
		unknownDesc: prometheus.NewDesc("vaultaire_routing_unknown_backend_rows",
			"Rows whose recorded backend is not a registered driver name (backend=\"\" is NULL), per table; read from the database.", []string{"table", "backend"}, nil),
		sharedDesc: prometheus.NewDesc("vaultaire_routing_shared_store_backends",
			"Registered backend names that share a store (same endpoint and bucket, or the same driver registered twice) with another name. Must be 0.", nil, nil),
		ratioDesc: prometheus.NewDesc("vaultaire_routing_truth_last_run_missing_ratio",
			"The routing_truth job's last run: rows whose bytes the recorded backend does not have, over rows with a verdict, per backend; read from job_runs.", []string{"backend"}, nil),
		lastDesc: prometheus.NewDesc("vaultaire_routing_truth_last_run_timestamp_seconds",
			"When the routing_truth job's last recorded run finished; read from job_runs.", nil, nil),
	}
}

func (r *routingCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- r.unknownDesc
	ch <- r.sharedDesc
	ch <- r.ratioDesc
	ch <- r.lastDesc
}

func (r *routingCollector) refresh() ([]unknownRows, routingLastRun, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if time.Since(r.readAt) < routingCollectorTTL {
		return r.unknown, r.last, r.ok
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	unknown, err := r.c.unknownBackendRows(ctx)
	r.readAt = time.Now()
	if err != nil {
		// Keep the previous read; a database blip must not blank the gauges
		// or stack scrapes.
		return r.unknown, r.last, r.ok
	}
	r.unknown, r.last, r.ok = unknown, r.c.lastRunGauges(ctx), true
	return r.unknown, r.last, r.ok
}

func (r *routingCollector) Collect(ch chan<- prometheus.Metric) {
	shared := 0
	for _, s := range r.c.eng.SharedStores() {
		shared += len(s.Backends)
	}
	ch <- prometheus.MustNewConstMetric(r.sharedDesc, prometheus.GaugeValue, float64(shared))

	unknown, last, ok := r.refresh()
	if !ok {
		return
	}
	for _, u := range unknown {
		ch <- prometheus.MustNewConstMetric(r.unknownDesc, prometheus.GaugeValue, float64(u.Rows), u.Table, u.Backend)
	}
	if last.finishedAt.IsZero() {
		return
	}
	ch <- prometheus.MustNewConstMetric(r.lastDesc, prometheus.GaugeValue, float64(last.finishedAt.Unix()))
	for b, ratio := range last.missingRatio {
		ch <- prometheus.MustNewConstMetric(r.ratioDesc, prometheus.GaugeValue, ratio, b)
	}
}

// --- the readers ------------------------------------------------------------------

// unknownReadLogged: one Warn per (op, backend), not one per request.
var unknownReadLogged sync.Map

// noteRecordedBackend is called by every reader of a head row's backend_name
// before it hints the engine: a name no driver is registered under is
// counted and logged once. The request goes on exactly as before (the
// engine's fan-out finds the bytes if any backend has them; WP-R6-1 removes
// that fallback, which is why these rows must be reconciled first).
func noteRecordedBackend(eng engine.Engine, logger *zap.Logger, op, backend string) {
	ce, ok := eng.(*engine.CoreEngine)
	if !ok || backend == "" {
		return
	}
	if _, registered := ce.GetDriver(backend); registered {
		return
	}
	routingUnknownReads.WithLabelValues(op, backend).Inc()
	if _, seen := unknownReadLogged.LoadOrStore(op+"/"+backend, struct{}{}); !seen && logger != nil {
		logger.Warn("routing truth: a request read a head row whose backend is not registered — served by the engine's fan-out, if at all (logged once per op and backend)",
			zap.String("op", op), zap.String("backend", backend))
	}
}
