package api

// The blob_copies job (WP-EDGE-4 Part A, Stage 1a Prompt 48; every 10 min,
// first run 6 min after boot): what the edge's mirror could not write
// itself, and what it must clean up.
//
//   - backfill: a tenant whose edge_mirror flag is on gets a `pending` copy
//     row for every whole object it holds that has none (origin writes,
//     multipart uploads, everything written before the flag) — up to
//     blobCopiesBackfillBatch rows a run, on the leg mirrorLegFor names for
//     the object's backend. Vault-floor objects are never mirrored (their
//     second copy is the parity, WP-VAULT-1); chunked objects mirror their
//     codec chunks at the edge only.
//   - copy: a `pending` row older than blobCopiesPendingAge (so the edge's
//     own commit has had its chance) is written from the primary — the
//     blob streamed from its recorded backend into the leg through the
//     drivers, its MD5 and size checked against the row — at most
//     blobCopiesMaxBytesPerRun a run, batch after batch while time remains
//     (RP76-4), and never past a source's free egress this UTC month
//     (egressLedger: backend_plans' multiple − 1 × the stored total —
//     one multiple is the customers', RP78-5 — the month's spend carried
//     in job_runs.result). This is the 2× lane (the bytes cross
//     SLC twice), rare by construction: the edge writes the copy when the
//     leg answers. A failure is the row's attempts and last_error; after
//     blobCopiesMaxAttempts it waits for the operator (a note).
//   - orphans: a `ready` or `pending` row whose blob no row names any more
//     (DELETE — which removes the leg's copy at once through the known
//     copies — an overwrite, a version delete, the dedup GC of a codec
//     chunk, a commit that failed after a chunk's copy was recorded) is
//     made `stale`; checked oldest-first, blobCopiesOrphanBatch a run.
//   - stale: deleted on the leg (absent is done), then the row.
//
// The leg's delete goes to the leg the row RECORDS; an unregistered leg or
// an open breaker is skipped (a note), never a failed run.

import (
	"context"
	"crypto/md5" // #nosec G501 -- the S3 ETag of the copied bytes
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/lib/pq"
	"github.com/prometheus/client_golang/prometheus"
	"go.uber.org/zap"
	"golang.org/x/sync/semaphore"

	"github.com/FairForge/vaultaire/internal/common"
	"github.com/FairForge/vaultaire/internal/engine"
)

const (
	blobCopiesJobName        = "blob_copies"
	blobCopiesPendingAge     = 5 * time.Minute
	blobCopiesMaxBytesPerRun = int64(50) << 30
	blobCopiesMaxAttempts    = 20
	blobCopiesBackfillBatch  = 1000
	blobCopiesOrphanBatch    = 500
	blobCopiesBatch          = 500
	// blobCopiesCopyBatch, blobCopiesConcurrency, blobCopiesLargeBytes: the
	// copy pass reads up to 2,000 pending rows a run and copies 8 at once;
	// a blob of 1 GiB or more is copied alone.
	blobCopiesCopyBatch   = 2000
	blobCopiesConcurrency = 8
	blobCopiesLargeBytes  = int64(1) << 30
	// blobCopiesOrphanRecheck: a row is asked whether its blob is still
	// named at most this often.
	blobCopiesOrphanRecheck = time.Hour
	blobCopiesCallTimeout   = 30 * time.Second
	// blobCopiesTailReserve is the time the copy pass leaves the orphan and
	// stale passes before the run's deadline (prod's first backfill run
	// spent all 9 minutes copying and the orphan pass hit the deadline).
	blobCopiesTailReserve = 90 * time.Second
)

// Outcomes (vaultaire_blob_copies_total{outcome}).
const (
	blobCopyEnqueued = "enqueued" // a backfill row written
	blobCopyCopied   = "copied"   // a pending copy written from the primary
	blobCopyFailed   = "failed"   // a copy attempt failed (retried next run)
	blobCopyOrphaned = "orphaned" // the blob is named by nothing: made stale
	blobCopyDeleted  = "deleted"  // a stale copy removed from its leg, row gone
	blobCopySkipped  = "skipped"  // a leg or source unregistered or its breaker open
	blobCopyCut      = "cut"      // a host cut a mirror that fell behind the primary (RP76-2)
	// blobCopySettled: a mirror that outlived the primary's answer settled
	// ready after the commit (RP78-2).
	blobCopySettled = "settled"
	// blobCopyUnbudgeted: a host's byte budget could not spare a mirror's
	// buffer; the PUT went unmirrored and the copy is owed (RP78-1).
	blobCopyUnbudgeted = "unbudgeted"
)

