package api

import (
	"bytes"
	"context"
	"encoding/xml"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/FairForge/vaultaire/internal/usage"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Stage 1a Prompt 3, Part 0 (the post-merge review of WP-R2-1 C1/C2, #697
// and #698). Two P1s: an object written before versioning was enabled is
// listed as version "null" but was never one — an Enabled overwrite or a
// delete marker retired its bytes (the collector deleted them a day later);
// and DeleteObjects, versioning-unaware, dropped the head row of a versioned
// key with no marker: the latest version kept its bytes and its bill and
// neither GET nor GET ?versionId could reach it. Red on db0a259.

// unversionedBucket creates test-bucket with no versioning status.
func (f *quotaAccountingFixture) unversionedBucket(t *testing.T) {
	t.Helper()
	_, err := f.db.Exec(`INSERT INTO buckets (tenant_id, name, visibility) VALUES ($1, 'test-bucket', 'private')
		ON CONFLICT (tenant_id, name) DO NOTHING`, f.tenantID)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = f.db.Exec(`DELETE FROM object_versions WHERE tenant_id = $1`, f.tenantID)
		_, _ = f.db.Exec(`DELETE FROM retired_blobs WHERE tenant_id = $1`, f.tenantID)
		_, _ = f.db.Exec(`DELETE FROM buckets WHERE tenant_id = $1`, f.tenantID)
	})
}

func (f *quotaAccountingFixture) getV(t *testing.T, method, key, vid string) *httptest.ResponseRecorder {
	t.Helper()
	p := "/test-bucket/" + key
	if vid != "" {
		p += "?versionId=" + vid
	}
	return doS3Request(f.server, f.tenant, method, p, nil)
}

// reconciled asserts the incremental ledger equals the reconcile (total)
// and returns it.
func (f *quotaAccountingFixture) reconciled(t *testing.T, step string) int64 {
	t.Helper()
	before := f.used(t)
	require.NoError(t, f.qm.ReconcileTenantStorageUsage(context.Background(), f.tenantID))
	require.Equal(t, f.used(t), before, "%s: the reconcile disagrees with the incremental ledger", step)
	return before
}

type batchEntry struct{ key, versionID string }

func (f *quotaAccountingFixture) batchDelete(t *testing.T, entries ...batchEntry) DeleteResult {
	t.Helper()
	var b strings.Builder
	b.WriteString("<Delete>")
	for _, e := range entries {
		b.WriteString("<Object><Key>" + e.key + "</Key>")
		if e.versionID != "" {
			b.WriteString("<VersionId>" + e.versionID + "</VersionId>")
		}
		b.WriteString("</Object>")
	}
	b.WriteString("</Delete>")
	w := doS3Request(f.server, f.tenant, "POST", "/test-bucket?delete", bytes.NewReader([]byte(b.String())))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var res DeleteResult
	require.NoError(t, xml.Unmarshal(w.Body.Bytes(), &res), w.Body.String())
	return res
}

func TestNullVersion_PreVersioningObject_KeptByAnOverwrite(t *testing.T) {
	// Arrange: an object written while the bucket was unversioned, then
	// versioning enabled. Before: GET ?versionId=null 404; the overwrite
	// retired its blob (gone after the grace) and released its bill.
	f := setupQuotaAccountingFixture(t, 1<<30)
	f.unversionedBucket(t)
	require.Equal(t, http.StatusOK, f.put(t, "k", testBytes(3000)))
	f.versioning(t, "Enabled")

	// Before any write: the null version IS the current object.
	w := f.getV(t, "GET", "k", "null")
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Equal(t, 3000, w.Body.Len())
	assert.Equal(t, "null", w.Header().Get("x-amz-version-id"))
	h := f.getV(t, "HEAD", "k", "null")
	require.Equal(t, http.StatusOK, h.Code)
	assert.Equal(t, "3000", h.Header().Get("Content-Length"))

	// Act
	code, v2 := f.putVersion(t, "k", 5000)
	require.Equal(t, http.StatusOK, code)

	// Assert: both versions served, both billed, nothing retired.
	w = f.getV(t, "GET", "k", "null")
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Equal(t, testBytes(3000), w.Body.Bytes())
	h = f.getV(t, "HEAD", "k", "null")
	require.Equal(t, http.StatusOK, h.Code)
	assert.Equal(t, "3000", h.Header().Get("Content-Length"))
	cur := f.getV(t, "GET", "k", "")
	require.Equal(t, http.StatusOK, cur.Code)
	assert.Equal(t, 5000, cur.Body.Len())
	assert.Equal(t, v2, cur.Header().Get("x-amz-version-id"))
	assert.Equal(t, int64(8000), f.reconciled(t, "versioned overwrite of a pre-versioning object"))
	var retired int
	require.NoError(t, f.db.QueryRow(`SELECT COUNT(*) FROM retired_blobs WHERE tenant_id = $1`, f.tenantID).Scan(&retired))
	assert.Zero(t, retired, "the null version's blob must not be retired")
	lv := doS3Request(f.server, f.tenant, "GET", "/test-bucket?versions", nil)
	assert.Contains(t, lv.Body.String(), "<VersionId>null</VersionId><IsLatest>false</IsLatest>")

	// DELETE ?versionId=null removes it and its bill.
	f.delVersion(t, "k", "null")
	assert.Equal(t, http.StatusNotFound, f.getV(t, "GET", "k", "null").Code)
	assert.Equal(t, int64(5000), f.reconciled(t, "the null version deleted"))
}

