package drivers

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/FairForge/vaultaire/internal/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// The review of the WP-FLEET next chain (2026-11-07, RB3-1): WP-FLEET-11
// made a settled fingerprint authoritative — Get / Exists / Delete ask the
// home account only. The fingerprint proves the fleet's ORDER is the one
// recorded; it says nothing about where the data sits. PERMAFROST_FLEET_ACCEPT
// rewrites every marker with the new order, so the very next check (the
// hourly re-check, or the next deploy) is `match` — and every object whose
// home moved (hash % fleet size) is a miss, un-deletable, and its tenant's
// erasure deferred for ever by RemoveContainerTree's re-listing. The README's
// own procedure says "no rebalance tool exists yet; the fleet walk keeps reads
// working" — it did, until #1180.
//
// Now the marker records `placed_under`: the digest of the order the account's
// DATA is laid out for. A first boot writes the current digest; an accept
// carries each old marker's value forward (a brand-new account gets the new
// digest — it holds nothing); a marker written before the field reads as
// placed under its own digest (prod's markers: authoritative as before). Reads
// are authoritative only while EVERY marker is placed under the current order
// — a rebalance (the tool that does not exist yet) rewrites the field.

// rewriteMarker edits one account's marker in place (what a rebalance tool
// does).
func rewriteMarker(t *testing.T, a *markerStore, edit func(m *fleetMarker)) {
	t.Helper()
	m := a.marker(t)
	edit(&m)
	b, err := json.Marshal(m)
	require.NoError(t, err)
	a.mu.Lock()
	a.files[fleetMarkerPath] = string(b)
	a.mu.Unlock()
}

func TestFleetFingerprint_AnAcceptedOrderKeepsTheWalkUntilTheDataIsRebalanced(t *testing.T) {
	// Arrange: a 3-account fleet recorded (its objects placed under that
	// order), then one account lost for good.
	acc := newAccounts("a", "b", "c")
	full := markerFleet(zap.NewNop(), acc...)
	recorded, err := full.CheckFleetFingerprint(context.Background(), "")
	require.NoError(t, err)
	require.True(t, full.authoritative(), "a first boot: the data is placed under the recorded order")
	shrunk := markerFleet(zap.NewNop(), acc[0], acc[2])
	mismatch, err := shrunk.CheckFleetFingerprint(context.Background(), "")
	require.NoError(t, err)
	require.Equal(t, FleetMismatch, mismatch.State)
	require.False(t, shrunk.authoritative())

	// Act: the operator accepts the new order (the README's step 3).
	accepted, err := shrunk.CheckFleetFingerprint(context.Background(), mismatch.Digest)
	require.NoError(t, err)
	require.Equal(t, FleetAccepted, accepted.State)

	// Assert: the walk stays — the data is still laid out for the old order.
	assert.False(t, shrunk.authoritative(), "accepted: every object whose home moved is still on its old account")
	assert.False(t, accepted.Placed)
	for _, a := range []*markerStore{acc[0], acc[2]} {
		m := a.marker(t)
		assert.Equal(t, mismatch.Digest, m.Digest, "the marker records the new order")
		assert.Equal(t, recorded.Digest, m.PlacedUnder, "and that the data was placed under the old one")
	}

	// And the hourly re-check (or the next deploy) does not lift it: the
	// verdict is match, the data is still off its homes.
	again, err := markerFleet(zap.NewNop(), acc[0], acc[2]).CheckFleetFingerprint(context.Background(), mismatch.Digest)
	require.NoError(t, err)
	assert.Equal(t, FleetMatch, again.State)
	assert.False(t, again.Placed)
	rebooted := markerFleet(zap.NewNop(), acc[0], acc[2])
	_, err = rebooted.CheckFleetFingerprint(context.Background(), "")
	require.NoError(t, err)
	assert.False(t, rebooted.authoritative(), "a reboot after the accept still walks")

	// A rebalance rewrites placed_under on every account: authoritative again.
	for _, a := range []*markerStore{acc[0], acc[2]} {
		rewriteMarker(t, a, func(m *fleetMarker) { m.PlacedUnder = mismatch.Digest })
	}
	rebalanced := markerFleet(zap.NewNop(), acc[0], acc[2])
	fp, err := rebalanced.CheckFleetFingerprint(context.Background(), "")
	require.NoError(t, err)
	assert.Equal(t, FleetMatch, fp.State)
	assert.True(t, fp.Placed)
	assert.True(t, rebalanced.authoritative())
}

