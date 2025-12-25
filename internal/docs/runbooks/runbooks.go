// Package runbooks provides utilities for operational runbook documentation.
package runbooks

import (
	"fmt"
	"io"
	"strings"
	"time"
)

// Runbook represents an operational runbook.
type Runbook struct {
	ID            string
	Title         string
	Description   string
	Category      Category
	Priority      Priority
	Owner         string
	LastTested    time.Time
	LastUpdated   time.Time
	Triggers      []Trigger
	Prerequisites []Prerequisite
	Procedures    []Procedure
	Verification  []Verification
	Rollback      []RollbackStep
	Escalation    *EscalationPath
	References    []Reference
}

// Category categorizes runbooks.
type Category string

const (
	CategoryIncident    Category = "incident"
	CategoryDeployment  Category = "deployment"
	CategoryMaintenance Category = "maintenance"
	CategoryRecovery    Category = "recovery"
	CategoryScaling     Category = "scaling"
	CategorySecurity    Category = "security"
	CategoryBackup      Category = "backup"
)

// Priority indicates runbook priority.
type Priority string

const (
	PriorityCritical Priority = "P1"
	PriorityHigh     Priority = "P2"
	PriorityMedium   Priority = "P3"
	PriorityLow      Priority = "P4"
)

// Trigger defines what triggers the runbook.
type Trigger struct {
	Type        TriggerType
	Condition   string
	Description string
	AlertName   string
}

// TriggerType categorizes triggers.
type TriggerType string

const (
	TriggerAlert     TriggerType = "alert"
	TriggerScheduled TriggerType = "scheduled"
	TriggerManual    TriggerType = "manual"
	TriggerAutomatic TriggerType = "automatic"
)

// Prerequisite defines a prerequisite.
type Prerequisite struct {
	Description string
	Required    bool
	CheckCmd    string
}

// Procedure represents a runbook procedure.
type Procedure struct {
	Name        string
	Description string
	Steps       []Step
	Duration    time.Duration
	Automated   bool
}

// Step represents a procedure step.
type Step struct {
	Number     int
	Action     string
	Command    string
	Expected   string
	Warning    string
	Timeout    time.Duration
	Retryable  bool
	MaxRetries int
}

// Verification defines how to verify success.
type Verification struct {
	Description string
	Command     string
	Expected    string
	Automated   bool
}

// RollbackStep defines a rollback step.
type RollbackStep struct {
	Number    int
	Condition string
	Action    string
	Command   string
}

// EscalationPath defines escalation procedures.
type EscalationPath struct {
	Levels     []EscalationLevel
	DefaultSLA time.Duration
}

// EscalationLevel defines an escalation level.
type EscalationLevel struct {
	Level    int
	Name     string
	Contact  string
	Method   string
	WaitTime time.Duration
	Actions  []string
}

// Reference provides additional resources.
type Reference struct {
	Title string
	URL   string
	Type  string
}

// RunbookBuilder helps construct runbooks.
type RunbookBuilder struct {
	runbook *Runbook
}

// NewRunbook creates a runbook builder.
func NewRunbook(id, title string) *RunbookBuilder {
	return &RunbookBuilder{
		runbook: &Runbook{
			ID:            id,
			Title:         title,
			LastUpdated:   time.Now(),
			Triggers:      make([]Trigger, 0),
			Prerequisites: make([]Prerequisite, 0),
			Procedures:    make([]Procedure, 0),
			Verification:  make([]Verification, 0),
			Rollback:      make([]RollbackStep, 0),
			References:    make([]Reference, 0),
		},
	}
}

// Description sets the description.
func (b *RunbookBuilder) Description(desc string) *RunbookBuilder {
	b.runbook.Description = desc
	return b
}

// Category sets the category.
func (b *RunbookBuilder) Category(cat Category) *RunbookBuilder {
	b.runbook.Category = cat
	return b
}

// Priority sets the priority.
func (b *RunbookBuilder) Priority(p Priority) *RunbookBuilder {
	b.runbook.Priority = p
	return b
}

// Owner sets the owner.
func (b *RunbookBuilder) Owner(owner string) *RunbookBuilder {
	b.runbook.Owner = owner
	return b
}

// LastTested sets the last tested date.
func (b *RunbookBuilder) LastTested(t time.Time) *RunbookBuilder {
	b.runbook.LastTested = t
	return b
}

// Trigger adds a trigger.
func (b *RunbookBuilder) Trigger(t Trigger) *RunbookBuilder {
	b.runbook.Triggers = append(b.runbook.Triggers, t)
	return b
}

