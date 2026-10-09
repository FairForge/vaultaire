package drivers

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"go.uber.org/zap"

	"github.com/FairForge/vaultaire/internal/engine"
)

// Striped objects (multi-bridge backend only). One object is one PUT to one
// bridge, and one bridge uploads at ~30 MB/s (2 GiB in 71 s, 5 GiB in
// 167 s on prod, 2026-10-07) while five bridges move 156 MB/s up / 450 down
// when they work in parallel. So a Put of a known length ≥ the stripe
// minimum (SYNC_WEBDAV_STRIPE_MIN, default 512 MiB) is cut into pieces of
// SYNC_WEBDAV_STRIPE_PIECE (default 256 MiB), each routed by HRW on its OWN
// name — the pieces spread over every bridge — and uploaded in parallel.
//
// Layout below the root (resource names; '%' followed by a non-hex letter
// is never produced by davName, which writes a literal '%' as "%25"):
//
//	t-<tenant>/<container>/…/<leaf>%s                  the manifest (the object)
//	t-<tenant>/<container>%p/<hh>/<h>-<gen>/key%o      which object the pieces are for
//	t-<tenant>/<container>%p/<hh>/<h>-<gen>/p00000%o   piece 0, p00001%o, …
//
// <h> is the first 32 hex digits of sha256(artifact) (keys run to 1,024
// characters; a name to 248), <hh> its first two; <gen> is a generation —
// "g<UTC time>-<random>" — so a reaper can tell an old one from a running
// upload without a server modtime (Sync's bridges report 1970 for every
// file). One folder per upload and nothing shared between uploads of a key:
// deleting a generation deletes its folder, and no per-key folder is left
// behind (an `<hh>` folder holds ≤ 50,000 entries on Sync). Manifests of the
// first layout (#626: `<hh>/<h>/<gen>/`) still read; the reaper removes its
// empty key folders. The manifest's leaf marker `%s` is not the plain object's `%o`, so
// an ordinary object can never be read as a manifest whatever its bytes.
// The manifest and the plain file share the key's bridge (HRW on the `%o`
// names): an object is either, and Get reads `%o` first, the manifest on a
// miss.
//
// Write order — nothing references a piece before every piece is verified:
// key file, pieces (≤ K staged on disk and in flight, K = bridges × large
// slots; each from a seekable section of its staging file so the driver's
// own retry resends it; size-verified), then the manifest (replacing an
// older one), then the plain `%o` file of a previous small version is
// deleted, then the previous generation's pieces. A failure before the
// manifest leaves pieces nobody references: deleted at once when possible,
// else by ReapOrphanStripes after a grace period (the stripe_gc job). A
// crash between the manifest and the deletion of an old `%o` file leaves
// both; Get then serves the `%o` file until the next write of the key.
//
// Unknown length (ContentLength 0): never striped — the body streams to one
// bridge as before (every engine caller states the length; aws-chunked
// streams are decoded to a known length upstream).

const (
	// WebDAVStripeMarker ends the resource name of a striped object's
	// manifest (the plain object's leaf ends in WebDAVLeafMarker).
	WebDAVStripeMarker = "%s"
	// webdavStripeDirSuffix ends the folder of a container's pieces.
	webdavStripeDirSuffix = "%p"
	// WebDAVDefaultStripeMin is the smallest known-length body striped.
	WebDAVDefaultStripeMin int64 = 512 << 20
	// WebDAVDefaultStripePiece is the piece size.
	WebDAVDefaultStripePiece int64 = 256 << 20
	// webdavStripePieceMin / Max bound SYNC_WEBDAV_STRIPE_PIECE.
	webdavStripePieceMin int64 = 16 << 20
	webdavStripePieceMax int64 = 4 << 30
	// WebDAVDefaultStripeGrace is the age past which an unreferenced
	// generation is an orphan (a striped upload never runs that long).
	WebDAVDefaultStripeGrace = 6 * time.Hour

	// webdavStripeSweeps / webdavDefaultStripeSettle: the extra cleanup
	// passes after a failed striped upload, and the pause before each.
	webdavStripeSweeps        = 2
	webdavDefaultStripeSettle = 2 * time.Second

	stripeManifestFormat = "vaultaire-webdav-stripe/1"
	stripeManifestMax    = 4 << 20
	stripeKeyFile        = "key" + WebDAVLeafMarker
	stripeGenTimeLayout  = "20060102T150405Z"
)

// errStripePieceGone: a piece the manifest names is not on its bridge — the
// object was overwritten or deleted during the read. Retryable (503), never
// a miss.
var errStripePieceGone = fmt.Errorf("%w: a piece of the striped object is gone (overwritten or deleted during the read)", engine.ErrAllBackendsUnavailable)

var (
	webdavStripePieces = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "vaultaire_webdav_stripe_pieces_total",
		Help: "Pieces of striped objects on a multi-bridge WebDAV backend, by backend and op (written, read, deleted).",
	}, []string{"backend", "op"})
	webdavStripeBytes = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "vaultaire_webdav_stripe_put_bytes_total",
		Help: "Bytes of objects stored striped over the bridges of a multi-bridge WebDAV backend, by backend.",
	}, []string{"backend"})
	webdavStripeOrphans = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "vaultaire_webdav_stripe_orphans_total",
		Help: "Piece generations no manifest references, by backend and outcome (left: a failed upload could not delete them; reaped: deleted by the reaper).",
	}, []string{"backend", "outcome"})
)

