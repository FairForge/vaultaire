package handlers

import (
	"sync/atomic"

	"github.com/FairForge/vaultaire/internal/modes"
)

// The bucket mode as the dashboard shows it (keystone K3 PR 3, plan line
// 1281): the name and sentences come from modes.json through
// internal/modes — never typed in a template. Behind the modes_copy flag
// (default OFF, per tenant); off, the bucket list and settings keep the old
// visibility labels (docs/reviews/K3-PROBE.md R1/R2 — whose "Private" is
// the Standard mode: the name trap).

var modesCopyFn atomic.Pointer[func(tenantID string) bool]

// SetModesCopy wires the modes_copy flag (the router: flags.Enabled for the
// tenant). nil = off for everyone.
func SetModesCopy(fn func(tenantID string) bool) {
	if fn == nil {
		modesCopyFn.Store(nil)
		return
	}
	modesCopyFn.Store(&fn)
}

// modesCopy reports whether the tenant sees the mode copy.
func modesCopy(tenantID string) bool {
	fn := modesCopyFn.Load()
	return fn != nil && (*fn)(tenantID)
}

// ModeLabel is one mode as a template renders it.
type ModeLabel struct {
	ID          string
	Name        string
	Sentence    string // the customer's sentence, true now (master key or not)
	ServerHolds string // what we can read
	How         string // how a bucket gets it
	Class       string // badge class
}

// modeLabel is the label of a mode id (the bucket's buckets.mode).
func modeLabel(id string) ModeLabel {
	m, ok := modes.Get(id)
	if !ok {
		return ModeLabel{ID: id, Name: id, Class: "badge-default"}
	}
	key := modes.MasterKeyLoaded()
	class := "badge-default"
	switch id {
	case modes.Public:
		class = "badge-success"
	case modes.Private:
		class = "badge-info"
	}
	return ModeLabel{ID: m.ID, Name: m.Name, Sentence: m.Customer.Render(key), ServerHolds: m.ServerHolds.Render(key), How: m.How.Render(key), Class: class}
}

// ModeLabelOf is modeLabel for the templates' tests.
func ModeLabelOf(id string) ModeLabel { return modeLabel(id) }
