package auth

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// WP-R5-12 — key hygiene: the grants list is the parser's list, allowlist
// entries are validated and compared as addresses, an expiry in the past is
// refused on create, an expired key has its own error.

func TestValidPermissions_EveryParserOperationIsGrantable(t *testing.T) {
	for _, op := range S3Operations {
		assert.NoError(t, ValidatePermissions([]string{op}), op)
	}
	// R5-21's 18: a sample that the old literal list lacked.
	require.NoError(t, ValidatePermissions([]string{OpGetBucketAcl, OpPutObjectAcl, OpGetBucketLocation,
		OpListObjectVersions, OpGetBucketLogging, OpPutBucketInventory, OpGetObjectTagging, OpDeleteObjectTagging}))
	assert.Len(t, ValidPermissions, len(S3Operations)+2, "the operations, `*`, and the one privilege")

	err := ValidatePermissions([]string{"Unknown"})
	require.ErrorIs(t, err, ErrInvalidPermission, "the parser's sentinel is not grantable")
	assert.Contains(t, err.Error(), `"Unknown"`)
}

func TestValidateIPAllowlist(t *testing.T) {
	t.Run("addresses and networks are kept, canonicalised", func(t *testing.T) {
		got, err := ValidateIPAllowlist([]string{"1.2.3.4", " 10.0.0.0/8 ", "2001:DB8::1", "::FFFF:9.9.9.9", "192.168.1.77/24", "fe80::/10"})
		require.NoError(t, err)
		assert.Equal(t, []string{"1.2.3.4", "10.0.0.0/8", "2001:db8::1", "9.9.9.9", "192.168.1.0/24", "fe80::/10"}, got)
	})
	t.Run("junk is refused by name", func(t *testing.T) {
		for _, junk := range []string{"office", "1.2.3", "1.2.3.4/33", "10.0.0.0/", "/8", "1.2.3.4,5.6.7.8", "", " "} {
			_, err := ValidateIPAllowlist([]string{"1.2.3.4", junk})
			require.ErrorIs(t, err, ErrInvalidIPAllowlist, "%q", junk)
			if junk != "" && junk != " " {
				assert.Contains(t, err.Error(), junk)
			}
		}
	})
	t.Run("empty list is unrestricted", func(t *testing.T) {
		got, err := ValidateIPAllowlist(nil)
		require.NoError(t, err)
		assert.Nil(t, got)
	})
}

// R5-23: the entry and the client address are both parsed, so the written
// form does not matter and an IPv4 address mapped into IPv6 matches.
func TestCheckIPAllowlist_ComparesAddressesNotStrings(t *testing.T) {
	assert.True(t, CheckIPAllowlist([]string{"::FFFF:1.2.3.4"}, "1.2.3.4"))
	assert.True(t, CheckIPAllowlist([]string{"2001:DB8::1"}, "2001:db8::1"))
	assert.True(t, CheckIPAllowlist([]string{"2001:db8::1"}, "2001:0db8:0000:0000:0000:0000:0000:0001"))
	assert.True(t, CheckIPAllowlist([]string{"10.1.2.3/24"}, "10.1.2.200"), "a host bit in the entry still names the network")
	assert.False(t, CheckIPAllowlist([]string{"1.2.3.4"}, "not-an-ip"))
	assert.False(t, CheckIPAllowlist([]string{"junk", "1.2.3.0/99"}, "1.2.3.4"), "junk entries grant nothing")
	assert.False(t, CheckIPAllowlist([]string{"1.2.3.4"}, ""), "no client address never matches a restricted key")
}

