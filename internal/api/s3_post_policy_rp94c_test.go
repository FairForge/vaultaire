package api

// RP94-5: vaultaire_auth_failures_by_key_total was blind to POST — the
// refusal was recorded with the OUTER request, whose context names no
// postPolicyUpload, so attemptedAccessKey was "" and the by-key series
// never moved: the "burst against a real key" page could not see a
// revoked- or IP-denied key's forms. The refusal carries the credential's
// key id now and both doors record it.

import (
	"net/http"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPostPolicy_ARefusedFormCountsAgainstItsKey(t *testing.T) {
	f := setupEdgeFixture(t)
	conds := []any{map[string]any{"bucket": f.bucket}, []any{"starts-with", "$key", "in/"}, []any{"content-length-range", 1, 10 << 20}}
	fields := f.policyFields(time.Now().Add(time.Hour), conds, formField{"key", "in/${filename}"})
	f.exec(`UPDATE api_keys SET revoked_at = NOW() WHERE key_id = $1`, f.ak)
	series := authFailuresByKey.WithLabelValues(accessKeyHash(f.ak))

	// The origin's door.
	before := testutil.ToFloat64(series)
	body, ct := buildPostForm(t, fields, "r.bin", []byte("revoked"))
	w := f.post(f.bucket, body, ct)
	require.Equal(t, http.StatusForbidden, w.Code, w.Body.String())
	assert.Equal(t, ErrInvalidAccessKeyId, postErrCode(t, w))
	assert.Equal(t, before+1, testutil.ToFloat64(series), "the origin's refusal counts against the form's key")

	// The edge's door.
	before = testutil.ToFloat64(series)
	resp := f.admit(f.postAdmit(fields, "r.png", 8192))
	require.NotNil(t, resp.Refusal, "%+v", resp)
	assert.Equal(t, http.StatusForbidden, resp.Refusal.Status)
	assert.Equal(t, before+1, testutil.ToFloat64(series), "the edge's refusal counts against the form's key")
}
