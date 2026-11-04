package api

// RP94-4: the Object Lock fields, x-amz-checksum-* and
// x-amz-website-redirect-location a policy names were accepted and
// silently dropped (postPolicyFieldHeaders mapped eleven headers, and every
// other named field was ignored): a signer who REQUIRED COMPLIANCE got an
// unlocked object, a wrong CRC32 was never checked. They are mapped onto
// the inner PUT's headers now, so the PUT path applies and verifies them;
// any other x-amz-* field the PUT path would ignore is 501 NotImplemented,
// as tagging is — never accepted and dropped.

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"hash/crc32"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func crc32B64(b []byte) string {
	var out [4]byte
	binary.BigEndian.PutUint32(out[:], crc32.ChecksumIEEE(b))
	return base64.StdEncoding.EncodeToString(out[:])
}

func TestPostPolicy_LockAndChecksumFieldsAreApplied(t *testing.T) {
	f := setupEdgeFixture(t)
	f.exec(`UPDATE buckets SET object_lock_enabled = TRUE, versioning_status = 'Enabled' WHERE tenant_id = $1 AND name = $2`, f.tenantID, f.bucket)
	until := time.Now().Add(48 * time.Hour).UTC().Format(time.RFC3339)
	content := bytes.Repeat([]byte("L"), 400)
	base := []any{map[string]any{"bucket": f.bucket}, []any{"starts-with", "$key", "in/"}, []any{"content-length-range", 1, 1000}}

	// COMPLIANCE, as the policy requires: an object_locks row.
	conds := append(append([]any{}, base...), map[string]any{"x-amz-object-lock-mode": "COMPLIANCE"},
		map[string]any{"x-amz-object-lock-retain-until-date": until}, map[string]any{"x-amz-checksum-crc32": crc32B64(content)})
	fields := f.policyFields(time.Now().Add(time.Hour), conds, formField{"key", "in/locked.bin"},
		formField{"x-amz-object-lock-mode", "COMPLIANCE"}, formField{"x-amz-object-lock-retain-until-date", until},
		formField{"x-amz-checksum-crc32", crc32B64(content)})
	body, ct := buildPostForm(t, fields, "locked.bin", content)
	w := f.post(f.bucket, body, ct)
	require.Equal(t, http.StatusNoContent, w.Code, w.Body.String())
	var mode string
	require.NoError(t, f.db.QueryRow(`SELECT retention_mode FROM object_locks WHERE tenant_id = $1 AND bucket = $2 AND object_key = 'in/locked.bin'`,
		f.tenantID, f.bucket).Scan(&mode), "the lock the policy required")
	assert.Equal(t, "COMPLIANCE", mode)

	// A wrong CRC32 the policy names: refused, nothing stored.
	conds = append(append([]any{}, base...), map[string]any{"x-amz-checksum-crc32": "AAAAAA=="})
	fields = f.policyFields(time.Now().Add(time.Hour), conds, formField{"key", "in/badsum.bin"}, formField{"x-amz-checksum-crc32", "AAAAAA=="})
	body, ct = buildPostForm(t, fields, "badsum.bin", content)
	w = f.post(f.bucket, body, ct)
	assert.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
	assert.Equal(t, ErrXAmzContentChecksumMismatch, postErrCode(t, w))
	_, _, _, ok := f.headRow("in/badsum.bin")
	assert.False(t, ok, "nothing stored")

	// An x-amz-* field the PUT path would ignore: 501, as tagging is.
	conds = append(append([]any{}, base...), map[string]any{"x-amz-grant-read": "uri=x"})
	fields = f.policyFields(time.Now().Add(time.Hour), conds, formField{"key", "in/grant.bin"}, formField{"x-amz-grant-read", "uri=x"})
	body, ct = buildPostForm(t, fields, "grant.bin", content)
	w = f.post(f.bucket, body, ct)
	assert.Equal(t, http.StatusNotImplemented, w.Code, w.Body.String())
	_, _, _, ok = f.headRow("in/grant.bin")
	assert.False(t, ok)
}

// At the edge a form naming a checksum is the origin's: the edge's link
// signs no checksum header, so the backend would not verify it.
func TestPostPolicy_EdgePassesAChecksumFormToTheOrigin(t *testing.T) {
	f := setupEdgeFixture(t)
	conds := []any{map[string]any{"bucket": f.bucket}, []any{"starts-with", "$key", "in/"}, []any{"content-length-range", 1, 10 << 20},
		map[string]any{"x-amz-checksum-crc32": "AAAAAA=="}}
	fields := f.policyFields(time.Now().Add(time.Hour), conds, formField{"key", "in/${filename}"}, formField{"x-amz-checksum-crc32", "AAAAAA=="})
	resp := f.admit(f.postAdmit(fields, "sum.png", 8192))
	assert.False(t, resp.Edge, "%+v", resp)
	assert.Equal(t, "post_checksum", resp.Reason)
}
