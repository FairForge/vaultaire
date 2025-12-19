package uat

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestNewRunner(t *testing.T) {
	runner := NewRunner()

	if runner.features == nil {
		t.Error("expected features slice")
	}
	if runner.results == nil {
		t.Error("expected results slice")
	}
}

func TestRunner_AddFeature(t *testing.T) {
	runner := NewRunner()

	feature := Feature{
		Name:        "Authentication",
		Description: "User authentication features",
	}

	runner.AddFeature(feature)

	if len(runner.features) != 1 {
		t.Errorf("expected 1 feature, got %d", len(runner.features))
	}
}

func TestRunner_Run(t *testing.T) {
	runner := NewRunner()

	feature := Feature{
		Name:        "Login",
		Description: "User login functionality",
		Stories: []UserStory{
			{
				ID:     "US-001",
				Title:  "User can log in",
				AsA:    "registered user",
				IWant:  "to log in with my credentials",
				SoThat: "I can access my account",
				Criteria: []AcceptanceCriterion{
					{
						ID:    "AC-001",
						Given: "I am on the login page",
						When:  "I enter valid credentials",
						Then:  "I am redirected to the dashboard",
						Verify: func(ctx context.Context) error {
							return nil // Simulates passing test
						},
					},
					{
						ID:    "AC-002",
						Given: "I am on the login page",
						When:  "I enter invalid credentials",
						Then:  "I see an error message",
						Verify: func(ctx context.Context) error {
							return nil
						},
					},
				},
			},
		},
	}

	runner.AddFeature(feature)

	ctx := context.Background()
	err := runner.Run(ctx)
	if err != nil {
		t.Fatalf("Run failed: %v", err)
	}

	results := runner.Results()
	if len(results) != 1 {
		t.Errorf("expected 1 result, got %d", len(results))
	}

	if results[0].PassedCount != 1 {
		t.Errorf("expected 1 passed story, got %d", results[0].PassedCount)
	}
}

func TestRunner_Run_WithFailure(t *testing.T) {
	runner := NewRunner()

	feature := Feature{
		Name: "Failing Feature",
		Stories: []UserStory{
			{
				ID:    "US-002",
				Title: "Failing story",
				Criteria: []AcceptanceCriterion{
					{
						ID: "AC-003",
						Verify: func(ctx context.Context) error {
							return errors.New("test failure")
						},
					},
				},
			},
		},
	}

	runner.AddFeature(feature)

	ctx := context.Background()
	_ = runner.Run(ctx)

	results := runner.Results()
	if results[0].FailedCount != 1 {
		t.Errorf("expected 1 failed story, got %d", results[0].FailedCount)
	}

	if results[0].StoryResults[0].Status != StatusFailed {
		t.Errorf("expected failed status, got %s", results[0].StoryResults[0].Status)
	}
}

func TestRunner_Run_SkippedCriterion(t *testing.T) {
	runner := NewRunner()

	feature := Feature{
		Name: "Skipped Feature",
		Stories: []UserStory{
			{
				ID: "US-003",
				Criteria: []AcceptanceCriterion{
					{
						ID:     "AC-004",
						Verify: nil, // No verify function = skipped
					},
				},
			},
		},
	}

	runner.AddFeature(feature)

	ctx := context.Background()
	_ = runner.Run(ctx)

	results := runner.Results()
	cr := results[0].StoryResults[0].CriteriaResults[0]
	if cr.Status != StatusSkipped {
		t.Errorf("expected skipped status, got %s", cr.Status)
	}
}

func TestRunner_Summary(t *testing.T) {
	runner := NewRunner()

	runner.AddFeature(Feature{
		Name: "Feature 1",
		Stories: []UserStory{
			{ID: "US-001", Criteria: []AcceptanceCriterion{{ID: "AC-001", Verify: func(ctx context.Context) error { return nil }}}},
			{ID: "US-002", Criteria: []AcceptanceCriterion{{ID: "AC-002", Verify: func(ctx context.Context) error { return errors.New("fail") }}}},
		},
	})

	ctx := context.Background()
	_ = runner.Run(ctx)

	summary := runner.Summary()

	if summary.Features != 1 {
		t.Errorf("expected 1 feature, got %d", summary.Features)
	}
	if summary.Stories != 2 {
		t.Errorf("expected 2 stories, got %d", summary.Stories)
	}
	if summary.Passed != 1 {
		t.Errorf("expected 1 passed, got %d", summary.Passed)
	}
	if summary.Failed != 1 {
		t.Errorf("expected 1 failed, got %d", summary.Failed)
	}
}

