// Package uat provides utilities for user acceptance testing.
package uat

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

// Feature represents a feature under test.
type Feature struct {
	Name        string
	Description string
	Stories     []UserStory
}

// UserStory represents a user story with acceptance criteria.
type UserStory struct {
	ID          string
	Title       string
	Description string
	AsA         string // As a <role>
	IWant       string // I want <capability>
	SoThat      string // So that <benefit>
	Criteria    []AcceptanceCriterion
	Priority    Priority
	Status      Status
}

// AcceptanceCriterion defines a single acceptance criterion.
type AcceptanceCriterion struct {
	ID       string
	Given    string
	When     string
	Then     string
	Verify   func(ctx context.Context) error
	Status   Status
	Error    error
	Duration time.Duration
}

// Priority indicates story priority.
type Priority string

const (
	PriorityCritical Priority = "critical"
	PriorityHigh     Priority = "high"
	PriorityMedium   Priority = "medium"
	PriorityLow      Priority = "low"
)

// Status indicates test status.
type Status string

const (
	StatusPending Status = "pending"
	StatusPassed  Status = "passed"
	StatusFailed  Status = "failed"
	StatusSkipped Status = "skipped"
	StatusBlocked Status = "blocked"
)

// Runner executes user acceptance tests.
type Runner struct {
	features []Feature
	results  []FeatureResult
}

// NewRunner creates a UAT runner.
func NewRunner() *Runner {
	return &Runner{
		features: make([]Feature, 0),
		results:  make([]FeatureResult, 0),
	}
}

// AddFeature adds a feature to test.
func (r *Runner) AddFeature(f Feature) {
	r.features = append(r.features, f)
}

// FeatureResult contains results for a feature.
type FeatureResult struct {
	Feature      Feature
	StoryResults []StoryResult
	Duration     time.Duration
	PassedCount  int
	FailedCount  int
	SkippedCount int
}

// StoryResult contains results for a user story.
type StoryResult struct {
	Story           UserStory
	CriteriaResults []CriterionResult
	Duration        time.Duration
	Status          Status
}

// CriterionResult contains results for a criterion.
type CriterionResult struct {
	Criterion AcceptanceCriterion
	Status    Status
	Error     error
	Duration  time.Duration
}

// Run executes all features.
func (r *Runner) Run(ctx context.Context) error {
	for _, feature := range r.features {
		result := r.runFeature(ctx, feature)
		r.results = append(r.results, result)
	}
	return nil
}

// RunT executes all features with testing.T.
func (r *Runner) RunT(t *testing.T) {
	t.Helper()

	ctx := context.Background()

	for _, feature := range r.features {
		t.Run(feature.Name, func(t *testing.T) {
			result := r.runFeature(ctx, feature)
			r.results = append(r.results, result)

			for _, sr := range result.StoryResults {
				if sr.Status == StatusFailed {
					t.Errorf("Story %s failed", sr.Story.ID)
					for _, cr := range sr.CriteriaResults {
						if cr.Status == StatusFailed {
							t.Errorf("  Criterion %s: %v", cr.Criterion.ID, cr.Error)
						}
					}
				}
			}
		})
	}
}

// runFeature executes a single feature.
func (r *Runner) runFeature(ctx context.Context, feature Feature) FeatureResult {
	start := time.Now()
	result := FeatureResult{
		Feature:      feature,
		StoryResults: make([]StoryResult, 0, len(feature.Stories)),
	}

	for _, story := range feature.Stories {
		sr := r.runStory(ctx, story)
		result.StoryResults = append(result.StoryResults, sr)

		switch sr.Status {
		case StatusPassed:
			result.PassedCount++
		case StatusFailed:
			result.FailedCount++
		case StatusSkipped:
			result.SkippedCount++
		}
	}

	result.Duration = time.Since(start)
	return result
}

// runStory executes a single user story.
func (r *Runner) runStory(ctx context.Context, story UserStory) StoryResult {
	start := time.Now()
	result := StoryResult{
		Story:           story,
		CriteriaResults: make([]CriterionResult, 0, len(story.Criteria)),
		Status:          StatusPassed,
	}

	for _, criterion := range story.Criteria {
		cr := r.runCriterion(ctx, criterion)
		result.CriteriaResults = append(result.CriteriaResults, cr)

		if cr.Status == StatusFailed {
			result.Status = StatusFailed
		}
	}

	result.Duration = time.Since(start)
	return result
}

// runCriterion executes a single criterion.
func (r *Runner) runCriterion(ctx context.Context, criterion AcceptanceCriterion) CriterionResult {
	start := time.Now()
	result := CriterionResult{
		Criterion: criterion,
		Status:    StatusPassed,
	}

	if criterion.Verify == nil {
		result.Status = StatusSkipped
		return result
	}

	err := criterion.Verify(ctx)
	result.Duration = time.Since(start)

	if err != nil {
		result.Status = StatusFailed
		result.Error = err
	}

	return result
}

// Results returns all feature results.
func (r *Runner) Results() []FeatureResult {
	results := make([]FeatureResult, len(r.results))
	copy(results, r.results)
	return results
}

