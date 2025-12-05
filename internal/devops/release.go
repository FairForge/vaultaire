// internal/devops/release.go
package devops

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

// Release statuses
const (
	ReleaseStatusDraft      = "draft"
	ReleaseStatusCandidate  = "candidate"
	ReleaseStatusReleased   = "released"
	ReleaseStatusDeprecated = "deprecated"
)

// Change types
const (
	ChangeTypeFeature  = "feature"
	ChangeTypeBugfix   = "bugfix"
	ChangeTypeBreaking = "breaking"
	ChangeTypeSecurity = "security"
)

// ReleaseConfig configures a release
type ReleaseConfig struct {
	Name              string   `json:"name"`
	Version           string   `json:"version"`
	Notes             string   `json:"notes"`
	RequiredApprovers []string `json:"required_approvers"`
}

// Validate checks configuration
func (c *ReleaseConfig) Validate() error {
	if c.Name == "" {
		return errors.New("release: name is required")
	}
	return nil
}

// ReleaseArtifact represents a release artifact
type ReleaseArtifact struct {
	Name      string    `json:"name"`
	Path      string    `json:"path"`
	Checksum  string    `json:"checksum"`
	Size      int64     `json:"size"`
	CreatedAt time.Time `json:"created_at"`
}

// ChangelogEntry represents a changelog entry
type ChangelogEntry struct {
	Type        string `json:"type"`
	Description string `json:"description"`
	IssueRef    string `json:"issue_ref"`
}

// Approval represents an approval
type Approval struct {
	Role       string    `json:"role"`
	ApprovedBy string    `json:"approved_by"`
	ApprovedAt time.Time `json:"approved_at"`
}

// Release represents a software release
type Release struct {
	config       *ReleaseConfig
	status       string
	artifacts    []*ReleaseArtifact
	changelog    []*ChangelogEntry
	approvals    map[string]*Approval
	tags         map[string]bool
	metadata     map[string]string
	createdAt    time.Time
	releasedAt   time.Time
	deprecatedAt time.Time
	deprecateMsg string
	mu           sync.RWMutex
}

// Name returns the release name
func (r *Release) Name() string {
	return r.config.Name
}

// Version returns the release version
func (r *Release) Version() string {
	return r.config.Version
}

// Status returns the release status
func (r *Release) Status() string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.status
}

// ReleasedAt returns when the release was published
func (r *Release) ReleasedAt() time.Time {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.releasedAt
}

// AddArtifact adds an artifact
func (r *Release) AddArtifact(artifact *ReleaseArtifact) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if artifact.CreatedAt.IsZero() {
		artifact.CreatedAt = time.Now()
	}

	r.artifacts = append(r.artifacts, artifact)
	return nil
}

// Artifacts returns all artifacts
func (r *Release) Artifacts() []*ReleaseArtifact {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.artifacts
}

// AddChangelogEntry adds a changelog entry
func (r *Release) AddChangelogEntry(entry *ChangelogEntry) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.changelog = append(r.changelog, entry)
	return nil
}

// Changelog generates changelog text
func (r *Release) Changelog() string {
	r.mu.RLock()
	defer r.mu.RUnlock()

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("# %s\n\n", r.config.Name))

	for _, entry := range r.changelog {
		sb.WriteString(fmt.Sprintf("- [%s] %s", entry.Type, entry.Description))
		if entry.IssueRef != "" {
			sb.WriteString(fmt.Sprintf(" (%s)", entry.IssueRef))
		}
		sb.WriteString("\n")
	}

	return sb.String()
}

// MarkAsCandidate marks release as candidate
func (r *Release) MarkAsCandidate() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.status = ReleaseStatusCandidate
	return nil
}

// Publish publishes the release
func (r *Release) Publish() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.status = ReleaseStatusReleased
	r.releasedAt = time.Now()
	return nil
}

