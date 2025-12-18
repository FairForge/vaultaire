// Package accessibility provides utilities for accessibility testing.
package accessibility

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"testing"
	"time"
)

// Checker performs accessibility checks.
type Checker struct {
	BaseURL    string
	HTTPClient *http.Client
}

// NewChecker creates an accessibility checker.
func NewChecker(baseURL string) *Checker {
	return &Checker{
		BaseURL: strings.TrimSuffix(baseURL, "/"),
		HTTPClient: &http.Client{
			Timeout: 30 * time.Second,
		},
	}
}

// Issue represents an accessibility issue.
type Issue struct {
	Type        IssueType
	Severity    Severity
	Element     string
	Description string
	Suggestion  string
	Line        int
}

// IssueType categorizes accessibility issues.
type IssueType string

const (
	IssueTypeImage    IssueType = "image"
	IssueTypeForm     IssueType = "form"
	IssueTypeLink     IssueType = "link"
	IssueTypeHeading  IssueType = "heading"
	IssueTypeContrast IssueType = "contrast"
	IssueTypeKeyboard IssueType = "keyboard"
	IssueTypeSemantic IssueType = "semantic"
	IssueTypeAPI      IssueType = "api"
	IssueTypeError    IssueType = "error"
)

// Severity indicates the severity of an issue.
type Severity string

const (
	SeverityCritical Severity = "critical"
	SeveritySerious  Severity = "serious"
	SeverityModerate Severity = "moderate"
	SeverityMinor    Severity = "minor"
)

// Report contains accessibility check results.
type Report struct {
	URL       string
	Timestamp time.Time
	Issues    []Issue
	Passed    []string
}

// HasCritical returns true if any critical issues exist.
func (r *Report) HasCritical() bool {
	for _, issue := range r.Issues {
		if issue.Severity == SeverityCritical {
			return true
		}
	}
	return false
}

// CountBySeverity counts issues by severity.
func (r *Report) CountBySeverity() map[Severity]int {
	counts := make(map[Severity]int)
	for _, issue := range r.Issues {
		counts[issue.Severity]++
	}
	return counts
}

// CheckHTML performs accessibility checks on HTML content.
func (c *Checker) CheckHTML(ctx context.Context, path string) (*Report, error) {
	resp, err := c.get(ctx, path)
	if err != nil {
		return nil, fmt.Errorf("fetching page: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading body: %w", err)
	}

	html := string(body)
	report := &Report{
		URL:       c.BaseURL + path,
		Timestamp: time.Now(),
		Issues:    make([]Issue, 0),
		Passed:    make([]string, 0),
	}

	// Run checks
	c.checkImages(html, report)
	c.checkForms(html, report)
	c.checkLinks(html, report)
	c.checkHeadings(html, report)
	c.checkLandmarks(html, report)
	c.checkLanguage(html, report)

	return report, nil
}

// checkImages verifies images have alt text.
func (c *Checker) checkImages(html string, report *Report) {
	// Find images without alt attribute
	imgPattern := regexp.MustCompile(`<img[^>]*>`)
	altPattern := regexp.MustCompile(`alt\s*=\s*["'][^"']*["']`)
	emptyAltPattern := regexp.MustCompile(`alt\s*=\s*["']\s*["']`)

	matches := imgPattern.FindAllString(html, -1)
	imagesWithAlt := 0

	for _, img := range matches {
		if !altPattern.MatchString(img) {
			report.Issues = append(report.Issues, Issue{
				Type:        IssueTypeImage,
				Severity:    SeverityCritical,
				Element:     truncate(img, 100),
				Description: "Image missing alt attribute",
				Suggestion:  "Add descriptive alt text to all images",
			})
		} else if emptyAltPattern.MatchString(img) {
			// Empty alt is okay for decorative images but should be intentional
			report.Issues = append(report.Issues, Issue{
				Type:        IssueTypeImage,
				Severity:    SeverityMinor,
				Element:     truncate(img, 100),
				Description: "Image has empty alt attribute",
				Suggestion:  "Ensure empty alt is intentional for decorative images",
			})
		} else {
			imagesWithAlt++
		}
	}

	if imagesWithAlt > 0 {
		report.Passed = append(report.Passed, fmt.Sprintf("%d images have alt text", imagesWithAlt))
	}
}

