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

// Collectors are the engine's metrics, for the server's registry.
func Collectors() []prometheus.Collector { return []prometheus.Collector{partialUnavailable} }

// PartialUnavailableCounter is vaultaire_engine_partial_unavailable_total
// (tests read it).
func PartialUnavailableCounter() *prometheus.CounterVec { return partialUnavailable }