func TestKeyCreateOptions_Validate(t *testing.T) {
	now := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
	t.Run("nil options are full access", func(t *testing.T) {
		var o *KeyCreateOptions
		require.NoError(t, o.Validate(now))
	})
	t.Run("expiry in the past is refused", func(t *testing.T) {
		past := now.Add(-time.Second)
		err := (&KeyCreateOptions{ExpiresAt: &past}).Validate(now)
		require.ErrorIs(t, err, ErrExpiryInPast)
		same := now
		require.ErrorIs(t, (&KeyCreateOptions{ExpiresAt: &same}).Validate(now), ErrExpiryInPast, "now is not in the future")
		future := now.Add(time.Minute)
		require.NoError(t, (&KeyCreateOptions{ExpiresAt: &future}).Validate(now))
	})
	t.Run("the allowlist is canonicalised in place", func(t *testing.T) {
		o := &KeyCreateOptions{IPAllowlist: []string{"2001:DB8::1", "10.1.2.3/24"}}
		require.NoError(t, o.Validate(now))
		assert.Equal(t, []string{"2001:db8::1", "10.1.2.0/24"}, o.IPAllowlist)
	})
	t.Run("an unknown permission is refused", func(t *testing.T) {
		require.ErrorIs(t, (&KeyCreateOptions{Permissions: []string{"GetObject", "Bogus"}}).Validate(now), ErrInvalidPermission)
	})
}

// GenerateAPIKey is where every entry point's scope is checked: the
// dashboard form, the user API and the management API cannot disagree.
func TestGenerateAPIKey_RefusesJunkScope(t *testing.T) {
	svc := NewAuthService(nil, nil)
	user, _, _, err := svc.CreateUserWithTenant(context.Background(), "hygiene@stored.ge", "a-long-password", "x")
	require.NoError(t, err)
	before, _ := svc.ListAPIKeys(context.Background(), user.ID)

	past := time.Now().Add(-time.Hour)
	cases := map[string]*KeyCreateOptions{
		"junk ip":          {IPAllowlist: []string{"office-lan"}},
		"bad cidr":         {IPAllowlist: []string{"10.0.0.0/40"}},
		"past expiry":      {ExpiresAt: &past},
		"unknown permname": {Permissions: []string{"Unknown"}},
	}
	wantErr := map[string]error{"junk ip": ErrInvalidIPAllowlist, "bad cidr": ErrInvalidIPAllowlist,
		"past expiry": ErrExpiryInPast, "unknown permname": ErrInvalidPermission}
	for name, opts := range cases {
		k, err := svc.GenerateAPIKey(context.Background(), user.ID, name, opts)
		require.ErrorIs(t, err, wantErr[name], name)
		assert.Nil(t, k)
	}
	after, _ := svc.ListAPIKeys(context.Background(), user.ID)
	assert.Len(t, after, len(before), "a refused key is not created")

	// A valid scope is stored canonical.
	future := time.Now().Add(24 * time.Hour)
	k, err := svc.GenerateAPIKey(context.Background(), user.ID, "ok", &KeyCreateOptions{
		Permissions: []string{OpGetObject, OpGetObjectTagging}, IPAllowlist: []string{"2001:DB8::/32", "::FFFF:1.2.3.4"}, ExpiresAt: &future})
	require.NoError(t, err)
	assert.Equal(t, []string{"2001:db8::/32", "1.2.3.4"}, k.IPAllowlist)
}

func TestGetOwnedAPIKey_ExpiredIsItsOwnError(t *testing.T) {
	svc := NewAuthService(nil, nil)
	user, _, _, err := svc.CreateUserWithTenant(context.Background(), "expired@stored.ge", "a-long-password", "x")
	require.NoError(t, err)
	soon := time.Now().Add(50 * time.Millisecond)
	k, err := svc.GenerateAPIKey(context.Background(), user.ID, "short", &KeyCreateOptions{ExpiresAt: &soon})
	require.NoError(t, err)

	got, err := svc.GetOwnedAPIKey(context.Background(), user.ID, k.ID)
	require.NoError(t, err)
	assert.Equal(t, k.ID, got.ID)

	time.Sleep(60 * time.Millisecond)
	_, err = svc.GetOwnedAPIKey(context.Background(), user.ID, k.ID)
	require.ErrorIs(t, err, ErrKeyExpired, "not ErrKeyRevoked, not a generic error")
	_, err = svc.ValidateAPIKey(context.Background(), k.Key, k.Secret)
	require.ErrorIs(t, err, ErrKeyExpired)
}