var (
	blobCopiesTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "vaultaire_blob_copies_total",
		Help: "Mirror copies (blob_copies, WP-EDGE-4) the blob_copies job handled, by outcome: enqueued (a backfill row) | copied (a pending copy written from the primary) | failed (a copy attempt failed; retried) | orphaned (its blob is named by nothing: made stale) | deleted (a stale copy removed from its leg) | skipped (a leg or source unregistered or its breaker open) | cut (a host cut a mirrored write's mirror leg loose for falling 16 MiB behind the primary or not answering within its bound — RP76-2; the copy is then owed) | settled (a mirror that outlived the primary's answer settled ready after the commit through POST /api/v1/edge/mirror — RP78-2) | unbudgeted (a host's byte budget could not spare a mirror's buffer and the PUT went unmirrored; the copy is owed — RP78-1).",
	}, []string{"outcome"})
	blobCopiesReads = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "vaultaire_blob_copies_reads_total",
		Help: "Edge reads the mirror copy served because the primary did not answer as admitted (WP-EDGE-4), by outcome (served_mirror).",
	}, []string{"outcome"})
	blobCopiesPending = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "vaultaire_blob_copies_pending",
		Help: "Mirror copies owed (blob_copies rows in state pending), counted at the end of each blob_copies run.",
	})
)

func init() {
	for _, o := range []string{blobCopyEnqueued, blobCopyCopied, blobCopyFailed, blobCopyOrphaned, blobCopyDeleted, blobCopySkipped, blobCopyCut, blobCopySettled, blobCopyUnbudgeted} {
		blobCopiesTotal.WithLabelValues(o)
	}
	blobCopiesReads.WithLabelValues("served_mirror")
}

// BlobCopiesJob is the job.
type BlobCopiesJob struct {
	s *Server
	// JobName is the job_runs name (tests use their own).
	JobName string
	// onlyTenant limits a run to one tenant (tests share the database).
	onlyTenant string
	now        func() time.Time
	maxBytes   int64
	// copyBatch is the pending rows one copy pass reads (blobCopiesCopyBatch);
	// capacityJob the backend_capacity run whose stored totals size the
	// egress budget (tests use their own).
	copyBatch   int
	capacityJob string
}

// newBlobCopiesJob is the job of s; nil without a database or an engine.
func newBlobCopiesJob(s *Server) *BlobCopiesJob {
	if s.db == nil || s.engine == nil {
		return nil
	}
	return &BlobCopiesJob{s: s, JobName: blobCopiesJobName, now: time.Now, maxBytes: blobCopiesMaxBytesPerRun,
		copyBatch: blobCopiesCopyBatch, capacityJob: backendCapacityJobName}
}

func (j *BlobCopiesJob) spec() jobSpec {
	return jobSpec{Name: j.JobName, Every: 10 * time.Minute, BootDelay: 6 * time.Minute, MaxRunTime: 9 * time.Minute,
		Run: func(ctx context.Context) (jobReport, error) {
			res, err := j.RunOnce(ctx)
			j.s.log().Info("blob copies run", zap.Int("enqueued", res.Enqueued), zap.Int("copied", res.Copied),
				zap.Int64("copied_bytes", res.CopiedBytes), zap.Int("failed", res.Failed), zap.Int("orphaned", res.Orphaned),
				zap.Int("deleted", res.Deleted), zap.Int("skipped", res.Skipped), zap.Error(err))
			rep := jobReport{Rows: int64(res.Copied + res.Deleted), Result: res}
			if res.Failed > 0 || res.Skipped > 0 || res.GaveUp > 0 {
				rep.Note = fmt.Sprintf("%d copy attempt(s) failed, %d skipped (unregistered, breaker open or the source's free egress spent), %d past %d attempts — they wait",
					res.Failed, res.Skipped, res.GaveUp, blobCopiesMaxAttempts)
			}
			if res.Stopped != "" {
				rep.Note = strings.TrimPrefix(rep.Note+"; "+res.Stopped, "; ")
			}
			if res.EgressNote != "" {
				rep.Note = strings.TrimPrefix(rep.Note+"; "+res.EgressNote, "; ")
			}
			return rep, err
		}}
}

// BlobCopiesResult is one run, recorded as job_runs.result.
type BlobCopiesResult struct {
	Enqueued    int   `json:"enqueued"`
	Copied      int   `json:"copied"`
	CopiedBytes int64 `json:"copied_bytes"`
	Failed      int   `json:"failed"`
	GaveUp      int   `json:"gave_up,omitempty"`
	Orphaned    int   `json:"orphaned"`
	Deleted     int   `json:"deleted"`
	Skipped     int   `json:"skipped"`
	Pending     int64 `json:"pending"`
	// Stopped: the run's time ran out; what is left is the next run's.
	Stopped string   `json:"stopped,omitempty"`
	Errors  []string `json:"errors,omitempty"`
	// Egress is what the copy pass read from each source this UTC month
	// (RP76-4), carried from run to run in job_runs.result; EgressNote
	// names the sources whose free egress stopped it.
	Egress     *blobCopyEgress `json:"egress,omitempty"`
	EgressNote string          `json:"egress_note,omitempty"`
}

