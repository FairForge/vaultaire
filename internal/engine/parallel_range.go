package engine

import (
	"context"

	"github.com/FairForge/vaultaire/internal/common"
)

// preferredBackend is the backend Get would ask first for an object: the
// recorded route (HintBackend / Put), then object_locations, then the
// primary.
func (e *CoreEngine) preferredBackend(ctx context.Context, container, artifact string) string {
	preferred := e.GetPrimary()
	if v, ok := e.objectBackends.Load(objectKey(container, artifact)); ok {
		if name, ok := v.(string); ok && name != "" {
			preferred = name
		}
	} else if e.locations != nil {
		if name, err := e.locations.LookupBackend(ctx, common.GetTenantID(ctx), container, artifact); err == nil && name != "" {
			preferred = name
			e.objectBackends.Store(objectKey(container, artifact), name)
		}
	}
	return preferred
}

// ParallelRangeSource is where a parallel ranged GET of one object reads
// from: the backend Get would ask first, when its driver implements
// VersionedRangeGetter and its breaker is closed. ok=false means the
// caller takes the single-stream Get (with its failover). The ranges are
// read from the driver directly; the caller reports the whole read as ONE
// outcome with RecordReadOutcome, so 64 ranges are one logical operation to
// the breaker, as the single GET they replace was.
func (e *CoreEngine) ParallelRangeSource(ctx context.Context, container, artifact string) (string, VersionedRangeGetter, bool) {
	name := e.preferredBackend(ctx, container, artifact)
	e.mu.RLock()
	d, exists := e.drivers[name]
	e.mu.RUnlock()
	if !exists {
		return "", nil, false
	}
	rg, ok := d.(VersionedRangeGetter)
	if !ok {
		return "", nil, false
	}
	e.failover.mu.RLock()
	b := e.failover.breakers[name]
	e.failover.mu.RUnlock()
	if b == nil || b.State() != StateClosed {
		return "", nil, false
	}
	return name, rg, true
}

// RecordReadOutcome charges one read's outcome to a backend's breaker, with
// the failover manager's rules: a backend failure counts, a client-level
// outcome (cancelled, not found, …) does not, a success closes it.
func (e *CoreEngine) RecordReadOutcome(backend string, err error) {
	e.failover.mu.RLock()
	b := e.failover.breakers[backend]
	e.failover.mu.RUnlock()
	if b == nil {
		return
	}
	switch {
	case err == nil:
		b.RecordSuccess()
	case isBackendFailure(err):
		b.RecordFailure()
	}
}
