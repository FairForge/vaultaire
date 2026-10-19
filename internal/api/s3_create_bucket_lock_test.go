package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// RP16-4: CreateBucket ignored x-amz-bucket-object-lock-enabled, so
// `aws s3api create-bucket --object-lock-enabled-for-bucket` made a bucket
// without Object Lock. Now the header creates it with lock on and versioning
// Enabled (as AWS does), a malformed value is 400 InvalidArgument, and a
// re-create of an owned unlocked bucket asking for lock is 409.

func (f *versioningFixture) createBucket(t *testing.T, bucket, lockHeader string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("PUT", "/"+bucket, nil)
	if lockHeader != "" {
		req.Header.Set("x-amz-bucket-object-lock-enabled", lockHeader)
	}
	req = req.WithContext(s3Ctx(req.Context(), f.tenant))
	w := httptest.NewRecorder()
	f.server.CreateBucket(w, req)
	return w
}

func (f *versioningFixture) bucketFlags(t *testing.T, bucket string) (exists, lock bool, versioning string) {
	t.Helper()
	err := f.db.QueryRow(`SELECT object_lock_enabled, versioning_status FROM buckets WHERE tenant_id = $1 AND name = $2`,
		f.tenantID, bucket).Scan(&lock, &versioning)
	if err != nil {
		return false, false, ""
	}
	return true, lock, versioning
}

func TestCreateBucket_ObjectLockHeaderCreatesALockedVersionedBucket(t *testing.T) {
	f := setupVersioningFixture(t)
	bucket := "lock-on-create"

	w := f.createBucket(t, bucket, "True")

	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	exists, lock, versioning := f.bucketFlags(t, bucket)
	require.True(t, exists)
	assert.True(t, lock, "object_lock_enabled set at creation")
	assert.Equal(t, "Enabled", versioning, "Object Lock implies versioning Enabled")

	// PutObjectLockConfiguration sets a default rule with no separate
	// enable step, and lock cannot be turned off: it was on from creation.
	putCfg := func(body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("PUT", "/"+bucket+"?object-lock", strings.NewReader(body))
		req = req.WithContext(s3Ctx(req.Context(), f.tenant))
		rec := httptest.NewRecorder()
		f.server.handlePutObjectLockConfiguration(rec, req, &S3Request{Bucket: bucket, TenantID: f.tenantID})
		return rec
	}
	assert.Equal(t, http.StatusConflict, putCfg(`<ObjectLockConfiguration></ObjectLockConfiguration>`).Code,
		"lock is on: a configuration without it is refused")
	assert.Equal(t, http.StatusOK, putCfg(`<ObjectLockConfiguration><ObjectLockEnabled>Enabled</ObjectLockEnabled><Rule><DefaultRetention><Mode>GOVERNANCE</Mode><Days>1</Days></DefaultRetention></Rule></ObjectLockConfiguration>`).Code)
}

func TestCreateBucket_ObjectLockHeaderFalseOrAbsentIsAPlainBucket(t *testing.T) {
	f := setupVersioningFixture(t)
	for bucket, hdr := range map[string]string{"lock-false": "FALSE", "lock-absent": ""} {
		w := f.createBucket(t, bucket, hdr)
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		exists, lock, versioning := f.bucketFlags(t, bucket)
		require.True(t, exists, bucket)
		assert.False(t, lock, bucket)
		assert.Equal(t, "disabled", versioning, bucket)
	}
}

func TestCreateBucket_AMalformedObjectLockHeaderIs400AndCreatesNothing(t *testing.T) {
	f := setupVersioningFixture(t)

	w := f.createBucket(t, "lock-malformed", "yes")

	assert.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), ErrInvalidArgument)
	exists, _, _ := f.bucketFlags(t, "lock-malformed")
	assert.False(t, exists)
}

func TestCreateBucket_ReCreatingAnUnlockedOwnedBucketAskingForLockIs409(t *testing.T) {
	f := setupVersioningFixture(t)
	require.Equal(t, http.StatusOK, f.createBucket(t, "lock-later", "").Code)

	w := f.createBucket(t, "lock-later", "true")

	assert.Equal(t, http.StatusConflict, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), ErrInvalidBucketState)
	_, lock, _ := f.bucketFlags(t, "lock-later")
	assert.False(t, lock, "nothing changed")
	assert.Equal(t, http.StatusOK, f.createBucket(t, "lock-later", "").Code, "a plain re-create stays idempotent")
}
