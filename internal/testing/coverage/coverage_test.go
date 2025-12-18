package coverage

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDefaultThresholds(t *testing.T) {
	th := DefaultThresholds()

	if th.Global != 70.0 {
		t.Errorf("expected global threshold 70.0, got %.1f", th.Global)
	}
	if th.Default != 60.0 {
		t.Errorf("expected default threshold 60.0, got %.1f", th.Default)
	}
	if len(th.Package) == 0 {
		t.Error("expected package-specific thresholds")
	}
}

func TestParseCoverageFile(t *testing.T) {
	// Create temp coverage file
	content := `mode: set
github.com/FairForge/vaultaire/internal/storage/backend.go:10.20,15.2 3 1
github.com/FairForge/vaultaire/internal/storage/backend.go:20.30,25.2 2 0
github.com/FairForge/vaultaire/internal/storage/local.go:10.20,20.2 5 1
github.com/FairForge/vaultaire/internal/auth/auth.go:10.20,30.2 10 10
`

	tmpDir := t.TempDir()
	coverFile := filepath.Join(tmpDir, "coverage.out")
	if err := os.WriteFile(coverFile, []byte(content), 0644); err != nil {
		t.Fatalf("failed to write coverage file: %v", err)
	}

	report, err := ParseCoverageFile(coverFile)
	if err != nil {
		t.Fatalf("failed to parse coverage file: %v", err)
	}

	if report.Total.Files != 3 {
		t.Errorf("expected 3 files, got %d", report.Total.Files)
	}
	if report.Total.Packages != 2 {
		t.Errorf("expected 2 packages, got %d", report.Total.Packages)
	}

	// Total: 3+2+5+10 = 20 statements, 3+5+10 = 18 covered
	if report.Total.Statements != 20 {
		t.Errorf("expected 20 statements, got %d", report.Total.Statements)
	}
	if report.Total.Covered != 18 {
		t.Errorf("expected 18 covered, got %d", report.Total.Covered)
	}

	expectedPct := 90.0 // 18/20 = 90%
	if report.Total.Percentage != expectedPct {
		t.Errorf("expected %.1f%% coverage, got %.1f%%", expectedPct, report.Total.Percentage)
	}

	t.Logf("Parsed report: %d packages, %d files, %.1f%% coverage",
		report.Total.Packages, report.Total.Files, report.Total.Percentage)
}

func TestParseCoverageFile_NotFound(t *testing.T) {
	_, err := ParseCoverageFile("/nonexistent/file.out")
	if err == nil {
		t.Error("expected error for nonexistent file")
	}
}

func TestReport_CheckThresholds_Pass(t *testing.T) {
	report := &Report{
		Total: CoverageSummary{
			Percentage: 85.0,
		},
		ByPackage: map[string]CoverageSummary{
			"pkg/a": {Percentage: 80.0},
			"pkg/b": {Percentage: 75.0},
		},
		Thresholds: Thresholds{
			Global:  70.0,
			Default: 60.0,
		},
	}

	violations := report.CheckThresholds()
	if len(violations) != 0 {
		t.Errorf("expected no violations, got %d", len(violations))
	}
}

func TestReport_CheckThresholds_GlobalFail(t *testing.T) {
	report := &Report{
		Total: CoverageSummary{
			Percentage: 50.0,
		},
		ByPackage:  map[string]CoverageSummary{},
		Thresholds: Thresholds{Global: 70.0},
	}

	violations := report.CheckThresholds()
	if len(violations) != 1 {
		t.Fatalf("expected 1 violation, got %d", len(violations))
	}
	if violations[0].Type != ViolationGlobal {
		t.Error("expected global violation")
	}

	t.Logf("Violation: %s", violations[0].Message)
}

func TestReport_CheckThresholds_PackageFail(t *testing.T) {
	report := &Report{
		Total: CoverageSummary{
			Percentage: 80.0,
		},
		ByPackage: map[string]CoverageSummary{
			"pkg/critical": {Percentage: 50.0},
		},
		Thresholds: Thresholds{
			Global:  70.0,
			Default: 60.0,
			Package: map[string]float64{
				"pkg/critical": 80.0,
			},
		},
	}

	violations := report.CheckThresholds()
	if len(violations) != 1 {
		t.Fatalf("expected 1 violation, got %d", len(violations))
	}
	if violations[0].Type != ViolationPackage {
		t.Error("expected package violation")
	}
	if violations[0].Package != "pkg/critical" {
		t.Errorf("expected pkg/critical, got %s", violations[0].Package)
	}

	t.Logf("Violation: %s", violations[0].Message)
}

func TestReport_GenerateReport(t *testing.T) {
	report := &Report{
		Total: CoverageSummary{
			Packages:   2,
			Files:      5,
			Statements: 100,
			Covered:    75,
			Percentage: 75.0,
		},
		ByPackage: map[string]CoverageSummary{
			"github.com/FairForge/vaultaire/internal/storage": {
				Files:      3,
				Statements: 60,
				Covered:    50,
				Percentage: 83.3,
			},
			"github.com/FairForge/vaultaire/internal/auth": {
				Files:      2,
				Statements: 40,
				Covered:    25,
				Percentage: 62.5,
			},
		},
		Thresholds: DefaultThresholds(),
	}

	output := report.GenerateReport()

	if !strings.Contains(output, "75.0%") {
		t.Error("expected total percentage in report")
	}
	if !strings.Contains(output, "internal/storage") {
		t.Error("expected package names in report")
	}

	t.Logf("Generated report:\n%s", output)
}

