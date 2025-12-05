// internal/devops/release_test.go
package devops

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestReleaseConfig_Validate(t *testing.T) {
	t.Run("valid config passes", func(t *testing.T) {
		config := &ReleaseConfig{
			Name:    "v1.0.0",
			Version: "1.0.0",
		}
		err := config.Validate()
		assert.NoError(t, err)
	})

	t.Run("rejects empty name", func(t *testing.T) {
		config := &ReleaseConfig{Version: "1.0.0"}
		err := config.Validate()
		assert.Error(t, err)
	})
}

func TestNewReleaseManager(t *testing.T) {
	t.Run("creates manager", func(t *testing.T) {
		manager := NewReleaseManager(nil)
		assert.NotNil(t, manager)
	})
}

func TestReleaseManager_Create(t *testing.T) {
	manager := NewReleaseManager(nil)

	t.Run("creates release", func(t *testing.T) {
		release, err := manager.Create(&ReleaseConfig{
			Name:    "v1.0.0",
			Version: "1.0.0",
			Notes:   "Initial release",
		})

		require.NoError(t, err)
		assert.Equal(t, "v1.0.0", release.Name())
		assert.Equal(t, ReleaseStatusDraft, release.Status())
	})

	t.Run("rejects duplicate", func(t *testing.T) {
		_, _ = manager.Create(&ReleaseConfig{Name: "dup", Version: "1.0.0"})
		_, err := manager.Create(&ReleaseConfig{Name: "dup", Version: "1.0.0"})
		assert.Error(t, err)
	})
}

func TestRelease_Artifacts(t *testing.T) {
	manager := NewReleaseManager(nil)
	release, _ := manager.Create(&ReleaseConfig{Name: "v1.0.0", Version: "1.0.0"})

	t.Run("adds artifact", func(t *testing.T) {
		err := release.AddArtifact(&ReleaseArtifact{
			Name:     "app-binary",
			Path:     "dist/app",
			Checksum: "sha256:abc123",
			Size:     1024000,
		})
		assert.NoError(t, err)
	})

	t.Run("lists artifacts", func(t *testing.T) {
		artifacts := release.Artifacts()
		assert.Len(t, artifacts, 1)
	})
}

func TestRelease_Changelog(t *testing.T) {
	manager := NewReleaseManager(nil)
	release, _ := manager.Create(&ReleaseConfig{Name: "v1.1.0", Version: "1.1.0"})

	t.Run("adds changelog entry", func(t *testing.T) {
		err := release.AddChangelogEntry(&ChangelogEntry{
			Type:        ChangeTypeFeature,
			Description: "Added new feature",
			IssueRef:    "#123",
		})
		assert.NoError(t, err)
	})

	t.Run("generates changelog", func(t *testing.T) {
		changelog := release.Changelog()
		assert.Contains(t, changelog, "Added new feature")
	})
}

func TestRelease_Lifecycle(t *testing.T) {
	manager := NewReleaseManager(nil)
	release, _ := manager.Create(&ReleaseConfig{Name: "v1.0.0", Version: "1.0.0"})

	t.Run("transitions to candidate", func(t *testing.T) {
		err := release.MarkAsCandidate()
		assert.NoError(t, err)
		assert.Equal(t, ReleaseStatusCandidate, release.Status())
	})

	t.Run("transitions to released", func(t *testing.T) {
		err := release.Publish()
		assert.NoError(t, err)
		assert.Equal(t, ReleaseStatusReleased, release.Status())
		assert.False(t, release.ReleasedAt().IsZero())
	})
}

func TestRelease_Approval(t *testing.T) {
	manager := NewReleaseManager(nil)
	release, _ := manager.Create(&ReleaseConfig{
		Name:              "v1.0.0",
		Version:           "1.0.0",
		RequiredApprovers: []string{"qa", "security"},
	})

	t.Run("adds approval", func(t *testing.T) {
		err := release.Approve("qa", "user@example.com")
		assert.NoError(t, err)
	})

	t.Run("tracks pending approvals", func(t *testing.T) {
		pending := release.PendingApprovals()
		assert.Contains(t, pending, "security")
		assert.NotContains(t, pending, "qa")
	})

	t.Run("is fully approved", func(t *testing.T) {
		_ = release.Approve("security", "admin@example.com")
		assert.True(t, release.IsFullyApproved())
	})
}

