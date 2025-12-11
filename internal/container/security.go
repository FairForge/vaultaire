// internal/container/security.go
package container

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// Severity represents vulnerability severity level
type Severity int

const (
	SeverityUnknown Severity = iota
	SeverityLow
	SeverityMedium
	SeverityHigh
	SeverityCritical
)

// String returns severity as string
func (s Severity) String() string {
	switch s {
	case SeverityCritical:
		return "CRITICAL"
	case SeverityHigh:
		return "HIGH"
	case SeverityMedium:
		return "MEDIUM"
	case SeverityLow:
		return "LOW"
	default:
		return "UNKNOWN"
	}
}

// ParseSeverity parses severity from string
func ParseSeverity(s string) Severity {
	switch strings.ToUpper(s) {
	case "CRITICAL":
		return SeverityCritical
	case "HIGH":
		return SeverityHigh
	case "MEDIUM":
		return SeverityMedium
	case "LOW":
		return SeverityLow
	default:
		return SeverityUnknown
	}
}

// Vulnerability represents a security vulnerability
type Vulnerability struct {
	ID          string   `json:"id"`
	Package     string   `json:"package"`
	Version     string   `json:"version"`
	FixedIn     string   `json:"fixed_in"`
	Severity    Severity `json:"severity"`
	Description string   `json:"description"`
	Link        string   `json:"link"`
}

// ScanResult represents image scan results
type ScanResult struct {
	Image           string          `json:"image"`
	Scanner         string          `json:"scanner"`
	ScannedAt       time.Time       `json:"scanned_at"`
	Vulnerabilities []Vulnerability `json:"vulnerabilities"`
}

// CountBySeverity returns count of vulnerabilities by severity
func (r *ScanResult) CountBySeverity() map[Severity]int {
	counts := make(map[Severity]int)
	for _, v := range r.Vulnerabilities {
		counts[v.Severity]++
	}
	return counts
}

// HasCritical returns true if there are critical vulnerabilities
func (r *ScanResult) HasCritical() bool {
	for _, v := range r.Vulnerabilities {
		if v.Severity == SeverityCritical {
			return true
		}
	}
	return false
}

// FilterBySeverity returns vulnerabilities at or above given severity
func (r *ScanResult) FilterBySeverity(minSeverity Severity) []Vulnerability {
	var filtered []Vulnerability
	for _, v := range r.Vulnerabilities {
		if v.Severity >= minSeverity {
			filtered = append(filtered, v)
		}
	}
	return filtered
}

// ScanPolicy defines security scanning policy
type ScanPolicy struct {
	FailOnCritical   bool     `json:"fail_on_critical"`
	FailOnHigh       bool     `json:"fail_on_high"`
	MaxCritical      int      `json:"max_critical"`
	MaxHigh          int      `json:"max_high"`
	IgnoredCVEs      []string `json:"ignored_cves"`
	RequiredScanners []string `json:"required_scanners"`
}

// Evaluate checks scan result against policy
func (p *ScanPolicy) Evaluate(result *ScanResult) (passed bool, violations []string) {
	// Filter out ignored CVEs
	var filtered []Vulnerability
	ignoredSet := make(map[string]bool)
	for _, cve := range p.IgnoredCVEs {
		ignoredSet[cve] = true
	}
	for _, v := range result.Vulnerabilities {
		if !ignoredSet[v.ID] {
			filtered = append(filtered, v)
		}
	}

	// Count by severity
	counts := make(map[Severity]int)
	for _, v := range filtered {
		counts[v.Severity]++
	}

	passed = true

	// Check critical
	if p.FailOnCritical && counts[SeverityCritical] > p.MaxCritical {
		passed = false
		violations = append(violations, fmt.Sprintf("critical vulnerabilities: %d (max: %d)", counts[SeverityCritical], p.MaxCritical))
	}

	// Check high
	if p.FailOnHigh && counts[SeverityHigh] > p.MaxHigh {
		passed = false
		violations = append(violations, fmt.Sprintf("high vulnerabilities: %d (max: %d)", counts[SeverityHigh], p.MaxHigh))
	}

	// Check max thresholds even without fail flags
	if p.MaxHigh > 0 && counts[SeverityHigh] > p.MaxHigh {
		passed = false
		violations = append(violations, fmt.Sprintf("high vulnerabilities exceed threshold: %d (max: %d)", counts[SeverityHigh], p.MaxHigh))
	}

	return passed, violations
}

// DefaultScanPolicy returns default security policy
func DefaultScanPolicy() *ScanPolicy {
	return &ScanPolicy{
		FailOnCritical: true,
		FailOnHigh:     false,
		MaxCritical:    0,
		MaxHigh:        10,
	}
}

// StrictScanPolicy returns strict security policy
func StrictScanPolicy() *ScanPolicy {
	return &ScanPolicy{
		FailOnCritical: true,
		FailOnHigh:     true,
		MaxCritical:    0,
		MaxHigh:        0,
	}
}

