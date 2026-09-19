package api

// S3 ?acl sub-resource (A1 conformance fix, WP-VG1 gap report 2026-09-18).
//
// Vaultaire has no ACL model — every bucket/object is owned by exactly one
// tenant, equivalent to AWS's BucketOwnerEnforced ownership setting. Before
// this file existed, ?acl was not routed at all: GET bucket?acl fell through
// to ListObjects (SDKs parsed the listing as an empty ACL — versitygw's
// harness nil-panics on it) and PUT object?acl fell through to PutObject,
// OVERWRITING the object's bytes with the ACL XML body.
//
// The behavior implemented here mirrors AWS with BucketOwnerEnforced:
//   - GET returns a canned AccessControlPolicy: owner + one FULL_CONTROL grant.
//   - PUT accepts only owner-equivalent ACLs (absent/private/
//     bucket-owner-full-control canned headers, or a body whose every grant is
//     FULL_CONTROL); anything else is refused with 400
//     AccessControlListNotSupported ("The bucket does not allow ACLs") rather
//     than silently pretending a public-read grant took effect.

import (
	"encoding/xml"
	"io"
	"net/http"

	"github.com/FairForge/vaultaire/internal/tenant"
)

// AccessControlPolicy is the S3 GET ?acl response document.
type AccessControlPolicy struct {
	XMLName xml.Name          `xml:"AccessControlPolicy"`
	Xmlns   string            `xml:"xmlns,attr"`
	Owner   ACLOwner          `xml:"Owner"`
	Grants  AccessControlList `xml:"AccessControlList"`
}

// ACLOwner identifies the bucket/object owner.
type ACLOwner struct {
	ID          string `xml:"ID"`
	DisplayName string `xml:"DisplayName"`
}

// AccessControlList wraps the grant list.
type AccessControlList struct {
	Grants []ACLGrant `xml:"Grant"`
}

// ACLGrant is a single grantee/permission pair.
type ACLGrant struct {
	Grantee    ACLGrantee `xml:"Grantee"`
	Permission string     `xml:"Permission"`
}

// ACLGrantee carries the xsi:type attribute AWS SDKs expect on Grantee.
type ACLGrantee struct {
	XmlnsXsi    string `xml:"xmlns:xsi,attr"`
	Type        string `xml:"xsi:type,attr"`
	ID          string `xml:"ID"`
	DisplayName string `xml:"DisplayName"`
}

func ownerACLPolicy(ownerID string) AccessControlPolicy {
	return AccessControlPolicy{
		Xmlns: "http://s3.amazonaws.com/doc/2006-03-01/",
		Owner: ACLOwner{ID: ownerID, DisplayName: ownerID},
		Grants: AccessControlList{Grants: []ACLGrant{{
			Grantee: ACLGrantee{
				XmlnsXsi:    "http://www.w3.org/2001/XMLSchema-instance",
				Type:        "CanonicalUser",
				ID:          ownerID,
				DisplayName: ownerID,
			},
			Permission: "FULL_CONTROL",
		}}},
	}
}

// aclBucketExists reports whether the tenant owns the bucket. A nil DB (dev
// mode) fails open — the S3 layer has no other bucket registry to consult.
func (s *Server) aclBucketExists(r *http.Request, tenantID, bucket string) bool {
	if s.db == nil {
		return true
	}
	var exists bool
	_ = s.db.QueryRowContext(r.Context(),
		`SELECT EXISTS(SELECT 1 FROM buckets WHERE tenant_id = $1 AND name = $2)`,
		tenantID, bucket).Scan(&exists)
	return exists
}

func (s *Server) aclObjectExists(r *http.Request, tenantID, bucket, key string) bool {
	if s.db == nil {
		return true
	}
	var exists bool
	_ = s.db.QueryRowContext(r.Context(),
		`SELECT EXISTS(SELECT 1 FROM object_head_cache WHERE tenant_id = $1 AND bucket = $2 AND object_key = $3)`,
		tenantID, bucket, key).Scan(&exists)
	return exists
}

func writeACLPolicy(w http.ResponseWriter, ownerID string) {
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(http.StatusOK)
	_ = xml.NewEncoder(w).Encode(ownerACLPolicy(ownerID))
}

