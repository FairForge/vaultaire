package api

// WP-R2-3 (R2-09 P1, R108-9): a request body the client did not finish
// sending is the client's error, on every PUT path. Over a real socket: a
// client that stops mid-body (half-closed, so it still reads the answer)
// gets 400 IncompleteBody; one that went away is recorded as a 4xx; the
// breaker is never charged, nothing is committed or staged, the reservation
// is released and no Error line is logged. "Before" = these tests on
// origin/main (5c383e8).

import (
	"bufio"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/FairForge/vaultaire/internal/auth"
	"github.com/FairForge/vaultaire/internal/engine"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

// bodyCut is how a test client stops sending.
type bodyCut int

const (
	sendAll   bodyCut = iota // the whole body (framing tests)
	halfClose                // stops, closes its write side, reads the answer
	killed                   // stops and closes the connection (curl killed)
)

// liveS3 serves the fixture's tenant over a real socket, through the
// logging middleware (the 5xx count) and handleS3Request.
type liveS3 struct {
	srv   *httptest.Server
	codes chan int // the status each request finished with
}

type statusRecorder struct {
	http.ResponseWriter
	code int
}

func (s *statusRecorder) WriteHeader(code int) {
	if s.code == 0 {
		s.code = code
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Write(p []byte) (int, error) {
	if s.code == 0 {
		s.code = http.StatusOK
	}
	return s.ResponseWriter.Write(p)
}

func (f *mpChunkFixture) live(t *testing.T) *liveS3 {
	t.Helper()
	l := &liveS3{codes: make(chan int, 64)}
	l.srv = httptest.NewServer(f.server.loggingMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := &statusRecorder{ResponseWriter: w}
		f.server.handleS3Request(rec, r.WithContext(s3Ctx(r.Context(), f.tenant)))
		l.codes <- rec.code
	})))
	t.Cleanup(l.srv.Close)
	return l
}

// send writes a request with Content-Length declared and only body on the
// wire, then cuts as asked. It returns the status the client read (0 when
// it went away) and the status the server finished with.
func (l *liveS3) send(t *testing.T, method, target string, hdr map[string]string, declared int, body []byte, cut bodyCut) (client, server int, respBody string) {
	t.Helper()
	conn, err := net.Dial("tcp", strings.TrimPrefix(l.srv.URL, "http://"))
	require.NoError(t, err)
	defer func() { _ = conn.Close() }()
	var b strings.Builder
	fmt.Fprintf(&b, "%s %s HTTP/1.1\r\nHost: test\r\nContent-Length: %d\r\n", method, target, declared)
	for k, v := range hdr {
		fmt.Fprintf(&b, "%s: %s\r\n", k, v)
	}
	b.WriteString("\r\n")
	_, err = conn.Write([]byte(b.String()))
	require.NoError(t, err)
	_, err = conn.Write(body)
	require.NoError(t, err)
	switch cut {
	case halfClose:
		require.NoError(t, conn.(*net.TCPConn).CloseWrite())
		fallthrough
	case sendAll:
		require.NoError(t, conn.SetReadDeadline(time.Now().Add(20*time.Second)))
		resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
		require.NoError(t, err, "the client is still there: it gets an answer")
		raw, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		client, respBody = resp.StatusCode, string(raw)
	case killed:
		time.Sleep(20 * time.Millisecond)
		_ = conn.Close()
	}
	select {
	case server = <-l.codes:
	case <-time.After(20 * time.Second):
		t.Fatal("the handler never finished")
	}
	return client, server, respBody
}

// clientBodyErrors scrapes vaultaire_client_body_errors_total{op} (every
// reason, or one) from the server's /metrics.
func (f *mpChunkFixture) clientBodyErrors(t *testing.T, op, reason string) float64 {
	t.Helper()
	f.server.initMetrics()
	w := httptest.NewRecorder()
	f.server.promHandler.ServeHTTP(w, httptest.NewRequest("GET", "/metrics", nil))
	var total float64
	for _, line := range strings.Split(w.Body.String(), "\n") {
		if !strings.HasPrefix(line, "vaultaire_client_body_errors_total{") || !strings.Contains(line, `op="`+op+`"`) {
			continue
		}
		if reason != "" && !strings.Contains(line, `reason="`+reason+`"`) {
			continue
		}
		v, err := strconv.ParseFloat(line[strings.LastIndexByte(line, ' ')+1:], 64)
		require.NoError(t, err)
		total += v
	}
	return total
}

