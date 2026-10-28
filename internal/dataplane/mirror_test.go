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

// A mirror leg that stops reading the body is cut loose once its buffer is
// full: the primary gets every byte and answers; the mirror reports no ETag.
func TestPutTwo_AStalledMirrorIsCutLooseAndThePrimaryFinishes(t *testing.T) {
	old := mirrorMaxLag
	mirrorMaxLag = 2 << 20
	defer func() { mirrorMaxLag = old }()

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
	resp, mirrorETag, cut, err := putTwo(context.Background(), NewBackendClient(5*time.Second), http.MethodPut,
		[2]putPair{{URL: primary.URL}, {URL: mirror.URL}}, int64(len(body)), bytes.NewReader(body))
	require.NoError(t, err)
	_ = resp.Body.Close()
	assert.Equal(t, `"p"`, resp.Header.Get("ETag"))
	assert.Equal(t, body, got, "every byte to the primary")
	assert.Empty(t, mirrorETag)
	assert.True(t, cut)
	assert.Less(t, time.Since(start), 10*time.Second)
	mirrorMaxLag = old // the healthy half runs with the real buffer

	// Both healthy: both answer, the same bytes.
	var gotB []byte
	mirror2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotB, _ = io.ReadAll(r.Body)
		w.Header().Set("ETag", `"m"`)
	}))
	defer mirror2.Close()
	resp, mirrorETag, cut, err = putTwo(context.Background(), NewBackendClient(5*time.Second), http.MethodPut,
		[2]putPair{{URL: primary.URL}, {URL: mirror2.URL}}, int64(len(body)), bytes.NewReader(body))
	require.NoError(t, err)
	_ = resp.Body.Close()
	assert.Equal(t, `"m"`, mirrorETag)
	assert.False(t, cut)
	assert.Equal(t, body, gotB)
}

// RP76-2: a mirror slower than the primary trails it by at most its
// buffer (mirrorMaxLag) and is cut there; the primary's writes never wait
// for it, so the primary receives every byte at its own pace.
func TestPutTwo_TheMirrorNeverHoldsThePrimaryPastItsBuffer(t *testing.T) {
	var got []byte
	var primaryDone time.Time
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, _ = io.ReadAll(r.Body)
		primaryDone = time.Now()
		w.Header().Set("ETag", `"p"`)
	}))
	defer primary.Close()
	mirror := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 4 MiB/s: 16 s for the 64 MiB body.
		b := make([]byte, 64<<10)
		for {
			_, err := r.Body.Read(b)
			if err != nil {
				return
			}
			time.Sleep(16 * time.Millisecond)
		}
	}))
	defer mirror.Close()

	body := bytes.Repeat([]byte("y"), 64<<20)
	start := time.Now()
	resp, mirrorETag, cut, err := putTwo(context.Background(), NewBackendClient(30*time.Second), http.MethodPut,
		[2]putPair{{URL: primary.URL}, {URL: mirror.URL}}, int64(len(body)), bytes.NewReader(body))
	require.NoError(t, err)
	_ = resp.Body.Close()
	assert.Equal(t, body, got, "every byte to the primary")
	assert.Less(t, primaryDone.Sub(start), 8*time.Second, "the primary kept its own pace (the mirror's is 16 s)")
	assert.True(t, cut, "the mirror is cut once it trails by its buffer")
	assert.Empty(t, mirrorETag)
}

// RP76-2: a mirror that took every byte but does not answer is given
// mirrorAfter past the primary's answer, then cut.
func TestPutTwo_AMirrorThatDoesNotAnswerIsCutAfterThePrimary(t *testing.T) {
	old := mirrorAfter
	mirrorAfter = 200 * time.Millisecond
	defer func() { mirrorAfter = old }()
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("ETag", `"p"`)
	}))
	defer primary.Close()
	release := make(chan struct{})
	mirror := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer mirror.Close()
	defer close(release)

	body := bytes.Repeat([]byte("z"), 1<<20)
	start := time.Now()
	resp, mirrorETag, cut, err := putTwo(context.Background(), NewBackendClient(30*time.Second), http.MethodPut,
		[2]putPair{{URL: primary.URL}, {URL: mirror.URL}}, int64(len(body)), bytes.NewReader(body))
	require.NoError(t, err)
	_ = resp.Body.Close()
	assert.Less(t, time.Since(start), 5*time.Second)
	assert.True(t, cut)
	assert.Empty(t, mirrorETag)
}
