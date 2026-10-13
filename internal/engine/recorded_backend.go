package engine

import (
	"context"
	"fmt"
	"slices"

	"github.com/FairForge/vaultaire/internal/common"
	"github.com/prometheus/client_golang/prometheus"
	"go.uber.org/zap"
)

// Location-authoritative calls (WP-R6-1).
//
// The row that describes an object — object_head_cache.backend_name, a
// version row's, the Smart-demotion ledger's — is the one authority for
// where its bytes are. A read asks that backend and no other: a miss there
// is a miss (404), a backend that cannot be asked (open breaker, failure, no
// driver by that name) is ErrAllBackendsUnavailable (503 + Retry-After),
// never a walk of the other registered backends — 18 round trips on prod
// for a genuine miss, and a stale copy anywhere was served as the object
// (R6-07). The caller says which backend on the context of the call
// (WithRecordedBackend); a call with none is the primary's (a row written
// before backend_name existed — routing_truth's resolve-null fixes those).
//
// A delete attempts the recorded backend AND every other backend the
// caller knows holds a copy at the same address (WithKnownCopies: the
// demotion ledger's other copy, a retired_blobs row's backend), and fails
// when any of them fails: never "deleted" while a copy may remain (R6-24).

type recordedBackendKey struct{}

type knownCopiesKey struct{}

// WithRecordedBackend names the backend the object's row records for the
// engine calls made on the returned context. "" leaves ctx unchanged.
func WithRecordedBackend(ctx context.Context, backend string) context.Context {
	if backend == "" {
		return ctx
	}
	return context.WithValue(ctx, recordedBackendKey{}, backend)
}

// RecordedBackend is the backend WithRecordedBackend named ("" when none).
func RecordedBackend(ctx context.Context) string {
	b, _ := ctx.Value(recordedBackendKey{}).(string)
	return b
}

// WithKnownCopies names the other backends a Delete must remove the
// artifact from (each is attempted; a miss is fine, a failure fails the
// delete). Names already on ctx are kept.
func WithKnownCopies(ctx context.Context, backends ...string) context.Context {
	all := slices.Clone(KnownCopies(ctx))
	for _, b := range backends {
		if b != "" && !slices.Contains(all, b) {
			all = append(all, b)
		}
	}
	return context.WithValue(ctx, knownCopiesKey{}, all)
}

// KnownCopies is what WithKnownCopies named.
func KnownCopies(ctx context.Context) []string {
	b, _ := ctx.Value(knownCopiesKey{}).([]string)
	return b
}

// ProbeTenant is the reserved tenant of the backend probes' canary object
// (t-_probe/_probe/<hostname>, internal/api/backend_canary.go). Like
// ChunkAddressTenant it starts with '_' (IsReservedTenantID).
const ProbeTenant = "_probe"

// ErrNoTenant is returned for an engine call whose context names no tenant
// (R6-22). It wraps ErrInvalidInput: a caller's bug — never a miss, never a
// breaker charge — that fails loudly. It used to resolve to the tenant
// "default": a shared prefix on the container-keyed backends, t-default/ on
// the fixed-bucket ones (refused there since WP-R8-7). The chunk container
// and the backend probes are no exception: their calls name their reserved
// tenant (ChunkContext, ProbeTenant).
var ErrNoTenant = fmt.Errorf("%w: the engine call's context names no tenant (common.WithTenantID; engine.ChunkContext for a chunk blob)", ErrInvalidInput)

// noTenantOps are the ops vaultaire_engine_no_tenant_refusals_total counts.
var noTenantOps = []string{"put", "get", "get_range", "delete", "exists"}

var noTenantRefusals = func() *prometheus.CounterVec {
	c := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "vaultaire_engine_no_tenant_refusals_total",
		Help: "Engine calls refused because their context named no tenant (ErrNoTenant; they used to run as tenant \"default\"), by op (put, get, get_range, delete, exists). Must stay 0: every increase is a caller's bug with an Error line.",
	}, []string{"op"})
	for _, op := range noTenantOps {
		c.WithLabelValues(op)
	}
	return c
}()

// NoTenantRefusalsCounter is vaultaire_engine_no_tenant_refusals_total
// (tests read it).
func NoTenantRefusalsCounter() *prometheus.CounterVec { return noTenantRefusals }

// requireTenant refuses a call whose context names no tenant.
func (e *CoreEngine) requireTenant(ctx context.Context, op, container, artifact string) error {
	if common.GetTenantID(ctx) != "" {
		return nil
	}
	noTenantRefusals.WithLabelValues(op).Inc()
	e.logger.Error("engine call refused: its context names no tenant",
		zap.String("op", op), zap.String("container", container), zap.String("artifact", artifact))
	return fmt.Errorf("%s %s/%s: %w", op, container, artifact, ErrNoTenant)
}

// unregisteredBackend logs (and, for a strict target, counts) a call routed
// to a backend no driver is registered under: 503, never another backend.
func (e *CoreEngine) unregisteredBackend(name, op string) error {
	if strictTarget(name) {
		strictUnregistered.WithLabelValues(name, op).Inc()
	}
	e.logger.Error("call routed to a backend that is not registered on this process — answered 503, never another backend",
		zap.String("backend", name), zap.String("op", op))
	return fmt.Errorf("%w: %w: %q", ErrAllBackendsUnavailable, ErrBackendNotRegistered, name)
}
