package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/FairForge/vaultaire/internal/audit"
	"github.com/FairForge/vaultaire/internal/engine"
	"github.com/lib/pq"
	"github.com/prometheus/client_golang/prometheus"
	"go.uber.org/zap"
)

// The chunk orphan sweep (WP-R8-2 B.2, Stage 1a Prompt 12): a chunk blob at
// the one address (engine.ChunkContext: `t-_global/_global/_chunks/<hash>`,
// `…/_chunks/<tenant>/<hash>`, `…/_chunks/<tenant>/<hash>.k<N>` on a
// fixed-bucket backend) that no global_content_index row names was found by
// nothing and paid for ever: a PUT the deadline cut after the backend had
// stored it (RP8-11, seen in the 2026-10-17 stall tests), a writer that died
// between its PUT and its row (since 093: a pending claim that was never made
// ready), a row dedup GC deleted whose blob delete failed.
//
// The `chunk_orphans` job, daily at 01:30 UTC (after inventory, before
// dedup_gc), on every registered backend that holds chunk rows or held them
// at the last run:
//
//  1. abandoned claims — pending rows older than the grace with no
//     reference: their blob (if any) and the row are erased, under the
//     chunk's lock, re-checked;
//  2. the listing — `_chunks/` at the one address, a page at a time
//     (engine.PageLister, through chunkStore.listPage), from the cursor of
//     the previous run (job_runs.result) and at most ListBudget keys per
//     backend per run; a key is an orphan when no row of any state names
//     (backend, key) — idx_gci_backend_storage_key, 085 — and no
//     retired_chunk_blobs row does (the re-encrypt collector's), and its
//     LastModified is older than the grace; it is erased under the chunk's
//     advisory lock after both tables are read again (a store that landed
//     meanwhile keeps it). A legacy address (a tenant's own prefix, before
//     WP-R8-7) is never listed: it is the chunk move's.
//
// The grace (CHUNK_ORPHAN_GRACE, 24 h) is measured on the blob's own age —
// the S3 LastModified the listing returns — and is longer than everything
// that can leave a blob briefly unnamed: the chunk PUT bound (90 s), the
// pending takeover window (3 min), the re-encrypt retire window (1 h) and a
// dedup GC run. A backend that cannot be asked (not registered, breaker open,
// no paged listing) is skipped with one Warn and named in not_swept, never
// a failed run.

const (
	chunkOrphansJobName = "chunk_orphans"

	defaultChunkOrphanGrace = 24 * time.Hour
	minChunkOrphanGrace     = time.Hour
	maxChunkOrphanGrace     = 7 * 24 * time.Hour

	// defaultChunkOrphanListBudget is the keys one run lists per backend
	// (prod 2026-10-18: lyve 24,399 rows, the largest): a run lists a whole
	// backend today; the cursor carries a larger one over several days.
	defaultChunkOrphanListBudget = 100_000
	defaultChunkOrphanPageSize   = 1000
	// chunkOrphanReportMax bounds a dry run's candidate list per backend.
	chunkOrphanReportMax = 1000
	// chunkOrphanPendingBatch bounds the abandoned claims one run erases per
	// backend.
	chunkOrphanPendingBatch = 1000

	// The guard: a run erases nothing on a backend where more than
	// max(chunkOrphanGuardFloor, chunkOrphanGuardFraction × listed) of the
	// keys it listed look unnamed. Orphans come from stalls and crashes —
	// tens, not a twentieth of a backend; that many is what a key-mapping
	// fault would look like (every live chunk unnamed), and the job is a
	// deleter. The dry run shows them; the guard is a note, never a failure.
	chunkOrphanGuardFloor    = 100
	chunkOrphanGuardFraction = 0.05

	// chunkKeyPrefix is the prefix of every chunk blob's key in the chunk
	// container.
	chunkKeyPrefix = "_chunks/"
)

// ChunkOrphanSweeper is the chunk_orphans job.
type ChunkOrphanSweeper struct {
	db     *sql.DB
	eng    *engine.CoreEngine
	chunks *chunkStore
	logger *zap.Logger

	// Grace is how old an unnamed blob (or an abandoned claim) must be.
	Grace time.Duration
	// ListBudget is the keys listed per backend per run; PageSize per page.
	ListBudget int
	PageSize   int
	// JobName is the job_runs name (tests use their own: the table is shared).
	JobName string

	now func() time.Time
	// onlyBackends limits a run to these backends (tests: other packages'
	// chunk rows are on the shared database).
	onlyBackends []string
	// beforeErase runs before an orphan's locked re-check (tests: the race).
	beforeErase func(chunkOrphan)
	// guardFloor overrides chunkOrphanGuardFloor (tests).
	guardFloor int
}

