package api

import (
	"encoding/xml"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/FairForge/vaultaire/internal/tenant"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// listVersions drives the ListObjectVersions handler the way the dispatch
// switch does, with the tenant on the context and the parsed query map.
func (f *versioningFixture) listVersions(t *testing.T, rawQuery string) (*httptest.ResponseRecorder, *ListVersionsResult) {
	t.Helper()
	target := "/" + f.bucket + "?versions"
	if rawQuery != "" {
		target += "&" + rawQuery
	}
	req := httptest.NewRequest("GET", target, nil)
	req = req.WithContext(tenant.WithTenant(req.Context(), f.tenant))

	q := map[string]string{}
	parsed, err := url.ParseQuery(req.URL.RawQuery)
	require.NoError(t, err)
	for k, v := range parsed {
		if len(v) > 0 {
			q[k] = v[0]
		} else {
			q[k] = ""
		}
	}

	w := httptest.NewRecorder()
	f.server.handleListObjectVersions(w, req, &S3Request{
		Bucket:   f.bucket,
		Query:    q,
		TenantID: f.tenantID,
	})

	var result ListVersionsResult
	if w.Code == http.StatusOK {
		require.NoError(t, xml.Unmarshal(w.Body.Bytes(), &result))
	}
	return w, &result
}

func TestParseRequest_VersionsSubresource(t *testing.T) {
	p := NewS3Parser(zap.NewNop())
	req := httptest.NewRequest("GET", "/mybucket?versions", nil)
	s3req, err := p.ParseRequest(req)
	require.NoError(t, err)
	assert.Equal(t, "ListObjectVersions", s3req.Operation)
}

func TestListObjectVersions_VersionedBucket(t *testing.T) {
	f := setupVersioningFixture(t)
	f.setVersioning(t, "Enabled")

	// Two versions of a.txt, then a delete marker; one version of b.txt.
	f.putObject(t, "a.txt", "version one")
	f.putObject(t, "a.txt", "version two")
	f.deleteObject(t, "a.txt")
	f.putObject(t, "b.txt", "only version")

	w, result := f.listVersions(t, "")
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	assert.Equal(t, f.bucket, result.Name)
	assert.False(t, result.IsTruncated)
	require.Len(t, result.Versions, 3, "two a.txt versions + one b.txt version")
	require.Len(t, result.DeleteMarkers, 1)

	// Keys ascend; within a key, newest first.
	assert.Equal(t, "a.txt", result.Versions[0].Key)
	assert.Equal(t, "a.txt", result.Versions[1].Key)
	assert.Equal(t, "b.txt", result.Versions[2].Key)

	// The delete marker is the latest state of a.txt, so no a.txt version is latest.
	assert.Equal(t, "a.txt", result.DeleteMarkers[0].Key)
	assert.True(t, result.DeleteMarkers[0].IsLatest)
	assert.False(t, result.Versions[0].IsLatest)
	assert.False(t, result.Versions[1].IsLatest)
	assert.True(t, result.Versions[2].IsLatest)

	for _, v := range result.Versions {
		assert.NotEmpty(t, v.VersionId)
		assert.NotEmpty(t, v.ETag)
		assert.NotEmpty(t, v.LastModified)
	}
}

func TestListObjectVersions_PrefixFilter(t *testing.T) {
	f := setupVersioningFixture(t)
	f.setVersioning(t, "Enabled")

	f.putObject(t, "logs/2026/one.txt", "x")
	f.putObject(t, "logs/2026/two.txt", "y")
	f.putObject(t, "data/other.txt", "z")

	w, result := f.listVersions(t, "prefix="+url.QueryEscape("logs/"))
	require.Equal(t, http.StatusOK, w.Code)

	require.Len(t, result.Versions, 2)
	assert.Equal(t, "logs/", result.Prefix)
	for _, v := range result.Versions {
		assert.Contains(t, v.Key, "logs/")
	}
}

func TestListObjectVersions_Pagination(t *testing.T) {
	f := setupVersioningFixture(t)
	f.setVersioning(t, "Enabled")

	f.putObject(t, "p1.txt", "a")
	f.putObject(t, "p2.txt", "b")
	f.putObject(t, "p3.txt", "c")

	w, page1 := f.listVersions(t, "max-keys=2")
	require.Equal(t, http.StatusOK, w.Code)
	require.Len(t, page1.Versions, 2)
	require.True(t, page1.IsTruncated)
	require.NotEmpty(t, page1.NextKeyMarker)
	require.NotEmpty(t, page1.NextVersionIdMarker)

	w, page2 := f.listVersions(t, "max-keys=2&key-marker="+url.QueryEscape(page1.NextKeyMarker)+
		"&version-id-marker="+url.QueryEscape(page1.NextVersionIdMarker))
	require.Equal(t, http.StatusOK, w.Code)
	require.Len(t, page2.Versions, 1)
	assert.False(t, page2.IsTruncated)
	assert.Equal(t, "p3.txt", page2.Versions[0].Key)
}

func TestListObjectVersions_UnversionedBucket_NullVersions(t *testing.T) {
	f := setupVersioningFixture(t)
	// versioning stays disabled — objects exist only in the head cache

	f.putObject(t, "plain.txt", "no versioning here")

	w, result := f.listVersions(t, "")
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	require.Len(t, result.Versions, 1)
	assert.Equal(t, "plain.txt", result.Versions[0].Key)
	assert.Equal(t, "null", result.Versions[0].VersionId)
	assert.True(t, result.Versions[0].IsLatest)
	assert.Empty(t, result.DeleteMarkers)
}

func TestListObjectVersions_MissingBucket(t *testing.T) {
	f := setupVersioningFixture(t)

	req := httptest.NewRequest("GET", "/nope-no-bucket?versions", nil)
	req = req.WithContext(tenant.WithTenant(req.Context(), f.tenant))
	w := httptest.NewRecorder()
	f.server.handleListObjectVersions(w, req, &S3Request{
		Bucket:   "nope-no-bucket",
		Query:    map[string]string{"versions": ""},
		TenantID: f.tenantID,
	})

	assert.Equal(t, http.StatusNotFound, w.Code)
	assert.Contains(t, w.Body.String(), "NoSuchBucket")
}
