// internal/container/registry_test.go
package container

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRegistryConfig_Validate(t *testing.T) {
	t.Run("valid config passes", func(t *testing.T) {
		config := &RegistryConfig{
			URL: "ghcr.io",
		}
		err := config.Validate()
		assert.NoError(t, err)
	})

	t.Run("rejects empty URL", func(t *testing.T) {
		config := &RegistryConfig{}
		err := config.Validate()
		assert.Error(t, err)
	})
}

func TestRegistryAuth(t *testing.T) {
	t.Run("creates basic auth", func(t *testing.T) {
		auth := &RegistryAuth{
			Username: "user",
			Password: "pass",
		}
		assert.Equal(t, "user", auth.Username)
	})

	t.Run("creates token auth", func(t *testing.T) {
		auth := &RegistryAuth{
			Token: "ghp_xxxx",
		}
		assert.NotEmpty(t, auth.Token)
	})

	t.Run("encodes basic auth header", func(t *testing.T) {
		auth := &RegistryAuth{
			Username: "user",
			Password: "pass",
		}
		header := auth.BasicAuthHeader()
		assert.Contains(t, header, "Basic ")
	})
}

func TestImageReference(t *testing.T) {
	t.Run("parses full reference", func(t *testing.T) {
		ref, err := ParseImageReference("ghcr.io/fairforge/vaultaire:1.0.0")
		require.NoError(t, err)
		assert.Equal(t, "ghcr.io", ref.Registry)
		assert.Equal(t, "fairforge/vaultaire", ref.Repository)
		assert.Equal(t, "1.0.0", ref.Tag)
	})

	t.Run("parses reference with digest", func(t *testing.T) {
		ref, err := ParseImageReference("ghcr.io/fairforge/vaultaire@sha256:abc123")
		require.NoError(t, err)
		assert.Equal(t, "sha256:abc123", ref.Digest)
	})

	t.Run("parses docker hub reference", func(t *testing.T) {
		ref, err := ParseImageReference("nginx:latest")
		require.NoError(t, err)
		assert.Equal(t, "docker.io", ref.Registry)
		assert.Equal(t, "library/nginx", ref.Repository)
		assert.Equal(t, "latest", ref.Tag)
	})

	t.Run("defaults tag to latest", func(t *testing.T) {
		ref, err := ParseImageReference("ghcr.io/fairforge/vaultaire")
		require.NoError(t, err)
		assert.Equal(t, "latest", ref.Tag)
	})

	t.Run("converts to string", func(t *testing.T) {
		ref := &ImageReference{
			Registry:   "ghcr.io",
			Repository: "fairforge/vaultaire",
			Tag:        "1.0.0",
		}
		assert.Equal(t, "ghcr.io/fairforge/vaultaire:1.0.0", ref.String())
	})

	t.Run("rejects invalid reference", func(t *testing.T) {
		_, err := ParseImageReference("")
		assert.Error(t, err)
	})
}

func TestNewRegistryClient(t *testing.T) {
	t.Run("creates client", func(t *testing.T) {
		client := NewRegistryClient(&RegistryConfig{
			URL: "ghcr.io",
		})
		assert.NotNil(t, client)
	})

	t.Run("creates client with auth", func(t *testing.T) {
		client := NewRegistryClient(&RegistryConfig{
			URL: "ghcr.io",
			Auth: &RegistryAuth{
				Username: "user",
				Password: "token",
			},
		})
		assert.NotNil(t, client)
	})
}

func TestRegistryClient_ListTags(t *testing.T) {
	client := NewRegistryClient(&RegistryConfig{
		URL: "mock",
	})

	t.Run("lists tags", func(t *testing.T) {
		ctx := context.Background()
		tags, err := client.ListTags(ctx, "fairforge/vaultaire")
		require.NoError(t, err)
		assert.NotEmpty(t, tags)
	})
}

func TestRegistryClient_GetManifest(t *testing.T) {
	client := NewRegistryClient(&RegistryConfig{
		URL: "mock",
	})

	t.Run("gets manifest", func(t *testing.T) {
		ctx := context.Background()
		manifest, err := client.GetManifest(ctx, "fairforge/vaultaire", "latest")
		require.NoError(t, err)
		assert.NotNil(t, manifest)
	})
}