// NewChunkOrphanSweeper builds the job; CHUNK_ORPHAN_GRACE is read here.
func NewChunkOrphanSweeper(db *sql.DB, eng *engine.CoreEngine, logger *zap.Logger) *ChunkOrphanSweeper {
	if db == nil || eng == nil {
		return nil
	}
	if logger == nil {
		logger = zap.NewNop()
	}
	return &ChunkOrphanSweeper{
		db: db, eng: eng, chunks: newChunkStore(eng, db, logger), logger: logger,
		Grace:      chunkOrphanGraceFromEnv(os.Getenv, logger),
		ListBudget: defaultChunkOrphanListBudget, PageSize: defaultChunkOrphanPageSize,
		JobName: chunkOrphansJobName, now: time.Now,
	}
}

// chunkOrphanGraceFromEnv is CHUNK_ORPHAN_GRACE (1h–7d), else the default; a
// rejected value is logged at Warn and the default kept.
func chunkOrphanGraceFromEnv(getenv func(string) string, logger *zap.Logger) time.Duration {
	raw := strings.TrimSpace(getenv("CHUNK_ORPHAN_GRACE"))
	if raw == "" {
		return defaultChunkOrphanGrace
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d < minChunkOrphanGrace || d > maxChunkOrphanGrace {
		logger.Warn("CHUNK_ORPHAN_GRACE rejected; default kept", zap.String("value", raw), zap.Duration("default", defaultChunkOrphanGrace))
		return defaultChunkOrphanGrace
	}
	return d
}

// chunkOrphan is one listed blob no row names.
type chunkOrphan struct {
	backend, scope, hash, key string
	size                      int64
}

// ChunkOrphanCandidate is a dry run's report of one orphan.
type ChunkOrphanCandidate struct {
	Key          string    `json:"key"`
	Size         int64     `json:"size"`
	LastModified time.Time `json:"last_modified"`
}

// ChunkOrphanBackend is one backend's part of a run.
type ChunkOrphanBackend struct {
	Listed       int `json:"listed"`
	Named        int `json:"named"`        // a row names the key
	Retired      int `json:"retired"`      // retired_chunk_blobs names it (the collector's)
	Unrecognized int `json:"unrecognized"` // not a chunk key: never touched
	Young        int `json:"young"`        // no row, younger than the grace
	Found        int `json:"found"`        // no row, older than the grace
	// UnknownAge: no row, and the listing gave no modification time (a zero
	// LastModified) — never erased, its age cannot be told (RP13-2).
	UnknownAge int `json:"unknown_age,omitempty"`
	Erased     int `json:"erased"`
	Failed     int `json:"failed"`
	Kept       int `json:"kept"` // a row named it by the locked re-check
	// OrphanBytes: the bytes of orphans seen and still there after the run
	// (young, failed, or a dry run's) — what is leaking now.
	OrphanBytes   int64 `json:"orphan_bytes"`
	BytesErased   int64 `json:"bytes_erased"`
	PendingFound  int   `json:"pending_found"`  // abandoned claims
	PendingErased int   `json:"pending_erased"` // ... erased with their blob
	PendingFailed int   `json:"pending_failed"`
	// Complete: the listing reached the end of the container this run.
	Complete bool `json:"complete"`
	// Guarded: more orphans than the guard allows — nothing was erased.
	Guarded    bool                   `json:"guarded,omitempty"`
	Candidates []ChunkOrphanCandidate `json:"candidates,omitempty"` // a dry run's, ≤ 1000
}

// ChunkOrphanResult is one run's outcome (job_runs.result).
type ChunkOrphanResult struct {
	DryRun        bool  `json:"dry_run,omitempty"`
	OrphansFound  int   `json:"orphans_found"`
	OrphansErased int   `json:"orphans_erased"`
	OrphansFailed int   `json:"orphans_failed"`
	BytesErased   int64 `json:"bytes_erased"`
	PendingErased int   `json:"pending_erased"`
	// NotSwept names the backends this run could not ask, and why.
	NotSwept map[string]string              `json:"not_swept,omitempty"`
	Backends map[string]*ChunkOrphanBackend `json:"backends"`
	// Cursors: per backend, the last key listed by a run its budget cut;
	// absent = the next run lists from the start.
	Cursors map[string]string `json:"cursors,omitempty"`
}

var chunkOrphanOutcomes = func() *prometheus.CounterVec {
	return prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "vaultaire_chunk_orphans_total",
		Help: "Chunk blobs at the one address that no index row names, by what the chunk_orphans job did: erased (past CHUNK_ORPHAN_GRACE), failed (a delete that did not go through — the next run retries), kept (a store named it by the locked re-check), pending_erased (an abandoned pending claim and its blob).",
	}, []string{"backend", "outcome"})
}()

