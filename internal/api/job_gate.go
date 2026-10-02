package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sync"
	"time"
)

// Single-flight for the admin-triggerable work (Review R13-05 / WP-R11-8). A
// run that finds another one in progress is refused, never queued: the
// trigger answers 409 already_running. The background jobs take ONE pg
// advisory lock per job in the scheduler (jobs.go, WP-R13-3), which also
// covers a second process; jobGate is the in-process form, still used by
// POST /api/v1/admin/quota-reconcile (not a scheduled job).

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
// The body is encoded, never concatenated: job is a server-side name today,
// and must stay harmless if a caller ever passes anything else.
func writeJobAlreadyRunning(w http.ResponseWriter, job string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusConflict)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": "already_running", "job": job})
}
