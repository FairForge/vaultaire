package engine

import "fmt"

type NotFoundError struct {
	Container string
	Artifact  string
}

func (e NotFoundError) Error() string {
	return fmt.Sprintf("not found: %s/%s", e.Container, e.Artifact)
}

func ErrNotFound(container, artifact string) error {
	return NotFoundError{Container: container, Artifact: artifact}
}

type PermissionError struct {
	TenantID string
	Action   string
}

func (e PermissionError) Error() string {
	return fmt.Sprintf("permission denied: tenant %s cannot %s", e.TenantID, e.Action)
}

func ErrPermissionDenied(tenantID, action string) error {
	return PermissionError{TenantID: tenantID, Action: action}
}

func WrapError(err error, message string) error {
	return fmt.Errorf("%s: %w", message, err)
}

// Common errors
var (
	ErrQuotaExceeded          = fmt.Errorf("quota exceeded")
	ErrInvalidInput           = fmt.Errorf("invalid input")
	ErrTimeout                = fmt.Errorf("operation timeout")
	ErrAllBackendsUnavailable = fmt.Errorf("all backends unavailable")

	// ErrBackendFull: the backend refused a write because its account or
	// container is full (Blomp's Swift answered 413 "Upload exceeds quota"
	// past its 40 GiB on 2026-10-21). Not a health failure — reads and
	// deletes still work, so the breaker is never charged — and not the
	// caller's to retry: FailoverManager.Execute stops on it (another
	// backend would store the bytes off the tier the bucket promises) and
	// the API answers 507 InsufficientStorage, never 503 "please retry".
	ErrBackendFull = fmt.Errorf("backend storage full")

	// ErrNoFailover marks a failure after which no further backend may be
	// attempted: the request body is a non-rewindable stream that has already
	// been partially consumed, so any retry would receive a drained reader —
	// a length-validating backend fails pointlessly (and gets its breaker
	// charged for a failure that is not its own), and a lax one silently
	// stores a TRUNCATED object as success. FailoverManager.Execute stops
	// iterating when it sees this sentinel.
	ErrNoFailover = fmt.Errorf("request body already consumed — failover unavailable")

	// ErrArchived: the object exists but is in archive storage (tape) and must
	// be restored before it can be read — Geyser/Vail returns InvalidObjectState
	// for GETs past the staging window. This is a definitive per-object state,
	// not a backend failure: FailoverManager.Execute stops iterating on it
	// (trying other backends would mask it as NotFound), and it never trips a
	// circuit breaker. The API layer maps it to 403 InvalidObjectState. (V18.2)
	ErrArchived = fmt.Errorf("object is archived — restore it before access")

	// ErrRestoreAlreadyInProgress: a RestoreObject was issued for an object
	// whose recall is already running. API layer maps it to 409.
	ErrRestoreAlreadyInProgress = fmt.Errorf("object restore already in progress")
)

var (
	// ErrPartiallyUnavailable: part of a backend cannot serve THIS call
	// right now — the one server of a multi-server backend the object lives
	// on (one Sync bridge of five), or an object caught mid-change (a
	// striped object whose piece went with an overwrite during the read) —
	// while the backend as a whole is healthy. It is never a backend failure
	// (no breaker charge: one bridge down used to open the whole `sync`
	// breaker, and every read of every bridge answered 503 for 30 s, again
	// and again — Prompt 2b.2 C1), never failed over to another backend
	// (writes stay on their tier, a fallback's "not found" is no verdict)
	// and never a miss. It wraps ErrAllBackendsUnavailable, so every caller
	// answers it as one: S3 503 + Retry-After, the head row kept.
	ErrPartiallyUnavailable = fmt.Errorf("%w: part of the backend is unavailable for this object", ErrAllBackendsUnavailable)

	// ErrCallerAborted: the request body could not be read — the client
	// went away mid-body (Go's server hands the handler the read error
	// BEFORE it cancels the request context, so the context.Canceled rule
	// does not cover it), a decoded stream broke, or it came in slower than
	// the minimum rate (ErrSourceTooSlow). The caller's failure: never a
	// backend failure, never failed over (the body is consumed). The API
	// answers 400 (IncompleteBody / RequestTimeout) to a client still there.
	ErrCallerAborted = fmt.Errorf("the request body could not be read")

	// ErrSourceTooSlow: the request body arrived below the minimum rate a
	// backend allows an upload to hold its connection (Sync's bridges:
	// SYNC_WEBDAV_SOURCE_MIN_RATE). Wraps ErrCallerAborted.
	ErrSourceTooSlow = fmt.Errorf("%w: it arrived below the minimum rate", ErrCallerAborted)

	// ErrObjectChanged: a read that had to be re-issued part way (the sia
	// driver resuming a body s3d cut, drivers/sia_read.go) found another
	// object at the key — a 412 to its If-Match, or another ETag. The body
	// fails rather than splice two objects; it is the object's state, not
	// the backend's health, so the breaker is never charged.
	ErrObjectChanged = fmt.Errorf("the object changed during the read")
)