// blobCopyEgress is the copy pass's egress this UTC month, per source.
type blobCopyEgress struct {
	Month string           `json:"month"` // 2006-01, UTC
	Spent map[string]int64 `json:"spent"`
}

func (r *BlobCopiesResult) note(err error) {
	if len(r.Errors) < 10 {
		r.Errors = append(r.Errors, err.Error())
	}
}

// RunOnce runs the four passes. A run whose time runs out stops where it
// is — a note (Stopped), never a failed run: the next run goes on.
func (j *BlobCopiesJob) RunOnce(ctx context.Context) (BlobCopiesResult, error) {
	var res BlobCopiesResult
	run := &copyRun{egress: j.egressLedger(ctx, &res)}
	stopped := func(name, where string) {
		res.Stopped = "the run's time ran out " + where + " the " + name + " pass; the next run goes on"
	}
	// Backfill and copy, batch after batch, while rows are pending and the
	// run's time remains (RP76-4, RB6-10); then the orphan and stale passes.
	for {
		if ctx.Err() != nil {
			stopped("backfill", "before")
			return res, nil
		}
		enqueued := res.Enqueued
		if err := j.backfill(ctx, &res); err != nil {
			if ctx.Err() != nil {
				stopped("backfill", "in")
				return res, nil
			}
			return res, fmt.Errorf("blob copies backfill: %w", err)
		}
		more, err := j.copyPending(ctx, &res, run)
		if err != nil {
			if ctx.Err() != nil {
				stopped("copy", "in")
				return res, nil
			}
			return res, fmt.Errorf("blob copies copy: %w", err)
		}
		if run.stop || (!more && res.Enqueued == enqueued) {
			break
		}
	}
	for _, p := range []struct {
		name string
		run  func(context.Context, *BlobCopiesResult) error
	}{{"orphans", j.findOrphans}, {"stale", j.deleteStale}} {
		if ctx.Err() != nil {
			stopped(p.name, "before")
			return res, nil
		}
		if err := p.run(ctx, &res); err != nil {
			if ctx.Err() != nil {
				stopped(p.name, "in")
				return res, nil
			}
			return res, fmt.Errorf("blob copies %s: %w", p.name, err)
		}
	}
	res.EgressNote = run.egress.note()
	if err := j.s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM blob_copies WHERE state = 'pending'`).Scan(&res.Pending); err == nil {
		blobCopiesPending.Set(float64(res.Pending))
	}
	return res, nil
}

// mirroredTenants is who the backfill covers: every tenant with an
// edge_mirror row on (all = the global row is on).
func (j *BlobCopiesJob) mirroredTenants(ctx context.Context) (all bool, tenants []string, err error) {
	rows, err := j.s.db.QueryContext(ctx, `SELECT tenant_id, enabled FROM feature_flags WHERE flag_key = $1`, flagEdgeMirror)
	if err != nil {
		return false, nil, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var t string
		var on bool
		if err := rows.Scan(&t, &on); err != nil {
			return false, nil, err
		}
		switch {
		case t == "*":
			all = on
		case on:
			tenants = append(tenants, t)
		}
	}
	return all, tenants, rows.Err()
}

// backfill writes a pending row for every whole object of a mirrored
// tenant that has no copy (each tenant re-checked through the flags: a
// per-tenant OFF under a global ON is respected).
func (j *BlobCopiesJob) backfill(ctx context.Context, res *BlobCopiesResult) error {
	all, tenants, err := j.mirroredTenants(ctx)
	if err != nil {
		return err
	}
	if j.onlyTenant != "" {
		all, tenants = false, []string{j.onlyTenant}
	}
	if !all && len(tenants) == 0 {
		return nil
	}
	rows, err := j.s.db.QueryContext(ctx, `
		SELECT h.tenant_id, h.bucket, h.object_key, COALESCE(h.storage_key, h.object_key), h.backend_name, h.size_bytes,
		       COALESCE(NULLIF(h.body_md5, ''), h.etag), COALESCE(b.region, ''), COALESCE(b.tier_preference, '')
		FROM object_head_cache h
		LEFT JOIN buckets b ON b.tenant_id = h.tenant_id AND b.name = h.bucket
		WHERE ($1 OR h.tenant_id = ANY($2)) AND NOT h.is_chunked AND h.floor <> 'vault'
		  AND COALESCE(h.backend_name, '') NOT IN ('', '`+chunkedBackendSentinel+`', '`+rsBackendSentinel+`')
		  AND NOT (COALESCE(h.backend_name, '') = ANY($4)) AND NOT (COALESCE(b.tier_preference, '') = ANY($4))
		  AND h.size_bytes > 0
		  AND NOT EXISTS (SELECT 1 FROM blob_copies c WHERE c.tenant_id = h.tenant_id
		                  AND c.storage_key = COALESCE(h.storage_key, h.object_key) AND c.kind = 'mirror')
		ORDER BY h.updated_at
		LIMIT $3`, all, pq.Array(tenants), blobCopiesBackfillBatch, pq.Array(engine.StrictTargets())) // RP78-8: the engine's list
	if err != nil {
		return err
	}
	type cand struct {
		c     blobCopy
		md5   string
		scope bucketScope
	}
	var cands []cand
	for rows.Next() {
		var c blobCopy
		var digest string
		var scope bucketScope
		if err := rows.Scan(&c.TenantID, &c.Bucket, &c.Key, &c.StorageKey, &c.Source, &c.Size, &digest, &scope.Region, &scope.Tier); err != nil {
			_ = rows.Close()
			return err
		}
		cands = append(cands, cand{c: c, md5: digest, scope: scope})
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	type legKey struct {
		source string
		scope  bucketScope
	}
	legFor := map[legKey]string{}
	for _, cd := range cands {
		c := cd.c
		if !j.s.flags.Enabled(flagEdgeMirror, c.TenantID) {
			continue
		}
		// The leg must satisfy the bucket (RP76-1): its region and tier.
		lk := legKey{c.Source, cd.scope}
		leg, ok := legFor[lk]
		if !ok {
			leg = j.s.mirrorLegFor(c.Source, cd.scope)
			legFor[lk] = leg
		}
		if leg == "" {
			continue
		}
		c.Leg, c.State = leg, copyPending
		if unverifiedReason(cd.md5) == "" {
			c.ETag = cd.md5
		}
		// Due at once: nothing at the edge is writing this copy.
		if _, err := j.s.db.ExecContext(ctx, `
			INSERT INTO blob_copies (tenant_id, bucket, object_key, storage_key, source_backend, leg, kind, state, etag, size, created_at)
			VALUES ($1, $2, $3, $4, $5, $6, 'mirror', 'pending', $7, $8, NOW() - $9::interval)
			ON CONFLICT DO NOTHING`,
			c.TenantID, c.Bucket, c.Key, c.StorageKey, c.Source, c.Leg, c.ETag, c.Size, blobCopiesPendingAge.String()); err != nil {
			return err
		}
		res.Enqueued++
		blobCopiesTotal.WithLabelValues(blobCopyEnqueued).Inc()
	}
	return nil
}

// copyRec is a blob_copies row the passes read.
type copyRec struct {
	blobCopy
	attempts int
}

// scanCopies reads copy rows.
func scanCopies(rows *sql.Rows) ([]copyRec, error) {
	defer func() { _ = rows.Close() }()
	var out []copyRec
	for rows.Next() {
		var c copyRec
		if err := rows.Scan(&c.TenantID, &c.Bucket, &c.Key, &c.StorageKey, &c.Source, &c.Leg, &c.State, &c.ETag, &c.Size, &c.attempts, &c.Kind, &c.Index); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

const copyColumns = `tenant_id, bucket, object_key, storage_key, source_backend, leg, state, etag, size, attempts, kind, shard_index`

// copyRun is one run's copy state across its batches: the bytes copied
// (against maxBytes), the keyset cursor past the rows already tried (a row
// that failed is the next run's), the egress ledger.
type copyRun struct {
	spent  int64
	after  copyCursor
	egress *egressLedger
	// stop: the copy pass stopped for the run's time or byte budget —
	// the loop ends.
	stop bool
}

type copyCursor struct {
	at               time.Time
	tenant, key, leg string
	index            int
	set              bool
}

// copyPending writes owed copies from the primary: one batch past the
// run's cursor; more = a full batch was read and the run may go on.
func (j *BlobCopiesJob) copyPending(ctx context.Context, res *BlobCopiesResult, run *copyRun) (bool, error) {
	batch := j.copyBatch
	if batch <= 0 {
		batch = blobCopiesCopyBatch
	}
	c0 := run.after
	rows, err := j.s.db.QueryContext(ctx, `SELECT `+copyColumns+`, created_at FROM blob_copies
		WHERE state = 'pending' AND kind = 'mirror' AND created_at <= $1 AND ($2 = '' OR tenant_id = $2)
		  AND (NOT $4 OR (created_at, tenant_id, storage_key, leg, shard_index) > ($5, $6, $7, $8, $9))
		ORDER BY created_at, tenant_id, storage_key, leg, shard_index LIMIT $3`,
		j.now().Add(-blobCopiesPendingAge), j.onlyTenant, batch, c0.set, c0.at, c0.tenant, c0.key, c0.leg, c0.index)
	if err != nil {
		return false, err
	}
	var due []copyRec
	for rows.Next() {
		var c copyRec
		var at time.Time
		if err := rows.Scan(&c.TenantID, &c.Bucket, &c.Key, &c.StorageKey, &c.Source, &c.Leg, &c.State, &c.ETag, &c.Size, &c.attempts, &c.Kind, &c.Index, &at); err != nil {
			_ = rows.Close()
			return false, err
		}
		due = append(due, c)
		run.after = copyCursor{at: at, tenant: c.TenantID, key: c.StorageKey, leg: c.Leg, index: c.Index, set: true}
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil {
		return false, err
	}
	more := len(due) == batch
	stopAt := time.Time{}
	if dl, ok := ctx.Deadline(); ok {
		stopAt = dl.Add(-blobCopiesTailReserve)
	}
	// blobCopiesConcurrency copies at once; a blob of blobCopiesLargeBytes
	// or more takes every slot (its upload holds the driver's part buffers).
	sem := semaphore.NewWeighted(blobCopiesConcurrency)
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, c := range due {
		mu.Lock()
		over := run.spent >= j.maxBytes
		mu.Unlock()
		late := !stopAt.IsZero() && !j.now().Before(stopAt)
		if ctx.Err() != nil || over || late {
			if late {
				mu.Lock()
				if res.Stopped == "" {
					res.Stopped = "the copy pass left the rest to the next run (its time is the orphan and stale passes')"
				}
				mu.Unlock()
			}
			more, run.stop = false, true
			break
		}
		if c.attempts >= blobCopiesMaxAttempts {
			mu.Lock()
			res.GaveUp++
			mu.Unlock()
			continue
		}
		// The source's free egress this month (RP76-4): a copy that would
		// pass it waits for the next month, skipped with the reason.
		if !run.egress.take(c.Source, c.Size) {
			mu.Lock()
			res.Skipped++
			mu.Unlock()
			blobCopiesTotal.WithLabelValues(blobCopySkipped).Inc()
			continue
		}
		weight := int64(1)
		if c.Size >= blobCopiesLargeBytes {
			weight = blobCopiesConcurrency
		}
		if err := sem.Acquire(ctx, weight); err != nil {
			run.egress.give(c.Source, c.Size)
			more = false
			break
		}
		mu.Lock()
		run.spent += c.Size
		mu.Unlock()
		wg.Add(1)
		go func(c copyRec, weight int64) {
			defer wg.Done()
			defer sem.Release(weight)
			ok := j.copyRow(ctx, c, res, &mu)
			if !ok {
				run.egress.give(c.Source, c.Size)
				mu.Lock()
				run.spent -= c.Size
				mu.Unlock()
			}
		}(c, weight)
	}
	wg.Wait()
	run.egress.record(res)
	return more, nil
}

// copyRow handles one pending row (res under mu); true = its bytes were
// copied.
func (j *BlobCopiesJob) copyRow(ctx context.Context, c copyRec, res *BlobCopiesResult, mu *sync.Mutex) bool {
	named, err := j.named(ctx, c.blobCopy)
	if err != nil {
		mu.Lock()
		res.note(err)
		mu.Unlock()
		return false
	}
	if !named {
		j.setState(ctx, c.blobCopy, copyStale, "")
		mu.Lock()
		res.Orphaned++
		mu.Unlock()
		blobCopiesTotal.WithLabelValues(blobCopyOrphaned).Inc()
		return false
	}
	if j.unmirrorable(c.blobCopy) {
		// RP76-6: a source no copy can be read from (an rs object's
		// sentinel, a strict target the backfill once enqueued) is no
		// mirror's: stale, never skipped for ever.
		j.setState(ctx, c.blobCopy, copyStale, "")
		mu.Lock()
		res.Orphaned++
		mu.Unlock()
		blobCopiesTotal.WithLabelValues(blobCopyOrphaned).Inc()
		return false
	}
	etag, cErr := j.copyOne(ctx, c.blobCopy)
	mu.Lock()
	defer mu.Unlock()
	switch {
	case errors.Is(cErr, errCopySkipped):
		res.Skipped++
		blobCopiesTotal.WithLabelValues(blobCopySkipped).Inc()
		return false
	case cErr != nil:
		res.Failed++
		res.note(cErr)
		blobCopiesTotal.WithLabelValues(blobCopyFailed).Inc()
		_, _ = j.s.db.ExecContext(context.WithoutCancel(ctx), `UPDATE blob_copies SET attempts = attempts + 1, last_error = $3, updated_at = NOW()
			WHERE tenant_id = $1 AND storage_key = $2 AND leg = $4 AND kind = 'mirror'`, c.TenantID, c.StorageKey, truncateErr(cErr), c.Leg)
		return false
	}
	res.Copied++
	res.CopiedBytes += c.Size
	blobCopiesTotal.WithLabelValues(blobCopyCopied).Inc()
	_, _ = j.s.db.ExecContext(context.WithoutCancel(ctx), `UPDATE blob_copies SET state = 'ready', etag = $3, last_error = NULL, updated_at = NOW()
		WHERE tenant_id = $1 AND storage_key = $2 AND leg = $4 AND kind = 'mirror' AND state = 'pending'`, c.TenantID, c.StorageKey, etag, c.Leg)
	return true
}

func truncateErr(err error) string {
	s := err.Error()
	if len(s) > 300 {
		s = s[:300]
	}
	return s
}

// unmirrorable reports whether c is a mirror copy whose source no copy may
// be read from: an rs object's sentinel, the chunked sentinel, or a strict
// target, which is never mirrored (RP76-1, RP76-6).
func (j *BlobCopiesJob) unmirrorable(c blobCopy) bool {
	if c.Kind != "" && c.Kind != "mirror" {
		return false
	}
	return c.Source == rsBackendSentinel || c.Source == chunkedBackendSentinel || engine.IsStrictTarget(c.Source)
}

// egressLedger is the copy pass's egress budget per source for one run
// (RP76-4): a source whose plan row (backend_plans) names an
// egress_free_multiple may give the pass, this UTC month, at most that
// multiple of what the last backend_capacity run counted stored on it,
// less what is already spent — the vendor's own month-to-date where it
// reports one, else (and at least) what this job read from it this month.
// A source with no plan row or a NULL multiple is unmetered. A metered
// source with no capacity run yet gets nothing (unknown = no egress).
type egressLedger struct {
	mu      sync.Mutex
	month   string
	spent   map[string]int64 // this month, through this run (the job's own)
	limit   map[string]int64 // remaining at the run's start; absent = unmetered
	took    map[string]int64 // this run, reserved or copied
	stopped map[string]bool  // sources a copy was refused for
}

// egressLedger reads the plans, the capacity run and this job's previous
// result; res.Egress carries the month on.
func (j *BlobCopiesJob) egressLedger(ctx context.Context, res *BlobCopiesResult) *egressLedger {
	l := &egressLedger{month: j.now().UTC().Format("2006-01"), spent: map[string]int64{}, limit: map[string]int64{},
		took: map[string]int64{}, stopped: map[string]bool{}}
	var prev BlobCopiesResult
	var raw []byte
	if err := j.s.db.QueryRowContext(ctx, `SELECT result FROM job_runs WHERE job = $1`, j.JobName).Scan(&raw); err == nil && len(raw) > 0 {
		_ = json.Unmarshal(raw, &prev)
	}
	if prev.Egress != nil && prev.Egress.Month == l.month {
		for k, v := range prev.Egress.Spent {
			l.spent[k] = v
		}
	}
	var capRes BackendCapacityResult
	haveCap := false
	raw = nil
	if err := j.s.db.QueryRowContext(ctx, `SELECT result FROM job_runs WHERE job = $1`, j.capacityJob).Scan(&raw); err == nil && len(raw) > 0 {
		haveCap = json.Unmarshal(raw, &capRes) == nil
	}
	rows, err := j.s.db.QueryContext(ctx, `SELECT backend, egress_free_multiple FROM backend_plans WHERE egress_free_multiple IS NOT NULL`)
	if err != nil {
		res.note(fmt.Errorf("egress budget: the plans could not be read — nothing metered is copied: %w", err))
		l.limit = nil // nil = every source refused (below)
	} else {
		for rows.Next() {
			var backend string
			var mult float64
			if rows.Scan(&backend, &mult) != nil {
				continue
			}
			var stored, used int64
			if haveCap {
				if b := capRes.Backends[backend]; b != nil {
					stored = b.Total
				}
				if v, ok := capRes.Vendor[backend]; ok && !v.EgressUnmetered {
					used = v.EgressMonthBytes
				}
			}
			used = max(used, l.spent[backend])
			// RP78-5: the pass spends only the allowance ABOVE one multiple
			// of stored — the customers' own GETs keep 1× stored before
			// the vendor's overage rate; the 2× lane is never worth that.
			l.limit[backend] = max(0, int64((mult-1)*float64(stored))-used)
		}
		if err := rows.Err(); err != nil {
			res.note(fmt.Errorf("egress budget: the plans could not be read — nothing metered is copied: %w", err))
			l.limit = nil
		}
		_ = rows.Close()
	}
	res.Egress = &blobCopyEgress{Month: l.month, Spent: map[string]int64{}}
	for k, v := range l.spent {
		res.Egress.Spent[k] = v
	}
	return l
}

// take reserves n bytes of source's budget; false = it would pass it.
func (l *egressLedger) take(source string, n int64) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.limit == nil {
		l.stopped[source] = true
		return false
	}
	rem, metered := l.limit[source]
	if metered && l.took[source]+n > rem {
		l.stopped[source] = true
		return false
	}
	l.took[source] += n
	return true
}

// give returns a reservation whose copy did not happen.
func (l *egressLedger) give(source string, n int64) {
	l.mu.Lock()
	l.took[source] -= n
	l.mu.Unlock()
}

// record writes the month's spend (the job's own) into res.
func (l *egressLedger) record(res *BlobCopiesResult) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for k, v := range l.took {
		res.Egress.Spent[k] = l.spent[k] + v
	}
}

// note names the sources whose budget refused a copy ("" = none).
func (l *egressLedger) note() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.stopped) == 0 {
		return ""
	}
	var names []string
	for k := range l.stopped {
		names = append(names, k)
	}
	sort.Strings(names)
	return "the copy pass stopped at the month's free egress of " + strings.Join(names, ", ") + " ((backend_plans multiple − 1) × stored: one multiple is the customers'); the rest waits for next month"
}

// errCopySkipped: a leg or the source cannot be asked now.
var errCopySkipped = errors.New("blob copies: the backend cannot be asked now")

// isChunkCopy: a codec chunk's copy (no bucket).
func (c blobCopy) isChunkCopy() bool { return c.Bucket == "" }

// address is the driver context, container and artifact of the blob.
func (c blobCopy) address(ctx context.Context) (context.Context, string, string) {
	if c.isChunkCopy() {
		return engine.ChunkContext(ctx), engine.ChunkContainer, c.StorageKey
	}
	artifact := c.StorageKey
	if c.Kind == "shard" {
		artifact = rsShardKey(c.StorageKey, c.Index)
	}
	return common.WithTenantID(ctx, c.TenantID), c.TenantID + "_" + c.Bucket, artifact
}

// copyOne streams the blob from its source into the leg and checks its
// MD5 (when the row knows it) and size; the leg's copy is removed when
// they do not match.
func (j *BlobCopiesJob) copyOne(ctx context.Context, c blobCopy) (string, error) {
	eng := j.s.engine
	if eng.CanAsk(c.Source) != nil || eng.CanAsk(c.Leg) != nil {
		return "", errCopySkipped
	}
	src, ok := eng.GetDriver(c.Source)
	leg, ok2 := eng.GetDriver(c.Leg)
	if !ok || !ok2 {
		return "", errCopySkipped
	}
	cctx, container, artifact := c.address(ctx)
	body, err := src.Get(cctx, container, artifact)
	if err != nil {
		return "", fmt.Errorf("read %s on %s: %w", artifact, c.Source, err)
	}
	defer func() { _ = body.Close() }()
	sum := md5.New() // #nosec G401
	counted := &countingReader{r: io.TeeReader(body, sum)}
	if err := leg.Put(cctx, container, artifact, counted, engine.WithContentLength(c.Size)); err != nil {
		return "", fmt.Errorf("write %s on %s: %w", artifact, c.Leg, err)
	}
	got := hex.EncodeToString(sum.Sum(nil))
	if counted.n != c.Size || (c.ETag != "" && !strings.EqualFold(got, c.ETag)) {
		dctx, cancel := context.WithTimeout(cctx, blobCopiesCallTimeout)
		defer cancel()
		_ = leg.Delete(dctx, container, artifact)
		return "", fmt.Errorf("the copy of %s is %d bytes md5 %s, the row says %d bytes md5 %s", artifact, counted.n, got, c.Size, c.ETag)
	}
	return got, nil
}

// named reports whether the copy's blob is still named: a whole blob by a
// head, version, demotion, promotion row or an active upload (blobNamedBy);
// a codec chunk by its ready index row.
func (j *BlobCopiesJob) named(ctx context.Context, c blobCopy) (bool, error) {
	if c.isChunkCopy() {
		keyed, ok := codecChunkKeyed(c.TenantID, c.StorageKey)
		if !ok {
			return false, nil
		}
		var named bool
		err := j.s.db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM global_content_index
			WHERE dedup_scope = $1 AND plaintext_hash = $2 AND state = 'ready')`, c.TenantID, keyed).Scan(&named)
		return named, err
	}
	n, err := blobNamedBy(ctx, j.s.db, c.TenantID, c.Bucket, c.Key, c.StorageKey)
	return n != blobFree, err
}

