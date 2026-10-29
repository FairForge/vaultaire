package handlers

import (
	"fmt"
	"math"
	"net/url"
	"sort"
	"strings"

	"github.com/FairForge/vaultaire/internal/usage"
)

// The storage room on the overview page: the same room as the site's box
// picker (one box is one terabyte since 2026-10-29) — viewBox 160x66, the
// floor at y 60 — drawn from live data. One cardboard box per Standard TB
// bought (left pile), one steel crate per Vault TB (right pile), up to 3 x 4
// drawn per pile; each box is filled by the bytes used and named and linked
// after the buckets poured into it. You, a plant and a lamp stand in the
// middle. Shadows and night light follow the landing's scenes (builder.js
// shadowShape / lampLevel / tintFor); the sprites, room background and CSS
// come from the generated template (templates/generated/house.html).

// piece kinds and sizes (sprites.py)
type pieceKind struct {
	Kind string
	W, H int
}

var (
	kindBox    = pieceKind{"box", 17, 15}
	kindCrate  = pieceKind{"crate", 17, 15}
	kindLamp   = pieceKind{"floorlamp", 10, 44}
	kindPlant  = pieceKind{"plant", 14, 23}
	kindMascot = pieceKind{"mascot", 16, 26}
)

// lampGlow is the floor lamp's bulb x, y and core pool radii (the landing's
// .glow ellipses on the floor lamp).
var lampGlow = [4]float64{5, 4, 22, 16}

const (
	sceneW    = 160
	floorY    = 60 // where pieces stand
	boxStep   = 13 // a box stands 2 px into the one under it
	pileShown = 12 // 3 columns x 4 rows
)

// pileX is each pile's column x (builder.js PILE).
var pileX = map[string][3]int{usage.FloorStandard: {3, 20, 37}, usage.FloorVault: {106, 123, 140}}

// FloorState is what the scene is built from, per floor.
type FloorState struct {
	Floor      string // usage.FloorStandard | usage.FloorVault
	LimitBytes int64
	UsedBytes  int64
	Buckets    []BucketBytes // any order; sorted by bytes here
}

// BucketBytes is a bucket's bytes on one floor.
type BucketBytes struct {
	Name  string
	Bytes int64
}

// ScenePiece is one drawn piece.
type ScenePiece struct {
	Kind         string
	X, Y, W, H   int
	Floor        string
	CapBytes     int64
	UsedBytes    int64
	FillPct      int    // 0–100 of the sprite's height drawn solid
	FillY, FillH int    // clip rect (sprite coordinates) for the solid part
	Tag          string // the pile's label, on its first box only: "STANDARD 6 TB"
	TagX, TagY   int
	TagW         int
	TagFill      string // yellow Standard, grey-blue Vault
	Title        string // "photos · 0.8 TB of 1 TB Standard"
	Href         string // the bucket, or the bucket list when empty
	Night        string // nf-warm | nf-mid | nf-dim (night tint by lamp distance)
	ClipID       string
	Buckets      []string
	Empty        bool
	ShadowDay    SceneShadow
	ShadowNight  SceneShadow
	Decor        bool // lamp / mascot: no tag, no link
	Symbol       string
	WaveSymbol   string // mascot: the second frame
	LightPool    bool   // lamp: draws the night glow
	Pools        []PoolEllipse
	TagMidX      float64 // tag text anchor
	HitW, HitH   int     // focus outline rect
}

// PoolEllipse is one of the lamp's three screen-blended light pools.
type PoolEllipse struct {
	CX, CY, RX, RY float64
	Fill           string
}

// SceneShadow is the blurred ellipse under a piece.
type SceneShadow struct {
	CX, CY, RX, RY, O float64
}

// FloorSummary is the fullness line per floor.
type FloorSummary struct {
	Floor    string
	Word     string // Standard | Vault
	Bought   bool
	LimitFmt string
	UsedFmt  string
	Pct      int
	Label    string // "4.2 TB of 6 TB Standard"
}

// HouseScene is what the overview template draws.
type HouseScene struct {
	Wall, Fit string // vibe class suffixes
	Pieces    []ScenePiece
	Std       FloorSummary
	Vault     FloorSummary
	AriaLabel string
	Empty     bool // nothing bought or used anywhere
}

