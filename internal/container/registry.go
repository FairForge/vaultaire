// internal/container/registry.go
package container

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// RegistryAuth contains registry authentication credentials
type RegistryAuth struct {
	Username string `json:"username"`
	Password string `json:"password"`
	Token    string `json:"token"`
}

// BasicAuthHeader returns the Basic auth header value
func (a *RegistryAuth) BasicAuthHeader() string {
	if a.Token != "" {
		return "Bearer " + a.Token
	}
	creds := base64.StdEncoding.EncodeToString([]byte(a.Username + ":" + a.Password))
	return "Basic " + creds
}

// RegistryConfig configures a container registry client
type RegistryConfig struct {
	URL      string        `json:"url"`
	Auth     *RegistryAuth `json:"auth"`
	Insecure bool          `json:"insecure"`
	Timeout  time.Duration `json:"timeout"`
}

// Validate checks the registry configuration
func (c *RegistryConfig) Validate() error {
	if c.URL == "" {
		return errors.New("registry: URL is required")
	}
	return nil
}

// WithDefaults applies default values
func (c *RegistryConfig) WithDefaults() {
	if c.Timeout == 0 {
		c.Timeout = 30 * time.Second
	}
}

// ImageReference represents a parsed container image reference
type ImageReference struct {
	Registry   string `json:"registry"`
	Repository string `json:"repository"`
	Tag        string `json:"tag"`
	Digest     string `json:"digest"`
}

// String returns the full image reference
func (r *ImageReference) String() string {
	s := r.Registry + "/" + r.Repository
	if r.Digest != "" {
		return s + "@" + r.Digest
	}
	if r.Tag != "" {
		return s + ":" + r.Tag
	}
	return s
}

// ParseImageReference parses an image reference string
func ParseImageReference(ref string) (*ImageReference, error) {
	if ref == "" {
		return nil, errors.New("registry: empty image reference")
	}

	result := &ImageReference{
		Registry: "docker.io",
		Tag:      "latest",
	}

	// Check for digest
	if idx := strings.Index(ref, "@"); idx != -1 {
		result.Digest = ref[idx+1:]
		ref = ref[:idx]
		result.Tag = ""
	}

	// Check for tag
	if idx := strings.LastIndex(ref, ":"); idx != -1 {
		// Make sure it's not a port number
		afterColon := ref[idx+1:]
		if !strings.Contains(afterColon, "/") {
			result.Tag = afterColon
			ref = ref[:idx]
		}
	}

	// Parse registry and repository
	parts := strings.SplitN(ref, "/", 2)
	if len(parts) == 1 {
		// Docker Hub official image
		result.Repository = "library/" + parts[0]
	} else if strings.Contains(parts[0], ".") || strings.Contains(parts[0], ":") || parts[0] == "localhost" {
		// Has registry
		result.Registry = parts[0]
		result.Repository = parts[1]
	} else {
		// Docker Hub user image
		result.Repository = ref
	}

	return result, nil
}

// ManifestConfig represents the config in a manifest
type ManifestConfig struct {
	MediaType string `json:"mediaType"`
	Digest    string `json:"digest"`
	Size      int64  `json:"size"`
}

// ManifestLayer represents a layer in a manifest
type ManifestLayer struct {
	MediaType string `json:"mediaType"`
	Digest    string `json:"digest"`
	Size      int64  `json:"size"`
}

// ImageManifest represents a container image manifest
type ImageManifest struct {
	SchemaVersion int             `json:"schemaVersion"`
	MediaType     string          `json:"mediaType"`
	Config        ManifestConfig  `json:"config"`
	Layers        []ManifestLayer `json:"layers"`
}

// TotalSize returns the total size of the image
func (m *ImageManifest) TotalSize() int64 {
	total := m.Config.Size
	for _, l := range m.Layers {
		total += l.Size
	}
	return total
}

