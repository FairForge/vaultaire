package auth

import (
	"context"
	"fmt"
	"net"
	"strings"
	"time"
)

// KeyScope carries the permission constraints for a single API key.
// Returned alongside the tenant ID from auth lookups so that callers
// can enforce scope without a second query.
type KeyScope struct {
	Permissions []string
	BucketScope []string
	IPAllowlist []string
	ExpiresAt   *time.Time
	// Temporary marks an STS token. A token never holds a privilege by
	// wildcard: `*` on a token is "every operation", not "every privilege"
	// (see CanBypassGovernanceRetention).
	Temporary bool
}

// PermBypassGovernanceRetention is the one entry of ValidPermissions that is
// not an S3 operation: it is the privilege to have
// `x-amz-bypass-governance-retention: true` honoured — to delete, overwrite
// or shorten the retention of an object under GOVERNANCE retention before
// its date (AWS: s3:BypassGovernanceRetention). It grants no operation by
// itself: a key still needs DeleteObject to delete.
const PermBypassGovernanceRetention = "BypassGovernanceRetention"

// CanBypassGovernanceRetention reports whether the key may bypass GOVERNANCE
// retention (WP-R4-1). A full-access key (`*` — the tenant's primary key, a
// key created without a permission list) may; a scoped key only when it
// lists the permission; an STS token only when it lists it explicitly — a
// token minted without asking for the bypass does not get it through `*`.
// A nil scope (no authenticated key) may not. COMPLIANCE retention and legal
// holds are not affected by any of this: nothing bypasses them.
func (s *KeyScope) CanBypassGovernanceRetention() bool {
	if s == nil {
		return false
	}
	for _, p := range s.Permissions {
		if p == PermBypassGovernanceRetention || (p == "*" && !s.Temporary) {
			return true
		}
	}
	return false
}

// KeyCreateOptions specifies optional scope constraints when creating
// a new API key. Nil means full access (["*"]).
type KeyCreateOptions struct {
	Permissions []string
	BucketScope []string
	IPAllowlist []string
	ExpiresAt   *time.Time
}

// ValidPermissions is the set of names that may appear in an API key's
// permissions list. They match the operation strings produced by
// determineOperation in s3.go, plus one privilege that is not an operation
// (PermBypassGovernanceRetention).
var ValidPermissions = map[string]bool{
	"*":                           true,
	PermBypassGovernanceRetention: true,
	"GetObject":                   true,
	"PutObject":                   true,
	"DeleteObject":                true,
	"HeadObject":                  true,
	"ListObjects":                 true,
	"ListBuckets":                 true,
	"CreateBucket":                true,
	"DeleteBucket":                true,
	"HeadBucket":                  true,
	"DeleteObjects":               true,
	"InitiateMultipartUpload":     true,
	"UploadPart":                  true,
	"CompleteMultipartUpload":     true,
	"AbortMultipartUpload":        true,
	"ListMultipartUploads":        true,
	"ListParts":                   true,
	"GetBucketVersioning":         true,
	"PutBucketVersioning":         true,
	"GetBucketNotification":       true,
	"PutBucketNotification":       true,
	"GetObjectLockConfiguration":  true,
	"PutObjectLockConfiguration":  true,
	"PutObjectRetention":          true,
	"GetObjectRetention":          true,
	"PutObjectLegalHold":          true,
	"GetObjectLegalHold":          true,
	"PostObject":                  true,
	"RestoreObject":               true,
}

// CheckPermission returns true if keyPerms authorizes the given operation.
func CheckPermission(keyPerms []string, operation string) bool {
	for _, p := range keyPerms {
		if p == "*" || p == operation {
			return true
		}
	}
	return false
}

// CheckBucketScope returns true if the bucket is allowed by the scope.
// An empty scope list means unrestricted (all buckets allowed).
func CheckBucketScope(scopes []string, bucket string) bool {
	if len(scopes) == 0 {
		return true
	}
	for _, s := range scopes {
		if s == bucket {
			return true
		}
	}
	return false
}

// CheckIPAllowlist returns true if clientIP is permitted.
// An empty allowlist means unrestricted (all IPs allowed).
func CheckIPAllowlist(allowlist []string, clientIP string) bool {
	if len(allowlist) == 0 {
		return true
	}
	parsed := net.ParseIP(clientIP)
	for _, entry := range allowlist {
		if strings.Contains(entry, "/") {
			_, cidr, err := net.ParseCIDR(entry)
			if err != nil {
				continue
			}
			if parsed != nil && cidr.Contains(parsed) {
				return true
			}
		} else {
			if entry == clientIP {
				return true
			}
		}
	}
	return false
}

// IsKeyExpired returns true if the key has passed its expiration time.
func IsKeyExpired(expiresAt *time.Time) bool {
	if expiresAt == nil {
		return false
	}
	return time.Now().After(*expiresAt)
}

// ValidatePermissions checks that every entry in perms is a known
// operation name. Returns an error naming the first invalid entry.
func ValidatePermissions(perms []string) error {
	for _, p := range perms {
		if !ValidPermissions[p] {
			return fmt.Errorf("unknown permission: %q", p)
		}
	}
	return nil
}

type keyScopeCtxKey struct{}

// WithKeyScope returns ctx carrying the scope of the key that authenticated
// the request.
func WithKeyScope(ctx context.Context, scope *KeyScope) context.Context {
	return context.WithValue(ctx, keyScopeCtxKey{}, scope)
}

// KeyScopeFromContext returns the scope put there by WithKeyScope, or nil.
func KeyScopeFromContext(ctx context.Context) *KeyScope {
	s, _ := ctx.Value(keyScopeCtxKey{}).(*KeyScope)
	return s
}
