package drivers

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// The segment reaper (RB-2): pieces of a large object no manifest
// references. A large upload writes its pieces under a generation,
// `t-<tenant>/_segments/<sha256(name)[:16]>/<gen>/`, then the manifest at
// the name. A crashed process, an upload whose cleanup failed, or a piece
// PUT that was cancelled after its body reached Blomp and landed after the
// failed upload's bulk delete leave pieces that nothing names.
//
// The generation's name does not lead back to its object (the digest is
// one-way), and a listing of the tenant's names cannot be trusted to find
// it either: Blomp's listing lags by minutes to 90+ min, so a manifest
// written an hour ago may not be listed yet. So every large upload also
// writes a marker in its generation, `<gen>/manifest`, whose body is the
// manifest's name (one small write beside the first piece). The reaper
// reads it with a GET — an object read is consistent, unlike the listing —
// checks that it names a key of the generation's own tenant and digest, and
// reads that key's manifest (`?multipart-manifest=get`):
//
//   - the manifest lists a piece of this generation: live, kept;
//   - 404, or an object that is not a manifest: an orphan — its listed
//     pieces are bulk-deleted; the marker goes on a later run, once the
//     generation lists nothing else (a piece the lagging listing did not
//     show yet keeps the marker, so it is found on a later run);
//   - anything else (a 5xx, a timeout): kept, the error noted.
//
// Only a generation whose NEWEST listed piece is older than the grace (6 h)
// is judged: an upload still running writes a piece every few seconds, and
// its manifest follows its last piece by seconds. A generation with no
// marker (written before markers, 2026-10-21) or a marker naming another
// key is never deleted (`unidentified`). Listing lag can only hide pieces
// from the reaper — the safe direction.

const (
	// swiftGenerationMarker is the leaf of a generation's marker; pieces
	// are named %08d, so it cannot be one.
	swiftGenerationMarker = "manifest"
	// SwiftDefaultSegmentGrace is how old a generation's newest piece must
	// be before the reaper judges it.
	SwiftDefaultSegmentGrace = 6 * time.Hour
	// swiftSegmentGCMaxPieces bounds the pieces one run deletes: at
	// Blomp's ~3 deletes a second inside a bulk delete, ~11 minutes.
	swiftSegmentGCMaxPieces = 2000
	swiftLastModifiedLayout = "2006-01-02T15:04:05.999999"
)

var (
	swiftSegmentGCGenerations = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "vaultaire_swift_segment_gc_generations_total",
		Help: "Large-object piece generations the Swift segment reaper judged, by backend and verdict (live, young, unidentified, reaped, error).",
	}, []string{"backend", "verdict"})
	swiftSegmentGCDeleted = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "vaultaire_swift_segment_gc_deleted_total",
		Help: "Objects the Swift segment reaper deleted, by backend and kind (piece, marker).",
	}, []string{"backend", "kind"})
)

func initSwiftSegmentGCSeries(backend string) {
	for _, v := range []string{"live", "young", "unidentified", "reaped", "error"} {
		swiftSegmentGCGenerations.WithLabelValues(backend, v)
	}
	for _, k := range []string{"piece", "marker"} {
		swiftSegmentGCDeleted.WithLabelValues(backend, k)
	}
}

// swiftSegmentPrefix is the generation folder of a large object's pieces.
func swiftSegmentPrefix(tenantID, name, gen string) string {
	digest := sha256.Sum256([]byte(name))
	return fmt.Sprintf("t-%s/_segments/%s/%s/", tenantID, hex.EncodeToString(digest[:16]), gen)
}

// generationMarkerOf is the marker beside a piece name.
func generationMarkerOf(piece string) (string, bool) {
	i := strings.LastIndex(piece, "/")
	if i < 0 || !strings.Contains(piece, "/_segments/") {
		return "", false
	}
	return piece[:i+1] + swiftGenerationMarker, true
}

// putMarker writes a generation's marker: the manifest's name.
func (d *SwiftDriver) putMarker(ctx context.Context, marker, manifest string) error {
	return d.putOne(ctx, marker, "text/plain", swiftRequest{bodyBytes: []byte(manifest)})
}

// SwiftSegmentReapResult is what ReapOrphanSegments did (job_runs.result).
type SwiftSegmentReapResult struct {
	Backend        string   `json:"backend"`
	Tenants        int      `json:"tenants_scanned"`
	Generations    int      `json:"generations_scanned"`
	Young          int      `json:"generations_young"`
	Live           int      `json:"generations_live"`
	Unidentified   int      `json:"generations_unidentified"`
	Reaped         int      `json:"generations_reaped"`
	PiecesDeleted  int      `json:"pieces_deleted"`
	MarkersDeleted int      `json:"markers_deleted"`
	BudgetStopped  bool     `json:"budget_stopped,omitempty"`
	Errors         []string `json:"errors,omitempty"`
}

