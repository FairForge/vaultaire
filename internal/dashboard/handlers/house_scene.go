package handlers

import (
	"fmt"
	"math"
	"net/url"
	"sort"
	"strings"

	"github.com/FairForge/vaultaire/internal/usage"
)

// The house on the overview page (docs/DASHBOARD_PLAN.md Phase 2): the same
// room as the site — viewBox 128×115, attic 10–54, downstairs 57–103 — drawn
// from live data. Pieces are sized by the quota bought per floor (bookcase
// 10 TB, dresser 5 TB, box 1 TB), their fill by the bytes used, their titles
// and links by the buckets that occupy them. Layout, shadows and night light
// are a port of the landing builder's rules (internal/api/landing/builder.js:
// spanOf/freeX, shadowShape, lampLevel/tintFor) so both houses look alike;
// the sprites, room background and CSS come from the generated template
// (internal/dashboard/templates/generated/house.html).

// piece kinds, sizes and capacities (sprites.py / builder.js P)
type pieceKind struct {
	Kind string
	W, H int
	TB   int
}

var (
	kindBox      = pieceKind{"box", 17, 15, 1}
	kindDresser  = pieceKind{"dresser", 31, 23, 5}
	kindBookcase = pieceKind{"bookcase", 31, 44, 10}
	kindLamp     = pieceKind{"lamp", 10, 13, 0}
	kindMascot   = pieceKind{"mascot", 16, 26, 0}
)

// lampGlow is builder.js P.lamp.glow: the bulb's x, y and the pool radii.
var lampGlow = [4]float64{5, 3, 18, 13}

const (
	sceneW      = 128
	atticFloor  = 54  // ZONE.attic.floor
	groundFloor = 103 // ZONE.ground.floor
	firstX      = 16  // clear of the ladder
	lastX       = 126 // right margin
	pieceGap    = 2
)

// window x per floor, the day light source (builder.js WINDOW_X)
var windowX = map[string]float64{usage.FloorStandard: 99, usage.FloorVault: 104}

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
	Tag          string // "5 TB"
	TagX, TagY   int
	TagW         int
	TagFill      string // floor colour
	Title        string // "photos · 3.1 TB of 5 TB, downstairs"
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
	Word     string // downstairs | attic
	Bought   bool
	LimitFmt string
	UsedFmt  string
	Pct      int
	Label    string // "4.2 of 6 TB downstairs"
}

