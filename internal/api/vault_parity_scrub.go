package api

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/lib/pq"
	"github.com/prometheus/client_golang/prometheus"
	"go.uber.org/zap"

	"github.com/FairForge/vaultaire/internal/common"
	"github.com/FairForge/vaultaire/internal/engine"
)

// The scrub pass of the vault_parity job (WP-VAULT-2, docs/reviews/WP-VAULT-2.md
// § 1–4 and the amendments of § 7). Nothing read a parity shard until the day
// Geyser failed — the job wrote four shards (a PUT 200) and the reconcile
// LISTED folders — and on 2026-10-21 Lyve answered HEAD 200 and GET 500 on
// week-old blobs. A copy that cannot be read is no copy.
//
// A pass, when due (ScrubEvery, after the reconcile and before the protect,
// in its own slice):
//
//   - reads every parity shard of `complete` rows end to end, in key order
//     from a cursor carried in job_runs.result that wraps at the end. The
//     bytes it reads are the footprint over the cycle (A1: every row once per
//     ScrubCycle), at least one row, at most the slice;
//   - checks each shard's length and, where the row's digests belong to its
//     etag, its SHA-256 (verdicts ok | unreadable | missing | corrupt). An
//     unreadable shard is read once more after ScrubRetryAfter; only the
//     second failure counts. A row without trusted digests is baselined with
//     what was read. A leg not registered or whose breaker is open is no
//     verdict: the row is skipped;
//   - rebuilds ONE object from its four parity shards alone and hashes the
//     result against the stored bytes' SHA-256 protect recorded (A2): a
//     parity computed wrong passes every digest; only this catches it;
//   - makes a row that failed `partial` with its attempts reset and
//     scrub_failures + 1, so the protect pass rewrites its four shards. The
//     row keeps naming every shard: the delete paths still erase what is
//     there, and the rewrite replaces it in place.
//
// Nothing here fails the run: a finding is a note and a metric.

const (
	defaultScrubEvery      = 30 * time.Minute
	defaultScrubCycle      = 30 * 24 * time.Hour
	defaultScrubSlice      = 10 * time.Minute
	defaultScrubRetryAfter = 30 * time.Second
	// restoreRecheck: a protect that found its object on tape asks again
	// (or looks whether the restore landed) no sooner than this.
	restoreRecheck = 10 * time.Minute
	// restoreDays: how long the restored copy stays readable — the protect
	// that follows reads it within minutes.
	restoreDays = 2
	// scrubPage: rows read per query.
	scrubPage = 50
	// maxScrubFindings: findings named in a result.
	maxScrubFindings = 20
)

var (
	vaultParityScrubShards = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "vaultaire_vault_parity_scrub_shards_total",
		Help: "Parity shards the vault_parity job's scrub read end to end, by verdict: ok, unreadable (the leg answered and the read failed twice), missing (the leg says it is not there), corrupt (wrong length, or a SHA-256 that is not the one protect recorded).",
	}, []string{"outcome"})
	vaultParityScrubRows = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "vaultaire_vault_parity_scrub_rows_total",
		Help: "Parity rows the scrub visited: ok, baselined (no digests of its etag yet: the scrub recorded what it read), reprotect (a shard failed: made partial for the protect to rewrite), skipped (its leg is not registered or its breaker is open — no verdict), changed (the row moved under the scrub — a delete or an overwrite).",
	}, []string{"outcome"})
	vaultParityRebuilds = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "vaultaire_vault_parity_rebuilds_total",
		Help: "Objects the scrub rebuilt from their four parity shards alone and hashed against the stored bytes' SHA-256: ok, mismatch (the parity does not rebuild the object: re-protected), error (a shard could not be read — the shard scrub judges that).",
	}, []string{"outcome"})
	vaultParityRestores = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "vaultaire_vault_parity_restores_total",
		Help: "Restores the vault_parity job asked of the object's backend because the object was on tape when it came to protect it: requested, in_progress (one was running already), error.",
	}, []string{"outcome"})
)

