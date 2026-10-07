package api

import (
	"bytes"
	"context"
	"encoding/xml"
	"fmt"
	"net/http"
	"time"

	"go.uber.org/zap"
)

// Long-running S3 operations (CompleteMultipartUpload, DeleteObjects) behind
// Cloudflare: the edge gives up on an origin that sends no byte for 100 s and
// answers 504, and the client's retry redoes the whole operation. On a slow
// backend both run far longer than that — a complete streams the assembled
// object into ONE backend PUT (~30 MB/s on a Sync bridge: 2 GiB ≈ 70 s), a
// 1,000-key batch delete costs ~1 s per key there (bench 2026-10-07).
//
// runLongS3Op does what AWS documents for these operations: an operation that
// is not done after longOpThreshold commits `200 OK` (Content-Type
// application/xml), then a single space every longOpInterval keeps the
// connection alive, and the outcome follows as the body — the result
// document, or an <Error> document with the operation's own code. SDKs read
// a 200 carrying <Error> on CompleteMultipartUpload/CopyObject as a failure
// (aws-sdk-go-v2, botocore). An operation that finishes before the threshold
// answers exactly as it would without this wrapper (its own status, headers
// and body).
//
// After the commit the response headers are gone: ETag is also in the
// CompleteMultipartUploadResult body (what SDKs read), x-amz-version-id is
// not delivered on that path (AWS has the same limitation for a slow
// complete). The XML declaration is dropped after the whitespace — a
// declaration not at the start of the document is invalid XML and Python's
// parser rejects it.
//
// The operation runs on a context detached from the client's cancellation
// (with its own longOpMaxDuration deadline): once the server holds the
// request it finishes it, so a client that goes away mid-assembly leaves a
// completed object with its head row, quota and upload status written
// together — never bytes on the backend without a row, or a half-done batch.
// The handler waits for the operation either way (graceful shutdown drains it).

const (
	defaultLongOpThreshold = 10 * time.Second
	defaultLongOpInterval  = 10 * time.Second
	// longOpMaxDuration bounds an operation once detached from its client
	// (a 50 GiB complete at 30 MB/s is ~30 min).
	longOpMaxDuration = 6 * time.Hour
)

func (s *Server) longOpTimings() (threshold, interval time.Duration) {
	threshold, interval = s.longOpThreshold, s.longOpInterval
	if threshold <= 0 {
		threshold = defaultLongOpThreshold
	}
	if interval <= 0 {
		interval = defaultLongOpInterval
	}
	return threshold, interval
}

// bufferedResponse captures what an operation writes (its responses are a
// few KiB of XML) so it can be replayed — as is, or after a commit.
type bufferedResponse struct {
	header http.Header
	status int
	body   bytes.Buffer
}

func (b *bufferedResponse) Header() http.Header { return b.header }

func (b *bufferedResponse) WriteHeader(code int) {
	if b.status == 0 {
		b.status = code
	}
}

func (b *bufferedResponse) Write(p []byte) (int, error) {
	if b.status == 0 {
		b.status = http.StatusOK
	}
	return b.body.Write(p)
}

func (b *bufferedResponse) code() int {
	if b.status == 0 {
		return http.StatusOK
	}
	return b.status
}

// runLongS3Op runs op (an S3 handler body) with the keep-alive described
// above. op must not hijack or stream: it writes one small response.
func (s *Server) runLongS3Op(w http.ResponseWriter, r *http.Request, op func(http.ResponseWriter, *http.Request)) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), longOpMaxDuration)
	defer cancel()
	opReq := r.WithContext(ctx)

	rec := &bufferedResponse{header: http.Header{}}
	done := make(chan struct{})
	go func() {
		defer close(done)
		// The goroutine is outside net/http's per-request recover: a panic
		// here would take the process down.
		defer func() {
			if p := recover(); p != nil {
				s.log().Error("long S3 operation panicked", zap.Any("panic", p), zap.String("path", r.URL.Path))
				rec.header = http.Header{}
				rec.status = 0
				rec.body.Reset()
				WriteS3Error(rec, ErrInternalError, r.URL.Path, generateRequestID())
			}
		}()
		op(rec, opReq)
	}()

	threshold, interval := s.longOpTimings()
	timer := time.NewTimer(threshold)
	defer timer.Stop()
	select {
	case <-done:
		for k, v := range rec.header {
			w.Header()[k] = v
		}
		w.WriteHeader(rec.code())
		_, _ = w.Write(rec.body.Bytes())
		return
	case <-timer.C:
	}

	// Commit: 200, then whitespace until the operation is done. Write errors
	// (the client left) are ignored — the operation finishes regardless.
	w.Header().Set("Content-Type", "application/xml")
	w.Header().Set("x-amz-request-id", generateRequestID())
	w.WriteHeader(http.StatusOK)
	rc := http.NewResponseController(w)
	_ = rc.Flush()
	tick := time.NewTicker(interval)
	defer tick.Stop()
	for {
		select {
		case <-done:
			_, _ = w.Write(committedBody(rec, r.URL.Path))
			_ = rc.Flush()
			s.log().Info("long S3 operation answered after keep-alive",
				zap.String("path", r.URL.Path), zap.Int("status", rec.code()))
			return
		case <-tick.C:
			_, _ = w.Write([]byte(" "))
			_ = rc.Flush()
		}
	}
}

// committedBody is the document that follows the keep-alive: the
// operation's own XML without its declaration, or — when it failed with
// something that is not an S3 error document — an InternalError document.
func committedBody(rec *bufferedResponse, resource string) []byte {
	body := bytes.TrimSpace(rec.body.Bytes())
	if bytes.HasPrefix(body, []byte("<?xml")) {
		if end := bytes.Index(body, []byte("?>")); end >= 0 {
			body = bytes.TrimSpace(body[end+2:])
		}
	}
	if rec.code() < 300 && len(body) > 0 {
		return body
	}
	if rec.code() >= 300 && bytes.HasPrefix(body, []byte("<Error")) {
		return body
	}
	doc, err := xml.Marshal(S3Error{
		Code:      ErrInternalError,
		Message:   fmt.Sprintf("%s (status %d)", errorMessages[ErrInternalError], rec.code()),
		Resource:  resource,
		RequestID: generateRequestID(),
	})
	if err != nil {
		return []byte("<Error><Code>InternalError</Code></Error>")
	}
	return doc
}

// log is the server's logger, or a no-op one for bare test servers.
func (s *Server) log() *zap.Logger {
	if s.logger == nil {
		return zap.NewNop()
	}
	return s.logger
}
