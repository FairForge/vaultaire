package auth

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCheckPermission_Wildcard(t *testing.T) {
	assert.True(t, CheckPermission([]string{"*"}, "GetObject"))
	assert.True(t, CheckPermission([]string{"*"}, "DeleteBucket"))
	assert.True(t, CheckPermission([]string{"*"}, "PutObject"))
}

func TestCheckPermission_Specific(t *testing.T) {
	perms := []string{"GetObject", "PutObject"}
	assert.True(t, CheckPermission(perms, "GetObject"))
	assert.True(t, CheckPermission(perms, "PutObject"))
	assert.False(t, CheckPermission(perms, "DeleteObject"))
	assert.False(t, CheckPermission(perms, "ListBuckets"))
}

func TestCheckPermission_Empty(t *testing.T) {
	assert.False(t, CheckPermission([]string{}, "GetObject"))
	assert.False(t, CheckPermission(nil, "PutObject"))
}

func TestCheckBucketScope_Unrestricted(t *testing.T) {
	assert.True(t, CheckBucketScope([]string{}, "anything"))
	assert.True(t, CheckBucketScope(nil, "any-bucket"))
}

func TestCheckBucketScope_Restricted(t *testing.T) {
	scopes := []string{"uploads"}
	assert.True(t, CheckBucketScope(scopes, "uploads"))
	assert.False(t, CheckBucketScope(scopes, "private"))
	assert.False(t, CheckBucketScope(scopes, "uploads2"))
}

func TestCheckIPAllowlist_Unrestricted(t *testing.T) {
	assert.True(t, CheckIPAllowlist([]string{}, "1.2.3.4"))
	assert.True(t, CheckIPAllowlist(nil, "10.0.0.1"))
}

func TestCheckIPAllowlist_CIDR(t *testing.T) {
	allowlist := []string{"10.0.0.0/8"}
	assert.True(t, CheckIPAllowlist(allowlist, "10.1.2.3"))
	assert.True(t, CheckIPAllowlist(allowlist, "10.255.255.255"))
	assert.False(t, CheckIPAllowlist(allowlist, "192.168.1.1"))
	assert.False(t, CheckIPAllowlist(allowlist, "11.0.0.1"))
}

func TestCheckIPAllowlist_ExactIP(t *testing.T) {
	allowlist := []string{"1.2.3.4"}
	assert.True(t, CheckIPAllowlist(allowlist, "1.2.3.4"))
	assert.False(t, CheckIPAllowlist(allowlist, "1.2.3.5"))
	assert.False(t, CheckIPAllowlist(allowlist, "5.6.7.8"))
}

func TestCheckIPAllowlist_Mixed(t *testing.T) {
	allowlist := []string{"10.0.0.0/8", "192.168.1.100", "172.16.0.0/12"}
	assert.True(t, CheckIPAllowlist(allowlist, "10.50.0.1"))
	assert.True(t, CheckIPAllowlist(allowlist, "192.168.1.100"))
	assert.True(t, CheckIPAllowlist(allowlist, "172.20.0.5"))
	assert.False(t, CheckIPAllowlist(allowlist, "192.168.1.101"))
	assert.False(t, CheckIPAllowlist(allowlist, "8.8.8.8"))
}

func TestIsKeyExpired_Valid(t *testing.T) {
	future := time.Now().Add(24 * time.Hour)
	assert.False(t, IsKeyExpired(&future))
}

func TestIsKeyExpired_Expired(t *testing.T) {
	past := time.Now().Add(-1 * time.Hour)
	assert.True(t, IsKeyExpired(&past))
}

func TestIsKeyExpired_Nil(t *testing.T) {
	assert.False(t, IsKeyExpired(nil))
}

func TestValidatePermissions_Valid(t *testing.T) {
	require.NoError(t, ValidatePermissions([]string{"*"}))
	require.NoError(t, ValidatePermissions([]string{"GetObject", "PutObject"}))
	require.NoError(t, ValidatePermissions([]string{"ListBuckets", "HeadObject", "DeleteObjects"}))
}

func TestValidatePermissions_Invalid(t *testing.T) {
	err := ValidatePermissions([]string{"GetObject", "InvalidOp"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "InvalidOp")
}

func TestValidatePermissions_Empty(t *testing.T) {
	require.NoError(t, ValidatePermissions([]string{}))
	require.NoError(t, ValidatePermissions(nil))
}