func initStripeSeries(backend string) {
	for _, op := range []string{"written", "read", "deleted"} {
		webdavStripePieces.WithLabelValues(backend, op)
	}
	webdavStripeBytes.WithLabelValues(backend)
	for _, o := range []string{"left", "reaped"} {
		webdavStripeOrphans.WithLabelValues(backend, o)
	}
}

// stripeManifest is the content of a striped object's `%s` file.
type stripeManifest struct {
	Format    string        `json:"format"`
	Size      int64         `json:"size"`
	PieceSize int64         `json:"piece_size"`
	Gen       string        `json:"gen"`
	Dir       []string      `json:"dir"` // the generation's folder, resource names below the root
	Pieces    []stripePiece `json:"pieces"`
}

type stripePiece struct {
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

// stripeKey is the content of a generation's key file.
type stripeKey struct {
	Container string `json:"container"`
	Artifact  string `json:"artifact"`
}

// --- names -----------------------------------------------------------------

// manifestSegment is the key segment of a manifest's resource name.
func manifestSegment(name string) (string, bool) {
	if !strings.HasSuffix(name, WebDAVStripeMarker) {
		return "", false
	}
	return keySegment(strings.TrimSuffix(name, WebDAVStripeMarker)), true
}

// objectLeafSegment decodes the leaf of an object: plain or striped.
func objectLeafSegment(name string) (string, bool) {
	if seg, ok := leafSegment(name); ok {
		return seg, true
	}
	return manifestSegment(name)
}

// manifestNamesOf turns an object's resource names (leaf `…%o`) into its
// manifest's (leaf `…%s`).
func manifestNamesOf(names []string) []string {
	out := append([]string(nil), names...)
	last := len(out) - 1
	out[last] = strings.TrimSuffix(out[last], WebDAVLeafMarker) + WebDAVStripeMarker
	return out
}

// routingNames are the names a resource is routed by: a manifest goes where
// its plain object would.
func routingNames(names []string) []string {
	last := names[len(names)-1]
	if !strings.HasSuffix(last, WebDAVStripeMarker) {
		return names
	}
	out := append([]string(nil), names...)
	out[len(out)-1] = strings.TrimSuffix(last, WebDAVStripeMarker) + WebDAVLeafMarker
	return out
}

func stripeKeyHash(artifact string) string {
	sum := sha256.Sum256([]byte(artifact))
	return hex.EncodeToString(sum[:16])
}

// stripeDir is a generation's folder: tenant folder and container name are
// the object's own (names[0], names[1]).
func stripeDir(tenantFolder, containerName, artifact, gen string) []string {
	h := stripeKeyHash(artifact)
	return []string{tenantFolder, containerName + webdavStripeDirSuffix, h[:2], h + "-" + gen}
}

// splitKeyGen splits a generation folder name of the current layout,
// `<32 hex>-<gen>`.
func splitKeyGen(name string) (h, gen string, ok bool) {
	if len(name) < 34 || name[32] != '-' || !isHex32(name[:32]) {
		return "", "", false
	}
	if _, ok := stripeGenTime(name[33:]); !ok {
		return "", "", false
	}
	return name[:32], name[33:], true
}

func isHex32(s string) bool {
	if len(s) != 32 {
		return false
	}
	for i := 0; i < len(s); i++ {
		if !isHex(s[i]) || ('A' <= s[i] && s[i] <= 'F') {
			return false
		}
	}
	return true
}

// dirGen is the generation a generation folder (either layout) holds.
func dirGen(dir []string) string {
	switch len(dir) {
	case 4:
		if _, g, ok := splitKeyGen(dir[3]); ok {
			return g
		}
	case 5:
		return dir[4]
	}
	return ""
}

func pieceName(i int) string { return fmt.Sprintf("p%05d", i) + WebDAVLeafMarker }

func newStripeGen(now time.Time) string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return "g" + now.UTC().Format(stripeGenTimeLayout) + "-" + hex.EncodeToString(b[:])
}

// stripeGenTime is the creation time a generation's name carries.
func stripeGenTime(gen string) (time.Time, bool) {
	if len(gen) != 1+len(stripeGenTimeLayout)+1+16 || gen[0] != 'g' || gen[1+len(stripeGenTimeLayout)] != '-' {
		return time.Time{}, false
	}
	t, err := time.Parse(stripeGenTimeLayout, gen[1:1+len(stripeGenTimeLayout)])
	if err != nil {
		return time.Time{}, false
	}
	if _, err := hex.DecodeString(gen[2+len(stripeGenTimeLayout):]); err != nil {
		return time.Time{}, false
	}
	return t, true
}

