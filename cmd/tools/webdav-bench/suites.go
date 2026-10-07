package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"math/rand/v2"
	"strings"
	"time"

	"github.com/FairForge/vaultaire/internal/engine"
)

// --- shared object operations (through the real driver) -------------------

// putGen streams a generated object through the driver and returns the
// sha256 of what was sent.
func (b *bench) putGen(key string, seed uint64, size int64) ([]byte, error) {
	g := newGen(seed, size).hashing()
	ctx, cancel := b.opCtx()
	defer cancel()
	if err := b.drv.Put(ctx, b.container, key, g, engine.WithContentLength(size)); err != nil {
		return nil, err
	}
	return g.Sum(), nil
}

// getVerify reads key through the driver and compares it to want (size and
// sha256); a difference is a MISMATCH. ttfb is the time to the first byte.
func (b *bench) getVerify(key string, want []byte, size int64) (ttfb time.Duration, err error) {
	start := time.Now()
	ctx, cancel := b.opCtx()
	defer cancel()
	rc, err := b.drv.Get(ctx, b.container, key)
	if err != nil {
		return 0, err
	}
	defer func() { _ = rc.Close() }()
	fr := &firstByteReader{r: rc, start: start}
	n, ok, err := verifyBody(fr, want)
	if err != nil {
		return fr.ttfb, fmt.Errorf("read %s: %w", key, err)
	}
	if n != size || !ok {
		b.mismatch("GET %s: %d bytes (want %d), sha256 match=%v", key, n, size, ok)
	}
	return fr.ttfb, nil
}

type firstByteReader struct {
	r     io.Reader
	start time.Time
	ttfb  time.Duration
}

func (f *firstByteReader) Read(p []byte) (int, error) {
	n, err := f.r.Read(p)
	if n > 0 && f.ttfb == 0 {
		f.ttfb = time.Since(f.start)
	}
	return n, err
}

func isNotFound(err error) bool {
	var nf engine.NotFoundError
	return errors.As(err, &nf)
}

// dropFolder removes a case's folder (raw recursive DELETE) to keep the
// server's (and a bridge's spill directory's) footprint to one case.
func (b *bench) dropFolder(key string) {
	if err := b.raw.remove(b.ctx, b.runSegs(splitPath(key)...), true); err != nil {
		b.fail("drop %s: %v", key, err)
	}
}

// --- small ----------------------------------------------------------------

func (b *bench) suiteSmall() {
	n := b.cfg.SmallN
	for _, size := range b.cfg.SmallSizes {
		for _, conc := range b.cfg.SmallConc {
			cs := fmt.Sprintf("%s c%d", sizeName(size), conc)
			folder := fmt.Sprintf("small/%s-c%d", sizeName(size), conc)
			key := func(i int) string { return fmt.Sprintf("%s/o%06d", folder, i) }
			hashes := make([][]byte, n)

			lat, errs, wall := b.pool(n, conc, "small PUT "+cs, func(i int) error {
				h, err := b.putGen(key(i), seedFor(b.cfg.Seed, key(i)), size)
				hashes[i] = h
				return err
			})
			b.addRow(row("small", cs, "PUT", lat, errs, wall, int64(n-errs)*size))

			lat, errs, wall = b.pool(n, conc, "small GET "+cs, func(i int) error {
				if hashes[i] == nil {
					return errors.New("not written")
				}
				_, err := b.getVerify(key(i), hashes[i], size)
				return err
			})
			b.addRow(row("small", cs, "GET", lat, errs, wall, int64(n-errs)*size))

			lat, errs, wall = b.pool(n, conc, "small Exists "+cs, func(i int) error {
				ctx, cancel := b.opCtx()
				defer cancel()
				ok, err := b.drv.Exists(ctx, b.container, key(i))
				if err == nil && !ok && hashes[i] != nil {
					return fmt.Errorf("Exists %s = false after a successful PUT", key(i))
				}
				return err
			})
			b.addRow(row("small", cs, "Exists", lat, errs, wall, 0))

			lat, errs, wall = b.pool(n, conc, "small Delete "+cs, func(i int) error {
				ctx, cancel := b.opCtx()
				defer cancel()
				return b.drv.Delete(ctx, b.container, key(i))
			})
			b.addRow(row("small", cs, "Delete", lat, errs, wall, 0))
			b.dropFolder(folder)
		}
	}
}

