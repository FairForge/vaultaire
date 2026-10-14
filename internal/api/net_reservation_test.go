package api

// WP-R10-8 Part B (R10-20): a write reserves size − displaced on the floor
// where both live, displaced = what its commit will release (the commit's
// own rule, dry-run: predictRelease). Before: reserve-before-release — at
// 100 % of a floor an overwrite with a SMALLER object was 403.

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const kib = 1 << 10

// fullFloor gives the tenant a 30 KiB downstairs and fills it: k (10 KiB) +
// fill (20 KiB).
func fullFloor(t *testing.T) *quotaAccountingFixture {
	t.Helper()
	f := setupQuotaAccountingFixture(t, 1<<30)
	f.house(t, 30*kib, 0)
	require.Equal(t, http.StatusOK, f.put(t, "k", testBytes(10*kib)))
	require.Equal(t, http.StatusOK, f.put(t, "fill", testBytes(20*kib)))
	std, _ := f.floors(t)
	require.Equal(t, int64(30*kib), std, "the floor is full")
	return f
}

func TestNetReservation_FullFloor_SmallerOverwrite200_LargerOverwrite403(t *testing.T) {
	// Before: the 1 KiB overwrite was 403 (10 KiB reserved on a full floor
	// before the 10 KiB it replaces was released).
	f := fullFloor(t)

	assert.Equal(t, http.StatusOK, f.put(t, "k", testBytes(1*kib)), "a smaller overwrite at 100 % frees space")
	f.ledgerAgrees(t, "after the smaller overwrite")
	std, _ := f.floors(t)
	assert.Equal(t, int64(21*kib), std)

	require.Equal(t, http.StatusOK, f.put(t, "fill2", testBytes(9*kib)), "back to 100 %")
	assert.Equal(t, http.StatusForbidden, f.put(t, "k", testBytes(20*kib)), "a larger overwrite still needs the difference")
	f.ledgerAgrees(t, "after the refused overwrite")
	std, _ = f.floors(t)
	assert.Equal(t, int64(30*kib), std)
	assert.Equal(t, int64(1*kib), f.headSize(t, "k"), "the refused write left the object as it was")
}

func TestNetReservation_EqualSizeOverwriteAtFullFloor_200(t *testing.T) {
	// A backup tool rewriting a manifest of the same size in place.
	f := fullFloor(t)
	assert.Equal(t, http.StatusOK, f.put(t, "k", testBytes(10*kib)))
	f.ledgerAgrees(t, "after the rewrite")
}

func TestNetReservation_CopyOntoItselfAtFullFloor_200(t *testing.T) {
	// Restoring an object's attributes / a version by copying it onto its
	// own key at 100 %: a wash, never a refusal.
	f := fullFloor(t)
	w := f.copyObject(t, "k", "k")
	assert.Equal(t, http.StatusOK, w.Code, w.Body.String())
	f.ledgerAgrees(t, "after the self-copy")
}

// raceBody runs act on its first Read: the write has reserved by then and
// has not committed.
type raceBody struct {
	io.Reader
	act  func()
	done bool
}

func (b *raceBody) Read(p []byte) (int, error) {
	if !b.done {
		b.done = true
		b.act()
	}
	return b.Reader.Read(p)
}

