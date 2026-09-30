package api

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"time"
)

// Single-flight for the background jobs that also have an admin trigger
// (Review R13-05 / WP-R11-8). The deploy is stop→swap→start on one box, so
// two binaries never overlap; inside one process the ticker and
// POST /api/v1/admin/{dedup-gc,smart-demotion,quota-reconcile} could, and
// an admin double-click ran two global jobs at once. A run that finds the
// gate held is refused, never queued: the ticker logs and waits for its
// next tick, the trigger answers 409 already_running.

// errJobAlreadyRunning is returned by a guarded run when another run of the
// same job holds the gate.
var errJobAlreadyRunning = errors.New("job already running")

// jobGate is a non-blocking mutex around one job's RunOnce.
type jobGate struct{ mu sync.Mutex }

// tryAcquire takes the gate without blocking; the caller must release() when
// it succeeds.
func (g *jobGate) tryAcquire() bool { return g.mu.TryLock() }

// release gives the gate back.
func (g *jobGate) release() { g.mu.Unlock() }

// adminTriggerTimeout bounds a manually triggered run. The cycle runs on a
// context detached from the request (Review R13-08): HAProxy's server
// timeout or a closed admin tab used to cancel a half-done move between the
// cold write and the routing flip, leaving a cold copy the next run could
// not see.
const adminTriggerTimeout = 6 * time.Hour

// adminTriggerContext returns the detached, bounded context a manual run
// executes on. Request values (actor, audit request info) are kept.
func adminTriggerContext(r *http.Request) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(r.Context()), adminTriggerTimeout)
}

// writeJobAlreadyRunning answers the admin trigger when the gate is held.
func writeJobAlreadyRunning(w http.ResponseWriter, job string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusConflict)
	_, _ = w.Write([]byte(`{"error":"already_running","job":"` + job + `"}`))
}