func TestSummary_PassRate(t *testing.T) {
	summary := Summary{Passed: 8, Failed: 2}

	rate := summary.PassRate()
	if rate != 80.0 {
		t.Errorf("expected 80%%, got %.1f%%", rate)
	}

	// Zero case
	empty := Summary{}
	if empty.PassRate() != 0 {
		t.Error("expected 0 for empty summary")
	}
}

func TestRunner_GenerateReport(t *testing.T) {
	runner := NewRunner()

	runner.AddFeature(Feature{
		Name:        "User Management",
		Description: "Manage user accounts",
		Stories: []UserStory{
			{
				ID:     "US-001",
				Title:  "Create account",
				AsA:    "visitor",
				IWant:  "to create an account",
				SoThat: "I can use the service",
				Criteria: []AcceptanceCriterion{
					{
						ID:     "AC-001",
						Given:  "I am on signup page",
						When:   "I submit valid details",
						Then:   "My account is created",
						Verify: func(ctx context.Context) error { return nil },
					},
				},
			},
		},
	})

	ctx := context.Background()
	_ = runner.Run(ctx)

	report := runner.GenerateReport()

	if report == "" {
		t.Error("expected non-empty report")
	}
	if len(report) < 200 {
		t.Error("report seems too short")
	}

	t.Logf("Report:\n%s", report)
}

func TestStatusIcon(t *testing.T) {
	tests := []struct {
		status   Status
		expected string
	}{
		{StatusPassed, "✓"},
		{StatusFailed, "✗"},
		{StatusSkipped, "○"},
		{StatusBlocked, "⊘"},
		{StatusPending, "?"},
	}

	for _, tt := range tests {
		result := statusIcon(tt.status)
		if result != tt.expected {
			t.Errorf("statusIcon(%s) = %s, expected %s", tt.status, result, tt.expected)
		}
	}
}

func TestNewChecklist(t *testing.T) {
	cl := NewChecklist("Login Tests")

	if cl.Name != "Login Tests" {
		t.Error("Name not set")
	}
	if cl.Items == nil {
		t.Error("Items not initialized")
	}
}

func TestChecklist_AddItem(t *testing.T) {
	cl := NewChecklist("Test")

	cl.AddItem(ChecklistItem{
		ID:          "CL-001",
		Description: "Verify login",
		Steps:       []string{"Go to login", "Enter credentials", "Click login"},
		Expected:    "User is logged in",
	})

	if len(cl.Items) != 1 {
		t.Errorf("expected 1 item, got %d", len(cl.Items))
	}
	if cl.Items[0].Status != StatusPending {
		t.Error("expected pending status")
	}
}

func TestChecklist_MarkPassed(t *testing.T) {
	cl := NewChecklist("Test")
	cl.AddItem(ChecklistItem{ID: "CL-001", Description: "Test item"})

	cl.MarkPassed("CL-001", "Works correctly", "tester1")

	if cl.Items[0].Status != StatusPassed {
		t.Error("expected passed status")
	}
	if cl.Items[0].Actual != "Works correctly" {
		t.Error("Actual not set")
	}
	if cl.Items[0].TestedBy != "tester1" {
		t.Error("TestedBy not set")
	}
}

func TestChecklist_MarkFailed(t *testing.T) {
	cl := NewChecklist("Test")
	cl.AddItem(ChecklistItem{ID: "CL-001", Description: "Test item"})

	cl.MarkFailed("CL-001", "Error occurred", "Need investigation", "tester2")

	if cl.Items[0].Status != StatusFailed {
		t.Error("expected failed status")
	}
	if cl.Items[0].Notes != "Need investigation" {
		t.Error("Notes not set")
	}
}

func TestChecklist_Progress(t *testing.T) {
	cl := NewChecklist("Test")
	cl.AddItem(ChecklistItem{ID: "CL-001"})
	cl.AddItem(ChecklistItem{ID: "CL-002"})
	cl.AddItem(ChecklistItem{ID: "CL-003"})

	completed, total := cl.Progress()
	if completed != 0 || total != 3 {
		t.Errorf("expected 0/3, got %d/%d", completed, total)
	}

	cl.MarkPassed("CL-001", "", "")
	cl.MarkFailed("CL-002", "", "", "")

	completed, total = cl.Progress()
	if completed != 2 || total != 3 {
		t.Errorf("expected 2/3, got %d/%d", completed, total)
	}
}

