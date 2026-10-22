package api

import (
	"encoding/xml"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// RP33-5 (Stage 1a Prompt 27): GET ?acl tells the truth about a Public
// bucket — AWS's canned public-read shape: the owner's FULL_CONTROL grant
// plus READ for the AllUsers group (AWS S3 User Guide, "Access control list
// (ACL) overview" § Canned ACL: public-read = "Owner gets FULL_CONTROL. The
// AllUsers group gets READ access"; API_GetBucketAcl: a Group grantee carries
// xsi:type="Group" and its URI). Other buckets keep the owner-only answer;
// PUT ?acl still refuses a non-owner grant.

type aclDoc struct {
	Grants []struct {
		Grantee struct {
			Type string `xml:"http://www.w3.org/2001/XMLSchema-instance type,attr"`
			ID   string `xml:"ID"`
			URI  string `xml:"URI"`
		} `xml:"Grantee"`
		Permission string `xml:"Permission"`
	} `xml:"AccessControlList>Grant"`
}

func TestGetBucketAcl_PublicBucket_SaysAllUsersRead(t *testing.T) {
	f := setupA1Fixture(t)
	_, err := f.db.Exec(`UPDATE buckets SET visibility = 'public-read' WHERE tenant_id = $1 AND name = $2`, f.tenantID, f.bucket)
	require.NoError(t, err)

	for _, c := range []struct {
		name string
		call func(w http.ResponseWriter, r *http.Request)
		path string
	}{
		{"bucket", func(w http.ResponseWriter, r *http.Request) {
			f.server.handleGetBucketAcl(w, r, &S3Request{Bucket: f.bucket, TenantID: f.tenantID})
		}, "/" + f.bucket + "?acl"},
		{"object in it", func(w http.ResponseWriter, r *http.Request) {
			f.server.handleGetObjectAcl(w, r, &S3Request{Bucket: f.bucket, Object: f.object, TenantID: f.tenantID})
		}, "/" + f.bucket + "/" + f.object + "?acl"},
	} {
		t.Run(c.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			c.call(w, httptest.NewRequest("GET", c.path, nil).WithContext(f.ctx()))

			require.Equal(t, http.StatusOK, w.Code)
			var doc aclDoc
			require.NoError(t, xml.Unmarshal(w.Body.Bytes(), &doc))
			require.Len(t, doc.Grants, 2, w.Body.String())
			assert.Equal(t, "CanonicalUser", doc.Grants[0].Grantee.Type)
			assert.Equal(t, f.tenantID, doc.Grants[0].Grantee.ID)
			assert.Equal(t, "FULL_CONTROL", doc.Grants[0].Permission)
			assert.Equal(t, "Group", doc.Grants[1].Grantee.Type)
			assert.Equal(t, "http://acs.amazonaws.com/groups/global/AllUsers", doc.Grants[1].Grantee.URI)
			assert.Equal(t, "READ", doc.Grants[1].Permission)
			assert.NotContains(t, w.Body.String(), "<ID></ID>", "a group grantee has no ID")
		})
	}

	// Back to Standard: owner-only again.
	_, err = f.db.Exec(`UPDATE buckets SET visibility = 'private' WHERE tenant_id = $1 AND name = $2`, f.tenantID, f.bucket)
	require.NoError(t, err)
	w := httptest.NewRecorder()
	f.server.handleGetBucketAcl(w, httptest.NewRequest("GET", "/"+f.bucket+"?acl", nil).WithContext(f.ctx()), &S3Request{Bucket: f.bucket, TenantID: f.tenantID})
	var doc aclDoc
	require.NoError(t, xml.Unmarshal(w.Body.Bytes(), &doc))
	require.Len(t, doc.Grants, 1)
	assert.Equal(t, "FULL_CONTROL", doc.Grants[0].Permission)

	// PUT ?acl public-read stays refused (making a bucket public is the
	// dashboard's, F-32).
	r := httptest.NewRequest("PUT", "/"+f.bucket+"?acl", strings.NewReader("")).WithContext(f.ctx())
	r.Header.Set("x-amz-acl", "public-read")
	w = httptest.NewRecorder()
	f.server.handlePutBucketAcl(w, r, &S3Request{Bucket: f.bucket, TenantID: f.tenantID})
	assert.Equal(t, http.StatusBadRequest, w.Code)
}
