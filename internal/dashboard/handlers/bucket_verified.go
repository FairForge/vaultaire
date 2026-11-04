package handlers

import (
	"context"
	"database/sql"
	"encoding/json"
	"strconv"
	"strings"
	"time"

	"github.com/FairForge/vaultaire/internal/api/landing"
	"github.com/FairForge/vaultaire/internal/usage"
)

// The bucket page's "verified" line (WP-VAULT-2 Part 2, Stage 1a Prompt 58
// PR B). The blob scrub (internal/api/scrub.go) reads every hot blob of a
// tenant with the blob_scrub flag once per cycle and records each backend's
// walk in job_runs.result; scrub_findings holds what it found wrong and
// what it repaired. The line says so only when it is true of every byte:
//
//   - the tenant is in the scrub (the flag: its own row, else the global),
//     and was before the cycles counted began (a flag set mid-cycle reads
//     only the walk after its cursor);
//   - every backend the BUCKET's bytes are on has finished a cycle within
//     the last two cycles (one in its first cycle, one the scrub never
//     walks — tape — or one gone stale means nothing is claimed);
//   - since the earliest of those cycles began, nothing found wrong in the
//     bucket is left unrepaired.
//
// The date is the earliest finished cycle's (UTC); the sentence comes from
// internal/api/landing/durability.json.

// ScrubJobName is the blob scrub's job_runs row (internal/api's
// blobScrubJobName; a test there holds the two equal). Tests use their own.
var ScrubJobName = "blob_scrub"

type durabilityCopy struct {
	Verified             string `json:"verified"`
	VerifiedRepairedOne  string `json:"verified_repaired_one"`
	VerifiedRepairedMany string `json:"verified_repaired_many"`
}

var durability = func() durabilityCopy {
	var c durabilityCopy
	if err := json.Unmarshal(landing.DurabilityJSON(), &c); err != nil || c.Verified == "" ||
		c.VerifiedRepairedOne == "" || c.VerifiedRepairedMany == "" {
		panic("internal/api/landing/durability.json: the verified line's three sentences are required")
	}
	return c
}()

// scrubWalkState is what the line reads of the job's result: the cycle
// fields of each backend (BlobScrubBack's JSON names) and the cycle.
type scrubWalkState struct {
	Cycle    string `json:"cycle"`
	Backends map[string]struct {
		CycleStartedAt     time.Time `json:"cycle_started_at"`
		LastCycleStartedAt time.Time `json:"last_cycle_started_at"`
		CycleCompletedAt   time.Time `json:"cycle_completed_at"`
	} `json:"backends"`
}

// bucketBackendsSQL names every backend the bucket's bytes are on: its whole
// head rows, its retained versions (usage.RetainedVersionsFrom's rule) and
// the chunks its chunked objects reference. The line is true only when each
// of them has a finished cycle (RP96-B-1: the result carries a row for every
// registered driver, and one the scrub never walks — tape — looked like "no
// data there", so a Vault bucket got the line).
const bucketBackendsSQL = `
	SELECT DISTINCT backend_name FROM object_head_cache
	 WHERE tenant_id = $1 AND bucket = $2 AND size_bytes > 0 AND NOT is_chunked AND backend_name IS NOT NULL
	UNION
	SELECT DISTINCT v.backend_name FROM ` + usage.RetainedVersionsFrom + `
	   AND v.tenant_id = $1 AND v.bucket = $2 AND v.size_bytes > 0 AND v.backend_name <> 'chunked'
	UNION
	SELECT DISTINCT g.backend_id FROM global_content_index g
	  JOIN tenant_chunk_refs r ON r.dedup_scope = g.dedup_scope AND r.plaintext_hash = g.plaintext_hash
	 WHERE r.tenant_id = $1 AND r.bucket_name = $2 AND g.state = 'ready'`

// BucketVerifiedLine is the line for (tenant, bucket), "" when nothing may
// be claimed.
func BucketVerifiedLine(ctx context.Context, db *sql.DB, tenantID, bucket string) string {
	if db == nil {
		return ""
	}
	// The flag's effective row (the tenant's, else the global) and when it
	// was set: a cycle that began before the tenant was in the scrub read
	// only the part of the walk after its cursor — it claims nothing.
	var on bool
	var since sql.NullTime
	if err := db.QueryRowContext(ctx, `SELECT enabled, updated_at FROM feature_flags
		WHERE flag_key = 'blob_scrub' AND tenant_id IN ($1, '*') ORDER BY tenant_id = '*' LIMIT 1`, tenantID).Scan(&on, &since); err != nil || !on {
		return ""
	}
	var raw []byte
	if err := db.QueryRowContext(ctx, `SELECT result FROM job_runs WHERE job = $1`, ScrubJobName).Scan(&raw); err != nil || len(raw) == 0 {
		return ""
	}
	var st scrubWalkState
	if json.Unmarshal(raw, &st) != nil {
		return ""
	}
	// The backends the bucket's bytes are on: none = nothing to claim.
	rows, err := db.QueryContext(ctx, bucketBackendsSQL, tenantID, bucket)
	if err != nil {
		return ""
	}
	defer func() { _ = rows.Close() }()
	var backends []string
	for rows.Next() {
		var b string
		if rows.Scan(&b) != nil {
			return ""
		}
		backends = append(backends, b)
	}
	if rows.Err() != nil || len(backends) == 0 {
		return ""
	}
	// Every one of them finished a cycle, none of them stale (a backend
	// skipped every run — its allowance spent, its breaker open — keeps an
	// old completed cycle for ever; RP96-B-4: past two cycles it says nothing).
	cycle, _ := time.ParseDuration(st.Cycle)
	var completed, windowStart time.Time
	for i, b := range backends {
		w, ok := st.Backends[b]
		if !ok || w.CycleCompletedAt.IsZero() {
			return "" // never walked (tape), or still in its first cycle
		}
		if cycle > 0 && time.Since(w.CycleCompletedAt) > 2*cycle {
			return ""
		}
		if i == 0 || w.CycleCompletedAt.Before(completed) {
			completed = w.CycleCompletedAt
		}
		if i == 0 || w.LastCycleStartedAt.Before(windowStart) {
			windowStart = w.LastCycleStartedAt
		}
	}
	if since.Valid && windowStart.Before(since.Time) {
		return "" // the finished cycle began before the tenant was in it
	}
	var repaired, unrepaired int
	if err := db.QueryRowContext(ctx, `
		SELECT COUNT(DISTINCT f.object_key) FILTER (WHERE f.verdict = 'repaired'),
		       COUNT(DISTINCT f.address) FILTER (WHERE f.verdict IN ('mismatch', 'unreadable', 'missing') AND NOT EXISTS (
		           SELECT 1 FROM scrub_findings r WHERE r.tenant_id = f.tenant_id AND r.bucket = f.bucket AND r.address = f.address
		              AND r.verdict = 'repaired' AND r.found_at >= f.found_at))
		  FROM scrub_findings f
		 WHERE f.tenant_id = $1 AND f.bucket = $2 AND f.found_at >= $3`, tenantID, bucket, windowStart).Scan(&repaired, &unrepaired); err != nil {
		return ""
	}
	if unrepaired > 0 {
		return ""
	}
	date := completed.UTC().Format("2 January 2006")
	switch repaired {
	case 0:
		return strings.ReplaceAll(durability.Verified, "{date}", date)
	case 1:
		return strings.ReplaceAll(durability.VerifiedRepairedOne, "{date}", date)
	}
	return strings.NewReplacer("{date}", date, "{n}", strconv.Itoa(repaired)).Replace(durability.VerifiedRepairedMany)
}
