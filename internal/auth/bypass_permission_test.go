package auth

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// WP-R4-1: the GOVERNANCE bypass is a privilege of the key.

func TestCanBypassGovernanceRetention(t *testing.T) {
	for _, tc := range []struct {
		name  string
		scope *KeyScope
		want  bool
	}{
		{"no authenticated key", nil, false},
		{"full access (primary key, key without a permission list)", &KeyScope{Permissions: []string{"*"}}, true},
		{"scoped key that can delete", &KeyScope{Permissions: []string{"DeleteObject", "PutObject"}}, false},
		{"scoped key with the permission", &KeyScope{Permissions: []string{"DeleteObject", PermBypassGovernanceRetention}}, true},
		{"no permissions at all (unparsable list)", &KeyScope{}, false},
		{"STS token with *", &KeyScope{Permissions: []string{"*"}, Temporary: true}, false},
		{"STS token that names it", &KeyScope{Permissions: []string{"*", PermBypassGovernanceRetention}, Temporary: true}, true},
	} {
		assert.Equal(t, tc.want, tc.scope.CanBypassGovernanceRetention(), tc.name)
	}
}

func TestBypassPermission_IsValidAndGrantsNoOperation(t *testing.T) {
	require.NoError(t, ValidatePermissions([]string{PermBypassGovernanceRetention}))
	// It is a privilege, not an operation: alone it authorizes nothing.
	for op := range ValidPermissions {
		if op == "*" || op == PermBypassGovernanceRetention {
			continue
		}
		assert.False(t, CheckPermission([]string{PermBypassGovernanceRetention}, op), op)
	}
}

func TestGenerateSTSToken_NeverInheritsTheBypass(t *testing.T) {
	mint := func(parent, requested []string) ([]string, error) {
		tok, err := GenerateSTSToken(context.Background(), nil, "t", "p", &KeyScope{Permissions: parent}, STSRequest{Permissions: requested})
		if err != nil {
			return nil, err
		}
		return tok.Permissions, nil
	}
	with := []string{"DeleteObject", "GetObject", PermBypassGovernanceRetention}

	// A parent without it cannot hand it out.
	got, err := mint([]string{"DeleteObject", "GetObject"}, []string{"DeleteObject", PermBypassGovernanceRetention})
	require.NoError(t, err)
	assert.Equal(t, []string{"DeleteObject"}, got)

	// A parent with it: kept only when asked for.
	got, err = mint(with, []string{"DeleteObject", PermBypassGovernanceRetention})
	require.NoError(t, err)
	assert.Equal(t, []string{"DeleteObject", PermBypassGovernanceRetention}, got)
	got, err = mint(with, []string{"DeleteObject"})
	require.NoError(t, err)
	assert.Equal(t, []string{"DeleteObject"}, got)
	got, err = mint(with, nil)
	require.NoError(t, err)
	assert.Equal(t, []string{"DeleteObject", "GetObject"}, got, "an unscoped token copies the parent's operations, not its bypass")
	assert.Equal(t, 3, len(with), "the parent's own list is not modified")

	// The account's own authority.
	got, err = mint([]string{"*"}, nil)
	require.NoError(t, err)
	assert.Equal(t, []string{"*"}, got)
	assert.False(t, (&KeyScope{Permissions: got, Temporary: true}).CanBypassGovernanceRetention())
	got, err = mint([]string{"*"}, []string{"*", PermBypassGovernanceRetention})
	require.NoError(t, err)
	assert.True(t, (&KeyScope{Permissions: got, Temporary: true}).CanBypassGovernanceRetention())

	// Asking for the bypass and nothing else from a parent that lacks it is
	// an empty scope, refused like any other.
	_, err = mint([]string{"DeleteObject"}, []string{PermBypassGovernanceRetention})
	require.ErrorIs(t, err, ErrSTSScope)
}
