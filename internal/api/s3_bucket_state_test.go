package api

import (
	"bytes"
	"encoding/xml"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/pquerna/otp/totp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// WP-R4-2 (Stage 1a Prompt 17 Part A, R4-10): the Object Lock / versioning /
// MFA-Delete bucket state machine, as AWS's API answers it.
//
//   - PutBucketVersioning Suspended on an object_lock_enabled bucket is 409
//     InvalidBucketState ("After you enable Object Lock on a bucket, you
//     can't disable Object Lock or suspend versioning for that bucket" —
//     the user guide's Configuring Object Lock page); it answered 200 and
//     flipped the row. The MFA check stays before it.
//   - PutObjectLockConfiguration ObjectLockEnabled=Enabled needs versioning
//     Enabled (AWS enables Object Lock on an existing bucket since 2023-11
//     and the API refuses an unversioned one with InvalidBucketState; the
//     console turns versioning on for you); it turned the lock on with
//     versioning off.
//   - MFA Delete is gated on versioning Enabled (the versioning subresource
//     holds it — the Configuring MFA delete page), never on the lock;
//     MfaDelete=Disabled needs x-amz-mfa too.
//   - A TOTP code in x-amz-mfa is accepted once (RFC 6238 §5.2; the
//     dashboard's ConsumeTOTPCode, shared).

// mfaCodeAt is a valid code for the fixture's secret at t: the validator
// accepts the adjacent 30 s steps, so now-30 s, now and now+30 s are three
// distinct valid codes inside one test.
func mfaCodeAt(t *testing.T, at time.Time) string {
	t.Helper()
	code, err := totp.GenerateCode("JBSWY3DPEHPK3PXP", at)
	require.NoError(t, err)
	return code
}

func (f *mfaDeleteFixture) putVersioning(t *testing.T, bucket, body, mfa string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("PUT", "/"+bucket+"?versioning", bytes.NewReader([]byte(body)))
	if mfa != "" {
		req.Header.Set("x-amz-mfa", "arn:aws:iam::serial "+mfa)
	}
	req = req.WithContext(s3Ctx(req.Context(), f.tenant))
	w := httptest.NewRecorder()
	f.server.handlePutBucketVersioning(w, req, &S3Request{Bucket: bucket, TenantID: f.tenantID})
	return w
}

func (f *mfaDeleteFixture) putLockConfig(t *testing.T, bucket, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("PUT", "/"+bucket+"?object-lock", bytes.NewReader([]byte(body)))
	req = req.WithContext(s3Ctx(req.Context(), f.tenant))
	w := httptest.NewRecorder()
	f.server.handlePutObjectLockConfiguration(w, req, &S3Request{Bucket: bucket, TenantID: f.tenantID})
	return w
}

// plainBucket is a second bucket of the fixture's tenant: no lock, no MFA
// Delete, versioning as given.
func (f *mfaDeleteFixture) plainBucket(t *testing.T, name, versioning string) {
	t.Helper()
	_, err := f.db.Exec(`
		INSERT INTO buckets (tenant_id, name, visibility, object_lock_enabled, mfa_delete_enabled, versioning_status)
		VALUES ($1, $2, 'private', FALSE, FALSE, $3)
		ON CONFLICT (tenant_id, name) DO UPDATE SET object_lock_enabled = FALSE, mfa_delete_enabled = FALSE, versioning_status = EXCLUDED.versioning_status`,
		f.tenantID, name, versioning)
	require.NoError(t, err)
	t.Cleanup(func() { _, _ = f.db.Exec(`DELETE FROM buckets WHERE tenant_id = $1 AND name = $2`, f.tenantID, name) })
}

func (f *mfaDeleteFixture) flags(t *testing.T, bucket string) (lock, mfa bool, versioning string) {
	t.Helper()
	require.NoError(t, f.db.QueryRow(`SELECT object_lock_enabled, mfa_delete_enabled, versioning_status FROM buckets WHERE tenant_id = $1 AND name = $2`,
		f.tenantID, bucket).Scan(&lock, &mfa, &versioning))
	return lock, mfa, versioning
}

func s3ErrorOf(t *testing.T, w *httptest.ResponseRecorder) S3Error {
	t.Helper()
	var e S3Error
	require.NoError(t, xml.Unmarshal(w.Body.Bytes(), &e), w.Body.String())
	return e
}

const (
	versioningSuspended = `<VersioningConfiguration xmlns="http://s3.amazonaws.com/doc/2006-03-01/"><Status>Suspended</Status></VersioningConfiguration>`
	versioningEnabled   = `<VersioningConfiguration xmlns="http://s3.amazonaws.com/doc/2006-03-01/"><Status>Enabled</Status></VersioningConfiguration>`
	mfaDeleteEnabled    = `<VersioningConfiguration xmlns="http://s3.amazonaws.com/doc/2006-03-01/"><MfaDelete>Enabled</MfaDelete></VersioningConfiguration>`
	mfaDeleteDisabled   = `<VersioningConfiguration xmlns="http://s3.amazonaws.com/doc/2006-03-01/"><MfaDelete>Disabled</MfaDelete></VersioningConfiguration>`
	lockEnabledConfig   = `<ObjectLockConfiguration xmlns="http://s3.amazonaws.com/doc/2006-03-01/"><ObjectLockEnabled>Enabled</ObjectLockEnabled></ObjectLockConfiguration>`
)

// --- A.1 ---

func TestBucketState_SuspendingVersioningOnALockBucketIs409(t *testing.T) {
	// Arrange: the fixture's bucket has the lock; versioning Enabled.
	f := setupMFADeleteFixture(t)
	_, err := f.db.Exec(`UPDATE buckets SET versioning_status = 'Enabled' WHERE tenant_id = $1 AND name = $2`, f.tenantID, f.bucket)
	require.NoError(t, err)

	// Act
	w := f.putVersioning(t, f.bucket, versioningSuspended, "")

	// Assert: 409, the row untouched.
	require.Equal(t, http.StatusConflict, w.Code, w.Body.String())
	e := s3ErrorOf(t, w)
	assert.Equal(t, "InvalidBucketState", e.Code)
	assert.Equal(t, lockSuspendRefusal, e.Message)
	_, _, v := f.flags(t, f.bucket)
	assert.Equal(t, "Enabled", v)
}

func TestBucketState_TheMFACheckComesBeforeTheLockRefusal(t *testing.T) {
	f := setupMFADeleteFixture(t)
	f.enableMFADelete(t)
	f.enableUserMFA(t)
	_, err := f.db.Exec(`UPDATE buckets SET versioning_status = 'Enabled' WHERE tenant_id = $1 AND name = $2`, f.tenantID, f.bucket)
	require.NoError(t, err)

	assert.Equal(t, http.StatusForbidden, f.putVersioning(t, f.bucket, versioningSuspended, "").Code, "no code: the MFA refusal first")
	assert.Equal(t, http.StatusConflict, f.putVersioning(t, f.bucket, versioningSuspended, mfaCodeAt(t, time.Now())).Code, "a code: then the lock")
}

// --- A.2 ---

func TestBucketState_EnablingTheLockNeedsVersioningEnabled(t *testing.T) {
	f := setupMFADeleteFixture(t)
	f.plainBucket(t, "plain-unversioned", "disabled")
	f.plainBucket(t, "plain-suspended", "Suspended")
	f.plainBucket(t, "plain-versioned", "Enabled")

	for _, b := range []string{"plain-unversioned", "plain-suspended"} {
		w := f.putLockConfig(t, b, lockEnabledConfig)
		require.Equal(t, http.StatusConflict, w.Code, "%s: %s", b, w.Body.String())
		e := s3ErrorOf(t, w)
		assert.Equal(t, "InvalidBucketState", e.Code)
		assert.Equal(t, lockNeedsVersioningRefusal, e.Message)
		lock, _, _ := f.flags(t, b)
		assert.False(t, lock, "%s: the lock stays off", b)
	}

	w := f.putLockConfig(t, "plain-versioned", lockEnabledConfig)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	lock, _, v := f.flags(t, "plain-versioned")
	assert.True(t, lock)
	assert.Equal(t, "Enabled", v, "versioning untouched")
	// And GetBucketVersioning still says Enabled.
	req := httptest.NewRequest("GET", "/plain-versioned?versioning", nil)
	req = req.WithContext(s3Ctx(req.Context(), f.tenant))
	rec := httptest.NewRecorder()
	f.server.handleGetBucketVersioning(rec, req, &S3Request{Bucket: "plain-versioned", TenantID: f.tenantID})
	var cfg VersioningConfiguration
	require.NoError(t, xml.Unmarshal(rec.Body.Bytes(), &cfg))
	assert.Equal(t, "Enabled", cfg.Status)
}

// --- A.3 ---

func TestBucketState_MFADeleteIsGatedOnVersioningNotOnTheLock(t *testing.T) {
	f := setupMFADeleteFixture(t)
	f.enableUserMFA(t)
	f.plainBucket(t, "plain-unversioned", "disabled")
	f.plainBucket(t, "plain-versioned", "Enabled")

	// An unversioned bucket (no lock): 409.
	w := f.putVersioning(t, "plain-unversioned", mfaDeleteEnabled, mfaCodeAt(t, time.Now().Add(-30*time.Second)))
	require.Equal(t, http.StatusConflict, w.Code, w.Body.String())
	e := s3ErrorOf(t, w)
	assert.Equal(t, "InvalidBucketState", e.Code)
	assert.Equal(t, mfaDeleteNeedsVersioningRefusal, e.Message)
	_, mfa, _ := f.flags(t, "plain-unversioned")
	assert.False(t, mfa)

	// The fixture's lock bucket is unversioned too: the lock does not help.
	w = f.putVersioning(t, f.bucket, mfaDeleteEnabled, mfaCodeAt(t, time.Now()))
	require.Equal(t, http.StatusConflict, w.Code, w.Body.String())

	// A versioned bucket WITHOUT the lock: 200 (it was 409 — gated on the lock).
	w = f.putVersioning(t, "plain-versioned", mfaDeleteEnabled, mfaCodeAt(t, time.Now().Add(30*time.Second)))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	_, mfa, _ = f.flags(t, "plain-versioned")
	assert.True(t, mfa)
}

func TestBucketState_StatusEnabledAndMFADeleteEnabledInOneRequest(t *testing.T) {
	// The documented way: Status and MfaDelete together on an unversioned
	// bucket ("you must include Status whenever you use MfaDelete").
	f := setupMFADeleteFixture(t)
	f.enableUserMFA(t)
	f.plainBucket(t, "plain-unversioned", "disabled")
	body := `<VersioningConfiguration xmlns="http://s3.amazonaws.com/doc/2006-03-01/"><Status>Enabled</Status><MfaDelete>Enabled</MfaDelete></VersioningConfiguration>`

	w := f.putVersioning(t, "plain-unversioned", body, mfaCodeAt(t, time.Now()))

	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	_, mfa, v := f.flags(t, "plain-unversioned")
	assert.True(t, mfa)
	assert.Equal(t, "Enabled", v)
}

func TestBucketState_DisablingMFADeleteNeedsTheHeaderToo(t *testing.T) {
	f := setupMFADeleteFixture(t)
	f.enableUserMFA(t)
	f.plainBucket(t, "plain-versioned", "Enabled")
	_, err := f.db.Exec(`UPDATE buckets SET mfa_delete_enabled = TRUE WHERE tenant_id = $1 AND name = $2`, f.tenantID, "plain-versioned")
	require.NoError(t, err)

	assert.Equal(t, http.StatusForbidden, f.putVersioning(t, "plain-versioned", mfaDeleteDisabled, "").Code)
	_, mfa, _ := f.flags(t, "plain-versioned")
	assert.True(t, mfa, "still on")
	assert.Equal(t, http.StatusOK, f.putVersioning(t, "plain-versioned", mfaDeleteDisabled, mfaCodeAt(t, time.Now())).Code)
	_, mfa, _ = f.flags(t, "plain-versioned")
	assert.False(t, mfa)
}

// --- A.4 ---

func TestBucketState_AnMFACodeIsAcceptedOnce(t *testing.T) {
	// Arrange: MFA Delete on, two objects, one code.
	f := setupMFADeleteFixture(t)
	f.enableMFADelete(t)
	f.enableUserMFA(t)
	f.putObject(t, "one.txt", "1")
	f.putObject(t, "two.txt", "2")
	code := "arn:aws:iam::serial " + mfaCodeAt(t, time.Now())
	del := func(key string) int {
		req := httptest.NewRequest("DELETE", "/"+f.bucket+"/"+key, nil)
		req.Header.Set("x-amz-mfa", code)
		req = req.WithContext(s3Ctx(req.Context(), f.tenant))
		w := httptest.NewRecorder()
		f.server.handleDeleteObject(w, req, &S3Request{Bucket: f.bucket, Object: key, TenantID: f.tenantID})
		return w.Code
	}

	// Act / Assert: 204, then 403 for the same code inside the window.
	assert.Equal(t, http.StatusNoContent, del("one.txt"))
	assert.Equal(t, http.StatusForbidden, del("two.txt"), "the same code again is a replay")
	// A different valid code (the next step) still works.
	code = "arn:aws:iam::serial " + mfaCodeAt(t, time.Now().Add(30*time.Second))
	assert.Equal(t, http.StatusNoContent, del("two.txt"))
}

// --- A.5 guard: the DELETE flavours on a lock bucket with MFA Delete answer as before ---

func TestBucketState_TheDeleteFlavoursOnALockBucketAreUnchanged(t *testing.T) {
	f := setupMFADeleteFixture(t)
	f.enableMFADelete(t)
	f.enableUserMFA(t)
	_, err := f.db.Exec(`UPDATE buckets SET versioning_status = 'Enabled' WHERE tenant_id = $1 AND name = $2`, f.tenantID, f.bucket)
	require.NoError(t, err)
	t.Cleanup(func() { _, _ = f.db.Exec(`DELETE FROM object_versions WHERE tenant_id = $1`, f.tenantID) })
	f.putObject(t, "a.txt", "a")
	f.putObject(t, "b.txt", "b")
	var vid string
	require.NoError(t, f.db.QueryRow(`SELECT version_id FROM object_versions WHERE tenant_id = $1 AND bucket = $2 AND object_key = 'b.txt' AND is_latest`, f.tenantID, f.bucket).Scan(&vid))

	send := func(method, path, body, mfa string, handler func(http.ResponseWriter, *http.Request)) int {
		req := httptest.NewRequest(method, path, bytes.NewReader([]byte(body)))
		if mfa != "" {
			req.Header.Set("x-amz-mfa", "arn:aws:iam::serial "+mfa)
		}
		req = req.WithContext(s3Ctx(req.Context(), f.tenant))
		w := httptest.NewRecorder()
		handler(w, req)
		return w.Code
	}
	single := func(w http.ResponseWriter, r *http.Request) {
		f.server.handleDeleteObject(w, r, &S3Request{Bucket: f.bucket, Object: "a.txt", TenantID: f.tenantID})
	}
	version := func(w http.ResponseWriter, r *http.Request) {
		f.server.handleDeleteObject(w, r, &S3Request{Bucket: f.bucket, Object: "b.txt", TenantID: f.tenantID})
	}
	batch := func(w http.ResponseWriter, r *http.Request) {
		f.server.handleDeleteObjects(w, r, &S3Request{Bucket: f.bucket, TenantID: f.tenantID})
	}
	batchBody := `<Delete><Object><Key>a.txt</Key></Object></Delete>`

	// Without the header: every flavour 403.
	assert.Equal(t, http.StatusForbidden, send("DELETE", "/"+f.bucket+"/a.txt", "", "", single))
	assert.Equal(t, http.StatusForbidden, send("DELETE", "/"+f.bucket+"/b.txt?versionId="+vid, "", "", version))
	assert.Equal(t, http.StatusForbidden, send("POST", "/"+f.bucket+"?delete", batchBody, "", batch))
	// With a fresh code each: the marker (204), the version delete (204), the batch (200).
	assert.Equal(t, http.StatusNoContent, send("DELETE", "/"+f.bucket+"/a.txt", "", mfaCodeAt(t, time.Now().Add(-30*time.Second)), single))
	assert.Equal(t, http.StatusNoContent, send("DELETE", "/"+f.bucket+"/b.txt?versionId="+vid, "", mfaCodeAt(t, time.Now()), version))
	assert.Equal(t, http.StatusOK, send("POST", "/"+f.bucket+"?delete", batchBody, mfaCodeAt(t, time.Now().Add(30*time.Second)), batch))
}