func (s *Server) handleGetBucketAcl(w http.ResponseWriter, r *http.Request, req *S3Request) {
	t, err := tenant.FromContext(r.Context())
	if err != nil || t == nil {
		WriteS3Error(w, ErrAccessDenied, r.URL.Path, generateRequestID())
		return
	}
	if !s.aclBucketExists(r, t.ID, req.Bucket) {
		WriteS3Error(w, ErrNoSuchBucket, r.URL.Path, generateRequestID())
		return
	}
	writeACLPolicy(w, t.ID)
}

func (s *Server) handleGetObjectAcl(w http.ResponseWriter, r *http.Request, req *S3Request) {
	t, err := tenant.FromContext(r.Context())
	if err != nil || t == nil {
		WriteS3Error(w, ErrAccessDenied, r.URL.Path, generateRequestID())
		return
	}
	if !s.aclBucketExists(r, t.ID, req.Bucket) {
		WriteS3Error(w, ErrNoSuchBucket, r.URL.Path, generateRequestID())
		return
	}
	if !s.aclObjectExists(r, t.ID, req.Bucket, req.Object) {
		WriteS3Error(w, ErrNoSuchKey, r.URL.Path, generateRequestID())
		return
	}
	writeACLPolicy(w, t.ID)
}

// aclWriteAllowed decides whether a PUT ?acl request is owner-equivalent.
// Canned headers other than private/bucket-owner-full-control, and body
// grants beyond FULL_CONTROL, are refused — granting them is unsupported.
func aclWriteAllowed(r *http.Request) bool {
	switch r.Header.Get("x-amz-acl") {
	case "", "private", "bucket-owner-full-control":
	default:
		return false
	}
	// Any explicit grant header is a non-owner grant request.
	for _, h := range []string{
		"x-amz-grant-read", "x-amz-grant-write", "x-amz-grant-read-acp",
		"x-amz-grant-write-acp", "x-amz-grant-full-control",
	} {
		if r.Header.Get(h) != "" {
			return false
		}
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 64*1024))
	if err != nil || len(body) == 0 {
		return err == nil
	}
	var policy struct {
		Grants []struct {
			Permission string `xml:"Permission"`
		} `xml:"AccessControlList>Grant"`
	}
	if xml.Unmarshal(body, &policy) != nil {
		return false
	}
	for _, g := range policy.Grants {
		if g.Permission != "FULL_CONTROL" {
			return false
		}
	}
	return true
}

func (s *Server) handlePutBucketAcl(w http.ResponseWriter, r *http.Request, req *S3Request) {
	t, err := tenant.FromContext(r.Context())
	if err != nil || t == nil {
		WriteS3Error(w, ErrAccessDenied, r.URL.Path, generateRequestID())
		return
	}
	if !s.aclBucketExists(r, t.ID, req.Bucket) {
		WriteS3Error(w, ErrNoSuchBucket, r.URL.Path, generateRequestID())
		return
	}
	if !aclWriteAllowed(r) {
		WriteS3Error(w, ErrAccessControlListNotSupported, r.URL.Path, generateRequestID())
		return
	}
	w.WriteHeader(http.StatusOK)
}

func (s *Server) handlePutObjectAcl(w http.ResponseWriter, r *http.Request, req *S3Request) {
	t, err := tenant.FromContext(r.Context())
	if err != nil || t == nil {
		WriteS3Error(w, ErrAccessDenied, r.URL.Path, generateRequestID())
		return
	}
	if !s.aclBucketExists(r, t.ID, req.Bucket) {
		WriteS3Error(w, ErrNoSuchBucket, r.URL.Path, generateRequestID())
		return
	}
	if !s.aclObjectExists(r, t.ID, req.Bucket, req.Object) {
		WriteS3Error(w, ErrNoSuchKey, r.URL.Path, generateRequestID())
		return
	}
	if !aclWriteAllowed(r) {
		WriteS3Error(w, ErrAccessControlListNotSupported, r.URL.Path, generateRequestID())
		return
	}
	w.WriteHeader(http.StatusOK)
}
