package api

// The origin's in-process control plane (WP-EDGE-3, Stage 1a Prompt 47):
// the three edge calls as function calls — what a data-plane host on THIS
// box (host=slc) asks, and what the harness's Go host asks in the tests.
// The same decisions, rows and metrics as the HTTP routes; no token, no
// peer rule (the caller is this process).

import (
	"context"
	"net/http"

	"github.com/FairForge/vaultaire/internal/dataplane"
)

// inProcessControlPlane implements dataplane.ControlPlane on the Server.
type inProcessControlPlane struct {
	s    *Server
	host dataplane.HostInfo
}

// DataPlane is the origin's in-process control plane, as host=slc.
func (s *Server) DataPlane() dataplane.ControlPlane {
	return inProcessControlPlane{s: s, host: dataplane.HostInfo{Name: dataplane.HostSLC, Kind: dataplane.KindSLC}}
}

func (c inProcessControlPlane) ctx(ctx context.Context) context.Context {
	return dataplane.WithHost(ctx, c.host)
}

// Admit decides one request (handleEdgeAdmit's body).
func (c inProcessControlPlane) Admit(ctx context.Context, req dataplane.AdmitRequest) (dataplane.AdmitResponse, error) {
	if req.Op != edgeOpPut && req.Op != edgeOpPart && req.Op != edgeOpGet {
		return dataplane.AdmitResponse{}, &dataplane.StatusError{Status: http.StatusBadRequest, Msg: "invalid_op"}
	}
	if c.s.db == nil || c.s.engine == nil {
		countEdgeAdmit(c.host.Name, req.Op, dataplane.AdmitResponse{}, http.StatusServiceUnavailable)
		return dataplane.AdmitResponse{}, &dataplane.StatusError{Status: http.StatusServiceUnavailable, Msg: "no_database"}
	}
	resp, status := c.s.edgeAdmit(c.ctx(ctx), req)
	countEdgeAdmit(c.host.Name, req.Op, resp, status)
	if status != http.StatusOK {
		return resp, &dataplane.StatusError{Status: status, Msg: "admit_failed"}
	}
	return resp, nil
}

// Index answers one batch of chunk hashes (handleEdgeIndex's body).
func (c inProcessControlPlane) Index(ctx context.Context, req dataplane.IndexRequest) (dataplane.IndexResponse, error) {
	if c.s.db == nil || c.s.gci == nil || c.s.tenantKeys == nil {
		return dataplane.IndexResponse{}, &dataplane.StatusError{Status: http.StatusServiceUnavailable, Msg: "no_index"}
	}
	resp, status, err := c.s.edgeIndex(c.ctx(ctx), req)
	if err != nil {
		return dataplane.IndexResponse{}, &dataplane.StatusError{Status: status, Msg: err.Error()}
	}
	edgeCodecIndexCalls.Inc()
	return *resp, nil
}

// Commit settles one admit on its own clock (handleEdgeCommit's body).
func (c inProcessControlPlane) Commit(ctx context.Context, req dataplane.CommitRequest) (dataplane.CommitResponse, error) {
	if req.AdmitID == "" {
		return dataplane.CommitResponse{}, &dataplane.StatusError{Status: http.StatusBadRequest, Msg: "admit_id_required"}
	}
	if c.s.db == nil {
		return dataplane.CommitResponse{}, &dataplane.StatusError{Status: http.StatusServiceUnavailable, Msg: "no_database"}
	}
	cctx, cancel := context.WithTimeout(context.WithoutCancel(c.ctx(ctx)), edgeCommitBound)
	defer cancel()
	relay, status, err := c.s.edgeCommit(cctx, req)
	if err != nil {
		return dataplane.CommitResponse{}, &dataplane.StatusError{Status: status, Msg: err.Error()}
	}
	return *relay, nil
}