// --- large ----------------------------------------------------------------

func (b *bench) suiteLarge() {
	for _, size := range b.cfg.LargeSizes {
		for _, conc := range b.cfg.LargeConc {
			if conc < 1 {
				conc = 1
			}
			cs := fmt.Sprintf("%s c%d", sizeName(size), conc)
			folder := fmt.Sprintf("large/%s-c%d", sizeName(size), conc)
			key := func(i int) string { return fmt.Sprintf("%s/o%d", folder, i) }
			hashes := make([][]byte, conc)

			lat, errs, wall := b.pool(conc, conc, "large PUT "+cs, func(i int) error {
				h, err := b.putGen(key(i), seedFor(b.cfg.Seed, key(i)), size)
				hashes[i] = h
				return err
			})
			b.addRow(row("large", cs, "PUT", lat, errs, wall, int64(conc-errs)*size))

			ttfb := make([]time.Duration, conc)
			lat, errs, wall = b.pool(conc, conc, "large GET "+cs, func(i int) error {
				if hashes[i] == nil {
					return errors.New("not written")
				}
				t, err := b.getVerify(key(i), hashes[i], size)
				ttfb[i] = t
				return err
			})
			b.addRow(row("large", cs, "GET", lat, errs, wall, int64(conc-errs)*size))
			r := row("large", cs, "GET TTFB", ttfb, 0, wall, 0)
			r.OpsSec, r.WallSec = 0, 0
			r.Note = "time to first byte"
			b.addRow(r)
			b.dropFolder(folder)
		}
	}
}

// --- range ----------------------------------------------------------------

func (b *bench) suiteRange() {
	size := b.cfg.RangeObject
	key := "range/obj"
	seed := seedFor(b.cfg.Seed, key)
	t := time.Now()
	if _, err := b.putGen(key, seed, size); err != nil {
		b.fail("range PUT %s: %v", sizeName(size), err)
		return
	}
	wall := time.Since(t)
	b.addRow(row("range", sizeName(size)+" object", "PUT", []time.Duration{wall}, 0, wall, size))

	for _, rs := range b.cfg.RangeSizes {
		if rs <= 0 || rs > size {
			b.fail("range %s does not fit a %s object", sizeName(rs), sizeName(size))
			continue
		}
		rng := rand.New(rand.NewPCG(uint64(b.cfg.Seed), uint64(rs)))
		offs := make([]int64, b.cfg.RangeCount)
		for i := range offs {
			offs[i] = rng.Int64N(size - rs + 1)
		}
		cs := fmt.Sprintf("%s in %s", sizeName(rs), sizeName(size))
		lat, errs, wall := b.pool(len(offs), 1, "range GET "+cs, func(i int) error {
			ctx, cancel := b.opCtx()
			defer cancel()
			rc, err := b.drv.GetRange(ctx, b.container, key, offs[i], rs)
			if err != nil {
				return err
			}
			got, err := io.ReadAll(rc)
			_ = rc.Close()
			if err != nil {
				return fmt.Errorf("read range at %d: %w", offs[i], err)
			}
			if want := expectedRange(seed, size, offs[i], rs); !bytes.Equal(got, want) {
				b.mismatch("GET range %s bytes=%d-%d: %d bytes, not the bytes written", key, offs[i], offs[i]+rs-1, len(got))
			}
			return nil
		})
		b.addRow(row("range", cs, "GET range", lat, errs, wall, int64(len(offs)-errs)*rs))
	}
	b.dropFolder("range")
}

// --- consistency ----------------------------------------------------------

// ConsistencyResult counts stale reads; each one is an Event with how long
// the server took to show the expected state (polled every 50 ms, ≤ 30 s).
type ConsistencyResult struct {
	Rounds         int          `json:"rounds"`
	StaleRAW       int          `json:"stale_read_after_write"`
	StaleOverwrite int          `json:"stale_overwrite"`
	StaleDelete    int          `json:"visible_after_delete"`
	Events         []StaleEvent `json:"events"`
}

// StaleEvent is one read that did not show the write just acknowledged.
type StaleEvent struct {
	Kind            string  `json:"kind"`
	Key             string  `json:"key"`
	Saw             string  `json:"saw"`
	Resolved        bool    `json:"resolved"`
	ResolvedAfterMs float64 `json:"resolved_after_ms"`
}

const consistencySize = 64 << 10

