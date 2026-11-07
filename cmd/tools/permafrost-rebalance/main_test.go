package main

import (
	"strings"
	"testing"

	"github.com/FairForge/vaultaire/internal/drivers"
	"github.com/stretchr/testify/assert"
)

// The rebalance itself is tested in internal/drivers (onedrive_rebalance_test.go)
// on a fake fleet; the tool is the plan's print and the run loop.

func TestPrintPlan_NamesTheMarkersStateEveryAccountAndTheStrays(t *testing.T) {
	p := &drivers.RebalancePlan{
		Digest: "abcd", State: drivers.FleetMatch, Laid: 1, Target: 0, Version: 2, Placed: true,
		Accounts: []drivers.RebalanceAccount{
			{Index: 0, Name: "tenant-1", Files: 10, Bytes: 3 << 30, MovesOut: 4},
			{Index: 1, Name: "tenant-2", Files: 5, Bytes: 1 << 30, MovesIn: 4},
		},
		Files: 15, Bytes: 4 << 30,
		Moves:  []drivers.RebalanceMove{{Path: "t-x/p/a/etag/p0", Size: 1 << 30, From: 0, To: 1}},
		Strays: []string{"tenant-1: t-x/p/odd/etag/p3"},
	}
	var sb strings.Builder
	printPlan(&sb, p)
	out := sb.String()
	assert.Contains(t, out, "fleet abcd  state=match  laid-out-under=v1  target-set=v0  placed=true  → version 2")
	assert.Contains(t, out, "tenant-1")
	assert.Contains(t, out, "3.00 GiB")
	assert.Contains(t, out, "total 15 files, 4.00 GiB; 1 to move (1.00 GiB)")
	assert.Contains(t, out, "1 files where neither placement puts them")
	assert.Contains(t, out, "tenant-1: t-x/p/odd/etag/p3")
}