// Prerequisite adds a prerequisite.
func (b *RunbookBuilder) Prerequisite(desc string, required bool, checkCmd string) *RunbookBuilder {
	b.runbook.Prerequisites = append(b.runbook.Prerequisites, Prerequisite{
		Description: desc,
		Required:    required,
		CheckCmd:    checkCmd,
	})
	return b
}

// Procedure adds a procedure.
func (b *RunbookBuilder) Procedure(p Procedure) *RunbookBuilder {
	b.runbook.Procedures = append(b.runbook.Procedures, p)
	return b
}

// Verification adds a verification step.
func (b *RunbookBuilder) Verification(desc, cmd, expected string, automated bool) *RunbookBuilder {
	b.runbook.Verification = append(b.runbook.Verification, Verification{
		Description: desc,
		Command:     cmd,
		Expected:    expected,
		Automated:   automated,
	})
	return b
}

// RollbackStep adds a rollback step.
func (b *RunbookBuilder) RollbackStep(num int, condition, action, cmd string) *RunbookBuilder {
	b.runbook.Rollback = append(b.runbook.Rollback, RollbackStep{
		Number:    num,
		Condition: condition,
		Action:    action,
		Command:   cmd,
	})
	return b
}

// Escalation sets the escalation path.
func (b *RunbookBuilder) Escalation(e *EscalationPath) *RunbookBuilder {
	b.runbook.Escalation = e
	return b
}

// Reference adds a reference.
func (b *RunbookBuilder) Reference(title, url, refType string) *RunbookBuilder {
	b.runbook.References = append(b.runbook.References, Reference{
		Title: title,
		URL:   url,
		Type:  refType,
	})
	return b
}

// Build returns the completed runbook.
func (b *RunbookBuilder) Build() *Runbook {
	return b.runbook
}

// ProcedureBuilder helps construct procedures.
type ProcedureBuilder struct {
	proc *Procedure
}

// NewProcedure creates a procedure builder.
func NewProcedure(name string) *ProcedureBuilder {
	return &ProcedureBuilder{
		proc: &Procedure{
			Name:  name,
			Steps: make([]Step, 0),
		},
	}
}

// Description sets the description.
func (b *ProcedureBuilder) Description(desc string) *ProcedureBuilder {
	b.proc.Description = desc
	return b
}

// Duration sets the estimated duration.
func (b *ProcedureBuilder) Duration(d time.Duration) *ProcedureBuilder {
	b.proc.Duration = d
	return b
}

// Automated marks the procedure as automated.
func (b *ProcedureBuilder) Automated() *ProcedureBuilder {
	b.proc.Automated = true
	return b
}

// Step adds a step.
func (b *ProcedureBuilder) Step(action, command, expected string) *ProcedureBuilder {
	step := Step{
		Number:   len(b.proc.Steps) + 1,
		Action:   action,
		Command:  command,
		Expected: expected,
	}
	b.proc.Steps = append(b.proc.Steps, step)
	return b
}

// StepWithWarning adds a step with a warning.
func (b *ProcedureBuilder) StepWithWarning(action, command, expected, warning string) *ProcedureBuilder {
	step := Step{
		Number:   len(b.proc.Steps) + 1,
		Action:   action,
		Command:  command,
		Expected: expected,
		Warning:  warning,
	}
	b.proc.Steps = append(b.proc.Steps, step)
	return b
}

// StepWithRetry adds a retryable step.
func (b *ProcedureBuilder) StepWithRetry(action, command, expected string, maxRetries int) *ProcedureBuilder {
	step := Step{
		Number:     len(b.proc.Steps) + 1,
		Action:     action,
		Command:    command,
		Expected:   expected,
		Retryable:  true,
		MaxRetries: maxRetries,
	}
	b.proc.Steps = append(b.proc.Steps, step)
	return b
}

// Build returns the completed procedure.
func (b *ProcedureBuilder) Build() Procedure {
	return *b.proc
}

// EscalationBuilder helps construct escalation paths.
type EscalationBuilder struct {
	path *EscalationPath
}

// NewEscalation creates an escalation builder.
func NewEscalation(defaultSLA time.Duration) *EscalationBuilder {
	return &EscalationBuilder{
		path: &EscalationPath{
			Levels:     make([]EscalationLevel, 0),
			DefaultSLA: defaultSLA,
		},
	}
}

// Level adds an escalation level.
func (b *EscalationBuilder) Level(level int, name, contact, method string, wait time.Duration, actions []string) *EscalationBuilder {
	b.path.Levels = append(b.path.Levels, EscalationLevel{
		Level:    level,
		Name:     name,
		Contact:  contact,
		Method:   method,
		WaitTime: wait,
		Actions:  actions,
	})
	return b
}

