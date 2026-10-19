package api

import (
	"encoding/xml"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// WP-R3-4 R4-17: DeleteObjects on an MFA-Delete bucket. The single DELETE
// checks x-amz-mfa before anything (handleDeleteObject); the batch did not, so
// POST /{bucket}?delete deleted every key of an mfa_delete_enabled bucket
// without a code — the way around the gate.

var batchMFAKeys = []string{"a.txt", "b.txt", "c.txt"}

func (f *mfaDeleteFixture) batchDelete(t *testing.T, mfa string, keys ...string) *httptest.ResponseRecorder {
	t.Helper()
	var body strings.Builder
	body.WriteString("<Delete>")
	for _, k := range keys {
		fmt.Fprintf(&body, "<Object><Key>%s</Key></Object>", k)
	}
	body.WriteString("</Delete>")
	r := httptest.NewRequest("POST", "/"+f.bucket+"?delete", strings.NewReader(body.String()))
	if mfa != "" {
		r.Header.Set("x-amz-mfa", mfa)
	}
	r = r.WithContext(s3Ctx(r.Context(), f.tenant))
	w := httptest.NewRecorder()
	f.server.handleDeleteObjects(w, r, &S3Request{Bucket: f.bucket, TenantID: f.tenantID})
	return w
}

func (f *mfaDeleteFixture) headRows(t *testing.T, keys ...string) int {
	t.Helper()
	var n int
	require.NoError(t, f.db.QueryRow(`SELECT count(*) FROM object_head_cache
		WHERE tenant_id = $1 AND bucket = $2 AND object_key = ANY($3)`,
		f.tenantID, f.bucket, "{"+strings.Join(keys, ",")+"}").Scan(&n))
	return n
}

func deletedKeys(t *testing.T, w *httptest.ResponseRecorder) []string {
	t.Helper()
	var res DeleteResult
	require.NoError(t, xml.Unmarshal(w.Body.Bytes(), &res), w.Body.String())
	var keys []string
	for _, d := range res.Deleted {
		keys = append(keys, d.Key)
	}
	return keys
}

func TestMFADelete_BatchWithoutHeaderIsRefusedOnAnMFABucket(t *testing.T) {
	// Arrange
	f := setupMFADeleteFixture(t)
	f.enableMFADelete(t)
	f.enableUserMFA(t)
	for _, k := range batchMFAKeys {
		f.putObject(t, k, "protected "+k)
	}

	// Act: before WP-R3-4 this answered 200 with every key <Deleted>.
	w := f.batchDelete(t, "", batchMFAKeys...)

	// Assert
	require.Equal(t, http.StatusForbidden, w.Code, w.Body.String())
	var errResp S3Error
	require.NoError(t, xml.Unmarshal(w.Body.Bytes(), &errResp))
	assert.Equal(t, "AccessDenied", errResp.Code)
	assert.Equal(t, len(batchMFAKeys), f.headRows(t, batchMFAKeys...), "every head row intact")
}

func TestMFADelete_BatchWithAnInvalidCodeIsRefused(t *testing.T) {
	f := setupMFADeleteFixture(t)
	f.enableMFADelete(t)
	f.enableUserMFA(t)
	for _, k := range batchMFAKeys {
		f.putObject(t, k, "protected "+k)
	}

	w := f.batchDelete(t, "arn:aws:iam::serial 999999", batchMFAKeys...)

	require.Equal(t, http.StatusForbidden, w.Code, w.Body.String())
	assert.Equal(t, len(batchMFAKeys), f.headRows(t, batchMFAKeys...))
}

func TestMFADelete_BatchOnAnMFABucketWithNoMFAOnTheAccountIsRefused(t *testing.T) {
	// mfaNotConfiguredError: the bucket asks for MFA, the account has none.
	f := setupMFADeleteFixture(t)
	f.enableMFADelete(t)
	for _, k := range batchMFAKeys {
		f.putObject(t, k, "protected "+k)
	}

	w := f.batchDelete(t, "arn:aws:iam::serial 123456", batchMFAKeys...)

	require.Equal(t, http.StatusForbidden, w.Code, w.Body.String())
	assert.Equal(t, len(batchMFAKeys), f.headRows(t, batchMFAKeys...))
}

func TestMFADelete_BatchWithAValidCodeDeletes(t *testing.T) {
	f := setupMFADeleteFixture(t)
	f.enableMFADelete(t)
	f.enableUserMFA(t)
	for _, k := range batchMFAKeys {
		f.putObject(t, k, "protected "+k)
	}

	w := f.batchDelete(t, "arn:aws:iam::serial "+testMFACode(t), batchMFAKeys...)

	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.ElementsMatch(t, batchMFAKeys, deletedKeys(t, w))
	assert.Zero(t, f.headRows(t, batchMFAKeys...))
}

func TestMFADelete_BatchRefusalPrecedesTheKeepAlive(t *testing.T) {
	// The refusal is decided before longOpBegin: a real 403, never a committed
	// 200 carrying an <Error> (internal/api/CLAUDE.md, s3_long_op.go).
	f := setupMFADeleteFixture(t)
	instantCommit(f.server)
	f.enableMFADelete(t)
	f.enableUserMFA(t)
	f.putObject(t, "a.txt", "protected")

	w := f.batchDelete(t, "", "a.txt")

	assert.Equal(t, http.StatusForbidden, w.Code, w.Body.String())
	assert.True(t, strings.HasPrefix(w.Body.String(), "<?xml"), "no keep-alive whitespace: %q", w.Body.String())
	assert.Contains(t, w.Body.String(), "<Code>AccessDenied</Code>")
	assert.Equal(t, 1, f.headRows(t, "a.txt"))
}

func TestMFADelete_BatchOnANonMFABucketNeedsNoHeader(t *testing.T) {
	f := setupMFADeleteFixture(t)
	for _, k := range batchMFAKeys {
		f.putObject(t, k, "plain "+k)
	}

	w := f.batchDelete(t, "", batchMFAKeys...)

	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.ElementsMatch(t, batchMFAKeys, deletedKeys(t, w))
	assert.Zero(t, f.headRows(t, batchMFAKeys...))
}