// TagInfo contains information about a tag
type TagInfo struct {
	Name      string    `json:"name"`
	Digest    string    `json:"digest"`
	CreatedAt time.Time `json:"created_at"`
	Size      int64     `json:"size"`
}

// TagPolicy defines tag retention policy
type TagPolicy struct {
	Pattern    string `json:"pattern"`
	Immutable  bool   `json:"immutable"`
	MaxAgeDays int    `json:"max_age_days"`
	KeepRecent int    `json:"keep_recent"`
}

// Matches checks if a tag matches the policy pattern
func (p *TagPolicy) Matches(tag string) bool {
	if p.Pattern == "" {
		return true
	}
	// Convert glob to regex
	pattern := strings.ReplaceAll(p.Pattern, "*", ".*")
	pattern = "^" + pattern + "$"
	matched, _ := regexp.MatchString(pattern, tag)
	return matched
}

// RegistryClient interacts with container registries
type RegistryClient struct {
	config *RegistryConfig
}

// NewRegistryClient creates a new registry client
func NewRegistryClient(config *RegistryConfig) *RegistryClient {
	config.WithDefaults()
	return &RegistryClient{config: config}
}

// ListTags lists all tags for a repository
func (c *RegistryClient) ListTags(ctx context.Context, repository string) ([]string, error) {
	if c.config.URL == "mock" {
		return []string{"latest", "v1.0.0", "v1.1.0"}, nil
	}
	// Real implementation would call registry API
	return []string{}, nil
}

// GetManifest gets the manifest for an image
func (c *RegistryClient) GetManifest(ctx context.Context, repository, tag string) (*ImageManifest, error) {
	if c.config.URL == "mock" {
		return &ImageManifest{
			SchemaVersion: 2,
			MediaType:     "application/vnd.docker.distribution.manifest.v2+json",
			Config: ManifestConfig{
				Digest: "sha256:abc123",
				Size:   1024,
			},
			Layers: []ManifestLayer{
				{Digest: "sha256:layer1", Size: 10240},
			},
		}, nil
	}
	return nil, errors.New("registry: not implemented")
}

// ImageExists checks if an image exists
func (c *RegistryClient) ImageExists(ctx context.Context, repository, tag string) (bool, error) {
	if c.config.URL == "mock" {
		return true, nil
	}
	_, err := c.GetManifest(ctx, repository, tag)
	if err != nil {
		return false, nil
	}
	return true, nil
}

// DeleteTag deletes a tag from the registry
func (c *RegistryClient) DeleteTag(ctx context.Context, repository, tag string) error {
	if c.config.URL == "mock" {
		return nil
	}
	return errors.New("registry: not implemented")
}

// CopyImage copies an image to a new tag
func (c *RegistryClient) CopyImage(ctx context.Context, src, dst string) error {
	if c.config.URL == "mock" {
		return nil
	}
	return errors.New("registry: not implemented")
}

// CleanupTags removes tags according to policy
func (c *RegistryClient) CleanupTags(ctx context.Context, repository string, policy *TagPolicy) ([]string, error) {
	if c.config.URL == "mock" {
		return []string{}, nil
	}
	// Real implementation would:
	// 1. List all tags
	// 2. Filter by policy
	// 3. Delete matching tags
	return []string{}, nil
}

// DockerHubConfig returns config for Docker Hub
func DockerHubConfig(username, token string) *RegistryConfig {
	return &RegistryConfig{
		URL: "docker.io",
		Auth: &RegistryAuth{
			Username: username,
			Password: token,
		},
	}
}

// GHCRConfig returns config for GitHub Container Registry
func GHCRConfig(token string) *RegistryConfig {
	return &RegistryConfig{
		URL: "ghcr.io",
		Auth: &RegistryAuth{
			Username: "token",
			Password: token,
		},
	}
}

// ECRConfig returns config for AWS ECR
func ECRConfig(accountID, region string) *RegistryConfig {
	return &RegistryConfig{
		URL: fmt.Sprintf("%s.dkr.ecr.%s.amazonaws.com", accountID, region),
	}
}
