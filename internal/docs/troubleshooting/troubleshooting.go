// Package troubleshooting provides utilities for troubleshooting documentation.
package troubleshooting

import (
	"fmt"
	"io"
	"strings"
	"time"
)

// Guide represents a troubleshooting guide.
type Guide struct {
	ID          string
	Title       string
	Description string
	Category    Category
	Problems    []Problem
	UpdatedAt   time.Time
}

// Category categorizes troubleshooting guides.
type Category string

const (
	CategoryConnection    Category = "connection"
	CategoryPerformance   Category = "performance"
	CategoryAuth          Category = "authentication"
	CategoryStorage       Category = "storage"
	CategoryNetwork       Category = "network"
	CategoryConfiguration Category = "configuration"
	CategoryData          Category = "data"
	CategoryAPI           Category = "api"
)

// Problem represents a problem with solutions.
type Problem struct {
	ID         string
	Title      string
	Symptoms   []string
	Causes     []Cause
	Solutions  []Solution
	Prevention []string
	RelatedIDs []string
	Severity   Severity
}

// Cause represents a possible cause.
type Cause struct {
	Description string
	Likelihood  Likelihood
	Diagnostic  string
}

// Likelihood indicates how likely a cause is.
type Likelihood string

const (
	LikelihoodHigh   Likelihood = "high"
	LikelihoodMedium Likelihood = "medium"
	LikelihoodLow    Likelihood = "low"
)

// Solution represents a solution to a problem.
type Solution struct {
	Title       string
	Description string
	Steps       []SolutionStep
	Duration    time.Duration
	Difficulty  Difficulty
	ForCause    string
}

// SolutionStep represents a step in a solution.
type SolutionStep struct {
	Action   string
	Command  string
	Expected string
	Warning  string
}

// Difficulty indicates solution difficulty.
type Difficulty string

const (
	DifficultyEasy     Difficulty = "easy"
	DifficultyModerate Difficulty = "moderate"
	DifficultyAdvanced Difficulty = "advanced"
	DifficultyExpert   Difficulty = "expert"
)

// Severity indicates problem severity.
type Severity string

const (
	SeverityLow      Severity = "low"
	SeverityMedium   Severity = "medium"
	SeverityHigh     Severity = "high"
	SeverityCritical Severity = "critical"
)

// GuideBuilder helps construct troubleshooting guides.
type GuideBuilder struct {
	guide *Guide
}

// NewGuide creates a troubleshooting guide builder.
func NewGuide(id, title string) *GuideBuilder {
	return &GuideBuilder{
		guide: &Guide{
			ID:        id,
			Title:     title,
			Problems:  make([]Problem, 0),
			UpdatedAt: time.Now(),
		},
	}
}

// Description sets the description.
func (b *GuideBuilder) Description(desc string) *GuideBuilder {
	b.guide.Description = desc
	return b
}

// Category sets the category.
func (b *GuideBuilder) Category(cat Category) *GuideBuilder {
	b.guide.Category = cat
	return b
}

// Problem adds a problem.
func (b *GuideBuilder) Problem(p Problem) *GuideBuilder {
	b.guide.Problems = append(b.guide.Problems, p)
	return b
}

// Build returns the completed guide.
func (b *GuideBuilder) Build() *Guide {
	return b.guide
}

// ProblemBuilder helps construct problems.
type ProblemBuilder struct {
	problem *Problem
}

// NewProblem creates a problem builder.
func NewProblem(id, title string) *ProblemBuilder {
	return &ProblemBuilder{
		problem: &Problem{
			ID:         id,
			Title:      title,
			Symptoms:   make([]string, 0),
			Causes:     make([]Cause, 0),
			Solutions:  make([]Solution, 0),
			Prevention: make([]string, 0),
			RelatedIDs: make([]string, 0),
			Severity:   SeverityMedium,
		},
	}
}

// Symptom adds a symptom.
func (b *ProblemBuilder) Symptom(symptom string) *ProblemBuilder {
	b.problem.Symptoms = append(b.problem.Symptoms, symptom)
	return b
}

