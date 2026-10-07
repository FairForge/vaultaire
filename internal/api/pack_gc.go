package api

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/FairForge/vaultaire/internal/engine"
	"github.com/FairForge/vaultaire/internal/packstore"
	"go.uber.org/zap"
)

// The pack store's garbage collector (internal/packstore, migration 080,
// Phase 37). The pack store batches small immutable objects into large,
// content-addressed packs on a slow per-file backend; its first backend is
// `sync` (Sync.com's WebDAV bridges). pack_gc is an interval job (1 h, boot
// +7 m, 2 h ceiling): finish retired packs, expire uploads whose commit never
// happened, delete orphan pack files, rewrite packs whose live bytes fell
// under half, that hold a tombstone older than 30 days, or whose member rows
// the account erasure deleted (packstore.GC). Registered only when the
// backend is. Nothing writes packs yet — the Vault parity leg that will is
// designed in internal/packstore/CLAUDE.md, not wired.

// packStoreBackend is the backend the server's pack store writes to.
const packStoreBackend = "sync"

type packGC struct {
	store      *packstore.Store
	JobName    string
	Every      time.Duration
	BootDelay  time.Duration
	MaxRunTime time.Duration
}

// newPackGC returns the pack store's GC job for this process, or nil when
// there is no database or no `sync` backend.
func newPackGC(db *sql.DB, eng *engine.CoreEngine, stagingDir string, logger *zap.Logger) *packGC {
	return newPackGCFor(db, eng, packStoreBackend, stagingDir, logger)
}

func newPackGCFor(db *sql.DB, eng *engine.CoreEngine, backend, stagingDir string, logger *zap.Logger) *packGC {
	if db == nil || eng == nil {
		return nil
	}
	drv, ok := eng.GetDriver(backend)
	if !ok {
		return nil
	}
	if stagingDir == "" {
		stagingDir = filepath.Join(os.TempDir(), "vaultaire-packs")
	}
	st, err := packstore.New(db, drv, packstore.Options{BackendName: backend, StagingDir: stagingDir, Logger: logger})
	if err != nil {
		logger.Error("pack store not available", zap.String("backend", backend), zap.Error(err))
		return nil
	}
	return &packGC{store: st, JobName: "pack_gc", Every: time.Hour, BootDelay: 7 * time.Minute, MaxRunTime: 2 * time.Hour}
}

// spec is the job (WP-R13-3 rules): an error only when the index cannot be
// read; one pack that could not be handled is a note.
func (g *packGC) spec() jobSpec {
	return jobSpec{Name: g.JobName, Every: g.Every, BootDelay: g.BootDelay, MaxRunTime: g.MaxRunTime,
		Run: func(ctx context.Context) (jobReport, error) {
			res, err := g.store.GC(ctx)
			rep := jobReport{
				Rows:   int64(res.RetiredFinished + res.OrphansDeleted + res.PacksRewritten + res.PacksDeleted),
				Result: res,
			}
			var notes []string
			if errors.Is(err, context.DeadlineExceeded) {
				notes = append(notes, "stopped at the run ceiling")
				err = nil
			}
			if n := len(res.Errors); n > 0 {
				notes = append(notes, fmt.Sprintf("%d item(s) failed, first: %s", n, res.Errors[0]))
			}
			rep.Note = strings.Join(notes, "; ")
			return rep, err
		}}
}
