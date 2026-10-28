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
//     blobCopiesMaxBytesPerRun a run. This is the 2× lane (the bytes cross
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
	"errors"
	"fmt"
	"io"
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
)

var (
	blobCopiesTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "vaultaire_blob_copies_total",
		Help: "Mirror copies (blob_copies, WP-EDGE-4) the blob_copies job handled, by outcome: enqueued (a backfill row) | copied (a pending copy written from the primary) | failed (a copy attempt failed; retried) | orphaned (its blob is named by nothing: made stale) | deleted (a stale copy removed from its leg) | skipped (a leg or source unregistered or its breaker open).",
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
	for _, o := range []string{blobCopyEnqueued, blobCopyCopied, blobCopyFailed, blobCopyOrphaned, blobCopyDeleted, blobCopySkipped} {
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
}

// newBlobCopiesJob is the job of s; nil without a database or an engine.
func newBlobCopiesJob(s *Server) *BlobCopiesJob {
	if s.db == nil || s.engine == nil {
		return nil
	}
	return &BlobCopiesJob{s: s, JobName: blobCopiesJobName, now: time.Now, maxBytes: blobCopiesMaxBytesPerRun}
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
				rep.Note = fmt.Sprintf("%d copy attempt(s) failed, %d skipped (unregistered or breaker open), %d past %d attempts — they wait",
					res.Failed, res.Skipped, res.GaveUp, blobCopiesMaxAttempts)
			}
			if res.Stopped != "" {
				rep.Note = strings.TrimPrefix(rep.Note+"; "+res.Stopped, "; ")
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
	passes := []struct {
		name string
		run  func(context.Context, *BlobCopiesResult) error
	}{{"backfill", j.backfill}, {"copy", j.copyPending}, {"orphans", j.findOrphans}, {"stale", j.deleteStale}}
	for _, p := range passes {
		if ctx.Err() != nil {
			res.Stopped = "the run's time ran out before the " + p.name + " pass; the next run goes on"
			return res, nil
		}
		if err := p.run(ctx, &res); err != nil {
			if ctx.Err() != nil {
				res.Stopped = "the run's time ran out in the " + p.name + " pass; the next run goes on"
				return res, nil
			}
			return res, fmt.Errorf("blob copies %s: %w", p.name, err)
		}
	}
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
		       COALESCE(NULLIF(h.body_md5, ''), h.etag)
		FROM object_head_cache h
		WHERE ($1 OR h.tenant_id = ANY($2)) AND NOT h.is_chunked AND h.floor <> 'vault'
		  AND COALESCE(h.backend_name, '') NOT IN ('', '`+chunkedBackendSentinel+`')
		  AND h.size_bytes > 0
		  AND NOT EXISTS (SELECT 1 FROM blob_copies c WHERE c.tenant_id = h.tenant_id
		                  AND c.storage_key = COALESCE(h.storage_key, h.object_key) AND c.kind = 'mirror')
		ORDER BY h.updated_at
		LIMIT $3`, all, pq.Array(tenants), blobCopiesBackfillBatch)
	if err != nil {
		return err
	}
	type cand struct {
		c   blobCopy
		md5 string
	}
	var cands []cand
	for rows.Next() {
		var c blobCopy
		var digest string
		if err := rows.Scan(&c.TenantID, &c.Bucket, &c.Key, &c.StorageKey, &c.Source, &c.Size, &digest); err != nil {
			_ = rows.Close()
			return err
		}
		cands = append(cands, cand{c: c, md5: digest})
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	legFor := map[string]string{}
	for _, cd := range cands {
		c := cd.c
		if !j.s.flags.Enabled(flagEdgeMirror, c.TenantID) {
			continue
		}
		leg, ok := legFor[c.Source]
		if !ok {
			leg = j.s.mirrorLegFor(c.Source)
			legFor[c.Source] = leg
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
		if err := rows.Scan(&c.TenantID, &c.Bucket, &c.Key, &c.StorageKey, &c.Source, &c.Leg, &c.State, &c.ETag, &c.Size, &c.attempts); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

const copyColumns = `tenant_id, bucket, object_key, storage_key, source_backend, leg, state, etag, size, attempts`

// copyPending writes owed copies from the primary.
func (j *BlobCopiesJob) copyPending(ctx context.Context, res *BlobCopiesResult) error {
	rows, err := j.s.db.QueryContext(ctx, `SELECT `+copyColumns+` FROM blob_copies
		WHERE state = 'pending' AND kind = 'mirror' AND created_at <= $1 AND ($2 = '' OR tenant_id = $2)
		ORDER BY created_at LIMIT $3`, j.now().Add(-blobCopiesPendingAge), j.onlyTenant, blobCopiesCopyBatch)
	if err != nil {
		return err
	}
	due, err := scanCopies(rows)
	if err != nil {
		return err
	}
	stopAt := time.Time{}
	if dl, ok := ctx.Deadline(); ok {
		stopAt = dl.Add(-blobCopiesTailReserve)
	}
	// blobCopiesConcurrency copies at once; a blob of blobCopiesLargeBytes
	// or more takes every slot (its upload holds the driver's part buffers).
	sem := semaphore.NewWeighted(blobCopiesConcurrency)
	var mu sync.Mutex
	var spent int64
	var wg sync.WaitGroup
	for _, c := range due {
		mu.Lock()
		over := spent >= j.maxBytes
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
			break
		}
		if c.attempts >= blobCopiesMaxAttempts {
			mu.Lock()
			res.GaveUp++
			mu.Unlock()
			continue
		}
		weight := int64(1)
		if c.Size >= blobCopiesLargeBytes {
			weight = blobCopiesConcurrency
		}
		if err := sem.Acquire(ctx, weight); err != nil {
			break
		}
		mu.Lock()
		spent += c.Size
		mu.Unlock()
		wg.Add(1)
		go func(c copyRec, weight int64) {
			defer wg.Done()
			defer sem.Release(weight)
			ok := j.copyRow(ctx, c, res, &mu)
			if !ok {
				mu.Lock()
				spent -= c.Size
				mu.Unlock()
			}
		}(c, weight)
	}
	wg.Wait()
	return nil
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

// errCopySkipped: a leg or the source cannot be asked now.
var errCopySkipped = errors.New("blob copies: the backend cannot be asked now")

// isChunkCopy: a codec chunk's copy (no bucket).
func (c blobCopy) isChunkCopy() bool { return c.Bucket == "" }

// address is the driver context, container and artifact of the blob.
func (c blobCopy) address(ctx context.Context) (context.Context, string, string) {
	if c.isChunkCopy() {
		return engine.ChunkContext(ctx), engine.ChunkContainer, c.StorageKey
	}
	return common.WithTenantID(ctx, c.TenantID), c.TenantID + "_" + c.Bucket, c.StorageKey
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
		WHERE tenant_id = $1 AND storage_key = $2 AND leg = $5 AND kind = 'mirror'`, c.TenantID, c.StorageKey, state, lastErr, c.Leg)
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
			_, _ = j.s.db.ExecContext(ctx, `UPDATE blob_copies SET updated_at = NOW() WHERE tenant_id = $1 AND storage_key = $2 AND leg = $3 AND kind = 'mirror'`,
				c.TenantID, c.StorageKey, c.Leg)
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
		if named, err := j.named(ctx, c.blobCopy); err == nil && named {
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
		if _, err := j.s.db.ExecContext(ctx, `DELETE FROM blob_copies WHERE tenant_id = $1 AND storage_key = $2 AND leg = $3 AND kind = 'mirror' AND state = 'stale'`,
			c.TenantID, c.StorageKey, c.Leg); err != nil {
			res.note(err)
			continue
		}
		res.Deleted++
		blobCopiesTotal.WithLabelValues(blobCopyDeleted).Inc()
	}
	return nil
}