// validate checks a manifest read for an object at names.
func (man *stripeManifest) validate(names []string) error {
	if man.Format != stripeManifestFormat {
		return fmt.Errorf("unknown stripe manifest format %q", man.Format)
	}
	if (len(man.Dir) != 4 && len(man.Dir) != 5) || man.Dir[0] != names[0] || man.Dir[1] != names[1]+webdavStripeDirSuffix {
		return errors.New("stripe manifest names a folder outside the object's container")
	}
	if _, ok := stripeGenTime(man.Gen); !ok || dirGen(man.Dir) != man.Gen {
		return fmt.Errorf("stripe manifest generation %q does not match its folder", man.Gen)
	}
	if man.PieceSize <= 0 || len(man.Pieces) == 0 {
		return errors.New("stripe manifest has no pieces")
	}
	var sum int64
	for i, p := range man.Pieces {
		if p.Size <= 0 || p.Size > man.PieceSize || (i < len(man.Pieces)-1 && p.Size != man.PieceSize) {
			return fmt.Errorf("stripe manifest piece %d has size %d (piece size %d)", i, p.Size, man.PieceSize)
		}
		sum += p.Size
	}
	if sum != man.Size {
		return fmt.Errorf("stripe manifest pieces sum to %d, size %d", sum, man.Size)
	}
	return nil
}

// --- write -------------------------------------------------------------------

// stripes reports whether a Put of o is striped.
func (m *MultiWebDAVDriver) stripes(o engine.PutOptions) bool {
	return m.stripeMin > 0 && o.ContentLength >= m.stripeMin
}

// putStriped stores a known-length body as pieces + a manifest (see the top
// of this file). names/order are the object's (`%o` leaf) and its bridges.
func (m *MultiWebDAVDriver) putStriped(ctx context.Context, key string, names []string, order []int,
	container, artifact string, data io.Reader, o engine.PutOptions) error {
	size := o.ContentLength
	gen := newStripeGen(m.now())
	dir := stripeDir(names[0], names[1], artifact, gen)
	man := &stripeManifest{Format: stripeManifestFormat, Size: size, PieceSize: m.stripePiece, Gen: gen, Dir: dir}

	kb, err := json.Marshal(stripeKey{Container: container, Artifact: artifact})
	if err != nil {
		return fmt.Errorf("%s put %s: %w", m.name, key, err)
	}
	keyNames := append(append([]string(nil), dir...), stripeKeyFile)
	if err := m.putSmall(ctx, key+" (stripe key)", keyNames, kb); err != nil {
		return fmt.Errorf("%s put %s: stripe key file: %w", m.name, key, err)
	}

	n := int((size + m.stripePiece - 1) / m.stripePiece)
	man.Pieces = make([]stripePiece, n)
	started := 0 // pieces whose upload began: a cancelled one may still land
	pctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		firstErr error
	)
	fail := func(err error) {
		mu.Lock()
		if firstErr == nil {
			firstErr = err
		}
		mu.Unlock()
		cancel()
	}
	consumed := false
	for i := 0; i < n && pctx.Err() == nil; i++ {
		release, err := acquireSlot(pctx, m.stripeSlots, "stripe staging slots")
		if err != nil {
			fail(err)
			break
		}
		sz := m.stripePiece
		if rest := size - int64(i)*m.stripePiece; rest < sz {
			sz = rest
		}
		f, sum, err := m.stage(data, sz)
		consumed = consumed || err == nil || !errors.Is(err, errStageCreate)
		if err != nil {
			release()
			fail(fmt.Errorf("piece %d: %w", i, err))
			break
		}
		man.Pieces[i] = stripePiece{Size: sz, SHA256: sum}
		started = i + 1
		wg.Add(1)
		go func(i int, f *os.File, sz int64) {
			defer wg.Done()
			defer release()
			defer m.unstage(f)
			if err := m.putPiece(pctx, key, dir, i, f, sz); err != nil {
				fail(fmt.Errorf("piece %d: %w", i, err))
			}
		}(i, f, sz)
	}
	wg.Wait() // every piece goroutine has returned (and unstaged its file) before any cleanup
	if firstErr == nil && ctx.Err() != nil {
		firstErr = ctx.Err()
	}
	if firstErr != nil {
		m.dropGeneration(ctx, key, man, started)
		err := fmt.Errorf("%s put %s (striped, %d pieces): %w", m.name, key, n, firstErr)
		if consumed {
			err = fmt.Errorf("%w: %w", err, engine.ErrNoFailover)
		}
		return err
	}

	// Commit: the manifest replaces whatever the key was.
	kbridge := m.bridges[order[0]]
	old, oerr := m.readManifestOn(ctx, kbridge, key, names, engine.ErrNotFound(container, artifact))
	if oerr != nil && !notFoundErr(oerr) {
		m.logger.Warn("webdav stripe: previous manifest unreadable — its pieces are left to the reaper",
			zap.String("backend", m.name), zap.String("key", key), zap.Error(oerr))
		old = nil
	}
	mb, err := json.Marshal(man)
	if err != nil {
		return fmt.Errorf("%s put %s: %w", m.name, key, err)
	}
	err = kbridge.drv.putNames(ctx, key+" (stripe manifest)", manifestNamesOf(names), bytes.NewReader(mb), engine.WithContentLength(int64(len(mb))))
	m.note(ctx, kbridge, err)
	if err != nil {
		m.dropGeneration(ctx, key, man, n)
		return fmt.Errorf("%s put %s: stripe manifest: %w: %w", m.name, key, err, engine.ErrNoFailover)
	}
	// The plain file of a previous small version would shadow the manifest.
	if err := kbridge.drv.removeNames(ctx, names); err != nil {
		return fmt.Errorf("%s put %s: the striped object is stored, but the previous plain file could not be removed and still shadows it: %w: %w",
			m.name, key, err, engine.ErrNoFailover)
	}
	webdavStripeBytes.WithLabelValues(m.name).Add(float64(size))
	m.mcache.put(manifestCacheKey(names), man)
	if old != nil && old.Gen != gen {
		if err := m.deleteStripe(ctx, key, old); err != nil {
			webdavStripeOrphans.WithLabelValues(m.name, "left").Inc()
			m.logger.Warn("webdav stripe: previous generation not deleted — left to the reaper",
				zap.String("backend", m.name), zap.String("key", key), zap.String("gen", old.Gen), zap.Error(err))
		}
	}
	return nil
}

