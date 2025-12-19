package browser

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestParseUserAgent_Chrome(t *testing.T) {
	ua := "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36"
	parsed := ParseUserAgent(ua)

	if parsed.Browser != BrowserChrome {
		t.Errorf("expected Chrome, got %s", parsed.Browser)
	}
	if parsed.Version != "120.0.0.0" {
		t.Errorf("expected version 120.0.0.0, got %s", parsed.Version)
	}
	if parsed.OS != "Windows" {
		t.Errorf("expected Windows, got %s", parsed.OS)
	}
	if parsed.Mobile {
		t.Error("expected not mobile")
	}
}

func TestParseUserAgent_Firefox(t *testing.T) {
	ua := "Mozilla/5.0 (Windows NT 10.0; Win64; x64; rv:121.0) Gecko/20100101 Firefox/121.0"
	parsed := ParseUserAgent(ua)

	if parsed.Browser != BrowserFirefox {
		t.Errorf("expected Firefox, got %s", parsed.Browser)
	}
	if parsed.Version != "121.0" {
		t.Errorf("expected version 121.0, got %s", parsed.Version)
	}
}

func TestParseUserAgent_Safari(t *testing.T) {
	ua := "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.2 Safari/605.1.15"
	parsed := ParseUserAgent(ua)

	if parsed.Browser != BrowserSafari {
		t.Errorf("expected Safari, got %s", parsed.Browser)
	}
	if parsed.OS != "macOS" {
		t.Errorf("expected macOS, got %s", parsed.OS)
	}
}

func TestParseUserAgent_Edge(t *testing.T) {
	ua := "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36 Edg/120.0.0.0"
	parsed := ParseUserAgent(ua)

	if parsed.Browser != BrowserEdge {
		t.Errorf("expected Edge, got %s", parsed.Browser)
	}
}

func TestParseUserAgent_IE(t *testing.T) {
	ua := "Mozilla/5.0 (Windows NT 10.0; WOW64; Trident/7.0; rv:11.0) like Gecko"
	parsed := ParseUserAgent(ua)

	if parsed.Browser != BrowserIE {
		t.Errorf("expected IE, got %s", parsed.Browser)
	}
	if parsed.Version != "11.0" {
		t.Errorf("expected version 11.0, got %s", parsed.Version)
	}
}

func TestParseUserAgent_Mobile(t *testing.T) {
	ua := "Mozilla/5.0 (iPhone; CPU iPhone OS 17_2 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.2 Mobile/15E148 Safari/604.1"
	parsed := ParseUserAgent(ua)

	if !parsed.Mobile {
		t.Error("expected mobile")
	}
	if parsed.OS != "iOS" {
		t.Errorf("expected iOS, got %s", parsed.OS)
	}
}

func TestParseUserAgent_Bot(t *testing.T) {
	ua := "Mozilla/5.0 (compatible; Googlebot/2.1; +http://www.google.com/bot.html)"
	parsed := ParseUserAgent(ua)

	if parsed.Browser != BrowserBot {
		t.Errorf("expected Bot, got %s", parsed.Browser)
	}
	if !parsed.Bot {
		t.Error("expected Bot flag")
	}
}

func TestUserAgent_IsSupported(t *testing.T) {
	ua := &UserAgent{Browser: BrowserChrome}
	supported := []Browser{BrowserChrome, BrowserFirefox, BrowserSafari}

	if !ua.IsSupported(supported) {
		t.Error("Chrome should be supported")
	}

	ua.Browser = BrowserIE
	if ua.IsSupported(supported) {
		t.Error("IE should not be supported")
	}
}

func TestCommonUserAgents(t *testing.T) {
	agents := CommonUserAgents()

	if len(agents) == 0 {
		t.Error("expected user agents")
	}

	required := []string{"chrome_windows", "firefox", "safari", "edge", "mobile_chrome"}
	for _, name := range required {
		if _, ok := agents[name]; !ok {
			t.Errorf("missing user agent: %s", name)
		}
	}
}

func TestNewTester(t *testing.T) {
	tester := NewTester("http://localhost:8080/")

	if tester.BaseURL != "http://localhost:8080" {
		t.Errorf("expected trailing slash removed, got %s", tester.BaseURL)
	}
}

func TestTester_TestEndpoint(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte("<html><head><meta name='viewport'></head><body>Test</body></html>"))
	}))
	defer server.Close()

	tester := NewTester(server.URL)
	ctx := context.Background()

	result := tester.TestEndpoint(ctx, "/", CommonUserAgents()["chrome_windows"])

	if result.Error != nil {
		t.Fatalf("TestEndpoint failed: %v", result.Error)
	}
	if result.StatusCode != 200 {
		t.Errorf("expected status 200, got %d", result.StatusCode)
	}
	if result.Browser != BrowserChrome {
		t.Errorf("expected Chrome, got %s", result.Browser)
	}
	if result.BodySize == 0 {
		t.Error("expected non-zero body size")
	}
}

func TestTester_TestEndpoint_IE_Issues(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`
			<html>
			<style>.container { display: flex; }</style>
			<script>fetch('/api'); const x = () => {};</script>
			</html>
		`))
	}))
	defer server.Close()

	tester := NewTester(server.URL)
	ctx := context.Background()

	result := tester.TestEndpoint(ctx, "/", CommonUserAgents()["ie11"])

	if len(result.Issues) == 0 {
		t.Error("expected IE compatibility issues")
	}

	t.Logf("IE issues: %v", result.Issues)
}

