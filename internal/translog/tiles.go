package translog

import (
	"encoding/binary"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"golang.org/x/mod/sumdb/tlog"
)

// The tile layout served (c2sp.org/tlog-tiles):
//
//	checkpoint                      the newest signed checkpoint
//	tile/<L>/<N>[.p/<W>]            Merkle tiles: level L (0…63), index N
//	tile/entries/<N>[.p/<W>]        entry bundles: entries N·256 … N·256+W-1
//
// N is written in zero-padded groups of three digits, every group but the
// last prefixed by "x" (1234067 → x001/x234/067). A full tile has 256
// hashes (or entries) and no ".p/W"; a partial one has W of 1…255. The
// contents of a Merkle tile are tlog's height-8 tiles: tile level L holds
// the tree's level-8L hashes, concatenated.
const (
	// TileHeight is log2 of TileWidth.
	TileHeight = 8
	// TileWidth is the hashes (or entries) in a full tile.
	TileWidth = 1 << TileHeight
	// EntriesLevel is the level argument naming an entry bundle.
	EntriesLevel = -1
	maxLevel     = 63
)

// ErrBadTilePath: a path outside the layout.
var ErrBadTilePath = errors.New("translog: not a tile path")

// TilePath is the path of tile N at level (EntriesLevel = an entry bundle)
// with width entries (TileWidth = full).
func TilePath(level int, n int64, width int) string {
	l := "entries"
	if level != EntriesLevel {
		l = strconv.Itoa(level)
	}
	nStr := fmt.Sprintf("%03d", n%1000)
	for n >= 1000 {
		n /= 1000
		nStr = fmt.Sprintf("x%03d/%s", n%1000, nStr)
	}
	p := "tile/" + l + "/" + nStr
	if width != TileWidth {
		p += ".p/" + strconv.Itoa(width)
	}
	return p
}

// ParseTilePath is TilePath's inverse; anything TilePath would not print
// is ErrBadTilePath.
func ParseTilePath(path string) (level int, n int64, width int, err error) {
	rest, ok := strings.CutPrefix(path, "tile/")
	if !ok {
		return 0, 0, 0, ErrBadTilePath
	}
	lStr, rest, ok := strings.Cut(rest, "/")
	if !ok {
		return 0, 0, 0, ErrBadTilePath
	}
	if lStr == "entries" {
		level = EntriesLevel
	} else {
		level, err = strconv.Atoi(lStr)
		if err != nil || level < 0 || level > maxLevel || strconv.Itoa(level) != lStr {
			return 0, 0, 0, ErrBadTilePath
		}
	}
	width = TileWidth
	if nPart, wStr, partial := strings.Cut(rest, ".p/"); partial {
		width, err = strconv.Atoi(wStr)
		if err != nil || width < 1 || width >= TileWidth || strconv.Itoa(width) != wStr {
			return 0, 0, 0, ErrBadTilePath
		}
		rest = nPart
	}
	groups := strings.Split(rest, "/")
	for i, g := range groups {
		last := i == len(groups)-1
		if !last {
			var ok bool
			if g, ok = strings.CutPrefix(g, "x"); !ok {
				return 0, 0, 0, ErrBadTilePath
			}
		}
		if len(g) != 3 || strings.Trim(g, "0123456789") != "" {
			return 0, 0, 0, ErrBadTilePath
		}
		d, _ := strconv.Atoi(g)
		n = n*1000 + int64(d)
		if n > 1<<54 {
			return 0, 0, 0, ErrBadTilePath
		}
	}
	// The canonical form only: no leading x000 group.
	if TilePath(level, n, width) != path {
		return 0, 0, 0, ErrBadTilePath
	}
	return level, n, width, nil
}

// TileData is the content of Merkle tile n at level with width hashes:
// tlog.ReadTileData of the height-8 tile.
func TileData(level int, n int64, width int, r HashReader) ([]byte, error) {
	if level < 0 || level > maxLevel || n < 0 || width < 1 || width > TileWidth {
		return nil, ErrBadTilePath
	}
	data, err := tlog.ReadTileData(tlog.Tile{H: TileHeight, L: level, N: n, W: width}, r)
	if err != nil {
		return nil, fmt.Errorf("translog: tile %s: %w", TilePath(level, n, width), err)
	}
	return data, nil
}

// TileWidthAt is the width of tile n at level in a tree of size entries:
// TileWidth when full, 1…255 when partial, 0 when the tile does not exist
// yet. For EntriesLevel it counts entries.
func TileWidthAt(level int, n, size int64) int {
	shift := uint(0)
	if level > 0 {
		shift = uint(TileHeight * level)
	}
	if shift >= 63 {
		return 0
	}
	nodes := size >> shift // complete nodes at the tile's tree level
	start := n * TileWidth
	if nodes <= start {
		return 0
	}
	if nodes-start >= TileWidth {
		return TileWidth
	}
	return int(nodes - start)
}

// EntryBundle is entries as one bundle: each a big-endian uint16 length
// followed by its bytes.
func EntryBundle(entries [][]byte) ([]byte, error) {
	size := 0
	for _, e := range entries {
		if len(e) > MaxEntrySize {
			return nil, ErrEntryTooLarge
		}
		size += 2 + len(e)
	}
	out := make([]byte, 0, size)
	for _, e := range entries {
		out = binary.BigEndian.AppendUint16(out, uint16(len(e)))
		out = append(out, e...)
	}
	return out, nil
}

// ParseEntryBundle splits a bundle into its entries.
func ParseEntryBundle(b []byte) ([][]byte, error) {
	var out [][]byte
	for len(b) > 0 {
		if len(b) < 2 {
			return nil, errors.New("translog: truncated entry bundle")
		}
		l := int(binary.BigEndian.Uint16(b))
		if len(b) < 2+l {
			return nil, errors.New("translog: truncated entry bundle")
		}
		out = append(out, append([]byte{}, b[2:2+l]...))
		b = b[2+l:]
	}
	return out, nil
}