var errStageCreate = errors.New("create staging file")

// stage copies the next sz bytes of data into a staging file and hashes
// them; the file is open and positioned anywhere (callers read sections).
func (m *MultiWebDAVDriver) stage(data io.Reader, sz int64) (*os.File, string, error) {
	f, err := os.CreateTemp(m.stagingDir, "piece-*")
	if err != nil {
		return nil, "", fmt.Errorf("%w: %w", errStageCreate, err)
	}
	cur := m.staged.Add(1)
	for {
		peak := m.stagedPeak.Load()
		if cur <= peak || m.stagedPeak.CompareAndSwap(peak, cur) {
			break
		}
	}
	h := sha256.New()
	w, err := io.CopyN(io.MultiWriter(f, h), data, sz)
	if err != nil {
		m.unstage(f)
		if errors.Is(err, io.EOF) {
			return nil, "", fmt.Errorf("%w: the body ended after %d of the piece's %d bytes (shorter than its declared length)", engine.ErrInvalidInput, w, sz)
		}
		return nil, "", fmt.Errorf("stage piece: %w", err)
	}
	return f, hex.EncodeToString(h.Sum(nil)), nil
}

func (m *MultiWebDAVDriver) unstage(f *os.File) {
	_ = f.Close()
	_ = os.Remove(f.Name())
	m.staged.Add(-1)
}

// putPiece uploads piece i from its staging file through the piece's bridge.
func (m *MultiWebDAVDriver) putPiece(ctx context.Context, key string, dir []string, i int, f *os.File, sz int64) error {
	pn := append(append([]string(nil), dir...), pieceName(i))
	b := m.bridges[m.rank(pn)[0]]
	release, err := acquireSlot(ctx, b.largeUp, fmt.Sprintf("large-upload slots of bridge %d", b.idx))
	if err != nil {
		return err
	}
	defer release()
	err = b.drv.putNames(ctx, fmt.Sprintf("%s (piece %d)", key, i), pn, io.NewSectionReader(f, 0, sz), engine.WithContentLength(sz))
	m.note(ctx, b, err)
	if err != nil {
		return fmt.Errorf("bridge %d: %w", b.idx, err)
	}
	webdavStripePieces.WithLabelValues(m.name, "written").Inc()
	return nil
}

// putSmall writes a small file through its routed bridge.
func (m *MultiWebDAVDriver) putSmall(ctx context.Context, key string, names []string, body []byte) error {
	b := m.bridges[m.rank(names)[0]]
	err := b.drv.putNames(ctx, key, names, bytes.NewReader(body), engine.WithContentLength(int64(len(body))))
	m.note(ctx, b, err)
	if err != nil {
		return fmt.Errorf("bridge %d: %w", b.idx, err)
	}
	return nil
}

// dropGeneration deletes what a failed striped upload wrote (best effort,
// detached from a cancelled caller); what stays is the reaper's. Every
// piece the upload STARTED is deleted, confirmed or not: a PUT cancelled
// after the bridge stored it has no confirmation, and one the bridge
// receives only after the client gave up lands after a first pass — so the
// pass is repeated webdavStripeSweeps times, stripeSettle apart. Each pass
// deletes by name on each piece's own bridge (never a listing, which a
// bridge's stale view could leave short).
func (m *MultiWebDAVDriver) dropGeneration(ctx context.Context, key string, man *stripeManifest, started int) {
	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Minute)
	defer cancel()
	pass := func() []error {
		var errs []error
		for i := 0; i < started; i++ {
			if err := m.removeRouted(cctx, append(append([]string(nil), man.Dir...), pieceName(i))); err != nil {
				errs = append(errs, err)
			}
		}
		if err := m.removeRouted(cctx, append(append([]string(nil), man.Dir...), stripeKeyFile)); err != nil {
			errs = append(errs, err)
		}
		return errs
	}
	errs := pass()
	for round := 0; round < webdavStripeSweeps && len(errs) == 0 && m.stripeSettle > 0; round++ {
		t := time.NewTimer(m.stripeSettle)
		select {
		case <-t.C:
		case <-cctx.Done():
			t.Stop()
		}
		errs = pass()
	}
	if len(errs) > 0 {
		webdavStripeOrphans.WithLabelValues(m.name, "left").Inc()
		m.logger.Warn("webdav stripe: a failed upload's pieces were not all deleted — left to the reaper",
			zap.String("backend", m.name), zap.String("key", key), zap.String("gen", man.Gen), zap.Error(errors.Join(errs...)))
		return
	}
	m.removeGenerationDir(cctx, man.Dir)
}