// codecChunkKeyed is the keyed hash of a codec chunk's address
// (_chunks/<tenant>/<keyed>.k<version>).
func codecChunkKeyed(tenantID, storageKey string) (string, bool) {
	rest, ok := strings.CutPrefix(storageKey, "_chunks/"+tenantID+"/")
	if !ok {
		return "", false
	}
	keyed, _, ok := strings.Cut(rest, ".k")
	return keyed, ok && keyed != ""
}

func (j *BlobCopiesJob) setState(ctx context.Context, c blobCopy, state, lastErr string) {
	_, _ = j.s.db.ExecContext(ctx, `UPDATE blob_copies SET state = $3, last_error = NULLIF($4, ''), updated_at = NOW()
		WHERE tenant_id = $1 AND storage_key = $2 AND leg = $5 AND shard_index = $6`, c.TenantID, c.StorageKey, state, lastErr, c.Leg, c.Index)
}

// findOrphans makes stale the copies whose blob nothing names.
func (j *BlobCopiesJob) findOrphans(ctx context.Context, res *BlobCopiesResult) error {
	rows, err := j.s.db.QueryContext(ctx, `SELECT `+copyColumns+` FROM blob_copies
		WHERE state IN ('ready', 'pending') AND updated_at <= $1 AND ($2 = '' OR tenant_id = $2)
		ORDER BY updated_at LIMIT $3`, j.now().Add(-blobCopiesOrphanRecheck), j.onlyTenant, blobCopiesOrphanBatch)
	if err != nil {
		return err
	}
	rs, err := scanCopies(rows)
	if err != nil {
		return err
	}
	for _, c := range rs {
		named, err := j.named(ctx, c.blobCopy)
		if err != nil {
			res.note(err)
			continue
		}
		if named {
			_, _ = j.s.db.ExecContext(ctx, `UPDATE blob_copies SET updated_at = NOW() WHERE tenant_id = $1 AND storage_key = $2 AND leg = $3 AND shard_index = $4`,
				c.TenantID, c.StorageKey, c.Leg, c.Index)
			continue
		}
		j.setState(ctx, c.blobCopy, copyStale, "")
		res.Orphaned++
		blobCopiesTotal.WithLabelValues(blobCopyOrphaned).Inc()
	}
	return nil
}

