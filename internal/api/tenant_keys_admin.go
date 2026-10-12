package api

import (
	"context"
	"database/sql"
	"net/http"
	"sync"
	"time"

	"github.com/FairForge/vaultaire/internal/audit"
	"github.com/FairForge/vaultaire/internal/crypto"
	"github.com/go-chi/chi/v5"
	"github.com/prometheus/client_golang/prometheus"
	"go.uber.org/zap"
)

// Tenant key versions (WP-R8-1): the boot check of the wrapped keys, the
// rotate endpoint, and the backlog a rotation leaves.

var (
	tenantKeysRewrapped = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "vaultaire_tenant_keys_rewrapped_total",
		Help: "Tenant key versions re-wrapped from ENCRYPTION_MASTER_KEY_PREVIOUS to ENCRYPTION_MASTER_KEY at boot (a master re-key; no data is re-encrypted).",
	})
	tenantKeysUnwrapFailures = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "vaultaire_tenant_keys_unwrap_failures",
		Help: "Tenant key versions that open under neither the configured master key nor the previous one, found at boot. Their tenants' encrypted reads answer 500 (never ciphertext) until the right key is configured.",
	})
)

// tenantKeysBootTimeout bounds the boot pass (one row per tenant version).
const tenantKeysBootTimeout = 2 * time.Minute

// checkTenantKeysAtBoot runs the key store's re-wrap pass: with
// ENCRYPTION_MASTER_KEY_PREVIOUS set, every row that opens only under the
// previous key is re-wrapped under the current one; without it, the pass
// only checks that every row opens. One Info line with the counts; one
// Error line naming the rows that open under neither key.
func checkTenantKeysAtBoot(ctx context.Context, keys *crypto.TenantKeys, logger *zap.Logger) {
	ctx, cancel := context.WithTimeout(ctx, tenantKeysBootTimeout)
	defer cancel()
	res, err := keys.RewrapAll(ctx)
	if err != nil {
		logger.Error("tenant keys: boot check could not read the key table", zap.Error(err))
		return
	}
	tenantKeysRewrapped.Add(float64(res.Rewrapped))
	tenantKeysUnwrapFailures.Set(float64(len(res.Failed)))
	if keys.HasPrevious() {
		logger.Info("tenant keys re-wrapped under the new master key — remove ENCRYPTION_MASTER_KEY_PREVIOUS once every slot has booted with it",
			zap.Int("rows", res.Rows), zap.Int("rewrapped", res.Rewrapped), zap.Int("rewrap_errors", res.RewrapErrored),
			zap.Int("unwrap_failures", len(res.Failed)))
	} else {
		logger.Info("tenant keys checked", zap.Int("rows", res.Rows), zap.Int("unwrap_failures", len(res.Failed)))
	}
	if len(res.Failed) > 0 {
		shown := res.Failed
		if len(shown) > 50 {
			shown = shown[:50]
		}
		logger.Error("tenant keys that open under no configured master key: these tenants' encrypted objects answer 500 until the right ENCRYPTION_MASTER_KEY (or _PREVIOUS) is set",
			zap.Int("count", len(res.Failed)), zap.Strings("first", shown))
	}
}

// handleRotateTenantKey is POST /api/v1/admin/tenants/{tenant_id}/encryption-key/rotate.
// New writes use the new version at once on this process and within a
// minute on every other; every blob keeps opening under the version it
// records, and the key_reencrypt job moves the tenant's chunks over. Whole
// SSE-S3 objects stay on their version until WP-R2-1.
func (s *Server) handleRotateTenantKey(w http.ResponseWriter, r *http.Request) {
	if s.tenantKeys == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "encryption_disabled",
			"message": "ENCRYPTION_MASTER_KEY is not set on this server: there are no tenant keys to rotate"})
		return
	}
	tenantID := chi.URLParam(r, "tenant_id")
	var exists bool
	if err := s.db.QueryRowContext(r.Context(), `SELECT EXISTS (SELECT 1 FROM tenants WHERE id = $1)`, tenantID).Scan(&exists); err != nil {
		s.logger.Error("rotate tenant key: tenant lookup", zap.Error(err))
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal_error"})
		return
	}
	if !exists {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "tenant_not_found"})
		return
	}

	version, err := s.tenantKeys.Rotate(r.Context(), tenantID)
	actor, _ := r.Context().Value(userIDKey).(string)
	audit.Record(r.Context(), s.db, audit.Entry{UserID: actor, TenantID: tenantID, EventType: "admin",
		Action: "admin.key_rotate", Error: err, Metadata: map[string]any{"key_version": version}})
	if err != nil {
		s.logger.Error("rotate tenant key", zap.String("tenant_id", tenantID), zap.Error(err))
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal_error"})
		return
	}
	s.logger.Info("tenant key rotated", zap.String("tenant_id", tenantID), zap.Int("key_version", version))
	writeJSON(w, http.StatusOK, map[string]any{"tenant_id": tenantID, "key_version": version})
}

