package engine

import (
	"context"

	"go.uber.org/zap"
)

// preferredBackend is the backend Get asks for an object (and the first a
// Delete attempts): the row's backend on the context (WithRecordedBackend),
// else the primary — a row with no backend on record (WP-R6-1). The
// fallback is counted (vaultaire_engine_unrecorded_backend_calls_total,
// Stage 1a Prompt 6 Part 0): every caller of an existing object names its
// row's backend, so a call that names none is a NULL backend_name row or a
// caller that dropped it on the way.
func (e *CoreEngine) preferredBackend(ctx context.Context, op, container, artifact string) string {
	if b := RecordedBackend(ctx); b != "" {
		return b
	}
	unrecordedBackendCalls.WithLabelValues(op).Inc()
	e.logger.Warn("engine call names no recorded backend — asked the primary (a NULL backend_name row, or a caller that dropped the row's backend)",
		zap.String("op", op), zap.String("container", container), zap.String("artifact", artifact))
	return e.GetPrimary()
}

// ParallelRangeSource is where a parallel ranged GET of one object reads
// from: the backend Get would ask first, when its driver implements
// VersionedRangeGetter and its breaker is closed. ok=false means the
// caller takes the single-stream Get (with its failover). The ranges are
// read from the driver directly; the caller reports the whole read as ONE
// outcome with RecordReadOutcome, so 64 ranges are one logical operation to
// the breaker, as the single GET they replace was.
func (e *CoreEngine) ParallelRangeSource(ctx context.Context, container, artifact string) (string, VersionedRangeGetter, bool) {
	name := e.preferredBackend(ctx, "parallel_get", container, artifact)
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