func TestFleetFingerprint_AnAppendedAccountCarriesTheOldPlacementOnTheOldOnes(t *testing.T) {
	// Arrange: two accounts recorded, a third appended (WP-FLEET-11's
	// append-only index) — the modulus changed, most homes moved.
	acc := newAccounts("a", "b", "c")
	two := markerFleet(zap.NewNop(), acc[0], acc[1])
	recorded, err := two.CheckFleetFingerprint(context.Background(), "")
	require.NoError(t, err)
	three := markerFleet(zap.NewNop(), acc...)
	mismatch, err := three.CheckFleetFingerprint(context.Background(), "")
	require.NoError(t, err)
	require.Equal(t, FleetMismatch, mismatch.State)

	// Act
	accepted, err := three.CheckFleetFingerprint(context.Background(), mismatch.Digest)
	require.NoError(t, err)
	require.Equal(t, FleetAccepted, accepted.State)

	// Assert: the old accounts say "placed under the 2-account order", the new
	// one (holding nothing) says the current; the fleet walks.
	assert.Equal(t, recorded.Digest, acc[0].marker(t).PlacedUnder)
	assert.Equal(t, recorded.Digest, acc[1].marker(t).PlacedUnder)
	assert.Equal(t, mismatch.Digest, acc[2].marker(t).PlacedUnder, "a brand-new account holds nothing placed under the old order")
	assert.False(t, three.authoritative())
}

func TestFleetFingerprint_AMarkerWrittenBeforePlacedUnderReadsAsPlacedUnderItsDigest(t *testing.T) {
	// Arrange: prod's markers (WP-R7-4, version 1, no placed_under field)
	// on a matching fleet.
	acc := newAccounts("a", "b")
	d := markerFleet(zap.NewNop(), acc...)
	_, err := d.CheckFleetFingerprint(context.Background(), "")
	require.NoError(t, err)
	for _, a := range acc {
		rewriteMarker(t, a, func(m *fleetMarker) { m.PlacedUnder = "" })
	}

	// Act
	fp, err := markerFleet(zap.NewNop(), acc...).CheckFleetFingerprint(context.Background(), "")

	// Assert: match, placed, authoritative — nothing rewritten (a match never
	// writes), the field stays absent.
	require.NoError(t, err)
	assert.Equal(t, FleetMatch, fp.State)
	assert.True(t, fp.Placed)
	assert.Equal(t, "", acc[0].marker(t).PlacedUnder)
}

func TestFleetFingerprint_ARepairedMarkerTakesTheFleetsPlacement(t *testing.T) {
	// Arrange: a fleet accepted after a shrink (placed under the old order),
	// then one of the surviving accounts loses its marker.
	acc := newAccounts("a", "b", "c")
	_, err := markerFleet(zap.NewNop(), acc...).CheckFleetFingerprint(context.Background(), "")
	require.NoError(t, err)
	shrunk := markerFleet(zap.NewNop(), acc[0], acc[2])
	mismatch, err := shrunk.CheckFleetFingerprint(context.Background(), "")
	require.NoError(t, err)
	_, err = shrunk.CheckFleetFingerprint(context.Background(), mismatch.Digest)
	require.NoError(t, err)
	old := acc[0].marker(t).PlacedUnder
	acc[2].mu.Lock()
	delete(acc[2].files, fleetMarkerPath)
	acc[2].mu.Unlock()

	// Act
	fp, err := markerFleet(zap.NewNop(), acc[0], acc[2]).CheckFleetFingerprint(context.Background(), "")

	// Assert: repaired with the fleet's (old) placement — never a fresh
	// "placed under the current order" that would lift the walk.
	require.NoError(t, err)
	assert.Equal(t, FleetRepaired, fp.State)
	assert.Equal(t, old, acc[2].marker(t).PlacedUnder)
	assert.False(t, fp.Placed)
}

func TestFleetAtScale_AnAcceptedFleetStillWalksAMiss(t *testing.T) {
	// The behaviour behind the flag: a miss on a fleet whose data is placed
	// under another order looks at the previous placement too, whatever the
	// verdict says — since WP-FLEET-5 that second look is computed (the home
	// under the other placement), never a walk of eight.
	ctx := common.WithTenantID(context.Background(), "x")
	d, rt := bigFleet(300)
	d.fleetState = FleetAccepted
	d.fleetUnplaced = true

	ok, err := d.Exists(ctx, "bkt", "nothing-here")

	require.NoError(t, err)
	assert.False(t, ok)
	assert.LessOrEqual(t, rt.n.Load(), int64(2), "the home and the previous home, no walk")
	assert.GreaterOrEqual(t, rt.n.Load(), int64(1))
}
