package engine

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/aws/smithy-go"
	smithyhttp "github.com/aws/smithy-go/transport/http"
	"go.uber.org/zap"
)

type CircuitState int

const (
	StateClosed   CircuitState = iota // healthy — requests flow through
	StateOpen                         // broken — requests are rejected
	StateHalfOpen                     // probing — one request allowed to test recovery
)

func (s CircuitState) String() string {
	switch s {
	case StateClosed:
		return "closed"
	case StateOpen:
		return "open"
	case StateHalfOpen:
		return "half-open"
	default:
		return "unknown"
	}
}

const (
	failureThreshold = 5
	failureWindow    = 60 * time.Second
	openDuration     = 30 * time.Second
)

type BackendCircuitBreaker struct {
	mu           sync.Mutex
	state        CircuitState
	failures     []time.Time
	lastOpenedAt time.Time
}

func NewBackendCircuitBreaker() *BackendCircuitBreaker {
	return &BackendCircuitBreaker{
		state: StateClosed,
	}
}

func (b *BackendCircuitBreaker) Allow() bool {
	b.mu.Lock()
	defer b.mu.Unlock()

	switch b.state {
	case StateClosed:
		return true
	case StateOpen:
		if time.Since(b.lastOpenedAt) >= openDuration {
			b.state = StateHalfOpen
			return true
		}
		return false
	case StateHalfOpen:
		return true
	default:
		return true
	}
}

func (b *BackendCircuitBreaker) RecordSuccess() {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.state = StateClosed
	b.failures = nil
}

func (b *BackendCircuitBreaker) RecordFailure() {
	b.mu.Lock()
	defer b.mu.Unlock()

	now := time.Now()
	cutoff := now.Add(-failureWindow)
	var recent []time.Time
	for _, t := range b.failures {
		if t.After(cutoff) {
			recent = append(recent, t)
		}
	}
	recent = append(recent, now)
	b.failures = recent

	if len(recent) >= failureThreshold {
		b.state = StateOpen
		b.lastOpenedAt = now
	}
}

func (b *BackendCircuitBreaker) State() CircuitState {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.state == StateOpen && time.Since(b.lastOpenedAt) >= openDuration {
		b.state = StateHalfOpen
	}
	return b.state
}

// isBackendFailure reports whether err indicates the backend itself is
// unhealthy (timeout, connection refused, DNS failure, 5xx, dead key) rather
// than a benign client-level outcome. Only the former should count toward
// opening a circuit breaker. Client-level outcomes — object not found, quota
// exceeded, invalid input, permission denied, the client hanging up — are
// normal traffic and must never trip the breaker; otherwise a 404 storm, a
// quota-capped tenant or a burst of aborted uploads could take a healthy
// backend out of routing for openDuration (and, with it, misplace writes on
// the next candidate and turn reads into fake 404s).
//
// The API layer's isObjectMissingErr (internal/api/not_found.go) is the other
// half of this taxonomy — keep the two in sync.
func isBackendFailure(err error) bool {
	if err == nil {
		return false
	}
	// The CLIENT went away (HAProxy cut the connection, an upload was
	// aborted): the request context is cancelled and the SDK hands the driver
	// a wrapped context.Canceled. Not the backend's fault (R6-06). A deadline
	// is different — it is the stall signal and stays a failure.
	if errors.Is(err, context.Canceled) {
		return false
	}
	// Object-not-found: os.Remove/Open on a missing path, or our own type.
	if errors.Is(err, os.ErrNotExist) {
		return false
	}
	var nf NotFoundError
	var nfp *NotFoundError
	if errors.As(err, &nf) || errors.As(err, &nfp) {
		return false
	}
	// Other client-level engine errors. Archived-on-tape is an object state,
	// not a backend health signal (V18.2).
	if errors.Is(err, ErrQuotaExceeded) || errors.Is(err, ErrInvalidInput) ||
		errors.Is(err, ErrArchived) {
		return false
	}
	var perr PermissionError
	if errors.As(err, &perr) {
		return false
	}
	// aws-sdk-go-v2, by type (R6-01). Every S3-class driver (idrive, lyve,
	// r2, geyser, s3compat, quotaless) wraps the SDK error with %w. A missing
	// BUCKET is a misconfigured backend — decided here, before the string
	// fallbacks, because its message also carries "StatusCode: 404".
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) && apiErr.ErrorCode() == "NoSuchBucket" {
		return true
	}
	if isSDKNotFound(err) {
		return false
	}
	// Precise string fallbacks for drivers that do not wrap a typed error:
	// the Graph API's item-absence code (permafrost), the local driver's path
	// text when it arrives pre-formatted, and the SDK's own v2 formatting in
	// case a driver stringifies with %v. Same list as isObjectMissingErr.
	// Kept narrow so real failures ("no such host", "connection refused")
	// still trip the breaker.
	msg := err.Error()
	for _, s := range []string{"no such file or directory", "NoSuchKey", "NotFound", "itemNotFound", "StatusCode: 404"} {
		if strings.Contains(msg, s) {
			return false
		}
	}
	return true
}