// spec: daily at 01:30 UTC — between inventory (00:30) and dedup_gc (02:30):
// orphans accrue slowly (a stall, a crash) and the grace is a day, so a
// daily walk is enough; ~25 list calls per 24k chunks.
func (c *ChunkOrphanSweeper) spec() jobSpec {
	return jobSpec{
		Name: c.JobName, Hour: 1, Minute: 30,
		BootDelay: 8 * time.Minute, MaxRunTime: time.Hour,
		Run: func(ctx context.Context) (jobReport, error) {
			res, err := c.Run(ctx, false)
			c.logger.Info("chunk orphans run", zap.Int("orphans_found", res.OrphansFound), zap.Int("orphans_erased", res.OrphansErased),
				zap.Int("orphans_failed", res.OrphansFailed), zap.Int64("bytes_erased", res.BytesErased),
				zap.Int("pending_erased", res.PendingErased), zap.Any("not_swept", res.NotSwept), zap.Error(err))
			rep := jobReport{Rows: int64(res.OrphansErased + res.PendingErased), Result: res}
			var notes []string
			if res.OrphansFailed > 0 {
				notes = append(notes, fmt.Sprintf("%d orphan(s) not erased (see the log); the next run retries", res.OrphansFailed))
			}
			if len(res.NotSwept) > 0 {
				names := make([]string, 0, len(res.NotSwept))
				for b, why := range res.NotSwept {
					names = append(names, b+": "+why)
				}
				sort.Strings(names)
				notes = append(notes, "not swept ("+strings.Join(names, ", ")+")")
			}
			rep.Note = strings.Join(notes, "; ")
			return rep, err
		},
	}
}

// Run is one pass over every backend. A dry run lists from the start of
// each container, erases nothing and moves no cursor; it reports the
// orphans past the grace (and the abandoned claims) with their sizes.
func (c *ChunkOrphanSweeper) Run(ctx context.Context, dryRun bool) (ChunkOrphanResult, error) {
	res := ChunkOrphanResult{DryRun: dryRun, Backends: map[string]*ChunkOrphanBackend{}, Cursors: map[string]string{}}
	prev := c.lastResult(ctx)
	backends, err := c.backends(ctx, prev)
	if err != nil {
		return res, err
	}
	for _, b := range backends {
		if ctx.Err() != nil {
			break
		}
		br := &ChunkOrphanBackend{}
		res.Backends[b] = br
		cursor := prev.Cursors[b]
		if dryRun {
			cursor = ""
		}
		if why := c.cannotAsk(b); why != "" {
			c.notSwept(&res, b, why, nil)
			if cursor != "" {
				res.Cursors[b] = cursor
			}
			continue
		}
		if err := c.pendingPass(ctx, b, dryRun, br); err != nil {
			c.notSwept(&res, b, "the index could not be read", err)
			continue
		}
		next, complete, orphans, err := c.listPass(ctx, b, cursor, dryRun, br)
		if br.UnknownAge > 0 {
			c.logger.Warn("chunk orphans: listed keys carry no modification time — never erased",
				zap.String("backend", b), zap.Int("keys", br.UnknownAge))
		}
		if err == nil && len(orphans) > 0 {
			if limit := c.guardLimit(br.Listed); len(orphans) > limit {
				br.Guarded = true
				for _, o := range orphans {
					br.OrphanBytes += o.size
				}
				c.notSwept(&res, b, fmt.Sprintf("orphan guard: %d of %d listed keys look unnamed (more than %d) — nothing erased; see the dry run",
					len(orphans), br.Listed, limit), nil)
			} else {
				for _, o := range orphans {
					if ctx.Err() != nil {
						break
					}
					c.erase(ctx, o, br)
				}
			}
		}
		if err != nil {
			why := "its listing failed"
			if errors.Is(err, errChunkCannotList) {
				why = "it cannot list its chunks in pages"
			}
			c.notSwept(&res, b, why, err)
			next = cursor
		}
		br.Complete = complete
		if !complete && next != "" {
			res.Cursors[b] = next
		}
	}
	for _, br := range res.Backends {
		res.OrphansFound += br.Found
		res.OrphansErased += br.Erased
		res.OrphansFailed += br.Failed
		res.BytesErased += br.BytesErased
		res.PendingErased += br.PendingErased
	}
	if len(res.Cursors) == 0 {
		res.Cursors = nil
	}
	return res, nil
}

