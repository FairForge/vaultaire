package main

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"slices"
	"sort"
	"strings"
	"sync"

	"github.com/FairForge/vaultaire/internal/drivers"
)

// entry is one item of a walked tree, by its path relative to the root.
type entry struct {
	id     string
	folder bool
	size   int64
	qxh    string // Graph's quickXorHash ("" = not computed yet)
}

// tree is a walked folder: every item under it by relative path.
type tree struct {
	driveID string
	rootID  string
	items   map[string]entry
}

func (t *tree) totals() (files, folders int, bytes int64) {
	for _, e := range t.items {
		if e.folder {
			folders++
			continue
		}
		files++
		bytes += e.size
	}
	return
}

// walk lists every item under the folder rootID on the drive, maxInFlight
// folders at once (the client bounds the requests too).
func walk(ctx context.Context, g *graph, driveID, rootID string) (*tree, error) {
	t := &tree{driveID: driveID, rootID: rootID, items: map[string]entry{}}
	var (
		mu       sync.Mutex
		wg       sync.WaitGroup
		firstErr error
		sem      = make(chan struct{}, maxInFlight)
	)
	var visit func(id, rel string)
	visit = func(id, rel string) {
		defer wg.Done()
		sem <- struct{}{}
		kids, err := g.children(ctx, driveID, id)
		<-sem
		mu.Lock()
		defer mu.Unlock()
		if err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("list %q: %w", rel, err)
			}
			return
		}
		for _, k := range kids {
			p := k.Name
			if rel != "" {
				p = rel + "/" + k.Name
			}
			e := entry{id: k.ID, folder: k.Folder != nil, size: k.Size}
			if k.File != nil {
				e.qxh = k.File.Hashes.QuickXorHash
			}
			t.items[p] = e
			if e.folder {
				wg.Add(1)
				go visit(k.ID, p)
			}
		}
	}
	wg.Add(1)
	go visit(rootID, "")
	wg.Wait()
	return t, firstErr
}

// diff is one difference between two trees.
type diff struct {
	path string
	kind string // missing | extra | type | size | hash
	note string
}

func (d diff) String() string {
	s := d.kind + " " + d.path
	if d.note != "" {
		s += " (" + d.note + ")"
	}
	return s
}

// compareMode says what the destination may hold beyond the source.
type compareMode int

const (
	// exact: the destination is the source — the copy, the switch.
	exact compareMode = iota
	// superset: every source item is on the destination as it is; the
	// destination may hold more (writes since the switch) — the retire.
	superset
)

// skipContent: the fleet marker exists on both sides but its content
// differs once -switch has run (drive_id, moved_to).
func skipContent(p string) bool { return p == drivers.FleetMarkerPath }

// hasher computes an item's quickXorHash from its bytes — where Graph has
// not computed one yet.
type hasher func(ctx context.Context, driveID, itemID string) (string, error)

func graphHasher(g *graph) hasher {
	return func(ctx context.Context, driveID, itemID string) (string, error) {
		rc, err := g.content(ctx, driveID, itemID)
		if err != nil {
			return "", err
		}
		defer func() { _ = rc.Close() }()
		h := drivers.NewQuickXorHash()
		if _, err := io.Copy(h, rc); err != nil {
			return "", fmt.Errorf("read: %w", err)
		}
		return base64.StdEncoding.EncodeToString(h.Sum(nil)), nil
	}
}

// compare is every difference between src and dst: the item count, the
// bytes and the quickXorHash of every file (Graph's on both sides; a side
// Graph has not hashed yet is hashed from its bytes). Sorted by path.
func compare(ctx context.Context, src, dst *tree, mode compareMode, hash hasher) ([]diff, error) {
	var out []diff
	paths := make([]string, 0, len(src.items))
	for p := range src.items {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, p := range paths {
		s := src.items[p]
		d, ok := dst.items[p]
		switch {
		case !ok:
			out = append(out, diff{path: p, kind: "missing"})
			continue
		case s.folder != d.folder:
			out = append(out, diff{path: p, kind: "type", note: fmt.Sprintf("folder %v vs %v", s.folder, d.folder)})
			continue
		case s.folder || skipContent(p):
			continue
		case s.size != d.size:
			out = append(out, diff{path: p, kind: "size", note: fmt.Sprintf("%d vs %d", s.size, d.size)})
			continue
		}
		sh, dh := s.qxh, d.qxh
		var err error
		if sh == "" {
			if sh, err = hash(ctx, src.driveID, s.id); err != nil {
				return nil, fmt.Errorf("hash source %s: %w", p, err)
			}
		}
		if dh == "" {
			if dh, err = hash(ctx, dst.driveID, d.id); err != nil {
				return nil, fmt.Errorf("hash destination %s: %w", p, err)
			}
		}
		if sh != dh {
			out = append(out, diff{path: p, kind: "hash", note: sh + " vs " + dh})
		}
	}
	if mode == exact {
		var extra []string
		for p := range dst.items {
			if _, ok := src.items[p]; !ok {
				extra = append(extra, p)
			}
		}
		sort.Strings(extra)
		for _, p := range extra {
			out = append(out, diff{path: p, kind: "extra"})
		}
	}
	return out, nil
}

// onlyMissing reports whether every difference is a source item absent from
// the destination — what a resumed copy can fill.
func onlyMissing(ds []diff) bool {
	return !slices.ContainsFunc(ds, func(d diff) bool { return d.kind != "missing" })
}

// topMissing is the missing paths whose parent is NOT missing: copying each
// of those (a folder copies whole) fills every missing path.
func topMissing(ds []diff) []string {
	missing := map[string]bool{}
	for _, d := range ds {
		if d.kind == "missing" {
			missing[d.path] = true
		}
	}
	var out []string
	for p := range missing {
		top := true
		for parent := parentOf(p); parent != ""; parent = parentOf(parent) {
			if missing[parent] {
				top = false
				break
			}
		}
		if top {
			out = append(out, p)
		}
	}
	sort.Strings(out)
	return out
}

func parentOf(p string) string {
	i := strings.LastIndex(p, "/")
	if i < 0 {
		return ""
	}
	return p[:i]
}

func baseOf(p string) string { return p[strings.LastIndex(p, "/")+1:] }