// Cause adds a cause.
func (b *ProblemBuilder) Cause(desc string, likelihood Likelihood, diagnostic string) *ProblemBuilder {
	b.problem.Causes = append(b.problem.Causes, Cause{
		Description: desc,
		Likelihood:  likelihood,
		Diagnostic:  diagnostic,
	})
	return b
}

// Solution adds a solution.
func (b *ProblemBuilder) Solution(s Solution) *ProblemBuilder {
	b.problem.Solutions = append(b.problem.Solutions, s)
	return b
}

// Prevention adds a prevention tip.
func (b *ProblemBuilder) Prevention(tip string) *ProblemBuilder {
	b.problem.Prevention = append(b.problem.Prevention, tip)
	return b
}

// Related adds a related problem ID.
func (b *ProblemBuilder) Related(id string) *ProblemBuilder {
	b.problem.RelatedIDs = append(b.problem.RelatedIDs, id)
	return b
}

// Severity sets the severity.
func (b *ProblemBuilder) Severity(s Severity) *ProblemBuilder {
	b.problem.Severity = s
	return b
}

// Build returns the completed problem.
func (b *ProblemBuilder) Build() Problem {
	return *b.problem
}

// SolutionBuilder helps construct solutions.
type SolutionBuilder struct {
	solution *Solution
}

// NewSolution creates a solution builder.
func NewSolution(title string) *SolutionBuilder {
	return &SolutionBuilder{
		solution: &Solution{
			Title:      title,
			Steps:      make([]SolutionStep, 0),
			Difficulty: DifficultyModerate,
		},
	}
}

// Description sets the description.
func (b *SolutionBuilder) Description(desc string) *SolutionBuilder {
	b.solution.Description = desc
	return b
}

// Duration sets the estimated duration.
func (b *SolutionBuilder) Duration(d time.Duration) *SolutionBuilder {
	b.solution.Duration = d
	return b
}

// Difficulty sets the difficulty.
func (b *SolutionBuilder) Difficulty(d Difficulty) *SolutionBuilder {
	b.solution.Difficulty = d
	return b
}

// ForCause sets which cause this solution addresses.
func (b *SolutionBuilder) ForCause(cause string) *SolutionBuilder {
	b.solution.ForCause = cause
	return b
}

// Step adds a step.
func (b *SolutionBuilder) Step(action, command, expected string) *SolutionBuilder {
	b.solution.Steps = append(b.solution.Steps, SolutionStep{
		Action:   action,
		Command:  command,
		Expected: expected,
	})
	return b
}

// StepWithWarning adds a step with a warning.
func (b *SolutionBuilder) StepWithWarning(action, command, expected, warning string) *SolutionBuilder {
	b.solution.Steps = append(b.solution.Steps, SolutionStep{
		Action:   action,
		Command:  command,
		Expected: expected,
		Warning:  warning,
	})
	return b
}

// Build returns the completed solution.
func (b *SolutionBuilder) Build() Solution {
	return *b.solution
}

// ErrorCode represents a documented error code.
type ErrorCode struct {
	Code        string
	Name        string
	Description string
	Causes      []string
	Solutions   []string
	Example     string
	SeeAlso     []string
}

// ErrorCodeRegistry manages error codes.
type ErrorCodeRegistry struct {
	codes map[string]*ErrorCode
}

// NewErrorCodeRegistry creates an error code registry.
func NewErrorCodeRegistry() *ErrorCodeRegistry {
	return &ErrorCodeRegistry{
		codes: make(map[string]*ErrorCode),
	}
}

// Register adds an error code.
func (r *ErrorCodeRegistry) Register(code *ErrorCode) {
	r.codes[code.Code] = code
}

// Get retrieves an error code.
func (r *ErrorCodeRegistry) Get(code string) (*ErrorCode, bool) {
	ec, ok := r.codes[code]
	return ec, ok
}

// All returns all error codes.
func (r *ErrorCodeRegistry) All() []*ErrorCode {
	codes := make([]*ErrorCode, 0, len(r.codes))
	for _, c := range r.codes {
		codes = append(codes, c)
	}
	return codes
}