// nothingKept: no head row, no bytes on the backend, nothing queued but
// this write's own address (a body that went out is queued whoever cut it:
// the store may have kept it — placeObject), the reservation given back, no
// Error line, the 5xx count and the primary's breaker untouched.
func (f *mpChunkFixture) nothingKept(t *testing.T, key string, logs *observer.ObservedLogs, errorsBefore int64) {
	t.Helper()
	assert.Zero(t, f.headRows(t, key), "no head row")
	assert.Zero(t, f.used(t), "the reservation is released")
	var other, own int
	require.NoError(t, f.db.QueryRow(`SELECT COUNT(*) FILTER (WHERE NOT (storage_key LIKE $2 AND reason = 'orphan_write')),
		COUNT(*) FILTER (WHERE storage_key LIKE $2 AND reason = 'orphan_write') FROM retired_blobs WHERE tenant_id = $1`,
		f.tenantID, key+"@%").Scan(&other, &own))
	assert.Zero(t, other, "the retired queue holds nothing else")
	assert.LessOrEqual(t, own, 1, "at most this write's own address")
	var files []string
	_ = filepath.Walk(f.fixed.dir, func(p string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() {
			files = append(files, p)
		}
		return nil
	})
	assert.Empty(t, files, "nothing on the backend")
	for _, e := range logs.FilterLevelExact(zap.ErrorLevel).All() {
		t.Errorf("an Error line for the client's body: %s %v", e.Message, e.ContextMap())
	}
	assert.Equal(t, errorsBefore, atomic.LoadInt64(&f.server.errorCount), "never a 5xx")
	assert.Equal(t, "closed", f.server.engine.GetFailoverStatus()["fixed"], "the primary's breaker is not charged")
}

func (f *mpChunkFixture) observe() *observer.ObservedLogs {
	core, logs := observer.New(zap.InfoLevel)
	f.server.logger = zap.New(core)
	return logs
}

func TestWPR23_PutBodiesTheClientDidNotFinish_AreItsError(t *testing.T) {
	cases := []struct {
		name     string
		withKey  bool
		sse      bool
		declared int
		sent     int
	}{
		{name: "whole", declared: 1000, sent: 400},
		{name: "chunked", declared: 64 << 10, sent: 20 << 10}, // R108-9: 500 before
		{name: "sealed", withKey: true, sse: true, declared: 1000, sent: 400},
		{name: "chunk-encrypted", withKey: true, sse: true, declared: 64 << 10, sent: 20 << 10},
	}
	for _, tc := range cases {
		for _, cut := range []bodyCut{halfClose, killed} {
			t.Run(fmt.Sprintf("%s/cut=%d", tc.name, cut), func(t *testing.T) {
				f := setupMPChunkFixture(t, tc.withKey)
				if tc.sse {
					f.bucketSet(t, "sse_enabled = TRUE")
				}
				l := f.live(t)
				logs := f.observe()
				errorsBefore := atomic.LoadInt64(&f.server.errorCount)
				before := f.clientBodyErrors(t, "PutObject", "")

				client, server, body := l.send(t, "PUT", "/test-bucket/cut.bin", nil, tc.declared, vbody(f, tc.declared, 3)[:tc.sent], cut)

				if cut == halfClose {
					assert.Equal(t, http.StatusBadRequest, client, body)
					assert.Contains(t, body, "<Code>IncompleteBody</Code>")
				}
				assert.Equal(t, http.StatusBadRequest, server)
				f.nothingKept(t, "cut.bin", logs, errorsBefore)
				assert.Equal(t, before+1, f.clientBodyErrors(t, "PutObject", ""), "counted once as the client's")
			})
		}
	}
}

func TestWPR23_UploadPartTheClientDidNotFinish_IsItsError(t *testing.T) {
	f := setupMPChunkFixture(t, false)
	l := f.live(t)
	w := f.req(t, "POST", "/test-bucket/mp.bin?uploads", nil, nil)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var init InitiateMultipartUploadResult
	require.NoError(t, xml.Unmarshal(w.Body.Bytes(), &init))
	t.Cleanup(func() { _ = os.RemoveAll(multipartDir(init.UploadID)) })
	logs := f.observe()
	errorsBefore := atomic.LoadInt64(&f.server.errorCount)
	before := f.clientBodyErrors(t, "UploadPart", "")

	client, server, body := l.send(t, "PUT", "/test-bucket/mp.bin?partNumber=1&uploadId="+init.UploadID, nil,
		64<<10, vbody(f, 64<<10, 4)[:10<<10], halfClose)

	assert.Equal(t, http.StatusBadRequest, client, body)
	assert.Contains(t, body, "<Code>IncompleteBody</Code>")
	assert.Equal(t, http.StatusBadRequest, server)
	assert.Zero(t, f.uploadHolds(t, init.UploadID), "the part's reservation is given back")
	entries, _ := os.ReadDir(multipartDir(init.UploadID))
	assert.Empty(t, entries, "no part and no temp file staged")
	assert.Zero(t, f.used(t))
	assert.Zero(t, logs.FilterLevelExact(zap.ErrorLevel).Len())
	assert.Equal(t, errorsBefore, atomic.LoadInt64(&f.server.errorCount))
	assert.Equal(t, before+1, f.clientBodyErrors(t, "UploadPart", ""))
}