func TestRelease_Tags(t *testing.T) {
	manager := NewReleaseManager(nil)
	release, _ := manager.Create(&ReleaseConfig{Name: "v1.0.0", Version: "1.0.0"})

	t.Run("adds tag", func(t *testing.T) {
		err := release.AddTag("stable")
		assert.NoError(t, err)
	})

	t.Run("checks tag", func(t *testing.T) {
		assert.True(t, release.HasTag("stable"))
		assert.False(t, release.HasTag("beta"))
	})
}

func TestReleaseManager_GetByVersion(t *testing.T) {
	manager := NewReleaseManager(nil)
	_, _ = manager.Create(&ReleaseConfig{Name: "v1.0.0", Version: "1.0.0"})
	_, _ = manager.Create(&ReleaseConfig{Name: "v1.1.0", Version: "1.1.0"})

	t.Run("gets by version", func(t *testing.T) {
		release := manager.GetByVersion("1.1.0")
		assert.NotNil(t, release)
		assert.Equal(t, "v1.1.0", release.Name())
	})
}

func TestReleaseManager_GetLatest(t *testing.T) {
	manager := NewReleaseManager(nil)

	r1, _ := manager.Create(&ReleaseConfig{Name: "v1.0.0", Version: "1.0.0"})
	_ = r1.Publish()

	r2, _ := manager.Create(&ReleaseConfig{Name: "v1.1.0", Version: "1.1.0"})
	_ = r2.Publish()

	t.Run("gets latest release", func(t *testing.T) {
		latest := manager.GetLatest()
		assert.NotNil(t, latest)
		assert.Equal(t, "v1.1.0", latest.Name())
	})
}

func TestReleaseManager_List(t *testing.T) {
	manager := NewReleaseManager(nil)
	_, _ = manager.Create(&ReleaseConfig{Name: "v1.0.0", Version: "1.0.0"})
	_, _ = manager.Create(&ReleaseConfig{Name: "v1.1.0", Version: "1.1.0"})

	t.Run("lists releases", func(t *testing.T) {
		releases := manager.List()
		assert.Len(t, releases, 2)
	})
}

func TestReleaseStatuses(t *testing.T) {
	t.Run("defines statuses", func(t *testing.T) {
		assert.Equal(t, "draft", ReleaseStatusDraft)
		assert.Equal(t, "candidate", ReleaseStatusCandidate)
		assert.Equal(t, "released", ReleaseStatusReleased)
		assert.Equal(t, "deprecated", ReleaseStatusDeprecated)
	})
}

func TestChangeTypes(t *testing.T) {
	t.Run("defines change types", func(t *testing.T) {
		assert.Equal(t, "feature", ChangeTypeFeature)
		assert.Equal(t, "bugfix", ChangeTypeBugfix)
		assert.Equal(t, "breaking", ChangeTypeBreaking)
		assert.Equal(t, "security", ChangeTypeSecurity)
	})
}

func TestRelease_Deprecate(t *testing.T) {
	manager := NewReleaseManager(nil)
	release, _ := manager.Create(&ReleaseConfig{Name: "v1.0.0", Version: "1.0.0"})
	_ = release.Publish()

	t.Run("deprecates release", func(t *testing.T) {
		err := release.Deprecate("Use v2.0.0 instead")
		assert.NoError(t, err)
		assert.Equal(t, ReleaseStatusDeprecated, release.Status())
	})
}

func TestRelease_Metadata(t *testing.T) {
	manager := NewReleaseManager(nil)
	release, _ := manager.Create(&ReleaseConfig{Name: "v1.0.0", Version: "1.0.0"})

	t.Run("sets metadata", func(t *testing.T) {
		release.SetMetadata("commit", "abc123")
		release.SetMetadata("branch", "main")
	})

	t.Run("gets metadata", func(t *testing.T) {
		commit := release.GetMetadata("commit")
		assert.Equal(t, "abc123", commit)
	})
}

func TestReleaseArtifact(t *testing.T) {
	t.Run("creates artifact", func(t *testing.T) {
		artifact := &ReleaseArtifact{
			Name:      "app.tar.gz",
			Path:      "/releases/app.tar.gz",
			Checksum:  "sha256:xyz",
			Size:      2048000,
			CreatedAt: time.Now(),
		}
		assert.Equal(t, "app.tar.gz", artifact.Name)
	})
}
