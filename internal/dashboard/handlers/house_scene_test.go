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

func onFloor(ps []ScenePiece, floor string) []ScenePiece {
	var out []ScenePiece
	for _, p := range storage(ps) {
		if p.Floor == floor {
			out = append(out, p)
		}
	}
	return out
}

func TestHouseScene_OneBoxPerTerabyte(t *testing.T) {
	sc := BuildHouseScene(
		FloorState{LimitBytes: 6 * usage.TB, UsedBytes: 4*usage.TB + usage.TB/5,
			Buckets: []BucketBytes{{"backups", 1 * usage.TB}, {"photos", 3*usage.TB + usage.TB/5}}},
		FloorState{LimitBytes: 1 * usage.TB, UsedBytes: usage.TB * 9 / 10, Buckets: []BucketBytes{{"archive", usage.TB * 9 / 10}}},
		"")

	// 6 TB Standard = six boxes; you, a plant and the lamp; 1 TB Vault = one crate.
	assert.Equal(t, []string{"box", "box", "box", "box", "box", "box", "mascot", "plant", "floorlamp", "crate"}, kinds(sc.Pieces))
	std := onFloor(sc.Pieces, usage.FloorStandard)
	require.Len(t, std, 6)
	// three on the floor, three standing on them
	assert.Equal(t, [][2]int{{3, 45}, {20, 45}, {37, 45}, {3, 32}, {20, 32}, {37, 32}},
		[][2]int{{std[0].X, std[0].Y}, {std[1].X, std[1].Y}, {std[2].X, std[2].Y}, {std[3].X, std[3].Y}, {std[4].X, std[4].Y}, {std[5].X, std[5].Y}})

	// photos (3.2 TB) fills three boxes and part of the fourth, backups the rest
	assert.Equal(t, 100, std[0].FillPct)
	assert.Equal(t, []string{"photos"}, std[0].Buckets)
	assert.Equal(t, "photos · 1.0 TB of 1 TB Standard", std[0].Title)
	assert.Equal(t, "/dashboard/buckets/photos", std[0].Href)
	assert.Equal(t, []string{"photos", "backups"}, std[3].Buckets, "the fourth box holds the end of photos and the start of backups")
	assert.Equal(t, 100, std[3].FillPct)
	assert.Equal(t, 20, std[4].FillPct, "0.2 TB of backups in the fifth")
	assert.True(t, std[5].Empty)
	assert.Equal(t, "Empty · 1 TB Standard", std[5].Title)
	assert.Equal(t, "/dashboard/buckets", std[5].Href)

	// one label per pile, on the first box, above the pile
	assert.Equal(t, "STANDARD 6 TB", std[0].Tag)
	assert.Equal(t, "#ffd400", std[0].TagFill)
	assert.Equal(t, 32-7, std[0].TagY)
	for _, p := range std[1:] {
		assert.Empty(t, p.Tag)
	}

	crate := onFloor(sc.Pieces, usage.FloorVault)[0]
	assert.Equal(t, "crate", crate.Symbol)
	assert.Equal(t, 106, crate.X)
	assert.Equal(t, 60-15, crate.Y)
	assert.Equal(t, 90, crate.FillPct)
	assert.Equal(t, "archive · 0.9 TB of 1 TB Vault", crate.Title)
	assert.Equal(t, "/dashboard/buckets/archive", crate.Href)
	assert.Equal(t, "VAULT 1 TB", crate.Tag)
	assert.Equal(t, "#b9c7d2", crate.TagFill)

	assert.Equal(t, "4.2 TB of 6 TB Standard", sc.Std.Label)
	assert.Equal(t, "0.9 TB of 1 TB Vault", sc.Vault.Label)
	assert.Equal(t, 70, sc.Std.Pct)
	assert.Equal(t, "midnight", sc.Wall)
	assert.False(t, sc.Empty)
	assert.Equal(t, "Your storage: 4.2 TB of 6 TB Standard, 0.9 TB of 1 TB Vault.", sc.AriaLabel)
}

func TestHouseScene_EmptyBoxesAndNoVault(t *testing.T) {
	sc := BuildHouseScene(FloorState{LimitBytes: 3 * usage.TB}, FloorState{}, "")
	st := storage(sc.Pieces)
	require.Len(t, st, 3)
	for _, p := range st {
		assert.True(t, p.Empty)
		assert.Equal(t, 0, p.FillPct)
		assert.Equal(t, "/dashboard/buckets", p.Href)
		assert.Equal(t, "Empty · 1 TB Standard", p.Title)
	}
	assert.Equal(t, "no Vault yet", sc.Vault.Label)
	assert.Equal(t, "0 B of 3 TB Standard", sc.Std.Label)
	assert.Equal(t, 45-7, st[0].TagY, "one row: the label sits above it")
}