// awsChunked frames payload as aws-chunked with one data chunk.
func awsChunked(payload []byte) []byte {
	return []byte(fmt.Sprintf("%x;chunk-signature=0\r\n%s\r\n0;chunk-signature=0\r\n\r\n", len(payload), payload))
}

func TestWPR23_AWSChunkedFramingErrors_AreTheClients(t *testing.T) {
	payload := []byte("0123456789abcdef")
	cases := map[string][]byte{
		// A framed body ends with its terminal chunk and every chunk with
		// CRLF; anything else is the client's framing, never a 500 and
		// never a body that "ended" early.
		"truncated mid-chunk":   []byte("10;chunk-signature=0\r\n012345"),
		"no terminal chunk":     []byte("10;chunk-signature=0\r\n0123456789abcdef\r\n"),
		"size not hex":          []byte("zz;chunk-signature=0\r\n0123456789abcdef\r\n0\r\n\r\n"),
		"no CRLF after a chunk": []byte("10;chunk-signature=0\r\n0123456789abcdefXX0\r\n\r\n"),
	}
	for _, path := range []string{"PutObject", "UploadPart", "chunked PutObject"} {
		for name, framed := range cases {
			t.Run(path+"/"+name, func(t *testing.T) {
				f := setupMPChunkFixture(t, false)
				if path == "chunked PutObject" {
					f.server.chunkingThreshold = 8
				}
				l := f.live(t)
				logs := f.observe()
				target := "/test-bucket/framed.bin"
				op := "PutObject"
				if path == "UploadPart" {
					w := f.req(t, "POST", "/test-bucket/framed.bin?uploads", nil, nil)
					require.Equal(t, http.StatusOK, w.Code)
					var init InitiateMultipartUploadResult
					require.NoError(t, xml.Unmarshal(w.Body.Bytes(), &init))
					t.Cleanup(func() { _ = os.RemoveAll(multipartDir(init.UploadID)) })
					target += "?partNumber=1&uploadId=" + init.UploadID
					op = "UploadPart"
				}
				before := f.clientBodyErrors(t, op, "framing")
				hdr := map[string]string{
					"x-amz-content-sha256":         "STREAMING-UNSIGNED-PAYLOAD-TRAILER",
					"Content-Encoding":             "aws-chunked",
					"x-amz-decoded-content-length": strconv.Itoa(len(payload)),
				}

				client, _, body := l.send(t, "PUT", target, hdr, len(framed), framed, sendAll)

				assert.Equal(t, http.StatusBadRequest, client, body)
				assert.Contains(t, body, "<Code>IncompleteBody</Code>")
				assert.Zero(t, f.headRows(t, "framed.bin"))
				assert.Zero(t, f.used(t))
				assert.Zero(t, logs.FilterLevelExact(zap.ErrorLevel).Len())
				assert.Equal(t, before+1, f.clientBodyErrors(t, op, "framing"))
			})
		}
	}
	t.Run("a well-framed body is stored", func(t *testing.T) {
		f := setupMPChunkFixture(t, false)
		l := f.live(t)
		framed := awsChunked(payload)
		hdr := map[string]string{"x-amz-content-sha256": "STREAMING-UNSIGNED-PAYLOAD-TRAILER", "Content-Encoding": "aws-chunked",
			"x-amz-decoded-content-length": strconv.Itoa(len(payload))}
		client, _, body := l.send(t, "PUT", "/test-bucket/ok.bin", hdr, len(framed), framed, sendAll)
		require.Equal(t, http.StatusOK, client, body)
		assert.Equal(t, payload, f.getBytes(t, "ok.bin", ""))
	})
}