var (
	wallNames = map[string]bool{"midnight": true, "matcha": true, "oat": true, "blush": true, "butter": true, "lilac": true}
	fitNames  = map[string]bool{"midnight": true, "matcha": true, "oat": true, "blush": true, "lilac": true, "noir": true}
)

// vibeFromRoom reads the wall and outfit colours out of the share link of
// the old landing builder (v2.<wall>.<fit>.<name>.<pieces>; v1 the same
// shape), stored on tenants who signed up from it; anything else is the
// default midnight room.
func vibeFromRoom(room string) (wall, fit string) {
	wall, fit = "midnight", "midnight"
	parts := strings.Split(room, ".")
	if len(parts) != 5 || (parts[0] != "v1" && parts[0] != "v2") || !wallNames[parts[1]] || !fitNames[parts[2]] {
		return wall, fit
	}
	return parts[1], parts[2]
}

// BuildHouseScene lays out the two piles and the decor. room is the
// tenant's saved share link (may be empty).
func BuildHouseScene(std, vault FloorState, room string) HouseScene {
	sc := HouseScene{}
	sc.Wall, sc.Fit = vibeFromRoom(room)
	std.Floor, vault.Floor = usage.FloorStandard, usage.FloorVault
	sc.Std = summarize(std)
	sc.Vault = summarize(vault)

	var n int
	pieces := layoutPile(std, &n)
	pieces = append(pieces, decor(kindMascot, 57), decor(kindPlant, 74))
	lamp := decor(kindLamp, 91)
	lamp.LightPool = true
	gx, gy := float64(lamp.X)+lampGlow[0], float64(lamp.Y)+lampGlow[1]
	lamp.Pools = []PoolEllipse{ // the landing's three pools: room fill, ambient, core
		{CX: gx, CY: gy + 8, RX: 120, RY: 60, Fill: "url(#lampfill)"},
		{CX: gx, CY: gy + 4, RX: 76, RY: 48, Fill: "url(#lampambient)"},
		{CX: gx, CY: gy, RX: lampGlow[2], RY: lampGlow[3], Fill: "url(#lampglow)"},
	}
	pieces = append(pieces, lamp)
	pieces = append(pieces, layoutPile(vault, &n)...)
	relight(pieces)
	sc.Pieces = pieces

	sc.Empty = std.LimitBytes == 0 && vault.LimitBytes == 0 && std.UsedBytes == 0 && vault.UsedBytes == 0
	sc.AriaLabel = "Your storage: " + sc.Std.Label + ", " + sc.Vault.Label + "."
	return sc
}

func floorWord(floor string) string {
	if floor == usage.FloorVault {
		return "Vault"
	}
	return "Standard"
}

func summarize(f FloorState) FloorSummary {
	s := FloorSummary{Floor: f.Floor, Word: floorWord(f.Floor), Bought: f.LimitBytes > 0,
		LimitFmt: wholeTBFmt(f.LimitBytes), UsedFmt: usedFmt(f.UsedBytes)}
	switch {
	case f.LimitBytes > 0:
		s.Pct = int(math.Min(100, math.Round(float64(f.UsedBytes)*100/float64(f.LimitBytes))))
		s.Label = fmt.Sprintf("%s of %s %s", s.UsedFmt, s.LimitFmt, s.Word)
	case f.UsedBytes > 0:
		s.Label = fmt.Sprintf("%s on %s, nothing bought", s.UsedFmt, s.Word)
	default:
		s.Label = "no " + s.Word + " yet"
	}
	return s
}

// wholeTBFmt prints a bought quota as the site does: whole TB, or the exact
// size below 1 TB (the free tier's 5 GB).
func wholeTBFmt(n int64) string {
	if n <= 0 {
		return "0"
	}
	if n%usage.TB == 0 {
		return fmt.Sprintf("%d TB", n/usage.TB)
	}
	if n > usage.TB {
		return fmt.Sprintf("%.1f TB", float64(n)/float64(usage.TB))
	}
	return formatBytes(n)
}

// usedFmt prints used bytes in TB with one decimal once past 100 GB, so
// "4.2 of 6 TB" reads like the site; smaller amounts use the usual units.
func usedFmt(n int64) string {
	if n >= 100*usage.GB {
		return fmt.Sprintf("%.1f TB", float64(n)/float64(usage.TB))
	}
	return formatBytes(n)
}

