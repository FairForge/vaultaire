package api

// RP94-A-1 (the review of WP-POST-1): a form with a part AFTER the file.
// D-POST-2 takes the file's size as the body less the fields before it and
// the closing delimiter — which assumes the file is the LAST part. A named
// submit button or a hidden field after the file input (a common page) put
// a part there: its bytes became the file's tail, the closing-delimiter
// check passed (the body did end at the real delimiter), and the object was
// stored CORRUPTED (file ‖ delimiter ‖ the part) with a 204 and an ETag of
// the wrong bytes. S3 ignores parts after the file; Stored refuses the form
// (400 MalformedPOSTRequest) — never stores them.

import (
	"bytes"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"testing"
	"testing/iotest"
	"time"

	"github.com/FairForge/vaultaire/internal/dataplane"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// buildPostFormWithTrailer is buildPostForm with more fields AFTER the
// file part.
func buildPostFormWithTrailer(t *testing.T, fields []formField, filename string, content []byte, after []formField) ([]byte, string) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for _, f := range fields {
		require.NoError(t, mw.WriteField(f.name, f.value))
	}
	h := textproto.MIMEHeader{}
	h.Set("Content-Disposition", fmt.Sprintf(`form-data; name="file"; filename=%q`, filename))
	h.Set("Content-Type", "application/octet-stream")
	pw, err := mw.CreatePart(h)
	require.NoError(t, err)
	_, err = pw.Write(content)
	require.NoError(t, err)
	for _, f := range after {
		require.NoError(t, mw.WriteField(f.name, f.value))
	}
	require.NoError(t, mw.Close())
	return buf.Bytes(), mw.FormDataContentType()
}

func TestPostPolicy_APartAfterTheFileIsRefusedNeverStored(t *testing.T) {
	f := setupEdgeFixture(t)
	conds := []any{map[string]any{"bucket": f.bucket}, []any{"starts-with", "$key", "in/"}, []any{"content-length-range", 1, 1000}}
	fields := f.policyFields(time.Now().Add(time.Hour), conds, formField{"key", "in/${filename}"})
	before := f.storageUsed()
	content := bytes.Repeat([]byte("q"), 300)

	// A named submit button after the file input: S3 ignores it; before
	// RP94-A-1 its bytes were stored as the file's tail with a 204.
	body, ct := buildPostFormWithTrailer(t, fields, "trail.bin", content, []formField{{"submit", "Upload"}})
	w := f.post(f.bucket, body, ct)
	assert.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
	assert.Equal(t, ErrMalformedPOSTRequest, postErrCode(t, w))
	assert.Contains(t, w.Body.String(), "follows the file")
	_, _, _, ok := f.headRow("in/trail.bin")
	assert.False(t, ok, "nothing committed")
	assert.Equal(t, before, f.storageUsed(), "the reservation released")

	// The same form with the file last: stored, the ETag the file's MD5.
	body, ct = buildPostForm(t, append([]formField{{"submit", "Upload"}}, fields...), "last.bin", content)
	w = f.post(f.bucket, body, ct)
	// "submit" is not a field the policy names: refused as an extra field —
	// the policy decides, as on S3. Name it and it is stored.
	assert.Equal(t, http.StatusForbidden, w.Code, w.Body.String())
	conds = append(conds, map[string]any{"submit": "Upload"})
	fields = f.policyFields(time.Now().Add(time.Hour), conds, formField{"key", "in/${filename}"}, formField{"submit", "Upload"})
	body, ct = buildPostForm(t, fields, "last.bin", content)
	w = f.post(f.bucket, body, ct)
	require.Equal(t, http.StatusNoContent, w.Code, w.Body.String())
	etag, _, _, ok := f.headRow("in/last.bin")
	require.True(t, ok)
	assert.Equal(t, postMD5(content), etag)
}

// The file reader stops at a delimiter inside the declared file bytes
// whatever the consumer's read size — a delimiter split across two reads
// included — and passes a file whose bytes merely resemble one.
func TestPostFileReader_ADelimiterInsideTheFileIsAnErrorAtEveryReadSize(t *testing.T) {
	fields := []formField{{"key", "k"}, {"policy", "p"}}
	content := []byte("file bytes\r\n--not-the-boundary--\r\nmore -- dashes\r\n")
	for _, size := range []int{1, 2, 3, 7, 64, 4096} {
		// A part after the file.
		body, ct := buildPostFormWithTrailer(t, fields, "a.bin", content, []formField{{"submit", "go"}})
		form, err := readPostForm(postRequest(body, ct))
		require.NoError(t, err)
		_, err = io.ReadAll(iotest.OneByteReader(sizedReader{form.file, size}))
		require.Error(t, err, "read size %d", size)
		assert.ErrorIs(t, err, errPostMalformed)
		assert.Contains(t, err.Error(), "follows the file")

		// The file last: every byte, then EOF.
		body, ct = buildPostForm(t, fields, "a.bin", content)
		form, err = readPostForm(postRequest(body, ct))
		require.NoError(t, err)
		got, err := io.ReadAll(sizedReader{form.file, size})
		require.NoError(t, err, "read size %d", size)
		assert.Equal(t, content, got)
	}
}

// RP94-B-1/B-2: a host that does not announce FeaturePostTail (the Worker
// 0.7.0) gets no form admitted — the origin serves it, with the reader
// above.
func TestPostPolicy_EdgeAdmitsAFormOnlyFromAHostThatProvesTheTail(t *testing.T) {
	f := setupEdgeFixture(t)
	conds := []any{map[string]any{"bucket": f.bucket}, []any{"starts-with", "$key", "in/"}, []any{"content-length-range", 1, 10 << 20}}
	fields := f.policyFields(time.Now().Add(time.Hour), conds, formField{"key", "in/${filename}"})
	req := f.postAdmit(fields, "cat.png", 8192)
	req.Features = nil // the 0.7.0 host
	resp := f.admit(req)
	assert.False(t, resp.Edge, "%+v", resp)
	assert.Equal(t, "post_tail_unproven", resp.Reason)
	var n int
	require.NoError(t, f.db.QueryRow(`SELECT count(*) FROM edge_admits WHERE tenant_id = $1`, f.tenantID).Scan(&n))
	assert.Equal(t, 0, n, "nothing admitted, nothing reserved")

	req.Features = []string{dataplane.FeaturePostTail}
	resp = f.admit(req)
	assert.True(t, resp.Edge, "%+v", resp)
}

func postRequest(body []byte, ct string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/b", bytes.NewReader(body))
	r.Header.Set("Content-Type", ct)
	r.ContentLength = int64(len(body))
	return r
}

// sizedReader caps every Read at n bytes.
type sizedReader struct {
	r io.Reader
	n int
}

func (s sizedReader) Read(p []byte) (int, error) {
	if len(p) > s.n {
		p = p[:s.n]
	}
	return s.r.Read(p)
}
