package api

// One request id per S3 request (RP78-3, Stage 1a Prompt 50 Part 0): the
// dispatcher takes the request-id middleware's id (X-Request-Id; a fresh
// one when the handler is called without it) and that is the request's
// x-amz-request-id on every response — success or error — the RequestId of
// every S3 error body, and the request_log row's request_id. The handlers'
// own generateRequestID() calls cannot disagree with it: the writer sets the
// header as the response's header is written (over any other value), and
// WriteS3Error takes the id from the writer it is handed.

import (
	"net/http"

	"github.com/google/uuid"
)

// s3RequestIDWriter carries the request's id and sets it as
// x-amz-request-id when the response's header is written.
type s3RequestIDWriter struct {
	http.ResponseWriter
	id    string
	wrote bool
}

// withS3RequestID wraps w with the request's id: the middleware's, else new.
func withS3RequestID(w http.ResponseWriter) (*s3RequestIDWriter, string) {
	id := w.Header().Get("X-Request-Id")
	if id == "" {
		id = uuid.NewString()
	}
	rw := &s3RequestIDWriter{ResponseWriter: w, id: id}
	w.Header().Set("x-amz-request-id", id)
	return rw, id
}

func (w *s3RequestIDWriter) s3RequestID() string { return w.id }

func (w *s3RequestIDWriter) stamp() {
	if !w.wrote {
		w.wrote = true
		w.Header().Set("x-amz-request-id", w.id)
	}
}

func (w *s3RequestIDWriter) WriteHeader(code int) {
	w.stamp()
	w.ResponseWriter.WriteHeader(code)
}

func (w *s3RequestIDWriter) Write(b []byte) (int, error) {
	w.stamp()
	return w.ResponseWriter.Write(b)
}

// Flush passes through (a streaming handler keeps flushing).
func (w *s3RequestIDWriter) Flush() {
	w.stamp()
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap lets http.ResponseController reach the underlying writer.
func (w *s3RequestIDWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// s3RequestIDOf is the request's id from the first writer in w's Unwrap
// chain that carries one; fallback when none does (or its id is empty).
func s3RequestIDOf(w http.ResponseWriter, fallback string) string {
	for w != nil {
		if c, ok := w.(interface{ s3RequestID() string }); ok {
			if id := c.s3RequestID(); id != "" {
				return id
			}
			return fallback
		}
		u, ok := w.(interface{ Unwrap() http.ResponseWriter })
		if !ok {
			break
		}
		w = u.Unwrap()
	}
	return fallback
}