// boxesFor is one box per whole TB bought, at most pileShown drawn (they
// then share the TB evenly, the tag says how many are shown); below 1 TB
// (the free tier) one box of the exact size; nothing bought but bytes
// stored (a legacy tenant, a cancelled Vault) one over-full box.
func boxesFor(f FloorState) (caps []int64, tag string) {
	word := strings.ToUpper(floorWord(f.Floor))
	switch {
	case f.LimitBytes <= 0 && f.UsedBytes > 0:
		return []int64{f.UsedBytes}, word + " " + strings.ToUpper(usedFmt(f.UsedBytes))
	case f.LimitBytes <= 0:
		return nil, ""
	case f.LimitBytes < usage.TB:
		return []int64{f.LimitBytes}, word + " " + strings.ToUpper(wholeTBFmt(f.LimitBytes))
	}
	tb := int((f.LimitBytes + usage.TB - 1) / usage.TB)
	tag = word + " " + strings.ToUpper(wholeTBFmt(f.LimitBytes))
	n := tb
	if n > pileShown {
		n = pileShown
		tag += fmt.Sprintf(" (%d SHOWN)", pileShown)
	}
	caps = make([]int64, n)
	left := f.LimitBytes
	for i := range caps {
		caps[i] = left / int64(n-i)
		left -= caps[i]
	}
	return caps, tag
}

// layoutPile stacks one floor's boxes (bottom row first, left to right),
// fills them from the bytes used in order and names them after the buckets
// poured into them, largest bucket first.
func layoutPile(f FloorState, n *int) []ScenePiece {
	caps, tag := boxesFor(f)
	if len(caps) == 0 {
		return nil
	}
	buckets := append([]BucketBytes(nil), f.Buckets...)
	sort.SliceStable(buckets, func(i, j int) bool { return buckets[i].Bytes > buckets[j].Bytes })

	k, tagFill := kindBox, "#ffd400"
	if f.Floor == usage.FloorVault {
		k, tagFill = kindCrate, "#b9c7d2"
	}
	word := floorWord(f.Floor)
	xs := pileX[f.Floor]

	pieces := make([]ScenePiece, 0, len(caps))
	remaining := f.UsedBytes
	bi, bleft := 0, int64(0) // bucket cursor
	if len(buckets) > 0 {
		bleft = buckets[0].Bytes
	}
	top := floorY
	for i, c := range caps {
		*n++
		p := ScenePiece{Kind: k.Kind, Symbol: k.Kind, W: k.W, H: k.H, Floor: f.Floor, CapBytes: c,
			X: xs[i%3], Y: floorY - k.H - (i/3)*boxStep}
		if p.Y < top {
			top = p.Y
		}
		p.HitW, p.HitH = k.W+2, k.H+2
		p.TagFill = tagFill
		p.ClipID = fmt.Sprintf("hs-clip-%d", *n)

		// fill
		p.UsedBytes = remaining
		if p.UsedBytes > p.CapBytes {
			p.UsedBytes = p.CapBytes
		}
		remaining -= p.UsedBytes
		if p.CapBytes > 0 {
			p.FillPct = int(math.Round(float64(p.UsedBytes) * 100 / float64(p.CapBytes)))
		}
		p.Empty = p.UsedBytes == 0
		p.FillH = int(math.Round(float64(k.H) * float64(p.FillPct) / 100))
		if p.UsedBytes > 0 && p.FillH < 2 {
			p.FillH = 2
		}
		p.FillY = k.H - p.FillH

		// buckets: pour the sorted buckets into the boxes in order
		poured := int64(0)
		for poured < p.UsedBytes && bi < len(buckets) {
			take := bleft
			if take > p.UsedBytes-poured {
				take = p.UsedBytes - poured
			}
			if take > 0 || bleft == 0 {
				if len(p.Buckets) == 0 || p.Buckets[len(p.Buckets)-1] != buckets[bi].Name {
					p.Buckets = append(p.Buckets, buckets[bi].Name)
				}
			}
			poured += take
			bleft -= take
			if bleft <= 0 {
				bi++
				if bi < len(buckets) {
					bleft = buckets[bi].Bytes
				}
			}
		}

		capFmt := wholeTBFmt(p.CapBytes)
		switch {
		case p.Empty:
			p.Title = fmt.Sprintf("Empty · %s %s", capFmt, word)
			p.Href = "/dashboard/buckets"
		case len(p.Buckets) == 0:
			p.Title = fmt.Sprintf("%s of %s %s", usedFmt(p.UsedBytes), capFmt, word)
			p.Href = "/dashboard/buckets"
		default:
			p.Title = fmt.Sprintf("%s · %s of %s %s", strings.Join(p.Buckets, ", "), usedFmt(p.UsedBytes), capFmt, word)
			p.Href = "/dashboard/buckets/" + url.PathEscape(p.Buckets[0])
		}
		pieces = append(pieces, p)
	}
	// the pile's one label, centred over the columns it uses, above its top box
	cols := len(caps)
	if cols > 3 {
		cols = 3
	}
	first := &pieces[0]
	first.Tag = tag
	first.TagW = len(tag)*3 + 3
	cx := float64(xs[0]) + float64(cols*k.W-3)/2
	first.TagX = int(math.Max(1, math.Min(float64(sceneW-1-first.TagW), math.Round(cx-float64(first.TagW)/2))))
	first.TagY = int(math.Max(0, float64(top-7))) // a full pile: the label touches the ceiling
	first.TagMidX = float64(first.TagW) / 2
	return pieces
}

