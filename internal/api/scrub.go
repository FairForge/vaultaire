package api

import (
	"bufio"
	"context"
	"crypto/md5" // #nosec G501 -- an S3 ETag is an MD5; the scrub compares the bytes to it
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/lib/pq"
	"github.com/prometheus/client_golang/prometheus"
	"go.uber.org/zap"

	"github.com/FairForge/vaultaire/internal/common"
	"github.com/FairForge/vaultaire/internal/crypto"
	"github.com/FairForge/vaultaire/internal/engine"
	"github.com/FairForge/vaultaire/internal/usage"
)

// The blob scrub (WP-VAULT-2 Part 2, Stage 1a Prompt 58; docs/DURABILITY.md).
// The parity scrub reads the Vault's second copy and the tape audit asks the
// tape; nothing read a HOT blob whole: routing_truth reads the first 4 KiB of
// a sample, a customer's GET reads what it reads. A blob that is wrong is
// found here, not at the customer's read.
//
// Once per SCRUB_CYCLE every hot blob of every tenant with the blob_scrub
// flag is read WHOLE on its RECORDED backend — the driver called directly,
// with the row's tenant in the context: never the fan-out, the breaker
// observed (an open one skips the backend), never charged (readProbe's rule)
// — and checked against its row:
//
//   - a head row with bytes (not chunked) and a retained version
//     (usage.RetainedVersionsFrom): the bytes' MD5 against the row's digest —
//     COALESCE(body_md5, a single-part ETag) for a plaintext object, the
//     backend's own ETag of the stored bytes (backend_etag) for a sealed one.
//     A multipart ETag with no body_md5, or a sealed object whose stored
//     bytes' MD5 was never recorded, is `unverifiable`: read (so unreadable
//     and missing are still found), counted, never a finding;
//   - a ready chunk blob (global_content_index): a plaintext chunk by its
//     SHA-256 after decompression, a convergent one by its stored bytes'
//     SHA-256 (ciphertext_hash), an AES256-EW one opened under its recorded
//     version and bound by its keyed hash (the codec's own check) when this
//     process holds the tenant keys, else by its stored bytes' MD5.
//
// The budget is A1's (docs/reviews/WP-VAULT-2.md): one run reads the
// backend's footprint × Every ÷ Cycle bytes — at least one blob — so every
// blob is read once a cycle; a cycle that finished early waits for the next.
// It is never past the vendor's free egress: before a backend's slice the
// account's egressFreeBytes less the month's real egress (the vendor's own
// month-to-date where reported, at least what this job and the blob copies
// read) bounds what it may read; nothing left = skipped, `vendor_allowance`.
// A metered backend with no capacity run yet reads nothing.
//
// A finding (scrub_findings, migration 125) is written only for what is not
// ok — after the row is read again (a write or delete that moved it is no
// finding). A mismatch, unreadable or missing blob with a ready mirror copy
// (blob_copies, kind 'mirror') is rewritten from it at the SAME address on
// the recorded backend, the copy's bytes checked against the copy row's MD5
// and size as they stream (a copy that does not verify is never written
// over the blob); the repair is its own finding (`repaired`, the leg in
// source). The vault parity is no repair source here: a vault-floor blob is
// on tape, which this scrub does not read (the parity scrub and the tape
// audit do).
//
// The cursor of each backend's walk (whole → version → chunk, in key order)
// is in job_runs.result: it survives a restart and a deploy.

const (
	blobScrubJobName      = "blob_scrub"
	defaultBlobScrubEvery = 10 * time.Minute
	blobScrubMaxRunTime   = 9 * time.Minute
	blobScrubPage         = 50
	// maxScrubChunkFindings bounds the rows one bad chunk writes (one per
	// object that references it).
	maxScrubChunkFindings = 1000
	maxBlobScrubNotes     = 20
	// scrubNoAnswerRuns: a blob with no verdict (no answer) this many runs in
	// a row is a finding and the walk goes on (RP96-A-1: the slice stopped at
	// it every run, for ever).
	scrubNoAnswerRuns = 3
)

// The kinds of blob, in the order a cycle walks them.
const (
	scrubKindWhole   = "whole"
	scrubKindVersion = "version"
	scrubKindChunk   = "chunk"
)

var scrubKinds = []string{scrubKindWhole, scrubKindVersion, scrubKindChunk}

// The verdicts (migration 125's CHECK).
const (
	scrubOK           = "ok"
	scrubMismatch     = "mismatch"
	scrubUnreadable   = "unreadable"
	scrubMissing      = "missing"
	scrubUnverifiable = "unverifiable"
	scrubRepaired     = "repaired"
	// scrubNoVerdict: the backend could not be asked (a deadline, a
	// connection) — no finding, the slice stops and the blob is read again.
	scrubNoVerdict = "error"
	// scrubChanged: the row moved under the scrub (a write, a delete).
	scrubChanged = "changed"
)

// scrubSkipBackends are never read: tape (the tape audit's).
var scrubSkipBackends = map[string]bool{"geyser": true}

var (
	scrubChecks = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "vaultaire_scrub_checked_total",
		Help: "Blobs the blob_scrub job read whole on their recorded backend and checked against their row (head rows, retained versions, chunks), by backend.",
	}, []string{"backend"})
	scrubFailures = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "vaultaire_scrub_failed_total",
		Help: "Blobs the blob_scrub job found wrong — a mismatch with the row's digest, unreadable, or missing — by backend (a repaired one counts here too: it was wrong).",
	}, []string{"backend"})
	scrubBytesRead = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "vaultaire_scrub_bytes_total",
		Help: "Bytes the blob_scrub job read from each backend (its egress there).",
	}, []string{"backend"})
)

// blobScrubCycleFromEnv is SCRUB_CYCLE: a Go duration or whole days, 1 d–365 d
// (parseScrubCycle, the parity scrub's parser). Unset = 0: the job is dark. A
// rejected value is logged at Warn and the job stays dark.
func blobScrubCycleFromEnv(getenv func(string) string, logger *zap.Logger) time.Duration {
	raw := strings.TrimSpace(getenv("SCRUB_CYCLE"))
	if raw == "" {
		return 0
	}
	d, ok := parseScrubCycle(raw, 365*24*time.Hour)
	if !ok {
		logger.Warn("SCRUB_CYCLE rejected; the blob scrub stays dark", zap.String("value", raw))
		return 0
	}
	return d
}