// Search searches error codes.
func (r *ErrorCodeRegistry) Search(query string) []*ErrorCode {
	query = strings.ToLower(query)
	results := make([]*ErrorCode, 0)

	for _, code := range r.codes {
		if strings.Contains(strings.ToLower(code.Code), query) ||
			strings.Contains(strings.ToLower(code.Name), query) ||
			strings.Contains(strings.ToLower(code.Description), query) {
			results = append(results, code)
		}
	}

	return results
}

// DecisionTree helps diagnose problems.
type DecisionTree struct {
	Root *DecisionNode
}

// DecisionNode represents a node in a decision tree.
type DecisionNode struct {
	Question  string
	YesNode   *DecisionNode
	NoNode    *DecisionNode
	Solution  string
	ProblemID string
}

// NewDecisionTree creates a decision tree.
func NewDecisionTree(root *DecisionNode) *DecisionTree {
	return &DecisionTree{Root: root}
}

// Traverse walks the tree based on answers.
func (dt *DecisionTree) Traverse(answers []bool) (*DecisionNode, error) {
	if dt.Root == nil {
		return nil, fmt.Errorf("empty decision tree")
	}

	current := dt.Root
	for i, answer := range answers {
		if current.Solution != "" || current.ProblemID != "" {
			return current, nil
		}

		if answer {
			if current.YesNode == nil {
				return nil, fmt.Errorf("no yes path at step %d", i)
			}
			current = current.YesNode
		} else {
			if current.NoNode == nil {
				return nil, fmt.Errorf("no no path at step %d", i)
			}
			current = current.NoNode
		}
	}

	return current, nil
}

// TroubleshootingRenderer renders troubleshooting documentation.
type TroubleshootingRenderer struct{}

// NewTroubleshootingRenderer creates a renderer.
func NewTroubleshootingRenderer() *TroubleshootingRenderer {
	return &TroubleshootingRenderer{}
}

// RenderGuide renders a guide to Markdown.
func (r *TroubleshootingRenderer) RenderGuide(w io.Writer, guide *Guide) error {
	var sb strings.Builder

	sb.WriteString("# " + guide.Title + "\n\n")
	if guide.Description != "" {
		sb.WriteString(guide.Description + "\n\n")
	}

	// Table of contents
	if len(guide.Problems) > 1 {
		sb.WriteString("## Problems\n\n")
		for _, p := range guide.Problems {
			severity := severityIcon(p.Severity)
			sb.WriteString(fmt.Sprintf("- %s [%s](#%s)\n", severity, p.Title, p.ID))
		}
		sb.WriteString("\n")
	}

	// Problems
	for _, p := range guide.Problems {
		r.renderProblem(&sb, p)
	}

	_, err := w.Write([]byte(sb.String()))
	return err
}

// renderProblem renders a problem to Markdown.
func (r *TroubleshootingRenderer) renderProblem(sb *strings.Builder, p Problem) {
	severity := severityIcon(p.Severity)
	fmt.Fprintf(sb, "## %s %s\n\n", severity, p.Title)

	// Symptoms
	if len(p.Symptoms) > 0 {
		sb.WriteString("### Symptoms\n\n")
		for _, s := range p.Symptoms {
			sb.WriteString("- " + s + "\n")
		}
		sb.WriteString("\n")
	}

	// Causes
	if len(p.Causes) > 0 {
		sb.WriteString("### Possible Causes\n\n")
		for _, c := range p.Causes {
			likelihood := likelihoodBadge(c.Likelihood)
			fmt.Fprintf(sb, "**%s** %s\n\n", c.Description, likelihood)
			if c.Diagnostic != "" {
				sb.WriteString("Diagnostic: `" + c.Diagnostic + "`\n\n")
			}
		}
	}

	// Solutions
	if len(p.Solutions) > 0 {
		sb.WriteString("### Solutions\n\n")
		for i, s := range p.Solutions {
			r.renderSolution(sb, s, i+1)
		}
	}

	// Prevention
	if len(p.Prevention) > 0 {
		sb.WriteString("### Prevention\n\n")
		for _, tip := range p.Prevention {
			sb.WriteString("- " + tip + "\n")
		}
		sb.WriteString("\n")
	}

	// Related
	if len(p.RelatedIDs) > 0 {
		sb.WriteString("### Related Problems\n\n")
		for _, id := range p.RelatedIDs {
			sb.WriteString("- [" + id + "](#" + id + ")\n")
		}
		sb.WriteString("\n")
	}
}

