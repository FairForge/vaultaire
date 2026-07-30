package api

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/FairForge/vaultaire/internal/config"
	"github.com/FairForge/vaultaire/internal/engine"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// http.Server.ReadTimeout bounds reading the ENTIRE request including the body,
// and WriteTimeout bounds the whole response. On a storage service both are
// data-size limits in disguise: a 30s ReadTimeout rejected every upload that
// took longer than 30s to stream to the backend (observed in production as a
// 502 on a 50 MB PUT), and a 30s WriteTimeout truncates any download slower
// than that. Neither may be set; slowloris is bounded by ReadHeaderTimeout,
// which applies only to headers.

func newTestServerForTimeouts(t *testing.T) *Server {
	t.Helper()
	return NewServer(
		&config.Config{Server: config.ServerConfig{Port: 0}},
		zap.NewNop(),
		engine.NewEngine(nil, zap.NewNop(), &engine.Config{DefaultBackend: "local"}),
		nil, nil,
	)
}

func TestServerTimeouts_NoBodySizeLimitInDisguise(t *testing.T) {
	srv := newTestServerForTimeouts(t)
	require.NotNil(t, srv.httpServer)

	assert.Zero(t, srv.httpServer.ReadTimeout,
		"ReadTimeout caps total request time — it silently rejects large uploads")
	assert.Zero(t, srv.httpServer.WriteTimeout,
		"WriteTimeout caps total response time — it silently truncates large downloads")

	assert.Positive(t, srv.httpServer.ReadHeaderTimeout,
		"ReadHeaderTimeout must still bound slowloris on headers")
	assert.LessOrEqual(t, srv.httpServer.ReadHeaderTimeout, 30*time.Second,
		"header timeout should stay tight")
	assert.Positive(t, srv.httpServer.IdleTimeout,
		"IdleTimeout should reap idle keep-alive connections")
}

// A body that arrives in slow dribbles must still be read to completion: this
// is the shape of a large upload over a slow link, and what the old 30s
// ReadTimeout broke.
func TestServerTimeouts_SlowBodyIsReadToCompletion(t *testing.T) {
	const chunks, chunkSize = 6, 4096
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n, err := io.Copy(io.Discard, r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusRequestTimeout)
			return
		}
		if n != chunks*chunkSize {
			http.Error(w, "short read", http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusOK)
	})

	ts := httptest.NewUnstartedServer(handler)
	ts.Config.ReadHeaderTimeout = 10 * time.Second
	ts.Config.ReadTimeout = 0 // the property under test
	ts.Start()
	defer ts.Close()

	pr, pw := io.Pipe()
	go func() {
		defer func() { _ = pw.Close() }()
		for i := 0; i < chunks; i++ {
			_, _ = pw.Write(bytes.Repeat([]byte("x"), chunkSize))
			time.Sleep(150 * time.Millisecond) // total ~0.9s of dribble
		}
	}()

	req, err := http.NewRequest(http.MethodPut, ts.URL+"/slow", pr)
	require.NoError(t, err)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()

	assert.Equal(t, http.StatusOK, resp.StatusCode,
		"a slowly-arriving body must not be cut off mid-upload")
}