// BlobScrubJob is the job.
type BlobScrubJob struct {
	s *Server
	// JobName is the job_runs name; capacityJob and copiesJob the runs whose
	// results size the vendor budget (tests use their own).
	JobName, capacityJob, copiesJob string
	Cycle, Every                    time.Duration
	// onlyTenant limits a run to one tenant (tests share the database).
	onlyTenant string
	now        func() time.Time
}

// newBlobScrubJob is the job of s; nil without a database, an engine or a
// cycle.
func newBlobScrubJob(s *Server, cycle time.Duration) *BlobScrubJob {
	if s.db == nil || s.engine == nil || cycle <= 0 {
		return nil
	}
	for _, b := range s.engine.GetDriverNames() {
		// A vec with no series never reaches the scrape (RB2-6's lesson).
		scrubChecks.WithLabelValues(b)
		scrubFailures.WithLabelValues(b)
		scrubBytesRead.WithLabelValues(b)
	}
	return &BlobScrubJob{s: s, JobName: blobScrubJobName, capacityJob: backendCapacityJobName, copiesJob: blobCopiesJobName,
		Cycle: cycle, Every: defaultBlobScrubEvery, now: time.Now}
}

func (j *BlobScrubJob) spec() jobSpec {
	return jobSpec{Name: j.JobName, Every: j.Every, BootDelay: 8 * time.Minute, MaxRunTime: blobScrubMaxRunTime,
		Run: func(ctx context.Context) (jobReport, error) {
			res, err := j.RunOnce(ctx)
			var checked, failed, repaired int
			for _, b := range res.Backends {
				for _, n := range b.Checked {
					checked += n
				}
				failed += b.Failed
				repaired += b.Repaired
			}
			j.s.log().Info("blob scrub run", zap.Int("checked", checked), zap.Int("failed", failed), zap.Int("repaired", repaired), zap.Error(err))
			rep := jobReport{Rows: int64(checked), Result: res}
			if len(res.Notes) > 0 {
				rep.Note = strings.Join(res.Notes, "; ")
			}
			return rep, err
		}}
}

// BlobScrubResult is one run, recorded as job_runs.result.
type BlobScrubResult struct {
	At       time.Time                 `json:"at"`
	Month    string                    `json:"month"` // 2006-01, UTC: the month MonthBytes counts
	Cycle    string                    `json:"cycle"`
	Backends map[string]*BlobScrubBack `json:"backends"`
	Notes    []string                  `json:"notes,omitempty"`
}

// BlobScrubBack is one backend's walk: the state carried from run to run
// and this run's counts.
type BlobScrubBack struct {
	Cursor scrubWalk `json:"cursor"`
	// CycleStartedAt: the cycle in progress began (zero = none in progress);
	// LastCycleStartedAt / CycleCompletedAt: the last cycle that finished.
	CycleStartedAt     time.Time `json:"cycle_started_at,omitempty"`
	LastCycleStartedAt time.Time `json:"last_cycle_started_at,omitempty"`
	CycleCompletedAt   time.Time `json:"cycle_completed_at,omitempty"`
	// MonthBytes: what the scrub read from it this UTC month.
	MonthBytes int64 `json:"month_bytes"`
	// Stall: the blob at the cursor gave no answer, and for how many runs
	// (RP96-A-1). Cleared by any verdict.
	Stall *scrubStall `json:"stall,omitempty"`

	// This run.
	Checked      map[string]int `json:"checked,omitempty"`
	Bytes        int64          `json:"bytes"`
	Failed       int            `json:"failed"`
	Repaired     int            `json:"repaired"`
	Unverifiable int            `json:"unverifiable"`
	Skipped      string         `json:"skipped,omitempty"`
}

// scrubStall is a blob that gave no answer: its position and the runs it did.
type scrubStall struct {
	Walk scrubWalk `json:"walk"`
	Runs int       `json:"runs"`
}

// scrubWalk is a position in a backend's walk: the kind, then the row key
// (a chunk: A = its scope, B = its hash). Zero = the start of a cycle.
type scrubWalk struct {
	Kind    string `json:"kind,omitempty"`
	Tenant  string `json:"tenant,omitempty"`
	Bucket  string `json:"bucket,omitempty"`
	Key     string `json:"key,omitempty"`
	Version string `json:"version,omitempty"`
}

func (w scrubWalk) zero() bool { return w == scrubWalk{} }

// scrubItem is one blob to read.
type scrubItem struct {
	kind                       string
	tenant, bucket, key, verID string
	storageKey                 string // the row's (a chunk's key)
	size                       int64
	digest                     string // the MD5 the stored bytes must have ("" = unverifiable)
	// sealed: the row's encryption_algorithm is set (SSE-S3 or SSE-C) — the
	// stored bytes are longer than size (a header, a tag per segment); the
	// stored length is a function of size and the header the blob carries
	// (RP96-1: the plaintext size was compared, and a healthy SSE-C object
	// was `mismatch`).
	sealed bool
	// a chunk
	scope, hash, encAlgo, ciphertextHash string
	compressed, encrypted                bool
	keyVersion                           int
}

func (it scrubItem) walk() scrubWalk {
	if it.kind == scrubKindChunk {
		return scrubWalk{Kind: it.kind, Tenant: it.scope, Key: it.hash}
	}
	return scrubWalk{Kind: it.kind, Tenant: it.tenant, Bucket: it.bucket, Key: it.key, Version: it.verID}
}

// address is the driver context, container and artifact of the blob.
func (it scrubItem) address(ctx context.Context) (context.Context, string, string) {
	if it.kind == scrubKindChunk {
		return engine.ChunkContext(ctx), engine.ChunkContainer, it.storageKey
	}
	container, artifact := objectBlobAddress(it.tenant, it.bucket, it.key, it.storageKey)
	return common.WithTenantID(ctx, it.tenant), container, artifact
}

// scrubScope is who the run covers.
type scrubScope struct {
	all     bool
	tenants []string
}