// renderSolution renders a solution to Markdown.
func (r *TroubleshootingRenderer) renderSolution(sb *strings.Builder, s Solution, num int) {
	difficulty := difficultyBadge(s.Difficulty)
	fmt.Fprintf(sb, "#### Solution %d: %s %s\n\n", num, s.Title, difficulty)

	if s.Description != "" {
		sb.WriteString(s.Description + "\n\n")
	}

	if s.Duration > 0 {
		fmt.Fprintf(sb, "⏱️ Estimated time: %s\n\n", s.Duration)
	}

	if s.ForCause != "" {
		sb.WriteString("*For cause: " + s.ForCause + "*\n\n")
	}

	for i, step := range s.Steps {
		fmt.Fprintf(sb, "**Step %d:** %s\n\n", i+1, step.Action)
		if step.Command != "" {
			sb.WriteString("```bash\n" + step.Command + "\n```\n\n")
		}
		if step.Expected != "" {
			sb.WriteString("*Expected:* " + step.Expected + "\n\n")
		}
		if step.Warning != "" {
			sb.WriteString("> ⚠️ " + step.Warning + "\n\n")
		}
	}
}

// RenderErrorCodes renders error codes to Markdown.
func (r *TroubleshootingRenderer) RenderErrorCodes(w io.Writer, registry *ErrorCodeRegistry) error {
	var sb strings.Builder

	sb.WriteString("# Error Codes Reference\n\n")

	codes := registry.All()
	sb.WriteString("| Code | Name | Description |\n")
	sb.WriteString("|------|------|-------------|\n")
	for _, c := range codes {
		fmt.Fprintf(&sb, "| %s | %s | %s |\n", c.Code, c.Name, c.Description)
	}
	sb.WriteString("\n")

	for _, c := range codes {
		fmt.Fprintf(&sb, "## %s - %s\n\n", c.Code, c.Name)
		sb.WriteString(c.Description + "\n\n")

		if len(c.Causes) > 0 {
			sb.WriteString("**Causes:**\n")
			for _, cause := range c.Causes {
				sb.WriteString("- " + cause + "\n")
			}
			sb.WriteString("\n")
		}

		if len(c.Solutions) > 0 {
			sb.WriteString("**Solutions:**\n")
			for _, sol := range c.Solutions {
				sb.WriteString("- " + sol + "\n")
			}
			sb.WriteString("\n")
		}

		if c.Example != "" {
			sb.WriteString("**Example:**\n```\n" + c.Example + "\n```\n\n")
		}
	}

	_, err := w.Write([]byte(sb.String()))
	return err
}

// severityIcon returns an icon for severity.
func severityIcon(s Severity) string {
	switch s {
	case SeverityLow:
		return "🟢"
	case SeverityMedium:
		return "🟡"
	case SeverityHigh:
		return "🟠"
	case SeverityCritical:
		return "🔴"
	default:
		return "⚪"
	}
}

// likelihoodBadge returns a badge for likelihood.
func likelihoodBadge(l Likelihood) string {
	switch l {
	case LikelihoodHigh:
		return "(Most likely)"
	case LikelihoodMedium:
		return "(Possible)"
	case LikelihoodLow:
		return "(Unlikely)"
	default:
		return ""
	}
}

// difficultyBadge returns a badge for difficulty.
func difficultyBadge(d Difficulty) string {
	switch d {
	case DifficultyEasy:
		return "🟢 Easy"
	case DifficultyModerate:
		return "🟡 Moderate"
	case DifficultyAdvanced:
		return "🟠 Advanced"
	case DifficultyExpert:
		return "🔴 Expert"
	default:
		return ""
	}
}
