// Package coverage provides tools for measuring and reporting test coverage.
package coverage

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Profile represents coverage data for a package.
type Profile struct {
	Package    string
	Filename   string
	Statements int
	Covered    int
	Percentage float64
}

// Report represents a complete coverage report.
type Report struct {
	Profiles   []Profile
	Total      CoverageSummary
	ByPackage  map[string]CoverageSummary
	Thresholds Thresholds
	Generated  string
}

// CoverageSummary provides aggregated coverage statistics.
type CoverageSummary struct {
	Packages   int
	Files      int
	Statements int
	Covered    int
	Percentage float64
}

// Thresholds defines minimum coverage requirements.
type Thresholds struct {
	Global  float64            // Minimum overall coverage
	Package map[string]float64 // Per-package minimums
	Default float64            // Default for unlisted packages
}

// DefaultThresholds returns recommended coverage thresholds.
func DefaultThresholds() Thresholds {
	return Thresholds{
		Global:  70.0,
		Default: 60.0,
		Package: map[string]float64{
			"github.com/FairForge/vaultaire/internal/storage": 80.0,
			"github.com/FairForge/vaultaire/internal/s3":      80.0,
			"github.com/FairForge/vaultaire/internal/auth":    85.0,
		},
	}
}

// ParseCoverageFile parses a Go coverage profile file.
func ParseCoverageFile(path string) (*Report, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("opening coverage file: %w", err)
	}
	defer func() {
		_ = file.Close()
	}()

	report := &Report{
		Profiles:   make([]Profile, 0),
		ByPackage:  make(map[string]CoverageSummary),
		Thresholds: DefaultThresholds(),
	}

	// Track coverage by file
	fileData := make(map[string]*Profile)

	scanner := bufio.NewScanner(file)
	lineNum := 0

	// Pattern: filename:startLine.startCol,endLine.endCol numStatements count
	pattern := regexp.MustCompile(`^(.+):(\d+)\.(\d+),(\d+)\.(\d+) (\d+) (\d+)$`)

	for scanner.Scan() {
		line := scanner.Text()
		lineNum++

		// Skip mode line
		if strings.HasPrefix(line, "mode:") {
			continue
		}

		matches := pattern.FindStringSubmatch(line)
		if matches == nil {
			continue
		}

		filename := matches[1]
		statements, _ := strconv.Atoi(matches[6])
		count, _ := strconv.Atoi(matches[7])

		// Get or create file profile
		profile, exists := fileData[filename]
		if !exists {
			pkg := extractPackage(filename)
			profile = &Profile{
				Package:  pkg,
				Filename: filename,
			}
			fileData[filename] = profile
		}

		profile.Statements += statements
		if count > 0 {
			profile.Covered += statements
		}
	}

	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("reading coverage file: %w", err)
	}

	// Calculate percentages and build report
	for _, profile := range fileData {
		if profile.Statements > 0 {
			profile.Percentage = float64(profile.Covered) / float64(profile.Statements) * 100
		}
		report.Profiles = append(report.Profiles, *profile)

		// Aggregate by package
		pkgSummary := report.ByPackage[profile.Package]
		pkgSummary.Files++
		pkgSummary.Statements += profile.Statements
		pkgSummary.Covered += profile.Covered
		report.ByPackage[profile.Package] = pkgSummary
	}

	// Calculate package percentages
	for pkg, summary := range report.ByPackage {
		if summary.Statements > 0 {
			summary.Percentage = float64(summary.Covered) / float64(summary.Statements) * 100
		}
		summary.Packages = 1
		report.ByPackage[pkg] = summary
	}

	// Calculate total
	report.Total.Packages = len(report.ByPackage)
	report.Total.Files = len(report.Profiles)
	for _, summary := range report.ByPackage {
		report.Total.Statements += summary.Statements
		report.Total.Covered += summary.Covered
	}
	if report.Total.Statements > 0 {
		report.Total.Percentage = float64(report.Total.Covered) / float64(report.Total.Statements) * 100
	}

	// Sort profiles by package then filename
	sort.Slice(report.Profiles, func(i, j int) bool {
		if report.Profiles[i].Package != report.Profiles[j].Package {
			return report.Profiles[i].Package < report.Profiles[j].Package
		}
		return report.Profiles[i].Filename < report.Profiles[j].Filename
	})

	return report, nil
}