func TestTester_TestAllBrowsers(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	tester := NewTester(server.URL)
	ctx := context.Background()

	results := tester.TestAllBrowsers(ctx, "/")

	if len(results) != len(CommonUserAgents()) {
		t.Errorf("expected %d results, got %d", len(CommonUserAgents()), len(results))
	}

	for name, result := range results {
		if result.Error != nil {
			t.Errorf("%s failed: %v", name, result.Error)
		}
	}
}

func TestCompatibilityReport_GenerateReport(t *testing.T) {
	report := &CompatibilityReport{
		URL:       "http://example.com",
		Timestamp: time.Now(),
		Results: map[string]*TestResult{
			"chrome": {
				UserAgent:  "Chrome UA",
				Browser:    BrowserChrome,
				StatusCode: 200,
				BodySize:   1000,
				Duration:   50 * time.Millisecond,
				Issues:     []string{},
			},
			"ie11": {
				UserAgent:  "IE11 UA",
				Browser:    BrowserIE,
				StatusCode: 200,
				BodySize:   1000,
				Duration:   100 * time.Millisecond,
				Issues:     []string{"Uses Flexbox"},
			},
		},
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

func TestCompatibilityReport_AllPassed(t *testing.T) {
	report := &CompatibilityReport{
		Results: map[string]*TestResult{
			"chrome":  {StatusCode: 200, Issues: []string{}},
			"firefox": {StatusCode: 200, Issues: []string{}},
		},
	}

	if !report.AllPassed() {
		t.Error("expected all passed")
	}

	report.Results["ie"] = &TestResult{StatusCode: 200, Issues: []string{"some issue"}}
	if report.AllPassed() {
		t.Error("expected not all passed")
	}
}

func TestCompatibilityReport_GetFailedBrowsers(t *testing.T) {
	report := &CompatibilityReport{
		Results: map[string]*TestResult{
			"chrome":  {StatusCode: 200, Issues: []string{}},
			"firefox": {StatusCode: 500, Issues: []string{}},
			"ie":      {StatusCode: 200, Issues: []string{"issue"}},
		},
	}

	failed := report.GetFailedBrowsers()

	if len(failed) != 2 {
		t.Errorf("expected 2 failed, got %d", len(failed))
	}
}

func TestNewFeatureDetector(t *testing.T) {
	fd := NewFeatureDetector()

	features := fd.GetFeatures()
	if len(features) == 0 {
		t.Error("expected features")
	}

	required := []string{"fetch", "flexbox", "grid", "es6"}
	for _, name := range required {
		if _, ok := features[name]; !ok {
			t.Errorf("missing feature: %s", name)
		}
	}
}

func TestFeatureDetector_CheckFeature(t *testing.T) {
	fd := NewFeatureDetector()

	// Chrome 120 should support fetch
	chrome := &UserAgent{Browser: BrowserChrome, Version: "120.0.0.0"}
	if !fd.CheckFeature("fetch", chrome) {
		t.Error("Chrome 120 should support fetch")
	}

	// IE should not support fetch
	ie := &UserAgent{Browser: BrowserIE, Version: "11.0"}
	if fd.CheckFeature("fetch", ie) {
		t.Error("IE should not support fetch")
	}

	// IE 11 should support flexbox
	if !fd.CheckFeature("flexbox", ie) {
		t.Error("IE 11 should support flexbox")
	}
}

func TestCompareVersions(t *testing.T) {
	tests := []struct {
		v1, v2   string
		expected int
	}{
		{"120", "42", 1},
		{"10", "42", -1},
		{"42", "42", 0},
		{"120.0.0", "42", 1},
	}

	for _, tt := range tests {
		result := compareVersions(tt.v1, tt.v2)
		if result != tt.expected {
			t.Errorf("compareVersions(%s, %s) = %d, expected %d",
				tt.v1, tt.v2, result, tt.expected)
		}
	}
}

func TestSupportedBrowsers(t *testing.T) {
	supported := SupportedBrowsers()

	if len(supported) == 0 {
		t.Error("expected supported browsers")
	}

	// Should include major browsers
	hasChrome := false
	hasFirefox := false
	for _, b := range supported {
		if b == BrowserChrome {
			hasChrome = true
		}
		if b == BrowserFirefox {
			hasFirefox = true
		}
	}

	if !hasChrome || !hasFirefox {
		t.Error("should include Chrome and Firefox")
	}
}

func TestBrowser(t *testing.T) {
	browsers := []Browser{
		BrowserChrome,
		BrowserFirefox,
		BrowserSafari,
		BrowserEdge,
		BrowserIE,
		BrowserOpera,
		BrowserMobile,
		BrowserBot,
		BrowserUnknown,
	}

	for _, b := range browsers {
		if b == "" {
			t.Error("browser constant should not be empty")
		}
	}
}

func TestTestResult(t *testing.T) {
	result := TestResult{
		UserAgent:   "test",
		Browser:     BrowserChrome,
		StatusCode:  200,
		ContentType: "text/html",
		BodySize:    1000,
		Duration:    50 * time.Millisecond,
		Issues:      []string{"issue1"},
	}

	if result.StatusCode != 200 {
		t.Error("StatusCode not set")
	}
	if len(result.Issues) != 1 {
		t.Error("Issues not set")
	}
}