func TestHouseScene_BigPileShowsTwelve(t *testing.T) {
	sc := BuildHouseScene(FloorState{LimitBytes: 100 * usage.TB, UsedBytes: 50 * usage.TB}, FloorState{}, "")
	st := storage(sc.Pieces)
	require.Len(t, st, 12, "3 x 4 drawn, never more")
	assert.Equal(t, "STANDARD 100 TB (12 SHOWN)", st[0].Tag)
	var caps int64
	for _, p := range st {
		caps += p.CapBytes
	}
	assert.Equal(t, 100*usage.TB, caps, "the drawn boxes share every TB bought")
	assert.Equal(t, 100, st[0].FillPct)
	assert.True(t, st[11].Empty)
	assert.Equal(t, 6, st[11].Y, "the fourth row")
	assert.GreaterOrEqual(t, st[0].TagY, 0, "the label stays in the room")
	for _, p := range st {
		assert.LessOrEqual(t, p.X+p.W, 57, "the Standard pile stays left of you")
	}
}

func TestHouseScene_FreeTierAndLegacy(t *testing.T) {
	// Free tier: 5 GB total, nothing whole-TB about it, one box, sized honestly.
	sc := BuildHouseScene(FloorState{LimitBytes: 5 * usage.GB, UsedBytes: 1 * usage.GB, Buckets: []BucketBytes{{"stuff", usage.GB}}}, FloorState{}, "")
	st := storage(sc.Pieces)
	require.Len(t, st, 1)
	assert.Equal(t, "STANDARD 5 GB", st[0].Tag)
	assert.Equal(t, 20, st[0].FillPct)
	assert.Equal(t, "stuff · 1 GB of 5 GB Standard", st[0].Title)
	assert.Equal(t, "1 GB of 5 GB Standard", sc.Std.Label)

	// Bytes on Vault with nothing bought there: one over-full crate, said plainly.
	sc = BuildHouseScene(FloorState{}, FloorState{UsedBytes: 300 * usage.GB, Buckets: []BucketBytes{{"old", 300 * usage.GB}}}, "")
	st = storage(sc.Pieces)
	require.Len(t, st, 1)
	assert.Equal(t, usage.FloorVault, st[0].Floor)
	assert.Equal(t, "crate", st[0].Kind)
	assert.Equal(t, 100, st[0].FillPct)
	assert.Equal(t, "0.3 TB on Vault, nothing bought", sc.Vault.Label)
}

func TestHouseScene_VibeFromTheSavedRoom(t *testing.T) {
	sc := BuildHouseScene(FloorState{LimitBytes: usage.TB}, FloorState{},
		"v2.oat.noir.Casa%20Viera.box-30-88_dresser-60-80")
	assert.Equal(t, "oat", sc.Wall)
	assert.Equal(t, "noir", sc.Fit)

	sc = BuildHouseScene(FloorState{LimitBytes: usage.TB}, FloorState{}, "v2.neon.noir.x.box-1-1")
	assert.Equal(t, "midnight", sc.Wall, "unknown wall → default")
	assert.Equal(t, "midnight", sc.Fit)
}

func TestHouseScene_LightRules(t *testing.T) {
	// Day: light from the left pushes shadows right; night: the lamp does.
	day := shadowShape(50, 60, 17, []light{{x: -40, reach: 240, weight: 0.35}}, false)
	assert.Greater(t, day.CX, 50.0, "shadow leans away from the light on the left")
	assert.Greater(t, day.O, 0.2, "and darkens a little with the light")
	night := shadowShape(50, 60, 17, nil, true)
	assert.Equal(t, 50.0, night.CX)
	assert.Equal(t, 0.34, night.O)

	assert.Equal(t, "nf-warm", tintFor(0.5))
	assert.Equal(t, "nf-mid", tintFor(0.2))
	assert.Equal(t, "nf-dim", tintFor(0))

	assert.Equal(t, 1.0, lampLevel(10, 10, []lampLight{{x: 10, y: 10, rx: 5, ry: 5}}))

	sc := BuildHouseScene(FloorState{LimitBytes: usage.TB}, FloorState{LimitBytes: usage.TB}, "")
	for _, p := range sc.Pieces {
		if p.LightPool {
			assert.Equal(t, "", p.Night, "the lamp is its own light")
			assert.Len(t, p.Pools, 3)
		}
	}
	assert.NotEqual(t, "nf-dim", onFloor(sc.Pieces, usage.FloorVault)[0].Night, "the crate is lit by the lamp beside it")
	assert.Equal(t, "nf-dim", onFloor(sc.Pieces, usage.FloorStandard)[0].Night, "the far box is not")
}

func TestHouseScene_EmptyRoomStillHasYou(t *testing.T) {
	sc := BuildHouseScene(FloorState{}, FloorState{}, "")
	assert.True(t, sc.Empty)
	assert.Equal(t, "no Standard yet", sc.Std.Label)
	assert.Equal(t, []string{"mascot", "plant", "floorlamp"}, kinds(sc.Pieces), "an empty room still has you in it")
	assert.Contains(t, sc.AriaLabel, "Standard")
}
