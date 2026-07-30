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

	// ErrNoFailover marks a failure after which no further backend may be
	// attempted: the request body is a non-rewindable stream that has already
	// been partially consumed, so any retry would receive a drained reader —
	// a length-validating backend fails pointlessly (and gets its breaker
	// charged for a failure that is not its own), and a lax one silently
	// stores a TRUNCATED object as success. FailoverManager.Execute stops
	// iterating when it sees this sentinel.
	ErrNoFailover = fmt.Errorf("request body already consumed — failover unavailable")
)
