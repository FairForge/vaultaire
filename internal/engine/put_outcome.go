package engine

import (
	"errors"
	"net"
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
type PutOutcome struct {
	mu         sync.Mutex
	unanswered []string
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

func (o *PutOutcome) record(backend string, err error) {
	if o == nil || err == nil || PutAnsweredOrUnsent(err) {
		return
	}
	o.mu.Lock()
	o.unanswered = append(o.unanswered, backend)
	o.mu.Unlock()
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
// the whole body with the backend.
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