func (b *bench) suiteConsistency() {
	res := &ConsistencyResult{Rounds: b.cfg.ConsistencyN}
	var putLat, rawLat, owLat, rowLat, delLat, rdelLat []time.Duration
	read := func(key string, h1, h2 []byte) (string, time.Duration, error) {
		t := time.Now()
		s, err := b.readState(key, h1, h2)
		return s, time.Since(t), err
	}
	stale := func(kind, key, saw, want string, h1, h2 []byte) {
		ev := StaleEvent{Kind: kind, Key: key, Saw: saw}
		start := time.Now()
		for time.Since(start) < 30*time.Second && b.ctx.Err() == nil {
			time.Sleep(50 * time.Millisecond)
			s, err := b.readState(key, h1, h2)
			if err == nil && s == want {
				ev.Resolved = true
				break
			}
		}
		ev.ResolvedAfterMs = ms(time.Since(start))
		res.Events = append(res.Events, ev)
		fprintf(b.out, "  !! stale %s %s: saw %s, want %s; resolved=%v after %.0f ms\n", kind, key, saw, want, ev.Resolved, ev.ResolvedAfterMs)
	}
	for i := 0; i < b.cfg.ConsistencyN && b.ctx.Err() == nil; i++ {
		key := fmt.Sprintf("consistency/k%04d", i)
		s1, s2 := seedFor(b.cfg.Seed, key+"#v1"), seedFor(b.cfg.Seed, key+"#v2")

		t := time.Now()
		h1, err := b.putGen(key, s1, consistencySize)
		putLat = append(putLat, time.Since(t))
		if err != nil {
			b.fail("consistency PUT v1 %s: %v", key, err)
			continue
		}
		h2 := expectedHash(s2, consistencySize)
		s, d, err := read(key, h1, h2)
		rawLat = append(rawLat, d)
		switch {
		case err != nil:
			b.fail("consistency GET after PUT %s: %v", key, err)
		case s == "other":
			b.mismatch("consistency %s after PUT v1: bytes that were never written", key)
		case s != "v1":
			res.StaleRAW++
			stale("read-after-write", key, s, "v1", h1, h2)
		}

		t = time.Now()
		if _, err := b.putGen(key, s2, consistencySize); err != nil {
			b.fail("consistency PUT v2 %s: %v", key, err)
			continue
		}
		owLat = append(owLat, time.Since(t))
		s, d, err = read(key, h1, h2)
		rowLat = append(rowLat, d)
		switch {
		case err != nil:
			b.fail("consistency GET after overwrite %s: %v", key, err)
		case s == "other":
			b.mismatch("consistency %s after PUT v2: bytes that were never written", key)
		case s != "v2":
			res.StaleOverwrite++
			stale("overwrite", key, s, "v2", h1, h2)
		}

		t = time.Now()
		if err := b.op(func(ctx context.Context) error { return b.drv.Delete(ctx, b.container, key) }); err != nil {
			b.fail("consistency DELETE %s: %v", key, err)
			continue
		}
		delLat = append(delLat, time.Since(t))
		s, d, err = read(key, h1, h2)
		rdelLat = append(rdelLat, d)
		switch {
		case err != nil:
			b.fail("consistency GET after DELETE %s: %v", key, err)
		case s == "other":
			b.mismatch("consistency %s after DELETE: bytes that were never written", key)
		case s != "missing":
			res.StaleDelete++
			stale("delete", key, s, "missing", h1, h2)
		}
	}
	sum := func(l []time.Duration) (t time.Duration) {
		for _, d := range l {
			t += d
		}
		return t
	}
	cs := fmt.Sprintf("%s ×%d", sizeName(consistencySize), b.cfg.ConsistencyN)
	b.addRow(row("consistency", cs, "PUT v1", putLat, 0, sum(putLat), int64(len(putLat))*consistencySize))
	b.addRow(row("consistency", cs, "GET new", rawLat, 0, sum(rawLat), 0))
	b.addRow(row("consistency", cs, "PUT v2", owLat, 0, sum(owLat), int64(len(owLat))*consistencySize))
	b.addRow(row("consistency", cs, "GET v2", rowLat, 0, sum(rowLat), 0))
	b.addRow(row("consistency", cs, "DELETE", delLat, 0, sum(delLat), 0))
	b.addRow(row("consistency", cs, "GET gone", rdelLat, 0, sum(rdelLat), 0))
	b.mu.Lock()
	b.rep.Consistency = res
	b.mu.Unlock()
	b.dropFolder("consistency")
}