func (j *BlobScrubJob) scope(ctx context.Context) (scrubScope, error) {
	if j.onlyTenant != "" {
		if j.s.flags != nil && j.s.flags.Enabled(flagBlobScrub, j.onlyTenant) {
			return scrubScope{tenants: []string{j.onlyTenant}}, nil
		}
		return scrubScope{}, nil
	}
	rows, err := j.s.db.QueryContext(ctx, `SELECT tenant_id, enabled FROM feature_flags WHERE flag_key = $1`, flagBlobScrub)
	if err != nil {
		return scrubScope{}, err
	}
	defer func() { _ = rows.Close() }()
	var sc scrubScope
	var listed []string
	for rows.Next() {
		var t string
		var on bool
		if err := rows.Scan(&t, &on); err != nil {
			return scrubScope{}, err
		}
		if t == "*" {
			sc.all = on
		} else {
			listed = append(listed, t)
		}
	}
	if err := rows.Err(); err != nil {
		return scrubScope{}, err
	}
	if sc.all {
		return sc, nil // a per-tenant OFF under a global ON is honoured per row (inScope)
	}
	for _, t := range listed {
		if j.s.flags != nil && j.s.flags.Enabled(flagBlobScrub, t) {
			sc.tenants = append(sc.tenants, t)
		}
	}
	return sc, nil
}

// inScope re-checks a tenant through the flags (a per-tenant OFF under a
// global ON).
func (j *BlobScrubJob) inScope(sc scrubScope, tenant string) bool {
	if !sc.all {
		return true // the SQL filtered by the list
	}
	return j.s.flags == nil || j.s.flags.Enabled(flagBlobScrub, tenant)
}

// RunOnce walks each backend's slice. A run never fails for a blob: a
// finding is a row, a backend that cannot be asked is a note.
func (j *BlobScrubJob) RunOnce(ctx context.Context) (BlobScrubResult, error) {
	now := j.now().UTC()
	res := BlobScrubResult{At: now, Month: now.Format("2006-01"), Cycle: j.Cycle.String(), Backends: map[string]*BlobScrubBack{}}
	prev := j.loadPrev(ctx)
	sc, err := j.scope(ctx)
	if err != nil {
		return res, fmt.Errorf("blob scrub: the flag's tenants: %w", err)
	}
	names := j.s.engine.GetDriverNames()
	sort.Strings(names)
	// Carry every backend's state, whether it is read this run or not.
	for _, b := range names {
		st := &BlobScrubBack{}
		if p, ok := prev.Backends[b]; ok && p != nil {
			st.Cursor, st.CycleStartedAt, st.LastCycleStartedAt, st.CycleCompletedAt = p.Cursor, p.CycleStartedAt, p.LastCycleStartedAt, p.CycleCompletedAt
			st.Stall = p.Stall
			if prev.Month == res.Month {
				st.MonthBytes = p.MonthBytes
			}
		}
		res.Backends[b] = st
	}
	if !sc.all && len(sc.tenants) == 0 {
		res.note("no tenant has the blob_scrub flag")
		return res, nil
	}
	budget := j.vendorBudget(ctx, &res, prev)
	for _, b := range names {
		if ctx.Err() != nil {
			res.note("the run's time ran out before " + b + "; the next run goes on")
			break
		}
		if scrubSkipBackends[b] {
			continue
		}
		if err := j.scrubBackend(ctx, b, sc, res.Backends[b], budget, &res); err != nil {
			if ctx.Err() != nil {
				res.note("the run's time ran out in " + b + "; the next run goes on")
				break
			}
			return res, err
		}
	}
	return res, nil
}

func (r *BlobScrubResult) note(s string) {
	if len(r.Notes) < maxBlobScrubNotes {
		r.Notes = append(r.Notes, s)
	}
}

// loadPrev is the last run's result (the walk's state).
func (j *BlobScrubJob) loadPrev(ctx context.Context) BlobScrubResult {
	var prev BlobScrubResult
	var raw []byte
	if err := j.s.db.QueryRowContext(ctx, `SELECT result FROM job_runs WHERE job = $1`, j.JobName).Scan(&raw); err == nil && len(raw) > 0 {
		_ = json.Unmarshal(raw, &prev)
	}
	return prev
}

// scrubAllowance is the vendor budget of the run, per plan ACCOUNT
// (planAccountOf): absent = unmetered; a nil map = nothing metered may be
// read (the plans could not be read).
type scrubAllowance struct {
	left map[string]int64
}

// remaining is what backend's account may still give this run.
func (a *scrubAllowance) remaining(backend string) (int64, bool) {
	if a.left == nil {
		return 0, true
	}
	n, metered := a.left[planAccountOf(backend)]
	return n, metered
}

func (a *scrubAllowance) spend(backend string, n int64) {
	if a.left == nil {
		return
	}
	if _, metered := a.left[planAccountOf(backend)]; metered {
		a.left[planAccountOf(backend)] -= n
	}
}

// vendorBudget reads the plans, the last capacity run, the blob copies'
// month and this job's month: free − max(the vendor's month-to-date, what
// this job and the copies read) per metered account.
func (j *BlobScrubJob) vendorBudget(ctx context.Context, res *BlobScrubResult, prev BlobScrubResult) *scrubAllowance {
	var capRes BackendCapacityResult
	haveCap := false
	var raw []byte
	if err := j.s.db.QueryRowContext(ctx, `SELECT result FROM job_runs WHERE job = $1`, j.capacityJob).Scan(&raw); err == nil && len(raw) > 0 {
		haveCap = json.Unmarshal(raw, &capRes) == nil
	}
	var copies BlobCopiesResult
	raw = nil
	if err := j.s.db.QueryRowContext(ctx, `SELECT result FROM job_runs WHERE job = $1`, j.copiesJob).Scan(&raw); err == nil && len(raw) > 0 {
		_ = json.Unmarshal(raw, &copies)
	}
	ours := map[string]int64{} // this job's month, per account
	if prev.Month == res.Month {
		for b, st := range prev.Backends {
			if st != nil {
				ours[planAccountOf(b)] += st.MonthBytes
			}
		}
	}
	rows, err := j.s.db.QueryContext(ctx, `SELECT backend, capacity_bytes, egress_free_multiple FROM backend_plans WHERE egress_free_multiple IS NOT NULL`)
	if err != nil {
		res.note("vendor_allowance: the plans could not be read — no metered backend is read: " + err.Error())
		return &scrubAllowance{}
	}
	defer func() { _ = rows.Close() }()
	a := &scrubAllowance{left: map[string]int64{}}
	for rows.Next() {
		var p BackendPlan
		var mult float64
		if rows.Scan(&p.Backend, &p.CapacityBytes, &mult) != nil {
			continue
		}
		p.EgressFreeMultiple = &mult
		if !haveCap {
			a.left[p.Backend] = 0 // unknown = no egress (the blob copies' rule)
			continue
		}
		free := egressFreeBytes(p, capRes.Backends)
		used := ours[p.Backend]
		if copies.Egress != nil && copies.Egress.Month == res.Month {
			used += copies.Egress.Spent[p.Backend]
		}
		if v, ok := capRes.Vendor[p.Backend]; ok && !v.EgressUnmetered {
			used = max(used, v.EgressMonthBytes)
		}
		a.left[p.Backend] = max(0, free-used)
	}
	if err := rows.Err(); err != nil {
		res.note("vendor_allowance: the plans could not be read — no metered backend is read: " + err.Error())
		return &scrubAllowance{}
	}
	return a
}

