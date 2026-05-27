package api

import (
	"context"
	"database/sql"
	"net/http"
	"strings"

	"github.com/FairForge/vaultaire/internal/auth"
)

// checkMFADelete validates the x-amz-mfa header when MFA Delete is enabled on a bucket.
// Returns nil if MFA Delete is not enabled or verification succeeds.
func checkMFADelete(ctx context.Context, db *sql.DB, authSvc *auth.AuthService,
	mfaSvc *auth.MFAService, tenantID, bucket string, r *http.Request) error {
	if db == nil {
		return nil
	}

	var mfaDeleteEnabled bool
	err := db.QueryRowContext(ctx,
		`SELECT mfa_delete_enabled FROM buckets WHERE tenant_id = $1 AND name = $2`,
		tenantID, bucket).Scan(&mfaDeleteEnabled)
	if err != nil {
		return nil
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

var errMFARequired = &mfaRequiredError{}

type mfaRequiredError struct{}

func (e *mfaRequiredError) Error() string {
	return "MFA verification required for this operation"
}

type mfaNotConfiguredError struct{}

func (e *mfaNotConfiguredError) Error() string {
	return "MFA must be configured on the account before using MFA Delete"
}