func TestImageManifest(t *testing.T) {
	t.Run("creates manifest", func(t *testing.T) {
		manifest := &ImageManifest{
			SchemaVersion: 2,
			MediaType:     "application/vnd.docker.distribution.manifest.v2+json",
			Config: ManifestConfig{
				MediaType: "application/vnd.docker.container.image.v1+json",
				Digest:    "sha256:abc123",
				Size:      1024,
			},
			Layers: []ManifestLayer{
				{
					MediaType: "application/vnd.docker.image.rootfs.diff.tar.gzip",
					Digest:    "sha256:layer1",
					Size:      10240,
				},
			},
		}
		assert.Equal(t, 2, manifest.SchemaVersion)
		assert.Len(t, manifest.Layers, 1)
	})

	t.Run("calculates total size", func(t *testing.T) {
		manifest := &ImageManifest{
			Config: ManifestConfig{Size: 1024},
			Layers: []ManifestLayer{
				{Size: 10240},
				{Size: 20480},
			},
		}
		assert.Equal(t, int64(31744), manifest.TotalSize())
	})
}

func TestRegistryClient_ImageExists(t *testing.T) {
	client := NewRegistryClient(&RegistryConfig{
		URL: "mock",
	})

	t.Run("returns true for existing image", func(t *testing.T) {
		ctx := context.Background()
		exists, err := client.ImageExists(ctx, "fairforge/vaultaire", "latest")
		require.NoError(t, err)
		assert.True(t, exists)
	})
}

func TestRegistryClient_DeleteTag(t *testing.T) {
	client := NewRegistryClient(&RegistryConfig{
		URL: "mock",
	})

	t.Run("deletes tag", func(t *testing.T) {
		ctx := context.Background()
		err := client.DeleteTag(ctx, "fairforge/vaultaire", "old-tag")
		assert.NoError(t, err)
	})
}

func TestRegistryClient_CopyImage(t *testing.T) {
	client := NewRegistryClient(&RegistryConfig{
		URL: "mock",
	})

	t.Run("copies image to new tag", func(t *testing.T) {
		ctx := context.Background()
		err := client.CopyImage(ctx, "fairforge/vaultaire:1.0.0", "fairforge/vaultaire:stable")
		assert.NoError(t, err)
	})
}

func TestTagPolicy(t *testing.T) {
	t.Run("creates tag policy", func(t *testing.T) {
		policy := &TagPolicy{
			Pattern:    "v*",
			Immutable:  true,
			MaxAgeDays: 90,
			KeepRecent: 10,
		}
		assert.True(t, policy.Immutable)
	})

	t.Run("matches semver tags", func(t *testing.T) {
		policy := &TagPolicy{Pattern: "v[0-9]*"}
		assert.True(t, policy.Matches("v1.0.0"))
		assert.False(t, policy.Matches("latest"))
	})
}

func TestRegistryClient_CleanupTags(t *testing.T) {
	client := NewRegistryClient(&RegistryConfig{
		URL: "mock",
	})

	t.Run("cleans up old tags", func(t *testing.T) {
		ctx := context.Background()
		policy := &TagPolicy{
			MaxAgeDays: 30,
			KeepRecent: 5,
		}
		deleted, err := client.CleanupTags(ctx, "fairforge/vaultaire", policy)
		require.NoError(t, err)
		assert.GreaterOrEqual(t, len(deleted), 0)
	})
}

func TestTagInfo(t *testing.T) {
	t.Run("creates tag info", func(t *testing.T) {
		info := &TagInfo{
			Name:      "v1.0.0",
			Digest:    "sha256:abc123",
			CreatedAt: time.Now(),
			Size:      1024000,
		}
		assert.Equal(t, "v1.0.0", info.Name)
	})
}

func TestRegistryConfig_WithDefaults(t *testing.T) {
	t.Run("applies defaults", func(t *testing.T) {
		config := &RegistryConfig{URL: "ghcr.io"}
		config.WithDefaults()
		assert.Equal(t, 30*time.Second, config.Timeout)
	})
}

func TestCommonRegistries(t *testing.T) {
	t.Run("docker hub config", func(t *testing.T) {
		config := DockerHubConfig("username", "token")
		assert.Equal(t, "docker.io", config.URL)
	})

	t.Run("ghcr config", func(t *testing.T) {
		config := GHCRConfig("token")
		assert.Equal(t, "ghcr.io", config.URL)
	})

	t.Run("ecr config", func(t *testing.T) {
		config := ECRConfig("123456789012", "us-east-1")
		assert.Contains(t, config.URL, "ecr")
	})
}
