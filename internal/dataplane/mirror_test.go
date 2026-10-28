package dataplane

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A mirror leg that stops reading the body is cut loose after mirrorStall:
// the primary gets every byte and answers; the mirror reports no ETag.
func TestPutTwo_AStalledMirrorIsCutLooseAndThePrimaryFinishes(t *testing.T) {
	old := mirrorStall
	mirrorStall = 200 * time.Millisecond
	defer func() { mirrorStall = old }()

	var got []byte
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, _ = io.ReadAll(r.Body)
		w.Header().Set("ETag", `"p"`)
	}))
	defer primary.Close()
	release := make(chan struct{})
	mirror := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b := make([]byte, 1)
		_, _ = r.Body.Read(b)
		<-release // never reads the rest
	}))
	defer mirror.Close()
	defer close(release)

	body := bytes.Repeat([]byte("x"), 8<<20)
	start := time.Now()
	resp, mirrorETag, err := putTwo(context.Background(), NewBackendClient(5*time.Second), http.MethodPut,
		[2]putPair{{URL: primary.URL}, {URL: mirror.URL}}, int64(len(body)), bytes.NewReader(body))
	require.NoError(t, err)
	_ = resp.Body.Close()
	assert.Equal(t, `"p"`, resp.Header.Get("ETag"))
	assert.Equal(t, body, got, "every byte to the primary")
	assert.Empty(t, mirrorETag)
	assert.Less(t, time.Since(start), 10*time.Second)

	// Both healthy: both answer, the same bytes.
	var gotB []byte
	mirror2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotB, _ = io.ReadAll(r.Body)
		w.Header().Set("ETag", `"m"`)
	}))
	defer mirror2.Close()
	resp, mirrorETag, err = putTwo(context.Background(), NewBackendClient(5*time.Second), http.MethodPut,
		[2]putPair{{URL: primary.URL}, {URL: mirror2.URL}}, int64(len(body)), bytes.NewReader(body))
	require.NoError(t, err)
	_ = resp.Body.Close()
	assert.Equal(t, `"m"`, mirrorETag)
	assert.Equal(t, body, gotB)
}
