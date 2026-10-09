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
//   - a PUT that fails is checked against its folder (one Depth 1 PROPFIND,
//     cached a minute): a folder at the limit makes the failure invalid input
//     (engine.ErrInvalidInput — a 400, no breaker charge, no failover; the
//     #620 path), whatever status the server chose for it;
//   - the largest folder this driver has counted is a gauge,
//     vaultaire_webdav_folder_files_max{backend,bridge}: every Depth 1
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
)

var (
	webdavFolderFilesMax = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "vaultaire_webdav_folder_files_max",
		Help: "The most files a WebDAV backend's bridge has counted in one folder since the process started (Sync refuses writes past 50,000 per folder), by backend and bridge.",
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
	max      int
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
	if files > t.max {
		t.max = files
		webdavFolderFilesMax.WithLabelValues(d.name, d.limits.bridge).Set(float64(files))
	}
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
	t.mu.Unlock()
	if !ok || time.Since(c.at) > webdavFolderCountTTL {
		if _, _, err := d.propfind(ctx, path, "1"); err != nil {
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
	if err == nil || ctx.Err() != nil || errors.Is(err, engine.ErrInvalidInput) {
		return err
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
