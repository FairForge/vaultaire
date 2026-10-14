package api

// WP-R10-8 Part C (R10-24, R3-14): multipart parts are reserved as they
// arrive, on the upload row (091); Complete settles them into the object,
// Abort / the reaper / the erasure release them, the reconcile counts
// them. Before (origin/main): parts were staged at no quota cost — a free
// tenant could pin 50 GiB × N of hub disk for 48 h.

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/FairForge/vaultaire/internal/usage"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// createUpload starts a multipart upload of key and returns its id.
func (f *quotaAccountingFixture) createUpload(t *testing.T, key string, hdr map[string]string) string {
	t.Helper()
	r := httptest.NewRequest("POST", "/test-bucket/"+key+"?uploads", nil)
	for k, v := range hdr {
		r.Header.Set(k, v)
	}
	r = r.WithContext(s3Ctx(r.Context(), f.tenant))
	w := httptest.NewRecorder()
	f.server.handleS3Request(w, r)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var init InitiateMultipartUploadResult
	require.NoError(t, xml.Unmarshal(w.Body.Bytes(), &init))
	t.Cleanup(func() { _ = os.RemoveAll(multipartDir(init.UploadID)) })
	return init.UploadID
}

// uploadPart PUTs part n of the upload with body.
func (f *quotaAccountingFixture) uploadPart(key, id string, n int, body io.Reader, length int64) *httptest.ResponseRecorder {
	r := httptest.NewRequest("PUT", fmt.Sprintf("/test-bucket/%s?uploadId=%s&partNumber=%d", key, id, n), body)
	r.ContentLength = length
	r = r.WithContext(s3Ctx(r.Context(), f.tenant))
	w := httptest.NewRecorder()
	f.server.handleS3Request(w, r)
	return w
}

func (f *quotaAccountingFixture) putPart(t *testing.T, key, id string, n, size int) *httptest.ResponseRecorder {
	t.Helper()
	return f.uploadPart(key, id, n, bytes.NewReader(testBytes(size)), int64(size))
}

func (f *quotaAccountingFixture) uploadHolds(t *testing.T, id string) int64 {
	t.Helper()
	var n int64
	require.NoError(t, f.db.QueryRow(`SELECT reserved_bytes FROM multipart_uploads WHERE upload_id = $1`, id).Scan(&n))
	return n
}

func TestUploadPart_ReservesBeforeStaging_ThreeParts(t *testing.T) {
	// Before: used stayed 0 through every part.
	f := setupQuotaAccountingFixture(t, 1<<30)
	id := f.createUpload(t, "big.bin", nil)
	for i := 1; i <= 3; i++ {
		require.Equal(t, http.StatusOK, f.putPart(t, "big.bin", id, i, 64*kib).Code)
		assert.Equal(t, int64(i*64*kib), f.used(t), "after part %d", i)
	}
	assert.Equal(t, int64(192*kib), f.uploadHolds(t, id))
	f.ledgerAgrees(t, "three parts in flight")
}

func TestUploadPart_PastTheQuota_403NothingStaged(t *testing.T) {
	// A free-tier sized limit: the part that would pass it is refused.
	// Before: 200 — staged at no quota cost.
	f := setupQuotaAccountingFixture(t, 100*kib)
	id := f.createUpload(t, "big.bin", nil)
	require.Equal(t, http.StatusOK, f.putPart(t, "big.bin", id, 1, 64*kib).Code)

	w := f.putPart(t, "big.bin", id, 2, 64*kib)

	assert.Equal(t, http.StatusForbidden, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), "QuotaExceeded")
	_, err := os.Stat(partFilePath(id, 2))
	assert.True(t, os.IsNotExist(err), "nothing staged")
	assert.Equal(t, int64(64*kib), f.used(t))
	f.ledgerAgrees(t, "after the refused part")
}

func TestUploadPart_ReuploadOfAPartNumber_HoldsTheDifference(t *testing.T) {
	f := setupQuotaAccountingFixture(t, 1<<30)
	id := f.createUpload(t, "big.bin", nil)
	require.Equal(t, http.StatusOK, f.putPart(t, "big.bin", id, 1, 64*kib).Code)
	require.Equal(t, http.StatusOK, f.putPart(t, "big.bin", id, 1, 32*kib).Code)
	assert.Equal(t, int64(32*kib), f.used(t), "a smaller re-upload gives the difference back")
	require.Equal(t, http.StatusOK, f.putPart(t, "big.bin", id, 1, 80*kib).Code)
	assert.Equal(t, int64(80*kib), f.used(t))
	assert.Equal(t, int64(80*kib), f.uploadHolds(t, id))
	f.ledgerAgrees(t, "after the re-uploads")
}

func TestUploadPart_ReuploadAtTheQuotaEdge_NoRefusal(t *testing.T) {
	// A retried part of the same size at 100 %: the part number already
	// holds its bytes.
	f := setupQuotaAccountingFixture(t, 64*kib)
	id := f.createUpload(t, "big.bin", nil)
	require.Equal(t, http.StatusOK, f.putPart(t, "big.bin", id, 1, 64*kib).Code)
	assert.Equal(t, http.StatusOK, f.putPart(t, "big.bin", id, 1, 64*kib).Code)
	f.ledgerAgrees(t, "after the retry")
}

// cutPartBody yields n bytes, then fails.
type cutPartBody struct{ n int }