// readState reads key and names what it holds: "v1", "v2", "missing", or
// "other" (bytes that were never written).
func (b *bench) readState(key string, h1, h2 []byte) (string, error) {
	ctx, cancel := b.opCtx()
	defer cancel()
	rc, err := b.drv.Get(ctx, b.container, key)
	if isNotFound(err) {
		return "missing", nil
	}
	if err != nil {
		return "", err
	}
	defer func() { _ = rc.Close() }()
	h := sha256.New()
	if _, err := io.Copy(h, rc); err != nil {
		return "", fmt.Errorf("read %s: %w", key, err)
	}
	sum := h.Sum(nil)
	switch {
	case bytes.Equal(sum, h1):
		return "v1", nil
	case bytes.Equal(sum, h2):
		return "v2", nil
	}
	return "other", nil
}

// --- listing --------------------------------------------------------------

func (b *bench) suiteListing() {
	folder := "list"
	segs := b.runSegs(folder)
	if err := b.raw.mkcolAll(b.ctx, segs); err != nil {
		b.fail("listing MKCOL: %v", err)
		return
	}
	name := func(i int) string { return fmt.Sprintf("f%07d", i) }
	created := 0
	for _, n := range b.cfg.ListCounts {
		if n > b.cfg.ListMax {
			fprintf(b.out, "  listing %d skipped (-list-max %d)\n", n, b.cfg.ListMax)
			continue
		}
		if n > created {
			from := created
			lat, errs, wall := b.pool(n-from, b.cfg.ListConc, "listing create", func(i int) error {
				code, body, err := b.raw.put(b.ctx, append(segs[:len(segs):len(segs)], name(from+i)), []byte{'x'})
				if err == nil && !ok2xx(code) {
					err = fmt.Errorf("PUT %s: %d %s", name(from+i), code, body)
				}
				return err
			})
			b.addRow(row("listing", fmt.Sprintf("%d files", n), "create", lat, errs, wall, int64(n-from)))
			created = n
		}
		cs := fmt.Sprintf("%d files", n)

		t := time.Now()
		var keys []string
		err := b.op(func(ctx context.Context) (err error) {
			keys, err = b.drv.List(ctx, b.container, folder+"/")
			return err
		})
		d := time.Since(t)
		r := row("listing", cs, "List drv", []time.Duration{d}, 0, d, 0)
		if err != nil {
			b.fail("listing driver List at %d: %v", n, err)
			r.Errors = 1
		} else if len(keys) != n {
			r.Note = fmt.Sprintf("listed %d, want %d", len(keys), n)
			b.fail("listing driver List: %s", r.Note)
		}
		b.addRow(r)

		names, d, err := b.raw.listDepth1(b.ctx, segs)
		r = row("listing", cs, "PROPFIND d1", []time.Duration{d}, 0, d, 0)
		if err != nil {
			b.fail("listing PROPFIND at %d: %v", n, err)
			r.Errors = 1
		} else if len(names) != n {
			r.Note = fmt.Sprintf("listed %d, want %d", len(names), n)
			b.fail("listing PROPFIND: %s", r.Note)
		}
		b.addRow(r)
	}
	b.dropFolder(folder)
}

// --- parity ---------------------------------------------------------------

