package api

// The data-plane host registry (WP-EDGE-3, Stage 1a Prompt 47; migration
// 112 data_plane_hosts): the hosts that move customer bytes for the
// control plane — the Worker on s3.stored.cloud (edge), the origin itself
// (slc), and data nodes (node: cmd/vaultaire with VAULTAIRE_ROLE=node).
// Mounted under /api/v1/admin (admin JWT):
//
//	GET    /data-plane-hosts/{host} — the row (never its token or hash)
//	PUT    /data-plane-hosts/{host} — {kind, address, tenant_id, weight, health}
//	DELETE /data-plane-hosts/{host}
//
// A node's token is minted on its FIRST PUT (32 random bytes, hex), stored
// as its sha256 and answered once, in that response only; a lost token is
// a DELETE and a new PUT. Every write is audited (data_plane.host_set /
// data_plane.host_deleted). A tenant-scoped (BYO) host's row is that
// tenant's configuration: its GET is an admin read of that tenant
// (translog ScopeTenantDetail, D-EDGE-34); a platform host's names no
// customer.

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"regexp"
	"time"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"github.com/FairForge/vaultaire/internal/audit"
	"github.com/FairForge/vaultaire/internal/dataplane"
	"github.com/FairForge/vaultaire/internal/translog"
	"github.com/FairForge/vaultaire/internal/translog/adminread"
)

// dataPlaneHost is a data_plane_hosts row as the admin API answers it.
type dataPlaneHost struct {
	Host      string     `json:"host"`
	Kind      string     `json:"kind"`
	Address   string     `json:"address,omitempty"`
	TenantID  string     `json:"tenant_id,omitempty"`
	Weight    int        `json:"weight"`
	Health    string     `json:"health"`
	LastSeen  *time.Time `json:"last_seen,omitempty"`
	Version   string     `json:"version,omitempty"`
	CreatedAt time.Time  `json:"created_at"`
	tokenHash string
}

