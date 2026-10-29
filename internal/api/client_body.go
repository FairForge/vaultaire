package api

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"

	"github.com/FairForge/vaultaire/internal/auth"
	"github.com/FairForge/vaultaire/internal/engine"
	"github.com/prometheus/client_golang/prometheus"
	"go.uber.org/zap"
)

// WP-R2-3 (R2-09, R108-9): a request body the client did not finish sending
// is the client's error, on every PUT path — whole, sealed, chunked and
// UploadPart. The body is tagged where it is read (clientBody): every error
// its reads return wraps engine.ErrCallerAborted (the one sentinel, Prompt
// 2b.2 C1 — the engine's own source check, isBackendFailure and the
// failover walk already know it), so whatever reads it — the chunker, the
// sealer, the MD5 tee, a driver that wraps with %w — hands the handler an
// error it can tell from the backend's. The reader also remembers that it
// failed, for a driver that does not wrap. The answer is 400 IncompleteBody
// (408 RequestTimeout for a body below the Sync bridge's minimum rate), the
// breaker is never charged, nothing is committed, and the request is
// counted in vaultaire_client_body_errors_total — never as a 5xx, never as
// an Error line.

// errAWSChunkedFraming: an aws-chunked body that is not well framed — a
// chunk size that is not hex, a chunk not followed by CRLF, a stream that
// ends before its terminal chunk.
var errAWSChunkedFraming = errors.New("aws-chunked framing")

// errAWSChunkedSignature: a signed aws-chunked body whose chunk (or trailer)
// signature is missing or does not verify (WP-R15-1): the body is not what
// was signed — 403 SignatureDoesNotMatch, nothing committed.
var errAWSChunkedSignature = errors.New("aws-chunked signature")

// Reasons of vaultaire_client_body_errors_total.
const (
	clientBodyShort   = "short"    // the body ended or broke before its declared length
	clientBodyGone    = "gone"     // the client went away (the request context ended)
	clientBodyFraming = "framing"  // aws-chunked framing the decoder could not follow
	clientBodyTooSlow = "too_slow" // below the backend's minimum rate (engine.ErrSourceTooSlow)
)

var clientBodyErrors = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "vaultaire_client_body_errors_total",
	Help: "Uploads whose request body the client did not finish sending, by operation (PutObject | UploadPart) and reason: short | gone | framing | too_slow. Answered 400 IncompleteBody (408 for too_slow) and never charged to a backend's breaker (WP-R2-3).",
}, []string{"op", "reason"})

func init() {
	for _, op := range []string{"PutObject", "UploadPart"} {
		for _, r := range []string{clientBodyShort, clientBodyGone, clientBodyFraming, clientBodyTooSlow} {
			clientBodyErrors.WithLabelValues(op, r)
		}
	}
}

// clientBody is the request body as every PUT path reads it.
type clientBody struct {
	r io.Reader

	mu  sync.Mutex
	err error // the first read failure (never io.EOF)
}

func newClientBody(r io.Reader) *clientBody { return &clientBody{r: r} }

func (c *clientBody) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	if err == nil || errors.Is(err, io.EOF) {
		return n, err
	}
	c.mu.Lock()
	if c.err == nil {
		c.err = err
	}
	c.mu.Unlock()
	if errors.Is(err, engine.ErrCallerAborted) {
		return n, err
	}
	return n, fmt.Errorf("%w: %w", engine.ErrCallerAborted, err)
}

// failed is the body's own read failure, if any.
func (c *clientBody) failed() error {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.err
}

// clientBodyFault reports whether err — what a PUT path ended with — is the
// client's body: tagged by the body (or a driver that kept the tag), the
// body's own read failed, or the client went away (a chunked write's
// workers see only their cancelled context).
func clientBodyFault(ctx context.Context, body *clientBody, err error) bool {
	if err == nil {
		return false
	}
	return errors.Is(err, engine.ErrCallerAborted) || body.failed() != nil || errors.Is(ctx.Err(), context.Canceled)
}

func clientBodyReason(ctx context.Context, body *clientBody, err error) string {
	cause := body.failed()
	if cause == nil {
		cause = err
	}
	switch {
	case errors.Is(cause, engine.ErrSourceTooSlow) || errors.Is(err, engine.ErrSourceTooSlow):
		return clientBodyTooSlow
	case errors.Is(cause, errAWSChunkedFraming):
		return clientBodyFraming
	case errors.Is(ctx.Err(), context.Canceled):
		return clientBodyGone
	default:
		return clientBodyShort
	}
}

// answerClientBody answers a body the client did not finish: counted, one
// Info line, 400 IncompleteBody (408 RequestTimeout for too_slow). A client
// that went away is answered the same way — nobody reads it, the access log
// records a 4xx.
func answerClientBody(w http.ResponseWriter, r *http.Request, logger *zap.Logger, op string, body *clientBody, err error, fields ...zap.Field) {
	// A payload that does not hash to its signed declaration arrived whole:
	// it keeps its own code and is no unfinished body.
	if errors.Is(err, auth.ErrContentSHA256Mismatch) || errors.Is(body.failed(), auth.ErrContentSHA256Mismatch) {
		logger.Info("request body does not match its signed x-amz-content-sha256",
			append(fields, zap.String("op", op), zap.Error(err))...)
		WriteS3Error(w, ErrXAmzContentSHA256Mismatch, r.URL.Path, generateRequestID())
		return
	}
	// So does a signed stream whose chunks are not what was signed
	// (WP-R15-1) — counted as a failure against a real key — and a body that
	// does not match its x-amz-checksum-*.
	if errors.Is(err, errAWSChunkedSignature) || errors.Is(body.failed(), errAWSChunkedSignature) {
		logger.Info("aws-chunked body does not match its chunk signatures",
			append(fields, zap.String("op", op), zap.Error(err))...)
		recordAuthFailure(r, "chunk_signature_mismatch", true)
		WriteS3Error(w, ErrSignatureDoesNotMatch, r.URL.Path, generateRequestID())
		return
	}
	if errors.Is(err, errChecksumMismatch) || errors.Is(body.failed(), errChecksumMismatch) {
		logger.Info("request body does not match its x-amz-checksum",
			append(fields, zap.String("op", op), zap.Error(err))...)
		WriteS3Error(w, ErrXAmzContentChecksumMismatch, r.URL.Path, generateRequestID())
		return
	}
	reason := clientBodyReason(r.Context(), body, err)
	clientBodyErrors.WithLabelValues(op, reason).Inc()
	code := ErrIncompleteBody
	if reason == clientBodyTooSlow {
		code = ErrRequestTimeout
	}
	logger.Info("request body not received: the client's",
		append(fields, zap.String("op", op), zap.String("reason", reason), zap.Error(err))...)
	WriteS3Error(w, code, r.URL.Path, generateRequestID())
}