// notSwept records a backend this run skipped, with one Warn.
func (c *ChunkOrphanSweeper) notSwept(res *ChunkOrphanResult, backend, why string, err error) {
	if res.NotSwept == nil {
		res.NotSwept = map[string]string{}
	}
	res.NotSwept[backend] = why
	c.logger.Warn("chunk orphans: backend not swept this run", zap.String("backend", backend), zap.String("why", why), zap.Error(err))
}

// cannotAsk is why the backend cannot be asked now ("" = it can).
func (c *ChunkOrphanSweeper) cannotAsk(backend string) string {
	if _, ok := c.eng.GetDriver(backend); !ok {
		return "not registered"
	}
	if err := c.eng.CanAsk(backend); err != nil {
		return "its breaker is open"
	}
	return ""
}

// lastResult is the previous run's result: the cursors, the backends.
func (c *ChunkOrphanSweeper) lastResult(ctx context.Context) ChunkOrphanResult {
	var raw []byte
	var last ChunkOrphanResult
	if err := c.db.QueryRowContext(ctx, `SELECT result FROM job_runs WHERE job = $1`, c.JobName).Scan(&raw); err != nil || len(raw) == 0 {
		return last
	}
	_ = json.Unmarshal(raw, &last)
	return last
}

// backends are the ones holding chunk rows now, or swept (or skipped) by the
// last run: a backend whose rows are gone may still hold orphans.
func (c *ChunkOrphanSweeper) backends(ctx context.Context, prev ChunkOrphanResult) ([]string, error) {
	if len(c.onlyBackends) > 0 {
		return append([]string(nil), c.onlyBackends...), nil
	}
	names, err := queryStrings(ctx, c.db, `SELECT DISTINCT backend_id FROM global_content_index WHERE backend_id <> ''`)
	if err != nil {
		return nil, fmt.Errorf("chunk orphans: backends with chunks: %w", err)
	}
	seen := map[string]bool{}
	var out []string
	add := func(b string) {
		if b != "" && b != chunkedBackendSentinel && !seen[b] {
			seen[b] = true
			out = append(out, b)
		}
	}
	for _, b := range names {
		add(b)
	}
	for b := range prev.Backends {
		add(b)
	}
	for b := range prev.NotSwept {
		add(b)
	}
	sort.Strings(out)
	return out, nil
}