// extractPackage extracts the package path from a filename.
func extractPackage(filename string) string {
	// Remove the file part
	dir := filepath.Dir(filename)
	return dir
}

// CheckThresholds validates coverage against thresholds.
func (r *Report) CheckThresholds() []Violation {
	violations := make([]Violation, 0)

	// Check global threshold
	if r.Total.Percentage < r.Thresholds.Global {
		violations = append(violations, Violation{
			Type:      ViolationGlobal,
			Package:   "",
			Threshold: r.Thresholds.Global,
			Actual:    r.Total.Percentage,
			Message: fmt.Sprintf("Global coverage %.1f%% is below threshold %.1f%%",
				r.Total.Percentage, r.Thresholds.Global),
		})
	}

	// Check per-package thresholds
	for pkg, summary := range r.ByPackage {
		threshold := r.Thresholds.Default
		if t, ok := r.Thresholds.Package[pkg]; ok {
			threshold = t
		}

		if summary.Percentage < threshold {
			violations = append(violations, Violation{
				Type:      ViolationPackage,
				Package:   pkg,
				Threshold: threshold,
				Actual:    summary.Percentage,
				Message: fmt.Sprintf("Package %s coverage %.1f%% is below threshold %.1f%%",
					pkg, summary.Percentage, threshold),
			})
		}
	}

	// Sort violations by severity (global first, then by gap)
	sort.Slice(violations, func(i, j int) bool {
		if violations[i].Type != violations[j].Type {
			return violations[i].Type < violations[j].Type
		}
		gapI := violations[i].Threshold - violations[i].Actual
		gapJ := violations[j].Threshold - violations[j].Actual
		return gapI > gapJ
	})

	return violations
}

// Violation represents a coverage threshold violation.
type Violation struct {
	Type      ViolationType
	Package   string
	Threshold float64
	Actual    float64
	Message   string
}

// ViolationType categorizes the violation.
type ViolationType int

const (
	ViolationGlobal ViolationType = iota
	ViolationPackage
)

// GenerateReport creates a formatted coverage report.
func (r *Report) GenerateReport() string {
	var sb strings.Builder

	sb.WriteString("Coverage Report\n")
	sb.WriteString("===============\n\n")

	// Summary
	sb.WriteString(fmt.Sprintf("Total Coverage: %.1f%%\n", r.Total.Percentage))
	sb.WriteString(fmt.Sprintf("Statements: %d/%d covered\n", r.Total.Covered, r.Total.Statements))
	sb.WriteString(fmt.Sprintf("Packages: %d\n", r.Total.Packages))
	sb.WriteString(fmt.Sprintf("Files: %d\n\n", r.Total.Files))

	// Package breakdown
	sb.WriteString("Package Coverage:\n")
	sb.WriteString("-----------------\n")

	// Sort packages by name
	packages := make([]string, 0, len(r.ByPackage))
	for pkg := range r.ByPackage {
		packages = append(packages, pkg)
	}
	sort.Strings(packages)

	for _, pkg := range packages {
		summary := r.ByPackage[pkg]
		status := "✓"
		threshold := r.Thresholds.Default
		if t, ok := r.Thresholds.Package[pkg]; ok {
			threshold = t
		}
		if summary.Percentage < threshold {
			status = "✗"
		}
		sb.WriteString(fmt.Sprintf("%s %s: %.1f%% (%d/%d)\n",
			status, shortenPackage(pkg), summary.Percentage, summary.Covered, summary.Statements))
	}

	// Threshold check
	violations := r.CheckThresholds()
	if len(violations) > 0 {
		sb.WriteString("\nThreshold Violations:\n")
		sb.WriteString("---------------------\n")
		for _, v := range violations {
			sb.WriteString(fmt.Sprintf("✗ %s\n", v.Message))
		}
	} else {
		sb.WriteString("\n✓ All coverage thresholds met\n")
	}

	return sb.String()
}

// shortenPackage removes common prefix for readability.
func shortenPackage(pkg string) string {
	const prefix = "github.com/FairForge/vaultaire/"
	if strings.HasPrefix(pkg, prefix) {
		return strings.TrimPrefix(pkg, prefix)
	}
	return pkg
}

