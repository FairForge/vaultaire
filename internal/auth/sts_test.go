package auth

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGenerateSTSToken(t *testing.T) {
	ctx := context.Background()
	parentScope := &KeyScope{Permissions: []string{"*"}}

	token, err := GenerateSTSToken(ctx, nil, "tenant-1", "parent-key-1", parentScope, STSRequest{
		Permissions: []string{"GetObject", "PutObject"},
		TTL:         600,
	})
	require.NoError(t, err)

	assert.True(t, len(token.AccessKey) >= 20)
	assert.Equal(t, "ASIA", token.AccessKey[:4])
	assert.Len(t, token.SecretKey, 40)
	assert.Equal(t, "tenant-1", token.TenantID)
	assert.Equal(t, "parent-key-1", token.ParentKeyID)
	assert.Equal(t, []string{"GetObject", "PutObject"}, token.Permissions)
	assert.WithinDuration(t, time.Now().Add(600*time.Second), token.ExpiresAt, 5*time.Second)
}

func TestSTSToken_ScopeIntersection(t *testing.T) {
	ctx := context.Background()
	parentScope := &KeyScope{
		Permissions: []string{"GetObject", "PutObject"},
	}

	token, err := GenerateSTSToken(ctx, nil, "tenant-1", "parent-key-1", parentScope, STSRequest{
		Permissions: []string{"PutObject", "DeleteObject"},
	})
	require.NoError(t, err)
	assert.Equal(t, []string{"PutObject"}, token.Permissions)
}

func TestSTSToken_EmptyIntersection(t *testing.T) {
	ctx := context.Background()
	parentScope := &KeyScope{
		Permissions: []string{"GetObject"},
	}

	_, err := GenerateSTSToken(ctx, nil, "tenant-1", "parent-key-1", parentScope, STSRequest{
		Permissions: []string{"DeleteObject"},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no permissions overlap")
}

func TestSTSToken_TTLClamping(t *testing.T) {
	ctx := context.Background()
	parentScope := &KeyScope{Permissions: []string{"*"}}

	t.Run("zero defaults to 3600", func(t *testing.T) {
		token, err := GenerateSTSToken(ctx, nil, "t", "k", parentScope, STSRequest{
			Permissions: []string{"GetObject"},
			TTL:         0,
		})
		require.NoError(t, err)
		assert.WithinDuration(t, time.Now().Add(3600*time.Second), token.ExpiresAt, 5*time.Second)
	})

	t.Run("over max clamped to 43200", func(t *testing.T) {
		token, err := GenerateSTSToken(ctx, nil, "t", "k", parentScope, STSRequest{
			Permissions: []string{"GetObject"},
			TTL:         99999,
		})
		require.NoError(t, err)
		assert.WithinDuration(t, time.Now().Add(43200*time.Second), token.ExpiresAt, 5*time.Second)
	})

	t.Run("negative defaults to 3600", func(t *testing.T) {
		token, err := GenerateSTSToken(ctx, nil, "t", "k", parentScope, STSRequest{
			Permissions: []string{"GetObject"},
			TTL:         -100,
		})
		require.NoError(t, err)
		assert.WithinDuration(t, time.Now().Add(3600*time.Second), token.ExpiresAt, 5*time.Second)
	})
}

func TestSTSToken_BucketScopeIntersection(t *testing.T) {
	ctx := context.Background()

	t.Run("parent restricted, request restricted — intersection", func(t *testing.T) {
		parentScope := &KeyScope{
			Permissions: []string{"*"},
			BucketScope: []string{"alpha", "bravo"},
		}
		token, err := GenerateSTSToken(ctx, nil, "t", "k", parentScope, STSRequest{
			Permissions: []string{"GetObject"},
			BucketScope: []string{"bravo", "charlie"},
		})
		require.NoError(t, err)
		assert.Equal(t, []string{"bravo"}, token.BucketScope)
	})

	t.Run("parent unrestricted, request restricted — request wins", func(t *testing.T) {
		parentScope := &KeyScope{
			Permissions: []string{"*"},
		}
		token, err := GenerateSTSToken(ctx, nil, "t", "k", parentScope, STSRequest{
			Permissions: []string{"GetObject"},
			BucketScope: []string{"uploads"},
		})
		require.NoError(t, err)
		assert.Equal(t, []string{"uploads"}, token.BucketScope)
	})

	t.Run("parent restricted, request empty — parent wins", func(t *testing.T) {
		parentScope := &KeyScope{
			Permissions: []string{"*"},
			BucketScope: []string{"data"},
		}
		token, err := GenerateSTSToken(ctx, nil, "t", "k", parentScope, STSRequest{
			Permissions: []string{"GetObject"},
		})
		require.NoError(t, err)
		assert.Equal(t, []string{"data"}, token.BucketScope)
	})
}

func TestSTSToken_IPRestrict(t *testing.T) {
	ctx := context.Background()
	parentScope := &KeyScope{
		Permissions: []string{"*"},
		IPAllowlist: []string{"10.0.0.0/8"},
	}

	t.Run("request narrows parent", func(t *testing.T) {
		token, err := GenerateSTSToken(ctx, nil, "t", "k", parentScope, STSRequest{
			Permissions: []string{"GetObject"},
			IPRestrict:  []string{"10.1.0.0/16"},
		})
		require.NoError(t, err)
		assert.Equal(t, []string{"10.1.0.0/16"}, token.IPRestrict)
	})

	t.Run("no request IP — inherits parent", func(t *testing.T) {
		token, err := GenerateSTSToken(ctx, nil, "t", "k", parentScope, STSRequest{
			Permissions: []string{"GetObject"},
		})
		require.NoError(t, err)
		assert.Equal(t, []string{"10.0.0.0/8"}, token.IPRestrict)
	})
}

func TestSTSToken_WildcardParent_NoRequestPerms(t *testing.T) {
	ctx := context.Background()
	parentScope := &KeyScope{Permissions: []string{"*"}}

	token, err := GenerateSTSToken(ctx, nil, "t", "k", parentScope, STSRequest{})
	require.NoError(t, err)
	assert.Equal(t, []string{"*"}, token.Permissions)
}

func TestSTSToken_AccessKeyFormat(t *testing.T) {
	ctx := context.Background()
	parentScope := &KeyScope{Permissions: []string{"*"}}

	for i := 0; i < 10; i++ {
		token, err := GenerateSTSToken(ctx, nil, "t", "k", parentScope, STSRequest{
			Permissions: []string{"GetObject"},
		})
		require.NoError(t, err)
		assert.Equal(t, "ASIA", token.AccessKey[:4])
		assert.Len(t, token.AccessKey, 20)
	}
}