// deleteStale removes stale copies from their legs, then their rows. A row
// whose blob is named again (a write reused the address) goes back to
// pending instead.
func (j *BlobCopiesJob) deleteStale(ctx context.Context, res *BlobCopiesResult) error {
	rows, err := j.s.db.QueryContext(ctx, `SELECT `+copyColumns+` FROM blob_copies
		WHERE state = 'stale' AND ($1 = '' OR tenant_id = $1) ORDER BY updated_at LIMIT $2`, j.onlyTenant, blobCopiesBatch)
	if err != nil {
		return err
	}
	rs, err := scanCopies(rows)
	if err != nil {
		return err
	}
	for _, c := range rs {
		if named, err := j.named(ctx, c.blobCopy); err == nil && named && !j.unmirrorable(c.blobCopy) {
			j.setState(ctx, c.blobCopy, copyPending, "")
			continue
		}
		leg, ok := j.s.engine.GetDriver(c.Leg)
		if !ok || j.s.engine.CanAsk(c.Leg) != nil {
			res.Skipped++
			blobCopiesTotal.WithLabelValues(blobCopySkipped).Inc()
			continue
		}
		cctx, container, artifact := c.address(ctx)
		dctx, cancel := context.WithTimeout(cctx, blobCopiesCallTimeout)
		err := leg.Delete(dctx, container, artifact)
		cancel()
		if err != nil && !isObjectMissingErr(err) {
			res.note(fmt.Errorf("delete %s on %s: %w", artifact, c.Leg, err))
			continue
		}
		if _, err := j.s.db.ExecContext(ctx, `DELETE FROM blob_copies WHERE tenant_id = $1 AND storage_key = $2 AND leg = $3 AND shard_index = $4 AND state = 'stale'`,
			c.TenantID, c.StorageKey, c.Leg, c.Index); err != nil {
			res.note(err)
			continue
		}
		res.Deleted++
		blobCopiesTotal.WithLabelValues(blobCopyDeleted).Inc()
	}
	return nil
}