// due reports whether the backend's walk has work this run: a cycle in
// progress, or the last one began a cycle ago.
func (j *BlobScrubJob) due(st *BlobScrubBack, now time.Time) bool {
	if !st.Cursor.zero() || !st.CycleStartedAt.IsZero() {
		return true
	}
	return st.CycleCompletedAt.IsZero() || !now.Before(st.LastCycleStartedAt.Add(j.Cycle))
}

func (j *BlobScrubJob) scrubBackend(ctx context.Context, backend string, sc scrubScope, st *BlobScrubBack, allow *scrubAllowance, res *BlobScrubResult) error {
	now := j.now().UTC()
	if !j.due(st, now) {
		return nil
	}
	d, ok := j.s.engine.GetDriver(backend)
	if !ok {
		return nil
	}
	if j.s.engine.GetFailoverStatus()[backend] == engine.StateOpen.String() {
		st.Skipped = "breaker_open"
		res.note(backend + ": its breaker is open — not read this run")
		return nil
	}
	if left, metered := allow.remaining(backend); metered && left <= 0 {
		st.Skipped = "vendor_allowance"
		res.note(backend + ": the month's free egress is spent — not read (vendor_allowance)")
		return nil
	}
	footprint, err := j.footprint(ctx, backend, sc)
	if err != nil {
		return err
	}
	if footprint == 0 && st.Cursor.zero() {
		return nil
	}
	if st.CycleStartedAt.IsZero() {
		st.CycleStartedAt = now
	}
	budget := scrubBudget(footprint, j.Every, j.Cycle)
	st.Checked = map[string]int{}
	checked := 0
	for {
		kind := st.Cursor.Kind
		if kind == "" {
			kind = scrubKindWhole
		}
		page, err := j.page(ctx, backend, kind, st.Cursor, sc)
		if err != nil {
			return err
		}
		if len(page) == 0 {
			next := kindAfter(kind)
			if next == "" {
				// The walk is over: the cycle is done.
				st.LastCycleStartedAt, st.CycleCompletedAt = st.CycleStartedAt, j.now().UTC()
				st.CycleStartedAt, st.Cursor = time.Time{}, scrubWalk{}
				return nil
			}
			st.Cursor = scrubWalk{Kind: next}
			continue
		}
		for _, it := range page {
			if checked > 0 && st.Bytes >= budget {
				return nil
			}
			if ctx.Err() != nil {
				return nil
			}
			if left, metered := allow.remaining(backend); metered && it.size > left {
				st.Skipped = "vendor_allowance"
				res.note(fmt.Sprintf("%s: stopped at the month's free egress (vendor_allowance) after %d bytes", backend, st.Bytes))
				return nil
			}
			if it.kind != scrubKindChunk && !j.inScope(sc, it.tenant) {
				st.Cursor = it.walk()
				continue
			}
			verdict, detail, n := j.check(ctx, d, backend, it, st, res)
			allow.spend(backend, n)
			st.Bytes += n
			st.MonthBytes += n
			scrubBytesRead.WithLabelValues(backend).Add(float64(n))
			if verdict == scrubNoVerdict {
				// Could not be asked: this blob is read again next run —
				// up to scrubNoAnswerRuns runs in a row, then it is a
				// finding and the walk goes on (RP96-A-1: a blob the run
				// could never finish reading held the cursor for ever).
				if st.Stall == nil || st.Stall.Walk != it.walk() {
					st.Stall = &scrubStall{Walk: it.walk()}
				}
				st.Stall.Runs++
				if st.Stall.Runs < scrubNoAnswerRuns {
					res.note(fmt.Sprintf("%s: %s %s could not be read (no answer, run %d of %d) — the slice stops here",
						backend, it.kind, it.key+it.hash, st.Stall.Runs, scrubNoAnswerRuns))
					return nil
				}
				verdict = j.recordNoAnswer(ctx, backend, it, st, res, fmt.Sprintf("no answer in %d runs: %s", st.Stall.Runs, detail))
				st.Stall = nil
			} else if st.Stall != nil && st.Stall.Walk == it.walk() {
				st.Stall = nil
			}
			st.Cursor = it.walk()
			if verdict == scrubChanged {
				continue
			}
			st.Checked[it.kind]++
			checked++
			scrubChecks.WithLabelValues(backend).Inc()
		}
	}
}

func kindAfter(k string) string {
	for i, x := range scrubKinds {
		if x == k && i+1 < len(scrubKinds) {
			return scrubKinds[i+1]
		}
	}
	return ""
}

