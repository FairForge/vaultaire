package packstore

import "github.com/prometheus/client_golang/prometheus"

// The pack store's metrics, registered on the server registry through
// Collectors() (internal/api/prom_metrics.go). One label: the backend.
var (
	packsSealed = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "vaultaire_packstore_packs_sealed_total",
		Help: "Packs uploaded and committed (writers and GC rewrites).",
	}, []string{"backend"})
	packBytes = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "vaultaire_packstore_pack_bytes_total",
		Help: "Bytes of the packs uploaded and committed.",
	}, []string{"backend"})
	packMembers = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "vaultaire_packstore_members_total",
		Help: "Members recorded by a pack commit.",
	}, []string{"backend"})
	gcRewrites = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "vaultaire_packstore_gc_rewrites_total",
		Help: "Packs whose live members GC rewrote into a new pack (compaction, tombstone age, erased rows).",
	}, []string{"backend"})
	gcPacksDeleted = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "vaultaire_packstore_gc_packs_deleted_total",
		Help: "Committed packs GC deleted from the backend once no live member was left in them.",
	}, []string{"backend"})
	orphansDeleted = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "vaultaire_packstore_orphans_deleted_total",
		Help: "Pack files GC deleted that no commit recorded: uploads whose commit never happened (expired intents) and files no row names.",
	}, []string{"backend"})
	memberReadSeconds = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "vaultaire_packstore_member_read_seconds",
		Help:    "Time to open a member read on the backend (index lookup + the ranged request).",
		Buckets: []float64{0.01, 0.05, 0.1, 0.25, 0.5, 1, 2, 5, 10, 30},
	}, []string{"backend"})
	memberCorrupt = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "vaultaire_packstore_member_corrupt_total",
		Help: "Member reads whose bytes did not match the recorded sha256 or length.",
	}, []string{"backend"})
)

// Collectors returns the package's metrics for the server registry.
func Collectors() []prometheus.Collector {
	return []prometheus.Collector{packsSealed, packBytes, packMembers, gcRewrites, gcPacksDeleted,
		orphansDeleted, memberReadSeconds, memberCorrupt}
}

// initSeries makes every series of a backend exist at 0.
func initSeries(backend string) {
	for _, c := range []*prometheus.CounterVec{packsSealed, packBytes, packMembers, gcRewrites, gcPacksDeleted, orphansDeleted, memberCorrupt} {
		c.WithLabelValues(backend)
	}
}