// removeGenerationDir deletes a generation's folder once its files are
// deleted, and a first-layout key folder it leaves empty (best effort: a
// bridge that does not see the deletions yet keeps them — the reaper's).
// A collection DELETE is recursive, so the folder goes only when EVERY
// bridge lists it empty (removeEmptyNames; Prompt 2b A6 — one bridge's
// empty view used to decide it).
func (m *MultiWebDAVDriver) removeGenerationDir(ctx context.Context, dir []string) {
	if err := m.removeEmptyNames(ctx, dir); err != nil || len(dir) != 5 {
		return
	}
	// The key folder of the first layout may hold the key's live generation:
	// removed only when every bridge lists it empty (removeEmptyNames).
	_ = m.removeEmptyNames(ctx, dir[:4])
}

// removeRouted deletes the file at names through its routed bridge.
func (m *MultiWebDAVDriver) removeRouted(ctx context.Context, names []string) error {
	b := m.bridges[m.rank(routingNames(names))[0]]
	err := b.drv.removeNames(ctx, names)
	m.note(ctx, b, err)
	return err
}

// deleteStripe deletes a generation's pieces (each through its bridge), its
// key file and its folder.
func (m *MultiWebDAVDriver) deleteStripe(ctx context.Context, key string, man *stripeManifest) error {
	var errs []error
	for i := range man.Pieces {
		if err := m.removeRouted(ctx, append(append([]string(nil), man.Dir...), pieceName(i))); err != nil {
			errs = append(errs, err)
			continue
		}
		webdavStripePieces.WithLabelValues(m.name, "deleted").Inc()
	}
	if err := m.removeRouted(ctx, append(append([]string(nil), man.Dir...), stripeKeyFile)); err != nil {
		errs = append(errs, err)
	}
	if len(errs) > 0 {
		return fmt.Errorf("%s delete %s (striped, gen %s): %w", m.name, key, man.Gen, errors.Join(errs...))
	}
	m.removeGenerationDir(ctx, man.Dir)
	return nil
}

// --- read --------------------------------------------------------------------

// readManifestOn reads the manifest of the object at names through bridge
// b; a missing one is notFound (wrapped).
func (m *MultiWebDAVDriver) readManifestOn(ctx context.Context, b *webdavBridge, key string, names []string, notFound error) (*stripeManifest, error) {
	rc, err := b.drv.getNames(ctx, key+" (stripe manifest)", manifestNamesOf(names), notFound)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rc.Close() }()
	raw, err := io.ReadAll(io.LimitReader(rc, stripeManifestMax+1))
	if err != nil {
		return nil, fmt.Errorf("%s read %s (stripe manifest): %w", m.name, key, err)
	}
	if len(raw) > stripeManifestMax {
		return nil, fmt.Errorf("%s read %s: stripe manifest larger than %d bytes", m.name, key, stripeManifestMax)
	}
	var man stripeManifest
	if err := json.Unmarshal(raw, &man); err != nil {
		return nil, fmt.Errorf("%s read %s: stripe manifest: %w", m.name, key, err)
	}
	if err := man.validate(names); err != nil {
		return nil, fmt.Errorf("%s read %s: %w", m.name, key, err)
	}
	return &man, nil
}

// openOn opens the object at names on bridge b: the plain file, else the
// manifest's pieces. whole = Get (offset 0, to the end, verified pieces).
func (m *MultiWebDAVDriver) openOn(ctx context.Context, b *webdavBridge, key string, names []string,
	notFound error, offset, length int64, whole bool) (io.ReadCloser, error) {
	ck := manifestCacheKey(names)
	if man, ok := m.mcache.get(ck); ok {
		r, err := m.openStripe(ctx, key, ck, man, offset, length)
		if !errors.Is(err, errStripePieceGone) {
			return r, err
		}
		m.mcache.drop(ck) // replaced or deleted elsewhere: resolve the key again
	}
	var rc io.ReadCloser
	var err error
	if whole {
		rc, err = b.drv.getNames(ctx, key, names, notFound)
	} else {
		rc, err = b.drv.getRangeNames(ctx, key, names, offset, length, notFound)
	}
	if !notFoundErr(err) {
		return rc, err
	}
	man, merr := m.readManifestOn(ctx, b, key, names, notFound)
	if merr != nil {
		if notFoundErr(merr) {
			return nil, err // neither: the plain file's miss
		}
		return nil, merr
	}
	m.mcache.put(ck, man)
	r, oerr := m.openStripe(ctx, key, ck, man, offset, length)
	if errors.Is(oerr, errStripePieceGone) {
		m.mcache.drop(ck)
		// Overwritten since the manifest was read: once more, from the new one.
		if man2, err2 := m.readManifestOn(ctx, b, key, names, notFound); err2 == nil && man2.Gen != man.Gen {
			m.mcache.put(ck, man2)
			return m.openStripe(ctx, key, ck, man2, offset, length)
		}
	}
	return r, oerr
}

