package api

// POST /api/v1/edge/heartbeat (WP-EDGE-3): a data node says it is alive,
// every 30 s (internal/node). Its row's last_seen and version are set and a
// DOWN node is brought up — never a draining one (the operator drains). The
// call is the edge gate's (edgeCaller: the node's token from its registered
// address) and is accepted whatever the host's health; the Worker sends
// none (its health is Cloudflare's), and a heartbeat with the service
// token is acknowledged and changes nothing.

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"go.uber.org/zap"

	"github.com/FairForge/vaultaire/internal/dataplane"
)

func (s *Server) handleEdgeHeartbeat(w http.ResponseWriter, r *http.Request) {
	host, _, ok := s.edgeCaller(r)
	if !ok {
		s.edgeNotFound(w, r)
		return
	}
	var hb dataplane.Heartbeat
	if err := json.NewDecoder(io.LimitReader(r.Body, 16<<10)).Decode(&hb); err != nil {
		writeEdgeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_json"})
		return
	}
	if host.Kind != dataplane.KindNode {
		writeEdgeJSON(w, http.StatusOK, map[string]string{"host": host.Name})
		return
	}
	if s.db == nil {
		writeEdgeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "no_database"})
		return
	}
	version := strings.TrimSpace(hb.Version)
	if len(version) > 64 {
		version = version[:64]
	}
	var health string
	if err := s.db.QueryRowContext(r.Context(), `
		UPDATE data_plane_hosts SET last_seen = NOW(), version = NULLIF($2, ''),
			health = CASE WHEN health = 'down' THEN 'up' ELSE health END
		WHERE host = $1 RETURNING health`, host.Name, version).Scan(&health); err != nil {
		s.logger.Warn("edge heartbeat: not recorded", zap.Error(err), zap.String("host", host.Name))
		writeEdgeJSON(w, http.StatusInternalServerError, map[string]string{"error": "not_recorded"})
		return
	}
	s.dataPlaneHosts().invalidate()
	writeEdgeJSON(w, http.StatusOK, map[string]string{"host": host.Name, "health": health})
}
