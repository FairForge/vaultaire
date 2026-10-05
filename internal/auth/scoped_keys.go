package auth

import (
	"context"
	"errors"
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

// The typed refusals of a key's scope at creation (WP-R5-12). Every entry
// point — dashboard form, user API, management API — gets them from
// GenerateAPIKey, so a junk allowlist entry or an expiry in the past is
// refused the same way everywhere instead of being stored (R5-23, R12-32: a
// typo in the allowlist never matched and the key was simply unusable; an
// expiry in the past made a key that was dead on arrival).
var (
	ErrInvalidPermission  = errors.New("unknown permission")
	ErrInvalidIPAllowlist = errors.New("invalid ip allowlist entry")
	// ErrUnrestrictedIPAllowlist: an entry such as 0.0.0.0/0 or ::/0 that
	// admits every address. An allowlist that restricts nothing is a
	// mistake, not a policy — the way to allow every address is no
	// allowlist (post-merge review of #584).
	ErrUnrestrictedIPAllowlist = errors.New("ip allowlist entry restricts nothing")
	ErrExpiryInPast            = errors.New("expiry must be in the future")
	// ErrKeyExpired: the key exists, is not revoked, and its expires_at has
	// passed. S3 answers ExpiredToken; the JSON APIs a typed 401.
	ErrKeyExpired = errors.New("API key expired")
)

// Validate checks the options and canonicalises the IP allowlist in place.
// now is the clock the expiry is checked against.
func (o *KeyCreateOptions) Validate(now time.Time) error {
	if o == nil {
		return nil
	}
	if err := ValidatePermissions(o.Permissions); err != nil {
		return err
	}
	canon, err := ValidateIPAllowlist(o.IPAllowlist)
	if err != nil {
		return err
	}
	o.IPAllowlist = canon
	if o.ExpiresAt != nil && !o.ExpiresAt.After(now) {
		return fmt.Errorf("%w: %s is not after %s", ErrExpiryInPast,
			o.ExpiresAt.UTC().Format(time.RFC3339), now.UTC().Format(time.RFC3339))
	}
	return nil
}

// ValidateIPAllowlist checks that every entry is an IP address or a CIDR
// network and returns the list in canonical form (net.IP.String /
// net.IPNet.String — the form clientip hands CheckIPAllowlist, so
// `::FFFF:1.2.3.4` and `2001:DB8::1` match the way they are written). A
// host bit set inside a CIDR is normalised to the network address. Junk is
// ErrInvalidIPAllowlist naming the entry.
func ValidateIPAllowlist(entries []string) ([]string, error) {
	if len(entries) == 0 {
		return entries, nil
	}
	out := make([]string, 0, len(entries))
	for _, raw := range entries {
		entry := strings.TrimSpace(raw)
		if entry == "" {
			return nil, fmt.Errorf("%w: empty entry", ErrInvalidIPAllowlist)
		}
		if strings.Contains(entry, "/") {
			_, cidr, err := net.ParseCIDR(entry)
			if err != nil {
				return nil, fmt.Errorf("%w: %q is not a CIDR network", ErrInvalidIPAllowlist, entry)
			}
			if ones, _ := cidr.Mask.Size(); ones == 0 {
				return nil, fmt.Errorf("%w: %q allows every address; leave the allowlist empty instead", ErrUnrestrictedIPAllowlist, entry)
			}
			out = append(out, cidr.String())
			continue
		}
		ip := net.ParseIP(entry)
		if ip == nil {
			return nil, fmt.Errorf("%w: %q is not an IP address or a CIDR network", ErrInvalidIPAllowlist, entry)
		}
		out = append(out, ip.String())
	}
	return out, nil
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
	parsed := net.ParseIP(strings.TrimSpace(clientIP))
	if parsed == nil {
		return false
	}
	for _, entry := range allowlist {
		entry = strings.TrimSpace(entry)
		if strings.Contains(entry, "/") {
			_, cidr, err := net.ParseCIDR(entry)
			if err != nil {
				continue // refused at creation since WP-R5-12; a legacy row's junk grants nothing
			}
			if cidr.Contains(parsed) {
				return true
			}
			continue
		}
		// Both sides parsed: `::FFFF:1.2.3.4` matches `1.2.3.4`, `2001:DB8::1`
		// matches `2001:db8::1` (R5-23 — the string compare never did).
		if ip := net.ParseIP(entry); ip != nil && ip.Equal(parsed) {
			return true
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
			return fmt.Errorf("%w: %q", ErrInvalidPermission, p)
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
