package api

// WP-EDGE-3 step 7: the edge calls accept a registered node — its token,
// looked up by sha256 in data_plane_hosts, from the IP the registry names
// (the trusted direct peer) — beside the Worker's service token from a
// Cloudflare peer. A node's call from another address is the edge's 404; a
// node marked down is refused at ADMIT (503, Retry-After 30), never at
// commit (a commit must always be accepted).

import (
	"bytes"
	"context"
	"crypto/md5" // #nosec G501 -- the S3 ETag
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
	promtestutil "github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/FairForge/vaultaire/internal/crypto"
	"github.com/FairForge/vaultaire/internal/dataplane"
)

// registerNode writes a node row (token hashed) and returns its token.
func (f *edgeFixture) registerNode(address, tenantID, health string) (string, string) {
	f.t.Helper()
	host := "node-" + uuid.New().String()[:8] + ".lab.test"
	token := "node-token-" + uuid.New().String()
	sum := sha256.Sum256([]byte(token))
	// A node registered up has just sent its heartbeat (a node up but
	// unseen for 90 s is down at the gate — step 13).
	f.exec(`INSERT INTO data_plane_hosts (host, kind, address, token_hash, tenant_id, weight, health, last_seen) VALUES ($1, 'node', $2, $3, NULLIF($4, ''), 1, $5, NOW())`,
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

// Step 8 (a): a tenant-scoped (BYO) node admits ONE tenant — another
// tenant's request through it is the relayed 403 AccessDenied, counted
// refused under the node's host label.
func TestEdgeCalls_ATenantScopedNodeAdmitsItsTenantOnly(t *testing.T) {
	f := setupEdgeFixture(t)
	other := "byo-" + uuid.New().String()[:8]
	f.exec(`INSERT INTO tenants (id, name, email) VALUES ($1, 'BYO', $2)`, other, other+"@test.local")
	t.Cleanup(func() { _, _ = f.db.Exec(`DELETE FROM tenants WHERE id = $1`, other) })
	host, token := f.registerNode("198.51.100.40", other, "up")
	refused := promtestutil.ToFloat64(dataPlaneAdmits.WithLabelValues(host, edgeOpPut, "refused"))
	w := f.callFrom("/api/v1/edge/admit", f.signed("/"+f.bucket+"/byo.bin", nil, testBytes(5000), nil, ""), token, "198.51.100.40:1")
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var resp edgeAdmitResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	require.NotNil(t, resp.Refusal, "%+v", resp)
	assert.Equal(t, http.StatusForbidden, resp.Refusal.Status)
	assert.Contains(t, resp.Refusal.Body, "AccessDenied")
	assert.Equal(t, refused+1, promtestutil.ToFloat64(dataPlaneAdmits.WithLabelValues(host, edgeOpPut, "refused")))

	// Its own tenant's request is admitted.
	_, own := f.registerNode("198.51.100.41", f.tenantID, "up")
	w = f.callFrom("/api/v1/edge/admit", f.signed("/"+f.bucket+"/byo2.bin", nil, testBytes(5000), nil, ""), own, "198.51.100.41:1")
	require.Equal(t, http.StatusOK, w.Code)
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.True(t, resp.Edge, "%+v", resp)
}

// Step 8 (b): a tenant-scoped node's reports are never trusted for
// integrity — every stored chunk of its codec commit is OPENED (RP68-3 at
// 100 %), not the first plus a HEAD of every 32nd.
func TestEdgeCodecCommit_ATenantScopedNodeHasEveryStoredChunkOpened(t *testing.T) {
	f := setupCodecFixture(t)
	_, token := f.registerNode("198.51.100.50", f.tenantID, "up")
	body := crypto.GoldenBody(4801, 40<<20)
	chunks := fixedChunks(body, 1<<20)
	w := f.callFrom("/api/v1/edge/admit", f.signed("/"+f.bucket+"/forty.bin", nil, body, nil, ""), token, "198.51.100.50:1")
	require.Equal(t, http.StatusOK, w.Code)
	var a edgeAdmitResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &a))
	require.NotNil(t, a.Codec, "%+v", a)
	req := edgeIndexRequest{AdmitID: a.AdmitID}
	for _, c := range chunks {
		req.Chunks = append(req.Chunks, dataplane.IndexChunk{Hash: c.Hash, Size: int64(c.Size)})
	}
	w = f.callFrom("/api/v1/edge/index", req, token, "198.51.100.50:1")
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var idx edgeIndexResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &idx))
	entries := f.storeMisses(a.Codec, chunks, idx)
	f.drv.heads.Store(0)
	f.drv.ranges.Store(0)
	sum := md5.Sum(body) // #nosec G401
	w = f.callFrom("/api/v1/edge/commit", edgeCommitRequest{AdmitID: a.AdmitID, Outcome: edgeOutcomeOK, Chunks: entries, BodyMD5: hex.EncodeToString(sum[:])}, token, "198.51.100.50:1")
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var relay edgeRelay
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &relay))
	require.Equal(t, http.StatusOK, relay.Status, relay.Body)
	assert.Equal(t, int32(40), f.drv.ranges.Load(), "every stored chunk opened")
	assert.Zero(t, f.drv.heads.Load(), "no HEAD sample: the opens cover it")
}