func (r *SwiftSegmentReapResult) fail(format string, args ...any) {
	if len(r.Errors) < 50 {
		r.Errors = append(r.Errors, fmt.Sprintf(format, args...))
	}
}

// errSegmentBudget stops a run whose piece budget is spent.
var errSegmentBudget = errors.New("segment reaper: piece budget spent")

// swiftEntry is one row of a container listing: an object or, with a
// delimiter, a subdir.
type swiftEntry struct {
	Name         string `json:"name"`
	Subdir       string `json:"subdir"`
	Bytes        int64  `json:"bytes"`
	LastModified string `json:"last_modified"`
}

// swiftGeneration is one generation as listed.
type swiftGeneration struct {
	prefix string // t-<tenant>/_segments/<digest>/<gen>/
	pieces []string
	marker bool
	newest time.Time
}

// ReapOrphanSegments deletes the pieces of every generation no manifest
// references and whose newest piece is older than olderThan (see the top
// of this file). An error only when the tenants or a tenant's segments
// cannot be listed; a generation that cannot be judged or deleted is an
// entry of Errors.
func (d *SwiftDriver) ReapOrphanSegments(ctx context.Context, olderThan time.Duration) (SwiftSegmentReapResult, error) {
	res := SwiftSegmentReapResult{Backend: d.name}
	initSwiftSegmentGCSeries(d.name)
	var tenants []string
	seen := map[string]bool{}
	if err := d.listEntries(ctx, "t-", "/", swiftWalkPageTimeout, func(e swiftEntry) error {
		if e.Subdir != "" && !seen[e.Subdir] {
			seen[e.Subdir] = true
			tenants = append(tenants, e.Subdir)
		}
		return nil
	}); err != nil {
		return res, err
	}
	budget := d.segmentGCMaxPieces
	cutoff := d.now().Add(-olderThan)
	for _, tdir := range tenants {
		res.Tenants++
		var cur *swiftGeneration
		judge := func() error {
			if cur == nil {
				return nil
			}
			g := cur
			cur = nil
			return d.judgeGeneration(ctx, tdir, g, cutoff, &budget, &res)
		}
		err := d.listEntries(ctx, tdir+"_segments/", "", swiftWalkPageTimeout, func(e swiftEntry) error {
			rest := strings.TrimPrefix(e.Name, tdir+"_segments/")
			parts := strings.Split(rest, "/")
			if len(parts) != 3 {
				return nil // not a piece of ours
			}
			prefix := tdir + "_segments/" + parts[0] + "/" + parts[1] + "/"
			if cur != nil && cur.prefix != prefix {
				if err := judge(); err != nil {
					return err
				}
			}
			if cur == nil {
				cur = &swiftGeneration{prefix: prefix}
			}
			if parts[2] == swiftGenerationMarker {
				cur.marker = true
			} else {
				cur.pieces = append(cur.pieces, e.Name)
			}
			if t, err := time.Parse(swiftLastModifiedLayout, e.LastModified); err == nil && t.After(cur.newest) {
				cur.newest = t
			} else if err != nil {
				cur.newest = d.now() // unknown age: never judged
			}
			return nil
		})
		if err == nil {
			err = judge()
		}
		if errors.Is(err, errSegmentBudget) {
			res.BudgetStopped = true
			return res, nil
		}
		if err != nil {
			return res, err
		}
	}
	return res, nil
}

// judgeGeneration decides one generation and deletes it when it is an
// orphan; errSegmentBudget when the run's budget cannot take it.
func (d *SwiftDriver) judgeGeneration(ctx context.Context, tdir string, g *swiftGeneration, cutoff time.Time, budget *int, res *SwiftSegmentReapResult) error {
	res.Generations++
	verdict := func(v string) { swiftSegmentGCGenerations.WithLabelValues(d.name, v).Inc() }
	if !g.newest.Before(cutoff) {
		res.Young++
		verdict("young")
		return nil
	}
	if !g.marker {
		res.Unidentified++
		verdict("unidentified")
		return nil
	}
	manifest, err := d.readMarker(ctx, g.prefix+swiftGenerationMarker)
	if err != nil {
		res.fail("%s: marker: %v", g.prefix, err)
		verdict("error")
		return nil
	}
	// The marker must name a key of this tenant whose digest is this
	// generation's: anything else is not ours to judge.
	digest := strings.Split(strings.TrimPrefix(g.prefix, tdir+"_segments/"), "/")[0]
	sum := sha256.Sum256([]byte(manifest))
	if !strings.HasPrefix(manifest, tdir) || strings.Contains(manifest, "/_segments/") || hex.EncodeToString(sum[:16]) != digest {
		res.Unidentified++
		verdict("unidentified")
		return nil
	}
	live, err := d.manifestReferences(ctx, manifest, g.prefix)
	if err != nil {
		res.fail("%s: manifest %s: %v", g.prefix, manifest, err)
		verdict("error")
		return nil
	}
	if live {
		res.Live++
		verdict("live")
		return nil
	}
	// An orphan: its pieces now, its marker once nothing else is listed.
	names, kind := g.pieces, "piece"
	if len(names) == 0 {
		names, kind = []string{g.prefix + swiftGenerationMarker}, "marker"
	}
	if len(names) > *budget {
		return errSegmentBudget
	}
	*budget -= len(names)
	if _, err := d.bulkDelete(ctx, names); err != nil {
		res.fail("%s: delete %d %ss: %v", g.prefix, len(names), kind, err)
		verdict("error")
		return nil
	}
	res.Reaped++
	verdict("reaped")
	swiftSegmentGCDeleted.WithLabelValues(d.name, kind).Add(float64(len(names)))
	if kind == "piece" {
		res.PiecesDeleted += len(names)
	} else {
		res.MarkersDeleted++
	}
	return nil
}