// Build returns the completed escalation path.
func (b *EscalationBuilder) Build() *EscalationPath {
	return b.path
}

// RunbookLibrary manages a collection of runbooks.
type RunbookLibrary struct {
	runbooks map[string]*Runbook
}

// NewRunbookLibrary creates a runbook library.
func NewRunbookLibrary() *RunbookLibrary {
	return &RunbookLibrary{
		runbooks: make(map[string]*Runbook),
	}
}

// Add adds a runbook to the library.
func (l *RunbookLibrary) Add(r *Runbook) {
	l.runbooks[r.ID] = r
}

// Get retrieves a runbook by ID.
func (l *RunbookLibrary) Get(id string) (*Runbook, bool) {
	r, ok := l.runbooks[id]
	return r, ok
}

// List returns all runbooks.
func (l *RunbookLibrary) List() []*Runbook {
	result := make([]*Runbook, 0, len(l.runbooks))
	for _, r := range l.runbooks {
		result = append(result, r)
	}
	return result
}

// ByCategory returns runbooks by category.
func (l *RunbookLibrary) ByCategory(cat Category) []*Runbook {
	result := make([]*Runbook, 0)
	for _, r := range l.runbooks {
		if r.Category == cat {
			result = append(result, r)
		}
	}
	return result
}

// ByPriority returns runbooks by priority.
func (l *RunbookLibrary) ByPriority(p Priority) []*Runbook {
	result := make([]*Runbook, 0)
	for _, r := range l.runbooks {
		if r.Priority == p {
			result = append(result, r)
		}
	}
	return result
}

// RunbookRenderer renders runbook documentation.
type RunbookRenderer struct{}

// NewRunbookRenderer creates a renderer.
func NewRunbookRenderer() *RunbookRenderer {
	return &RunbookRenderer{}
}

// RenderRunbook renders a runbook to Markdown.
func (r *RunbookRenderer) RenderRunbook(w io.Writer, rb *Runbook) error {
	var sb strings.Builder

	// Header
	priority := priorityBadge(rb.Priority)
	fmt.Fprintf(&sb, "# %s %s\n\n", rb.Title, priority)

	if rb.Description != "" {
		sb.WriteString(rb.Description + "\n\n")
	}

	// Metadata
	sb.WriteString("---\n")
	fmt.Fprintf(&sb, "Category: %s\n", rb.Category)
	fmt.Fprintf(&sb, "Owner: %s\n", rb.Owner)
	fmt.Fprintf(&sb, "Last Updated: %s\n", rb.LastUpdated.Format("2006-01-02"))
	if !rb.LastTested.IsZero() {
		fmt.Fprintf(&sb, "Last Tested: %s\n", rb.LastTested.Format("2006-01-02"))
	}
	sb.WriteString("---\n\n")

	// Triggers
	if len(rb.Triggers) > 0 {
		sb.WriteString("## Triggers\n\n")
		for _, t := range rb.Triggers {
			fmt.Fprintf(&sb, "- **%s**: %s\n", t.Type, t.Description)
			if t.Condition != "" {
				sb.WriteString("  - Condition: `" + t.Condition + "`\n")
			}
			if t.AlertName != "" {
				sb.WriteString("  - Alert: " + t.AlertName + "\n")
			}
		}
		sb.WriteString("\n")
	}

	// Prerequisites
	if len(rb.Prerequisites) > 0 {
		sb.WriteString("## Prerequisites\n\n")
		for _, p := range rb.Prerequisites {
			required := ""
			if p.Required {
				required = " *(required)*"
			}
			sb.WriteString("- " + p.Description + required + "\n")
			if p.CheckCmd != "" {
				sb.WriteString("  ```bash\n  " + p.CheckCmd + "\n  ```\n")
			}
		}
		sb.WriteString("\n")
	}

	// Procedures
	if len(rb.Procedures) > 0 {
		sb.WriteString("## Procedures\n\n")
		for _, p := range rb.Procedures {
			r.renderProcedure(&sb, p)
		}
	}

	// Verification
	if len(rb.Verification) > 0 {
		sb.WriteString("## Verification\n\n")
		for _, v := range rb.Verification {
			automated := ""
			if v.Automated {
				automated = " 🤖"
			}
			sb.WriteString("### " + v.Description + automated + "\n\n")
			if v.Command != "" {
				sb.WriteString("```bash\n" + v.Command + "\n```\n\n")
			}
			if v.Expected != "" {
				sb.WriteString("*Expected:* " + v.Expected + "\n\n")
			}
		}
	}

	// Rollback
	if len(rb.Rollback) > 0 {
		sb.WriteString("## Rollback\n\n")
		sb.WriteString("> Use these steps if the procedure fails.\n\n")
		for _, step := range rb.Rollback {
			fmt.Fprintf(&sb, "**Step %d:** %s\n\n", step.Number, step.Action)
			if step.Condition != "" {
				sb.WriteString("*Condition:* " + step.Condition + "\n\n")
			}
			if step.Command != "" {
				sb.WriteString("```bash\n" + step.Command + "\n```\n\n")
			}
		}
	}

	// Escalation
	if rb.Escalation != nil {
		sb.WriteString("## Escalation\n\n")
		fmt.Fprintf(&sb, "Default SLA: %s\n\n", rb.Escalation.DefaultSLA)
		for _, level := range rb.Escalation.Levels {
			fmt.Fprintf(&sb, "### Level %d: %s\n\n", level.Level, level.Name)
			fmt.Fprintf(&sb, "- **Contact:** %s (%s)\n", level.Contact, level.Method)
			fmt.Fprintf(&sb, "- **Wait Time:** %s\n\n", level.WaitTime)
			if len(level.Actions) > 0 {
				sb.WriteString("**Actions:**\n")
				for _, a := range level.Actions {
					sb.WriteString("- " + a + "\n")
				}
				sb.WriteString("\n")
			}
		}
	}

	// References
	if len(rb.References) > 0 {
		sb.WriteString("## References\n\n")
		for _, ref := range rb.References {
			fmt.Fprintf(&sb, "- [%s](%s) (%s)\n", ref.Title, ref.URL, ref.Type)
		}
		sb.WriteString("\n")
	}

	_, err := w.Write([]byte(sb.String()))
	return err
}

