package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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

// routingReadBytes is how much of an object the job READS after its HEAD
// said present: the first bytes, through the driver's ranged read where it
// has one (the S3-class drivers, Geyser), else a Get closed after them.
// 2026-10-21, Lyve us-east-1: HEAD 200 with the right size and ETag for
// chunk blobs whose every GET was a 500 InternalError for ~45 s — a HEAD
// alone cannot see that, and the job said "present" for every one of them
// (bench-results/LYVE-READ-FAULT-2026-10-21.md). A present row whose bytes
// cannot be read is "unreadable": its own verdict, never present, never
// missing.
const routingReadBytes = 4096

var (
	routingTruthChecks = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "vaultaire_routing_truth_checks_total",
		Help: "Head rows the routing_truth job classified, by recorded backend and result: present (HEAD and a read of the first bytes), unreadable (HEAD 200, the read failed), missing (bytes gone), changed (row changed mid-check), error (backend could not be asked), unknown_backend (no driver has the name; counted, never asked).",
	}, []string{"backend", "result"})
	routingChunkChecks = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "vaultaire_routing_truth_chunk_checks_total",
		Help: "Chunks of sampled chunked objects the routing_truth job checked at the one address, by backend and result: present (HEAD and a read of the first bytes), unreadable (HEAD 200, the read failed), legacy (found only under the uploader's prefix — run the chunk move), missing, error, unknown_backend.",
	}, []string{"backend", "result"})
	routingUnknownReads = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "vaultaire_routing_unknown_backend_reads_total",
		Help: "Requests that read a head row whose recorded backend is not a registered driver (backend=\"\" is a row with no backend): the request fell back to the engine's fan-out.",
	}, []string{"op", "backend"})
)

// routingResults are the counter's result labels, created at 0 per backend.
var routingResults = []string{"present", "unreadable", "missing", "changed", "error"}

// RoutingTruthChecker is the boot check and the sampled job.
type RoutingTruthChecker struct {
	db     *sql.DB
	eng    *engine.CoreEngine
	gci    *crypto.GlobalContentIndex
	chunks *chunkStore
	logger *zap.Logger
	// tenantKeys opens an edge-codec chunk (AES256-EW) in the chunked
	// sample (RP68-3 c); nil = such chunks are read, not opened.
	tenantKeys *crypto.TenantKeys

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
	size                        int64  // size_bytes: what the read probe may read (0 = nothing to read)
	storageKey                  string // the row's blob address ("" = the key, WP-R2-1)
	manifest                    string // a chunked row's manifest (WP-VM-1: its address, or the key)
}

// --- the result ---------------------------------------------------------------

// BackendTruth is one registered backend's part of a run.
type BackendTruth struct {
	// Rows is how many whole-object head rows name the backend.
	Rows int64 `json:"rows"`
	// Sampled is how many of them were asked.
	Sampled int `json:"sampled"`
	// Present: the backend has the bytes and served their first ones.
	Present int `json:"present"`
	// Unreadable: the backend says the bytes are there (HEAD 200) and
	// could not serve them (the read failed; 2026-10-21 Lyve). A verdict.
	Unreadable int `json:"unreadable"`
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
	// unreadable + missing); 0 when none. UnreadableRatio the same for
	// Unreadable.
	MissingRatio    float64 `json:"missing_ratio"`
	UnreadableRatio float64 `json:"unreadable_ratio"`
	// Skipped says why the backend was not (fully) sampled: an open breaker,
	// too many consecutive errors.
	Skipped string `json:"skipped,omitempty"`
}

