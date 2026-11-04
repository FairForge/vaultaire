package handlers

import (
	"context"
	"database/sql"
	"encoding/json"
	"strconv"
	"strings"
	"time"

	"github.com/FairForge/vaultaire/internal/api/landing"
)

// The bucket page's "verified" line (WP-VAULT-2 Part 2, Stage 1a Prompt 58
// PR B). The blob scrub (internal/api/scrub.go) reads every hot blob of a
// tenant with the blob_scrub flag once per cycle and records each backend's
// walk in job_runs.result; scrub_findings holds what it found wrong and
// what it repaired. The line says so only when it is true of every byte:
//
//   - the tenant is in the scrub (the flag: its own row, else the global);
//   - every backend the scrub holds data for has finished a cycle (one in
//     its first cycle means nothing is claimed yet);
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
// fields of each backend (BlobScrubBack's JSON names).
type scrubWalkState struct {
	Backends map[string]struct {
		CycleStartedAt     time.Time `json:"cycle_started_at"`
		LastCycleStartedAt time.Time `json:"last_cycle_started_at"`
		CycleCompletedAt   time.Time `json:"cycle_completed_at"`
	} `json:"backends"`
}

// BucketVerifiedLine is the line for (tenant, bucket), "" when nothing may
// be claimed.
func BucketVerifiedLine(ctx context.Context, db *sql.DB, tenantID, bucket string) string {
	if db == nil {
		return ""
	}
	var on bool
	if err := db.QueryRowContext(ctx, `SELECT COALESCE(
		(SELECT enabled FROM feature_flags WHERE flag_key = 'blob_scrub' AND tenant_id = $1),
		(SELECT enabled FROM feature_flags WHERE flag_key = 'blob_scrub' AND tenant_id = '*'), FALSE)`, tenantID).Scan(&on); err != nil || !on {
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
	var completed, windowStart time.Time
	seen := false
	for _, b := range st.Backends {
		if b.CycleCompletedAt.IsZero() {
			if !b.CycleStartedAt.IsZero() {
				return "" // a backend still in its first cycle: nothing is claimed
			}
			continue // no data there
		}
		if !seen || b.CycleCompletedAt.Before(completed) {
			completed = b.CycleCompletedAt
		}
		if !seen || b.LastCycleStartedAt.Before(windowStart) {
			windowStart = b.LastCycleStartedAt
		}
		seen = true
	}
	if !seen {
		return ""
	}
	var repaired, unrepaired int
	if err := db.QueryRowContext(ctx, `
		SELECT COUNT(*) FILTER (WHERE f.verdict = 'repaired'),
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