// Summary returns a summary of all results.
func (r *Runner) Summary() Summary {
	summary := Summary{
		Features: len(r.features),
	}

	for _, fr := range r.results {
		summary.Stories += len(fr.StoryResults)
		summary.Passed += fr.PassedCount
		summary.Failed += fr.FailedCount
		summary.Skipped += fr.SkippedCount
		summary.Duration += fr.Duration

		for _, sr := range fr.StoryResults {
			summary.Criteria += len(sr.CriteriaResults)
		}
	}

	return summary
}

// Summary contains UAT summary statistics.
type Summary struct {
	Features int
	Stories  int
	Criteria int
	Passed   int
	Failed   int
	Skipped  int
	Duration time.Duration
}

// PassRate returns the pass percentage.
func (s Summary) PassRate() float64 {
	total := s.Passed + s.Failed
	if total == 0 {
		return 0
	}
	return float64(s.Passed) / float64(total) * 100
}

// GenerateReport creates a formatted UAT report.
func (r *Runner) GenerateReport() string {
	var sb strings.Builder

	sb.WriteString("User Acceptance Test Report\n")
	sb.WriteString("===========================\n\n")

	summary := r.Summary()
	sb.WriteString("Summary:\n")
	sb.WriteString(fmt.Sprintf("  Features:  %d\n", summary.Features))
	sb.WriteString(fmt.Sprintf("  Stories:   %d\n", summary.Stories))
	sb.WriteString(fmt.Sprintf("  Criteria:  %d\n", summary.Criteria))
	sb.WriteString(fmt.Sprintf("  Passed:    %d (%.1f%%)\n", summary.Passed, summary.PassRate()))
	sb.WriteString(fmt.Sprintf("  Failed:    %d\n", summary.Failed))
	sb.WriteString(fmt.Sprintf("  Skipped:   %d\n", summary.Skipped))
	sb.WriteString(fmt.Sprintf("  Duration:  %v\n\n", summary.Duration.Round(time.Millisecond)))

	for _, fr := range r.results {
		sb.WriteString(fmt.Sprintf("Feature: %s\n", fr.Feature.Name))
		sb.WriteString(fmt.Sprintf("  %s\n", fr.Feature.Description))
		sb.WriteString(fmt.Sprintf("  Duration: %v\n\n", fr.Duration.Round(time.Millisecond)))

		for _, sr := range fr.StoryResults {
			status := statusIcon(sr.Status)
			sb.WriteString(fmt.Sprintf("  %s Story: %s - %s\n", status, sr.Story.ID, sr.Story.Title))
			sb.WriteString(fmt.Sprintf("    As a %s\n", sr.Story.AsA))
			sb.WriteString(fmt.Sprintf("    I want %s\n", sr.Story.IWant))
			sb.WriteString(fmt.Sprintf("    So that %s\n\n", sr.Story.SoThat))

			for _, cr := range sr.CriteriaResults {
				status = statusIcon(cr.Status)
				sb.WriteString(fmt.Sprintf("    %s Criterion: %s\n", status, cr.Criterion.ID))
				sb.WriteString(fmt.Sprintf("      Given: %s\n", cr.Criterion.Given))
				sb.WriteString(fmt.Sprintf("      When:  %s\n", cr.Criterion.When))
				sb.WriteString(fmt.Sprintf("      Then:  %s\n", cr.Criterion.Then))
				if cr.Error != nil {
					sb.WriteString(fmt.Sprintf("      Error: %v\n", cr.Error))
				}
				sb.WriteString("\n")
			}
		}
	}

	return sb.String()
}

// statusIcon returns an icon for the status.
func statusIcon(s Status) string {
	switch s {
	case StatusPassed:
		return "✓"
	case StatusFailed:
		return "✗"
	case StatusSkipped:
		return "○"
	case StatusBlocked:
		return "⊘"
	default:
		return "?"
	}
}

// Checklist provides a simple checklist for manual UAT.
type Checklist struct {
	Name  string
	Items []ChecklistItem
}

// ChecklistItem represents a single check item.
type ChecklistItem struct {
	ID          string
	Description string
	Steps       []string
	Expected    string
	Actual      string
	Status      Status
	Notes       string
	TestedBy    string
	TestedAt    time.Time
}

// NewChecklist creates a new checklist.
func NewChecklist(name string) *Checklist {
	return &Checklist{
		Name:  name,
		Items: make([]ChecklistItem, 0),
	}
}

// AddItem adds an item to the checklist.
func (c *Checklist) AddItem(item ChecklistItem) {
	if item.Status == "" {
		item.Status = StatusPending
	}
	c.Items = append(c.Items, item)
}

// MarkPassed marks an item as passed.
func (c *Checklist) MarkPassed(id, actual, tester string) {
	for i := range c.Items {
		if c.Items[i].ID == id {
			c.Items[i].Status = StatusPassed
			c.Items[i].Actual = actual
			c.Items[i].TestedBy = tester
			c.Items[i].TestedAt = time.Now()
			return
		}
	}
}