// checkForms verifies form accessibility.
func (c *Checker) checkForms(html string, report *Report) {
	// Check for inputs without labels
	inputPattern := regexp.MustCompile(`<input[^>]*>`)
	idPattern := regexp.MustCompile(`id\s*=\s*["']([^"']+)["']`)

	matches := inputPattern.FindAllString(html, -1)
	for _, input := range matches {
		// Skip hidden inputs
		if strings.Contains(input, `type="hidden"`) || strings.Contains(input, `type='hidden'`) {
			continue
		}

		// Check for id
		idMatch := idPattern.FindStringSubmatch(input)
		if len(idMatch) < 2 {
			report.Issues = append(report.Issues, Issue{
				Type:        IssueTypeForm,
				Severity:    SeveritySerious,
				Element:     truncate(input, 100),
				Description: "Form input missing id attribute",
				Suggestion:  "Add id attribute and associate with label",
			})
			continue
		}

		// Check if label exists for this id
		labelPattern := regexp.MustCompile(fmt.Sprintf(`<label[^>]*for\s*=\s*["']%s["']`, regexp.QuoteMeta(idMatch[1])))
		if !labelPattern.MatchString(html) {
			// Check for aria-label
			if !strings.Contains(input, "aria-label") {
				report.Issues = append(report.Issues, Issue{
					Type:        IssueTypeForm,
					Severity:    SeveritySerious,
					Element:     truncate(input, 100),
					Description: fmt.Sprintf("Input with id=%q has no associated label", idMatch[1]),
					Suggestion:  "Add <label for=\"...\"> or aria-label attribute",
				})
			}
		}
	}
}

// checkLinks verifies link accessibility.
func (c *Checker) checkLinks(html string, report *Report) {
	// Find links with non-descriptive text
	linkPattern := regexp.MustCompile(`<a[^>]*>([^<]*)</a>`)

	matches := linkPattern.FindAllStringSubmatch(html, -1)
	badLinkTexts := []string{"click here", "here", "read more", "more", "link"}

	for _, match := range matches {
		if len(match) >= 2 {
			linkText := strings.ToLower(strings.TrimSpace(match[1]))
			for _, bad := range badLinkTexts {
				if linkText == bad {
					report.Issues = append(report.Issues, Issue{
						Type:        IssueTypeLink,
						Severity:    SeverityModerate,
						Element:     truncate(match[0], 100),
						Description: fmt.Sprintf("Link text %q is not descriptive", linkText),
						Suggestion:  "Use descriptive link text that indicates destination",
					})
					break
				}
			}
		}
	}

	// Check for links without href using simple pattern
	allLinksPattern := regexp.MustCompile(`<a[^>]*>`)
	hrefPattern := regexp.MustCompile(`href\s*=`)

	allLinks := allLinksPattern.FindAllString(html, -1)
	for _, link := range allLinks {
		if !hrefPattern.MatchString(link) {
			report.Issues = append(report.Issues, Issue{
				Type:        IssueTypeLink,
				Severity:    SeveritySerious,
				Element:     truncate(link, 100),
				Description: "Link missing href attribute",
				Suggestion:  "Add href attribute or use button element",
			})
		}
	}
}

// checkHeadings verifies heading hierarchy.
func (c *Checker) checkHeadings(html string, report *Report) {
	headingPattern := regexp.MustCompile(`<h([1-6])[^>]*>`)
	matches := headingPattern.FindAllStringSubmatch(html, -1)

	if len(matches) == 0 {
		report.Issues = append(report.Issues, Issue{
			Type:        IssueTypeHeading,
			Severity:    SeverityModerate,
			Description: "Page has no headings",
			Suggestion:  "Add heading structure for better navigation",
		})
		return
	}

	// Check for h1
	hasH1 := false
	for _, match := range matches {
		if match[1] == "1" {
			hasH1 = true
			break
		}
	}
	if !hasH1 {
		report.Issues = append(report.Issues, Issue{
			Type:        IssueTypeHeading,
			Severity:    SeveritySerious,
			Description: "Page has no h1 heading",
			Suggestion:  "Add a single h1 heading as the main page title",
		})
	}

	// Check heading order (shouldn't skip levels)
	prevLevel := 0
	for _, match := range matches {
		level := int(match[1][0] - '0')
		if prevLevel > 0 && level > prevLevel+1 {
			report.Issues = append(report.Issues, Issue{
				Type:        IssueTypeHeading,
				Severity:    SeverityModerate,
				Description: fmt.Sprintf("Heading level skipped from h%d to h%d", prevLevel, level),
				Suggestion:  "Maintain proper heading hierarchy without skipping levels",
			})
		}
		prevLevel = level
	}
}

// checkLandmarks verifies ARIA landmarks.
func (c *Checker) checkLandmarks(html string, report *Report) {
	landmarks := map[string]bool{
		"main":   false,
		"nav":    false,
		"header": false,
		"footer": false,
	}

	for landmark := range landmarks {
		pattern := regexp.MustCompile(fmt.Sprintf(`<%s[^>]*>|role\s*=\s*["']%s["']`, landmark, landmark))
		if pattern.MatchString(html) {
			landmarks[landmark] = true
		}
	}

	if !landmarks["main"] {
		report.Issues = append(report.Issues, Issue{
			Type:        IssueTypeSemantic,
			Severity:    SeverityModerate,
			Description: "Page missing main landmark",
			Suggestion:  "Add <main> element or role=\"main\"",
		})
	}

	// Count found landmarks as passed
	found := 0
	for _, present := range landmarks {
		if present {
			found++
		}
	}
	if found > 0 {
		report.Passed = append(report.Passed, fmt.Sprintf("%d landmark regions defined", found))
	}
}