// pendingPass erases the abandoned claims on backend: pending rows older than
// the grace that nobody references (a writer that died; the previous build in
// a switch overlap may have taken a reference — such a row is the next
// writer's to take over). Re-checked under the chunk's lock.
func (c *ChunkOrphanSweeper) pendingPass(ctx context.Context, backend string, dryRun bool, br *ChunkOrphanBackend) error {
	rows, err := c.db.QueryContext(ctx, `
		SELECT dedup_scope, plaintext_hash, storage_key
		  FROM global_content_index
		 WHERE backend_id = $1 AND state = 'pending' AND ref_count = 0
		   AND pending_since < NOW() - make_interval(secs => $2)
		 ORDER BY pending_since LIMIT $3`, backend, c.Grace.Seconds(), chunkOrphanPendingBatch)
	if err != nil {
		return fmt.Errorf("abandoned claims on %s: %w", backend, err)
	}
	var claims []chunkAddr
	for rows.Next() {
		a := chunkAddr{backend: backend}
		if err := rows.Scan(&a.scope, &a.hash, &a.key); err != nil {
			_ = rows.Close()
			return fmt.Errorf("abandoned claim on %s: %w", backend, err)
		}
		claims = append(claims, a)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return fmt.Errorf("abandoned claims on %s: %w", backend, err)
	}
	_ = rows.Close()
	br.PendingFound = len(claims)
	if dryRun {
		return nil
	}
	for _, a := range claims {
		if ctx.Err() != nil {
			return nil
		}
		err := withChunkLock(ctx, c.db, a.scope, a.hash, func(conn *sql.Conn) error {
			var since time.Time
			err := conn.QueryRowContext(ctx, `
				SELECT pending_since FROM global_content_index
				 WHERE dedup_scope = $1 AND plaintext_hash = $2 AND backend_id = $3 AND state = 'pending' AND ref_count = 0
				   AND pending_since < NOW() - make_interval(secs => $4)`,
				a.scope, a.hash, backend, c.Grace.Seconds()).Scan(&since)
			if errors.Is(err, sql.ErrNoRows) {
				return nil // made ready, taken over or gone since the scan
			}
			if err != nil {
				return fmt.Errorf("re-read the claim: %w", err)
			}
			if err := c.chunks.delete(ctx, a); err != nil {
				return fmt.Errorf("delete the claim's blob: %w", err)
			}
			if _, err := conn.ExecContext(ctx, `
				DELETE FROM global_content_index
				 WHERE dedup_scope = $1 AND plaintext_hash = $2 AND state = 'pending' AND pending_since = $3 AND ref_count = 0`,
				a.scope, a.hash, since); err != nil {
				return fmt.Errorf("delete the claim: %w", err)
			}
			br.PendingErased++
			chunkOrphanOutcomes.WithLabelValues(backend, "pending_erased").Inc()
			c.logger.Warn("chunk orphans: an abandoned chunk claim was erased with its blob",
				zap.String("backend", backend), zap.String("key", a.key), zap.Time("pending_since", since))
			return nil
		})
		if err != nil {
			br.PendingFailed++
			c.logger.Warn("chunk orphans: an abandoned claim was not erased; the next run retries",
				zap.String("backend", backend), zap.String("key", a.key), zap.Error(err))
		}
	}
	return nil
}

// guardLimit is the most orphans a run erases on a backend it listed n keys
// of.
func (c *ChunkOrphanSweeper) guardLimit(n int) int {
	floor := c.guardFloor
	if floor <= 0 {
		floor = chunkOrphanGuardFloor
	}
	return max(floor, int(chunkOrphanGuardFraction*float64(n)))
}

// listPass lists `_chunks/` on backend from cursor until the budget or the end
// and sorts every page; next is the last key listed, complete reports the end
// of the container, orphans are the keys to erase (none on a dry run: they
// are reported) — erased by the caller once the guard has seen them all.
func (c *ChunkOrphanSweeper) listPass(ctx context.Context, backend, cursor string, dryRun bool, br *ChunkOrphanBackend) (next string, complete bool, orphans []chunkOrphan, err error) {
	page := c.PageSize
	if page <= 0 {
		page = defaultChunkOrphanPageSize
	}
	budget := c.ListBudget
	if budget <= 0 {
		budget = defaultChunkOrphanListBudget
	}
	next = cursor
	for walked := 0; walked < budget; {
		if ctx.Err() != nil {
			return next, false, orphans, nil
		}
		objs, more, err := c.chunks.listPage(ctx, backend, next, min(page, budget-walked))
		if err != nil {
			return next, false, nil, err
		}
		walked += len(objs)
		br.Listed += len(objs)
		if len(objs) > 0 {
			next = objs[len(objs)-1].Artifact
		}
		found, err := c.examine(ctx, backend, objs, dryRun, br)
		if err != nil {
			return next, false, nil, err
		}
		orphans = append(orphans, found...)
		if !more {
			return "", true, orphans, nil
		}
	}
	return next, false, orphans, nil
}

