package engine

import "github.com/prometheus/client_golang/prometheus"

// partialUnavailable counts the calls a backend answered with
// ErrPartiallyUnavailable: the part of it that holds the object is out
// while the rest answers (one Sync bridge of five). Never charged to the
// breaker, so without this a bridge out for hours shows only as 503s
// (Prompt 2b.3 D1.2).
var partialUnavailable = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "vaultaire_engine_partial_unavailable_total",
	Help: "Engine calls a backend answered as partially unavailable (part of the backend that holds the object is out, the rest answers; a 503, never a breaker charge), by backend and op (get, get_range, put, delete, other).",
}, []string{"backend", "op"})

// fallover counts the times ExecuteOp walked past a backend that failed (or
// whose breaker was open) to ask the next candidate: the request was then
// served — or written — by a backend other than the one it was routed to
// (R6-04). A target-only strict backend (`sync`) never appears as `from`
// (Prompt 2b.4 E1.1).
var fallover = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "vaultaire_engine_fallover_total",
	Help: "Engine calls that moved on from a failed backend (or one whose breaker was open) to the next candidate, by the backend that failed, the one asked next, and op (get, get_range, put, delete, other).",
}, []string{"from", "to", "op"})

// Collectors are the engine's metrics, for the server's registry.
func Collectors() []prometheus.Collector {
	return []prometheus.Collector{partialUnavailable, fallover}
}

// PartialUnavailableCounter is vaultaire_engine_partial_unavailable_total
// (tests read it).
func PartialUnavailableCounter() *prometheus.CounterVec { return partialUnavailable }

// FalloverCounter is vaultaire_engine_fallover_total (tests read it).
func FalloverCounter() *prometheus.CounterVec { return fallover }
