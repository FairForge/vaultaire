// Package witness is the transparency log's external witness (keystone
// K4b/K4c-2, plan Phase 43.6, docs/ARCHITECTURE_CONTRACT.md rule 7: "the log
// has signed checkpoints and an external witness"; docs/TRANSPARENCY.md
// § The witness). It runs on any machine and trusts nothing of ours but the
// pinned verifier key of the log. It cosigns in the c2sp.org/tlog-cosignature
// form (cosignature/v1: timestamped, ML-DSA-44, its own key —
// translog.NewCosigner).
//
// Two ways in, one state: <dir>/checkpoint (the log's note it accepted and
// cosigned last), under one lock (<dir>/lock, flock — RP42-5):
//   - the pull pass (Pass; cron): fetch the newest checkpoint and verify its
//     signature; prove it extends the accepted one (a rewritten, forked or
//     shrunk history is refused); mirror, byte for byte, every full entry
//     bundle and full level-0 hash tile it does not hold yet and the partial
//     entry bundle at the checkpoint's size, each entry's leaf hash proven
//     against the checkpoint before it is kept; then cosign — unless that
//     very checkpoint is cosigned already;
//   - serve (Server; c2sp.org/tlog-witness add-checkpoint): the log pushes
//     each new checkpoint with a consistency proof from the size the
//     witness holds; it cosigns without mirroring.
//
// Either keeps the note with both signature lines (<dir>/cosigned) and one
// line per new note in the append-only <dir>/cosigned.history; the accepted
// checkpoint moves only after every step passed.
//
// Why: with the witness's cache, entries lost to a database restore can be
// put back byte for byte (cmd/tools/translog-restore, RP38-6), and a
// checkpoint the restored log must extend survives the loss of the box and
// its high-water file (RP38-5). Run by us it is our own second record on
// another machine; a third party running the same protocol (K4c-2's owner
// items) is the independent one.
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
	CosignFile  = "cosigned"         // that note with the witness's cosignature added
	HistoryFile = "cosigned.history" // every new cosigned note, base64, one per line
	LockFile    = "lock"             // flock: one pass or request at a time (RP42-5)
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

// Pass runs one pass (see the package comment) under the directory's lock
// (RP42-5: a cron pass, a -once=false loop and `serve` never interleave). On
// any error nothing it accepted moves: the state file is untouched (the
// cache may hold more verified bytes, never unverified ones). A checkpoint
// the witness already cosigned is not cosigned again: a cosignature is
// timestamped (c2sp.org/tlog-cosignature), so re-signing an unchanged tree
// would add a history line every run.
func (w *Witness) Pass(ctx context.Context) (Result, error) {
	res := Result{PrevSize: -1}
	if w.Client == nil || w.Client.Verifier == nil || w.Signer == nil || w.Dir == "" {
		return res, errors.New("witness: a directory, the log's verifier key and the witness's signer key are required")
	}
	v := w.Client.Verifier
	if w.Signer.Name() == v.Name() && w.Signer.KeyHash() == v.KeyHash() {
		return res, ErrSameKey
	}
	unlock, err := lockDir(ctx, w.Dir)
	if err != nil {
		return res, err
	}
	defer unlock()
	cache := OpenCache(w.Dir)

	cp, msg, err := w.Client.Checkpoint(ctx)
	if err != nil {
		return res, fmt.Errorf("witness: checkpoint: %w", err)
	}
	res.Size, res.Root = cp.Size, cp.Root

	// Consistency with what was accepted before, with a READ-ONLY view of
	// the cache: a forked tree's tiles are never kept.
	old, ok, err := readHeld(w.Dir, v)
	if err != nil {
		return res, err
	}
	if ok {
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
	cos, _, err := accept(w.Dir, msg, n, w.Signer, false)
	if err != nil {
		return res, err
	}
	res.Cosigned = cos
	return res, nil
}

// readHeld is the checkpoint the witness accepted (and cosigned) last,
// opened with the log's key; ok = false when there is none.
func readHeld(dir string, v note.Verifier) (translog.Checkpoint, bool, error) {
	prev, err := os.ReadFile(filepath.Join(dir, StateFile)) // #nosec G304 -- the witness's own directory
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return translog.Checkpoint{}, false, nil
	case err != nil:
		return translog.Checkpoint{}, false, fmt.Errorf("witness: read the accepted checkpoint: %w", err)
	}
	old, err := translog.OpenCheckpoint(prev, v)
	if err != nil {
		return translog.Checkpoint{}, false, fmt.Errorf("witness: the accepted checkpoint does not open with the log's key: %w", err)
	}
	return old, true, nil
}

// accept cosigns n (the log's verified note; msg as it came) and records it,
// under the directory's lock which the caller holds: the cosigned note
// (the log's signature and the witness's line) to <dir>/cosigned and, when
// it is new, one base64 line to <dir>/cosigned.history; then the state.
// refresh = false keeps an existing cosignature of the same checkpoint
// (signed = false); true always signs anew (an add-checkpoint request).
func accept(dir string, msg []byte, n *note.Note, s note.Signer, refresh bool) (cos []byte, signed bool, err error) {
	cosPath := filepath.Join(dir, CosignFile)
	before, err := os.ReadFile(cosPath) // #nosec G304 -- the witness's own directory
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, false, fmt.Errorf("witness: read %s: %w", CosignFile, err)
	}
	if !refresh && before != nil && noteText(before) == n.Text && len(translog.CosignatureLines(before, s.Name(), s.KeyHash())) > 0 {
		cos = before
	} else {
		cos, err = note.Sign(&note.Note{Text: n.Text, Sigs: n.Sigs}, s)
		if err != nil {
			return nil, false, fmt.Errorf("witness: co-sign: %w", err)
		}
		signed = true
	}
	if !bytes.Equal(before, cos) {
		if err := appendLine(filepath.Join(dir, HistoryFile), base64.StdEncoding.EncodeToString(cos)); err != nil {
			return nil, false, err
		}
		if err := writeAtomic(cosPath, cos); err != nil {
			return nil, false, fmt.Errorf("witness: write %s: %w", CosignFile, err)
		}
	}
	if err := writeAtomic(filepath.Join(dir, StateFile), msg); err != nil {
		return nil, false, fmt.Errorf("witness: write %s: %w", StateFile, err)
	}
	return cos, signed, nil
}

// noteText is a note's text: everything before its signature block.
func noteText(msg []byte) string {
	i := bytes.LastIndex(msg, []byte("\n\n"))
	if i < 0 {
		return ""
	}
	return string(msg[:i+1])
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