// examine sorts one page: named, retired, unrecognized, young, or an orphan
// to erase (returned) or report (a dry run).
func (c *ChunkOrphanSweeper) examine(ctx context.Context, backend string, objs []engine.ListedObject, dryRun bool, br *ChunkOrphanBackend) ([]chunkOrphan, error) {
	if len(objs) == 0 {
		return nil, nil
	}
	keys := make([]string, len(objs))
	for i, o := range objs {
		keys[i] = o.Artifact
	}
	named, err := c.keysNamed(ctx, c.db, backend, keys)
	if err != nil {
		return nil, err
	}
	var orphans []chunkOrphan
	now := c.now()
	for _, o := range objs {
		switch named[o.Artifact] {
		case "row":
			br.Named++
			continue
		case "retired":
			br.Retired++
			continue
		}
		scope, hash, ok := chunkBlobIdentity(o.Artifact)
		if !ok {
			br.Unrecognized++
			continue
		}
		if o.LastModified.IsZero() {
			// No time: never "old" (a zero time read as infinitely old
			// would erase every unnamed key at its first sighting). Counted
			// and reported, never erased (RP13-2).
			br.UnknownAge++
			br.OrphanBytes += o.Size
			if dryRun && len(br.Candidates) < chunkOrphanReportMax {
				br.Candidates = append(br.Candidates, ChunkOrphanCandidate{Key: o.Artifact, Size: o.Size})
			}
			continue
		}
		if now.Sub(o.LastModified) < c.Grace {
			br.Young++
			br.OrphanBytes += o.Size
			continue
		}
		br.Found++
		if dryRun {
			br.OrphanBytes += o.Size
			if len(br.Candidates) < chunkOrphanReportMax {
				br.Candidates = append(br.Candidates, ChunkOrphanCandidate{Key: o.Artifact, Size: o.Size, LastModified: o.LastModified})
			}
			continue
		}
		orphans = append(orphans, chunkOrphan{backend: backend, scope: scope, hash: hash, key: o.Artifact, size: o.Size})
	}
	return orphans, nil
}

// keysNamed reads which of keys on backend a row names: "row" (an index row
// of any state — a pending claim names its blob too) or "retired" (a
// re-encrypt's old address, the collector's to delete).
func (c *ChunkOrphanSweeper) keysNamed(ctx context.Context, q interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}, backend string, keys []string) (map[string]string, error) {
	out := map[string]string{}
	for _, t := range []struct{ query, kind string }{
		{`SELECT storage_key FROM global_content_index WHERE backend_id = $1 AND storage_key = ANY($2)`, "row"},
		{`SELECT storage_key FROM retired_chunk_blobs WHERE backend_id = $1 AND storage_key = ANY($2)`, "retired"},
	} {
		rows, err := q.QueryContext(ctx, t.query, backend, pq.Array(keys))
		if err != nil {
			return nil, fmt.Errorf("chunk orphans: which keys are named on %s: %w", backend, err)
		}
		for rows.Next() {
			var k string
			if err := rows.Scan(&k); err != nil {
				_ = rows.Close()
				return nil, fmt.Errorf("chunk orphans: scan a named key: %w", err)
			}
			if out[k] == "" {
				out[k] = t.kind
			}
		}
		err = rows.Err()
		_ = rows.Close()
		if err != nil {
			return nil, fmt.Errorf("chunk orphans: which keys are named on %s: %w", backend, err)
		}
	}
	return out, nil
}

// erase deletes one orphan under its chunk's advisory lock (the lock the
// store and dedup GC take) after reading both tables again: a store that
// landed — or claimed — meanwhile keeps the blob.
func (c *ChunkOrphanSweeper) erase(ctx context.Context, o chunkOrphan, br *ChunkOrphanBackend) {
	if c.beforeErase != nil {
		c.beforeErase(o)
	}
	kept := false
	err := withChunkLock(ctx, c.db, o.scope, o.hash, func(conn *sql.Conn) error {
		named, err := c.keysNamed(ctx, conn, o.backend, []string{o.key})
		if err != nil {
			return err
		}
		if named[o.key] != "" {
			kept = true
			return nil
		}
		return c.chunks.delete(ctx, chunkAddr{scope: o.scope, hash: o.hash, backend: o.backend, key: o.key})
	})
	switch {
	case err != nil:
		br.Failed++
		br.OrphanBytes += o.size
		chunkOrphanOutcomes.WithLabelValues(o.backend, "failed").Inc()
		c.logger.Warn("chunk orphans: an orphan blob was not erased; the next run retries",
			zap.String("backend", o.backend), zap.String("key", o.key), zap.Error(err))
	case kept:
		br.Kept++
		chunkOrphanOutcomes.WithLabelValues(o.backend, "kept").Inc()
	default:
		br.Erased++
		br.BytesErased += o.size
		chunkOrphanOutcomes.WithLabelValues(o.backend, "erased").Inc()
		c.logger.Warn("chunk orphans: a chunk blob no index row names was erased",
			zap.String("backend", o.backend), zap.String("key", o.key), zap.Int64("bytes", o.size))
	}
}