// hostNameRe: a lower-case DNS name (or the registry's "slc").
var hostNameRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?(\.[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?)*$`)

func validHostName(h string) bool { return len(h) <= 253 && hostNameRe.MatchString(h) }

// readDataPlaneHost reads one row (sql.ErrNoRows when absent).
func readDataPlaneHost(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, host string) (dataPlaneHost, error) {
	var h dataPlaneHost
	var address, tenantID, tokenHash, version sql.NullString
	var lastSeen sql.NullTime
	err := q.QueryRowContext(ctx, `SELECT host, kind, address, tenant_id, token_hash, weight, health, last_seen, version, created_at
		FROM data_plane_hosts WHERE host = $1`, host).Scan(&h.Host, &h.Kind, &address, &tenantID, &tokenHash, &h.Weight, &h.Health,
		&lastSeen, &version, &h.CreatedAt)
	if err != nil {
		return h, err
	}
	h.Address, h.TenantID, h.tokenHash, h.Version = address.String, tenantID.String, tokenHash.String, version.String
	if lastSeen.Valid {
		t := lastSeen.Time
		h.LastSeen = &t
	}
	return h, nil
}

// registerAdminDataPlaneHostRoutes mounts the registry under /api/v1/admin.
func (s *Server) registerAdminDataPlaneHostRoutes(r chi.Router) {
	r.Get("/data-plane-hosts/{host}", s.requireAdmin(s.adminRead("/api/v1/admin/data-plane-hosts/{host}", s.handleGetDataPlaneHost)))
	r.Put("/data-plane-hosts/{host}", s.requireAdmin(s.handlePutDataPlaneHost))
	r.Delete("/data-plane-hosts/{host}", s.requireAdmin(s.handleDeleteDataPlaneHost))
}

// dataPlaneHostScope is the GET's admin-read classification (D-EDGE-34): a
// tenant-scoped host's row is that tenant's record; a platform host's — or
// one that does not exist — names no customer (no entry).
func dataPlaneHostScope(r *http.Request, db *sql.DB) (string, string, error) {
	if db == nil {
		return "", "", nil
	}
	h, err := readDataPlaneHost(r.Context(), db, chi.URLParam(r, "host"))
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return "", "", nil
	case err != nil:
		return "", "", err
	case h.TenantID != "":
		return translog.ScopeTenantDetail, h.TenantID, nil
	}
	return "", "", nil
}

func (s *Server) handleGetDataPlaneHost(w http.ResponseWriter, r *http.Request) {
	if s.db == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "no_database"})
		return
	}
	h, err := readDataPlaneHost(r.Context(), s.db, chi.URLParam(r, "host"))
	switch {
	case errors.Is(err, sql.ErrNoRows):
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not_found"})
	case err != nil:
		s.logger.Error("data-plane host: read failed", zap.Error(err))
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "read_failed"})
	default:
		writeJSON(w, http.StatusOK, h)
	}
}

// dataPlaneHostPut is the PUT body.
type dataPlaneHostPut struct {
	Kind     string  `json:"kind"`
	Address  string  `json:"address"`
	TenantID string  `json:"tenant_id"`
	Weight   *int    `json:"weight"`
	Health   *string `json:"health"`
}

func (s *Server) handlePutDataPlaneHost(w http.ResponseWriter, r *http.Request) {
	if s.db == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "no_database"})
		return
	}
	host := chi.URLParam(r, "host")
	var in dataPlaneHostPut
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_json"})
		return
	}
	bad := func(why string) { writeJSON(w, http.StatusBadRequest, map[string]string{"error": why}) }
	switch {
	case !validHostName(host):
		bad("invalid_host")
		return
	case in.Kind != dataplane.KindEdge && in.Kind != dataplane.KindNode && in.Kind != dataplane.KindSLC:
		bad("invalid_kind")
		return
	case in.Kind == dataplane.KindNode && net.ParseIP(in.Address) == nil:
		bad("a node's address is the IP it calls from")
		return
	case in.Kind != dataplane.KindNode && in.Address != "":
		bad("only a node has an address (edge: the Cloudflare rule; slc: in-process)")
		return
	case in.Kind != dataplane.KindNode && in.TenantID != "":
		bad("only a node may be tenant-scoped")
		return
	case in.Weight != nil && *in.Weight < 0:
		bad("weight must be >= 0")
		return
	case in.Health != nil && *in.Health != "up" && *in.Health != "down" && *in.Health != "draining":
		bad("health is up, down or draining")
		return
	}
	if in.TenantID != "" {
		ok, err := adminread.TenantExists(r.Context(), s.db, in.TenantID)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "read_failed"})
			return
		}
		if !ok {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "tenant_not_found"})
			return
		}
	}
	if in.Kind == dataplane.KindNode {
		in.Address = net.ParseIP(in.Address).String()
	}

	tx, err := s.db.BeginTx(r.Context(), nil)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "write_failed"})
		return
	}
	defer func() { _ = tx.Rollback() }()
	prev, err := readDataPlaneHost(r.Context(), tx, host)
	exists := err == nil
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "read_failed"})
		return
	}
	weight, health := 0, "down"
	if exists {
		weight, health = prev.Weight, prev.Health
	}
	if in.Weight != nil {
		weight = *in.Weight
	}
	if in.Health != nil {
		health = *in.Health
	}
	token, tokenHash := "", prev.tokenHash
	if in.Kind != dataplane.KindNode {
		tokenHash = ""
	} else if tokenHash == "" {
		var raw [32]byte
		if _, err := rand.Read(raw[:]); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "token_failed"})
			return
		}
		token = hex.EncodeToString(raw[:])
		sum := sha256.Sum256([]byte(token))
		tokenHash = hex.EncodeToString(sum[:])
	}
	if _, err := tx.ExecContext(r.Context(), `
		INSERT INTO data_plane_hosts (host, kind, address, token_hash, tenant_id, weight, health)
		VALUES ($1, $2, NULLIF($3, ''), NULLIF($4, ''), NULLIF($5, ''), $6, $7)
		ON CONFLICT (host) DO UPDATE SET kind = EXCLUDED.kind, address = EXCLUDED.address, token_hash = EXCLUDED.token_hash,
			tenant_id = EXCLUDED.tenant_id, weight = EXCLUDED.weight, health = EXCLUDED.health`,
		host, in.Kind, in.Address, tokenHash, in.TenantID, weight, health); err != nil {
		s.logger.Error("data-plane host: write failed", zap.Error(err), zap.String("host", host))
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "write_failed"})
		return
	}
	if err := tx.Commit(); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "write_failed"})
		return
	}
	s.dataPlaneHosts().invalidate()
	actor, _ := r.Context().Value(userIDKey).(string)
	audit.Record(r.Context(), s.db, audit.Entry{UserID: actor, TenantID: in.TenantID, EventType: "admin", Action: "data_plane.host_set",
		Resource: "data_plane_host:" + host, Metadata: map[string]any{"host": host, "kind": in.Kind, "address": in.Address,
			"tenant_id": in.TenantID, "weight": weight, "health": health, "token_minted": token != "", "created": !exists}})
	s.logger.Info("data-plane host set", zap.String("host", host), zap.String("kind", in.Kind), zap.Bool("tenant_scoped", in.TenantID != ""),
		zap.Int("weight", weight), zap.String("health", health), zap.Bool("token_minted", token != ""))
	stored, err := readDataPlaneHost(r.Context(), s.db, host)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "read_failed"})
		return
	}
	if token == "" {
		writeJSON(w, http.StatusOK, stored)
		return
	}
	// The token, ONCE: never stored, never logged, never answered again.
	writeJSON(w, http.StatusOK, struct {
		dataPlaneHost
		Token string `json:"token"`
	}{stored, token})
}

func (s *Server) handleDeleteDataPlaneHost(w http.ResponseWriter, r *http.Request) {
	if s.db == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "no_database"})
		return
	}
	host := chi.URLParam(r, "host")
	var tenantID sql.NullString
	err := s.db.QueryRowContext(r.Context(), `DELETE FROM data_plane_hosts WHERE host = $1 RETURNING tenant_id`, host).Scan(&tenantID)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not_found"})
		return
	case err != nil:
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "write_failed"})
		return
	}
	s.dataPlaneHosts().invalidate()
	actor, _ := r.Context().Value(userIDKey).(string)
	audit.Record(r.Context(), s.db, audit.Entry{UserID: actor, TenantID: tenantID.String, EventType: "admin", Action: "data_plane.host_deleted",
		Resource: "data_plane_host:" + host, Metadata: map[string]any{"host": host}})
	s.logger.Info("data-plane host deleted", zap.String("host", host))
	writeJSON(w, http.StatusOK, map[string]string{"deleted": host})
}
