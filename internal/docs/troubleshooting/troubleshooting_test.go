package troubleshooting

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

func TestNewGuide(t *testing.T) {
	guide := NewGuide("conn-issues", "Connection Issues").
		Description("Troubleshoot connection problems").
		Category(CategoryConnection).
		Build()

	if guide.ID != "conn-issues" {
		t.Errorf("expected ID 'conn-issues', got %s", guide.ID)
	}
	if guide.Category != CategoryConnection {
		t.Error("Category not set")
	}
}

func TestGuideBuilder_Problem(t *testing.T) {
	problem := NewProblem("timeout", "Connection Timeout").
		Symptom("Request times out after 30 seconds").
		Severity(SeverityHigh).
		Build()

	guide := NewGuide("test", "Test").
		Problem(problem).
		Build()

	if len(guide.Problems) != 1 {
		t.Error("Problem not added")
	}
}

func TestNewProblem(t *testing.T) {
	problem := NewProblem("slow-upload", "Slow Upload Speed").
		Symptom("Uploads take longer than expected").
		Symptom("Progress bar moves slowly").
		Cause("Network congestion", LikelihoodHigh, "ping -c 10 api.stored.ge").
		Cause("Large file size", LikelihoodMedium, "ls -lh file").
		Prevention("Use multipart upload for large files").
		Related("timeout").
		Severity(SeverityMedium).
		Build()

	if problem.ID != "slow-upload" {
		t.Error("ID not set")
	}
	if len(problem.Symptoms) != 2 {
		t.Errorf("expected 2 symptoms, got %d", len(problem.Symptoms))
	}
	if len(problem.Causes) != 2 {
		t.Errorf("expected 2 causes, got %d", len(problem.Causes))
	}
	if len(problem.Prevention) != 1 {
		t.Error("Prevention not set")
	}
	if len(problem.RelatedIDs) != 1 {
		t.Error("Related not set")
	}
}

func TestNewSolution(t *testing.T) {
	solution := NewSolution("Increase timeout").
		Description("Increase the client timeout setting").
		Duration(5*time.Minute).
		Difficulty(DifficultyEasy).
		ForCause("Network latency").
		Step("Open config", "vim ~/.stored/config.yaml", "File opens").
		StepWithWarning("Update timeout", "timeout: 120", "Config updated", "Restart required").
		Build()

	if solution.Title != "Increase timeout" {
		t.Error("Title not set")
	}
	if solution.Duration != 5*time.Minute {
		t.Error("Duration not set")
	}
	if solution.Difficulty != DifficultyEasy {
		t.Error("Difficulty not set")
	}
	if solution.ForCause != "Network latency" {
		t.Error("ForCause not set")
	}
	if len(solution.Steps) != 2 {
		t.Errorf("expected 2 steps, got %d", len(solution.Steps))
	}
	if solution.Steps[1].Warning == "" {
		t.Error("Warning not set on step")
	}
}

func TestErrorCodeRegistry(t *testing.T) {
	registry := NewErrorCodeRegistry()

	registry.Register(&ErrorCode{
		Code:        "E001",
		Name:        "InvalidBucket",
		Description: "The specified bucket does not exist",
		Causes:      []string{"Typo in bucket name", "Bucket was deleted"},
		Solutions:   []string{"Check bucket name", "Create the bucket"},
	})

	registry.Register(&ErrorCode{
		Code:        "E002",
		Name:        "AccessDenied",
		Description: "Access to the resource was denied",
	})

	// Get
	code, ok := registry.Get("E001")
	if !ok {
		t.Error("Code not found")
	}
	if code.Name != "InvalidBucket" {
		t.Error("Wrong code returned")
	}

	// All
	all := registry.All()
	if len(all) != 2 {
		t.Errorf("expected 2 codes, got %d", len(all))
	}

	// Search
	results := registry.Search("bucket")
	if len(results) != 1 {
		t.Errorf("expected 1 result, got %d", len(results))
	}

	results = registry.Search("denied")
	if len(results) != 1 {
		t.Error("Search by description failed")
	}
}

func TestDecisionTree(t *testing.T) {
	// Build a simple decision tree
	tree := NewDecisionTree(&DecisionNode{
		Question: "Is the service responding?",
		YesNode: &DecisionNode{
			Question: "Are credentials correct?",
			YesNode: &DecisionNode{
				Solution: "Check network firewall",
			},
			NoNode: &DecisionNode{
				Solution:  "Regenerate credentials",
				ProblemID: "auth-failed",
			},
		},
		NoNode: &DecisionNode{
			Solution: "Check if service is running",
		},
	})

	// Test traversal
	// Yes, Yes -> firewall
	node, err := tree.Traverse([]bool{true, true})
	if err != nil {
		t.Fatalf("Traverse failed: %v", err)
	}
	if node.Solution != "Check network firewall" {
		t.Errorf("expected firewall solution, got %s", node.Solution)
	}

	// Yes, No -> credentials
	node, err = tree.Traverse([]bool{true, false})
	if err != nil {
		t.Fatalf("Traverse failed: %v", err)
	}
	if node.ProblemID != "auth-failed" {
		t.Error("expected auth-failed problem ID")
	}

	// No -> service not running
	node, err = tree.Traverse([]bool{false})
	if err != nil {
		t.Fatalf("Traverse failed: %v", err)
	}
	if node.Solution != "Check if service is running" {
		t.Error("expected service solution")
	}
}

func TestDecisionTree_EmptyTree(t *testing.T) {
	tree := NewDecisionTree(nil)
	_, err := tree.Traverse([]bool{true})
	if err == nil {
		t.Error("expected error for empty tree")
	}
}