// stripeSeg is one piece's part of a read.
type stripeSeg struct {
	piece    int
	from, to int64 // within the piece
}

// openStripe opens [offset, offset+length) of a striped object (length <= 0
// = to the end): the first piece and the second at the same time (a range
// across a boundary waits for one round trip, not two; a whole read has the
// next piece on its way before the first byte), then always one ahead. The
// first piece's errors are the open's. ck is the manifest's cache key (a
// piece found gone mid-read drops the entry).
func (m *MultiWebDAVDriver) openStripe(ctx context.Context, key, ck string, man *stripeManifest, offset, length int64) (io.ReadCloser, error) {
	if offset < 0 {
		return nil, fmt.Errorf("%s get range %s: %w: negative offset", m.name, key, engine.ErrInvalidInput)
	}
	end := man.Size
	if length > 0 && offset+length < end {
		end = offset + length
	}
	if offset >= end {
		return io.NopCloser(strings.NewReader("")), nil
	}
	var segs []stripeSeg
	for i := int(offset / man.PieceSize); i < len(man.Pieces); i++ {
		start := int64(i) * man.PieceSize
		if start >= end {
			break
		}
		from := max(offset-start, 0)
		to := min(end-start, man.Pieces[i].Size)
		segs = append(segs, stripeSeg{piece: i, from: from, to: to})
	}
	r := &stripeReader{m: m, ctx: ctx, key: key, ck: ck, man: man, segs: segs}
	first := make(chan openResult, 1)
	go func() {
		rc, err := m.readSeg(ctx, key, man, segs[0])
		first <- openResult{rc, err}
	}()
	r.prefetch(1)
	res := <-first
	if res.err != nil {
		_ = r.Close()
		return nil, res.err
	}
	r.cur = res.rc
	return r, nil
}

// readSeg opens one segment on the piece's bridge (with the read fallback).
func (m *MultiWebDAVDriver) readSeg(ctx context.Context, key string, man *stripeManifest, s stripeSeg) (io.ReadCloser, error) {
	pn := append(append([]string(nil), man.Dir...), pieceName(s.piece))
	p := man.Pieces[s.piece]
	whole := s.from == 0 && s.to == p.Size
	what := fmt.Sprintf("%s (piece %d)", key, s.piece)
	gone := fmt.Errorf("%w: piece %d", errStripePieceGone, s.piece)
	rc, err := readFrom(ctx, m, "get "+what, m.rank(pn),
		func(b *webdavBridge) (io.ReadCloser, error) {
			if whole {
				return b.drv.getNames(ctx, what, pn, gone)
			}
			return b.drv.getRangeNames(ctx, what, pn, s.from, s.to-s.from, gone)
		},
		func(_ io.ReadCloser, err error) bool { return errors.Is(err, errStripePieceGone) })
	if err != nil {
		return nil, err
	}
	webdavStripePieces.WithLabelValues(m.name, "read").Inc()
	sr := &segReader{rc: rc, want: s.to - s.from, what: what}
	if whole {
		sr.h, sr.sum = sha256.New(), p.SHA256
	}
	return sr, nil
}

// segReader checks a segment's length (and a whole piece's sha256) at EOF.
type segReader struct {
	rc   io.ReadCloser
	want int64
	n    int64
	h    hash.Hash
	sum  string
	what string
}

func (s *segReader) Read(p []byte) (int, error) {
	n, err := s.rc.Read(p)
	s.n += int64(n)
	if s.h != nil && n > 0 {
		s.h.Write(p[:n])
	}
	if s.n > s.want {
		return n, fmt.Errorf("%s: more than the %d bytes expected", s.what, s.want)
	}
	if errors.Is(err, io.EOF) {
		if s.n != s.want {
			return n, fmt.Errorf("%s: %d of %d bytes: %w", s.what, s.n, s.want, io.ErrUnexpectedEOF)
		}
		if s.h != nil && hex.EncodeToString(s.h.Sum(nil)) != s.sum {
			return n, fmt.Errorf("%s: sha256 mismatch (stored piece corrupt)", s.what)
		}
	}
	return n, err
}

func (s *segReader) Close() error { return s.rc.Close() }

type openResult struct {
	rc  io.ReadCloser
	err error
}

// stripeReader streams the segments in order, one prefetched.
type stripeReader struct {
	m    *MultiWebDAVDriver
	ctx  context.Context
	key  string
	ck   string
	man  *stripeManifest
	segs []stripeSeg
	idx  int
	cur  io.ReadCloser
	next chan openResult
	err  error
}

func (r *stripeReader) prefetch(k int) {
	if k >= len(r.segs) {
		r.next = nil
		return
	}
	ch := make(chan openResult, 1)
	r.next = ch
	go func() {
		rc, err := r.m.readSeg(r.ctx, r.key, r.man, r.segs[k])
		ch <- openResult{rc, err}
	}()
}

func (r *stripeReader) Read(p []byte) (int, error) {
	for {
		if r.err != nil {
			return 0, r.err
		}
		if r.cur == nil {
			if r.idx >= len(r.segs) || r.next == nil {
				return 0, io.EOF
			}
			res := <-r.next
			r.next = nil
			if res.err != nil {
				r.fail(res.err)
				return 0, r.err
			}
			r.cur = res.rc
			r.prefetch(r.idx + 1)
		}
		n, err := r.cur.Read(p)
		if errors.Is(err, io.EOF) {
			_ = r.cur.Close()
			r.cur = nil
			r.idx++
			if n > 0 {
				return n, nil
			}
			continue
		}
		if err != nil {
			r.fail(err)
		}
		return n, err
	}
}

