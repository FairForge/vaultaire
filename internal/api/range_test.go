package api

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseRangeHeader_Valid(t *testing.T) {
	tests := []struct {
		name       string
		header     string
		totalSize  int64
		wantStart  int64
		wantEnd    int64
		wantLength int64
	}{
		{
			name:       "full range",
			header:     "bytes=0-99",
			totalSize:  1000,
			wantStart:  0,
			wantEnd:    99,
			wantLength: 100,
		},
		{
			name:       "middle range",
			header:     "bytes=100-199",
			totalSize:  1000,
			wantStart:  100,
			wantEnd:    199,
			wantLength: 100,
		},
		{
			name:       "suffix range",
			header:     "bytes=-100",
			totalSize:  1000,
			wantStart:  900,
			wantEnd:    999,
			wantLength: 100,
		},
		{
			name:       "open-ended range",
			header:     "bytes=500-",
			totalSize:  1000,
			wantStart:  500,
			wantEnd:    999,
			wantLength: 500,
		},
		{
			name:       "single byte",
			header:     "bytes=0-0",
			totalSize:  100,
			wantStart:  0,
			wantEnd:    0,
			wantLength: 1,
		},
		{
			name:       "last byte",
			header:     "bytes=-1",
			totalSize:  100,
			wantStart:  99,
			wantEnd:    99,
			wantLength: 1,
		},
		{
			name:       "end clamped to total",
			header:     "bytes=0-9999",
			totalSize:  100,
			wantStart:  0,
			wantEnd:    99,
			wantLength: 100,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rng, err := parseRangeHeader(tc.header, tc.totalSize)
			require.NoError(t, err)
			assert.Equal(t, tc.wantStart, rng.start)
			assert.Equal(t, tc.wantEnd, rng.end)
			assert.Equal(t, tc.wantLength, rng.length)
		})
	}
}

func TestParseRangeHeader_Invalid(t *testing.T) {
	tests := []struct {
		name      string
		header    string
		totalSize int64
	}{
		{"empty", "", 100},
		{"no bytes prefix", "0-99", 100},
		{"multi-range", "bytes=0-50, 100-150", 1000},
		{"start after end", "bytes=100-50", 1000},
		{"start past total", "bytes=1000-2000", 100},
		{"negative not a suffix", "bytes=-0", 100},
		{"garbage", "bytes=abc-def", 100},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseRangeHeader(tc.header, tc.totalSize)
			assert.Error(t, err)
		})
	}
}

func TestServeRange_WithSeeker(t *testing.T) {
	content := []byte("Hello, World! This is range test content.")
	reader := bytes.NewReader(content)

	rng := &httpRange{start: 7, end: 11, length: 5}

	w := httptest.NewRecorder()
	err := serveRange(w, reader, rng, int64(len(content)), "text/plain")
	require.NoError(t, err)

	assert.Equal(t, http.StatusPartialContent, w.Code)
	assert.Equal(t, "bytes 7-11/41", w.Header().Get("Content-Range"))
	assert.Equal(t, "5", w.Header().Get("Content-Length"))
	assert.Equal(t, "text/plain", w.Header().Get("Content-Type"))
	assert.Equal(t, "bytes", w.Header().Get("Accept-Ranges"))

	body, _ := io.ReadAll(w.Body)
	assert.Equal(t, "World", string(body))
}

func TestServeRange_WithNonSeeker(t *testing.T) {
	content := []byte("Hello, World! This is range test content.")
	reader := io.NopCloser(strings.NewReader(string(content)))

	rng := &httpRange{start: 7, end: 11, length: 5}

	w := httptest.NewRecorder()
	err := serveRange(w, reader, rng, int64(len(content)), "application/octet-stream")
	require.NoError(t, err)

	assert.Equal(t, http.StatusPartialContent, w.Code)
	assert.Equal(t, "bytes 7-11/41", w.Header().Get("Content-Range"))
	assert.Equal(t, "5", w.Header().Get("Content-Length"))

	body, _ := io.ReadAll(w.Body)
	assert.Equal(t, "World", string(body))
}

func TestServeRange_EntireFile(t *testing.T) {
	content := []byte("ABCDE")
	reader := bytes.NewReader(content)

	rng := &httpRange{start: 0, end: 4, length: 5}

	w := httptest.NewRecorder()
	err := serveRange(w, reader, rng, 5, "text/plain")
	require.NoError(t, err)

	assert.Equal(t, http.StatusPartialContent, w.Code)
	assert.Equal(t, "bytes 0-4/5", w.Header().Get("Content-Range"))

	body, _ := io.ReadAll(w.Body)
	assert.Equal(t, "ABCDE", string(body))
}

func TestWriteRangeNotSatisfiable(t *testing.T) {
	w := httptest.NewRecorder()
	writeRangeNotSatisfiable(w, 500)

	assert.Equal(t, http.StatusRequestedRangeNotSatisfiable, w.Code)
	assert.Equal(t, "bytes */500", w.Header().Get("Content-Range"))
}