func TestNullVersion_PreVersioningObject_KeptBehindADeleteMarker(t *testing.T) {
	// Before: the marker released the bill and retired the blob.
	f := setupQuotaAccountingFixture(t, 1<<30)
	f.unversionedBucket(t)
	require.Equal(t, http.StatusOK, f.put(t, "k", testBytes(3000)))
	f.versioning(t, "Enabled")

	f.delVersion(t, "k", "")

	assert.Equal(t, http.StatusNotFound, f.getV(t, "GET", "k", "").Code)
	w := f.getV(t, "GET", "k", "null")
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Equal(t, testBytes(3000), w.Body.Bytes())
	assert.Equal(t, int64(3000), f.reconciled(t, "behind a marker"))
}

func TestNullVersion_DeleteVersionIdNull_OfAnUnanchoredObject(t *testing.T) {
	// A key with no version rows at all: ?versionId=null names the object
	// itself (AWS). Before: 404 NoSuchVersion, the object stayed.
	f := setupQuotaAccountingFixture(t, 1<<30)
	f.unversionedBucket(t)
	require.Equal(t, http.StatusOK, f.put(t, "k", testBytes(3000)))
	f.versioning(t, "Enabled")

	f.delVersion(t, "k", "null")

	assert.Equal(t, http.StatusNotFound, f.getV(t, "GET", "k", "").Code)
	assert.Equal(t, int64(0), f.reconciled(t, "the null version deleted"))
}

func TestDeleteObjects_EnabledBucket_WritesADeleteMarker(t *testing.T) {
	// Before: the head row went with no marker; the version kept its bytes
	// and its bill; GET ?versionId 404.
	f := setupQuotaAccountingFixture(t, 1<<30)
	f.versioning(t, "Enabled")
	code, v1 := f.putVersion(t, "k", 3000)
	require.Equal(t, http.StatusOK, code)

	res := f.batchDelete(t, batchEntry{key: "k"})

	require.Len(t, res.Deleted, 1)
	assert.True(t, res.Deleted[0].DeleteMarker)
	assert.NotEmpty(t, res.Deleted[0].DeleteMarkerVersionID)
	g := f.getV(t, "GET", "k", "")
	assert.Equal(t, http.StatusNotFound, g.Code)
	assert.Equal(t, "true", g.Header().Get("x-amz-delete-marker"))
	w := f.getV(t, "GET", "k", v1)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Equal(t, testBytes(3000), w.Body.Bytes())
	assert.Equal(t, int64(3000), f.reconciled(t, "behind the batch's marker"))
}