// ScannerConfig configures the image scanner
type ScannerConfig struct {
	Scanner       string        `json:"scanner"`
	Timeout       time.Duration `json:"timeout"`
	CacheDir      string        `json:"cache_dir"`
	OfflineMode   bool          `json:"offline_mode"`
	IgnoreUnfixed bool          `json:"ignore_unfixed"`
}

// ImageScanner scans container images for vulnerabilities
type ImageScanner struct {
	config *ScannerConfig
}

// NewImageScanner creates a new image scanner
func NewImageScanner(config *ScannerConfig) *ImageScanner {
	return &ImageScanner{config: config}
}

// Scan scans an image for vulnerabilities
func (s *ImageScanner) Scan(ctx context.Context, image string) (*ScanResult, error) {
	if s.config.Scanner == "mock" {
		return &ScanResult{
			Image:           image,
			Scanner:         "mock",
			ScannedAt:       time.Now(),
			Vulnerabilities: []Vulnerability{},
		}, nil
	}

	// In real implementation, would exec trivy/grype
	return &ScanResult{
		Image:     image,
		Scanner:   s.config.Scanner,
		ScannedAt: time.Now(),
	}, nil
}

// BuildCommand builds the scanner command
func (s *ImageScanner) BuildCommand(image string) []string {
	switch s.config.Scanner {
	case "trivy":
		cmd := []string{"trivy", "image", "--format", "json"}
		if s.config.IgnoreUnfixed {
			cmd = append(cmd, "--ignore-unfixed")
		}
		if s.config.CacheDir != "" {
			cmd = append(cmd, "--cache-dir", s.config.CacheDir)
		}
		cmd = append(cmd, image)
		return cmd
	default:
		return []string{s.config.Scanner, image}
	}
}

// TrivyOutput represents Trivy JSON output
type TrivyOutput struct {
	Results []TrivyResult `json:"Results"`
}

// TrivyResult represents a single Trivy result
type TrivyResult struct {
	Vulnerabilities []TrivyVuln `json:"Vulnerabilities"`
}

// TrivyVuln represents a Trivy vulnerability
type TrivyVuln struct {
	VulnerabilityID  string `json:"VulnerabilityID"`
	PkgName          string `json:"PkgName"`
	InstalledVersion string `json:"InstalledVersion"`
	FixedVersion     string `json:"FixedVersion"`
	Severity         string `json:"Severity"`
	Description      string `json:"Description"`
}

// ParseTrivyOutput parses Trivy JSON output
func (s *ImageScanner) ParseTrivyOutput(data []byte) (*ScanResult, error) {
	var output TrivyOutput
	if err := json.Unmarshal(data, &output); err != nil {
		return nil, fmt.Errorf("parse trivy output: %w", err)
	}

	result := &ScanResult{
		Scanner:   "trivy",
		ScannedAt: time.Now(),
	}

	for _, r := range output.Results {
		for _, v := range r.Vulnerabilities {
			result.Vulnerabilities = append(result.Vulnerabilities, Vulnerability{
				ID:          v.VulnerabilityID,
				Package:     v.PkgName,
				Version:     v.InstalledVersion,
				FixedIn:     v.FixedVersion,
				Severity:    ParseSeverity(v.Severity),
				Description: v.Description,
			})
		}
	}

	return result, nil
}

// GenerateSecurityReport generates a text security report
func GenerateSecurityReport(result *ScanResult) string {
	var sb strings.Builder

	sb.WriteString(fmt.Sprintf("Security Scan Report: %s\n", result.Image))
	sb.WriteString(fmt.Sprintf("Scanner: %s\n", result.Scanner))
	sb.WriteString(fmt.Sprintf("Scanned: %s\n", result.ScannedAt.Format(time.RFC3339)))
	sb.WriteString(strings.Repeat("-", 60) + "\n")

	counts := result.CountBySeverity()
	sb.WriteString(fmt.Sprintf("CRITICAL: %d\n", counts[SeverityCritical]))
	sb.WriteString(fmt.Sprintf("HIGH: %d\n", counts[SeverityHigh]))
	sb.WriteString(fmt.Sprintf("MEDIUM: %d\n", counts[SeverityMedium]))
	sb.WriteString(fmt.Sprintf("LOW: %d\n", counts[SeverityLow]))
	sb.WriteString(strings.Repeat("-", 60) + "\n")

	for _, v := range result.Vulnerabilities {
		sb.WriteString(fmt.Sprintf("[%s] %s - %s@%s\n", v.Severity.String(), v.ID, v.Package, v.Version))
		if v.FixedIn != "" {
			sb.WriteString(fmt.Sprintf("  Fixed in: %s\n", v.FixedIn))
		}
	}

	return sb.String()
}
