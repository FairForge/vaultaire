package api

// host=slc (WP-EDGE-3 step 11, plan 40.10: "the first hosts are the client
// and SLC itself"): with flag data_plane_slc ON for a tenant, the origin's
// own PutObject, UploadPart and GetObject of an edge-eligible request go
// through internal/dataplane IN-PROCESS — the admit and the commit as
// function calls (Server.DataPlane), the bytes through the same mover or
// codec on this box. Every byte is then labelled by host and the rows are
// the ONE shape the Worker and the nodes write. What the admit passes
// through (small, sealed, chunked without the codec, a copy, an unsigned
// request …) and a control plane that does not answer are the handler as
// today, the body untouched. OFF = no change at all.
//
// The bytes still cross this box, and the origin's own dispatch meters
// them (handleS3Request's counting writer, the access-log row, the egress
// guard's pacing on a GET): the commit of a host=slc admit records neither
// (settleEdgeGet, recordEdgeAccess, the reaper's expired-read meter).

import (
	"io"
	"net/http"

	"go.uber.org/zap"

	"github.com/FairForge/vaultaire/internal/dataplane"
)

// slcHost is the origin's data-plane host over its in-process control plane.
func (s *Server) slcHost() *dataplane.Host {
	client := &http.Client{}
	if s.dataPlaneTransport != nil {
		client.Transport = s.dataPlaneTransport
	}
	return &dataplane.Host{CP: s.DataPlane(), Client: client, ClientIP: extractClientIP, Logger: s.logger}
}

// slcEnabled says whether host=slc takes this tenant's requests.
func (s *Server) slcEnabled(tenantID string) bool {
	return s.flags != nil && tenantID != "" && s.flags.Enabled(flagDataPlaneSLC, tenantID)
}

// slcWrite runs a PutObject or UploadPart as host=slc; false = the handler
// takes it as today (the body untouched).
func (s *Server) slcWrite(w http.ResponseWriter, r *http.Request, tenantID string) bool {
	if !s.slcEnabled(tenantID) || s.db == nil {
		return false
	}
	res, err := s.slcHost().Write(r.Context(), r, r.Body, r.ContentLength)
	switch {
	case err != nil:
		s.logger.Warn("host=slc: the admit failed — the handler takes the request", zap.Error(err))
		return false
	case res.PassThrough:
		return false
	}
	dataplane.WriteRelay(w, res.Relay)
	return true
}

// slcRead runs a GetObject as host=slc; false = the handler serves it.
func (s *Server) slcRead(w http.ResponseWriter, r *http.Request, tenantID string) bool {
	if !s.slcEnabled(tenantID) || s.db == nil {
		return false
	}
	res, err := s.slcHost().Read(r.Context(), r)
	switch {
	case err != nil:
		s.logger.Warn("host=slc: the read admit failed — the handler serves it", zap.Error(err))
		return false
	case res.PassThrough:
		return false
	case res.Relay != nil:
		dataplane.WriteRelay(w, res.Relay)
		return true
	}
	defer func() { _ = res.Body.Close() }()
	for k, v := range res.Headers {
		w.Header().Set(k, v)
	}
	w.WriteHeader(res.Status)
	if _, err := io.Copy(w, res.Body); err != nil {
		// The client's response is cut (never a clean short end).
		s.logger.Warn("host=slc: the read was cut", zap.Error(err))
		cutUnlessClientGone(r.Context())
	}
	return true
}
