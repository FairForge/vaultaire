package dataplane

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The node's control plane: the three calls over HTTPS with the node's
// token in X-Vaultaire-Edge-Token, each JSON body the harness's shape; a
// non-200 is a StatusError carrying the call's own error; the commit runs
// on its own clock — a caller already gone still commits.
func TestHTTPControlPlane(t *testing.T) {
	var commits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Vaultaire-Edge-Token") != "node-token-0123456789abcdef" || r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/edge/admit":
			var req AdmitRequest
			require.NoError(t, json.NewDecoder(r.Body).Decode(&req))
			if req.Op == OpGet {
				w.WriteHeader(http.StatusServiceUnavailable)
				_, _ = w.Write([]byte(`{"error":"host_down"}`))
				return
			}
			_, _ = w.Write([]byte(`{"edge":true,"admit_id":"A1","codec":{"version":3,"kem_public_key":"AAEC","chunk_threshold":4194304,"segment_size":1048576}}`))
		case "/api/v1/edge/index":
			var req IndexRequest
			require.NoError(t, json.NewDecoder(r.Body).Decode(&req))
			assert.Equal(t, "A1", req.AdmitID)
			_, _ = w.Write([]byte(`{"hits":["aa"],"misses":[{"hash":"bb","url":"https://b/x","send_headers":{"content-length":"1203"},"sealed_length":1203}]}`))
		case "/api/v1/edge/commit":
			time.Sleep(50 * time.Millisecond)
			commits.Add(1)
			_, _ = w.Write([]byte(`{"status":200,"headers":{"ETag":"\"e\""}}`))
		}
	}))
	defer srv.Close()
	cp := NewHTTPControlPlane(srv.URL, []byte("node-token-0123456789abcdef"), nil)

	adm, err := cp.Admit(context.Background(), AdmitRequest{Op: OpPut})
	require.NoError(t, err)
	assert.True(t, adm.Edge)
	require.NotNil(t, adm.Codec)
	assert.Equal(t, 3, adm.Codec.Version)
	assert.Equal(t, []byte{0, 1, 2}, adm.Codec.KEMPublicKey)

	_, err = cp.Admit(context.Background(), AdmitRequest{Op: OpGet})
	var se *StatusError
	require.True(t, errors.As(err, &se), "%v", err)
	assert.Equal(t, http.StatusServiceUnavailable, se.Status)
	assert.Equal(t, "host_down", se.Msg)

	idx, err := cp.Index(context.Background(), IndexRequest{AdmitID: "A1", Chunks: []IndexChunk{{Hash: "aa", Size: 1}, {Hash: "bb", Size: 2}}})
	require.NoError(t, err)
	assert.Equal(t, []string{"aa"}, idx.Hits)
	require.Len(t, idx.Misses, 1)
	assert.Equal(t, int64(1203), idx.Misses[0].SealedLength)

	gone, cancel := context.WithCancel(context.Background())
	cancel()
	relay, err := cp.Commit(gone, CommitRequest{AdmitID: "A1", Outcome: OutcomeOK})
	require.NoError(t, err, "the commit is on its own clock")
	assert.Equal(t, 200, relay.Status)
	assert.Equal(t, int32(1), commits.Load())

	bad := NewHTTPControlPlane(srv.URL, []byte("wrong-token-0123456789abcd"), nil)
	_, err = bad.Admit(context.Background(), AdmitRequest{Op: OpPut})
	require.True(t, errors.As(err, &se))
	assert.Equal(t, http.StatusNotFound, se.Status)
}
