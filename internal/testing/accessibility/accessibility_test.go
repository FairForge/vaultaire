package accessibility

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestNewChecker(t *testing.T) {
	checker := NewChecker("http://localhost:8080/")

	if checker.BaseURL != "http://localhost:8080" {
		t.Errorf("expected trailing slash removed, got %s", checker.BaseURL)
	}
	if checker.HTTPClient == nil {
		t.Error("expected HTTP client")
	}
}

func TestChecker_CheckHTML_AllIssues(t *testing.T) {
	html := `<!DOCTYPE html>
<html>
<head><title>Test</title></head>
<body>
<img src="photo.jpg">
<input type="text" name="email">
<a>Click here</a>
<h3>Subheading without h1</h3>
</body>
</html>`

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(html))
	}))
	defer server.Close()

	checker := NewChecker(server.URL)
	ctx := context.Background()

	report, err := checker.CheckHTML(ctx, "/")
	if err != nil {
		t.Fatalf("CheckHTML failed: %v", err)
	}

	if len(report.Issues) == 0 {
		t.Error("expected issues to be found")
	}

	// Check for specific issues
	foundImageIssue := false
	foundFormIssue := false
	foundHeadingIssue := false
	foundLangIssue := false

	for _, issue := range report.Issues {
		switch issue.Type {
		case IssueTypeImage:
			foundImageIssue = true
		case IssueTypeForm:
			foundFormIssue = true
		case IssueTypeHeading:
			foundHeadingIssue = true
		case IssueTypeSemantic:
			if issue.Description == "HTML element missing lang attribute" {
				foundLangIssue = true
			}
		}
	}

	if !foundImageIssue {
		t.Error("expected image accessibility issue")
	}
	if !foundFormIssue {
		t.Error("expected form accessibility issue")
	}
	if !foundHeadingIssue {
		t.Error("expected heading accessibility issue")
	}
	if !foundLangIssue {
		t.Error("expected lang attribute issue")
	}

	t.Logf("Found %d issues", len(report.Issues))
}

func TestChecker_CheckHTML_Accessible(t *testing.T) {
	html := `<!DOCTYPE html>
<html lang="en">
<head><title>Accessible Page</title></head>
<body>
<header><nav>Navigation</nav></header>
<main>
<h1>Main Title</h1>
<h2>Section</h2>
<img src="photo.jpg" alt="A descriptive photo">
<form>
<label for="email">Email:</label>
<input type="email" id="email" name="email">
</form>
<a href="/about">About our company</a>
</main>
<footer>Footer</footer>
</body>
</html>`

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(html))
	}))
	defer server.Close()

	checker := NewChecker(server.URL)
	ctx := context.Background()

	report, err := checker.CheckHTML(ctx, "/")
	if err != nil {
		t.Fatalf("CheckHTML failed: %v", err)
	}

	// Should have minimal issues
	criticalCount := 0
	for _, issue := range report.Issues {
		if issue.Severity == SeverityCritical {
			criticalCount++
			t.Logf("Critical issue: %s", issue.Description)
		}
	}

	if criticalCount > 0 {
		t.Errorf("expected no critical issues, got %d", criticalCount)
	}

	if len(report.Passed) == 0 {
		t.Error("expected some passed checks")
	}

	t.Logf("Passed: %v", report.Passed)
}

func TestChecker_CheckAPIAccessibility(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error": "Invalid request", "message": "Missing parameter"}`))
	}))
	defer server.Close()

	checker := NewChecker(server.URL)
	ctx := context.Background()

	report, err := checker.CheckAPIAccessibility(ctx, "/api/test")
	if err != nil {
		t.Fatalf("CheckAPIAccessibility failed: %v", err)
	}

	// Should pass Content-Type check
	foundContentType := false
	for _, p := range report.Passed {
		if p == "Content-Type header present" {
			foundContentType = true
		}
	}
	if !foundContentType {
		t.Error("expected Content-Type to pass")
	}
}

func TestChecker_CheckAPIAccessibility_MissingContentType(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Write header first to prevent auto Content-Type detection
		w.WriteHeader(http.StatusOK)
		// Don't write body - avoids Content-Type auto-detection
	}))
	defer server.Close()

	checker := NewChecker(server.URL)
	ctx := context.Background()

	report, err := checker.CheckAPIAccessibility(ctx, "/")
	if err != nil {
		t.Fatalf("CheckAPIAccessibility failed: %v", err)
	}

	foundIssue := false
	for _, issue := range report.Issues {
		if issue.Description == "API response missing Content-Type header" {
			foundIssue = true
		}
	}
	if !foundIssue {
		t.Error("expected Content-Type issue")
	}
}

