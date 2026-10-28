package api

// WP-EDGE-3: a data node end to end in one process — internal/node's
// router and HTTP control plane against the Server's real edge routes (an
// httptest server: the node's token accepted from 127.0.0.1, the address
// its registry row names), the fake backend's transport as the node's link
// to the backend. A PUT through the node commits a head row admitted for
// the node; a GET through it serves the bytes; a HEAD (not the node's) is
// handed to the origin.

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/FairForge/vaultaire/internal/node"
)

func TestDataNode_EndToEndThroughTheControlPlanesRoutes(t *testing.T) {
	f := setupEdgeFixture(t)
	f.wireEgress(1<<40, 0, nil)
	f.srv.edge.trustAnyPeer = false
	f.srv.registerEdgeRoutes()
	cp := httptest.NewServer(f.srv.router)
	defer cp.Close()
	host, token := f.registerNode("127.0.0.1", "", "down")

	env := map[string]string{"NODE_CONTROL_PLANE_URL": cp.URL, "NODE_TOKEN": token, "NODE_PUBLIC_HOST": host, "NODE_BIND": "127.0.0.1"}
	n, err := node.Build(func(k string) string { return env[k] }, os.ReadFile, "e2e", zap.NewNop())
	require.NoError(t, err)
	n.SetBackendTransport(f.drv)

	// A node that never sent a heartbeat is down: its admit is refused and
	// the request goes to the origin untouched (here the test's origin
	// mounts only the edge routes: its 404) — nothing admitted, nothing
	// committed.
	body := testBytes(12000)
	w := httptest.NewRecorder()
	n.Router().ServeHTTP(w, f.signedHTTP(http.MethodPut, "/"+f.bucket+"/via-node.bin", nil, body, nil))
	assert.Equal(t, http.StatusNotFound, w.Code, "handed to the origin: %s", w.Body.String())
	assert.Zero(t, f.pending())
	_, committed := f.head("via-node.bin")
	assert.False(t, committed)

	f.exec(`UPDATE data_plane_hosts SET health = 'up' WHERE host = $1`, host)
	f.srv.dataPlaneHosts().invalidate()
	w = httptest.NewRecorder()
	n.Router().ServeHTTP(w, f.signedHTTP(http.MethodPut, "/"+f.bucket+"/via-node.bin", nil, body, nil))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	h, ok := f.head("via-node.bin")
	require.True(t, ok)
	assert.Equal(t, int64(len(body)), h.size)
	var admitHost string
	require.NoError(t, f.db.QueryRow(`SELECT host FROM edge_admits WHERE tenant_id = $1 AND object_key = 'via-node.bin' AND op = 'put'`, f.tenantID).Scan(&admitHost))
	assert.Equal(t, host, admitHost, "admitted for the node")

	w = httptest.NewRecorder()
	n.Router().ServeHTTP(w, f.signedHTTP(http.MethodGet, "/"+f.bucket+"/via-node.bin", nil, nil, nil))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	got, _ := io.ReadAll(w.Body)
	assert.True(t, bytes.Equal(body, got), "the bytes, through the node")
	assert.Equal(t, `"`+h.etag+`"`, w.Header().Get("ETag"), "under the origin's headers")
}

// RP74-9: the node 404s the origin's web and API paths by their FIRST
// segment, from a list that must cover the router's reserved bucket names
// (the router derives them; the node cannot build the router).
func TestNodeReservedSegments_AreTheRoutersReservedNames(t *testing.T) {
	srv, _ := gateServer(t, prodTodayHosts)
	for _, name := range srv.reservedBucketNameList() {
		assert.True(t, node.ReservedSegments[name], "the router reserves %q: the node must 404 it", name)
	}
}
