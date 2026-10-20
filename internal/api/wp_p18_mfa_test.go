package api

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/FairForge/vaultaire/internal/auth"
	"github.com/FairForge/vaultaire/internal/tenant"
	"github.com/FairForge/vaultaire/internal/testutil"
	"github.com/lib/pq"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// RP19-9 (Stage 1a Prompt 18, Part 0.2): the MfaDelete element's value is
// validated like Status — anything but Enabled/Disabled is 400 MalformedXML
// BEFORE the MFA check: no code consumed, nothing written. It passed the
// `!= ""` gate, consumed a valid code and wrote mfa_delete_enabled = false.
func TestBucketState_AnMfaDeleteValueThatIsNeitherEnabledNorDisabledIsMalformed(t *testing.T) {
	// Arrange: MFA Delete on, versioning Enabled, one valid code.
	f := setupMFADeleteFixture(t)
	f.enableUserMFA(t)
	f.plainBucket(t, "garbage-mfa", "Enabled")
	_, err := f.db.Exec(`UPDATE buckets SET mfa_delete_enabled = TRUE WHERE tenant_id = $1 AND name = $2`, f.tenantID, "garbage-mfa")
	require.NoError(t, err)
	code := mfaCodeAt(t, time.Now())
	garbage := `<VersioningConfiguration xmlns="http://s3.amazonaws.com/doc/2006-03-01/"><Status>Enabled</Status><MfaDelete>Garbage</MfaDelete></VersioningConfiguration>`

	// Act
	w := f.putVersioning(t, "garbage-mfa", garbage, code)

	// Assert: refused as malformed, nothing written, the code not spent.
	assert.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), ErrMalformedXML)
	_, mfa, v := f.flags(t, "garbage-mfa")
	assert.True(t, mfa, "MFA Delete is still on")
	assert.Equal(t, "Enabled", v)
	assert.Equal(t, http.StatusOK, f.putVersioning(t, "garbage-mfa", mfaDeleteDisabled, code).Code,
		"the code was not consumed by the refused request")
}

var userMFAPoisonOnce sync.Once

// RP19-10 (Part 0.2): a database error reading the user's MFA state is the
// RP16-5 shape — 503 + Retry-After, nothing written — never "MFA not
// configured" (403); the true "no TOTP enrolled" keeps its 403.
func TestBucketState_AnMFAStateReadErrorIs503NotNotConfigured(t *testing.T) {
	// Arrange: an auth service whose every user_mfa read fails, its own
	// user and tenant, a versioned bucket of that tenant.
	f := setupMFADeleteFixture(t)
	userMFAPoisonOnce.Do(func() {
		sql.Register("postgres-poison-user-mfa", &poisonedSQLDriver{inner: &pq.Driver{}, poison: "user_mfa"})
	})
	pdb, err := sql.Open("postgres-poison-user-mfa", testutil.DSN())
	require.NoError(t, err)
	pdb.SetMaxOpenConns(4)
	t.Cleanup(func() { _ = pdb.Close() })
	authSvc := auth.NewAuthService(nil, pdb)
	email := fmt.Sprintf("mfa-poison-%d@test.local", time.Now().UnixNano())
	user, tn, _, err := authSvc.CreateUserWithTenant(context.Background(), email, "testpass123", "MFA Poison Co")
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = f.db.Exec(`DELETE FROM buckets WHERE tenant_id = $1`, tn.ID)
		_, _ = f.db.Exec(`DELETE FROM api_keys WHERE tenant_id = $1`, tn.ID)
		_, _ = f.db.Exec(`DELETE FROM tenant_quotas WHERE tenant_id = $1`, tn.ID)
		_, _ = f.db.Exec(`DELETE FROM tenants WHERE id = $1`, tn.ID)
		_, _ = f.db.Exec(`DELETE FROM users WHERE id = $1`, user.ID)
	})
	_, err = f.db.Exec(`INSERT INTO buckets (tenant_id, name, visibility, versioning_status) VALUES ($1, 'poisoned', 'private', 'Enabled')`, tn.ID)
	require.NoError(t, err)
	f.server.auth = authSvc

	// Act
	req := httptest.NewRequest("PUT", "/poisoned?versioning", bytes.NewReader([]byte(mfaDeleteEnabled)))
	req.Header.Set("x-amz-mfa", "arn:aws:iam::serial "+mfaCodeAt(t, time.Now()))
	req = req.WithContext(s3Ctx(req.Context(), &tenant.Tenant{ID: tn.ID, Namespace: "tenant/" + tn.ID + "/"}))
	w := httptest.NewRecorder()
	f.server.handlePutBucketVersioning(w, req, &S3Request{Bucket: "poisoned", TenantID: tn.ID})

	// Assert
	assertMFAUnknown503(t, w)
	var mfa bool
	require.NoError(t, f.db.QueryRow(`SELECT mfa_delete_enabled FROM buckets WHERE tenant_id = $1 AND name = 'poisoned'`, tn.ID).Scan(&mfa))
	assert.False(t, mfa, "nothing written")
}

func TestBucketState_NoTOTPEnrolledKeepsIts403(t *testing.T) {
	// Arrange: the fixture's user has no MFA enrolled.
	f := setupMFADeleteFixture(t)
	f.plainBucket(t, "no-totp", "Enabled")

	// Act
	w := f.putVersioning(t, "no-totp", mfaDeleteEnabled, mfaCodeAt(t, time.Now()))

	// Assert
	assert.Equal(t, http.StatusForbidden, w.Code, w.Body.String())
	assert.Empty(t, w.Header().Get("Retry-After"))
}
