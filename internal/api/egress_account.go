package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/FairForge/vaultaire/internal/drivers"
)

// The free-egress allowance of a vendor ACCOUNT (2026-11-02,
// bench-results/IDRIVE-WORKER-2026-11-02.md § 7). iDrive meters one account
// — one key set, one bill, one allowance — across all its regions: the
// primary `idrive` and every `idrive-<region>` driver are that account, so
// their stored bytes form one pool and their egress is spent against it.
// The pool is strict: the plan row's multiple × min(the capacity bought,
// what the account stores). The vendor's wording names either base ("3×
// active storage", "3× quota"), and what Stored sells (prices.json's
// standard_free_ratio × the quota a customer BOUGHT) sits on the larger
// one — so the code counts on the smaller.

// planAccountOf is the backend_plans row a backend is billed under: an
// iDrive region of the table is the `idrive` account; any other backend is
// its own.
func planAccountOf(backend string) string {
	if region, ok := strings.CutPrefix(backend, "idrive-"); ok && drivers.IsValidRegion(region) {
		return "idrive"
	}
	return backend
}

// accountStored is what an account stores: the sum over every backend of
// the account in a backend_capacity run.
func accountStored(stored map[string]*BackendBytes, account string) int64 {
	var total int64
	for backend, b := range stored {
		if b != nil && planAccountOf(backend) == account {
			total += b.Total
		}
	}
	return total
}

// egressBase is what a plan's free-egress multiple may be counted on: the
// smaller of the capacity bought and the bytes stored (no capacity on the
// row = the bytes stored).
func egressBase(capacity, stored int64) int64 {
	if capacity > 0 && stored > capacity {
		return capacity
	}
	return stored
}

// egressFreeBytes is a plan row's free egress this month: its multiple ×
// egressBase over the account's stored total (0 for an unmetered row).
func egressFreeBytes(p BackendPlan, stored map[string]*BackendBytes) int64 {
	if p.EgressFreeMultiple == nil {
		return 0
	}
	return int64(*p.EgressFreeMultiple * float64(egressBase(p.CapacityBytes, accountStored(stored, p.Backend))))
}

// egressJobs names the job_runs rows the jobs' vendor ledger reads: the
// capacity run (stored totals, the vendor's view) and the two jobs that
// spend a vendor's free egress — the blob scrub and the blob copies.
type egressJobs struct {
	capacity, scrub, copies string
}

// jobsEgressLeft is the ONE vendor ledger of the scrub and the blob copies
// (RP96-7): per plan ACCOUNT, what the two jobs may still read this UTC
// month. Absent = unmetered (no plan row, or a NULL multiple). A metered
// account with no capacity run yet gets 0 (unknown = no egress).
//
//   - Without the vendor's month-to-date (prod: no reseller key), "used" is
//     only what the two jobs read — the scrub's month_bytes plus the copies'
//     spent — and the customers' own reads are invisible, so ONE multiple
//     stays theirs: (multiple − 1) × base − used.
//   - With the vendor's figure (it counts the customers' reads and ours):
//     the full multiple × base − max(the vendor's, ours).
//
// A plan row named for an iDrive region is its account's (the PUT refuses
// one now, RP96-8); two rows of one account keep the smaller remainder.
func jobsEgressLeft(ctx context.Context, db *sql.DB, jobs egressJobs, month string) (map[string]int64, []string, error) {
	var notes []string
	read := func(job string, into any) bool {
		var raw []byte
		if job == "" {
			return false
		}
		if err := db.QueryRowContext(ctx, `SELECT result FROM job_runs WHERE job = $1`, job).Scan(&raw); err != nil || len(raw) == 0 {
			return false
		}
		return json.Unmarshal(raw, into) == nil
	}
	var capRes BackendCapacityResult
	haveCap := read(jobs.capacity, &capRes)
	ours := map[string]int64{}
	var scrub BlobScrubResult
	if read(jobs.scrub, &scrub) && scrub.Month == month {
		for b, st := range scrub.Backends {
			if st != nil {
				ours[planAccountOf(b)] += st.MonthBytes
			}
		}
	}
	var copies BlobCopiesResult
	if read(jobs.copies, &copies) && copies.Egress != nil && copies.Egress.Month == month {
		for b, n := range copies.Egress.Spent {
			ours[planAccountOf(b)] += n
		}
	}
	rows, err := db.QueryContext(ctx, `SELECT backend, capacity_bytes, egress_free_multiple FROM backend_plans WHERE egress_free_multiple IS NOT NULL`)
	if err != nil {
		return nil, nil, fmt.Errorf("the vendor plans: %w", err)
	}
	defer func() { _ = rows.Close() }()
	left := map[string]int64{}
	for rows.Next() {
		var name string
		var capacity int64
		var mult float64
		if rows.Scan(&name, &capacity, &mult) != nil {
			continue
		}
		account := planAccountOf(name)
		if account != name {
			notes = append(notes, fmt.Sprintf("the plan row %q is its account %q's (one account, one row)", name, account))
		}
		n := int64(0)
		if haveCap {
			base := egressBase(capacity, accountStored(capRes.Backends, account))
			used := ours[account]
			if v, ok := capRes.Vendor[account]; ok && !v.EgressUnmetered {
				n = int64(mult*float64(base)) - max(v.EgressMonthBytes, used)
			} else {
				n = int64((mult-1)*float64(base)) - used
			}
			n = max(0, n)
		}
		if prev, ok := left[account]; !ok || n < prev {
			left[account] = n
		}
	}
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("the vendor plans: %w", err)
	}
	return left, notes, nil
}