func TestNetReservation_TheCreditGoesAway_CommitRefusesNothingCommitted(t *testing.T) {
	// The PUT of 1 KiB over k (10 KiB) at 100 % is credited 10 KiB. While its
	// body streams, k is deleted (the credit's bytes go) and another 10 KiB
	// is written: the floor is full again, and the commit — releasing
	// nothing — would end at 31 KiB of 30. Refused: 403, nothing committed,
	// the new blob queued as an orphan.
	f := fullFloor(t)
	body := &raceBody{Reader: bytes.NewReader(testBytes(1 * kib)), act: func() {
		require.Equal(t, http.StatusNoContent, f.del(t, "k"))
		require.Equal(t, http.StatusOK, f.put(t, "other", testBytes(10*kib)))
	}}
	req := httptest.NewRequest("PUT", "/test-bucket/k", body)
	req.ContentLength = 1 * kib
	req = req.WithContext(f.ctx(req.Context()))
	w := httptest.NewRecorder()

	f.server.handlePutObject(w, req, f.s3Req("test-bucket", "k"))

	assert.Equal(t, http.StatusForbidden, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), "QuotaExceeded")
	assert.Equal(t, 0, f.headRows(t, "k"), "nothing committed")
	f.ledgerAgrees(t, "after the refused commit")
	std, _ := f.floors(t)
	assert.Equal(t, int64(30*kib), std)
	var orphans int
	require.NoError(t, f.db.QueryRow(`SELECT COUNT(*) FROM retired_blobs WHERE tenant_id = $1 AND reason = $2`,
		f.tenantID, retiredOrphanWrite).Scan(&orphans))
	assert.Equal(t, 1, orphans, "the new blob is queued for the collector")
}

func TestNetReservation_TheCreditGoesAwayButItStillFits_Committed(t *testing.T) {
	// The same race with room left: the correction is a charge, not a
	// refusal.
	f := fullFloor(t)
	body := &raceBody{Reader: bytes.NewReader(testBytes(1 * kib)), act: func() {
		require.Equal(t, http.StatusNoContent, f.del(t, "k"))
	}}
	req := httptest.NewRequest("PUT", "/test-bucket/k", body)
	req.ContentLength = 1 * kib
	req = req.WithContext(f.ctx(req.Context()))
	w := httptest.NewRecorder()
	f.server.handlePutObject(w, req, f.s3Req("test-bucket", "k"))

	assert.Equal(t, http.StatusOK, w.Code, w.Body.String())
	f.ledgerAgrees(t, "after the corrected commit")
	std, _ := f.floors(t)
	assert.Equal(t, int64(21*kib), std)
}

func TestNetReservation_VersioningEnabled_TheOldBytesStay_NoCredit(t *testing.T) {
	// An Enabled overwrite keeps the old blob as a version: nothing is
	// released, so nothing is credited.
	f := setupQuotaAccountingFixture(t, 1<<30)
	f.versioning(t, "Enabled")
	f.house(t, 30*kib, 0)
	require.Equal(t, http.StatusOK, f.put(t, "k", testBytes(10*kib)))
	require.Equal(t, http.StatusOK, f.put(t, "fill", testBytes(20*kib)))

	assert.Equal(t, http.StatusForbidden, f.put(t, "k", testBytes(1*kib)))
	f.ledgerAgrees(t, "after the refusal")
}

func TestNetReservation_FloorChange_IsAFullReservationOnTheNewFloor(t *testing.T) {
	// k sits in the attic; overwriting it downstairs frees the attic, not
	// the full downstairs.
	f := setupQuotaAccountingFixture(t, 1<<30)
	f.house(t, 20*kib, 20*kib)
	require.Equal(t, http.StatusOK, f.putClass(t, "test-bucket", "k", "GLACIER", testBytes(10*kib)))
	require.Equal(t, http.StatusOK, f.put(t, "fill", testBytes(20*kib)))

	assert.Equal(t, http.StatusForbidden, f.put(t, "k", testBytes(1*kib)))
	f.ledgerAgrees(t, "after the refusal")
	_, vault := f.floors(t)
	assert.Equal(t, int64(10*kib), vault, "the attic object is untouched")
}

func (f *quotaAccountingFixture) headSize(t *testing.T, key string) int64 {
	t.Helper()
	var n int64
	require.NoError(t, f.db.QueryRow(`SELECT size_bytes FROM object_head_cache WHERE tenant_id = $1 AND bucket = 'test-bucket' AND object_key = $2`,
		f.tenantID, key).Scan(&n))
	return n
}
