package dataplane

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeCP answers one admit and records the commit.
type fakeCP struct {
	admit  AdmitResponse
	mu     sync.Mutex
	commit *CommitRequest
}

func (c *fakeCP) Admit(context.Context, AdmitRequest) (AdmitResponse, error) { return c.admit, nil }
func (c *fakeCP) Index(context.Context, IndexRequest) (IndexResponse, error) {
	return IndexResponse{}, nil
}
func (c *fakeCP) Commit(_ context.Context, r CommitRequest) (CommitResponse, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.commit = &r
	if r.Outcome != OutcomeOK {
		return Relay{Status: http.StatusServiceUnavailable}, nil
	}
	return Relay{Status: http.StatusOK}, nil
}

// A backend that takes the body and never answers (Lyve us-east-1, 1–3 %
// of PUTs on 2026-10-17) ends the move at the host's response deadline as
// edge_error — the client's 503, never a request that hangs.
func TestHost_ABackendThatNeverAnswersEndsAtTheDeadline(t *testing.T) {
	release := make(chan struct{})
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		<-release
	}))
	defer backend.Close()
	defer close(release)
	cp := &fakeCP{admit: AdmitResponse{Edge: true, AdmitID: "A1", URL: backend.URL + "/b/k", Method: http.MethodPut,
		SendHeaders: map[string]string{"content-length": "5"}}}
	h := &Host{CP: cp, Client: NewBackendClient(300 * time.Millisecond)}
	req := httptest.NewRequest(http.MethodPut, "https://s3.test/b/k", nil)
	start := time.Now()
	res, err := h.Write(context.Background(), req, bytes.NewReader([]byte("hello")), 5)
	require.NoError(t, err)
	assert.Less(t, time.Since(start), 5*time.Second, "ended at the deadline")
	require.NotNil(t, cp.commit)
	assert.Equal(t, OutcomeEdgeError, cp.commit.Outcome)
	assert.Equal(t, http.StatusServiceUnavailable, res.Relay.Status)
}