func TestChecklist_AllPassed(t *testing.T) {
	cl := NewChecklist("Test")
	cl.AddItem(ChecklistItem{ID: "CL-001"})
	cl.AddItem(ChecklistItem{ID: "CL-002"})

	if cl.AllPassed() {
		t.Error("should not be all passed")
	}

	cl.MarkPassed("CL-001", "", "")
	cl.MarkPassed("CL-002", "", "")

	if !cl.AllPassed() {
		t.Error("should be all passed")
	}
}

func TestChecklist_GenerateReport(t *testing.T) {
	cl := NewChecklist("Login Tests")
	cl.AddItem(ChecklistItem{
		ID:          "CL-001",
		Description: "Login with valid credentials",
		Steps:       []string{"Navigate to login", "Enter credentials", "Click login"},
		Expected:    "User sees dashboard",
	})
	cl.MarkPassed("CL-001", "Dashboard displayed", "tester")

	report := cl.GenerateReport()

	if report == "" {
		t.Error("expected non-empty report")
	}

	t.Logf("Checklist Report:\n%s", report)
}

func TestNewTestPlan(t *testing.T) {
	tp := NewTestPlan("v1.0 Release", "1.0.0", "staging")

	if tp.Name != "v1.0 Release" {
		t.Error("Name not set")
	}
	if tp.Version != "1.0.0" {
		t.Error("Version not set")
	}
	if tp.Environment != "staging" {
		t.Error("Environment not set")
	}
}

func TestTestPlan_AddFeature(t *testing.T) {
	tp := NewTestPlan("Test", "1.0", "prod")
	tp.AddFeature(Feature{Name: "Login"})

	if len(tp.Features) != 1 {
		t.Error("Feature not added")
	}
}

func TestTestPlan_AddChecklist(t *testing.T) {
	tp := NewTestPlan("Test", "1.0", "prod")
	tp.AddChecklist(NewChecklist("Smoke Tests"))

	if len(tp.Checklists) != 1 {
		t.Error("Checklist not added")
	}
}

func TestTestPlan_AddTester(t *testing.T) {
	tp := NewTestPlan("Test", "1.0", "prod")
	tp.AddTester("Alice")
	tp.AddTester("Bob")

	if len(tp.Testers) != 2 {
		t.Errorf("expected 2 testers, got %d", len(tp.Testers))
	}
}

func TestTestPlan_GenerateReport(t *testing.T) {
	tp := NewTestPlan("v1.0 UAT", "1.0.0", "staging")
	tp.StartDate = time.Now()
	tp.EndDate = time.Now().Add(7 * 24 * time.Hour)
	tp.AddTester("Alice")
	tp.AddFeature(Feature{Name: "Login", Stories: make([]UserStory, 3)})
	tp.AddChecklist(NewChecklist("Smoke Tests"))

	report := tp.GenerateReport()

	if report == "" {
		t.Error("expected non-empty report")
	}

	t.Logf("Test Plan:\n%s", report)
}

func TestPriority(t *testing.T) {
	priorities := []Priority{
		PriorityCritical,
		PriorityHigh,
		PriorityMedium,
		PriorityLow,
	}

	for _, p := range priorities {
		if p == "" {
			t.Error("priority should not be empty")
		}
	}
}

func TestStatus(t *testing.T) {
	statuses := []Status{
		StatusPending,
		StatusPassed,
		StatusFailed,
		StatusSkipped,
		StatusBlocked,
	}

	for _, s := range statuses {
		if s == "" {
			t.Error("status should not be empty")
		}
	}
}

func TestUserStory(t *testing.T) {
	story := UserStory{
		ID:          "US-001",
		Title:       "Test Story",
		Description: "A test story",
		AsA:         "user",
		IWant:       "to test",
		SoThat:      "I can verify",
		Priority:    PriorityHigh,
		Status:      StatusPending,
	}

	if story.ID != "US-001" {
		t.Error("ID not set")
	}
	if story.Priority != PriorityHigh {
		t.Error("Priority not set")
	}
}

func TestAcceptanceCriterion(t *testing.T) {
	ac := AcceptanceCriterion{
		ID:       "AC-001",
		Given:    "precondition",
		When:     "action",
		Then:     "result",
		Status:   StatusPassed,
		Duration: time.Second,
	}

	if ac.ID != "AC-001" {
		t.Error("ID not set")
	}
	if ac.Given != "precondition" {
		t.Error("Given not set")
	}
}

func TestFeatureResult(t *testing.T) {
	fr := FeatureResult{
		PassedCount:  5,
		FailedCount:  2,
		SkippedCount: 1,
		Duration:     time.Minute,
	}

	if fr.PassedCount != 5 {
		t.Error("PassedCount not set")
	}
	if fr.Duration != time.Minute {
		t.Error("Duration not set")
	}
}