func TestReport_GenerateHTML(t *testing.T) {
	report := &Report{
		Total: CoverageSummary{
			Packages:   1,
			Files:      2,
			Statements: 50,
			Covered:    40,
			Percentage: 80.0,
		},
		ByPackage: map[string]CoverageSummary{
			"github.com/FairForge/vaultaire/internal/storage": {
				Files:      2,
				Statements: 50,
				Covered:    40,
				Percentage: 80.0,
			},
		},
		Thresholds: DefaultThresholds(),
	}

	html := report.GenerateHTML()

	if !strings.Contains(html, "<!DOCTYPE html>") {
		t.Error("expected HTML doctype")
	}
	if !strings.Contains(html, "80.0%") {
		t.Error("expected percentage in HTML")
	}
	if !strings.Contains(html, "coverage-bar") {
		t.Error("expected coverage bar CSS")
	}

	t.Logf("Generated HTML length: %d bytes", len(html))
}

func TestReport_SetThreshold(t *testing.T) {
	report := &Report{
		Thresholds: Thresholds{},
	}

	report.SetThreshold("pkg/a", 90.0)

	if report.Thresholds.Package["pkg/a"] != 90.0 {
		t.Error("threshold not set correctly")
	}
}

func TestReport_SetGlobalThreshold(t *testing.T) {
	report := &Report{
		Thresholds: Thresholds{Global: 70.0},
	}

	report.SetGlobalThreshold(80.0)

	if report.Thresholds.Global != 80.0 {
		t.Error("global threshold not set correctly")
	}
}

func TestReport_GetUncoveredPackages(t *testing.T) {
	report := &Report{
		ByPackage: map[string]CoverageSummary{
			"pkg/good":     {Percentage: 80.0},
			"pkg/bad":      {Percentage: 40.0},
			"pkg/critical": {Percentage: 70.0},
		},
		Thresholds: Thresholds{
			Default: 60.0,
			Package: map[string]float64{
				"pkg/critical": 80.0,
			},
		},
	}

	uncovered := report.GetUncoveredPackages()

	if len(uncovered) != 2 {
		t.Errorf("expected 2 uncovered packages, got %d", len(uncovered))
	}

	t.Logf("Uncovered packages: %v", uncovered)
}

func TestCoverageSummary(t *testing.T) {
	summary := CoverageSummary{
		Packages:   5,
		Files:      20,
		Statements: 1000,
		Covered:    800,
		Percentage: 80.0,
	}

	if summary.Packages != 5 {
		t.Error("Packages not set correctly")
	}
	if summary.Percentage != 80.0 {
		t.Error("Percentage not set correctly")
	}
}

func TestViolation(t *testing.T) {
	v := Violation{
		Type:      ViolationPackage,
		Package:   "pkg/test",
		Threshold: 80.0,
		Actual:    60.0,
		Message:   "Coverage below threshold",
	}

	if v.Type != ViolationPackage {
		t.Error("Type not set correctly")
	}
	if v.Threshold-v.Actual != 20.0 {
		t.Error("Gap calculation incorrect")
	}
}

func TestExtractPackage(t *testing.T) {
	tests := []struct {
		filename string
		expected string
	}{
		{"github.com/FairForge/vaultaire/internal/storage/backend.go", "github.com/FairForge/vaultaire/internal/storage"},
		{"pkg/auth/auth.go", "pkg/auth"},
		{"main.go", "."},
	}

	for _, tt := range tests {
		result := extractPackage(tt.filename)
		if result != tt.expected {
			t.Errorf("extractPackage(%q) = %q, expected %q", tt.filename, result, tt.expected)
		}
	}
}

func TestShortenPackage(t *testing.T) {
	tests := []struct {
		pkg      string
		expected string
	}{
		{"github.com/FairForge/vaultaire/internal/storage", "internal/storage"},
		{"github.com/FairForge/vaultaire/pkg/api", "pkg/api"},
		{"other/package", "other/package"},
	}

	for _, tt := range tests {
		result := shortenPackage(tt.pkg)
		if result != tt.expected {
			t.Errorf("shortenPackage(%q) = %q, expected %q", tt.pkg, result, tt.expected)
		}
	}
}

func TestCoverageClass(t *testing.T) {
	tests := []struct {
		pct      float64
		expected string
	}{
		{90.0, "high"},
		{80.0, "high"},
		{75.0, "medium"},
		{60.0, "medium"},
		{50.0, "low"},
		{0.0, "low"},
	}

	for _, tt := range tests {
		result := coverageClass(tt.pct)
		if result != tt.expected {
			t.Errorf("coverageClass(%.1f) = %q, expected %q", tt.pct, result, tt.expected)
		}
	}
}