// GenerateHTML creates an HTML coverage report.
func (r *Report) GenerateHTML() string {
	var sb strings.Builder

	sb.WriteString(`<!DOCTYPE html>
<html>
<head>
    <title>Coverage Report</title>
    <style>
        body { font-family: -apple-system, BlinkMacSystemFont, sans-serif; margin: 40px; }
        .summary { background: #f5f5f5; padding: 20px; border-radius: 8px; margin-bottom: 20px; }
        .coverage-bar { background: #e0e0e0; height: 20px; border-radius: 4px; overflow: hidden; }
        .coverage-fill { height: 100%; transition: width 0.3s; }
        .high { background: #4caf50; }
        .medium { background: #ff9800; }
        .low { background: #f44336; }
        table { width: 100%; border-collapse: collapse; }
        th, td { padding: 10px; text-align: left; border-bottom: 1px solid #ddd; }
        th { background: #f5f5f5; }
        .pass { color: #4caf50; }
        .fail { color: #f44336; }
    </style>
</head>
<body>
`)

	// Summary section
	sb.WriteString(fmt.Sprintf(`
    <div class="summary">
        <h1>Coverage Report</h1>
        <h2>%.1f%% Total Coverage</h2>
        <div class="coverage-bar">
            <div class="coverage-fill %s" style="width: %.1f%%"></div>
        </div>
        <p>%d/%d statements covered across %d packages</p>
    </div>
`, r.Total.Percentage, coverageClass(r.Total.Percentage), r.Total.Percentage,
		r.Total.Covered, r.Total.Statements, r.Total.Packages))

	// Package table
	sb.WriteString(`
    <table>
        <thead>
            <tr>
                <th>Package</th>
                <th>Coverage</th>
                <th>Statements</th>
                <th>Status</th>
            </tr>
        </thead>
        <tbody>
`)

	packages := make([]string, 0, len(r.ByPackage))
	for pkg := range r.ByPackage {
		packages = append(packages, pkg)
	}
	sort.Strings(packages)

	for _, pkg := range packages {
		summary := r.ByPackage[pkg]
		threshold := r.Thresholds.Default
		if t, ok := r.Thresholds.Package[pkg]; ok {
			threshold = t
		}
		status := "pass"
		statusText := "✓"
		if summary.Percentage < threshold {
			status = "fail"
			statusText = "✗"
		}

		sb.WriteString(fmt.Sprintf(`
            <tr>
                <td>%s</td>
                <td>
                    <div class="coverage-bar" style="width: 200px; display: inline-block;">
                        <div class="coverage-fill %s" style="width: %.1f%%"></div>
                    </div>
                    %.1f%%
                </td>
                <td>%d/%d</td>
                <td class="%s">%s</td>
            </tr>
`, shortenPackage(pkg), coverageClass(summary.Percentage), summary.Percentage,
			summary.Percentage, summary.Covered, summary.Statements, status, statusText))
	}

	sb.WriteString(`
        </tbody>
    </table>
</body>
</html>
`)

	return sb.String()
}

// coverageClass returns CSS class based on coverage percentage.
func coverageClass(pct float64) string {
	if pct >= 80 {
		return "high"
	}
	if pct >= 60 {
		return "medium"
	}
	return "low"
}

// SetThreshold sets a package-specific coverage threshold.
func (r *Report) SetThreshold(pkg string, threshold float64) {
	if r.Thresholds.Package == nil {
		r.Thresholds.Package = make(map[string]float64)
	}
	r.Thresholds.Package[pkg] = threshold
}

// SetGlobalThreshold sets the global coverage threshold.
func (r *Report) SetGlobalThreshold(threshold float64) {
	r.Thresholds.Global = threshold
}

// GetUncoveredPackages returns packages below the default threshold.
func (r *Report) GetUncoveredPackages() []string {
	uncovered := make([]string, 0)
	for pkg, summary := range r.ByPackage {
		threshold := r.Thresholds.Default
		if t, ok := r.Thresholds.Package[pkg]; ok {
			threshold = t
		}
		if summary.Percentage < threshold {
			uncovered = append(uncovered, pkg)
		}
	}
	sort.Strings(uncovered)
	return uncovered
}
