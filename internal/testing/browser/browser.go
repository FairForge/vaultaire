// Package browser provides utilities for browser compatibility testing.
package browser

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"
)

// Browser represents a browser type.
type Browser string

const (
	BrowserChrome  Browser = "chrome"
	BrowserFirefox Browser = "firefox"
	BrowserSafari  Browser = "safari"
	BrowserEdge    Browser = "edge"
	BrowserIE      Browser = "ie"
	BrowserOpera   Browser = "opera"
	BrowserMobile  Browser = "mobile"
	BrowserBot     Browser = "bot"
	BrowserUnknown Browser = "unknown"
)

// UserAgent contains parsed user agent information.
type UserAgent struct {
	Raw       string
	Browser   Browser
	Version   string
	OS        string
	OSVersion string
	Mobile    bool
	Bot       bool
}

// ParseUserAgent extracts browser information from a user agent string.
func ParseUserAgent(ua string) *UserAgent {
	parsed := &UserAgent{
		Raw:     ua,
		Browser: BrowserUnknown,
	}

	uaLower := strings.ToLower(ua)

	// Check for bots first
	botPatterns := []string{"bot", "crawler", "spider", "curl", "wget", "python", "java", "go-http"}
	for _, pattern := range botPatterns {
		if strings.Contains(uaLower, pattern) {
			parsed.Browser = BrowserBot
			parsed.Bot = true
			return parsed
		}
	}

	// Check mobile
	mobilePatterns := []string{"mobile", "android", "iphone", "ipad", "ipod"}
	for _, pattern := range mobilePatterns {
		if strings.Contains(uaLower, pattern) {
			parsed.Mobile = true
			break
		}
	}

	// Parse browser - order matters (Edge contains Chrome, etc.)
	switch {
	case strings.Contains(uaLower, "edg"):
		parsed.Browser = BrowserEdge
		parsed.Version = extractVersion(ua, `Edg[e]?/(\d+[\d.]*)`)
	case strings.Contains(uaLower, "opr") || strings.Contains(uaLower, "opera"):
		parsed.Browser = BrowserOpera
		parsed.Version = extractVersion(ua, `(?:OPR|Opera)[/ ](\d+[\d.]*)`)
	case strings.Contains(uaLower, "firefox"):
		parsed.Browser = BrowserFirefox
		parsed.Version = extractVersion(ua, `Firefox/(\d+[\d.]*)`)
	case strings.Contains(uaLower, "safari") && !strings.Contains(uaLower, "chrome"):
		parsed.Browser = BrowserSafari
		parsed.Version = extractVersion(ua, `Version/(\d+[\d.]*)`)
	case strings.Contains(uaLower, "chrome"):
		parsed.Browser = BrowserChrome
		parsed.Version = extractVersion(ua, `Chrome/(\d+[\d.]*)`)
	case strings.Contains(uaLower, "msie") || strings.Contains(uaLower, "trident"):
		parsed.Browser = BrowserIE
		parsed.Version = extractVersion(ua, `(?:MSIE |rv:)(\d+[\d.]*)`)
	}

	// Override for mobile
	if parsed.Mobile && parsed.Browser == BrowserUnknown {
		parsed.Browser = BrowserMobile
	}

	// Parse OS - order matters (iOS contains Mac OS X in UA)
	switch {
	case strings.Contains(uaLower, "iphone") || strings.Contains(uaLower, "ipad"):
		parsed.OS = "iOS"
		parsed.OSVersion = extractVersion(ua, `OS (\d+[_\d.]*)`)
	case strings.Contains(uaLower, "android"):
		parsed.OS = "Android"
		parsed.OSVersion = extractVersion(ua, `Android (\d+[\d.]*)`)
	case strings.Contains(uaLower, "mac os"):
		parsed.OS = "macOS"
		parsed.OSVersion = extractVersion(ua, `Mac OS X (\d+[_\d.]*)`)
	case strings.Contains(uaLower, "windows"):
		parsed.OS = "Windows"
		parsed.OSVersion = extractVersion(ua, `Windows NT (\d+[\d.]*)`)
	case strings.Contains(uaLower, "linux"):
		parsed.OS = "Linux"
	}

	return parsed
}

// extractVersion extracts version number using regex.
func extractVersion(ua, pattern string) string {
	re := regexp.MustCompile(pattern)
	matches := re.FindStringSubmatch(ua)
	if len(matches) >= 2 {
		return strings.ReplaceAll(matches[1], "_", ".")
	}
	return ""
}

// IsSupported checks if the browser is in the supported list.
func (ua *UserAgent) IsSupported(supported []Browser) bool {
	for _, b := range supported {
		if ua.Browser == b {
			return true
		}
	}
	return false
}