// reencryptSuffix is a re-encrypted chunk's address suffix (`.k<N>`).
var reencryptSuffix = regexp.MustCompile(`\.k[0-9]+$`)

// chunkBlobIdentity is the (scope, hash) of a key at the one address — a
// re-encrypt's `.k<N>` address included.
func chunkBlobIdentity(key string) (scope, hash string, ok bool) {
	return chunkKeyIdentity(reencryptSuffix.ReplaceAllString(key, ""))
}

// chunkOrphanCollector exports vaultaire_chunk_orphan_bytes{backend}, READ
// FROM job_runs.result (a process that has just started reports the run of
// the process before it — the R13 lesson). Cached for a minute.
type chunkOrphanCollector struct {
	c    *ChunkOrphanSweeper
	desc *prometheus.Desc

	mu     sync.Mutex
	readAt time.Time
	last   map[string]int64
}

func newChunkOrphanCollector(c *ChunkOrphanSweeper) *chunkOrphanCollector {
	return &chunkOrphanCollector{c: c, desc: prometheus.NewDesc("vaultaire_chunk_orphan_bytes",
		"Bytes of chunk blobs no index row names that the last chunk_orphans run saw and left (younger than CHUNK_ORPHAN_GRACE, or a delete that failed), by backend — read from job_runs.result.",
		[]string{"backend"}, nil)}
}

func (cc *chunkOrphanCollector) Describe(ch chan<- *prometheus.Desc) { ch <- cc.desc }

func (cc *chunkOrphanCollector) Collect(ch chan<- prometheus.Metric) {
	cc.mu.Lock()
	if cc.last == nil || time.Since(cc.readAt) > time.Minute {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		res := cc.c.lastResult(ctx)
		cancel()
		m := map[string]int64{}
		for b, br := range res.Backends {
			m[b] = br.OrphanBytes
		}
		cc.last, cc.readAt = m, time.Now()
	}
	last := cc.last
	cc.mu.Unlock()
	for b, n := range last {
		ch <- prometheus.MustNewConstMetric(cc.desc, prometheus.GaugeValue, float64(n), b)
	}
}

// handleChunkOrphans is POST /api/v1/admin/chunk-orphans?dry_run=true: the
// orphans past the grace and the abandoned claims per backend, with their
// sizes — nothing erased, no cursor moved. A real run is the scheduler's
// (POST /api/v1/admin/jobs/chunk_orphans/run, under the job's lock, its
// result recorded).
func (s *Server) handleChunkOrphans(w http.ResponseWriter, r *http.Request) {
	if s.chunkOrphans == nil {
		http.Error(w, "chunk orphans not available (no database or engine)", http.StatusServiceUnavailable)
		return
	}
	if v := r.URL.Query().Get("dry_run"); v != "" {
		dry, err := strconv.ParseBool(v)
		if err != nil {
			http.Error(w, "dry_run must be true or false", http.StatusBadRequest)
			return
		}
		if !dry {
			http.Error(w, "this endpoint only reports (dry_run=true); erase through POST /api/v1/admin/jobs/chunk_orphans/run", http.StatusBadRequest)
			return
		}
	}
	if !s.chunkOrphansGate.tryAcquire() {
		writeJobAlreadyRunning(w, "chunk_orphans dry run")
		return
	}
	defer s.chunkOrphansGate.release()
	ctx, cancel := adminTriggerContext(r)
	defer cancel()
	res, err := s.chunkOrphans.Run(ctx, true)
	actor, _ := r.Context().Value(userIDKey).(string)
	audit.Record(r.Context(), s.db, audit.Entry{UserID: actor, EventType: "admin", Action: "admin.chunk_orphans_dry_run", Error: err,
		Metadata: map[string]any{"orphans_found": res.OrphansFound, "not_swept": res.NotSwept}})
	if err != nil {
		s.logger.Error("chunk orphans dry run failed", zap.Error(err))
		http.Error(w, "chunk orphans dry run failed (see the log)", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(res)
}