// Step 8 (c): a tenant-scoped node's GET is metered at the ADMITTED span
// whatever Content-Length it reports (the 2026-10-26 COGS rule: a
// customer-run host's reports are never trusted for metering).
func TestEdgeCommitGet_ATenantScopedNodeIsMeteredAtTheAdmittedSpan(t *testing.T) {
	f := setupEdgeFixture(t)
	f.wireEgress(1<<40, 0, nil)
	body := testBytes(9000)
	f.putObject("byo-read.bin", body, nil)
	_, token := f.registerNode("198.51.100.60", f.tenantID, "up")
	w := f.callFrom("/api/v1/edge/admit", f.signedGet("/"+f.bucket+"/byo-read.bin", nil, nil), token, "198.51.100.60:1")
	require.Equal(t, http.StatusOK, w.Code)
	var a edgeAdmitResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &a))
	require.True(t, a.Edge, "%+v", a)
	before := f.egressRecorded()
	w = f.callFrom("/api/v1/edge/commit", servedCommit(a.AdmitID, 10, http.StatusPartialContent), token, "198.51.100.60:1")
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Equal(t, before+int64(len(body)), f.egressRecorded(), "the admitted span, not the 10 bytes the host reported")

	// It holds the presigned link whatever it reports: a "fallback" (the
	// origin served, it says) is metered at the admitted span too — the
	// trade is a double meter if the origin did serve it.
	w = f.callFrom("/api/v1/edge/admit", f.signedGet("/"+f.bucket+"/byo-read.bin", nil, nil), token, "198.51.100.60:1")
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &a))
	require.True(t, a.Edge)
	before = f.egressRecorded()
	w = f.callFrom("/api/v1/edge/commit", edgeCommitRequest{AdmitID: a.AdmitID, Outcome: edgeGetFallback, BackendStatus: http.StatusPreconditionFailed}, token, "198.51.100.60:1")
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Equal(t, before+int64(len(body)), f.egressRecorded(), "a BYO host's fallback is metered at the admitted span")
}

// Step 9: every admit row records the host it was made for — "edge" for
// the Worker's service token, the registry's name for a node, "slc" for the
// origin in-process — and the committed bytes are counted under it.
func TestEdgeAdmitRow_CarriesItsHost(t *testing.T) {
	f := setupEdgeFixture(t)
	host, token := f.registerNode("198.51.100.70", "", "up")
	body := testBytes(5000)
	hostOf := func(id string) string {
		var h string
		require.NoError(t, f.db.QueryRow(`SELECT COALESCE(host, '') FROM edge_admits WHERE id = $1`, id).Scan(&h))
		return h
	}

	bytesBefore := promtestutil.ToFloat64(dataPlaneBytes.WithLabelValues(host, edgeOpPut))
	w := f.callFrom("/api/v1/edge/admit", f.signed("/"+f.bucket+"/h1.bin", nil, body, nil, ""), token, "198.51.100.70:1")
	var a edgeAdmitResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &a))
	require.True(t, a.Edge, "%+v", a)
	assert.Equal(t, host, hostOf(a.AdmitID), "a node's admit")
	etag := f.drv.workerPut(f.tenantID, f.container(), a.StorageKey, body)
	w = f.callFrom("/api/v1/edge/commit", okCommit(a.AdmitID, body, etag), token, "198.51.100.70:1")
	require.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, bytesBefore+float64(len(body)), promtestutil.ToFloat64(dataPlaneBytes.WithLabelValues(host, edgeOpPut)), "counted under the node")

	a = f.admit(f.signed("/"+f.bucket+"/h2.bin", nil, body, nil, ""))
	require.True(t, a.Edge)
	assert.Equal(t, edgeHost, hostOf(a.AdmitID), "the Worker's admit")

	res, err := f.dataHost().Write(context.Background(), f.signedHTTP(http.MethodPut, "/"+f.bucket+"/h3.bin", nil, body, nil), bytes.NewReader(body), int64(len(body)))
	require.NoError(t, err)
	assert.Equal(t, dataplane.HostSLC, hostOf(res.Admit.AdmitID), "the origin in-process")
}

// The heartbeat (POST /api/v1/edge/heartbeat): a node's call from its
// address sets last_seen and version and brings a DOWN node up — never a
// draining one (the operator's) — and is accepted whatever the health.
func TestEdgeHeartbeat_BringsADownNodeUp(t *testing.T) {
	f := setupEdgeFixture(t)
	host, token := f.registerNode("198.51.100.80", "", "down")
	hb := dataplane.Heartbeat{Version: "abc1234", BuildSHA: "abc1234", InFlight: 2}
	call := func(peer string) *httptest.ResponseRecorder {
		b, _ := json.Marshal(hb)
		r := httptest.NewRequest(http.MethodPost, "/api/v1/edge/heartbeat", bytes.NewReader(b))
		r.RemoteAddr = peer
		r.Header.Set(edgeTokenHeader, token)
		w := httptest.NewRecorder()
		f.srv.handleEdgeHeartbeat(w, r)
		return w
	}
	assert.Equal(t, http.StatusNotFound, call("198.51.100.81:1").Code, "from another address")
	require.Equal(t, http.StatusOK, call("198.51.100.80:1").Code)
	var health, version string
	var seen bool
	require.NoError(t, f.db.QueryRow(`SELECT health, COALESCE(version, ''), last_seen IS NOT NULL FROM data_plane_hosts WHERE host = $1`, host).Scan(&health, &version, &seen))
	assert.Equal(t, "up", health)
	assert.Equal(t, "abc1234", version)
	assert.True(t, seen)

	f.exec(`UPDATE data_plane_hosts SET health = 'draining' WHERE host = $1`, host)
	f.srv.dataPlaneHosts().invalidate()
	require.Equal(t, http.StatusOK, call("198.51.100.80:1").Code)
	require.NoError(t, f.db.QueryRow(`SELECT health FROM data_plane_hosts WHERE host = $1`, host).Scan(&health))
	assert.Equal(t, "draining", health, "a draining node stays draining")
}