// MarkFailed marks an item as failed.
func (c *Checklist) MarkFailed(id, actual, notes, tester string) {
	for i := range c.Items {
		if c.Items[i].ID == id {
			c.Items[i].Status = StatusFailed
			c.Items[i].Actual = actual
			c.Items[i].Notes = notes
			c.Items[i].TestedBy = tester
			c.Items[i].TestedAt = time.Now()
			return
		}
	}
}

// Progress returns completion statistics.
func (c *Checklist) Progress() (completed, total int) {
	total = len(c.Items)
	for _, item := range c.Items {
		if item.Status != StatusPending {
			completed++
		}
	}
	return
}

// AllPassed returns true if all items passed.
func (c *Checklist) AllPassed() bool {
	for _, item := range c.Items {
		if item.Status != StatusPassed {
			return false
		}
	}
	return len(c.Items) > 0
}

// GenerateReport creates a checklist report.
func (c *Checklist) GenerateReport() string {
	var sb strings.Builder

	sb.WriteString(fmt.Sprintf("UAT Checklist: %s\n", c.Name))
	sb.WriteString(strings.Repeat("=", len(c.Name)+15) + "\n\n")

	completed, total := c.Progress()
	sb.WriteString(fmt.Sprintf("Progress: %d/%d (%.1f%%)\n\n",
		completed, total, float64(completed)/float64(total)*100))

	for _, item := range c.Items {
		status := statusIcon(item.Status)
		sb.WriteString(fmt.Sprintf("%s [%s] %s\n", status, item.ID, item.Description))

		if len(item.Steps) > 0 {
			sb.WriteString("  Steps:\n")
			for i, step := range item.Steps {
				sb.WriteString(fmt.Sprintf("    %d. %s\n", i+1, step))
			}
		}

		sb.WriteString(fmt.Sprintf("  Expected: %s\n", item.Expected))

		if item.Actual != "" {
			sb.WriteString(fmt.Sprintf("  Actual:   %s\n", item.Actual))
		}
		if item.Notes != "" {
			sb.WriteString(fmt.Sprintf("  Notes:    %s\n", item.Notes))
		}
		if item.TestedBy != "" {
			sb.WriteString(fmt.Sprintf("  Tested:   %s at %s\n",
				item.TestedBy, item.TestedAt.Format(time.RFC3339)))
		}
		sb.WriteString("\n")
	}

	return sb.String()
}

// TestPlan represents a UAT test plan.
type TestPlan struct {
	Name        string
	Version     string
	Environment string
	StartDate   time.Time
	EndDate     time.Time
	Testers     []string
	Features    []Feature
	Checklists  []*Checklist
}

// NewTestPlan creates a new test plan.
func NewTestPlan(name, version, env string) *TestPlan {
	return &TestPlan{
		Name:        name,
		Version:     version,
		Environment: env,
		Features:    make([]Feature, 0),
		Checklists:  make([]*Checklist, 0),
	}
}

// AddFeature adds a feature to the test plan.
func (tp *TestPlan) AddFeature(f Feature) {
	tp.Features = append(tp.Features, f)
}

// AddChecklist adds a checklist to the test plan.
func (tp *TestPlan) AddChecklist(c *Checklist) {
	tp.Checklists = append(tp.Checklists, c)
}

// AddTester adds a tester to the plan.
func (tp *TestPlan) AddTester(name string) {
	tp.Testers = append(tp.Testers, name)
}

// GenerateReport creates a test plan report.
func (tp *TestPlan) GenerateReport() string {
	var sb strings.Builder

	sb.WriteString(fmt.Sprintf("UAT Test Plan: %s\n", tp.Name))
	sb.WriteString(strings.Repeat("=", len(tp.Name)+15) + "\n\n")

	sb.WriteString(fmt.Sprintf("Version:     %s\n", tp.Version))
	sb.WriteString(fmt.Sprintf("Environment: %s\n", tp.Environment))
	if !tp.StartDate.IsZero() {
		sb.WriteString(fmt.Sprintf("Start Date:  %s\n", tp.StartDate.Format("2006-01-02")))
	}
	if !tp.EndDate.IsZero() {
		sb.WriteString(fmt.Sprintf("End Date:    %s\n", tp.EndDate.Format("2006-01-02")))
	}
	if len(tp.Testers) > 0 {
		sb.WriteString(fmt.Sprintf("Testers:     %s\n", strings.Join(tp.Testers, ", ")))
	}
	sb.WriteString("\n")

	sb.WriteString(fmt.Sprintf("Features: %d\n", len(tp.Features)))
	for _, f := range tp.Features {
		sb.WriteString(fmt.Sprintf("  - %s (%d stories)\n", f.Name, len(f.Stories)))
	}
	sb.WriteString("\n")

	sb.WriteString(fmt.Sprintf("Checklists: %d\n", len(tp.Checklists)))
	for _, c := range tp.Checklists {
		completed, total := c.Progress()
		sb.WriteString(fmt.Sprintf("  - %s (%d/%d)\n", c.Name, completed, total))
	}

	return sb.String()
}
