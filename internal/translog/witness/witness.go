// Package witness is the transparency log's external witness (keystone
// K4b, plan Phase 43.6, docs/ARCHITECTURE_CONTRACT.md rule 7: "the log has
// signed checkpoints and an external witness"; docs/TRANSPARENCY.md
// § The witness). It runs on any machine against the public read side and
// trusts nothing of ours but the pinned verifier key of the log.
//
// One pass: fetch the newest checkpoint and verify its signature; prove it
// extends the checkpoint the witness accepted last (a rewritten, forked or
// shrunk history is refused); mirror, byte for byte, every full entry
// bundle and full level-0 hash tile it does not hold yet and the partial
// entry bundle at the checkpoint's size, each entry's leaf hash proven
// against the checkpoint before it is kept; then co-sign the checkpoint
// with the witness's OWN key — the note carries both signature lines — and
// keep that note (<dir>/cosigned, and one line per new note in the
// append-only <dir>/cosigned.history). The accepted checkpoint
// (<dir>/checkpoint) moves forward only after every step passed.
//
// Why: with the witness's cache, entries lost to a database restore can be
// put back byte for byte (cmd/tools/translog-restore, RP38-6), and a
// checkpoint the restored log must extend survives the loss of the box and
// its high-water file (RP38-5). This is our own second record under our own
// key on another machine; the c2sp.org/tlog-cosignature timestamped form
// comes with a real third-party witness (K4c).
//
// Operator code: imported by cmd/tools only, never linked into cmd/vaultaire.
package witness

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"golang.org/x/mod/sumdb/note"

	"github.com/FairForge/vaultaire/internal/translog"
)

// Witness is one witness: its directory, the log's client (Verifier = the
// pinned log key) and the witness's own signer.
type Witness struct {
	Dir    string
	Client *translog.Client
	Signer note.Signer
}

// Files of a witness directory.
const (
	StateFile   = "checkpoint"       // the log's note the witness accepted last
	CosignFile  = "cosigned"         // that note with the witness's signature added
	HistoryFile = "cosigned.history" // every new cosigned note, base64, one per line
)

// Result is what one pass did.
type Result struct {
	// PrevSize is the accepted checkpoint's size before the pass (-1 = none:
	// the first pass).
	PrevSize int64
	Size     int64
	Root     translog.Hash
	// Bundles is how many entry bundles were fetched and kept this pass.
	Bundles int
	// Cosigned is the checkpoint with both signatures.
	Cosigned []byte
}

// ErrSameKey: the witness was given the log's own key — a co-signature by
// it would say nothing.
var ErrSameKey = errors.New("witness: the witness key is the log's key — make the witness its own key (translog-keygen -name)")

// Pass runs one pass (see the package comment). On any error nothing it
// accepted moves: the state file is untouched (the cache may hold more
// verified bytes, never unverified ones).
func (w *Witness) Pass(ctx context.Context) (Result, error) {
	res := Result{PrevSize: -1}
	if w.Client == nil || w.Client.Verifier == nil || w.Signer == nil || w.Dir == "" {
		return res, errors.New("witness: a directory, the log's verifier key and the witness's signer key are required")
	}
	v := w.Client.Verifier
	if w.Signer.Name() == v.Name() && w.Signer.KeyHash() == v.KeyHash() {
		return res, ErrSameKey
	}
	cache := OpenCache(w.Dir)

	cp, msg, err := w.Client.Checkpoint(ctx)
	if err != nil {
		return res, fmt.Errorf("witness: checkpoint: %w", err)
	}
	res.Size, res.Root = cp.Size, cp.Root

	// Consistency with what was accepted before, with a READ-ONLY view of
	// the cache: a forked tree's tiles are never kept.
	prev, err := os.ReadFile(filepath.Join(w.Dir, StateFile))
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		return res, fmt.Errorf("witness: read the accepted checkpoint: %w", err)
	default:
		old, err := translog.OpenCheckpoint(prev, v)
		if err != nil {
			return res, fmt.Errorf("witness: the accepted checkpoint does not open with the log's key: %w", err)
		}
		res.PrevSize = old.Size
		ro := *w.Client
		ro.Tiles = readOnly{cache}
		if err := ro.VerifyConsistency(ctx, old, cp); err != nil {
			return res, fmt.Errorf("witness: the log does not extend the checkpoint accepted before (size %d): %w", old.Size, err)
		}
	}

	// Mirror: proven tiles go to the cache through the client's hook.
	mc := *w.Client
	mc.Tiles = cache
	for n := int64(0); n*translog.TileWidth < cp.Size; n++ {
		width := translog.TileWidthAt(translog.EntriesLevel, n, cp.Size)
		_, have, ok, err := cache.Bundle(n)
		if err != nil {
			return res, err
		}
		if ok && have >= width {
			if width < translog.TileWidth {
				continue
			}
			if _, tileOK := cache.Tile(translog.TilePath(0, n, translog.TileWidth)); tileOK {
				continue
			}
		}
		raw, _, err := mc.EntryBundle(ctx, cp, n)
		if err != nil {
			return res, fmt.Errorf("witness: %w", err)
		}
		if err := cache.SaveBundle(n, width, raw); err != nil {
			return res, err
		}
		res.Bundles++
	}
	if err := cache.Err(); err != nil {
		return res, err
	}

	// Co-sign: the log's verified signature kept, the witness's added.
	n, err := note.Open(msg, note.VerifierList(v))
	if err != nil {
		return res, fmt.Errorf("witness: open the checkpoint note: %w", err)
	}
	cos, err := note.Sign(n, w.Signer)
	if err != nil {
		return res, fmt.Errorf("witness: co-sign: %w", err)
	}
	res.Cosigned = cos
	cosPath := filepath.Join(w.Dir, CosignFile)
	before, err := os.ReadFile(cosPath) // #nosec G304 -- the witness's own directory
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return res, fmt.Errorf("witness: read %s: %w", CosignFile, err)
	}
	if !bytes.Equal(before, cos) {
		if err := appendLine(filepath.Join(w.Dir, HistoryFile), base64.StdEncoding.EncodeToString(cos)); err != nil {
			return res, err
		}
		if err := writeAtomic(cosPath, cos); err != nil {
			return res, fmt.Errorf("witness: write %s: %w", CosignFile, err)
		}
	}
	if err := writeAtomic(filepath.Join(w.Dir, StateFile), msg); err != nil {
		return res, fmt.Errorf("witness: write %s: %w", StateFile, err)
	}
	return res, nil
}

// readOnly is a cache view that keeps nothing.
type readOnly struct{ c *Cache }

func (r readOnly) Tile(path string) ([]byte, bool) { return r.c.Tile(path) }
func (readOnly) SaveTile(string, []byte)           {}

// appendLine appends line to the append-only history, synced.
func appendLine(path, line string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("witness: %w", err)
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600) // #nosec G304 -- the witness's own directory
	if err != nil {
		return fmt.Errorf("witness: open %s: %w", path, err)
	}
	if _, err := f.WriteString(line + "\n"); err != nil {
		_ = f.Close()
		return fmt.Errorf("witness: append to %s: %w", path, err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return fmt.Errorf("witness: sync %s: %w", path, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("witness: close %s: %w", path, err)
	}
	return nil
}