func (b *bench) suiteParity() {
	k, m := b.cfg.ParityK, b.cfg.ParityM
	for _, size := range b.cfg.ParitySizes {
		shard := size / int64(k)
		folder := "parity/" + sizeName(size)
		cs := fmt.Sprintf("%s k%d+m%d (%s shards)", sizeName(size), k, m, sizeName(shard))
		key := func(i int) string { return fmt.Sprintf("%s/p%02d", folder, i) }
		hashes := make([][]byte, m)
		lat, errs, wall := b.pool(m, m, "parity write "+cs, func(i int) error {
			h, err := b.putGen(key(i), seedFor(b.cfg.Seed, key(i)), shard)
			hashes[i] = h
			return err
		})
		b.addRow(row("parity", cs, "write m", lat, errs, wall, int64(m-errs)*shard))
		readN := func(op string, n int) {
			lat, errs, wall := b.pool(n, n, "parity "+op+" "+cs, func(i int) error {
				j := i % m
				if hashes[j] == nil {
					return errors.New("not written")
				}
				_, err := b.getVerify(key(j), hashes[j], shard)
				return err
			})
			b.addRow(row("parity", cs, op, lat, errs, wall, int64(n-errs)*shard))
		}
		readN("read m", m)
		readN("degraded k", k)
		b.dropFolder(folder)
	}

	n, size, conc := b.cfg.ParitySmallN, b.cfg.ParitySmallSize, b.cfg.ParitySmallConc
	if n <= 0 {
		return
	}
	for _, layout := range []string{"flat", "fanout"} {
		folder := "parity-small/" + layout
		key := func(i int) string {
			name := fmt.Sprintf("s%06d", i)
			if layout == "flat" {
				return folder + "/" + name
			}
			h := fnv.New32a()
			_, _ = h.Write([]byte(name))
			x := fmt.Sprintf("%08x", h.Sum32())
			return folder + "/" + x[0:2] + "/" + x[2:4] + "/" + name
		}
		cs := fmt.Sprintf("%d×%s %s c%d", n, sizeName(size), layout, conc)
		hashes := make([][]byte, n)
		lat, errs, wall := b.pool(n, conc, "parity small PUT "+cs, func(i int) error {
			h, err := b.putGen(key(i), seedFor(b.cfg.Seed, key(i)), size)
			hashes[i] = h
			return err
		})
		b.addRow(row("parity", cs, "PUT", lat, errs, wall, int64(n-errs)*size))
		lat, errs, wall = b.pool(n, conc, "parity small GET "+cs, func(i int) error {
			if hashes[i] == nil {
				return errors.New("not written")
			}
			_, err := b.getVerify(key(i), hashes[i], size)
			return err
		})
		b.addRow(row("parity", cs, "GET", lat, errs, wall, int64(n-errs)*size))
		b.dropFolder(folder)
	}
	b.dropFolder("parity-small")
	b.dropFolder("parity")
}

// --- limits ---------------------------------------------------------------

// LimitsResult is what the server accepted of Sync's documented limits.
type LimitsResult struct {
	Paths  []LimitProbe  `json:"paths"`
	Names  []LimitProbe  `json:"names"`
	Folder *FolderResult `json:"folder,omitempty"`
}

// LimitProbe is one PUT + GET (+ listing) of an unusual path or name.
type LimitProbe struct {
	Name      string `json:"name,omitempty"`
	Length    int    `json:"length,omitempty"`
	PutStatus int    `json:"put_status"`
	PutBody   string `json:"put_body,omitempty"`
	ReadBack  bool   `json:"read_back"`
	Listed    bool   `json:"listed"`
	Note      string `json:"note,omitempty"`
}

// FolderResult is the files-per-folder probe.
type FolderResult struct {
	Target       int     `json:"target"`
	Created      int     `json:"created"`
	FirstFailure int     `json:"first_failure"` // -1 = none
	Status       int     `json:"status,omitempty"`
	Body         string  `json:"body,omitempty"`
	WallSec      float64 `json:"wall_sec"`
}

// limitNames are the characters and names Sync documents as unsupported
// or special (Windows-reserved characters and names, Office/OS temp files).
var limitNames = []string{
	"colon:x", "question?x", "star*x", "lt<x", "gt>x", "pipe|x", `quote"x`, `back\x`,
	"trailingdot.", " leading-space", "trailing-space ", "pct%41x", "hash#x", "plus+x",
	"ünïcødé-ñ", "emoji-😀", "CON", "NUL", "AUX", "con.txt", ".DS_Store", "desktop.ini", "~$x.docx",
}