// Deprecate deprecates the release
func (r *Release) Deprecate(message string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.status = ReleaseStatusDeprecated
	r.deprecatedAt = time.Now()
	r.deprecateMsg = message
	return nil
}

// Approve adds an approval
func (r *Release) Approve(role, approvedBy string) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.approvals[role] = &Approval{
		Role:       role,
		ApprovedBy: approvedBy,
		ApprovedAt: time.Now(),
	}
	return nil
}

// PendingApprovals returns pending approvals
func (r *Release) PendingApprovals() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()

	var pending []string
	for _, role := range r.config.RequiredApprovers {
		if _, approved := r.approvals[role]; !approved {
			pending = append(pending, role)
		}
	}
	return pending
}

// IsFullyApproved checks if all approvals are in
func (r *Release) IsFullyApproved() bool {
	r.mu.RLock()
	defer r.mu.RUnlock()

	for _, role := range r.config.RequiredApprovers {
		if _, approved := r.approvals[role]; !approved {
			return false
		}
	}
	return true
}

// AddTag adds a tag
func (r *Release) AddTag(tag string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.tags[tag] = true
	return nil
}

// HasTag checks if release has a tag
func (r *Release) HasTag(tag string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.tags[tag]
}

// SetMetadata sets metadata
func (r *Release) SetMetadata(key, value string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.metadata[key] = value
}

// GetMetadata gets metadata
func (r *Release) GetMetadata(key string) string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.metadata[key]
}

// ReleaseManagerConfig configures the manager
type ReleaseManagerConfig struct {
	MaxReleases int
}

// ReleaseManager manages releases
type ReleaseManager struct {
	config   *ReleaseManagerConfig
	releases map[string]*Release
	mu       sync.RWMutex
}

// NewReleaseManager creates a release manager
func NewReleaseManager(config *ReleaseManagerConfig) *ReleaseManager {
	if config == nil {
		config = &ReleaseManagerConfig{MaxReleases: 1000}
	}

	return &ReleaseManager{
		config:   config,
		releases: make(map[string]*Release),
	}
}

// Create creates a release
func (m *ReleaseManager) Create(config *ReleaseConfig) (*Release, error) {
	if err := config.Validate(); err != nil {
		return nil, err
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	if _, exists := m.releases[config.Name]; exists {
		return nil, fmt.Errorf("release: %s already exists", config.Name)
	}

	release := &Release{
		config:    config,
		status:    ReleaseStatusDraft,
		artifacts: make([]*ReleaseArtifact, 0),
		changelog: make([]*ChangelogEntry, 0),
		approvals: make(map[string]*Approval),
		tags:      make(map[string]bool),
		metadata:  make(map[string]string),
		createdAt: time.Now(),
	}

	m.releases[config.Name] = release
	return release, nil
}

// Get returns a release by name
func (m *ReleaseManager) Get(name string) *Release {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.releases[name]
}

// GetByVersion returns a release by version
func (m *ReleaseManager) GetByVersion(version string) *Release {
	m.mu.RLock()
	defer m.mu.RUnlock()

	for _, r := range m.releases {
		if r.config.Version == version {
			return r
		}
	}
	return nil
}

// GetLatest returns the latest published release
func (m *ReleaseManager) GetLatest() *Release {
	m.mu.RLock()
	defer m.mu.RUnlock()

	var latest *Release
	var latestTime time.Time

	for _, r := range m.releases {
		r.mu.RLock()
		if r.status == ReleaseStatusReleased && r.releasedAt.After(latestTime) {
			latest = r
			latestTime = r.releasedAt
		}
		r.mu.RUnlock()
	}

	return latest
}

// List returns all releases
func (m *ReleaseManager) List() []*Release {
	m.mu.RLock()
	defer m.mu.RUnlock()

	releases := make([]*Release, 0, len(m.releases))
	for _, r := range m.releases {
		releases = append(releases, r)
	}
	return releases
}