func (b *cutPartBody) Read(p []byte) (int, error) {
	if b.n <= 0 {
		return 0, errors.New("connection reset by peer")
	}
	k := min(len(p), b.n)
	b.n -= k
	return k, nil
}

func TestUploadPart_CutBody_GivesItsReservationBack(t *testing.T) {
	f := setupQuotaAccountingFixture(t, 1<<30)
	id := f.createUpload(t, "big.bin", nil)
	w := f.uploadPart("big.bin", id, 1, &cutPartBody{n: 10 * kib}, 64*kib)
	assert.NotEqual(t, http.StatusOK, w.Code)
	assert.Zero(t, f.used(t), "the cut part's reservation is given back")
	assert.Zero(t, f.uploadHolds(t, id))
}

func TestAbort_ReleasesWhatTheUploadHolds(t *testing.T) {
	f := setupQuotaAccountingFixture(t, 1<<30)
	require.Equal(t, http.StatusOK, f.put(t, "other", testBytes(1000)))
	id := f.createUpload(t, "big.bin", nil)
	for i := 1; i <= 3; i++ {
		require.Equal(t, http.StatusOK, f.putPart(t, "big.bin", id, i, 64*kib).Code)
	}
	w := doS3Request(f.server, f.tenant, "DELETE", "/test-bucket/big.bin?uploadId="+id, nil)
	require.Equal(t, http.StatusNoContent, w.Code)
	assert.Equal(t, int64(1000), f.used(t), "back to the start")
	assert.Zero(t, f.uploadHolds(t, id))
	f.ledgerAgrees(t, "after the abort")
}

func TestComplete_SettlesThePartsIntoTheObject_UnlistedReleased(t *testing.T) {
	f := setupQuotaAccountingFixture(t, 1<<30)
	id := f.createUpload(t, "big.bin", nil)
	var etags []string
	for i := 1; i <= 3; i++ {
		w := f.putPart(t, "big.bin", id, i, 64*kib)
		require.Equal(t, http.StatusOK, w.Code)
		etags = append(etags, w.Header().Get("ETag"))
	}
	body := "<CompleteMultipartUpload>" +
		"<Part><PartNumber>1</PartNumber><ETag>" + etags[0] + "</ETag></Part>" +
		"<Part><PartNumber>2</PartNumber><ETag>" + etags[1] + "</ETag></Part>" +
		"</CompleteMultipartUpload>"

	w := doS3Request(f.server, f.tenant, "POST", "/test-bucket/big.bin?uploadId="+id, strings.NewReader(body))

	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Equal(t, int64(128*kib), f.used(t), "exactly the object; part 3 released")
	assert.Zero(t, f.uploadHolds(t, id))
	f.ledgerAgrees(t, "after the complete")
}

func TestComplete_OverwriteAndAttic_SettleOnTheirFloors(t *testing.T) {
	// A GLACIER upload over a downstairs object: the parts are held in the
	// attic, the object lands there, the downstairs object is released.
	f := setupQuotaAccountingFixture(t, 1<<30)
	f.house(t, 1<<20, 1<<20)
	require.Equal(t, http.StatusOK, f.put(t, "big.bin", testBytes(100*kib)))
	id := f.createUpload(t, "big.bin", map[string]string{"x-amz-storage-class": "GLACIER"})
	var parts strings.Builder
	parts.WriteString("<CompleteMultipartUpload>")
	for i := 1; i <= 2; i++ {
		w := f.putPart(t, "big.bin", id, i, 64*kib)
		require.Equal(t, http.StatusOK, w.Code)
		fmt.Fprintf(&parts, "<Part><PartNumber>%d</PartNumber><ETag>%s</ETag></Part>", i, w.Header().Get("ETag"))
	}
	parts.WriteString("</CompleteMultipartUpload>")
	std, vault := f.floors(t)
	assert.Equal(t, int64(100*kib), std)
	assert.Equal(t, int64(128*kib), vault, "the parts are held on the upload's floor")

	w := doS3Request(f.server, f.tenant, "POST", "/test-bucket/big.bin?uploadId="+id, strings.NewReader(parts.String()))

	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	std, vault = f.floors(t)
	assert.Zero(t, std)
	assert.Equal(t, int64(128*kib), vault)
	f.ledgerAgrees(t, "after the attic complete")
}

func TestReconcile_CountsWhatActiveUploadsHold(t *testing.T) {
	// Before: the first reconcile after a part freed it.
	f := setupQuotaAccountingFixture(t, 1<<30)
	id := f.createUpload(t, "big.bin", nil)
	require.Equal(t, http.StatusOK, f.putPart(t, "big.bin", id, 1, 64*kib).Code)
	require.NoError(t, f.qm.ReconcileTenantStorageUsage(context.Background(), f.tenantID))
	assert.Equal(t, int64(64*kib), f.used(t))
}

func TestSetHouse_SeedsAFloorRowWithWhatUploadsHold(t *testing.T) {
	// R10-19: the new floor row is seeded by the reconcile's computation —
	// uploads included — so the Complete's settle finds them there.
	f := setupQuotaAccountingFixture(t, 1<<30)
	id := f.createUpload(t, "big.bin", nil)
	require.Equal(t, http.StatusOK, f.putPart(t, "big.bin", id, 1, 64*kib).Code)
	require.NoError(t, f.qm.SetHouse(context.Background(), f.tenantID, usage.House{StdBytes: 1 << 30}))
	std, _ := f.floors(t)
	assert.Equal(t, int64(64*kib), std)
	f.ledgerAgrees(t, "after the house")
}
