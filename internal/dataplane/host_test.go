package dataplane

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/FairForge/vaultaire/internal/crypto"
	"github.com/FairForge/vaultaire/sdk/go/e2ee"
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

func (c *fakeCP) Mirror(context.Context, MirrorSettle) (MirrorSettleResponse, error) {
	return MirrorSettleResponse{State: "pending"}, nil
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

// D-EDGE-45 (RB5-5 answered): a sealed chunk is in memory on the host, so a
// PUT that gets no answer is sent once more on a fresh connection — one
// retry, one stored chunk. (A mover's streaming body is not replayable:
// never retried — the client's 503.)
func TestHost_ASealedChunkPutWithNoAnswerIsRetriedOnceOnAFreshConnection(t *testing.T) {
	var puts atomic.Int32
	var stored atomic.Int32
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if puts.Add(1) == 1 {
			// Take the body, then drop the connection without an answer.
			_, _ = io.Copy(io.Discard, r.Body)
			hj, ok := w.(http.Hijacker)
			require.True(t, ok)
			c, _, err := hj.Hijack()
			require.NoError(t, err)
			_ = c.Close()
			return
		}
		_, _ = io.Copy(io.Discard, r.Body)
		stored.Add(1)
		w.Header().Set("ETag", `"0123456789abcdef0123456789abcdef"`)
	}))
	defer backend.Close()
	seed := make([]byte, 32)
	_, _ = rand.Read(seed)
	pub, err := e2ee.PublicKey(seed)
	require.NoError(t, err)
	h := &Host{CP: &fakeCP{}, Client: NewBackendClient(5 * time.Second)}
	data := bytes.Repeat([]byte("chunk"), 4000)
	sum := sha256.Sum256(data)
	c := crypto.Chunk{Data: data, Size: len(data), Hash: hex.EncodeToString(sum[:])}
	etag, n, err := h.SealAndStore(context.Background(), &CodecGrant{Version: 1, KEMPublicKey: pub}, c,
		IndexMiss{Hash: c.Hash, URL: backend.URL + "/b/chunk"})
	require.NoError(t, err)
	assert.Equal(t, `"0123456789abcdef0123456789abcdef"`, etag)
	assert.Equal(t, crypto.WrappedChunkSealedLength(int64(len(data))), n)
	assert.Equal(t, int32(2), puts.Load(), "one retry")
	assert.Equal(t, int32(1), stored.Load(), "one stored chunk")

	// A backend that ANSWERS an error is not retried.
	puts.Store(10)
	refusing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		puts.Add(1)
		w.WriteHeader(http.StatusForbidden)
	}))
	defer refusing.Close()
	_, _, err = h.SealAndStore(context.Background(), &CodecGrant{Version: 1, KEMPublicKey: pub}, c, IndexMiss{Hash: c.Hash, URL: refusing.URL + "/b/c"})
	assert.Error(t, err)
	assert.Equal(t, int32(11), puts.Load(), "an answer is the backend's verdict")
}

// RP76-7: a whole read the mirror serves is wrapped by Verify.WholeRead as
// the primary's would be, so its commit says verified.
func TestHost_AMirrorServedWholeReadIsCountedVerified(t *testing.T) {
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer primary.Close()
	mirror := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "5")
		_, _ = w.Write([]byte("hello"))
	}))
	defer mirror.Close()
	cp := &fakeCP{admit: AdmitResponse{Edge: true, AdmitID: "R1", URL: primary.URL + "/b/k", URLs: []string{primary.URL + "/b/k", mirror.URL + "/b/k"},
		Method: http.MethodGet, ExpectStatus: http.StatusOK, Status: http.StatusOK, ContentLength: 5, BodyMD5: "5d41402abc4b2a76b9719d911017c592"}}
	wrapped := false
	h := &Host{CP: cp, Client: NewBackendClient(5 * time.Second), Verify: &Verify{WholeRead: func(b io.ReadCloser, _ string, _ int64) io.ReadCloser {
		wrapped = true
		return b
	}}}
	res, err := h.Read(context.Background(), httptest.NewRequest(http.MethodGet, "https://s3.test/b/k", nil))
	require.NoError(t, err)
	require.False(t, res.PassThrough, "%+v", res)
	_ = res.Body.Close()
	require.NotNil(t, cp.commit)
	assert.Equal(t, 1, cp.commit.ServedURL)
	assert.True(t, wrapped)
	assert.True(t, cp.commit.Verified, "the mirror's whole read is verified like the primary's")
}
