package witness

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"sync"

	"github.com/FairForge/vaultaire/internal/translog"
)

// Cache is the witness's byte-exact mirror under <dir>/cache, in the
// c2sp.org/tlog-tiles layout itself (tile/<L>/<N>, tile/entries/<N>[.p/<W>]):
// full Merkle tiles and full entry bundles, which never change, and the
// partial entry bundle at the newest checkpoint, replaced as it widens.
// translog-restore reads entries back from it.
type Cache struct {
	root string
	mu   sync.Mutex
	err  error // the first failed SaveTile (tlog's hook returns nothing)
}

// OpenCache is the cache under dir (the witness's -dir); nothing is created
// until something is saved.
func OpenCache(dir string) *Cache { return &Cache{root: filepath.Join(dir, "cache")} }

// file is where a tile path lives; a path outside the layout is refused.
func (c *Cache) file(path string) (string, error) {
	if _, _, _, err := translog.ParseTilePath(path); err != nil {
		return "", fmt.Errorf("witness cache: %q: %w", path, err)
	}
	return filepath.Join(c.root, filepath.FromSlash(path)), nil
}

// Tile implements translog.TileCache: a full tile saved earlier.
func (c *Cache) Tile(path string) ([]byte, bool) {
	f, err := c.file(path)
	if err != nil {
		return nil, false
	}
	b, err := os.ReadFile(f) // #nosec G304 -- a tile path parsed above, under the witness's own directory
	if err != nil {
		return nil, false
	}
	return b, true
}

// SaveTile implements translog.TileCache: tlog authenticated the tile, so
// it is kept — never overwritten (a full tile is immutable). A failure is
// kept for Err.
func (c *Cache) SaveTile(path string, data []byte) {
	if _, ok := c.Tile(path); ok {
		return
	}
	if err := c.write(path, data); err != nil {
		c.mu.Lock()
		if c.err == nil {
			c.err = err
		}
		c.mu.Unlock()
	}
}

// Err is the first SaveTile that failed, nil when none.
func (c *Cache) Err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.err
}

func (c *Cache) write(path string, data []byte) error {
	f, err := c.file(path)
	if err != nil {
		return err
	}
	if err := writeAtomic(f, data); err != nil {
		return fmt.Errorf("witness cache: %s: %w", path, err)
	}
	return nil
}

// Bundle is the widest copy of entry bundle n the cache holds: the full one,
// else the widest partial; ok = false when none.
func (c *Cache) Bundle(n int64) (raw []byte, width int, ok bool, err error) {
	full := translog.TilePath(translog.EntriesLevel, n, translog.TileWidth)
	f, err := c.file(full)
	if err != nil {
		return nil, 0, false, err
	}
	raw, err = os.ReadFile(f) // #nosec G304 -- a tile path built by translog.TilePath, under the witness's own directory
	if err == nil {
		return raw, translog.TileWidth, true, nil
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return nil, 0, false, fmt.Errorf("witness cache: %s: %w", full, err)
	}
	w, err := c.widestPartial(n)
	if err != nil || w == 0 {
		return nil, 0, false, err
	}
	p := translog.TilePath(translog.EntriesLevel, n, w)
	f, err = c.file(p)
	if err != nil {
		return nil, 0, false, err
	}
	raw, err = os.ReadFile(f) // #nosec G304 -- as above
	if err != nil {
		return nil, 0, false, fmt.Errorf("witness cache: %s: %w", p, err)
	}
	return raw, w, true, nil
}

// partials lists the widths of the partial copies of bundle n.
func (c *Cache) partials(n int64) ([]int, error) {
	f, err := c.file(translog.TilePath(translog.EntriesLevel, n, 1))
	if err != nil {
		return nil, err
	}
	des, err := os.ReadDir(filepath.Dir(f))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("witness cache: partials of bundle %d: %w", n, err)
	}
	var out []int
	for _, de := range des {
		// <N>.p/<W>, W in 1…255 in its canonical spelling (a ".tmp-*"
		// file of an interrupted write is not one).
		w, err := strconv.Atoi(de.Name())
		if err == nil && !de.IsDir() && w >= 1 && w < translog.TileWidth && strconv.Itoa(w) == de.Name() {
			out = append(out, w)
		}
	}
	return out, nil
}

func (c *Cache) widestPartial(n int64) (int, error) {
	ws, err := c.partials(n)
	best := 0
	for _, w := range ws {
		best = max(best, w)
	}
	return best, err
}

// SaveBundle keeps entry bundle n of width entries, raw as served. A full
// bundle replaces every partial copy of it; a partial one every narrower
// partial (the newest checkpoint's is the one a restore needs).
func (c *Cache) SaveBundle(n int64, width int, raw []byte) error {
	if err := c.write(translog.TilePath(translog.EntriesLevel, n, width), raw); err != nil {
		return err
	}
	ws, err := c.partials(n)
	if err != nil {
		return err
	}
	for _, w := range ws {
		if w < width {
			f, err := c.file(translog.TilePath(translog.EntriesLevel, n, w))
			if err != nil {
				return err
			}
			if err := os.Remove(f); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return fmt.Errorf("witness cache: drop the narrower copy of bundle %d: %w", n, err)
			}
		}
	}
	return nil
}

// Entries returns the bodies of entries from … to-1 out of the cached
// bundles, byte for byte. A bundle that is missing, or narrower than the
// range needs, is an error naming it.
func (c *Cache) Entries(from, to int64) ([][]byte, error) {
	if from < 0 || to < from {
		return nil, fmt.Errorf("witness cache: bad range %d…%d", from, to)
	}
	var out [][]byte
	for n := from / translog.TileWidth; n*translog.TileWidth < to; n++ {
		raw, width, ok, err := c.Bundle(n)
		if err != nil {
			return nil, err
		}
		need := min(to-n*translog.TileWidth, translog.TileWidth)
		if !ok {
			return nil, fmt.Errorf("witness cache: entry bundle %d (%s, entries %d…%d) is missing",
				n, translog.TilePath(translog.EntriesLevel, n, int(need)), n*translog.TileWidth, n*translog.TileWidth+need-1)
		}
		if int64(width) < need {
			return nil, fmt.Errorf("witness cache: entry bundle %d holds %d entries, %d are needed (the witness never saw a checkpoint past entry %d)",
				n, width, need, n*translog.TileWidth+int64(width)-1)
		}
		bodies, err := translog.ParseEntryBundle(raw)
		if err != nil {
			return nil, fmt.Errorf("witness cache: entry bundle %d: %w", n, err)
		}
		if len(bodies) != width {
			return nil, fmt.Errorf("witness cache: entry bundle %d holds %d entries, its name says %d", n, len(bodies), width)
		}
		for i, b := range bodies {
			idx := n*translog.TileWidth + int64(i)
			if idx >= from && idx < to {
				out = append(out, b)
			}
		}
	}
	return out, nil
}

// writeAtomic replaces path with data: a temporary file in the same
// directory, synced, renamed over it, the directory synced.
func writeAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer func() { _ = os.Remove(tmp) }()
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	if d, err := os.Open(dir); err == nil { // #nosec G304 -- the file's own directory
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}
