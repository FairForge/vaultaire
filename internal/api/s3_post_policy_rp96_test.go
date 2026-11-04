package api

// RP96-B-2: the by-key auth-failure series is for KNOWN keys (R11-28: a
// label hashed from an attacker-chosen id before any lookup is unbounded
// Prometheus cardinality). A POST form naming an id no one has — in the
// SigV2 shape (AWSAccessKeyId, refused before the lookup) or the SigV4 one —
// must add no series at either door.

import (
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPostPolicy_AnUnknownKeysFormAddsNoByKeySeries(t *testing.T) {
	f := setupEdgeFixture(t)
	conds := []any{map[string]any{"bucket": f.bucket}, []any{"starts-with", "$key", ""}, []any{"content-length-range", 1, 10 << 20}}
	base := f.policyFields(time.Now().Add(time.Hour), conds, formField{"key", "k.bin"})
	day := time.Now().UTC().Format("20060102")
	before := testutil.CollectAndCount(authFailuresByKey)
	for i := 0; i < 20; i++ {
		id := fmt.Sprintf("AKIARP96%012d", i)
		var v2, v4 []formField
		for _, fl := range base {
			if fl.name == "x-amz-credential" {
				fl.value = id + "/" + day + "/us-east-1/s3/aws4_request"
			}
			v4 = append(v4, fl)
			if fl.name != "x-amz-algorithm" {
				v2 = append(v2, fl)
			}
		}
		v2 = append(v2, formField{"AWSAccessKeyId", id})
		for _, fields := range [][]formField{v2, v4} {
			body, ct := buildPostForm(t, fields, "r.bin", []byte("x"))
			w := f.post(f.bucket, body, ct)
			require.True(t, w.Code == http.StatusBadRequest || w.Code == http.StatusForbidden, "%d %s", w.Code, w.Body.String())
			resp := f.admit(f.postAdmit(fields, "r.png", 8192))
			require.NotNil(t, resp.Refusal, "%+v", resp)
		}
	}
	assert.Equal(t, before, testutil.CollectAndCount(authFailuresByKey), "an id no one has is never a by-key series")
}