func TestTroubleshootingRenderer_RenderGuide(t *testing.T) {
	solution := NewSolution("Restart service").
		Description("Restart the storage service").
		Duration(2*time.Minute).
		Difficulty(DifficultyEasy).
		Step("Stop service", "systemctl stop vaultaire", "Service stopped").
		Step("Start service", "systemctl start vaultaire", "Service started").
		Build()

	problem := NewProblem("service-down", "Service Not Responding").
		Symptom("API returns 503 errors").
		Symptom("Health check fails").
		Cause("Service crashed", LikelihoodHigh, "systemctl status vaultaire").
		Cause("Port conflict", LikelihoodLow, "netstat -tlnp | grep 8000").
		Solution(solution).
		Prevention("Set up monitoring alerts").
		Severity(SeverityCritical).
		Build()

	guide := NewGuide("service-issues", "Service Troubleshooting").
		Description("Troubleshoot service problems").
		Category(CategoryConnection).
		Problem(problem).
		Build()

	renderer := NewTroubleshootingRenderer()
	var buf bytes.Buffer

	err := renderer.RenderGuide(&buf, guide)
	if err != nil {
		t.Fatalf("RenderGuide failed: %v", err)
	}

	output := buf.String()

	if !strings.Contains(output, "# Service Troubleshooting") {
		t.Error("Title not in output")
	}
	if !strings.Contains(output, "### Symptoms") {
		t.Error("Symptoms section not in output")
	}
	if !strings.Contains(output, "### Possible Causes") {
		t.Error("Causes section not in output")
	}
	if !strings.Contains(output, "### Solutions") {
		t.Error("Solutions section not in output")
	}
	if !strings.Contains(output, "🔴") {
		t.Error("Severity icon not in output")
	}

	t.Logf("Guide:\n%s", output)
}

func TestTroubleshootingRenderer_RenderErrorCodes(t *testing.T) {
	registry := NewErrorCodeRegistry()
	registry.Register(&ErrorCode{
		Code:        "E001",
		Name:        "NoSuchBucket",
		Description: "The specified bucket does not exist",
		Causes:      []string{"Bucket name is incorrect"},
		Solutions:   []string{"Verify bucket name"},
		Example:     "<Error><Code>NoSuchBucket</Code></Error>",
	})

	renderer := NewTroubleshootingRenderer()
	var buf bytes.Buffer

	err := renderer.RenderErrorCodes(&buf, registry)
	if err != nil {
		t.Fatalf("RenderErrorCodes failed: %v", err)
	}

	output := buf.String()

	if !strings.Contains(output, "# Error Codes Reference") {
		t.Error("Title not in output")
	}
	if !strings.Contains(output, "E001") {
		t.Error("Error code not in output")
	}
	if !strings.Contains(output, "**Causes:**") {
		t.Error("Causes not in output")
	}

	t.Logf("Error Codes:\n%s", output)
}

func TestSeverityIcon(t *testing.T) {
	tests := []struct {
		severity Severity
		contains string
	}{
		{SeverityLow, "🟢"},
		{SeverityMedium, "🟡"},
		{SeverityHigh, "🟠"},
		{SeverityCritical, "🔴"},
	}

	for _, tt := range tests {
		result := severityIcon(tt.severity)
		if result != tt.contains {
			t.Errorf("severityIcon(%s) = %s, expected %s", tt.severity, result, tt.contains)
		}
	}
}

func TestLikelihoodBadge(t *testing.T) {
	tests := []struct {
		likelihood Likelihood
		contains   string
	}{
		{LikelihoodHigh, "Most likely"},
		{LikelihoodMedium, "Possible"},
		{LikelihoodLow, "Unlikely"},
	}

	for _, tt := range tests {
		result := likelihoodBadge(tt.likelihood)
		if !strings.Contains(result, tt.contains) {
			t.Errorf("likelihoodBadge(%s) = %s, expected to contain %s", tt.likelihood, result, tt.contains)
		}
	}
}

func TestDifficultyBadge(t *testing.T) {
	tests := []struct {
		difficulty Difficulty
		contains   string
	}{
		{DifficultyEasy, "Easy"},
		{DifficultyModerate, "Moderate"},
		{DifficultyAdvanced, "Advanced"},
		{DifficultyExpert, "Expert"},
	}

	for _, tt := range tests {
		result := difficultyBadge(tt.difficulty)
		if !strings.Contains(result, tt.contains) {
			t.Errorf("difficultyBadge(%s) = %s, expected to contain %s", tt.difficulty, result, tt.contains)
		}
	}
}

func TestCategory(t *testing.T) {
	categories := []Category{
		CategoryConnection,
		CategoryPerformance,
		CategoryAuth,
		CategoryStorage,
		CategoryNetwork,
		CategoryConfiguration,
		CategoryData,
		CategoryAPI,
	}

	for _, cat := range categories {
		if cat == "" {
			t.Error("category should not be empty")
		}
	}
}

func TestCause(t *testing.T) {
	cause := Cause{
		Description: "Test cause",
		Likelihood:  LikelihoodHigh,
		Diagnostic:  "test command",
	}

	if cause.Diagnostic != "test command" {
		t.Error("Diagnostic not set")
	}
}

func TestSolutionStep(t *testing.T) {
	step := SolutionStep{
		Action:   "Do something",
		Command:  "cmd",
		Expected: "result",
		Warning:  "be careful",
	}

	if step.Warning != "be careful" {
		t.Error("Warning not set")
	}
}

func TestErrorCode(t *testing.T) {
	ec := ErrorCode{
		Code:        "E999",
		Name:        "TestError",
		Description: "A test error",
		Causes:      []string{"cause1"},
		Solutions:   []string{"solution1"},
		Example:     "example",
		SeeAlso:     []string{"E001"},
	}

	if len(ec.SeeAlso) != 1 {
		t.Error("SeeAlso not set")
	}
}
