package api

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// 2026-11-02 (bench-results/IDRIVE-WORKER-2026-11-02.md § 7): iDrive meters
// ONE account — one key set, one bill, one free-egress allowance — across
// all fourteen regions, and the allowance the code may count on is the
// strict one: 3 × min(the plan bought, what the account actually stores).
// The gauge and the copy pass's budget counted the primary `idrive` backend
// alone — a region's bytes were in no pool and a region's reads were spent
// against none — and did not stop at the plan.

func TestPlanAccountOf_EveryIDriveRegionIsTheIDriveAccount(t *testing.T) {
	assert.Equal(t, "idrive", planAccountOf("idrive"))
	assert.Equal(t, "idrive", planAccountOf("idrive-eu-west-1"))
	assert.Equal(t, "idrive", planAccountOf("idrive-ap-southeast-2"))
	assert.Equal(t, "lyve", planAccountOf("lyve"))
	assert.Equal(t, "idrive-nowhere-9", planAccountOf("idrive-nowhere-9"), "only a region of the table is the account's")
	assert.Equal(t, "", planAccountOf(""))
}

func TestAccountStored_SumsEveryBackendOfTheAccount(t *testing.T) {
	stored := map[string]*BackendBytes{
		"idrive":           {Total: 100},
		"idrive-eu-west-1": {Total: 20},
		"idrive-us-west-2": {Total: 5},
		"lyve":             {Total: 1000},
	}
	assert.Equal(t, int64(125), accountStored(stored, "idrive"))
	assert.Equal(t, int64(1000), accountStored(stored, "lyve"))
	assert.Zero(t, accountStored(stored, "wasabi"))
	assert.Zero(t, accountStored(nil, "idrive"))
}

func TestEgressBase_IsTheSmallerOfThePlanAndTheStored(t *testing.T) {
	assert.Equal(t, int64(109), egressBase(1000, 109), "stored under the plan: what is stored")
	assert.Equal(t, int64(1000), egressBase(1000, 1500), "stored over the plan: never more than the plan")
	assert.Equal(t, int64(1500), egressBase(0, 1500), "no capacity on the row: what is stored")
	assert.Zero(t, egressBase(1000, 0))
}

func TestEgressFreeBytes_TheAccountsStrictAllowance(t *testing.T) {
	mult := 3.0
	plan := BackendPlan{Backend: "idrive", CapacityBytes: 1000, EgressFreeMultiple: &mult}
	stored := map[string]*BackendBytes{"idrive": {Total: 100}, "idrive-eu-west-1": {Total: 50}}
	assert.Equal(t, int64(450), egressFreeBytes(plan, stored), "3 × (100 + 50): the regions are in the pool")

	stored["idrive-us-west-2"] = &BackendBytes{Total: 2000}
	assert.Equal(t, int64(3000), egressFreeBytes(plan, stored), "3 × the plan once the account stores more than it")
}

func TestEgressLedger_ARegionsCopySpendsTheAccountsBudget(t *testing.T) {
	l := &egressLedger{limit: map[string]int64{"idrive": 100}, spent: map[string]int64{}, took: map[string]int64{}, stopped: map[string]bool{}}

	assert.True(t, l.take("idrive-eu-west-1", 60), "a region's copy draws on the account")
	assert.False(t, l.take("idrive", 60), "and the primary's then finds 40 left")
	assert.True(t, l.take("idrive-us-west-2", 40))
	assert.True(t, l.stopped["idrive"], "the refusal is named by the account")

	l.give("idrive-eu-west-1", 60)
	assert.True(t, l.take("idrive", 60), "a reservation given back returns to the account")
	assert.True(t, l.take("lyve", 1<<40), "a source with no plan row stays unmetered")
}
