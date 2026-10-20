package api

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/FairForge/vaultaire/internal/auth"
	"go.uber.org/zap"
)

// checkMFADelete validates the x-amz-mfa header when MFA Delete is enabled on a bucket.
// Returns nil if MFA Delete is not enabled (or the bucket has no row) or
// verification succeeds. A read of mfa_delete_enabled that fails is
// *mfaDeleteUnknownError — never "off" (RP16-5: a DB outage used to read as
// MFA Delete disabled on every gated path); the callers answer it with
// writeMFADeleteRefusal.
func checkMFADelete(ctx context.Context, db *sql.DB, authSvc *auth.AuthService,
	mfaSvc *auth.MFAService, tenantID, bucket string, r *http.Request) error {
	if db == nil {
		return nil
	}

	var mfaDeleteEnabled bool
	err := db.QueryRowContext(ctx,
		`SELECT mfa_delete_enabled FROM buckets WHERE tenant_id = $1 AND name = $2`,
		tenantID, bucket).Scan(&mfaDeleteEnabled)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return &mfaDeleteUnknownError{err: fmt.Errorf("read mfa_delete_enabled of %s: %w", bucket, err)}
	}

	if !mfaDeleteEnabled {
		return nil
	}

	return validateMFAHeader(ctx, authSvc, mfaSvc, tenantID, r)
}

// validateMFAHeader verifies the x-amz-mfa header contains a valid TOTP code
// for the tenant's owning user. Used both by checkMFADelete (for already-enabled
// buckets) and directly by PutBucketVersioning when enabling/disabling MFA Delete.
func validateMFAHeader(ctx context.Context, authSvc *auth.AuthService,
	mfaSvc *auth.MFAService, tenantID string, r *http.Request) error {
	if authSvc == nil || mfaSvc == nil {
		return errMFARequired
	}

	mfaHeader := r.Header.Get("x-amz-mfa")
	if mfaHeader == "" {
		return errMFARequired
	}

	parts := strings.Fields(mfaHeader)
	if len(parts) == 0 {
		return errMFARequired
	}
	code := parts[len(parts)-1]

	userID := authSvc.GetUserIDByTenantID(ctx, tenantID)
	if userID == "" {
		return errMFARequired
	}

	// A read that fails is not "MFA not configured" (RP19-10): it is the
	// RP16-5 shape — 503 + Retry-After through writeMFADeleteRefusal — and
	// still refuses (fails closed). Only a successful read that finds no
	// TOTP enrolled is the 403 mfaNotConfiguredError.
	enabled, err := authSvc.IsMFAEnabled(ctx, userID)
	if err != nil {
		return &mfaDeleteUnknownError{err: fmt.Errorf("read MFA state of the tenant's user: %w", err)}
	}
	if !enabled {
		return &mfaNotConfiguredError{}
	}

	secret, err := authSvc.GetMFASecret(ctx, userID)
	if err != nil {
		// Enabled a moment ago: a failure here is the read, not the state.
		return &mfaDeleteUnknownError{err: fmt.Errorf("read MFA secret of the tenant's user: %w", err)}
	}

	if !mfaSvc.ValidateCode(secret, code) {
		return errMFARequired
	}
	// A code is accepted once (RFC 6238 §5.2; WP-R4-2 A.4): the dashboard's
	// guard, shared — keyed by the tenant's user, every code accepted inside
	// the validator's window remembered (R12-38: a guard that kept only the
	// latest let an adjacent step's code be replayed).
	if !authSvc.ConsumeTOTPCode(userID, code) {
		return errMFARequired
	}

	return nil
}

// The bucket state machine's refusals (WP-R4-2, R4-10), each 409
// InvalidBucketState with the sentence AWS answers:
const (
	// lockSuspendRefusal: PutBucketVersioning Suspended on a bucket with
	// Object Lock ("After you enable Object Lock on a bucket, you can't
	// disable Object Lock or suspend versioning for that bucket" —
	// Configuring Object Lock, the user guide).
	lockSuspendRefusal = "An Object Lock configuration is present on this bucket, so the versioning state cannot be changed."
	// lockNeedsVersioningRefusal: PutObjectLockConfiguration
	// ObjectLockEnabled=Enabled on a bucket whose versioning is not Enabled
	// ("Object Lock works only with versioned buckets").
	lockNeedsVersioningRefusal = "Versioning must be 'Enabled' on the bucket to apply a Object Lock configuration"
	// mfaDeleteNeedsVersioningRefusal: MfaDelete=Enabled without versioning
	// Enabled — the request's Status or the bucket's (MFA delete lives in
	// the versioning subresource; Configuring MFA delete, the user guide).
	mfaDeleteNeedsVersioningRefusal = "MFA Delete requires versioning to be Enabled on the bucket."
)

// mfaDeleteRetryAfter is the Retry-After of a 503 for an MFA Delete flag
// that could not be read.
const mfaDeleteRetryAfter = "5"

// writeMFADeleteRefusal answers a checkMFADelete error: 503
// ServiceUnavailable + Retry-After when the flag could not be read (the
// request may succeed once the database answers; nothing was touched), else
// 403 AccessDenied (the MFA refusals).
func writeMFADeleteRefusal(w http.ResponseWriter, r *http.Request, err error) {
	var unknown *mfaDeleteUnknownError
	if errors.As(err, &unknown) {
		w.Header().Set("Retry-After", mfaDeleteRetryAfter)
		WriteS3Error(w, ErrServiceUnavailable, r.URL.Path, generateRequestID())
		return
	}
	WriteS3Error(w, ErrAccessDenied, r.URL.Path, generateRequestID())
}

// mfaDeleteUnknownError: the bucket's MFA Delete flag could not be read, so
// whether the request needs a code is unknown.
type mfaDeleteUnknownError struct{ err error }

func (e *mfaDeleteUnknownError) Error() string { return e.err.Error() }
func (e *mfaDeleteUnknownError) Unwrap() error { return e.err }

var errMFARequired = &mfaRequiredError{}

type mfaRequiredError struct{}

func (e *mfaRequiredError) Error() string {
	return "MFA verification required for this operation"
}

type mfaNotConfiguredError struct{}

func (e *mfaNotConfiguredError) Error() string {
	return "MFA must be configured on the account before using MFA Delete"
}

// logMFADeleteUnknown logs a flag read that failed (the MFA refusals are the
// caller's, not logged).
func (s *Server) logMFADeleteUnknown(err error, bucket string) {
	var unknown *mfaDeleteUnknownError
	if errors.As(err, &unknown) {
		s.log().Error("mfa delete: the flag or the user's MFA state unreadable — refused 503",
			zap.String("bucket", bucket), zap.Error(err))
	}
}