// The walk's queries. $2 = every tenant, $3 = the tenants (when not every).
const (
	scrubWholeSQL = `SELECT tenant_id, bucket, object_key, COALESCE(storage_key, ''), size_bytes, etag, COALESCE(body_md5, ''),
		encryption_algorithm, COALESCE(backend_etag, '')
		  FROM object_head_cache
		 WHERE backend_name = $1 AND NOT is_chunked AND size_bytes > 0 AND ($2 OR tenant_id = ANY($3))
		   AND (tenant_id, bucket, object_key) > ($4, $5, $6)
		 ORDER BY tenant_id, bucket, object_key LIMIT $7`
	scrubVersionSQL = `SELECT v.tenant_id, v.bucket, v.object_key, v.version_id, COALESCE(v.storage_key, ''), v.size_bytes, v.etag,
		COALESCE(v.body_md5, ''), COALESCE(v.encryption_algorithm, '')
		  FROM ` + usage.RetainedVersionsFrom + `
		   AND v.backend_name = $1 AND v.size_bytes > 0 AND ($2 OR v.tenant_id = ANY($3))
		   AND (v.tenant_id, v.bucket, v.object_key, v.version_id) > ($4, $5, $6, $7)
		 ORDER BY v.tenant_id, v.bucket, v.object_key, v.version_id LIMIT $8`
	scrubChunkSQL = `SELECT g.dedup_scope, g.plaintext_hash, g.storage_key, g.size_bytes, COALESCE(g.compressed_size, 0),
		COALESCE(g.compression_algo, ''), g.encrypted, COALESCE(g.encryption_algo, ''), COALESCE(g.ciphertext_hash, ''),
		COALESCE(g.encryption_key_version, 0)
		  FROM global_content_index g
		 WHERE g.backend_id = $1 AND g.state = 'ready' AND NOT g.marked_for_deletion
		   AND ($2 OR EXISTS (SELECT 1 FROM tenant_chunk_refs r WHERE r.dedup_scope = g.dedup_scope AND r.plaintext_hash = g.plaintext_hash
		                       AND r.tenant_id = ANY($3)))
		   AND (g.dedup_scope, g.plaintext_hash) > ($4, $5)
		 ORDER BY g.dedup_scope, g.plaintext_hash LIMIT $6`
	scrubFootprintSQL = `SELECT
		COALESCE((SELECT SUM(size_bytes) FROM object_head_cache
		           WHERE backend_name = $1 AND NOT is_chunked AND size_bytes > 0 AND ($2 OR tenant_id = ANY($3))), 0)
	  + COALESCE((SELECT SUM(v.size_bytes) FROM ` + usage.RetainedVersionsFrom + `
		           AND v.backend_name = $1 AND v.size_bytes > 0 AND ($2 OR v.tenant_id = ANY($3))), 0)
	  + COALESCE((SELECT SUM(COALESCE(NULLIF(g.compressed_size, 0), g.size_bytes)) FROM global_content_index g
		           WHERE g.backend_id = $1 AND g.state = 'ready' AND NOT g.marked_for_deletion
		             AND ($2 OR EXISTS (SELECT 1 FROM tenant_chunk_refs r WHERE r.dedup_scope = g.dedup_scope
		                                AND r.plaintext_hash = g.plaintext_hash AND r.tenant_id = ANY($3)))), 0)`
)

// footprint is the bytes a full cycle reads on backend.
func (j *BlobScrubJob) footprint(ctx context.Context, backend string, sc scrubScope) (int64, error) {
	var n int64
	if err := j.s.db.QueryRowContext(ctx, scrubFootprintSQL, backend, sc.all, pq.Array(sc.tenants)).Scan(&n); err != nil {
		return 0, fmt.Errorf("blob scrub: the footprint of %s: %w", backend, err)
	}
	return n, nil
}

