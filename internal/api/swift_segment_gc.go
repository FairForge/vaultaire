package api

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/FairForge/vaultaire/internal/drivers"
	"github.com/FairForge/vaultaire/internal/engine"
	"go.uber.org/zap"
)

// swift_segment_gc reaps the piece generations of Blomp's large objects that
// no manifest references (internal/drivers/swift_segment_gc.go, RB-2): a
// crashed upload, a failed upload whose bulk delete failed, a piece PUT that
// landed after its upload's cleanup. A generation is found by its marker,
// read with a GET (Blomp's listing lags), and judged only when its newest
// piece is older than the grace (6 h); an unreadable manifest keeps it.
// Interval job (1 h, boot +11 m, 1 h ceiling), registered only when the
// `blomp` backend is.

// swiftSegmentBackend is the Swift backend whose segments the job reaps.
const swiftSegmentBackend = "blomp"

// segmentReaper is a backend that stores large objects as SLO pieces.
type segmentReaper interface {
	ReapOrphanSegments(ctx context.Context, olderThan time.Duration) (drivers.SwiftSegmentReapResult, error)
}

type swiftSegmentGC struct {
	r          segmentReaper
	Grace      time.Duration
	JobName    string
	Every      time.Duration
	BootDelay  time.Duration
	MaxRunTime time.Duration
}

// newSwiftSegmentGC is the job for the `blomp` backend, nil when it is not
// registered.
func newSwiftSegmentGC(eng *engine.CoreEngine, logger *zap.Logger) *swiftSegmentGC {
	if eng == nil {
		return nil
	}
	drv, ok := eng.GetDriver(swiftSegmentBackend)
	if !ok {
		return nil
	}
	r, ok := drv.(segmentReaper)
	if !ok {
		return nil
	}
	logger.Info("swift_segment_gc registered", zap.String("backend", swiftSegmentBackend))
	return &swiftSegmentGC{r: r, Grace: drivers.SwiftDefaultSegmentGrace, JobName: "swift_segment_gc",
		Every: time.Hour, BootDelay: 11 * time.Minute, MaxRunTime: time.Hour}
}

// spec: an error only when the tenants or a tenant's segments cannot be
// listed; a generation that could not be judged or deleted, the run's piece
// budget and its ceiling are notes.
func (g *swiftSegmentGC) spec() jobSpec {
	return jobSpec{Name: g.JobName, Every: g.Every, BootDelay: g.BootDelay, MaxRunTime: g.MaxRunTime,
		Run: func(ctx context.Context) (jobReport, error) {
			res, err := g.r.ReapOrphanSegments(ctx, g.Grace)
			rep := jobReport{Rows: int64(res.PiecesDeleted + res.MarkersDeleted), Result: res}
			var notes []string
			if errors.Is(err, context.DeadlineExceeded) {
				notes = append(notes, "stopped at the run ceiling")
				err = nil
			}
			if res.BudgetStopped {
				notes = append(notes, "stopped at the piece budget (the rest next run)")
			}
			if res.Unidentified > 0 {
				notes = append(notes, fmt.Sprintf("%d generation(s) without a marker naming their object, kept", res.Unidentified))
			}
			if n := len(res.Errors); n > 0 {
				notes = append(notes, fmt.Sprintf("%d generation(s) kept on an error, first: %s", n, res.Errors[0]))
			}
			rep.Note = strings.Join(notes, "; ")
			return rep, err
		}}
}
