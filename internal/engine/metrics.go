package engine

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

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

// strictUnregistered counts calls routed to a strict target (`sync`) that no
// driver is registered under on this process: each is a 503, never another
// backend's (possibly stale) copy (Prompt 2b.5 F3.1).
var strictUnregistered = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "vaultaire_engine_strict_target_unregistered_total",
	Help: "Engine calls routed to a strict backend (sync) that is not registered on this process, answered 503, by backend and op (get, get_range, put, delete).",
}, []string{"backend", "op"})

// Collectors are the engine's metrics, for the server's registry.
func Collectors() []prometheus.Collector {
	return []prometheus.Collector{partialUnavailable, fallover, strictUnregistered, noTenantRefusals, unrecordedBackendCalls, driverOpSeconds}
}

// StrictUnregisteredCounter is vaultaire_engine_strict_target_unregistered_total.
func StrictUnregisteredCounter() *prometheus.CounterVec { return strictUnregistered }

// PartialUnavailableCounter is vaultaire_engine_partial_unavailable_total
// (tests read it).
func PartialUnavailableCounter() *prometheus.CounterVec { return partialUnavailable }

// FalloverCounter is vaultaire_engine_fallover_total (tests read it).
func FalloverCounter() *prometheus.CounterVec { return fallover }

// driverOpSeconds is how long each driver call the engine makes takes
// (WP-R6-3, RP8-10): one observation per call, success or failure. On
// 2026-10-17 a 60 s mode on the Lyve primary was found by a probe, not by a
// dashboard. A GET is timed to its first byte (the driver returns its
// reader once the backend answers; the body then streams at the client's
// pace); a PUT without the time the driver spent waiting on the request
// body (sizeTrackingReader.waited — a slow uploader is not a slow backend);
// head = Exists, probe = HealthCheck. A call refused before the driver is
// asked (an open breaker, an unregistered name) is not observed.
var driverOpSeconds = prometheus.NewHistogramVec(prometheus.HistogramOpts{
	Name:    "vaultaire_driver_op_seconds",
	Help:    "Driver calls made through the engine, by driver (the registered backend name) and op (head, get_first_byte, put, delete, list, probe): a GET to its first byte, a PUT without the time spent waiting on the request body.",
	Buckets: []float64{0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 15, 25, 30, 60, 120},
}, []string{"driver", "op"})

// The op labels of driverOpSeconds.
const (
	opHead         = "head"
	opGetFirstByte = "get_first_byte"
	opPut          = "put"
	opDelete       = "delete"
	opList         = "list"
	opProbe        = "probe"
)

// observeDriverOp records one driver call that started at start, less
// waited (time the call spent waiting on its source, not the backend).
func observeDriverOp(driver, op string, start time.Time, waited time.Duration) {
	driverOpSeconds.WithLabelValues(driver, op).Observe(max(time.Since(start)-waited, 0).Seconds())
}

// InitDriverOpSeries creates a backend's latency series at zero samples so a
// dashboard lists it before its first call.
func InitDriverOpSeries(driver string) {
	for _, op := range []string{opHead, opGetFirstByte, opPut, opDelete, opList, opProbe} {
		driverOpSeconds.WithLabelValues(driver, op)
	}
}