func decor(k pieceKind, x int) ScenePiece {
	p := ScenePiece{Kind: k.Kind, Symbol: k.Kind, W: k.W, H: k.H, X: x, Y: floorY - k.H, Decor: true, Floor: usage.FloorStandard}
	switch k.Kind {
	case "mascot":
		p.WaveSymbol = "mascot-wave"
		p.Title = "You"
	case "plant":
		p.Title = "Plant"
	default:
		p.Title = "Lamp"
	}
	return p
}

// ---- light (builder.js shadowShape / lampLevel / tintFor) ----

type light struct{ x, reach, weight float64 }

func shadowShape(cx, bottom, w float64, lights []light, night bool) SceneShadow {
	best, dir := 0.0, 0.0
	for _, L := range lights {
		dx := cx - L.x
		d := math.Abs(dx)
		if d >= L.reach {
			continue
		}
		s := (1 - d/L.reach) * L.weight
		if s > best {
			best = s
			if dx >= 0 {
				dir = 1
			} else {
				dir = -1
			}
		}
	}
	base := 0.2
	if night {
		base = 0.34
	}
	return SceneShadow{
		CX: r2(cx + dir*best*w*0.45), CY: r2(bottom - 0.6),
		RX: r2(w*0.55 + best*w*0.7), RY: r2(2.2 + best*0.8),
		O: r2(math.Min(base+best*0.18, 0.55)),
	}
}

func r2(v float64) float64 { return math.Round(v*100) / 100 }

type lampLight struct{ x, y, rx, ry, reach float64 }

func lampLevel(px, py float64, lamps []lampLight) float64 {
	lvl := 0.0
	for _, L := range lamps {
		dx, dy := (px-L.x)/L.rx, (py-L.y)/L.ry
		lvl = math.Max(lvl, 1-math.Sqrt(dx*dx+dy*dy))
	}
	return lvl
}

func tintFor(lvl float64) string {
	switch {
	case lvl > 0.45:
		return "nf-warm"
	case lvl > 0.12:
		return "nf-mid"
	}
	return "nf-dim"
}

// relight computes each piece's day and night shadows and its night tint:
// by day the light comes from the left (the landing's scenes), by night from
// the lamp.
func relight(pieces []ScenePiece) {
	var lamps []lampLight
	var nightLights []light
	for _, p := range pieces {
		if !p.LightPool {
			continue
		}
		L := lampLight{x: float64(p.X) + lampGlow[0], y: float64(p.Y) + lampGlow[1] + 5,
			rx: lampGlow[2] * 3.6, ry: lampGlow[3] * 3.1, reach: lampGlow[2] * 4}
		lamps = append(lamps, L)
		nightLights = append(nightLights, light{x: L.x, reach: L.reach, weight: 1})
	}
	dayLights := []light{{x: -40, reach: sceneW + 80, weight: 0.35}}
	for i := range pieces {
		p := &pieces[i]
		cx := float64(p.X) + float64(p.W)/2
		bottom := float64(p.Y + p.H)
		p.ShadowDay = shadowShape(cx, bottom, float64(p.W), dayLights, false)
		p.ShadowNight = shadowShape(cx, bottom, float64(p.W), nightLights, true)
		if p.LightPool {
			continue // the lamp lights itself
		}
		p.Night = tintFor(lampLevel(cx, float64(p.Y)+float64(p.H)/2, lamps))
	}
}