// CommonUserAgents returns a map of common user agents for testing.
func CommonUserAgents() map[string]string {
	return map[string]string{
		"chrome_windows": "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36",
		"chrome_mac":     "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36",
		"firefox":        "Mozilla/5.0 (Windows NT 10.0; Win64; x64; rv:121.0) Gecko/20100101 Firefox/121.0",
		"safari":         "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.2 Safari/605.1.15",
		"edge":           "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36 Edg/120.0.0.0",
		"ie11":           "Mozilla/5.0 (Windows NT 10.0; WOW64; Trident/7.0; rv:11.0) like Gecko",
		"mobile_chrome":  "Mozilla/5.0 (Linux; Android 10; SM-G981B) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Mobile Safari/537.36",
		"mobile_safari":  "Mozilla/5.0 (iPhone; CPU iPhone OS 17_2 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.2 Mobile/15E148 Safari/604.1",
		"googlebot":      "Mozilla/5.0 (compatible; Googlebot/2.1; +http://www.google.com/bot.html)",
		"curl":           "curl/8.4.0",
	}
}

// Tester performs browser compatibility testing.
type Tester struct {
	BaseURL    string
	HTTPClient *http.Client
	Timeout    time.Duration
}

// NewTester creates a browser compatibility tester.
func NewTester(baseURL string) *Tester {
	return &Tester{
		BaseURL: strings.TrimSuffix(baseURL, "/"),
		HTTPClient: &http.Client{
			Timeout: 30 * time.Second,
		},
		Timeout: 30 * time.Second,
	}
}

// TestResult contains the result of a browser compatibility test.
type TestResult struct {
	UserAgent   string
	Browser     Browser
	StatusCode  int
	ContentType string
	BodySize    int
	Duration    time.Duration
	Error       error
	Issues      []string
}

// TestEndpoint tests an endpoint with a specific user agent.
func (t *Tester) TestEndpoint(ctx context.Context, path, userAgent string) *TestResult {
	result := &TestResult{
		UserAgent: userAgent,
		Issues:    make([]string, 0),
	}

	// Parse user agent
	parsed := ParseUserAgent(userAgent)
	result.Browser = parsed.Browser

	start := time.Now()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, t.BaseURL+path, nil)
	if err != nil {
		result.Error = err
		return result
	}

	req.Header.Set("User-Agent", userAgent)

	resp, err := t.HTTPClient.Do(req)
	if err != nil {
		result.Error = err
		return result
	}
	defer func() { _ = resp.Body.Close() }()

	result.Duration = time.Since(start)
	result.StatusCode = resp.StatusCode
	result.ContentType = resp.Header.Get("Content-Type")

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		result.Error = err
		return result
	}
	result.BodySize = len(body)

	// Check for browser-specific issues
	t.checkIssues(result, string(body), parsed)

	return result
}

// checkIssues analyzes response for browser compatibility issues.
func (t *Tester) checkIssues(result *TestResult, body string, ua *UserAgent) {
	// Check for IE-specific issues
	if ua.Browser == BrowserIE {
		if strings.Contains(body, "flex") || strings.Contains(body, "grid") {
			result.Issues = append(result.Issues, "Uses CSS Flexbox/Grid which has limited IE11 support")
		}
		if strings.Contains(body, "fetch(") {
			result.Issues = append(result.Issues, "Uses Fetch API which is not supported in IE")
		}
		if strings.Contains(body, "=>") {
			result.Issues = append(result.Issues, "Uses arrow functions which are not supported in IE")
		}
	}

	// Check for mobile-specific issues
	if ua.Mobile {
		if !strings.Contains(body, "viewport") {
			result.Issues = append(result.Issues, "Missing viewport meta tag for mobile")
		}
	}

	// Check for general issues
	if result.StatusCode >= 400 {
		result.Issues = append(result.Issues, fmt.Sprintf("HTTP error: %d", result.StatusCode))
	}
}

// TestAllBrowsers tests an endpoint with all common user agents.
func (t *Tester) TestAllBrowsers(ctx context.Context, path string) map[string]*TestResult {
	results := make(map[string]*TestResult)

	for name, ua := range CommonUserAgents() {
		results[name] = t.TestEndpoint(ctx, path, ua)
	}

	return results
}

// CompatibilityReport contains a full compatibility test report.
type CompatibilityReport struct {
	URL       string
	Timestamp time.Time
	Results   map[string]*TestResult
}

// GenerateReport creates a formatted compatibility report.
func (r *CompatibilityReport) GenerateReport() string {
	report := "Browser Compatibility Report\n"
	report += "============================\n\n"
	report += fmt.Sprintf("URL: %s\n", r.URL)
	report += fmt.Sprintf("Tested: %s\n\n", r.Timestamp.Format(time.RFC3339))

	// Summary
	passed := 0
	failed := 0
	for _, result := range r.Results {
		if result.Error == nil && result.StatusCode < 400 && len(result.Issues) == 0 {
			passed++
		} else {
			failed++
		}
	}
	report += fmt.Sprintf("Summary: %d passed, %d failed\n\n", passed, failed)

	// Details
	report += "Results:\n"
	report += "--------\n"

	for name, result := range r.Results {
		status := "✓"
		if result.Error != nil || result.StatusCode >= 400 || len(result.Issues) > 0 {
			status = "✗"
		}
		report += fmt.Sprintf("\n%s %s (%s)\n", status, name, result.Browser)
		report += fmt.Sprintf("  Status: %d, Size: %d bytes, Time: %v\n",
			result.StatusCode, result.BodySize, result.Duration)

		if result.Error != nil {
			report += fmt.Sprintf("  Error: %v\n", result.Error)
		}
		for _, issue := range result.Issues {
			report += fmt.Sprintf("  Issue: %s\n", issue)
		}
	}

	return report
}