// HouseScene is what the overview template draws.
type HouseScene struct {
	Wall, Fit string // vibe class suffixes
	Name      string // the sign on the wall
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

// vibeFromRoom reads the wall, outfit and name out of a share link
// (v2.<wall>.<fit>.<name>.<pieces>; v1 the same shape) — the room the
// customer built on the site, stored on the tenant at signup.
func vibeFromRoom(room string) (wall, fit, name string) {
	wall, fit = "midnight", "midnight"
	parts := strings.Split(room, ".")
	if len(parts) != 5 || (parts[0] != "v1" && parts[0] != "v2") || !wallNames[parts[1]] || !fitNames[parts[2]] {
		return wall, fit, ""
	}
	wall, fit = parts[1], parts[2]
	if n, err := url.QueryUnescape(parts[3]); err == nil {
		name = cleanRoomName(n)
	}
	return wall, fit, name
}

// cleanRoomName keeps the sign readable: printable, no angle brackets,
// at most 18 characters (the sign is 40 px wide at 5 px per glyph).
func cleanRoomName(s string) string {
	var b strings.Builder
	for _, r := range strings.TrimSpace(s) {
		if r < 0x20 || r == '<' || r == '>' || r == 0x7f {
			continue
		}
		b.WriteRune(r)
		if b.Len() >= 18 {
			break
		}
	}
	return strings.TrimSpace(b.String())
}

// BuildHouseScene lays out both floors. room is the tenant's saved share
// link (may be empty).
func BuildHouseScene(std, vault FloorState, room string) HouseScene {
	sc := HouseScene{}
	sc.Wall, sc.Fit, sc.Name = vibeFromRoom(room)
	if sc.Name == "" {
		sc.Name = "my place"
	}
	std.Floor, vault.Floor = usage.FloorStandard, usage.FloorVault
	sc.Std = summarize(std)
	sc.Vault = summarize(vault)

	var n int
	sc.Pieces = append(sc.Pieces, layoutFloor(vault, atticFloor, &n)...)
	ground := layoutFloor(std, groundFloor, &n)
	// Decor downstairs where there is room: you, then a lamp.
	cursor := firstX
	for _, p := range ground {
		if end := p.X + spanOf(p.W, p.Tag).w + pieceGap; end > cursor {
			cursor = end
		}
	}
	var lamp *ScenePiece
	if cursor+kindMascot.W+pieceGap <= lastX-14 { // keep the window clear
		ground = append(ground, decor(kindMascot, cursor, groundFloor))
		cursor += kindMascot.W + pieceGap
	}
	if cursor+kindLamp.W <= lastX-14 {
		ground = append(ground, decor(kindLamp, cursor, groundFloor))
		lamp = &ground[len(ground)-1]
		lamp.LightPool = true
		gx, gy := float64(lamp.X)+lampGlow[0], float64(lamp.Y)+lampGlow[1]
		lamp.Pools = []PoolEllipse{ // builder.js lightPool: room fill, ambient, core
			{CX: gx, CY: gy + 10, RX: lampGlow[2] * 6, RY: lampGlow[3] * 4.6, Fill: "url(#lampfill)"},
			{CX: gx, CY: gy + 5, RX: lampGlow[2] * 3.6, RY: lampGlow[3] * 3.1, Fill: "url(#lampambient)"},
			{CX: gx, CY: gy, RX: lampGlow[2], RY: lampGlow[3], Fill: "url(#lampglow)"},
		}
	}
	relight(ground, lamp, usage.FloorStandard)
	relight(sc.Pieces, nil, usage.FloorVault) // no lamp in the attic: night = dim
	sc.Pieces = append(sc.Pieces, ground...)

	sc.Empty = std.LimitBytes == 0 && vault.LimitBytes == 0 && std.UsedBytes == 0 && vault.UsedBytes == 0
	sc.AriaLabel = "Your house: " + sc.Std.Label + ", " + sc.Vault.Label + "."
	return sc
}

func summarize(f FloorState) FloorSummary {
	s := FloorSummary{Floor: f.Floor, Word: "downstairs", Bought: f.LimitBytes > 0,
		LimitFmt: wholeTBFmt(f.LimitBytes), UsedFmt: usedFmt(f.UsedBytes)}
	if f.Floor == usage.FloorVault {
		s.Word = "attic"
	}
	if f.LimitBytes > 0 {
		s.Pct = int(math.Min(100, math.Round(float64(f.UsedBytes)*100/float64(f.LimitBytes))))
		s.Label = fmt.Sprintf("%s of %s %s", s.UsedFmt, s.LimitFmt, s.Word)
		if f.Floor == usage.FloorVault {
			s.Label = fmt.Sprintf("%s of %s in the attic", s.UsedFmt, s.LimitFmt)
		}
	} else {
		s.Label = "no " + s.Word
		if f.Floor == usage.FloorVault {
			s.Label = "no attic"
		}
		if f.UsedBytes > 0 {
			s.Label = fmt.Sprintf("%s %s, nothing bought", s.UsedFmt, s.Word)
		}
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

// decompose turns a whole-TB quota into pieces, greedy 10/5/1.
func decompose(tb int) []pieceKind {
	var out []pieceKind
	for tb >= kindBookcase.TB {
		out = append(out, kindBookcase)
		tb -= kindBookcase.TB
	}
	for tb >= kindDresser.TB {
		out = append(out, kindDresser)
		tb -= kindDresser.TB
	}
	for tb >= 1 {
		out = append(out, kindBox)
		tb--
	}
	return out
}

type span struct{ off, w int }

// spanOf is builder.js spanOf: a piece claims the wider of its own width and
// its tag, centred on the piece.
func spanOf(w int, tag string) span {
	tw := len(tag)*3 + 3
	sw := w
	if tw > sw {
		sw = tw
	}
	return span{off: int(math.Round(float64(w)/2 - float64(sw)/2)), w: sw}
}

// rowFits reports whether pieces with their tags fit on one floor left to
// right from firstX with pieceGap between spans.
func rowFits(kinds []pieceKind, tags []string) bool {
	c := firstX
	for i, k := range kinds {
		c += spanOf(k.W, tags[i]).w + pieceGap
	}
	return c-pieceGap <= lastX
}

// piecesFor picks the pieces for a floor: the greedy split when it fits the
// row, otherwise up to three bookcases sharing the TB (tags carry the real
// amount, so a 100 TB floor reads "34 TB · 33 TB · 33 TB").
func piecesFor(limit int64) ([]pieceKind, []int64) {
	if limit <= 0 {
		return nil, nil
	}
	if limit < usage.TB {
		return []pieceKind{kindBox}, []int64{limit}
	}
	tb := int((limit + usage.TB - 1) / usage.TB)
	kinds := decompose(tb)
	caps := make([]int64, len(kinds))
	tags := make([]string, len(kinds))
	for i, k := range kinds {
		caps[i] = int64(k.TB) * usage.TB
		tags[i] = fmt.Sprintf("%d TB", k.TB)
	}
	if rowFits(kinds, tags) {
		return kinds, caps
	}
	n := 3
	if tb < 30 {
		n = (tb + 9) / 10
	}
	kinds = kinds[:0]
	caps = caps[:0]
	rem := tb
	for i := 0; i < n; i++ {
		share := (rem + (n - i) - 1) / (n - i)
		kinds = append(kinds, kindBookcase)
		caps = append(caps, int64(share)*usage.TB)
		rem -= share
	}
	return kinds, caps
}

func tagFor(cap int64) string {
	if cap%usage.TB == 0 {
		return fmt.Sprintf("%d TB", cap/usage.TB)
	}
	return strings.ToUpper(formatBytes(cap))
}

// layoutFloor places the pieces of one floor, fills them from the bytes used
// (left to right) and names them after the buckets that occupy them.
func layoutFloor(f FloorState, floorY int, n *int) []ScenePiece {
	kinds, caps := piecesFor(f.LimitBytes)
	if len(kinds) == 0 && f.UsedBytes > 0 {
		// Nothing bought on this floor but bytes live here (a legacy tenant,
		// or an attic that was cancelled): one box, over-full.
		kinds, caps = []pieceKind{kindBox}, []int64{f.UsedBytes}
	}
	buckets := append([]BucketBytes(nil), f.Buckets...)
	sort.SliceStable(buckets, func(i, j int) bool { return buckets[i].Bytes > buckets[j].Bytes })

	word := "downstairs"
	tagFill := "#ffd400"
	if f.Floor == usage.FloorVault {
		word, tagFill = "in the attic", "#b9c7d2"
	}

	pieces := make([]ScenePiece, 0, len(kinds))
	cursor := firstX
	remaining := f.UsedBytes
	bi, bleft := 0, int64(0) // bucket cursor
	if len(buckets) > 0 {
		bleft = buckets[0].Bytes
	}
	for i, k := range kinds {
		*n++
		p := ScenePiece{Kind: k.Kind, Symbol: k.Kind, W: k.W, H: k.H, Floor: f.Floor, CapBytes: caps[i]}
		p.Tag = tagFor(caps[i])
		sp := spanOf(k.W, p.Tag)
		p.X = cursor - sp.off
		p.Y = floorY - k.H
		cursor += sp.w + pieceGap
		p.TagW = len(p.Tag)*3 + 3
		p.TagX = p.X + int(math.Round(float64(k.W)/2-float64(p.TagW)/2))
		p.TagY = p.Y - 6
		p.TagMidX = float64(p.TagW) / 2
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

		// buckets: pour the sorted buckets into the pieces in order
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
			names := p.Buckets[0]
			if len(p.Buckets) > 1 {
				names = strings.Join(p.Buckets, ", ")
			}
			p.Title = fmt.Sprintf("%s · %s of %s %s", names, usedFmt(p.UsedBytes), capFmt, word)
			p.Href = "/dashboard/buckets/" + url.PathEscape(p.Buckets[0])
		}
		pieces = append(pieces, p)
	}
	return pieces
}

func decor(k pieceKind, x, floorY int) ScenePiece {
	p := ScenePiece{Kind: k.Kind, Symbol: k.Kind, W: k.W, H: k.H, X: x, Y: floorY - k.H, Decor: true, Floor: usage.FloorStandard}
	if k.Kind == "mascot" {
		p.WaveSymbol = "mascot-wave"
		p.Title = "You"
	} else {
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

// relight computes each piece's day and night shadows and its night tint.
func relight(pieces []ScenePiece, lamp *ScenePiece, floor string) {
	var lamps []lampLight
	var nightLights []light
	if lamp != nil {
		L := lampLight{x: float64(lamp.X) + lampGlow[0], y: float64(lamp.Y) + lampGlow[1] + 5,
			rx: lampGlow[2] * 3.6, ry: lampGlow[3] * 3.1, reach: lampGlow[2] * 4}
		lamps = append(lamps, L)
		nightLights = append(nightLights, light{x: L.x, reach: L.reach, weight: 1})
	}
	dayLights := []light{{x: windowX[floor], reach: 80, weight: 0.5}}
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