// ChunkTruth is the chunked part of a run.
type ChunkTruth struct {
	Objects int `json:"objects"`
	Chunks  int `json:"chunks"`
	Present int `json:"present"`
	// Unreadable: at the one address by HEAD, and its read failed.
	Unreadable int `json:"unreadable"`
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
		// A chunked row's backend_name is not routing truth (its bytes are the
		// content index's — the chunk rows carry their own backend_id and the
		// job checks them chunk by chunk): every chunked write stamps the
		// sentinel 'chunked' (WP-R3-1); older rows carry the first stored
		// chunk's backend, or '' from a manifest copy.
		{"object_head_cache", `SELECT COALESCE(backend_name, ''), COUNT(*) FROM object_head_cache
			WHERE ($2 = '' OR tenant_id = $2) AND NOT is_chunked
			  AND (backend_name IS NULL OR NOT (backend_name = ANY($1::text[]))) GROUP BY 1`},
		{"smart_demotions", `SELECT b, COUNT(*) FROM (
			SELECT hot_backend AS b FROM smart_demotions WHERE hot_deleted_at IS NULL AND ($2 = '' OR tenant_id = $2)
			UNION ALL
			SELECT cold_backend FROM smart_demotions WHERE hot_deleted_at IS NULL AND ($2 = '' OR tenant_id = $2)) x
			WHERE NOT (b = ANY($1::text[])) GROUP BY 1`},
		// A promoted copy of a Sia object (sia_promotion.go) is read from
		// the backend its row names.
		{"sia_promotions", `SELECT promoted_backend, COUNT(*) FROM sia_promotions
			WHERE ($2 = '' OR tenant_id = $2) AND NOT (promoted_backend = ANY($1::text[])) GROUP BY 1`},
		// A chunked version names its manifest with the same sentinel
		// (WP-VM-1): checked chunk by chunk below, never a route.
		{"object_versions", `SELECT COALESCE(backend_name, ''), COUNT(*) FROM object_versions
			WHERE NOT is_delete_marker AND ($2 = '' OR tenant_id = $2)
			  AND backend_name IS DISTINCT FROM '` + chunkedBackendSentinel + `'
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
	rows, err := c.db.QueryContext(ctx, `SELECT tenant_id, bucket, object_key, COALESCE(etag, ''), COALESCE(storage_key, ''), size_bytes FROM object_head_cache
		WHERE backend_name = $1 AND NOT is_chunked AND ($2 = '' OR tenant_id = $2)
		ORDER BY tenant_id, bucket, object_key OFFSET $3 LIMIT $4`, backend, c.scopeTenant, offset, n)
	if err != nil {
		return fmt.Errorf("routing truth: sample %s: %w", backend, err)
	}
	var refs []headRef
	for rows.Next() {
		var h headRef
		if err := rows.Scan(&h.tenantID, &h.bucket, &h.key, &h.etag, &h.storageKey, &h.size); err != nil {
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
		case "unreadable":
			bt.Unreadable++
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
	if verdicts := bt.Present + bt.Unreadable + bt.Missing; verdicts > 0 {
		bt.MissingRatio = float64(bt.Missing) / float64(verdicts)
		bt.UnreadableRatio = float64(bt.Unreadable) / float64(verdicts)
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
// context, whether the object's bytes are there — at the row's address
// (storage_key, WP-R2-1; the key when it records none).
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
	container, blob := objectBlobAddress(h.tenantID, h.bucket, h.key, h.storageKey)
	ok, err := d.Exists(cctx, container, blob)
	if err != nil {
		c.logger.Warn("routing truth: backend could not be asked", zap.String("backend", backend),
			zap.String("tenant_id", h.tenantID), zap.String("bucket", h.bucket), zap.String("key", h.key), zap.Error(err))
		return "error"
	}
	if ok {
		verdict, rerr := readProbe(cctx, d, container, blob, h.size)
		switch verdict {
		case "present":
			return verdict
		case "unreadable":
			c.logger.Warn("routing truth: head row points at bytes the backend has but cannot serve",
				zap.String("backend", backend), zap.String("tenant_id", h.tenantID), zap.String("bucket", h.bucket), zap.String("key", h.key), zap.Error(rerr))
			return verdict
		case "error":
			c.logger.Warn("routing truth: backend could not be read", zap.String("backend", backend),
				zap.String("tenant_id", h.tenantID), zap.String("bucket", h.bucket), zap.String("key", h.key), zap.Error(rerr))
			return verdict
		}
		// "missing": the read answered a miss right after the HEAD — the
		// miss branch below tells a deleted/overwritten row from drift.
	}
	// A miss: is the row still what we read? An overwrite, a demotion, a
	// delete or an erasure that landed meanwhile is not drift.
	var one int
	rerr := c.db.QueryRowContext(ctx, `SELECT 1 FROM object_head_cache
		WHERE tenant_id = $1 AND bucket = $2 AND object_key = $3 AND backend_name = $4 AND COALESCE(etag, '') = $5 AND NOT is_chunked
		  AND COALESCE(storage_key, '') = $6`,
		h.tenantID, h.bucket, h.key, backend, h.etag, h.storageKey).Scan(&one)
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

// readProbe reads the first routingReadBytes of an object the backend has
// just said it holds — through the driver's ranged read where it has one
// (one request; a Geyser object on tape answers ErrArchived: present), else
// a Get closed after those bytes. The driver is called directly: the probe
// never charges a breaker. Verdicts: present (bytes served; an empty object
// has none to read), unreadable (the backend answered the HEAD and not the
// read: a 5xx, a body cut after the status line, an empty body), error (no
// answer within the deadline), missing (a miss right after the HEAD — the
// caller re-reads the row).
func readProbe(ctx context.Context, d engine.Driver, container, blob string, size int64) (string, error) {
	if size == 0 {
		return "present", nil
	}
	n := int64(routingReadBytes)
	if size > 0 && size < n {
		n = size
	}
	var rc io.ReadCloser
	var err error
	if rg, ok := d.(engine.RangeGetter); ok {
		rc, err = rg.GetRange(ctx, container, blob, 0, n)
	} else {
		rc, err = d.Get(ctx, container, blob)
	}
	if err != nil {
		return classifyReadError(err), err
	}
	defer func() { _ = rc.Close() }()
	got, rerr := io.CopyN(io.Discard, rc, n)
	if rerr != nil && !errors.Is(rerr, io.EOF) {
		return classifyReadError(rerr), rerr
	}
	if got == 0 {
		return "unreadable", errors.New("empty body for an object of recorded size")
	}
	return "present", nil
}

func classifyReadError(err error) string {
	switch {
	case errors.Is(err, engine.ErrArchived):
		return "present"
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, engine.ErrTimeout):
		return "error"
	case isObjectMissingErr(err):
		return "missing"
	}
	return "unreadable"
}

// chunkedManifestsSQL lists every manifest a row names (tenant $1, the
// empty string for all): the chunked head rows' and, since WP-VM-1, the
// chunked versions' (one per manifest: a head row's own version names the
// same one).
const chunkedManifestsSQL = `
	SELECT tenant_id, bucket, object_key, COALESCE(storage_key, object_key) AS manifest
	  FROM object_head_cache WHERE is_chunked AND ($1 = '' OR tenant_id = $1)
	UNION
	SELECT tenant_id, bucket, object_key, storage_key
	  FROM object_versions
	 WHERE backend_name = '` + chunkedBackendSentinel + `' AND storage_key IS NOT NULL AND NOT is_delete_marker
	   AND ($1 = '' OR tenant_id = $1)`

// sampleChunked checks the chunks of a few chunked objects — current and,
// since WP-VM-1, non-current versions, each through the manifest its row
// names — at the one address (WP-R8-7), through the chunk store: the same
// path a GET takes, legacy fallback included.
func (c *RoutingTruthChecker) sampleChunked(ctx context.Context, res *RoutingTruthResult) error {
	if c.gci == nil || c.ChunkedSample <= 0 {
		return nil
	}
	var total int64
	if err := c.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM (`+chunkedManifestsSQL+`) m`, c.scopeTenant).Scan(&total); err != nil {
		return fmt.Errorf("routing truth: count chunked: %w", err)
	}
	if total == 0 {
		return nil
	}
	offset := int64(0)
	if total > int64(c.ChunkedSample) {
		offset = rand.Int64N(total - int64(c.ChunkedSample) + 1) // #nosec G404 — a sampling window
	}
	rows, err := c.db.QueryContext(ctx, `SELECT tenant_id, bucket, object_key, manifest FROM (`+chunkedManifestsSQL+`) m
		ORDER BY tenant_id, bucket, object_key, manifest OFFSET $2 LIMIT $3`,
		c.scopeTenant, offset, c.ChunkedSample)
	if err != nil {
		return fmt.Errorf("routing truth: sample chunked: %w", err)
	}
	var objs []headRef
	for rows.Next() {
		var h headRef
		if err := rows.Scan(&h.tenantID, &h.bucket, &h.key, &h.manifest); err != nil {
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
		refs, err := c.gci.GetObjectChunks(ctx, h.tenantID, h.bucket, h.manifest)
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
				// One error for the run, and the chunk pass is over: the
				// backend is down for everyone (not 127 "errors" for 127
				// chunks nobody asked about — prod's first run).
				ct.Errors++
				routingChunkChecks.WithLabelValues(backend, "error").Inc()
				consecutive = routingErrorCutoff
				continue
			}
			result := c.checkChunk(ctx, chunkAddr{scope: scope, hash: ref.PlaintextHash, backend: backend, key: key})
			if result == "present" {
				result = c.openCodecChunk(ctx, scope, backend, key, ref, lookup.Entry)
			}
			routingChunkChecks.WithLabelValues(backend, result).Inc()
			switch result {
			case "present":
				ct.Present++
				consecutive = 0
			case "unreadable":
				ct.Unreadable++
				consecutive = 0
				c.logger.Warn("routing truth: chunk blob is at the one address and cannot be read", zap.String("backend", backend), zap.String("key", h.key), zap.String("hash", shortHash(ref.PlaintextHash)))
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

// checkChunk: the one address first, then the legacy addresses (a blob
// written before WP-R8-7 under its uploader's prefix is present — and the
// chunk move's work). The driver is called directly, like checkWhole: the
// chunk store's exists goes through engine.ExistsOn and charges the breaker
// — on prod's first run five 403s on chunk HEADs opened the primary's
// breaker, which the whole-object pass had been built not to do.
func (c *RoutingTruthChecker) checkChunk(ctx context.Context, a chunkAddr) string {
	d, ok := c.eng.GetDriver(a.backend)
	if !ok {
		return "error"
	}
	cctx, cancel := context.WithTimeout(ctx, routingExistsTimeout)
	defer cancel()
	present, err := d.Exists(engine.ChunkContext(cctx), chunkContainer, a.key)
	if err != nil {
		return "error"
	}
	if present {
		verdict, _ := readProbe(engine.ChunkContext(cctx), d, chunkContainer, a.key, -1)
		if verdict != "missing" {
			return verdict
		}
	}
	tenants, err := c.chunks.legacyTenants(cctx, a)
	if err != nil {
		return "error"
	}
	for _, tid := range tenants {
		legacy, err := d.Exists(engine.LegacyChunkContext(cctx, tid), chunkContainer, a.key)
		if err != nil {
			return "error"
		}
		if legacy {
			return "legacy"
		}
	}
	return "missing"
}

// openCodecChunk: an edge-codec chunk (AES256-EW, a 0x06 blob — WP-EDGE-2)
// is OPENED, not only read (RP68-3 c): its integrity rests on what the edge
// host uploaded, and a blob that does not hash to its row, does not open
// under its version or does not bind to its keyed hash is unreadable — found
// by the daily sample, not at a customer's read. Any other chunk keeps the
// verdict it has; so does every chunk when this process holds no tenant keys.
func (c *RoutingTruthChecker) openCodecChunk(ctx context.Context, scope, backend, key string, ref crypto.TenantChunkRef, e *crypto.GCIEntry) string {
	if c.tenantKeys == nil || e == nil || !e.Encrypted || e.EncryptionAlgo == nil || *e.EncryptionAlgo != crypto.EdgeChunkEncryptionAlgo {
		return "present"
	}
	d, ok := c.eng.GetDriver(backend)
	if !ok {
		return "error"
	}
	cctx, cancel := context.WithTimeout(ctx, routingExistsTimeout)
	defer cancel()
	rc, err := d.Get(engine.ChunkContext(cctx), chunkContainer, key)
	if err != nil {
		return "unreadable"
	}
	blob, err := io.ReadAll(io.LimitReader(rc, maxChunkBlobBytes+1))
	_ = rc.Close()
	if err != nil || len(blob) > maxChunkBlobBytes {
		return "unreadable"
	}
	desc := chunkDesc{scope: scope, storageKey: key, backendID: backend, plaintextHash: ref.PlaintextHash, size: e.SizeBytes,
		encrypted: true, encAlgo: crypto.EdgeChunkEncryptionAlgo}
	if e.CiphertextHash != nil {
		desc.ciphertextHash = *e.CiphertextHash
	}
	switch {
	case e.EncryptionKeyVersion != nil:
		desc.keyVersion = *e.EncryptionKeyVersion
	default:
		desc.keyVersion = ref.EncryptionKeyVersion
	}
	if _, err := openEdgeCodecBlob(cctx, c.tenantKeys, desc, scope, blob); err != nil {
		c.logger.Warn("routing truth: edge-codec chunk does not open", zap.String("backend", backend), zap.String("hash", shortHash(ref.PlaintextHash)), zap.Error(err))
		return "unreadable"
	}
	return "present"
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
		p := fmt.Sprintf("%s: %d of %d rows sampled, %d present, %d missing, %d unreadable", b, bt.Sampled, bt.Rows, bt.Present, bt.Missing, bt.Unreadable)
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
		p += fmt.Sprintf(", %d missing, %d unreadable", ct.Missing, ct.Unreadable)
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

// routingLastRun is the last run as the collector exports it: every
// backend of the result, whether or not the job reached a verdict on it.
type routingLastRun struct {
	finishedAt time.Time
	backends   map[string]BackendTruth
	chunks     ChunkTruth
}

// lastRunGauges reads the last run from job_runs.result. Nothing in memory:
// a process that has just started reports the run of the process before it.
func (c *RoutingTruthChecker) lastRunGauges(ctx context.Context) routingLastRun {
	out := routingLastRun{backends: map[string]BackendTruth{}}
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
	out.chunks = res.Chunks
	for b, bt := range res.Backends {
		if bt != nil {
			out.backends[b] = *bt
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
//	vaultaire_routing_unknown_backend_rows{table,backend}   rows on a name no driver has ("" = NULL; the NULL
//	                                                         count of object_head_cache and object_versions is a
//	                                                         series at 0 when there is none)
//	vaultaire_routing_shared_store_backends                   registered names that share a store with another
//	vaultaire_routing_truth_last_run_missing_ratio{backend} the last run's missing ratio (job_runs.result) —
//	                                                         for EVERY backend of the run, 0 without a verdict
//	vaultaire_routing_truth_last_run_rows{backend}            head rows on the backend at the run
//	vaultaire_routing_truth_last_run_verdicts{backend}        present + missing: rows the run decided on
//	vaultaire_routing_truth_last_run_errors{backend}          rows the backend could not be asked about
//	vaultaire_routing_truth_last_run_timestamp_seconds        when the last run finished
//
// RP14-12: the ratio used to be emitted only for a backend with a verdict,
// so on prod — one backend with rows, 10 errors, no verdict — no ratio
// series existed and the rules on it alerted on nothing. The rows and
// verdicts gauges say when a 0 ratio means "nothing missing" and when it
// means "nothing checked" (RoutingTruthNoVerdict).
type routingCollector struct {
	c *RoutingTruthChecker

	unknownDesc, sharedDesc, ratioDesc, rowsDesc, verdictsDesc, errorsDesc, lastDesc *prometheus.Desc
	unreadableDesc, chunksUnreadableDesc                                             *prometheus.Desc

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
			"The routing_truth job's last run: rows whose bytes the recorded backend does not have, over rows with a verdict, per backend of the run (0 when it reached no verdict — see _verdicts); read from job_runs.", []string{"backend"}, nil),
		rowsDesc: prometheus.NewDesc("vaultaire_routing_truth_last_run_rows",
			"The routing_truth job's last run: whole-object head rows naming the backend; read from job_runs.", []string{"backend"}, nil),
		verdictsDesc: prometheus.NewDesc("vaultaire_routing_truth_last_run_verdicts",
			"The routing_truth job's last run: sampled rows it reached a verdict on (present + missing), per backend; 0 with rows > 0 means the backend could not be checked; read from job_runs.", []string{"backend"}, nil),
		errorsDesc: prometheus.NewDesc("vaultaire_routing_truth_last_run_errors",
			"The routing_truth job's last run: sampled rows the backend could not be asked about (driver error, open breaker, deadline), per backend; read from job_runs.", []string{"backend"}, nil),
		unreadableDesc: prometheus.NewDesc("vaultaire_routing_truth_last_run_unreadable",
			"The routing_truth job's last run: sampled head rows the backend has (HEAD 200) and could not serve (the read of their first bytes failed), per backend; read from job_runs.", []string{"backend"}, nil),
		chunksUnreadableDesc: prometheus.NewDesc("vaultaire_routing_truth_last_run_chunks_unreadable",
			"The routing_truth job's last run: chunk blobs at the one address (HEAD 200) whose read failed; read from job_runs.", nil, nil),
		lastDesc: prometheus.NewDesc("vaultaire_routing_truth_last_run_timestamp_seconds",
			"When the routing_truth job's last recorded run finished; read from job_runs.", nil, nil),
	}
}

func (r *routingCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- r.unknownDesc
	ch <- r.sharedDesc
	ch <- r.ratioDesc
	ch <- r.rowsDesc
	ch <- r.verdictsDesc
	ch <- r.errorsDesc
	ch <- r.unreadableDesc
	ch <- r.chunksUnreadableDesc
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
	nullSeen := map[string]bool{}
	for _, u := range unknown {
		if u.Backend == "" {
			nullSeen[u.Table] = true
		}
		ch <- prometheus.MustNewConstMetric(r.unknownDesc, prometheus.GaugeValue, float64(u.Rows), u.Table, u.Backend)
	}
	// The NULL count of the two head tables is a series even at 0: the one
	// "unknown backend" every deployment can have (rows written before
	// backend_name existed), and the sign that the collector reads.
	for _, table := range []string{"object_head_cache", "object_versions"} {
		if !nullSeen[table] {
			ch <- prometheus.MustNewConstMetric(r.unknownDesc, prometheus.GaugeValue, 0, table, "")
		}
	}
	if last.finishedAt.IsZero() {
		return
	}
	ch <- prometheus.MustNewConstMetric(r.lastDesc, prometheus.GaugeValue, float64(last.finishedAt.Unix()))
	ch <- prometheus.MustNewConstMetric(r.chunksUnreadableDesc, prometheus.GaugeValue, float64(last.chunks.Unreadable))
	for b, bt := range last.backends {
		ch <- prometheus.MustNewConstMetric(r.ratioDesc, prometheus.GaugeValue, bt.MissingRatio, b)
		ch <- prometheus.MustNewConstMetric(r.rowsDesc, prometheus.GaugeValue, float64(bt.Rows), b)
		ch <- prometheus.MustNewConstMetric(r.verdictsDesc, prometheus.GaugeValue, float64(bt.Present+bt.Unreadable+bt.Missing), b)
		ch <- prometheus.MustNewConstMetric(r.errorsDesc, prometheus.GaugeValue, float64(bt.Errors), b)
		ch <- prometheus.MustNewConstMetric(r.unreadableDesc, prometheus.GaugeValue, float64(bt.Unreadable), b)
	}
}

// --- the readers ------------------------------------------------------------------

// unknownReadLogged: one Warn per (op, backend), not one per request.
var unknownReadLogged sync.Map

// noteRecordedBackend is called by every reader of a WHOLE object's head row
// backend_name before it names it to the engine (a chunked row's name is a
// sentinel — 'chunked' or ” — never a route): a name no driver is
// registered under is counted and logged once. Since WP-R6-1 the engine
// answers such a read or delete 503 (ErrBackendNotRegistered, an Error line
// per call) — the fan-out that used to find the bytes on whichever backend
// had a copy is gone; prod had 0 such rows on 2026-10-13 (routing_truth).
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
		logger.Warn("routing truth: a request read a head row whose backend is not registered — answered 503, never another backend's copy (logged once per op and backend)",
			zap.String("op", op), zap.String("backend", backend))
	}
}
