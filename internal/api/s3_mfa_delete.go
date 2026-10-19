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

	enabled, err := authSvc.IsMFAEnabled(ctx, userID)
	if err != nil || !enabled {
		return &mfaNotConfiguredError{}
	}

	secret, err := authSvc.GetMFASecret(ctx, userID)
	if err != nil {
		return errMFARequired
	}

	if !mfaSvc.ValidateCode(secret, code) {
		return errMFARequired
	}

	return nil
}

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
		s.log().Error("mfa delete: flag unreadable — refused 503",
			zap.String("bucket", bucket), zap.Error(err))
	}
}
