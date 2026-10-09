package drivers

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"go.uber.org/zap"

	"github.com/FairForge/vaultaire/internal/engine"
)

// Sync holds at most 50,000 files per folder (help article 38275627279379;
// folders unlimited). The driver mirrors a key's `/` segments into folders,
// so a flat bucket past 50,000 keys has its writes refused — and a refusal
// counted as a bridge failure took the bridge down for every tenant (and a
// write failed over to another backend). Prompt 2b A5, the guard only:
//
//   - a PUT the server REFUSED (an HTTP status error — never a timeout, a
//     stall, a refused connection or the caller's body) is checked against
//     its folder (one Depth 1 PROPFIND under the idle watchdog, the count
//     cached a minute, a failed count 30 s): a folder at the limit makes the
//     failure invalid input (engine.ErrInvalidInput — a 400, no breaker
//     charge, no failover; the #620 path), whatever status the server chose
//     for it. A PUT that failed any other way says nothing about the folder,
//     and the check used to add a PROPFIND (60 s on a stalled bridge, three
//     attempts) to each one, with the key's lock held (Prompt 2b.2 C1);
//   - the largest folder among those this driver last counted is a gauge,
//     vaultaire_webdav_folder_files_max{backend,bridge} (a recount after a
//     cleanup lowers it — it used to only rise until a restart): every Depth 1
//     listing counts, and a folder is counted again every 1,000 PUTs into it
//     (one PROPFIND per 1,000 writes) so a growing flat bucket shows up
//     before it hits the wall; the SyncFolderNearFileLimit rule warns at
//     40,000.
//
// The layout fix (a hash fan-out level per container) is not built here.

const (
	// WebDAVFolderFileLimit is Sync's hard limit of files per folder.
	WebDAVFolderFileLimit = 50_000
	// webdavFolderRecountEvery: PUTs into one folder between two counts.
	webdavFolderRecountEvery = 1000
	// webdavFolderCountTTL: how long a count answers the failure check.
	webdavFolderCountTTL = time.Minute
	// webdavFolderTrackMax bounds the folders tracked (the map is reset past it).
	webdavFolderTrackMax = 20_000
	// webdavFolderFailTTL: how long a failed count is not tried again.
	webdavFolderFailTTL = 30 * time.Second
)

var (
	webdavFolderFilesMax = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "vaultaire_webdav_folder_files_max",
		Help: "The most files in one folder among the folders a WebDAV backend's bridge last counted (a recount lowers it; Sync refuses writes past 50,000 per folder), by backend and bridge.",
	}, []string{"backend", "bridge"})
	webdavFolderFull = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "vaultaire_webdav_folder_full_total",
		Help: "PUTs refused because their folder holds Sync's 50,000-file limit, answered as invalid input (400) instead of a bridge failure, by backend.",
	}, []string{"backend"})
)

type folderCount struct {
	files int
	at    time.Time
}

// folderTracker counts files per folder (escaped collection path).
type folderTracker struct {
	mu       sync.Mutex
	puts     map[string]int
	counts   map[string]folderCount
	failed   map[string]time.Time // a count that failed, by folder
	max      int
	maxPath  string      // the folder holding max
	counting atomic.Bool // one background count at a time
}

func (d *WebDAVDriver) folderLimit() int {
	if d.folderFileLimit > 0 {
		return d.folderFileLimit
	}
	return WebDAVFolderFileLimit
}

// recordFolder notes a Depth 1 listing of the collection at path.
func (d *WebDAVDriver) recordFolder(path string, files int) {
	t := &d.folders
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.counts == nil || len(t.counts) > webdavFolderTrackMax {
		t.counts = map[string]folderCount{}
	}
	t.counts[path] = folderCount{files: files, at: time.Now()}
	switch {
	case files >= t.max:
		t.max, t.maxPath = files, path
	case path == t.maxPath:
		// The largest folder shrank (a cleanup): the largest is now
		// whichever counted folder holds the most.
		t.max, t.maxPath = 0, ""
		for p, c := range t.counts {
			if c.files > t.max || t.maxPath == "" {
				t.max, t.maxPath = c.files, p
			}
		}
	default:
		return
	}
	webdavFolderFilesMax.WithLabelValues(d.name, d.limits.bridge).Set(float64(t.max))
}

// notePut counts a successful PUT into the collection at path; every
// webdavFolderRecountEvery-th one counts the folder in the background.
func (d *WebDAVDriver) notePut(path string) {
	t := &d.folders
	t.mu.Lock()
	if t.puts == nil || len(t.puts) > webdavFolderTrackMax {
		t.puts = map[string]int{}
	}
	t.puts[path]++
	due := t.puts[path]%webdavFolderRecountEvery == 0
	t.mu.Unlock()
	if !due || !t.counting.CompareAndSwap(false, true) {
		return
	}
	go func() {
		defer t.counting.Store(false)
		ctx, cancel := context.WithTimeout(context.Background(), 2*webdavMetaTimeout)
		defer cancel()
		if _, _, err := d.propfind(ctx, path, "1"); err != nil {
			d.logger.Debug("webdav folder count failed", zap.String("backend", d.name), zap.String("folder", path), zap.Error(err))
		}
	}()
}

// folderAtLimit reports whether the collection at path holds the limit (a
// count at most webdavFolderCountTTL old, else one PROPFIND).
func (d *WebDAVDriver) folderAtLimit(ctx context.Context, path string) (int, bool) {
	t := &d.folders
	t.mu.Lock()
	c, ok := t.counts[path]
	failedAt, failed := t.failed[path]
	t.mu.Unlock()
	if !ok || time.Since(c.at) > webdavFolderCountTTL {
		if failed && time.Since(failedAt) < webdavFolderFailTTL {
			return 0, false
		}
		if _, _, err := d.propfind(ctx, path, "1"); err != nil {
			t.mu.Lock()
			if t.failed == nil || len(t.failed) > webdavFolderTrackMax {
				t.failed = map[string]time.Time{}
			}
			t.failed[path] = time.Now()
			t.mu.Unlock()
			return 0, false
		}
		t.mu.Lock()
		c = t.counts[path]
		t.mu.Unlock()
	}
	return c.files, c.files >= d.folderLimit()
}

// fullFolderErr turns a failed PUT into invalid input when its folder is at
// the limit; otherwise it returns err.
func (d *WebDAVDriver) fullFolderErr(ctx context.Context, key, folder string, err error) error {
	var se *webdavStatusError
	if err == nil || ctx.Err() != nil || errors.Is(err, engine.ErrInvalidInput) || !errors.As(err, &se) {
		return err // only a refusal by the server can be a full folder
	}
	n, full := d.folderAtLimit(ctx, folder)
	if !full {
		return err
	}
	webdavFolderFull.WithLabelValues(d.name).Inc()
	d.logger.Warn("webdav PUT refused: its folder holds the server's file limit",
		zap.String("backend", d.name), zap.String("key", key), zap.Int("files", n), zap.Int("limit", d.folderLimit()))
	return fmt.Errorf("%w: %s put %s: the folder holds %d files, the server's limit is %d per folder (%s)",
		engine.ErrInvalidInput, d.name, key, n, d.folderLimit(), err.Error())
}