// renderProcedure renders a procedure to Markdown.
func (r *RunbookRenderer) renderProcedure(sb *strings.Builder, p Procedure) {
	automated := ""
	if p.Automated {
		automated = " 🤖 Automated"
	}
	sb.WriteString("### " + p.Name + automated + "\n\n")

	if p.Description != "" {
		sb.WriteString(p.Description + "\n\n")
	}

	if p.Duration > 0 {
		fmt.Fprintf(sb, "⏱️ Estimated duration: %s\n\n", p.Duration)
	}

	for _, step := range p.Steps {
		fmt.Fprintf(sb, "**Step %d:** %s\n\n", step.Number, step.Action)

		if step.Command != "" {
			sb.WriteString("```bash\n" + step.Command + "\n```\n\n")
		}

		if step.Expected != "" {
			sb.WriteString("*Expected:* " + step.Expected + "\n\n")
		}

		if step.Warning != "" {
			sb.WriteString("> ⚠️ " + step.Warning + "\n\n")
		}

		if step.Retryable {
			fmt.Fprintf(sb, "*Retryable: up to %d times*\n\n", step.MaxRetries)
		}
	}
}

// RenderLibraryIndex renders a library index to Markdown.
func (r *RunbookRenderer) RenderLibraryIndex(w io.Writer, lib *RunbookLibrary) error {
	var sb strings.Builder

	sb.WriteString("# Runbook Library\n\n")

	// Group by category
	categories := make(map[Category][]*Runbook)
	for _, rb := range lib.List() {
		categories[rb.Category] = append(categories[rb.Category], rb)
	}

	for cat, runbooks := range categories {
		fmt.Fprintf(&sb, "## %s\n\n", cat)
		sb.WriteString("| Runbook | Priority | Owner | Last Tested |\n")
		sb.WriteString("|---------|----------|-------|-------------|\n")
		for _, rb := range runbooks {
			lastTested := "Never"
			if !rb.LastTested.IsZero() {
				lastTested = rb.LastTested.Format("2006-01-02")
			}
			fmt.Fprintf(&sb, "| [%s](#%s) | %s | %s | %s |\n",
				rb.Title, rb.ID, rb.Priority, rb.Owner, lastTested)
		}
		sb.WriteString("\n")
	}

	_, err := w.Write([]byte(sb.String()))
	return err
}

// priorityBadge returns a badge for priority.
func priorityBadge(p Priority) string {
	switch p {
	case PriorityCritical:
		return "🔴 P1"
	case PriorityHigh:
		return "🟠 P2"
	case PriorityMedium:
		return "🟡 P3"
	case PriorityLow:
		return "🟢 P4"
	default:
		return ""
	}
}