// fail ends the read; a piece found gone drops the cached manifest.
func (r *stripeReader) fail(err error) {
	r.err = err
	if errors.Is(err, errStripePieceGone) {
		r.m.mcache.drop(r.ck)
	}
}

func (r *stripeReader) Close() error {
	var err error
	if r.cur != nil {
		err = r.cur.Close()
		r.cur = nil
	}
	if ch := r.next; ch != nil {
		r.next = nil
		go func() {
			if res := <-ch; res.rc != nil {
				_ = res.rc.Close()
			}
		}()
	}
	r.err = errors.New("read after close")
	return err
}

// --- reaper --------------------------------------------------------------------

// StripeReapResult is what ReapOrphanStripes did.
type StripeReapResult struct {
	Backend        string   `json:"backend"`
	Generations    int      `json:"generations_scanned"`
	Young          int      `json:"generations_young"`
	Live           int      `json:"generations_live"`
	Reaped         int      `json:"generations_reaped"`
	FilesDeleted   int      `json:"files_deleted"`
	FoldersRemoved int      `json:"folders_removed"`
	UnknownFolders int      `json:"unknown_folders"`
	Errors         []string `json:"errors,omitempty"`
}

func (r *StripeReapResult) fail(format string, args ...any) {
	if len(r.Errors) < 50 {
		r.Errors = append(r.Errors, fmt.Sprintf(format, args...))
	}
}

// ReapOrphanStripes deletes piece generations no manifest references whose
// name says they are older than olderThan (≤ 0 = WebDAVDefaultStripeGrace).
// It never trusts a server modtime, and never deletes on doubt: a key file
// that does not match its folder, or a manifest that cannot be read, keeps
// the generation (an error in the result). The error is a listing that
// failed (nothing can be said about the rest).
func (m *MultiWebDAVDriver) ReapOrphanStripes(ctx context.Context, olderThan time.Duration) (StripeReapResult, error) {
	res := StripeReapResult{Backend: m.name}
	if olderThan <= 0 {
		olderThan = WebDAVDefaultStripeGrace
	}
	lb := m.listingBridge()
	if lb == nil {
		return res, fmt.Errorf("%s reap stripes: every bridge is down", m.name)
	}
	tenants, _, err := lb.drv.children(ctx, nil)
	if err != nil {
		return res, fmt.Errorf("%s reap stripes: list root: %w", m.name, err)
	}
	cutoff := m.now().Add(-olderThan)
	for _, t := range tenants {
		if !strings.HasPrefix(t, "t-") {
			continue
		}
		conts, _, err := lb.drv.children(ctx, []string{t})
		if err != nil {
			return res, fmt.Errorf("%s reap stripes: list %s: %w", m.name, t, err)
		}
		for _, c := range conts {
			if !strings.HasSuffix(c, webdavStripeDirSuffix) {
				continue
			}
			if err := m.reapContainer(ctx, lb, []string{t, c}, cutoff, &res); err != nil {
				return res, err
			}
		}
	}
	return res, ctx.Err()
}

func (m *MultiWebDAVDriver) listingBridge() *webdavBridge {
	for _, b := range m.bridges {
		if m.healthy(b) {
			return b
		}
	}
	return nil
}

func (m *MultiWebDAVDriver) reapContainer(ctx context.Context, lb *webdavBridge, top []string, cutoff time.Time, res *StripeReapResult) error {
	hhs, _, err := lb.drv.children(ctx, top)
	if err != nil {
		return fmt.Errorf("%s reap stripes: list %s: %w", m.name, strings.Join(top, "/"), err)
	}
	for _, hh := range hhs {
		hhDir := append(append([]string(nil), top...), hh)
		entries, _, err := lb.drv.children(ctx, hhDir)
		if err != nil {
			return fmt.Errorf("%s reap stripes: list %s: %w", m.name, strings.Join(hhDir, "/"), err)
		}
		for _, e := range entries {
			if ctx.Err() != nil {
				return nil
			}
			dir := append(append([]string(nil), hhDir...), e)
			if _, gen, ok := splitKeyGen(e); ok {
				m.reapGeneration(ctx, lb, dir, gen, cutoff, res)
				continue
			}
			if !isHex32(e) {
				res.UnknownFolders++
				continue
			}
			// A key folder of the first layout (#626): its generations, then
			// the folder once nothing is left in it. No upload writes this
			// layout any more, so an empty one is never in use.
			gens, files, err := lb.drv.children(ctx, dir)
			if err != nil {
				return fmt.Errorf("%s reap stripes: list %s: %w", m.name, strings.Join(dir, "/"), err)
			}
			left := len(gens) + len(files)
			for _, g := range gens {
				if m.reapGeneration(ctx, lb, append(append([]string(nil), dir...), g), g, cutoff, res) {
					left--
				}
			}
			if left == 0 && m.removeEmptyNames(ctx, dir) == nil {
				res.FoldersRemoved++
			}
		}
	}
	return nil
}

