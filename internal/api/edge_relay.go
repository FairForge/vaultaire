package api

// What the Worker relays to the S3 client (WP-EDGE-1 Part A): the exact
// status, headers and XML body the origin would have answered. The admit
// and commit handlers run the origin's own refusal writers (WriteS3Error,
// a placement refusal, the lock refusal) against a relayRecorder and hand
// the recording to the Worker, so a refused edge write reads exactly like
// a refused origin write.

import (
	"bytes"
	"net/http"

	"github.com/FairForge/vaultaire/internal/dataplane"
)

// edgeRelay is one S3 answer, as JSON for the host (dataplane.Relay).
type edgeRelay = dataplane.Relay

// relayRecorder captures what a refusal writer would have sent.
type relayRecorder struct {
	status int
	header http.Header
	body   bytes.Buffer
}

func newRelayRecorder() *relayRecorder { return &relayRecorder{header: http.Header{}} }

func (r *relayRecorder) Header() http.Header { return r.header }

func (r *relayRecorder) Write(p []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	return r.body.Write(p)
}

func (r *relayRecorder) WriteHeader(code int) {
	if r.status == 0 {
		r.status = code
	}
}

// relay is the recording as the Worker relays it. Every header is sent
// once (S3 answers carry single-valued headers).
func (r *relayRecorder) relay() *edgeRelay {
	out := &edgeRelay{Status: r.status, Headers: map[string]string{}, Body: r.body.String()}
	if out.Status == 0 {
		out.Status = http.StatusOK
	}
	for k, v := range r.header {
		if len(v) > 0 {
			out.Headers[k] = v[0]
		}
	}
	return out
}