func init() {
	for _, o := range []string{"ok", "unreadable", "missing", "corrupt"} {
		vaultParityScrubShards.WithLabelValues(o)
	}
	for _, o := range []string{"ok", "baselined", "reprotect", "skipped", "changed"} {
		vaultParityScrubRows.WithLabelValues(o)
	}
	for _, o := range []string{"ok", "mismatch", "error"} {
		vaultParityRebuilds.WithLabelValues(o)
	}
	for _, o := range []string{"requested", "in_progress", "error"} {
		vaultParityRestores.WithLabelValues(o)
	}
}

// scrubEveryFromEnv is VAULT_PARITY_SCRUB_EVERY (1m–24h), else the default;
// a rejected value is logged at Warn and the default kept.
func scrubEveryFromEnv(getenv func(string) string, logger *zap.Logger) time.Duration {
	raw := strings.TrimSpace(getenv("VAULT_PARITY_SCRUB_EVERY"))
	if raw == "" {
		return defaultScrubEvery
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d < time.Minute || d > 24*time.Hour {
		logger.Warn("VAULT_PARITY_SCRUB_EVERY rejected; default kept", zap.String("value", raw), zap.Duration("default", defaultScrubEvery))
		return defaultScrubEvery
	}
	return d
}

// scrubCycleFromEnv is VAULT_PARITY_SCRUB_CYCLE: a Go duration or whole days
// ("30d"), 1d–180d, else the default; a rejected value is logged at Warn.
func scrubCycleFromEnv(getenv func(string) string, logger *zap.Logger) time.Duration {
	raw := strings.TrimSpace(getenv("VAULT_PARITY_SCRUB_CYCLE"))
	if raw == "" {
		return defaultScrubCycle
	}
	var d time.Duration
	var err error
	if days, ok := strings.CutSuffix(raw, "d"); ok {
		var n int
		n, err = strconv.Atoi(days)
		d = time.Duration(n) * 24 * time.Hour
	} else {
		d, err = time.ParseDuration(raw)
	}
	if err != nil || d < 24*time.Hour || d > 180*24*time.Hour {
		logger.Warn("VAULT_PARITY_SCRUB_CYCLE rejected; default kept", zap.String("value", raw), zap.Duration("default", defaultScrubCycle))
		return defaultScrubCycle
	}
	return d
}

// scrubBudget is the bytes one pass reads: the footprint's share of the
// cycle that one interval is, at least 1 (a pass always moves one row), at
// most the footprint.
func scrubBudget(footprint int64, every, cycle time.Duration) int64 {
	if footprint <= 0 {
		return 0
	}
	if cycle <= 0 || every >= cycle {
		return footprint
	}
	b := int64(float64(footprint) * (float64(every) / float64(cycle)))
	return min(max(b, 1), footprint)
}

// scrubCursor is a row key: the walk resumes after it. Zero = the start.
type scrubCursor struct {
	Tenant string `json:"tenant,omitempty"`
	Bucket string `json:"bucket,omitempty"`
	Key    string `json:"key,omitempty"`
}

func (c scrubCursor) zero() bool { return c == scrubCursor{} }

type scrubState struct {
	At      time.Time
	Cursor  scrubCursor
	Rebuild scrubCursor
}

func (p *VaultParity) scrubEvery() time.Duration {
	if p.ScrubEvery <= 0 {
		return defaultScrubEvery
	}
	return p.ScrubEvery
}

func (p *VaultParity) scrubCycle() time.Duration {
	if p.ScrubCycle <= 0 {
		return defaultScrubCycle
	}
	return p.ScrubCycle
}

func (p *VaultParity) scrubSlice() time.Duration {
	if p.ScrubSlice <= 0 {
		return defaultScrubSlice
	}
	return p.ScrubSlice
}

// loadScrubState is the scrub state of the last run: job_runs.result, or the
// in-memory copy when it is newer or the table has none (the reconcile's
// rule).
func (p *VaultParity) loadScrubState(ctx context.Context) scrubState {
	st := p.scrubMem
	var raw []byte
	if err := p.db.QueryRowContext(ctx, `SELECT result FROM job_runs WHERE job = $1`, p.JobName).Scan(&raw); err != nil || len(raw) == 0 {
		return st
	}
	var last VaultParityResult
	if json.Unmarshal(raw, &last) != nil || last.ScrubAt.IsZero() || last.ScrubAt.Before(st.At) {
		return st
	}
	return scrubState{At: last.ScrubAt, Cursor: last.ScrubCursor, Rebuild: last.RebuildCursor}
}

// scrubRowData is one complete row as the scrub reads it.
type scrubRowData struct {
	parityRow
	digests    []string
	digestEtag string
	objectSHA  string
	updatedAt  time.Time
}

func (r scrubRowData) cursor() scrubCursor {
	return scrubCursor{Tenant: r.tenantID, Bucket: r.bucket, Key: r.key}
}

// trusted: the row's digests belong to its etag (an older build's protect
// of a new etag leaves the old etag's digests behind — migration 118).
func (r scrubRowData) trusted() bool {
	return r.digestEtag == r.etag && len(r.digests) == r.m
}

// scrubRows reads complete rows after `after` (from the start when zero),
// up to and including `until` when it is set (the wrapped half of a pass);
// hashed keeps only rows a rebuild can be checked against (a stored-bytes
// hash of their etag).
func (p *VaultParity) scrubRows(ctx context.Context, after scrubCursor, until *scrubCursor, hashed bool, limit int) ([]scrubRowData, error) {
	var u scrubCursor
	if until != nil {
		u = *until
	}
	rows, err := p.db.QueryContext(ctx, `
		SELECT tenant_id, bucket, object_key, etag, size_bytes, data_shards, parity_shards, stripe_bytes, shard_bytes,
		       shard_prefix, legs, shard_digests, COALESCE(digest_etag, ''), COALESCE(object_sha256, ''), updated_at
		FROM vault_parity
		WHERE state = 'complete' AND ($1 = '' OR tenant_id = $1)
		  AND ($2::BOOLEAN OR (tenant_id, bucket, object_key) > ($3::TEXT, $4::TEXT, $5::TEXT))
		  AND (NOT $6::BOOLEAN OR (tenant_id, bucket, object_key) <= ($7::TEXT, $8::TEXT, $9::TEXT))
		  AND (NOT $11::BOOLEAN OR (object_sha256 IS NOT NULL AND digest_etag = etag))
		ORDER BY tenant_id, bucket, object_key
		LIMIT $10`, p.scopeTenant, after.zero(), after.Tenant, after.Bucket, after.Key,
		until != nil, u.Tenant, u.Bucket, u.Key, limit, hashed)
	if err != nil {
		return nil, fmt.Errorf("vault parity scrub: rows: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []scrubRowData
	for rows.Next() {
		var r scrubRowData
		var legs, digests pq.StringArray
		if err := rows.Scan(&r.tenantID, &r.bucket, &r.key, &r.etag, &r.size, &r.k, &r.m, &r.stripe, &r.shardBytes,
			&r.prefix, &legs, &digests, &r.digestEtag, &r.objectSHA, &r.updatedAt); err != nil {
			return nil, fmt.Errorf("vault parity scrub: scan: %w", err)
		}
		r.legs, r.digests, r.state = []string(legs), []string(digests), "complete"
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("vault parity scrub: rows: %w", err)
	}
	return out, nil
}

// scrubFootprint is the parity bytes of every complete row.
func (p *VaultParity) scrubFootprint(ctx context.Context) (int64, error) {
	var n int64
	err := p.db.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(shard_bytes * parity_shards), 0)::BIGINT FROM vault_parity
		WHERE state = 'complete' AND ($1 = '' OR tenant_id = $1)`, p.scopeTenant).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("vault parity scrub: footprint: %w", err)
	}
	return n, nil
}

// scrubPass is one pass: the shards of its budget, then one rebuild.
func (p *VaultParity) scrubPass(ctx context.Context, res *VaultParityResult, st scrubState) {
	footprint, err := p.scrubFootprint(ctx)
	if err != nil {
		res.Errors = append(res.Errors, err.Error())
		return
	}
	budget := scrubBudget(footprint, p.scrubEvery(), p.scrubCycle())
	if p.scrubBudgetOverride > 0 {
		budget = p.scrubBudgetOverride
	}
	res.ScrubBudget = budget
	status := p.eng.GetFailoverStatus()

	cur, start := st.Cursor, st.Cursor
	var until *scrubCursor
	var spent int64
walk:
	// Checked before every query: a pass whose budget is spent never wraps
	// (the next one does, from where this one stopped).
	for spent < budget && ctx.Err() == nil {
		rows, err := p.scrubRows(ctx, cur, until, false, scrubPage)
		if err != nil {
			res.Errors = append(res.Errors, err.Error())
			break
		}
		if len(rows) == 0 {
			if until != nil || start.zero() {
				break // a whole cycle was read, or there was nothing
			}
			// Wrap: from the start up to where this pass began.
			res.ScrubWrapped, cur, until = true, scrubCursor{}, &start
			continue
		}
		for _, r := range rows {
			if spent >= budget || ctx.Err() != nil {
				break walk
			}
			// A row whose read is estimated longer than a whole slice would
			// be cut on every pass and hold the cursor for ever: it is named
			// and passed over. One that only does not fit what is left of
			// this slice is the next pass's first.
			est := p.protectEstimate(r.shardBytes * int64(r.m))
			if est > p.scrubSlice() {
				res.ScrubTooLarge++
				if len(res.ScrubFindings) < maxScrubFindings {
					res.ScrubFindings = append(res.ScrubFindings, fmt.Sprintf("%s/%s: not scrubbed, its read is estimated longer than a pass (%s)", r.bucket, r.key, est.Round(time.Second)))
				}
				cur = r.cursor()
				continue
			}
			if deadline, ok := ctx.Deadline(); ok && time.Until(deadline) < est {
				break walk
			}
			n, verdict := p.scrubRow(ctx, res, r, status)
			if verdict == "" {
				break walk // the slice ran out mid-row: no verdict, the row is next
			}
			spent += n
			cur = r.cursor()
		}
	}
	res.ScrubBytes += spent
	rebuild := p.rebuildCheck(ctx, res, st.Rebuild, status)
	p.scrubMem = scrubState{At: p.now(), Cursor: cur, Rebuild: rebuild}
	res.ScrubAt, res.ScrubCursor, res.RebuildCursor = p.scrubMem.At, cur, rebuild
}

// shardVerdict is one shard's read: its verdict, its SHA-256, the bytes read.
type shardVerdict struct {
	outcome string // ok | unreadable | missing | corrupt | "" (cut by the deadline: no verdict)
	sum     string
	read    int64
	err     error
}

// readShard reads one shard end to end and judges it.
func (p *VaultParity) readShard(ctx context.Context, drv engine.Driver, container, artifact string, want int64, digest string) shardVerdict {
	rc, err := drv.Get(ctx, container, artifact)
	if err != nil {
		if ctx.Err() != nil {
			return shardVerdict{}
		}
		if isObjectMissingErr(err) {
			return shardVerdict{outcome: "missing", err: err}
		}
		return shardVerdict{outcome: "unreadable", err: err}
	}
	defer func() { _ = rc.Close() }()
	h := sha256.New()
	n, err := io.Copy(h, io.LimitReader(rc, want+1))
	switch {
	case ctx.Err() != nil:
		return shardVerdict{read: n}
	case err != nil:
		return shardVerdict{outcome: "unreadable", read: n, err: err}
	}
	sum := hex.EncodeToString(h.Sum(nil))
	if n != want {
		return shardVerdict{outcome: "corrupt", sum: sum, read: n, err: fmt.Errorf("%d bytes, the row says %d", n, want)}
	}
	if digest != "" && sum != digest {
		return shardVerdict{outcome: "corrupt", sum: sum, read: n, err: errors.New("SHA-256 is not the one protect recorded")}
	}
	return shardVerdict{outcome: "ok", sum: sum, read: n}
}

// scrubRow reads a row's shards and records the outcome. It returns the
// bytes read and the row's verdict ("" = cut by the deadline, nothing
// recorded).
func (p *VaultParity) scrubRow(ctx context.Context, res *VaultParityResult, r scrubRowData, status map[string]string) (int64, string) {
	drivers := make([]engine.Driver, r.m)
	for j := 0; j < r.m; j++ {
		leg := ""
		if j < len(r.legs) {
			leg = r.legs[j]
		}
		drv, ok := p.eng.GetDriver(leg)
		if leg == "" || !ok || status[leg] == engine.StateOpen.String() {
			res.ScrubSkipped++
			vaultParityScrubRows.WithLabelValues("skipped").Inc()
			return 0, "skipped"
		}
		drivers[j] = drv
	}
	tctx := common.WithTenantID(ctx, r.tenantID)
	container := parityContainer(r.tenantID)
	trusted := r.trusted()
	sums := make([]string, r.m)
	var read int64
	var failures []string
	for j := 0; j < r.m; j++ {
		digest := ""
		if trusted {
			digest = r.digests[j]
		}
		artifact := shardArtifact(r.prefix, j)
		v := p.readShard(tctx, drivers[j], container, artifact, r.shardBytes, digest)
		if v.outcome == "unreadable" {
			// A transient is not a fault: read once more.
			select {
			case <-ctx.Done():
				return read, ""
			case <-time.After(p.scrubRetryAfter()):
			}
			read += v.read
			v = p.readShard(tctx, drivers[j], container, artifact, r.shardBytes, digest)
		}
		read += v.read
		if v.outcome == "" {
			return read, ""
		}
		vaultParityScrubShards.WithLabelValues(v.outcome).Inc()
		sums[j] = v.sum
		if v.outcome != "ok" {
			p.logger.Warn("vault parity scrub: a parity shard failed",
				zap.String("tenant_id", r.tenantID), zap.String("bucket", r.bucket), zap.String("key", r.key),
				zap.String("leg", r.legs[j]), zap.Int("shard", j), zap.String("verdict", v.outcome), zap.Error(v.err))
			failures = append(failures, fmt.Sprintf("p%d %s on %s: %v", j, v.outcome, r.legs[j], v.err))
		}
	}
	res.ScrubRows++
	if len(failures) > 0 {
		res.ScrubShardsFailed += len(failures)
		p.reprotect(ctx, res, r, "scrub: "+strings.Join(failures, "; "))
		return read, "reprotect"
	}
	// Clean: stamp it, and record the digests read when none of this etag
	// were there (the baseline). updated_at is left alone — it is the
	// protect's and the delete's guard (finishRow, markIncomplete).
	q := `UPDATE vault_parity SET scrubbed_at = NOW(), scrub_failures = 0
	      WHERE tenant_id = $1 AND bucket = $2 AND object_key = $3 AND etag = $4 AND state = 'complete' AND updated_at = $5`
	args := []any{r.tenantID, r.bucket, r.key, r.etag, r.updatedAt}
	outcome := "ok"
	if !trusted {
		q = `UPDATE vault_parity SET scrubbed_at = NOW(), scrub_failures = 0, shard_digests = $6, digest_etag = etag,
		            object_sha256 = CASE WHEN digest_etag = etag THEN object_sha256 END
		     WHERE tenant_id = $1 AND bucket = $2 AND object_key = $3 AND etag = $4 AND state = 'complete' AND updated_at = $5`
		args = append(args, pq.Array(sums))
		outcome = "baselined"
	}
	result, err := p.db.ExecContext(context.WithoutCancel(ctx), q, args...)
	if err != nil {
		res.Errors = append(res.Errors, fmt.Sprintf("scrub %s/%s: record: %v", r.bucket, r.key, err))
		return read, "ok"
	}
	if n, _ := result.RowsAffected(); n == 0 {
		vaultParityScrubRows.WithLabelValues("changed").Inc()
		return read, "changed"
	}
	if outcome == "baselined" {
		res.ScrubBaselined++
	}
	vaultParityScrubRows.WithLabelValues(outcome).Inc()
	return read, outcome
}

func (p *VaultParity) scrubRetryAfter() time.Duration {
	if p.ScrubRetryAfter <= 0 {
		return defaultScrubRetryAfter
	}
	return p.ScrubRetryAfter
}

// reprotect makes a complete row partial for the protect pass to rewrite:
// attempts reset, scrub_failures + 1, the finding as last_error. Only the
// row as the scrub read it (etag, updated_at): a delete or an overwrite that
// moved it meanwhile owns it.
func (p *VaultParity) reprotect(ctx context.Context, res *VaultParityResult, r scrubRowData, finding string) {
	result, err := p.db.ExecContext(context.WithoutCancel(ctx), `
		UPDATE vault_parity SET state = 'partial', attempts = 0, written_at = NULL, scrub_failures = scrub_failures + 1,
		       last_error = $6, updated_at = NOW()
		WHERE tenant_id = $1 AND bucket = $2 AND object_key = $3 AND etag = $4 AND state = 'complete' AND updated_at = $5`,
		r.tenantID, r.bucket, r.key, r.etag, r.updatedAt, finding)
	if err != nil {
		res.Errors = append(res.Errors, fmt.Sprintf("scrub %s/%s: re-protect: %v", r.bucket, r.key, err))
		return
	}
	if n, _ := result.RowsAffected(); n == 0 {
		vaultParityScrubRows.WithLabelValues("changed").Inc()
		return
	}
	res.ScrubReprotect++
	vaultParityScrubRows.WithLabelValues("reprotect").Inc()
	if len(res.ScrubFindings) < maxScrubFindings {
		res.ScrubFindings = append(res.ScrubFindings, fmt.Sprintf("%s/%s: %s", r.bucket, r.key, finding))
	}
}

// rebuildCheck rebuilds the next eligible row (a stored-bytes hash of its
// etag, an estimate that fits what is left of the slice) from its four
// parity shards alone, hashing the result against object_sha256. It returns
// the cursor for the next pass.
func (p *VaultParity) rebuildCheck(ctx context.Context, res *VaultParityResult, after scrubCursor, status map[string]string) scrubCursor {
	res.Rebuild = "none"
	if ctx.Err() != nil {
		return after
	}
	var pick *scrubRowData
	deadline, hasDeadline := ctx.Deadline()
	for _, from := range []scrubCursor{after, {}} {
		var until *scrubCursor
		if from.zero() && !after.zero() {
			until = &after // the wrapped half
		}
		rows, err := p.scrubRows(ctx, from, until, true, scrubPage)
		if err != nil {
			res.Errors = append(res.Errors, err.Error())
			return after
		}
		for i := range rows {
			if hasDeadline && time.Until(deadline) < p.protectEstimate(rows[i].size) {
				continue // too large for what is left of the slice: the next one
			}
			pick = &rows[i]
			break
		}
		if pick != nil || after.zero() {
			break
		}
	}
	if pick == nil {
		return after
	}
	r := *pick
	outcome, err := p.rebuild(ctx, r, status)
	res.Rebuild = outcome
	vaultParityRebuilds.WithLabelValues(outcome).Inc()
	switch outcome {
	case "ok":
		if _, uerr := p.db.ExecContext(context.WithoutCancel(ctx), `
			UPDATE vault_parity SET rebuilt_at = NOW()
			WHERE tenant_id = $1 AND bucket = $2 AND object_key = $3 AND etag = $4 AND state = 'complete' AND updated_at = $5`,
			r.tenantID, r.bucket, r.key, r.etag, r.updatedAt); uerr != nil {
			res.Errors = append(res.Errors, fmt.Sprintf("rebuild %s/%s: record: %v", r.bucket, r.key, uerr))
		}
	case "mismatch":
		p.logger.Warn("vault parity scrub: the parity does not rebuild the object",
			zap.String("tenant_id", r.tenantID), zap.String("bucket", r.bucket), zap.String("key", r.key), zap.Error(err))
		p.reprotect(ctx, res, r, "rebuild from the parity alone: "+err.Error())
	default:
		if err != nil && len(res.ScrubFindings) < maxScrubFindings {
			res.ScrubFindings = append(res.ScrubFindings, fmt.Sprintf("%s/%s: rebuild: %v", r.bucket, r.key, err))
		}
	}
	return r.cursor()
}

// errParityAloneOnly: the rebuild check reads no data piece — the proof is
// that the parity alone is the copy.
var errParityAloneOnly = errors.New("the rebuild check reads the parity alone")

// rebuild decodes the whole stored object from the four parity shards and
// compares its SHA-256 (ok | mismatch | error).
func (p *VaultParity) rebuild(ctx context.Context, r scrubRowData, status map[string]string) (string, error) {
	l := parityLayout{k: r.k, m: r.m, stripe: r.stripe, size: r.size}
	tctx := common.WithTenantID(ctx, r.tenantID)
	container := parityContainer(r.tenantID)
	streams := make([]io.ReadCloser, l.m)
	for j := 0; j < l.m; j++ {
		drv, ok := p.eng.GetDriver(r.legs[j])
		if !ok || status[r.legs[j]] == engine.StateOpen.String() {
			closeAll(streams)
			return "error", fmt.Errorf("shard p%d: leg %s unavailable", j, r.legs[j])
		}
		rc, err := drv.Get(tctx, container, shardArtifact(r.prefix, j))
		if err != nil {
			closeAll(streams)
			return "error", fmt.Errorf("shard p%d: %w", j, err)
		}
		streams[j] = rc
	}
	none := func(context.Context, int64, int) ([]byte, error) { return nil, errParityAloneOnly }
	rb := newParityRebuilder(tctx, l, streams, none, 0, l.size)
	defer func() { _ = rb.Close() }()
	h := sha256.New()
	n, err := io.Copy(h, rb)
	if err != nil {
		if ctx.Err() != nil {
			return "error", ctx.Err()
		}
		return "error", fmt.Errorf("decode at %d of %d bytes: %w", n, l.size, err)
	}
	if sum := hex.EncodeToString(h.Sum(nil)); n != l.size || sum != r.objectSHA {
		return "mismatch", fmt.Errorf("%d bytes with SHA-256 %s, the object's stored bytes are %d with %s", n, sum, l.size, r.objectSHA)
	}
	return "ok", nil
}

func closeAll(rcs []io.ReadCloser) {
	for _, rc := range rcs {
		if rc != nil {
			_ = rc.Close()
		}
	}
}

// awaitRestore is protect's answer to an object on tape (A3): it asks the
// object's backend for the restore (once; a running one is in_progress),
// leaves the row partial WITHOUT spending the attempt the upsert counted,
// and stamps restore_checked_at — protectCandidates passes the row over for
// restoreRecheck, and the protect after the restore landed reads it.
func (p *VaultParity) awaitRestore(ctx context.Context, c parityCandidate, container, blob string) (string, error) {
	drv, ok := p.eng.GetDriver(c.backend)
	restorer, isRestorer := drv.(engine.Restorer)
	if !ok || !isRestorer {
		return "", fmt.Errorf("the object is on tape and backend %q cannot restore it", c.backend)
	}
	outcome := "requested"
	rerr := restorer.RestoreObject(common.WithTenantID(ctx, c.tenantID), container, blob, restoreDays)
	switch {
	case errors.Is(rerr, engine.ErrRestoreAlreadyInProgress):
		outcome = "in_progress"
	case rerr != nil:
		vaultParityRestores.WithLabelValues("error").Inc()
		return "", fmt.Errorf("the object is on tape and its restore was refused: %w", rerr)
	}
	vaultParityRestores.WithLabelValues(outcome).Inc()
	result, err := p.db.ExecContext(context.WithoutCancel(ctx), `
		UPDATE vault_parity SET state = 'partial', attempts = GREATEST(attempts - 1, 0),
		       last_error = $6, restore_requested_at = COALESCE(restore_requested_at, NOW()), restore_checked_at = NOW(),
		       updated_at = NOW()
		WHERE tenant_id = $1 AND bucket = $2 AND object_key = $3 AND etag = $4 AND updated_at = $5`,
		c.tenantID, c.bucket, c.key, c.etag, c.rowAt, "on tape: restore "+strings.ReplaceAll(outcome, "_", " ")+", protected once it lands")
	if err != nil {
		return "", fmt.Errorf("vault parity: record the restore: %w", err)
	}
	if n, _ := result.RowsAffected(); n == 0 {
		return "gone", nil
	}
	return outcome, nil
}

// vaultParityScrubCollector reads the scrub's standing gauges from the table
// on scrape (cached a minute; a failed read keeps the last values): only the
// active slot runs the job, and an in-process gauge would read 0 after every
// deploy.
type vaultParityScrubCollector struct {
	p                                              *VaultParity
	completeDesc, backlogDesc, lastDesc, stuckDesc *prometheus.Desc

	mu     sync.Mutex
	readAt time.Time
	vals   [4]float64
}

func newVaultParityScrubCollector(p *VaultParity) *vaultParityScrubCollector {
	return &vaultParityScrubCollector{p: p,
		completeDesc: prometheus.NewDesc("vaultaire_vault_parity_complete_rows",
			"Vault parity rows whose four shards are written (state complete); read from the database.", nil, nil),
		backlogDesc: prometheus.NewDesc("vaultaire_vault_parity_scrub_backlog",
			"Complete vault parity rows the scrub has not read clean for longer than VAULT_PARITY_SCRUB_CYCLE + 7 days (counted from the write when never scrubbed); read from the database.", nil, nil),
		lastDesc: prometheus.NewDesc("vaultaire_vault_parity_scrub_last_timestamp_seconds",
			"When the scrub last read a row clean (0 = never); read from the database.", nil, nil),
		stuckDesc: prometheus.NewDesc("vaultaire_vault_parity_restores_stuck",
			"Vault objects the parity job found on tape and asked to restore more than 24 h ago, still without a complete parity copy; read from the database.", nil, nil),
	}
}

func (c *vaultParityScrubCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.completeDesc
	ch <- c.backlogDesc
	ch <- c.lastDesc
	ch <- c.stuckDesc
}

func (c *vaultParityScrubCollector) Collect(ch chan<- prometheus.Metric) {
	v := c.read()
	ch <- prometheus.MustNewConstMetric(c.completeDesc, prometheus.GaugeValue, v[0])
	ch <- prometheus.MustNewConstMetric(c.backlogDesc, prometheus.GaugeValue, v[1])
	ch <- prometheus.MustNewConstMetric(c.lastDesc, prometheus.GaugeValue, v[2])
	ch <- prometheus.MustNewConstMetric(c.stuckDesc, prometheus.GaugeValue, v[3])
}

func (c *vaultParityScrubCollector) read() [4]float64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.readAt.IsZero() && time.Since(c.readAt) < time.Minute {
		return c.vals
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	grace := c.p.scrubCycle() + 7*24*time.Hour
	var complete, backlog, stuck int64
	var last sql.NullFloat64
	err := c.p.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FILTER (WHERE state = 'complete'),
		       COUNT(*) FILTER (WHERE state = 'complete'
		                          AND COALESCE(scrubbed_at, written_at, created_at) < NOW() - make_interval(secs => $1)),
		       EXTRACT(EPOCH FROM MAX(scrubbed_at))::FLOAT8,
		       COUNT(*) FILTER (WHERE state <> 'complete' AND restore_requested_at < NOW() - INTERVAL '24 hours')
		FROM vault_parity`, grace.Seconds()).Scan(&complete, &backlog, &last, &stuck)
	c.readAt = time.Now()
	if err != nil {
		return c.vals
	}
	c.vals = [4]float64{float64(complete), float64(backlog), last.Float64, float64(stuck)}
	return c.vals
}