// reapGeneration judges one generation folder and deletes it when its name
// says it is past the grace and no manifest references it; true when the
// folder is gone. An empty one (an upload that died after its MKCOL) is a
// folder removed, not a generation reaped.
func (m *MultiWebDAVDriver) reapGeneration(ctx context.Context, lb *webdavBridge, dir []string, gen string, cutoff time.Time, res *StripeReapResult) bool {
	t, ok := stripeGenTime(gen)
	if !ok {
		res.UnknownFolders++
		return false
	}
	res.Generations++
	if t.After(cutoff) {
		res.Young++
		return false
	}
	live, err := m.generationLive(ctx, dir)
	if err != nil {
		res.fail("%s: %v", strings.Join(dir, "/"), err)
		return false
	}
	if live {
		res.Live++
		return false
	}
	n, err := m.removeGeneration(ctx, lb, dir)
	res.FilesDeleted += n
	if err != nil {
		res.fail("%s: %v", strings.Join(dir, "/"), err)
		return false
	}
	if n == 0 {
		res.FoldersRemoved++
		return true
	}
	res.Reaped++
	webdavStripeOrphans.WithLabelValues(m.name, "reaped").Inc()
	m.logger.Info("webdav stripe: orphan generation reaped", zap.String("backend", m.name),
		zap.String("dir", strings.Join(dir, "/")), zap.Int("files", n))
	return true
}

// generationLive: the manifest of the object the key file names references
// this generation. A missing key file (the upload died before writing it)
// is not live; a key file that does not belong to its folder is an error.
func (m *MultiWebDAVDriver) generationLive(ctx context.Context, dir []string) (bool, error) {
	keyNames := append(append([]string(nil), dir...), stripeKeyFile)
	rc, err := readFrom(ctx, m, "get stripe key "+strings.Join(dir, "/"), m.rank(keyNames),
		func(b *webdavBridge) (io.ReadCloser, error) {
			return b.drv.getNames(ctx, strings.Join(keyNames, "/"), keyNames, engine.ErrNotFound(dir[1], stripeKeyFile))
		},
		func(_ io.ReadCloser, err error) bool { return notFoundErr(err) })
	if notFoundErr(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	raw, err := io.ReadAll(io.LimitReader(rc, 64<<10))
	_ = rc.Close()
	if err != nil {
		return false, err
	}
	var k stripeKey
	if err := json.Unmarshal(raw, &k); err != nil {
		return false, fmt.Errorf("stripe key file: %w", err)
	}
	if strings.Contains(k.Container, "/") || davName(k.Container)+webdavStripeDirSuffix != dir[1] {
		return false, errors.New("stripe key file names another container")
	}
	h := stripeKeyHash(k.Artifact)
	owned := dir[2] == h[:2] && ((len(dir) == 4 && strings.HasPrefix(dir[3], h+"-")) || (len(dir) == 5 && dir[3] == h))
	if !owned {
		return false, errors.New("stripe key file names another object")
	}
	names := []string{dir[0], davName(k.Container)}
	segs := strings.Split(k.Artifact, "/")
	for _, s := range segs[:len(segs)-1] {
		names = append(names, davName(s))
	}
	names = append(names, leafName(segs[len(segs)-1]))
	// The manifest is asked of EVERY bridge (Prompt 2b A6): one that has not
	// seen the commit yet answers "none" for a live generation. Live when
	// any bridge's manifest names it; a bridge that cannot answer keeps the
	// generation (never a delete on doubt).
	what := strings.Join(names, "/")
	for _, b := range m.bridges {
		man, err := m.readManifestOn(ctx, b, what, names, engine.ErrNotFound(k.Container, k.Artifact))
		m.note(ctx, b, err)
		if notFoundErr(err) {
			continue
		}
		if err != nil {
			return false, fmt.Errorf("bridge %d: %w", b.idx, err)
		}
		if man.Gen == dirGen(dir) {
			return true, nil
		}
	}
	return false, nil
}

// removeGeneration deletes every file of a generation's folder (each through
// its bridge), then the folder — only once every bridge lists it empty (a
// collection DELETE is recursive: a file another bridge sees and lb does
// not would go with it, Prompt 2b A6). A folder a bridge still shows
// something in is left for the next pass.
func (m *MultiWebDAVDriver) removeGeneration(ctx context.Context, lb *webdavBridge, dir []string) (int, error) {
	_, files, err := lb.drv.children(ctx, dir)
	if err != nil {
		return 0, err
	}
	n := 0
	var errs []error
	for _, f := range files {
		if err := m.removeRouted(ctx, append(append([]string(nil), dir...), f)); err != nil {
			errs = append(errs, err)
			continue
		}
		n++
	}
	if len(errs) > 0 {
		return n, errors.Join(errs...)
	}
	if err := m.removeEmptyNames(ctx, dir); err != nil && !errors.Is(err, ErrDirNotEmpty) {
		return n, err
	}
	return n, nil
}

// defaultStagingDir is where pieces are staged without SYNC_WEBDAV_STAGING_DIR.
func defaultStagingDir() string { return filepath.Join(os.TempDir(), "vaultaire-stripes") }