func TestReport_HasCritical(t *testing.T) {
	report := &Report{
		Issues: []Issue{
			{Severity: SeverityModerate},
			{Severity: SeverityMinor},
		},
	}

	if report.HasCritical() {
		t.Error("expected no critical")
	}

	report.Issues = append(report.Issues, Issue{Severity: SeverityCritical})
	if !report.HasCritical() {
		t.Error("expected critical")
	}
}

func TestReport_CountBySeverity(t *testing.T) {
	report := &Report{
		Issues: []Issue{
			{Severity: SeverityCritical},
			{Severity: SeveritySerious},
			{Severity: SeveritySerious},
			{Severity: SeverityModerate},
			{Severity: SeverityMinor},
			{Severity: SeverityMinor},
			{Severity: SeverityMinor},
		},
	}

	counts := report.CountBySeverity()

	if counts[SeverityCritical] != 1 {
		t.Errorf("expected 1 critical, got %d", counts[SeverityCritical])
	}
	if counts[SeveritySerious] != 2 {
		t.Errorf("expected 2 serious, got %d", counts[SeveritySerious])
	}
	if counts[SeverityMinor] != 3 {
		t.Errorf("expected 3 minor, got %d", counts[SeverityMinor])
	}
}

func TestReport_GenerateReport(t *testing.T) {
	report := &Report{
		URL:       "http://example.com",
		Timestamp: time.Now(),
		Issues: []Issue{
			{
				Type:        IssueTypeImage,
				Severity:    SeverityCritical,
				Element:     "<img src='test.jpg'>",
				Description: "Missing alt attribute",
				Suggestion:  "Add alt text",
			},
		},
		Passed: []string{"Language defined", "Main landmark present"},
	}

	output := report.GenerateReport()

	if output == "" {
		t.Error("expected non-empty report")
	}
	if len(output) < 100 {
		t.Error("report seems too short")
	}

	t.Logf("Report:\n%s", output)
}

func TestIssue(t *testing.T) {
	issue := Issue{
		Type:        IssueTypeForm,
		Severity:    SeveritySerious,
		Element:     "<input>",
		Description: "Missing label",
		Suggestion:  "Add label",
		Line:        42,
	}

	if issue.Type != IssueTypeForm {
		t.Error("Type not set")
	}
	if issue.Line != 42 {
		t.Error("Line not set")
	}
}

func TestIssueType(t *testing.T) {
	types := []IssueType{
		IssueTypeImage,
		IssueTypeForm,
		IssueTypeLink,
		IssueTypeHeading,
		IssueTypeContrast,
		IssueTypeKeyboard,
		IssueTypeSemantic,
		IssueTypeAPI,
		IssueTypeError,
	}

	for _, typ := range types {
		if typ == "" {
			t.Error("issue type should not be empty")
		}
	}
}

func TestSeverity(t *testing.T) {
	severities := []Severity{
		SeverityCritical,
		SeveritySerious,
		SeverityModerate,
		SeverityMinor,
	}

	for _, s := range severities {
		if s == "" {
			t.Error("severity should not be empty")
		}
	}
}

func TestTruncate(t *testing.T) {
	tests := []struct {
		input    string
		maxLen   int
		expected string
	}{
		{"short", 10, "short"},
		{"this is a longer string", 10, "this is..."},
		{"exact", 5, "exact"},
	}

	for _, tt := range tests {
		result := truncate(tt.input, tt.maxLen)
		if result != tt.expected {
			t.Errorf("truncate(%q, %d) = %q, expected %q",
				tt.input, tt.maxLen, result, tt.expected)
		}
	}
}

func TestAssertAccessible(t *testing.T) {
	mockT := &testing.T{}

	report := &Report{
		Issues: []Issue{
			{Severity: SeverityMinor},
		},
	}
	AssertAccessible(mockT, report)

	// With critical would fail
}

func TestAssertNoIssues(t *testing.T) {
	mockT := &testing.T{}

	report := &Report{Issues: []Issue{}}
	AssertNoIssues(mockT, report)

	// With any issues would fail
}