func (b *bench) suiteLimits() {
	res := &LimitsResult{}
	base := b.runSegs("limits")
	if err := b.raw.mkcolAll(b.ctx, base); err != nil {
		b.fail("limits MKCOL: %v", err)
		return
	}

	for _, total := range b.cfg.LimitPaths {
		p := LimitProbe{Length: total}
		dir := append(base[:len(base):len(base)], fmt.Sprintf("path-%d", total))
		room := total - b.raw.decodedLen(dir) - 1
		if room < 1 {
			p.Note = fmt.Sprintf("skipped: the run folder alone is %d chars", b.raw.decodedLen(dir))
			res.Paths = append(res.Paths, p)
			continue
		}
		segs := dir
		for room > 0 { // segments ≤ 200 chars: the per-name limit is not what this probes
			n := min(room, 200)
			if room-n == 1 { // a last segment of 0 chars after the '/' is impossible
				n--
			}
			segs = append(segs, strings.Repeat("a", n))
			room -= n
			if room > 0 {
				room-- // the '/'
			}
		}
		if got := b.raw.decodedLen(segs); got != total {
			p.Note = fmt.Sprintf("built %d chars", got)
		}
		if err := b.raw.mkcolAll(b.ctx, segs[:len(segs)-1]); err != nil {
			p.Note = strings.TrimSpace(p.Note + " MKCOL: " + err.Error())
			res.Paths = append(res.Paths, p)
			continue
		}
		b.probe(&p, segs)
		res.Paths = append(res.Paths, p)
	}

	names := append(base[:len(base):len(base)], "names")
	if err := b.raw.mkcolAll(b.ctx, names); err != nil {
		b.fail("limits MKCOL names: %v", err)
	} else {
		for _, name := range limitNames {
			p := LimitProbe{Name: name}
			b.probe(&p, append(names[:len(names):len(names)], name))
			res.Names = append(res.Names, p)
		}
		listed, _, err := b.raw.listDepth1(b.ctx, names)
		if err != nil {
			b.fail("limits list names: %v", err)
		}
		for i := range res.Names {
			res.Names[i].Listed = contains(listed, res.Names[i].Name)
		}
	}

	if b.cfg.LimitsFolder {
		res.Folder = b.folderLimit(append(base[:len(base):len(base)], "folder"))
	}
	b.mu.Lock()
	b.rep.Limits = res
	b.mu.Unlock()
	for _, p := range append(append([]LimitProbe(nil), res.Paths...), res.Names...) {
		fprintf(b.out, "  limits %-16q len=%-4d put=%d get=%v %s\n", p.Name, p.Length, p.PutStatus, p.ReadBack, p.Note)
	}
	b.dropFolder("limits")
}

// probe PUTs the probe's own name as the body and reads it back.
func (b *bench) probe(p *LimitProbe, segs []string) {
	body := []byte("limits:" + strings.Join(segs, "/"))
	code, msg, err := b.raw.put(b.ctx, segs, body)
	if err != nil {
		p.Note = err.Error()
		return
	}
	p.PutStatus = code
	if !ok2xx(code) {
		p.PutBody = msg
		return
	}
	gc, got, err := b.raw.get(b.ctx, segs)
	switch {
	case err != nil:
		p.Note = "GET: " + err.Error()
	case gc != 200:
		p.Note = fmt.Sprintf("GET %d after a %d PUT", gc, code)
	case !bytes.Equal(got, body):
		p.Note = "GET returned other bytes"
		b.mismatch("limits %q: GET returned bytes that were not written", strings.Join(segs, "/"))
	default:
		p.ReadBack = true
	}
}

// folderLimit creates zero-byte files in one folder until LimitsFolderMax or
// the first failure (Sync: 50,000 files per folder).
func (b *bench) folderLimit(segs []string) *FolderResult {
	res := &FolderResult{Target: b.cfg.LimitsFolderMax, FirstFailure: -1}
	if err := b.raw.mkcolAll(b.ctx, segs); err != nil {
		b.fail("limits folder MKCOL: %v", err)
		return res
	}
	var mu = make(chan struct{}, 1)
	mu <- struct{}{}
	failed := false
	created := 0
	start := time.Now()
	_, _, wall := b.pool(res.Target, b.cfg.LimitsConc, "limits folder", func(i int) error {
		<-mu
		stop := failed
		mu <- struct{}{}
		if stop {
			return nil
		}
		code, body, err := b.raw.put(b.ctx, append(segs[:len(segs):len(segs)], fmt.Sprintf("z%06d", i)), nil)
		<-mu
		defer func() { mu <- struct{}{} }()
		if err != nil || !ok2xx(code) {
			if !failed || i < res.FirstFailure {
				res.FirstFailure, res.Status, res.Body = i, code, body
				if err != nil {
					res.Body = err.Error()
				}
			}
			failed = true
			return nil
		}
		created++
		if created%1000 == 0 {
			fprintf(b.out, "  limits folder: %d files (%.0f/s)\n", created, float64(created)/time.Since(start).Seconds())
		}
		return nil
	})
	res.Created = created
	res.WallSec = wall.Seconds()
	return res
}
