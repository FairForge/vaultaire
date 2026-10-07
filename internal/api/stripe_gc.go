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

// stripe_gc reaps the piece generations of striped objects that no manifest
// references (internal/drivers/webdav_stripe.go): an upload that failed and
// could not delete what it had written, an overwrite whose old pieces could
// not be deleted. Generation ages come from their names (Sync's bridges
// report a 1970 modtime for every file); only generations older than the
// grace (6 h — a striped upload never runs that long) are candidates, and a
// generation whose manifest cannot be read is kept. Interval job (1 h, boot
// +9 m, 1 h ceiling), registered only when the `sync` backend can stripe.

// stripeReaper is a backend that stripes large objects.
type stripeReaper interface {
	ReapOrphanStripes(ctx context.Context, olderThan time.Duration) (drivers.StripeReapResult, error)
}

type stripeGC struct {
	r          stripeReaper
	Grace      time.Duration
	JobName    string
	Every      time.Duration
	BootDelay  time.Duration
	MaxRunTime time.Duration
}

// newStripeGC is the job for the `sync` backend, nil when it is not
// registered or cannot stripe.
func newStripeGC(eng *engine.CoreEngine, logger *zap.Logger) *stripeGC {
	if eng == nil {
		return nil
	}
	drv, ok := eng.GetDriver(packStoreBackend)
	if !ok {
		return nil
	}
	r, ok := drv.(stripeReaper)
	if !ok {
		return nil
	}
	logger.Info("stripe_gc registered", zap.String("backend", packStoreBackend))
	return &stripeGC{r: r, Grace: drivers.WebDAVDefaultStripeGrace, JobName: "stripe_gc",
		Every: time.Hour, BootDelay: 9 * time.Minute, MaxRunTime: time.Hour}
}

// spec: an error only when the stripe folders cannot be listed; a
// generation that could not be judged or deleted is a note.
func (g *stripeGC) spec() jobSpec {
	return jobSpec{Name: g.JobName, Every: g.Every, BootDelay: g.BootDelay, MaxRunTime: g.MaxRunTime,
		Run: func(ctx context.Context) (jobReport, error) {
			res, err := g.r.ReapOrphanStripes(ctx, g.Grace)
			rep := jobReport{Rows: int64(res.Reaped), Result: res}
			var notes []string
			if errors.Is(err, context.DeadlineExceeded) {
				notes = append(notes, "stopped at the run ceiling")
				err = nil
			}
			if n := len(res.Errors); n > 0 {
				notes = append(notes, fmt.Sprintf("%d generation(s) kept on an error, first: %s", n, res.Errors[0]))
			}
			rep.Note = strings.Join(notes, "; ")
			return rep, err
		}}
}