// isSDKNotFound reports an aws-sdk-go-v2 error chain that means "the object is
// not there": the NoSuchKey / NotFound API codes (types.NoSuchKey,
// types.NotFound, or the GenericAPIError the deserializer builds from the
// status text when a vendor sends a 404 with no error body), or any HTTP 404
// from the endpoint. NoSuchBucket is also a 404 but means the backend is
// misconfigured — that IS a failure and is excluded before the status check.
//
// The chain the S3 client produces is OperationError → s3shared.ResponseError
// (→ awshttp.ResponseError → smithyhttp.ResponseError, reachable through the
// embedded types' As methods) → APIError; errors.As walks all of it.
func isSDKNotFound(err error) bool {
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) {
		switch apiErr.ErrorCode() {
		case "NoSuchKey", "NotFound":
			return true
		case "NoSuchBucket":
			return false
		}
	}
	var re *smithyhttp.ResponseError
	if errors.As(err, &re) && re.HTTPStatusCode() == http.StatusNotFound {
		return true
	}
	return false
}

type FailoverManager struct {
	mu       sync.RWMutex
	breakers map[string]*BackendCircuitBreaker
	logger   *zap.Logger
}

func NewFailoverManager(logger *zap.Logger) *FailoverManager {
	return &FailoverManager{
		breakers: make(map[string]*BackendCircuitBreaker),
		logger:   logger,
	}
}

func (f *FailoverManager) Register(backend string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, exists := f.breakers[backend]; !exists {
		f.breakers[backend] = NewBackendCircuitBreaker()
	}
}

// Execute tries fn against each registered backend in order, skipping open
// breakers, and returns the first backend that succeeded.
//
// Verdict rules when nothing succeeds (R6-02): the FIRST candidate is the
// recorded / target backend — the one that knows. If it answered with a
// client-level outcome (not found, quota, archived, consumed body) that is the
// verdict and later failures do not change it. If it was unavailable (open
// breaker, timeout, 5xx, refused) then nothing a fallback said — including
// "not found" — is a verdict about the object, and the aggregate is
// ErrAllBackendsUnavailable (the API answers 503 + Retry-After, never 404).
func (f *FailoverManager) Execute(ctx context.Context, backends []string, fn func(driverName string) error) (string, error) {
	var (
		lastErr          error
		firstErr         error // outcome of the first candidate that was actually asked
		firstUnavailable bool  // first candidate skipped (open breaker) or failed as a backend
		first            = true
	)

	for _, backend := range backends {
		// The client is gone: do not replay a dead request against every
		// remaining backend (R6-06). Their breakers are not charged either.
		if ctxErr := ctx.Err(); ctxErr != nil {
			if lastErr != nil {
				return "", fmt.Errorf("%w (last backend error: %v)", ctxErr, lastErr)
			}
			return "", fmt.Errorf("request abandoned before %s: %w", backend, ctxErr)
		}

		f.mu.RLock()
		breaker, exists := f.breakers[backend]
		f.mu.RUnlock()

		if !exists {
			continue
		}
		isFirst := first
		first = false

		if !breaker.Allow() {
			if isFirst {
				firstUnavailable = true
			}
			f.logger.Debug("circuit breaker open, skipping backend",
				zap.String("backend", backend))
			continue
		}

		if err := fn(backend); err != nil {
			// Only genuine backend-health failures trip the circuit breaker.
			// Benign client-level outcomes (object not found, quota exceeded,
			// bad input, permission denied, client cancelled) must NOT —
			// otherwise a burst of 404s or aborted uploads would open the
			// breaker and take a healthy backend out of routing.
			failure := isBackendFailure(err)
			if failure {
				breaker.RecordFailure()
				f.logger.Warn("backend failed, trying next",
					zap.String("backend", backend),
					zap.Error(err))
			}
			if isFirst {
				firstErr = err
				firstUnavailable = failure
			}
			lastErr = err
			// Archived is definitive wherever it is seen: the object EXISTS
			// on this backend but needs a restore. Trying other backends
			// would fail NotFound and mask the archived state as a 404 (or
			// 500). Stop here so the API layer can answer 403
			// InvalidObjectState. (V18.2)
			if errors.Is(err, ErrArchived) {
				return "", fmt.Errorf("all backends failed: %w", err)
			}
			// A consumed non-rewindable body makes every further attempt a
			// doomed retry against a drained stream: it would charge healthy
			// backends' breakers for a failure that is not theirs — or worse,
			// silently store a truncated object on a backend that does not
			// validate Content-Length. Stop here; the client retries the
			// whole request.
			if errors.Is(err, ErrNoFailover) {
				break
			}
			continue
		}

		breaker.RecordSuccess()
		return backend, nil
	}

	switch {
	case firstErr != nil && !firstUnavailable:
		// The backend that holds (or would hold) the object gave a
		// client-level answer. That is authoritative.
		return "", fmt.Errorf("all backends failed: %w", firstErr)
	case lastErr != nil:
		// The first candidate was unreachable, so no fallback's answer is a
		// verdict about the object; and if every candidate failed hard the
		// request is equally undeliverable.
		return "", fmt.Errorf("%w: all backends failed: %w", ErrAllBackendsUnavailable, lastErr)
	default:
		return "", ErrAllBackendsUnavailable
	}
}

func (f *FailoverManager) GetStatus(backend string) string {
	f.mu.RLock()
	breaker, exists := f.breakers[backend]
	f.mu.RUnlock()
	if !exists {
		return "unknown"
	}
	return breaker.State().String()
}

func (f *FailoverManager) GetAllStatuses() map[string]string {
	f.mu.RLock()
	defer f.mu.RUnlock()

	statuses := make(map[string]string, len(f.breakers))
	for name, breaker := range f.breakers {
		statuses[name] = breaker.State().String()
	}
	return statuses
}