func TestDeleteObjects_VersionId_DeletesThatVersionOnly(t *testing.T) {
	// Before: the <VersionId> was ignored and the CURRENT object deleted.
	f := setupQuotaAccountingFixture(t, 1<<30)
	f.versioning(t, "Enabled")
	code, v1 := f.putVersion(t, "k", 3000)
	require.Equal(t, http.StatusOK, code)
	code, v2 := f.putVersion(t, "k", 5000)
	require.Equal(t, http.StatusOK, code)

	res := f.batchDelete(t, batchEntry{key: "k", versionID: v1}, batchEntry{key: "k", versionID: "no-such-version"})

	require.Len(t, res.Deleted, 1, "%+v", res)
	assert.Equal(t, v1, res.Deleted[0].VersionID)
	require.Len(t, res.Errors, 1)
	assert.Equal(t, ErrNoSuchVersion, res.Errors[0].Code)
	cur := f.getV(t, "GET", "k", "")
	require.Equal(t, http.StatusOK, cur.Code)
	assert.Equal(t, v2, cur.Header().Get("x-amz-version-id"))
	assert.Equal(t, http.StatusNotFound, f.getV(t, "GET", "k", v1).Code)
	assert.Equal(t, int64(5000), f.reconciled(t, "the old version deleted by the batch"))

	// The current version by id: the key has no current object left.
	res = f.batchDelete(t, batchEntry{key: "k", versionID: v2})
	require.Len(t, res.Deleted, 1)
	assert.Equal(t, http.StatusNotFound, f.getV(t, "GET", "k", "").Code)
	assert.Equal(t, int64(0), f.reconciled(t, "every version deleted by the batch"))
}

// Question 2 of the review: after every writer and deleter over a versioned
// key, the incremental ledger (total and floor) equals the reconcile.
func TestVersionBytes_EveryWriterAndDeleterAgreesWithTheReconcile(t *testing.T) {
	for _, status := range []string{"Enabled", "Suspended"} {
		t.Run(status, func(t *testing.T) {
			f := setupQuotaAccountingFixture(t, 1<<30)
			f.versioning(t, "Enabled")
			require.NoError(t, f.qm.SetHouse(context.Background(), f.tenantID, usage.House{StdBytes: 1 << 30}))
			agrees := func(step string) {
				t.Helper()
				total, floor := f.used(t), f.floorUsed(t, usage.FloorStandard)
				require.NoError(t, f.qm.ReconcileTenantStorageUsage(context.Background(), f.tenantID))
				assert.Equal(t, f.used(t), total, "%s: total", step)
				assert.Equal(t, f.floorUsed(t, usage.FloorStandard), floor, "%s: floor", step)
			}
			copyTo := func(src, dst string) {
				t.Helper()
				r := httptest.NewRequest("PUT", "/test-bucket/"+dst, nil)
				r.Header.Set("x-amz-copy-source", "/test-bucket/"+src)
				r = r.WithContext(f.ctx(r.Context()))
				w := httptest.NewRecorder()
				f.server.handleS3Request(w, r)
				require.Equal(t, http.StatusOK, w.Code, w.Body.String())
			}

			require.Equal(t, http.StatusOK, f.put(t, "src", testBytes(700)))
			code, v1 := f.putVersion(t, "k", 3000)
			require.Equal(t, http.StatusOK, code)
			agrees("put")
			f.versioning(t, status)
			code, _ = f.putVersion(t, "k", 5000)
			require.Equal(t, http.StatusOK, code)
			agrees("put over")
			copyTo("src", "k")
			agrees("copy over")
			uid, body := uploadTwoParts(t, f.server, f.tenant, "k")
			for _, step := range []string{"complete over", "complete replay"} {
				w := doS3Request(f.server, f.tenant, "POST", "/test-bucket/k?uploadId="+uid, strings.NewReader(body))
				require.Equal(t, http.StatusOK, w.Code, w.Body.String())
				agrees(step)
			}
			f.batchDelete(t, batchEntry{key: "k"})
			agrees("DeleteObjects")
			code, _ = f.putVersion(t, "k", 4000)
			require.Equal(t, http.StatusOK, code)
			agrees("put after the batch")
			f.delVersion(t, "k", "")
			agrees("plain DELETE")
			code, _ = f.putVersion(t, "k", 1500)
			require.Equal(t, http.StatusOK, code)
			f.delVersion(t, "k", v1)
			agrees("DELETE ?versionId (old)")
			rows, err := f.db.Query(`SELECT version_id FROM object_versions WHERE tenant_id = $1 AND object_key = 'k'
				ORDER BY is_latest DESC`, f.tenantID)
			require.NoError(t, err)
			var vids []string
			for rows.Next() {
				var v string
				require.NoError(t, rows.Scan(&v))
				vids = append(vids, v)
			}
			require.NoError(t, rows.Err())
			require.NoError(t, rows.Close())
			for i, v := range vids {
				if i%2 == 0 {
					f.delVersion(t, "k", v)
				} else {
					f.batchDelete(t, batchEntry{key: "k", versionID: v})
				}
				agrees(fmt.Sprintf("version %d deleted", i))
			}
			assert.Equal(t, int64(700), f.used(t), "only src is left")
		})
	}
}
