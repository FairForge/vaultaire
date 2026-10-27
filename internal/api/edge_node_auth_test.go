package api

// WP-EDGE-3 step 7: the edge calls accept a registered node — its token,
// looked up by sha256 in data_plane_hosts, from the IP the registry names
// (the trusted direct peer) — beside the Worker's service token from a
// Cloudflare peer. A node's call from another address is the edge's 404; a
// node marked down is refused at ADMIT (503, Retry-After 30), never at
// commit (a commit must always be accepted).

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// registerNode writes a node row (token hashed) and returns its token.
func (f *edgeFixture) registerNode(address, tenantID, health string) (string, string) {
	f.t.Helper()
	host := "node-" + uuid.New().String()[:8] + ".lab.test"
	token := "node-token-" + uuid.New().String()
	sum := sha256.Sum256([]byte(token))
	f.exec(`INSERT INTO data_plane_hosts (host, kind, address, token_hash, tenant_id, weight, health) VALUES ($1, 'node', $2, $3, NULLIF($4, ''), 1, $5)`,
		host, address, hex.EncodeToString(sum[:]), tenantID, health)
	f.t.Cleanup(func() { _, _ = f.db.Exec(`DELETE FROM data_plane_hosts WHERE host = $1`, host) })
	f.srv.dataPlaneHosts().invalidate()
	return host, token
}

// callFrom is an edge call with token from peer.
func (f *edgeFixture) callFrom(path string, body any, token, peer string) *httptest.ResponseRecorder {
	f.t.Helper()
	b, err := json.Marshal(body)
	require.NoError(f.t, err)
	r := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(b))
	r.RemoteAddr = peer
	r.Header.Set(edgeTokenHeader, token)
	w := httptest.NewRecorder()
	switch path {
	case "/api/v1/edge/admit":
		f.srv.handleEdgeAdmit(w, r)
	case "/api/v1/edge/index":
		f.srv.handleEdgeIndex(w, r)
	default:
		f.srv.handleEdgeCommit(w, r)
	}
	return w
}

func TestEdgeCalls_ANodeTokenFromItsRegisteredAddress(t *testing.T) {
	f := setupEdgeFixture(t)
	f.srv.edge.trustAnyPeer = false // the Worker's rule applies: a Cloudflare peer
	_, token := f.registerNode("198.51.100.20", "", "up")
	body := testBytes(5000)
	req := f.signed("/"+f.bucket+"/node.bin", nil, body, nil, "")

	w := f.callFrom("/api/v1/edge/admit", req, token, "198.51.100.21:4000")
	assert.Equal(t, http.StatusNotFound, w.Code, "a node's token from another address: the route does not exist")
	w = f.callFrom("/api/v1/edge/admit", req, "node-token-unknown", "198.51.100.20:4000")
	assert.Equal(t, http.StatusNotFound, w.Code, "an unknown token")
	w = f.callFrom("/api/v1/edge/admit", req, edgeTestToken, "198.51.100.20:4000")
	assert.Equal(t, http.StatusNotFound, w.Code, "the service token from a peer that is not Cloudflare")

	w = f.callFrom("/api/v1/edge/admit", req, token, "198.51.100.20:4000")
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var resp edgeAdmitResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	require.True(t, resp.Edge, "%+v", resp)
	etag := f.drv.workerPut(f.tenantID, f.container(), resp.StorageKey, body)
	w = f.callFrom("/api/v1/edge/commit", okCommit(resp.AdmitID, body, etag), token, "198.51.100.20:4000")
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	_, ok := f.head("node.bin")
	assert.True(t, ok)
}

func TestEdgeCalls_ADownNodeIsRefusedAtAdmitNeverAtCommit(t *testing.T) {
	f := setupEdgeFixture(t)
	host, token := f.registerNode("198.51.100.30", "", "up")
	body := testBytes(5000)
	w := f.callFrom("/api/v1/edge/admit", f.signed("/"+f.bucket+"/down.bin", nil, body, nil, ""), token, "198.51.100.30:1")
	require.Equal(t, http.StatusOK, w.Code)
	var resp edgeAdmitResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	require.True(t, resp.Edge)

	f.exec(`UPDATE data_plane_hosts SET health = 'down' WHERE host = $1`, host)
	f.srv.dataPlaneHosts().invalidate()
	w = f.callFrom("/api/v1/edge/admit", f.signed("/"+f.bucket+"/down2.bin", nil, body, nil, ""), token, "198.51.100.30:1")
	assert.Equal(t, http.StatusServiceUnavailable, w.Code)
	assert.Equal(t, "30", w.Header().Get("Retry-After"))

	etag := f.drv.workerPut(f.tenantID, f.container(), resp.StorageKey, body)
	w = f.callFrom("/api/v1/edge/commit", okCommit(resp.AdmitID, body, etag), token, "198.51.100.30:1")
	assert.Equal(t, http.StatusOK, w.Code, "a commit of a down host is accepted: its bytes landed")
}
