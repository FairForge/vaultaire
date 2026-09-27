package handlers

import (
	"testing"

	"github.com/FairForge/vaultaire/internal/usage"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func kinds(ps []ScenePiece) []string {
	var out []string
	for _, p := range ps {
		out = append(out, p.Kind)
	}
	return out
}

func storage(ps []ScenePiece) []ScenePiece {
	var out []ScenePiece
	for _, p := range ps {
		if !p.Decor {
			out = append(out, p)
		}
	}
	return out
}

func TestHouseScene_PiecesFollowTheQuota(t *testing.T) {
	sc := BuildHouseScene(
		FloorState{LimitBytes: 6 * usage.TB, UsedBytes: 4*usage.TB + usage.TB/5,
			Buckets: []BucketBytes{{"backups", 1 * usage.TB}, {"photos", 3*usage.TB + usage.TB/5}}},
		FloorState{LimitBytes: 1 * usage.TB, UsedBytes: usage.TB * 9 / 10, Buckets: []BucketBytes{{"archive", usage.TB * 9 / 10}}},
		"")

	// 6 TB downstairs = dresser + box; 1 TB attic = box; then you and a lamp.
	assert.Equal(t, []string{"box", "dresser", "box", "mascot", "lamp"}, kinds(sc.Pieces))
	attic := sc.Pieces[0]
	assert.Equal(t, usage.FloorVault, attic.Floor)
	assert.Equal(t, 54-15, attic.Y, "attic pieces stand on the plank")
	assert.Equal(t, "1 TB", attic.Tag)
	assert.Equal(t, 90, attic.FillPct)
	assert.Equal(t, "archive · 0.9 TB of 1 TB in the attic", attic.Title)
	assert.Equal(t, "/dashboard/buckets/archive", attic.Href)
	assert.Equal(t, "#b9c7d2", attic.TagFill)

	dresser, box := sc.Pieces[1], sc.Pieces[2]
	assert.Equal(t, 103-23, dresser.Y)
	assert.Equal(t, 16, dresser.X, "first piece starts clear of the ladder")
	assert.Equal(t, 84, dresser.FillPct, "4.2 of the dresser's 5 TB")
	assert.Equal(t, []string{"photos", "backups"}, dresser.Buckets, "largest bucket first, then the next")
	assert.Equal(t, "photos, backups · 4.2 TB of 5 TB downstairs", dresser.Title)
	assert.Equal(t, "/dashboard/buckets/photos", dresser.Href)
	assert.Equal(t, 0, box.FillPct, "the last box is still empty")
	assert.True(t, box.Empty)
	assert.Equal(t, "Empty · 1 TB downstairs", box.Title)
	assert.Equal(t, 16+31+2, box.X)
	assert.Equal(t, "#ffd400", dresser.TagFill)
	assert.Equal(t, "nf-warm", box.Night, "next to the lamp")

	assert.Equal(t, "4.2 TB of 6 TB downstairs", sc.Std.Label)
	assert.Equal(t, "0.9 TB of 1 TB in the attic", sc.Vault.Label)
	assert.Equal(t, 70, sc.Std.Pct)
	assert.Equal(t, "midnight", sc.Wall)
	assert.Equal(t, "my place", sc.Name)
	assert.False(t, sc.Empty)

	lamp := sc.Pieces[4]
	assert.True(t, lamp.LightPool)
	assert.Equal(t, "", lamp.Night, "the lamp is its own light")
	assert.Equal(t, "nf-dim", attic.Night, "no lamp in the attic")
}

func TestHouseScene_EmptyPiecesAndNoAttic(t *testing.T) {
	sc := BuildHouseScene(FloorState{LimitBytes: 3 * usage.TB}, FloorState{}, "")
	st := storage(sc.Pieces)
	require.Len(t, st, 3)
	for _, p := range st {
		assert.True(t, p.Empty)
		assert.Equal(t, 0, p.FillPct)
		assert.Equal(t, "/dashboard/buckets", p.Href)
		assert.Equal(t, "Empty · 1 TB downstairs", p.Title)
	}
	assert.Equal(t, "no attic", sc.Vault.Label)
	assert.Equal(t, "0 B of 3 TB downstairs", sc.Std.Label)
}

func TestHouseScene_LargeQuotaAggregates(t *testing.T) {
	sc := BuildHouseScene(FloorState{LimitBytes: 100 * usage.TB, UsedBytes: 50 * usage.TB}, FloorState{}, "")
	st := storage(sc.Pieces)
	require.Len(t, st, 3, "more than a row of bookcases → three bookcases sharing the TB")
	assert.Equal(t, []string{"34 TB", "33 TB", "33 TB"}, []string{st[0].Tag, st[1].Tag, st[2].Tag})
	assert.Equal(t, 100, st[0].FillPct)
	assert.Equal(t, 48, st[1].FillPct, "16 of 33 TB")
	assert.Equal(t, 0, st[2].FillPct)
	last := st[2]
	assert.LessOrEqual(t, last.X+last.W, 126, "everything fits in the room")
	assert.Equal(t, "bookcase", last.Kind)
	assert.Equal(t, 103-44, last.Y)

	// 12 TB: bookcase + 2 boxes fits, no aggregation.
	sc = BuildHouseScene(FloorState{LimitBytes: 12 * usage.TB}, FloorState{}, "")
	assert.Equal(t, []string{"bookcase", "box", "box"}, kinds(storage(sc.Pieces)))
	// 25 TB: 2 bookcases + dresser = 99 px, fits.
	sc = BuildHouseScene(FloorState{LimitBytes: 25 * usage.TB}, FloorState{}, "")
	assert.Equal(t, []string{"bookcase", "bookcase", "dresser"}, kinds(storage(sc.Pieces)))
	// 28 TB: 2 bookcases + dresser + 3 boxes does not fit → 3 bookcases 10/9/9.
	sc = BuildHouseScene(FloorState{LimitBytes: 28 * usage.TB}, FloorState{}, "")
	st = storage(sc.Pieces)
	assert.Equal(t, []string{"bookcase", "bookcase", "bookcase"}, kinds(st))
	assert.Equal(t, "10 TB", st[0].Tag)
	assert.Equal(t, "9 TB", st[2].Tag)
}

func TestHouseScene_FreeTierAndLegacy(t *testing.T) {
	// Free tier: 5 GB total, nothing whole-TB about it — one box, sized honestly.
	sc := BuildHouseScene(FloorState{LimitBytes: 5 * usage.GB, UsedBytes: 1 * usage.GB, Buckets: []BucketBytes{{"stuff", usage.GB}}}, FloorState{}, "")
	st := storage(sc.Pieces)
	require.Len(t, st, 1)
	assert.Equal(t, "5 GB", st[0].Tag)
	assert.Equal(t, 20, st[0].FillPct)
	assert.Equal(t, "stuff · 1 GB of 5 GB downstairs", st[0].Title)
	assert.Equal(t, "1 GB of 5 GB downstairs", sc.Std.Label)

	// Bytes on a floor nothing was bought on: one over-full box, said plainly.
	sc = BuildHouseScene(FloorState{}, FloorState{UsedBytes: 300 * usage.GB, Buckets: []BucketBytes{{"old", 300 * usage.GB}}}, "")
	st = storage(sc.Pieces)
	require.Len(t, st, 1)
	assert.Equal(t, usage.FloorVault, st[0].Floor)
	assert.Equal(t, 100, st[0].FillPct)
	assert.Equal(t, "0.3 TB attic, nothing bought", sc.Vault.Label)
}

func TestHouseScene_VibeFromTheSavedRoom(t *testing.T) {
	sc := BuildHouseScene(FloorState{LimitBytes: usage.TB}, FloorState{},
		"v2.oat.noir.Casa%20Viera.box-30-88_dresser-60-80")
	assert.Equal(t, "oat", sc.Wall)
	assert.Equal(t, "noir", sc.Fit)
	assert.Equal(t, "Casa Viera", sc.Name)

	sc = BuildHouseScene(FloorState{LimitBytes: usage.TB}, FloorState{}, "v2.neon.noir.x.box-1-1")
	assert.Equal(t, "midnight", sc.Wall, "unknown wall → default")
	assert.Equal(t, "my place", sc.Name)

	sc = BuildHouseScene(FloorState{LimitBytes: usage.TB}, FloorState{}, "v2.oat.noir.%3Cscript%3Ea%20very%20long%20name%20indeed.box-1-1")
	assert.Equal(t, "scripta very long", sc.Name, "angle brackets dropped, 18 chars max")
}

func TestHouseScene_LightRules(t *testing.T) {
	// Day: the window pushes shadows away from itself; night: the lamp does.
	day := shadowShape(50, 103, 31, []light{{x: 99, reach: 80, weight: 0.5}}, false)
	assert.Less(t, day.CX, 50.0, "shadow leans away from the window on the right")
	assert.Greater(t, day.O, 0.2, "and darkens a little with the light")
	night := shadowShape(50, 103, 31, nil, true)
	assert.Equal(t, 50.0, night.CX)
	assert.Equal(t, 0.34, night.O)

	assert.Equal(t, "nf-warm", tintFor(0.5))
	assert.Equal(t, "nf-mid", tintFor(0.2))
	assert.Equal(t, "nf-dim", tintFor(0))

	assert.Equal(t, 1.0, lampLevel(10, 10, []lampLight{{x: 10, y: 10, rx: 5, ry: 5}}))
}

func TestHouseScene_MetricsAndDecorPlacement(t *testing.T) {
	sc := BuildHouseScene(FloorState{}, FloorState{}, "")
	assert.True(t, sc.Empty)
	assert.Equal(t, "no downstairs", sc.Std.Label)
	assert.Equal(t, []string{"mascot", "lamp"}, kinds(sc.Pieces), "an empty house still has you in it")

	// A full row leaves no room for decor.
	sc = BuildHouseScene(FloorState{LimitBytes: 25 * usage.TB}, FloorState{}, "")
	assert.Equal(t, []string{"bookcase", "bookcase", "dresser"}, kinds(sc.Pieces))
	assert.Contains(t, sc.AriaLabel, "downstairs")
}
