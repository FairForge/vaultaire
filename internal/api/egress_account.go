package api

import (
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
