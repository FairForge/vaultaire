package translog

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"golang.org/x/mod/sumdb/note"
)

// Source is what the read side serves: the newest signed checkpoint, the
// stored hashes, the entry bodies. internal/translog/logstore.Store is the
// database one; MemorySource the in-process one.
type Source interface {
	// CheckpointNote is the newest signed checkpoint (a signed note) and
	// its tree size; ok = false when none was ever signed.
	CheckpointNote(ctx context.Context) (note []byte, size int64, ok bool, err error)
	HashReader(ctx context.Context) HashReader
	Entries(ctx context.Context, from, n int64) ([][]byte, error)
}

// AnchorSource is a Source that also serves its checkpoints' anchors
// (K4c-3a, docs/TRANSPARENCY.md § Anchors): "anchor/<size>.ots".
type AnchorSource interface {
	// AnchorOTS is the OpenTimestamps proof of the checkpoint of size;
	// ErrNoAnchor when there is none.
	AnchorOTS(ctx context.Context, size int64) ([]byte, error)
}

// ErrNoAnchor: the checkpoint has no anchor (or was never signed).
var ErrNoAnchor = errors.New("translog: no anchor of that checkpoint")

// OTSContentType is the .ots file's media type (the calendars' own Accept).
const OTSContentType = "application/vnd.opentimestamps.v1"

// The cache rule (c2sp.org/tlog-tiles): a full tile never changes; a
// partial tile and the checkpoint do.
const (
	cacheImmutable = "public, max-age=31536000, immutable"
	cacheNone      = "no-store"
)

// NewHandler serves the layout of tiles.go — "checkpoint", "tile/<L>/<N>[.p/<W>]",
// "tile/entries/<N>[.p/<W>]" — and, when src is an AnchorSource,
// "anchor/<size>.ots" (no-store: a pending proof is completed later) for a
// size up to the newest checkpoint's, relative to where it is mounted (strip the
// prefix first). Only what the newest checkpoint commits to is served: a
// tile wider than its width at the checkpoint's size is 404 (a narrower
// partial tile — an older checkpoint's — is served). No checkpoint = 404
// everywhere: without a signature there is nothing a reader could check.
func NewHandler(src Source) http.Handler {
	return &handler{src: src}
}

type handler struct{ src Source }

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	ctx := r.Context()
	p := strings.TrimPrefix(r.URL.Path, "/")
	if rest, ok := strings.CutPrefix(p, "anchor/"); ok {
		h.anchor(w, r, rest)
		return
	}
	var (
		level, width int
		n            int64
	)
	if p != "checkpoint" {
		var err error
		if level, n, width, err = ParseTilePath(p); err != nil {
			notFound(w, r)
			return
		}
	}
	msg, size, ok, err := h.src.CheckpointNote(ctx)
	if err != nil {
		w.Header().Set("Cache-Control", cacheNone)
		http.Error(w, "the log cannot be read right now", http.StatusServiceUnavailable)
		return
	}
	if !ok {
		notFound(w, r)
		return
	}
	if p == "checkpoint" {
		write(w, "text/plain; charset=utf-8", cacheNone, msg)
		return
	}
	if width > TileWidthAt(level, n, size) {
		notFound(w, r)
		return
	}
	var data []byte
	if level == EntriesLevel {
		bodies, err := h.src.Entries(ctx, n*TileWidth, int64(width))
		if err == nil && len(bodies) != width {
			err = errShortRead
		}
		if err == nil {
			data, err = EntryBundle(bodies)
		}
		if err != nil {
			w.Header().Set("Cache-Control", cacheNone)
			http.Error(w, "the log cannot be read right now", http.StatusServiceUnavailable)
			return
		}
	} else {
		data, err = TileData(level, n, width, h.src.HashReader(ctx))
		if err != nil {
			w.Header().Set("Cache-Control", cacheNone)
			http.Error(w, "the log cannot be read right now", http.StatusServiceUnavailable)
			return
		}
	}
	cache := cacheNone
	if width == TileWidth {
		cache = cacheImmutable
	}
	write(w, "application/octet-stream", cache, data)
}

// anchor serves "anchor/<size>.ots".
func (h *handler) anchor(w http.ResponseWriter, r *http.Request, name string) {
	as, ok := h.src.(AnchorSource)
	digits, isOTS := strings.CutSuffix(name, ".ots")
	size, err := strconv.ParseInt(digits, 10, 64)
	if !ok || !isOTS || err != nil || size <= 0 || strconv.FormatInt(size, 10) != digits {
		notFound(w, r)
		return
	}
	_, newest, signed, err := h.src.CheckpointNote(r.Context())
	if err != nil {
		w.Header().Set("Cache-Control", cacheNone)
		http.Error(w, "the log cannot be read right now", http.StatusServiceUnavailable)
		return
	}
	if !signed || size > newest {
		notFound(w, r)
		return
	}
	b, err := as.AnchorOTS(r.Context(), size)
	switch {
	case errors.Is(err, ErrNoAnchor):
		notFound(w, r)
		return
	case err != nil:
		w.Header().Set("Cache-Control", cacheNone)
		http.Error(w, "the log cannot be read right now", http.StatusServiceUnavailable)
		return
	}
	write(w, OTSContentType, cacheNone, b)
}

// notFound is the read side's 404, never cached: a tile beyond the newest
// checkpoint, or the checkpoint before the first signature, exists later
// (RP42-2: Cloudflare fronts the trust host).
func notFound(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", cacheNone)
	http.NotFound(w, r)
}

type readErr string

func (e readErr) Error() string { return string(e) }

const errShortRead = readErr("translog: fewer entries than the tile width")

func write(w http.ResponseWriter, contentType, cache string, b []byte) {
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Cache-Control", cache)
	w.Header().Set("Content-Length", strconv.Itoa(len(b)))
	w.Header().Set("X-Content-Type-Options", "nosniff")
	_, _ = w.Write(b) // #nosec G705 -- a signed note (text/plain), tile bytes (octet-stream) or an .ots file under nosniff, never HTML
}

// MemorySource serves a Memory log, signing a checkpoint of its current
// size on every read (tests and tools; nil Signer = never signed).
type MemorySource struct {
	Log    *Memory
	Signer note.Signer
}

// CheckpointNote implements Source.
func (s *MemorySource) CheckpointNote(context.Context) ([]byte, int64, bool, error) {
	if s.Signer == nil || s.Log.Size() == 0 {
		return nil, 0, false, nil
	}
	root, err := Root(s.Log.Size(), s.Log)
	if err != nil {
		return nil, 0, false, err
	}
	msg, err := SignCheckpoint(s.Signer, s.Log.Size(), root)
	return msg, s.Log.Size(), err == nil, err
}

// HashReader implements Source.
func (s *MemorySource) HashReader(context.Context) HashReader { return s.Log }

// Entries implements Source.
func (s *MemorySource) Entries(_ context.Context, from, n int64) ([][]byte, error) {
	var out [][]byte
	for i := from; i < from+n && i < s.Log.Size(); i++ {
		out = append(out, s.Log.Entry(i))
	}
	return out, nil
}