// AllPassed returns true if all tests passed.
func (r *CompatibilityReport) AllPassed() bool {
	for _, result := range r.Results {
		if result.Error != nil || result.StatusCode >= 400 || len(result.Issues) > 0 {
			return false
		}
	}
	return true
}

// GetFailedBrowsers returns browsers that failed.
func (r *CompatibilityReport) GetFailedBrowsers() []string {
	failed := make([]string, 0)
	for name, result := range r.Results {
		if result.Error != nil || result.StatusCode >= 400 || len(result.Issues) > 0 {
			failed = append(failed, name)
		}
	}
	return failed
}

// FeatureDetector checks for browser feature support.
type FeatureDetector struct {
	features map[string]FeatureSupport
}

// FeatureSupport describes browser support for a feature.
type FeatureSupport struct {
	Name        string
	Chrome      string // Minimum version
	Firefox     string
	Safari      string
	Edge        string
	IE          string // empty means not supported
	Description string
}

// NewFeatureDetector creates a feature detector with common features.
func NewFeatureDetector() *FeatureDetector {
	return &FeatureDetector{
		features: map[string]FeatureSupport{
			"fetch": {
				Name:        "Fetch API",
				Chrome:      "42",
				Firefox:     "39",
				Safari:      "10.1",
				Edge:        "14",
				IE:          "",
				Description: "Modern HTTP request API",
			},
			"flexbox": {
				Name:        "CSS Flexbox",
				Chrome:      "29",
				Firefox:     "28",
				Safari:      "9",
				Edge:        "12",
				IE:          "11",
				Description: "Flexible box layout",
			},
			"grid": {
				Name:        "CSS Grid",
				Chrome:      "57",
				Firefox:     "52",
				Safari:      "10.1",
				Edge:        "16",
				IE:          "",
				Description: "Grid layout system",
			},
			"es6": {
				Name:        "ES6/ES2015",
				Chrome:      "51",
				Firefox:     "54",
				Safari:      "10",
				Edge:        "15",
				IE:          "",
				Description: "Modern JavaScript features",
			},
			"webp": {
				Name:        "WebP Images",
				Chrome:      "32",
				Firefox:     "65",
				Safari:      "14",
				Edge:        "18",
				IE:          "",
				Description: "WebP image format support",
			},
			"serviceworker": {
				Name:        "Service Workers",
				Chrome:      "40",
				Firefox:     "44",
				Safari:      "11.1",
				Edge:        "17",
				IE:          "",
				Description: "Offline and background sync",
			},
		},
	}
}

// CheckFeature checks if a browser supports a feature.
func (fd *FeatureDetector) CheckFeature(feature string, ua *UserAgent) bool {
	support, ok := fd.features[feature]
	if !ok {
		return false // Unknown feature
	}

	var minVersion string
	switch ua.Browser {
	case BrowserChrome:
		minVersion = support.Chrome
	case BrowserFirefox:
		minVersion = support.Firefox
	case BrowserSafari:
		minVersion = support.Safari
	case BrowserEdge:
		minVersion = support.Edge
	case BrowserIE:
		minVersion = support.IE
	default:
		return true // Assume supported for unknown browsers
	}

	if minVersion == "" {
		return false // Not supported
	}

	// Simple version comparison (first number only)
	return compareVersions(ua.Version, minVersion) >= 0
}

// compareVersions compares two version strings.
func compareVersions(v1, v2 string) int {
	// Extract first number
	v1Parts := strings.Split(v1, ".")
	v2Parts := strings.Split(v2, ".")

	if len(v1Parts) == 0 || len(v2Parts) == 0 {
		return 0
	}

	var n1, n2 int
	_, _ = fmt.Sscanf(v1Parts[0], "%d", &n1)
	_, _ = fmt.Sscanf(v2Parts[0], "%d", &n2)

	if n1 < n2 {
		return -1
	}
	if n1 > n2 {
		return 1
	}
	return 0
}

// GetFeatures returns all known features.
func (fd *FeatureDetector) GetFeatures() map[string]FeatureSupport {
	features := make(map[string]FeatureSupport)
	for k, v := range fd.features {
		features[k] = v
	}
	return features
}

// SupportedBrowsers returns the default list of supported browsers.
func SupportedBrowsers() []Browser {
	return []Browser{
		BrowserChrome,
		BrowserFirefox,
		BrowserSafari,
		BrowserEdge,
	}
}