// readMarker reads a generation's marker: the manifest's name.
func (d *SwiftDriver) readMarker(ctx context.Context, marker string) (string, error) {
	status, _, body, err := d.meta(ctx, swiftRequest{method: http.MethodGet, name: marker})
	if err != nil {
		return "", err
	}
	if status != http.StatusOK {
		return "", &SwiftError{Op: "GET (marker) " + marker, Status: status}
	}
	return string(body), nil
}

// manifestReferences reports whether the object at name is a large object
// one of whose pieces is under prefix. A missing object, or one that is not
// a manifest, references nothing; any other answer is an error (kept).
func (d *SwiftDriver) manifestReferences(ctx context.Context, name, prefix string) (bool, error) {
	status, hdr, body, err := d.meta(ctx, swiftRequest{method: http.MethodGet, name: name,
		query: url.Values{"multipart-manifest": {"get"}}})
	if err != nil {
		return false, err
	}
	switch status {
	case http.StatusNotFound:
		return false, nil
	case http.StatusOK:
	default:
		return false, &SwiftError{Op: "GET (manifest) " + name, Status: status}
	}
	var entries []struct {
		Name string `json:"name"`
		Path string `json:"path"`
	}
	if err := json.Unmarshal(body, &entries); err != nil {
		if strings.EqualFold(hdr.Get("X-Static-Large-Object"), "true") {
			return false, fmt.Errorf("manifest: %w", err)
		}
		return false, nil // a plain object at the name
	}
	inContainer := "/" + d.cfg.Container + "/"
	for _, e := range entries {
		p := e.Name
		if p == "" {
			p = e.Path
		}
		if u, err := url.PathUnescape(p); err == nil {
			p = u
		}
		if strings.HasPrefix(strings.TrimPrefix(p, inContainer), prefix) {
			return true, nil
		}
	}
	return false, nil
}

// listEntries hands every listing row under prefix (with delimiter: names
// and subdirs) to fn, page by page.
func (d *SwiftDriver) listEntries(ctx context.Context, prefix, delimiter string, pageTimeout time.Duration, fn func(swiftEntry) error) error {
	marker := ""
	for {
		q := url.Values{"format": {"json"}, "prefix": {prefix}, "limit": {strconv.Itoa(d.listPageSize)}}
		if delimiter != "" {
			q.Set("delimiter", delimiter)
		}
		if marker != "" {
			q.Set("marker", marker)
		}
		pctx, cancel := context.WithTimeout(ctx, pageTimeout)
		status, _, body, err := d.metaWithin(pctx, swiftRequest{method: http.MethodGet, query: q})
		cancel()
		if err != nil {
			return fmt.Errorf("%s list %s: %w", d.name, prefix, err)
		}
		if status == http.StatusNoContent {
			return nil
		}
		if status != http.StatusOK {
			return &SwiftError{Op: "list " + prefix, Status: status, Body: truncate(string(body), 200)}
		}
		var page []swiftEntry
		if err := json.Unmarshal(body, &page); err != nil {
			return fmt.Errorf("%s list %s: decode: %w", d.name, prefix, err)
		}
		for _, e := range page {
			key := e.Name
			if e.Subdir != "" {
				key = e.Subdir
			}
			if !strings.HasPrefix(key, prefix) {
				return fmt.Errorf("%s list %s: the backend returned %q, outside the prefix", d.name, prefix, key)
			}
			if err := fn(e); err != nil {
				return err
			}
		}
		if len(page) < d.listPageSize {
			return nil
		}
		last := page[len(page)-1]
		marker = last.Name
		if last.Subdir != "" {
			marker = last.Subdir
		}
	}
}