// keyRotationPendingCollector exports vaultaire_key_rotation_pending{kind}:
// what a rotation has left on an older version, read from the tables on
// scrape (15 s cache — the R13 lesson: never an in-process value).
//
//	chunk: encrypted index rows below their tenant's newest version (the
//	       key_reencrypt job's backlog)
//	whole: SSE-S3 objects below it (re-encrypted with WP-R2-1; until then
//	       this only falls when the objects are overwritten or deleted)
type keyRotationPendingCollector struct {
	db     *sql.DB
	logger *zap.Logger
	desc   *prometheus.Desc

	mu     sync.Mutex
	at     time.Time
	counts map[string]float64
}

const keyRotationPendingTTL = 15 * time.Second

func newKeyRotationPendingCollector(db *sql.DB, logger *zap.Logger) *keyRotationPendingCollector {
	return &keyRotationPendingCollector{db: db, logger: logger,
		desc: prometheus.NewDesc("vaultaire_key_rotation_pending",
			"Encrypted data still sealed under an older version of its tenant's key, by kind: chunk (the key_reencrypt job's backlog) | whole (SSE-S3 objects; re-encrypted with WP-R2-1). Read from the tables.",
			[]string{"kind"}, nil)}
}

func (c *keyRotationPendingCollector) Describe(ch chan<- *prometheus.Desc) { ch <- c.desc }

func (c *keyRotationPendingCollector) Collect(ch chan<- prometheus.Metric) {
	counts := c.read()
	if counts == nil {
		return // never read: unknown, not zero
	}
	for _, kind := range []string{"chunk", "whole"} {
		ch <- prometheus.MustNewConstMetric(c.desc, prometheus.GaugeValue, counts[kind], kind)
	}
}

// keyRotationPendingSQL counts both backlogs in one statement.
const keyRotationPendingSQL = `
	WITH cur AS (
		SELECT tenant_id, MAX(key_version) AS v FROM tenant_key_versions GROUP BY tenant_id HAVING COUNT(*) > 1
	)
	SELECT
		(SELECT COUNT(*) FROM global_content_index g JOIN cur ON cur.tenant_id = g.dedup_scope
		  WHERE g.encrypted AND g.encryption_key_version < cur.v),
		(SELECT COUNT(*) FROM object_head_cache h JOIN cur ON cur.tenant_id = h.tenant_id
		  WHERE h.encryption_algorithm = $1 AND COALESCE(h.encryption_key_version, 1) < cur.v)`

func (c *keyRotationPendingCollector) read() map[string]float64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.db == nil || (c.counts != nil && time.Since(c.at) < keyRotationPendingTTL) {
		return c.counts
	}
	c.at = time.Now() // stamped on failure too: a dead database costs one query per TTL
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	var chunk, whole int64
	if err := c.db.QueryRowContext(ctx, keyRotationPendingSQL, crypto.SSEAlgorithm).Scan(&chunk, &whole); err != nil {
		if c.logger != nil {
			c.logger.Warn("key rotation metrics: could not read the backlog", zap.Error(err))
		}
		return c.counts
	}
	c.counts = map[string]float64{"chunk": float64(chunk), "whole": float64(whole)}
	return c.counts
}