// page is the next blobs of kind after the cursor.
func (j *BlobScrubJob) page(ctx context.Context, backend, kind string, c scrubWalk, sc scrubScope) ([]scrubItem, error) {
	if c.Kind != kind {
		c = scrubWalk{Kind: kind}
	}
	var rows *sql.Rows
	var err error
	switch kind {
	case scrubKindWhole:
		rows, err = j.s.db.QueryContext(ctx, scrubWholeSQL, backend, sc.all, pq.Array(sc.tenants), c.Tenant, c.Bucket, c.Key, blobScrubPage)
	case scrubKindVersion:
		rows, err = j.s.db.QueryContext(ctx, scrubVersionSQL, backend, sc.all, pq.Array(sc.tenants), c.Tenant, c.Bucket, c.Key, c.Version, blobScrubPage)
	default:
		rows, err = j.s.db.QueryContext(ctx, scrubChunkSQL, backend, sc.all, pq.Array(sc.tenants), c.Tenant, c.Key, blobScrubPage)
	}
	if err != nil {
		return nil, fmt.Errorf("blob scrub: the %s walk of %s: %w", kind, backend, err)
	}
	defer func() { _ = rows.Close() }()
	var out []scrubItem
	for rows.Next() {
		it := scrubItem{kind: kind}
		switch kind {
		case scrubKindWhole:
			var etag, bodyMD5, enc, backendETag string
			if err := rows.Scan(&it.tenant, &it.bucket, &it.key, &it.storageKey, &it.size, &etag, &bodyMD5, &enc, &backendETag); err != nil {
				return nil, fmt.Errorf("blob scrub: scan: %w", err)
			}
			it.digest = scrubDigest(enc, bodyMD5, etag, backendETag)
			it.sealed = enc != ""
		case scrubKindVersion:
			var etag, bodyMD5, enc string
			if err := rows.Scan(&it.tenant, &it.bucket, &it.key, &it.verID, &it.storageKey, &it.size, &etag, &bodyMD5, &enc); err != nil {
				return nil, fmt.Errorf("blob scrub: scan: %w", err)
			}
			it.digest = scrubDigest(enc, bodyMD5, etag, "") // a version records no backend ETag: a sealed one is unverifiable
			it.sealed = enc != ""
		default:
			var compressedSize int64
			var algo string
			if err := rows.Scan(&it.scope, &it.hash, &it.storageKey, &it.size, &compressedSize, &algo, &it.encrypted, &it.encAlgo,
				&it.ciphertextHash, &it.keyVersion); err != nil {
				return nil, fmt.Errorf("blob scrub: scan: %w", err)
			}
			it.compressed = algo != "" && algo != "none"
			if compressedSize > 0 {
				it.size = compressedSize // what is read
			}
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

// scrubDigest is the MD5 a whole blob's stored bytes must have: the body's
// for a plaintext object (body_md5, else a single-part ETag), the backend's
// own ETag of the stored bytes for a sealed one; "" = unverifiable.
func scrubDigest(encryption, bodyMD5, etag, backendETag string) string {
	if encryption == "" {
		if isMD5Hex(bodyMD5) {
			return strings.ToLower(bodyMD5)
		}
		if e := strings.Trim(etag, `"`); isMD5Hex(e) {
			return strings.ToLower(e)
		}
		return ""
	}
	if e := strings.Trim(backendETag, `"`); isMD5Hex(e) {
		return strings.ToLower(e)
	}
	return ""
}

func isMD5Hex(s string) bool {
	if len(s) != 32 {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil
}

// check reads one blob whole and judges it; a finding is written for what
// is not ok, and a repair tried. n = the bytes read; detail says why for a
// verdict that is not ok (a no-answer's error too).
func (j *BlobScrubJob) check(ctx context.Context, d engine.Driver, backend string, it scrubItem, st *BlobScrubBack, res *BlobScrubResult) (string, string, int64) {
	verdict, detail, n := j.read(ctx, d, backend, it)
	switch verdict {
	case scrubOK, scrubNoVerdict:
		return verdict, detail, n
	case scrubUnverifiable:
		st.Unverifiable++
		return verdict, "", n
	}
	// Not ok: is the row still the one read? A write or a delete that moved
	// it while the blob was read is no finding.
	same, err := j.stillNamed(ctx, backend, it)
	if err != nil {
		res.note(fmt.Sprintf("%s: the row of %s could not be read again: %v", backend, it.key+it.hash, err))
		return scrubNoVerdict, err.Error(), n
	}
	if !same {
		return scrubChanged, "", n
	}
	j.finding(ctx, backend, it, st, res, verdict, detail)
	_, _, artifact := it.address(ctx)
	if leg, ok := j.repair(ctx, d, backend, it); ok {
		st.Repaired++
		j.record(ctx, it, backend, artifact, scrubRepaired, leg, "rewritten from the mirror copy on "+leg, res)
		j.s.log().Info("blob scrub: repaired from the mirror", zap.String("backend", backend), zap.String("leg", leg),
			zap.String("tenant_id", it.tenant), zap.String("key", it.key), zap.String("chunk", shortScrubHash(it.hash)))
	}
	return verdict, detail, n
}

// finding counts and writes a blob that is not what its row says.
func (j *BlobScrubJob) finding(ctx context.Context, backend string, it scrubItem, st *BlobScrubBack, res *BlobScrubResult, verdict, detail string) {
	st.Failed++
	scrubFailures.WithLabelValues(backend).Inc()
	_, _, artifact := it.address(ctx)
	j.s.log().Warn("blob scrub: a blob is not what its row says", zap.String("backend", backend), zap.String("kind", it.kind),
		zap.String("tenant_id", it.tenant), zap.String("bucket", it.bucket), zap.String("key", it.key), zap.String("chunk", shortScrubHash(it.hash)),
		zap.String("verdict", verdict), zap.String("detail", detail))
	j.record(ctx, it, backend, artifact, verdict, "", detail, res)
}

// recordNoAnswer is a blob that gave no answer scrubNoAnswerRuns runs in a
// row: `unreadable` to us (the detail says why — a read longer than the run,
// a timeout, an archived answer), a finding the walk goes past. No repair: the
// bytes may well be whole. The verdict returned is the loop's (changed when
// the row moved meanwhile).
func (j *BlobScrubJob) recordNoAnswer(ctx context.Context, backend string, it scrubItem, st *BlobScrubBack, res *BlobScrubResult, detail string) string {
	same, err := j.stillNamed(ctx, backend, it)
	if err != nil || !same {
		return scrubChanged
	}
	j.finding(ctx, backend, it, st, res, scrubUnreadable, detail)
	res.note(fmt.Sprintf("%s: %s %s gave no answer in %d runs — recorded unreadable, the walk goes on", backend, it.kind, it.key+it.hash, scrubNoAnswerRuns))
	return scrubUnreadable
}

func shortScrubHash(h string) string {
	if len(h) > 16 {
		return h[:16]
	}
	return h
}

// scrubReadTimeout bounds one blob's read: a minute plus the time to read
// it at 4 MiB/s.
func scrubReadTimeout(size int64) time.Duration {
	return time.Minute + time.Duration(size/(4<<20))*time.Second
}

// read is the blob's verdict: ok, mismatch, unreadable, missing,
// unverifiable, or no verdict (no answer).
func (j *BlobScrubJob) read(ctx context.Context, d engine.Driver, backend string, it scrubItem) (verdict, detail string, n int64) {
	cctx, cancel := context.WithTimeout(ctx, scrubReadTimeout(it.size))
	defer cancel()
	actx, container, artifact := it.address(cctx)
	rc, err := d.Get(actx, container, artifact)
	if err != nil && it.kind == scrubKindChunk && scrubReadVerdict(err) == scrubMissing {
		// A chunk written before WP-R8-7 may still sit under an uploader's
		// prefix until the chunk move (routing truth's `legacy`): read and
		// check it there — never a `missing` finding for a blob that is.
		if lrc, ok := j.legacyChunk(cctx, d, backend, it); ok {
			rc, err = lrc, nil
		}
	}
	if err != nil {
		return scrubReadVerdict(err), err.Error(), 0
	}
	defer func() { _ = rc.Close() }()
	if it.kind == scrubKindChunk {
		blob, rerr := io.ReadAll(io.LimitReader(rc, maxChunkBlobBytes+1))
		n = int64(len(blob))
		if rerr != nil {
			return scrubReadVerdict(rerr), rerr.Error(), n
		}
		if len(blob) > maxChunkBlobBytes {
			return scrubMismatch, "a chunk blob larger than any chunk", n
		}
		v, why := j.verifyChunk(ctx, it, blob)
		return v, why, n
	}
	sum := md5.New() // #nosec G401 -- the S3 ETag
	cr := &countingReader{r: io.TeeReader(rc, sum)}
	br := bufio.NewReader(cr)
	// What the stored length must be: the row's size for a plaintext blob;
	// for a sealed one (RP96-1) the streaming format's length — the header
	// the blob carries plus a tag per segment — read through the hash. A
	// sealed blob in another format (the legacy whole-buffer SSE-C, the
	// 0x05 multipart container) has no length the row can predict: only its
	// digest is checked, and it must be at least its plaintext.
	expected, sizeKnown := it.size, true
	if it.sealed {
		sizeKnown = false
		if lead, perr := br.Peek(1); perr == nil && (lead[0] == crypto.StreamFormat || lead[0] == crypto.StreamFormatV3) {
			h, herr := crypto.ReadStreamHeader(br)
			if herr != nil {
				return scrubMismatch, "a sealed blob whose header does not parse: " + herr.Error(), cr.n
			}
			expected, sizeKnown = crypto.StreamCiphertextLength(len(h.Raw), h.SegmentSize, it.size), true
		}
	}
	if _, err = io.Copy(io.Discard, br); err != nil {
		return scrubReadVerdict(err), err.Error(), cr.n
	}
	n = cr.n
	if sizeKnown && n != expected {
		if it.sealed {
			return scrubMismatch, fmt.Sprintf("%d bytes, the row's %d seal to %d", n, it.size, expected), n
		}
		return scrubMismatch, fmt.Sprintf("%d bytes, the row says %d", n, it.size), n
	}
	if !sizeKnown && n < it.size {
		return scrubMismatch, fmt.Sprintf("%d bytes, shorter than the row's %d plaintext", n, it.size), n
	}
	if it.digest == "" {
		return scrubUnverifiable, "", n
	}
	if got := hex.EncodeToString(sum.Sum(nil)); got != it.digest {
		return scrubMismatch, "md5 " + got + ", the row says " + it.digest, n
	}
	return scrubOK, "", n
}

// legacyChunk opens a chunk at a legacy (uploader-prefix) address, the
// driver called directly.
func (j *BlobScrubJob) legacyChunk(ctx context.Context, d engine.Driver, backend string, it scrubItem) (io.ReadCloser, bool) {
	cs := newChunkStore(j.s.engine, j.s.db, j.s.log())
	tenants, err := cs.legacyTenants(ctx, chunkAddr{scope: it.scope, hash: it.hash, backend: backend, key: it.storageKey})
	if err != nil {
		return nil, false
	}
	for _, tid := range tenants {
		if rc, err := d.Get(engine.LegacyChunkContext(ctx, tid), engine.ChunkContainer, it.storageKey); err == nil {
			return rc, true
		}
	}
	return nil, false
}

// scrubReadVerdict is readProbe's classification of a failed read: a tape
// object (ErrArchived) is no verdict here (the tape audit's).
func scrubReadVerdict(err error) string {
	switch classifyReadError(err) {
	case "missing":
		return scrubMissing
	case "error", "present":
		return scrubNoVerdict
	}
	return scrubUnreadable
}

// verifyChunk checks a chunk blob as its kind allows.
func (j *BlobScrubJob) verifyChunk(ctx context.Context, it scrubItem, blob []byte) (string, string) {
	switch {
	case it.encrypted && it.encAlgo == crypto.EdgeChunkEncryptionAlgo:
		if j.s.tenantKeys != nil {
			desc := chunkDesc{scope: it.scope, storageKey: it.storageKey, plaintextHash: it.hash, encrypted: true,
				encAlgo: crypto.EdgeChunkEncryptionAlgo, ciphertextHash: it.ciphertextHash, keyVersion: it.keyVersion}
			if _, err := openEdgeCodecBlob(ctx, j.s.tenantKeys, desc, it.scope, blob); err != nil {
				return scrubMismatch, "does not open: " + err.Error()
			}
			return scrubOK, ""
		}
		return compareDigest(md5.New(), blob, it.ciphertextHash) // #nosec G401
	case it.encrypted:
		return compareDigest(sha256.New(), blob, it.ciphertextHash)
	}
	data := blob
	if it.compressed {
		var err error
		if data, err = crypto.DecompressBuffer(blob); err != nil {
			return scrubMismatch, "does not decompress: " + err.Error()
		}
	}
	return compareDigest(sha256.New(), data, it.hash)
}

func compareDigest(h hash.Hash, b []byte, want string) (string, string) {
	if want == "" {
		return scrubUnverifiable, ""
	}
	h.Write(b)
	if got := hex.EncodeToString(h.Sum(nil)); !strings.EqualFold(got, strings.Trim(want, `"`)) {
		return scrubMismatch, "digest " + got + ", the row says " + want
	}
	return scrubOK, ""
}

// stillNamed re-reads the row: the same address on the same backend.
func (j *BlobScrubJob) stillNamed(ctx context.Context, backend string, it scrubItem) (bool, error) {
	var one int
	var err error
	switch it.kind {
	case scrubKindWhole:
		err = j.s.db.QueryRowContext(ctx, `SELECT 1 FROM object_head_cache WHERE tenant_id = $1 AND bucket = $2 AND object_key = $3
			AND COALESCE(storage_key, '') = $4 AND backend_name = $5`, it.tenant, it.bucket, it.key, it.storageKey, backend).Scan(&one)
	case scrubKindVersion:
		err = j.s.db.QueryRowContext(ctx, `SELECT 1 FROM object_versions WHERE tenant_id = $1 AND bucket = $2 AND object_key = $3
			AND version_id = $4 AND COALESCE(storage_key, '') = $5 AND backend_name = $6`, it.tenant, it.bucket, it.key, it.verID, it.storageKey, backend).Scan(&one)
	default:
		err = j.s.db.QueryRowContext(ctx, `SELECT 1 FROM global_content_index WHERE dedup_scope = $1 AND plaintext_hash = $2
			AND storage_key = $3 AND backend_id = $4 AND state = 'ready' AND NOT marked_for_deletion`, it.scope, it.hash, it.storageKey, backend).Scan(&one)
	}
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

// record writes a finding: a whole blob's or a version's for its row, a
// chunk's for every object that references it.
func (j *BlobScrubJob) record(ctx context.Context, it scrubItem, backend, address, verdict, source, detail string, res *BlobScrubResult) {
	var src any
	if source != "" {
		src = source
	}
	if it.kind != scrubKindChunk {
		if _, err := j.s.db.ExecContext(ctx, `INSERT INTO scrub_findings (tenant_id, bucket, object_key, address, backend, kind, verdict, source, detail)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`, it.tenant, it.bucket, it.key, address, backend, it.kind, verdict, src, detail); err != nil {
			res.note("a finding could not be written: " + err.Error())
		}
		return
	}
	if _, err := j.s.db.ExecContext(ctx, `INSERT INTO scrub_findings (tenant_id, bucket, object_key, address, backend, kind, verdict, source, detail)
		SELECT r.tenant_id, r.bucket_name, r.object_key, $3, $4, 'chunk', $5, $6, $7
		  FROM tenant_chunk_refs r WHERE r.dedup_scope = $1 AND r.plaintext_hash = $2
		 ORDER BY r.tenant_id, r.bucket_name, r.object_key LIMIT $8`,
		it.scope, it.hash, address, backend, verdict, src, detail, maxScrubChunkFindings); err != nil {
		res.note("a chunk's findings could not be written: " + err.Error())
	}
}

// repair rewrites a bad blob from its ready mirror copy, at the same
// address on the recorded backend. The copy's bytes are checked against the
// copy row's MD5 and size AS THEY STREAM: the reader fails with the last
// byte when they differ, so the backend write is never completed with
// bytes that are not the copy row's.
func (j *BlobScrubJob) repair(ctx context.Context, d engine.Driver, backend string, it scrubItem) (string, bool) {
	actx, container, artifact := it.address(ctx)
	var leg, etag string
	var size int64
	q := `SELECT leg, etag, size FROM blob_copies WHERE tenant_id = $1 AND bucket = $2 AND storage_key = $3 AND source_backend = $4
		AND kind = 'mirror' AND state = 'ready' ORDER BY leg LIMIT 1`
	args := []any{it.tenant, it.bucket, artifact, backend}
	if it.kind == scrubKindChunk {
		q = `SELECT leg, etag, size FROM blob_copies WHERE bucket = '' AND storage_key = $1 AND source_backend = $2
			AND kind = 'mirror' AND state = 'ready' ORDER BY leg LIMIT 1`
		args = []any{artifact, backend}
	}
	if err := j.s.db.QueryRowContext(ctx, q, args...).Scan(&leg, &etag, &size); err != nil {
		return "", false
	}
	if !isMD5Hex(strings.Trim(etag, `"`)) || leg == backend {
		return "", false
	}
	src, ok := j.s.engine.GetDriver(leg)
	if !ok || j.s.engine.GetFailoverStatus()[leg] == engine.StateOpen.String() {
		return "", false
	}
	cctx, cancel := context.WithTimeout(actx, 2*scrubReadTimeout(size))
	defer cancel()
	body, err := src.Get(cctx, container, artifact)
	if err != nil {
		return "", false
	}
	defer func() { _ = body.Close() }()
	vr := &verifyingReader{r: body, want: strings.ToLower(strings.Trim(etag, `"`)), size: size, h: md5.New()} // #nosec G401
	if err := d.Put(cctx, container, artifact, vr, engine.WithContentLength(size)); err != nil || !vr.ok {
		j.s.log().Warn("blob scrub: the mirror copy did not verify or could not be written — the blob is left as it was",
			zap.String("backend", backend), zap.String("leg", leg), zap.String("tenant_id", it.tenant), zap.String("key", it.key), zap.Error(err))
		return "", false
	}
	return leg, true
}

// verifyingReader passes size bytes through and fails WITH the last one when
// their MD5 or their count is not the wanted one.
type verifyingReader struct {
	r    io.Reader
	want string
	size int64
	h    hash.Hash
	n    int64
	ok   bool
}

var errScrubCopyDiffers = errors.New("blob scrub: the mirror copy is not the bytes its row records")

func (v *verifyingReader) Read(p []byte) (int, error) {
	if v.n >= v.size {
		if v.ok {
			return 0, io.EOF
		}
		return 0, errScrubCopyDiffers
	}
	if int64(len(p)) > v.size-v.n {
		p = p[:v.size-v.n]
	}
	n, err := v.r.Read(p)
	v.h.Write(p[:n])
	v.n += int64(n)
	if v.n == v.size {
		if hex.EncodeToString(v.h.Sum(nil)) != v.want {
			return n, errScrubCopyDiffers
		}
		v.ok = true
		return n, nil
	}
	if errors.Is(err, io.EOF) {
		return n, errScrubCopyDiffers // short
	}
	return n, err
}

// --- the gauges the alerts read (from job_runs, on scrape) ---

type blobScrubCollector struct {
	j                       *BlobScrubJob
	completed, started, cyc *prometheus.Desc
}

func newBlobScrubCollector(j *BlobScrubJob) *blobScrubCollector {
	return &blobScrubCollector{j: j,
		completed: prometheus.NewDesc("vaultaire_scrub_cycle_completed_timestamp_seconds",
			"When the blob_scrub job last finished a cycle over the backend (every blob read once); read from job_runs.", []string{"backend"}, nil),
		started: prometheus.NewDesc("vaultaire_scrub_cycle_started_timestamp_seconds",
			"When the cycle in progress over the backend began (absent between cycles); read from job_runs.", []string{"backend"}, nil),
		cyc: prometheus.NewDesc("vaultaire_scrub_cycle_seconds", "The blob scrub's cycle (SCRUB_CYCLE).", nil, nil)}
}

func (c *blobScrubCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.completed
	ch <- c.started
	ch <- c.cyc
}

func (c *blobScrubCollector) Collect(ch chan<- prometheus.Metric) {
	ch <- prometheus.MustNewConstMetric(c.cyc, prometheus.GaugeValue, c.j.Cycle.Seconds())
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	prev := c.j.loadPrev(ctx)
	for b, st := range prev.Backends {
		if st == nil {
			continue
		}
		if !st.CycleCompletedAt.IsZero() {
			ch <- prometheus.MustNewConstMetric(c.completed, prometheus.GaugeValue, float64(st.CycleCompletedAt.Unix()), b)
		}
		if !st.CycleStartedAt.IsZero() {
			ch <- prometheus.MustNewConstMetric(c.started, prometheus.GaugeValue, float64(st.CycleStartedAt.Unix()), b)
		}
	}
}
