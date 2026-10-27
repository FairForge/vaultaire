package engine

import (
	"errors"
	"net"
	"slices"
	"strings"
	"sync"

	"github.com/aws/smithy-go"
)

// PutOutcome is what CoreEngine.Put tells a caller that passed WithPutOutcome
// about the attempts behind a failed write (RP13-1): the backends whose
// attempt ended WITHOUT AN ANSWER — a deadline (the fixed-bucket driver's
// "no answer within …, twice"), a reset or EOF, a cancel that landed after
// the request went out. Such a backend may still commit the bytes later
// (RP8-11: the body acknowledged, the answer never sent), so a caller that
// owns the address — the chunk store's claim — must not hand it to another
// writer at once.
//
// An attempt is NOT unanswered when the backend answered with an HTTP status
// (any status: an S3 error answer means nothing was committed), when the
// connection was never made (a refused dial, a DNS failure), when the driver
// refused the call before sending (invalid input), or when the backend was
// not asked at all (an open breaker, a request already gone).
//
// Sent is wider: every backend whose failed attempt went out, answered or
// not. An error answer does not prove the store kept nothing — a proxy in
// front of it can answer 503 after the store took the body (Blomp, two
// 64 MiB PUTs on 2026-10-21). A caller writing to an address only it uses
// (a whole object's blob address) queues the address on each of them for
// the collector; a miss costs one delete.
type PutOutcome struct {
	mu         sync.Mutex
	unanswered []string
	sent       []string
	// backendETag is the ETag the backend answered for the write that
	// succeeded (RP72-1), set by a driver that knows it (SetBackendETag);
	// "" = not reported.
	backendETag string
}

// SetBackendETag records the ETag the backend answered for a successful
// write (a driver calls it; nil-safe). The head row records it as
// backend_etag: an edge read signs If-Match from the backend's own answer.
func (o *PutOutcome) SetBackendETag(etag string) {
	if o == nil {
		return
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	o.backendETag = etag
}

// BackendETag is the ETag the backend answered, unquoted ("" when no
// driver reported one).
func (o *PutOutcome) BackendETag() string {
	if o == nil {
		return ""
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	return strings.Trim(strings.TrimSpace(o.backendETag), `"`)
}

// WithPutOutcome asks Put to record its attempts' outcome in o. Drivers
// ignore it.
func WithPutOutcome(o *PutOutcome) PutOption {
	return func(opts *PutOptions) { opts.Outcome = o }
}

// MayHaveLanded reports whether some attempt ended without an answer.
func (o *PutOutcome) MayHaveLanded() bool {
	if o == nil {
		return false
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	return len(o.unanswered) > 0
}

// Unanswered lists the backends whose attempt ended without an answer, in
// the order they were asked.
func (o *PutOutcome) Unanswered() []string {
	if o == nil {
		return nil
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]string(nil), o.unanswered...)
}

// Sent lists the backends whose failed attempt went out (answered or not),
// each once, in the order they were asked.
func (o *PutOutcome) Sent() []string {
	if o == nil {
		return nil
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]string(nil), o.sent...)
}

func (o *PutOutcome) record(backend string, err error) {
	if o == nil || err == nil || putUnsent(err) {
		return
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if !slices.Contains(o.sent, backend) {
		o.sent = append(o.sent, backend)
	}
	if !PutAnsweredOrUnsent(err) {
		o.unanswered = append(o.unanswered, backend)
	}
}

// httpStatusError is any error that carries the status of an HTTP answer:
// aws-sdk-go-v2's *smithyhttp.ResponseError (and awshttp.ResponseError,
// which embeds it), and a test double of a backend's answer.
type httpStatusError interface{ HTTPStatusCode() int }

// PutAnsweredOrUnsent reports whether a failed put attempt certainly
// committed nothing: the backend answered (an HTTP status, an S3 error
// code), the request never reached it (a dial or DNS failure), or the driver
// refused it before sending (ErrInvalidInput, ErrChunkAddress). Everything
// else — a deadline, a reset, a cancel, an unknown error — may have left
// the whole body with the backend. An answer is not proof everywhere: a
// proxy's 503 came after the store kept the body (Sent).
func PutAnsweredOrUnsent(err error) bool {
	var hs httpStatusError
	if errors.As(err, &hs) && hs.HTTPStatusCode() > 0 {
		return true
	}
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) {
		return true
	}
	var perr PermissionError
	if errors.As(err, &perr) {
		return true
	}
	return putUnsent(err)
}

// putUnsent reports whether a failed put attempt never left this process:
// the driver refused it (ErrInvalidInput, ErrChunkAddress) or the
// connection was never made (a dial or DNS failure).
func putUnsent(err error) bool {
	if errors.Is(err, ErrInvalidInput) || errors.Is(err, ErrChunkAddress) {
		return true
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) && opErr.Op == "dial" {
		return true
	}
	var dnsErr *net.DNSError
	return errors.As(err, &dnsErr)
}