// checkLanguage verifies language attribute.
func (c *Checker) checkLanguage(html string, report *Report) {
	langPattern := regexp.MustCompile(`<html[^>]*lang\s*=\s*["'][^"']+["']`)
	if !langPattern.MatchString(html) {
		report.Issues = append(report.Issues, Issue{
			Type:        IssueTypeSemantic,
			Severity:    SeveritySerious,
			Description: "HTML element missing lang attribute",
			Suggestion:  "Add lang attribute: <html lang=\"en\">",
		})
	} else {
		report.Passed = append(report.Passed, "Page has language defined")
	}
}

// CheckAPIAccessibility verifies API responses are accessible.
func (c *Checker) CheckAPIAccessibility(ctx context.Context, path string) (*Report, error) {
	report := &Report{
		URL:       c.BaseURL + path,
		Timestamp: time.Now(),
		Issues:    make([]Issue, 0),
		Passed:    make([]string, 0),
	}

	resp, err := c.get(ctx, path)
	if err != nil {
		return nil, fmt.Errorf("fetching API: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	// Check Content-Type header
	contentType := resp.Header.Get("Content-Type")
	if contentType == "" {
		report.Issues = append(report.Issues, Issue{
			Type:        IssueTypeAPI,
			Severity:    SeverityModerate,
			Description: "API response missing Content-Type header",
			Suggestion:  "Set appropriate Content-Type header",
		})
	} else {
		report.Passed = append(report.Passed, "Content-Type header present")
	}

	// Check for proper error format
	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(resp.Body)
		bodyStr := string(body)

		// Check if error response is structured
		if !strings.Contains(bodyStr, "error") && !strings.Contains(bodyStr, "message") {
			report.Issues = append(report.Issues, Issue{
				Type:        IssueTypeError,
				Severity:    SeverityModerate,
				Description: "Error response lacks structured error message",
				Suggestion:  "Return JSON with 'error' or 'message' field",
			})
		}
	}

	return report, nil
}

// get performs a GET request.
func (c *Checker) get(ctx context.Context, path string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+path, nil)
	if err != nil {
		return nil, err
	}
	return c.HTTPClient.Do(req)
}

// truncate shortens a string for display.
func truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen-3] + "..."
}

// GenerateReport creates a formatted accessibility report.
func (r *Report) GenerateReport() string {
	report := "Accessibility Report\n"
	report += "====================\n\n"
	report += fmt.Sprintf("URL: %s\n", r.URL)
	report += fmt.Sprintf("Checked: %s\n\n", r.Timestamp.Format(time.RFC3339))

	counts := r.CountBySeverity()
	report += "Summary:\n"
	report += fmt.Sprintf("  Critical: %d\n", counts[SeverityCritical])
	report += fmt.Sprintf("  Serious:  %d\n", counts[SeveritySerious])
	report += fmt.Sprintf("  Moderate: %d\n", counts[SeverityModerate])
	report += fmt.Sprintf("  Minor:    %d\n\n", counts[SeverityMinor])

	if len(r.Passed) > 0 {
		report += "Passed Checks:\n"
		for _, p := range r.Passed {
			report += fmt.Sprintf("  ✓ %s\n", p)
		}
		report += "\n"
	}

	if len(r.Issues) > 0 {
		report += "Issues:\n"
		for i, issue := range r.Issues {
			report += fmt.Sprintf("\n%d. [%s] %s\n", i+1, issue.Severity, issue.Description)
			report += fmt.Sprintf("   Type: %s\n", issue.Type)
			if issue.Element != "" {
				report += fmt.Sprintf("   Element: %s\n", issue.Element)
			}
			report += fmt.Sprintf("   Suggestion: %s\n", issue.Suggestion)
		}
	} else {
		report += "No issues found.\n"
	}

	return report
}

// AssertAccessible fails the test if critical issues exist.
func AssertAccessible(t *testing.T, report *Report) {
	t.Helper()
	if report.HasCritical() {
		t.Error("Accessibility check found critical issues")
		for _, issue := range report.Issues {
			if issue.Severity == SeverityCritical {
				t.Logf("  [%s] %s: %s", issue.Type, issue.Severity, issue.Description)
			}
		}
	}
}

// AssertNoIssues fails the test if any issues exist.
func AssertNoIssues(t *testing.T, report *Report) {
	t.Helper()
	if len(report.Issues) > 0 {
		t.Errorf("Accessibility check found %d issues", len(report.Issues))
		for _, issue := range report.Issues {
			t.Logf("  [%s] %s: %s", issue.Type, issue.Severity, issue.Description)
		}
	}
}
