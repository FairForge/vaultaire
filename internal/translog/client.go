package translog

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"golang.org/x/mod/sumdb/note"
	"golang.org/x/mod/sumdb/tlog"
)

// Client reads a log served in the layout of tiles.go and checks it: the
// checkpoint against a pinned verifier key, an entry's inclusion, and the
// consistency of a newer checkpoint with one saved earlier — the witness's
// job until an external witness exists (K4b). Every hash it uses comes from
// tlog.TileHashReader, which authenticates each tile against the
// checkpoint's root before handing a hash out; proofs are then verified by
// transparency-dev/merkle (VerifyInclusion / VerifyConsistency).
type Client struct {
	// BaseURL is where "checkpoint" and "tile/…" live, ending in "/".
	BaseURL  string
	HTTP     *http.Client
	Verifier note.Verifier
}

const maxFetch = 32 << 20 // a full entry bundle is at most 256 × 64 KiB

func (c *Client) get(ctx context.Context, path string) ([]byte, error) {
	hc := c.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Second}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimSuffix(c.BaseURL, "/")+"/"+path, nil)
	if err != nil {
		return nil, err
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("GET %s: %w", path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: %s", path, resp.Status)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxFetch+1))
	if err != nil {
		return nil, fmt.Errorf("GET %s: %w", path, err)
	}
	if len(b) > maxFetch {
		return nil, fmt.Errorf("GET %s: larger than %d bytes", path, maxFetch)
	}
	return b, nil
}

// Checkpoint fetches the newest checkpoint and verifies its signature.
func (c *Client) Checkpoint(ctx context.Context) (Checkpoint, []byte, error) {
	msg, err := c.get(ctx, "checkpoint")
	if err != nil {
		return Checkpoint{}, nil, err
	}
	cp, err := OpenCheckpoint(msg, c.Verifier)
	if err != nil {
		return Checkpoint{}, nil, err
	}
	return cp, msg, nil
}

// tileReader fetches Merkle tiles for tlog.TileHashReader.
type tileReader struct {
	ctx context.Context
	c   *Client
}

func (r *tileReader) Height() int { return TileHeight }

func (r *tileReader) ReadTiles(tiles []tlog.Tile) ([][]byte, error) {
	out := make([][]byte, len(tiles))
	for i, t := range tiles {
		b, err := r.c.get(r.ctx, TilePath(t.L, t.N, t.W))
		if err != nil {
			return nil, err
		}
		out[i] = b
	}
	return out, nil
}

func (r *tileReader) SaveTiles([]tlog.Tile, [][]byte) {}

// hashes reads hashes authenticated against cp.
func (c *Client) hashes(ctx context.Context, cp Checkpoint) HashReader {
	return tlog.TileHashReader(tlog.Tree{N: cp.Size, Hash: cp.Root}, &tileReader{ctx: ctx, c: c})
}

// VerifyInclusion fetches entry idx and proves it is in cp's tree; it
// returns the entry's body.
func (c *Client) VerifyInclusion(ctx context.Context, cp Checkpoint, idx int64) ([]byte, error) {
	if idx < 0 || idx >= cp.Size {
		return nil, fmt.Errorf("entry %d is not in a tree of %d", idx, cp.Size)
	}
	n := idx / TileWidth
	bundle, err := c.get(ctx, TilePath(EntriesLevel, n, TileWidthAt(EntriesLevel, n, cp.Size)))
	if err != nil {
		return nil, err
	}
	bodies, err := ParseEntryBundle(bundle)
	if err != nil {
		return nil, err
	}
	i := idx % TileWidth
	if int64(len(bodies)) <= i {
		return nil, fmt.Errorf("entry bundle %d has %d entries", n, len(bodies))
	}
	body := bodies[i]
	p, err := InclusionProof(idx, cp.Size, c.hashes(ctx, cp))
	if err != nil {
		return nil, err
	}
	if err := VerifyInclusion(p, idx, cp.Size, LeafHash(body), cp.Root); err != nil {
		return nil, err
	}
	return body, nil
}

// VerifyConsistency proves cp extends old (saved earlier): nil means no
// entry of old's tree was rewritten or removed.
func (c *Client) VerifyConsistency(ctx context.Context, old, cp Checkpoint) error {
	if old.Size > cp.Size {
		return fmt.Errorf("the log shrank: %d entries, a checkpoint of %d was seen before", cp.Size, old.Size)
	}
	if old.Size == cp.Size {
		if old.Root != cp.Root {
			return errors.New("two different roots for the same size: a fork")
		}
		return nil
	}
	p, err := ConsistencyProof(old.Size, cp.Size, c.hashes(ctx, cp))
	if err != nil {
		return err
	}
	return VerifyConsistency(p, old.Size, cp.Size, old.Root, cp.Root)
}