func TestWPR23_TwentyAbortsInAMinute_LeaveTheBreakerClosed(t *testing.T) {
	// R2-09's blast radius: five aborted uploads used to open the primary's
	// breaker for 30 s (misplaced writes, fake 404s for everyone).
	f := setupMPChunkFixture(t, false)
	l := f.live(t)
	errorsBefore := atomic.LoadInt64(&f.server.errorCount)
	for i := 0; i < 20; i++ {
		size := 1000
		if i%2 == 1 {
			size = 64 << 10 // the chunked path too
		}
		_, server, _ := l.send(t, "PUT", fmt.Sprintf("/test-bucket/abort-%d.bin", i), nil, size, vbody(f, size, byte(i))[:size/3], killed)
		assert.Equal(t, http.StatusBadRequest, server, "abort %d", i)
	}
	assert.Equal(t, "closed", f.server.engine.GetFailoverStatus()["fixed"])
	assert.Equal(t, errorsBefore, atomic.LoadInt64(&f.server.errorCount))
	assert.NoError(t, f.server.engine.CanAsk("fixed"))
	w := f.req(t, "PUT", "/test-bucket/after.bin", []byte("still served"), nil)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Equal(t, []byte("still served"), f.getBytes(t, "after.bin", ""))
	assert.Zero(t, f.used(t)-int64(len("still served")))
}

func TestWPR23_ABackendFailureIsStillABackendFailure(t *testing.T) {
	// The deliberate 5xx stays one: a backend that fails a complete body is
	// a 5xx (counted), it charges the breaker, and it is no client error.
	for _, size := range []int{1000, 64 << 10} {
		t.Run(strconv.Itoa(size), func(t *testing.T) {
			f := setupMPChunkFixture(t, false)
			l := f.live(t)
			f.fixed.failPut.Store(true)
			errorsBefore := atomic.LoadInt64(&f.server.errorCount)
			before := f.clientBodyErrors(t, "PutObject", "")

			client, _, body := l.send(t, "PUT", "/test-bucket/fail.bin", nil, size, vbody(f, size, 9), sendAll)

			assert.GreaterOrEqual(t, client, 500, body)
			assert.Equal(t, errorsBefore+1, atomic.LoadInt64(&f.server.errorCount), "counted as a 5xx")
			assert.Equal(t, before, f.clientBodyErrors(t, "PutObject", ""), "never the client's")
			assert.Zero(t, f.headRows(t, "fail.bin"))
			assert.Zero(t, f.used(t))
		})
	}
}

func TestWPR23_ClientBody_TagsEveryReadErrorAndRemembersIt(t *testing.T) {
	cut := errors.New("read tcp: connection reset by peer")
	b := newClientBody(io.MultiReader(strings.NewReader("abc"), &readFails{err: cut}))

	got, err := io.ReadAll(b)

	assert.Equal(t, "abc", string(got))
	assert.ErrorIs(t, err, engine.ErrCallerAborted, "tagged where it is read")
	assert.ErrorIs(t, err, cut, "the cause stays in the chain")
	assert.Equal(t, cut, b.failed())
	clean := newClientBody(strings.NewReader("abc"))
	_, err = io.ReadAll(clean)
	assert.NoError(t, err)
	assert.NoError(t, clean.failed(), "EOF is no failure")
}

func TestWPR23_ABodyBelowTheMinimumRateIs408AndCountedTooSlow(t *testing.T) {
	// The Sync driver's slow-source guard (Prompt 2b.2) keeps its 408.
	before := testutil.ToFloat64(clientBodyErrors.WithLabelValues("PutObject", clientBodyTooSlow))
	w := httptest.NewRecorder()
	r := httptest.NewRequest("PUT", "/b/k", nil)

	answerClientBody(w, r, zap.NewNop(), "PutObject", newClientBody(strings.NewReader("")),
		fmt.Errorf("sync put: %w", engine.ErrSourceTooSlow))

	assert.Equal(t, http.StatusRequestTimeout, w.Code)
	assert.Contains(t, w.Body.String(), "<Code>RequestTimeout</Code>")
	assert.Equal(t, before+1, testutil.ToFloat64(clientBodyErrors.WithLabelValues("PutObject", clientBodyTooSlow)))
}

func TestWPR23_ASignedHashMismatchKeepsItsCode(t *testing.T) {
	w := httptest.NewRecorder()
	r := httptest.NewRequest("PUT", "/b/k", nil)
	body := newClientBody(&readFails{err: auth.ErrContentSHA256Mismatch})
	_, err := io.ReadAll(body)

	answerClientBody(w, r, zap.NewNop(), "PutObject", body, err)

	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "<Code>"+ErrXAmzContentSHA256Mismatch+"</Code>")
}

type readFails struct{ err error }

func (f *readFails) Read([]byte) (int, error) { return 0, f.err }
