// Package admin provides utilities for administrator documentation.
package admin

import (
	"fmt"
	"io"
	"strings"
	"time"
)

// AdminGuide represents an administrator guide.
type AdminGuide struct {
	ID           string
	Title        string
	Description  string
	Category     AdminCategory
	Audience     Audience
	Procedures   []Procedure
	Requirements []Requirement
	Warnings     []Warning
	LastReviewed time.Time
	Version      string
}

// AdminCategory categorizes admin guides.
type AdminCategory string

const (
	CategoryInstallation     AdminCategory = "installation"
	CategoryConfiguration    AdminCategory = "configuration"
	CategoryMaintenance      AdminCategory = "maintenance"
	CategoryBackupRestore    AdminCategory = "backup-restore"
	CategoryMonitoring       AdminCategory = "monitoring"
	CategoryScaling          AdminCategory = "scaling"
	CategoryUpgrade          AdminCategory = "upgrade"
	CategoryDisasterRecovery AdminCategory = "disaster-recovery"
	CategoryUserManagement   AdminCategory = "user-management"
	CategorySecurityAdmin    AdminCategory = "security"
)

// Audience indicates who the guide is for.
type Audience string

const (
	AudienceSysAdmin    Audience = "system-administrator"
	AudienceDevOps      Audience = "devops"
	AudienceDBA         Audience = "database-administrator"
	AudienceSecurityOps Audience = "security-operations"
	AudienceSupport     Audience = "support"
)

// Procedure represents a step-by-step procedure.
type Procedure struct {
	ID            string
	Title         string
	Purpose       string
	Prerequisites []string
	Steps         []Step
	Verification  []VerificationStep
	Rollback      []Step
	Duration      time.Duration
	RiskLevel     RiskLevel
}

// Step represents a single procedural step.
type Step struct {
	Number     int
	Action     string
	Command    string
	Expected   string
	Notes      string
	Warning    string
	Screenshot string
}

// VerificationStep validates procedure success.
type VerificationStep struct {
	Description string
	Command     string
	Expected    string
}

// RiskLevel indicates procedure risk.
type RiskLevel string

const (
	RiskLow      RiskLevel = "low"
	RiskMedium   RiskLevel = "medium"
	RiskHigh     RiskLevel = "high"
	RiskCritical RiskLevel = "critical"
)

// Requirement specifies prerequisites.
type Requirement struct {
	Type        RequirementType
	Description string
	Minimum     string
	Recommended string
}

// RequirementType categorizes requirements.
type RequirementType string

const (
	RequirementHardware   RequirementType = "hardware"
	RequirementSoftware   RequirementType = "software"
	RequirementNetwork    RequirementType = "network"
	RequirementPermission RequirementType = "permission"
	RequirementKnowledge  RequirementType = "knowledge"
)

// Warning represents an important warning.
type Warning struct {
	Severity WarningLevel
	Title    string
	Content  string
}

// WarningLevel indicates warning severity.
type WarningLevel string

const (
	WarnCaution  WarningLevel = "caution"
	WarnWarning  WarningLevel = "warning"
	WarnDanger   WarningLevel = "danger"
	WarnCritical WarningLevel = "critical"
)

// AdminGuideBuilder helps construct admin guides.
type AdminGuideBuilder struct {
	guide *AdminGuide
}

// NewAdminGuide creates an admin guide builder.
func NewAdminGuide(id, title string) *AdminGuideBuilder {
	return &AdminGuideBuilder{
		guide: &AdminGuide{
			ID:           id,
			Title:        title,
			Procedures:   make([]Procedure, 0),
			Requirements: make([]Requirement, 0),
			Warnings:     make([]Warning, 0),
			LastReviewed: time.Now(),
			Version:      "1.0",
		},
	}
}

// Description sets the description.
func (b *AdminGuideBuilder) Description(desc string) *AdminGuideBuilder {
	b.guide.Description = desc
	return b
}

// Category sets the category.
func (b *AdminGuideBuilder) Category(cat AdminCategory) *AdminGuideBuilder {
	b.guide.Category = cat
	return b
}

// Audience sets the target audience.
func (b *AdminGuideBuilder) Audience(aud Audience) *AdminGuideBuilder {
	b.guide.Audience = aud
	return b
}

// Version sets the version.
func (b *AdminGuideBuilder) Version(ver string) *AdminGuideBuilder {
	b.guide.Version = ver
	return b
}

// Requirement adds a requirement.
func (b *AdminGuideBuilder) Requirement(reqType RequirementType, desc, min, rec string) *AdminGuideBuilder {
	b.guide.Requirements = append(b.guide.Requirements, Requirement{
		Type:        reqType,
		Description: desc,
		Minimum:     min,
		Recommended: rec,
	})
	return b
}

// Warning adds a warning.
func (b *AdminGuideBuilder) Warning(level WarningLevel, title, content string) *AdminGuideBuilder {
	b.guide.Warnings = append(b.guide.Warnings, Warning{
		Severity: level,
		Title:    title,
		Content:  content,
	})
	return b
}

// Procedure adds a procedure.
func (b *AdminGuideBuilder) Procedure(proc Procedure) *AdminGuideBuilder {
	b.guide.Procedures = append(b.guide.Procedures, proc)
	return b
}

// Build returns the completed guide.
func (b *AdminGuideBuilder) Build() *AdminGuide {
	return b.guide
}

// ProcedureBuilder helps construct procedures.
type ProcedureBuilder struct {
	proc *Procedure
}

// NewProcedure creates a procedure builder.
func NewProcedure(id, title string) *ProcedureBuilder {
	return &ProcedureBuilder{
		proc: &Procedure{
			ID:            id,
			Title:         title,
			Prerequisites: make([]string, 0),
			Steps:         make([]Step, 0),
			Verification:  make([]VerificationStep, 0),
			Rollback:      make([]Step, 0),
			RiskLevel:     RiskLow,
		},
	}
}

// Purpose sets the procedure purpose.
func (b *ProcedureBuilder) Purpose(purpose string) *ProcedureBuilder {
	b.proc.Purpose = purpose
	return b
}

// Duration sets estimated duration.
func (b *ProcedureBuilder) Duration(d time.Duration) *ProcedureBuilder {
	b.proc.Duration = d
	return b
}

// Risk sets the risk level.
func (b *ProcedureBuilder) Risk(level RiskLevel) *ProcedureBuilder {
	b.proc.RiskLevel = level
	return b
}

// Prerequisite adds a prerequisite.
func (b *ProcedureBuilder) Prerequisite(prereq string) *ProcedureBuilder {
	b.proc.Prerequisites = append(b.proc.Prerequisites, prereq)
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

// Verify adds a verification step.
func (b *ProcedureBuilder) Verify(desc, command, expected string) *ProcedureBuilder {
	b.proc.Verification = append(b.proc.Verification, VerificationStep{
		Description: desc,
		Command:     command,
		Expected:    expected,
	})
	return b
}

// RollbackStep adds a rollback step.
func (b *ProcedureBuilder) RollbackStep(action, command, expected string) *ProcedureBuilder {
	step := Step{
		Number:   len(b.proc.Rollback) + 1,
		Action:   action,
		Command:  command,
		Expected: expected,
	}
	b.proc.Rollback = append(b.proc.Rollback, step)
	return b
}

// Build returns the completed procedure.
func (b *ProcedureBuilder) Build() Procedure {
	return *b.proc
}

// Runbook represents an operational runbook.
type Runbook struct {
	ID          string
	Title       string
	Description string
	Triggers    []Trigger
	Procedures  []Procedure
	Contacts    []Contact
	Escalation  []EscalationLevel
	SLA         *SLA
}

// Trigger defines when a runbook should be executed.
type Trigger struct {
	Type        TriggerType
	Condition   string
	Description string
}

// TriggerType categorizes triggers.
type TriggerType string

const (
	TriggerAlert    TriggerType = "alert"
	TriggerSchedule TriggerType = "schedule"
	TriggerManual   TriggerType = "manual"
	TriggerIncident TriggerType = "incident"
)

// Contact represents a team contact.
type Contact struct {
	Name    string
	Role    string
	Email   string
	Phone   string
	Slack   string
	OnCall  bool
	Primary bool
}

// EscalationLevel defines escalation path.
type EscalationLevel struct {
	Level       int
	WaitMinutes int
	Contacts    []Contact
	Actions     []string
}

// SLA defines service level agreement.
type SLA struct {
	ResponseTime   time.Duration
	ResolutionTime time.Duration
	Priority       string
}

// RunbookBuilder helps construct runbooks.
type RunbookBuilder struct {
	runbook *Runbook
}

// NewRunbook creates a runbook builder.
func NewRunbook(id, title string) *RunbookBuilder {
	return &RunbookBuilder{
		runbook: &Runbook{
			ID:         id,
			Title:      title,
			Triggers:   make([]Trigger, 0),
			Procedures: make([]Procedure, 0),
			Contacts:   make([]Contact, 0),
			Escalation: make([]EscalationLevel, 0),
		},
	}
}

// Description sets the description.
func (b *RunbookBuilder) Description(desc string) *RunbookBuilder {
	b.runbook.Description = desc
	return b
}

// Trigger adds a trigger.
func (b *RunbookBuilder) Trigger(triggerType TriggerType, condition, desc string) *RunbookBuilder {
	b.runbook.Triggers = append(b.runbook.Triggers, Trigger{
		Type:        triggerType,
		Condition:   condition,
		Description: desc,
	})
	return b
}

// Procedure adds a procedure.
func (b *RunbookBuilder) Procedure(proc Procedure) *RunbookBuilder {
	b.runbook.Procedures = append(b.runbook.Procedures, proc)
	return b
}

// Contact adds a contact.
func (b *RunbookBuilder) Contact(name, role, email string, primary bool) *RunbookBuilder {
	b.runbook.Contacts = append(b.runbook.Contacts, Contact{
		Name:    name,
		Role:    role,
		Email:   email,
		Primary: primary,
	})
	return b
}

// EscalationLevel adds an escalation level.
func (b *RunbookBuilder) EscalationLevel(level, waitMins int, contacts []Contact, actions []string) *RunbookBuilder {
	b.runbook.Escalation = append(b.runbook.Escalation, EscalationLevel{
		Level:       level,
		WaitMinutes: waitMins,
		Contacts:    contacts,
		Actions:     actions,
	})
	return b
}

// SLA sets the SLA.
func (b *RunbookBuilder) SLA(response, resolution time.Duration, priority string) *RunbookBuilder {
	b.runbook.SLA = &SLA{
		ResponseTime:   response,
		ResolutionTime: resolution,
		Priority:       priority,
	}
	return b
}

// Build returns the completed runbook.
func (b *RunbookBuilder) Build() *Runbook {
	return b.runbook
}

// AdminRenderer renders admin documentation.
type AdminRenderer struct{}

// NewAdminRenderer creates an admin renderer.
func NewAdminRenderer() *AdminRenderer {
	return &AdminRenderer{}
}

// RenderGuideMarkdown renders an admin guide to Markdown.
func (r *AdminRenderer) RenderGuideMarkdown(w io.Writer, guide *AdminGuide) error {
	var sb strings.Builder

	// Header
	sb.WriteString("# " + guide.Title + "\n\n")
	if guide.Description != "" {
		sb.WriteString(guide.Description + "\n\n")
	}

	// Metadata
	sb.WriteString("---\n")
	sb.WriteString(fmt.Sprintf("Category: %s\n", guide.Category))
	sb.WriteString(fmt.Sprintf("Audience: %s\n", guide.Audience))
	sb.WriteString(fmt.Sprintf("Version: %s\n", guide.Version))
	sb.WriteString(fmt.Sprintf("Last Reviewed: %s\n", guide.LastReviewed.Format("2006-01-02")))
	sb.WriteString("---\n\n")

	// Warnings
	if len(guide.Warnings) > 0 {
		sb.WriteString("## ⚠️ Important Warnings\n\n")
		for _, warn := range guide.Warnings {
			icon := warningIcon(warn.Severity)
			sb.WriteString(fmt.Sprintf("> %s **%s**: %s\n>\n", icon, warn.Title, warn.Content))
		}
		sb.WriteString("\n")
	}

	// Requirements
	if len(guide.Requirements) > 0 {
		sb.WriteString("## Requirements\n\n")
		sb.WriteString("| Type | Description | Minimum | Recommended |\n")
		sb.WriteString("|------|-------------|---------|-------------|\n")
		for _, req := range guide.Requirements {
			sb.WriteString(fmt.Sprintf("| %s | %s | %s | %s |\n",
				req.Type, req.Description, req.Minimum, req.Recommended))
		}
		sb.WriteString("\n")
	}

	// Procedures
	for _, proc := range guide.Procedures {
		r.renderProcedureMarkdown(&sb, proc)
	}

	_, err := w.Write([]byte(sb.String()))
	return err
}

// renderProcedureMarkdown renders a procedure to Markdown.
func (r *AdminRenderer) renderProcedureMarkdown(sb *strings.Builder, proc Procedure) {
	sb.WriteString("## " + proc.Title + "\n\n")

	if proc.Purpose != "" {
		sb.WriteString("**Purpose:** " + proc.Purpose + "\n\n")
	}

	// Metadata
	fmt.Fprintf(sb, "- **Duration:** %s\n", proc.Duration)
	fmt.Fprintf(sb, "- **Risk Level:** %s\n\n", riskBadge(proc.RiskLevel))

	// Prerequisites
	if len(proc.Prerequisites) > 0 {
		sb.WriteString("### Prerequisites\n\n")
		for _, prereq := range proc.Prerequisites {
			sb.WriteString("- " + prereq + "\n")
		}
		sb.WriteString("\n")
	}

	// Steps
	if len(proc.Steps) > 0 {
		sb.WriteString("### Steps\n\n")
		for _, step := range proc.Steps {
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
		}
	}

	// Verification
	if len(proc.Verification) > 0 {
		sb.WriteString("### Verification\n\n")
		for _, v := range proc.Verification {
			sb.WriteString("- **" + v.Description + "**\n")
			if v.Command != "" {
				sb.WriteString("  ```bash\n  " + v.Command + "\n  ```\n")
			}
			sb.WriteString("  Expected: " + v.Expected + "\n\n")
		}
	}

	// Rollback
	if len(proc.Rollback) > 0 {
		sb.WriteString("### Rollback Procedure\n\n")
		sb.WriteString("> Use these steps if something goes wrong.\n\n")
		for _, step := range proc.Rollback {
			fmt.Fprintf(sb, "%d. %s\n", step.Number, step.Action)
			if step.Command != "" {
				sb.WriteString("   ```bash\n   " + step.Command + "\n   ```\n")
			}
		}
		sb.WriteString("\n")
	}
}

// RenderRunbookMarkdown renders a runbook to Markdown.
func (r *AdminRenderer) RenderRunbookMarkdown(w io.Writer, runbook *Runbook) error {
	var sb strings.Builder

	sb.WriteString("# Runbook: " + runbook.Title + "\n\n")
	if runbook.Description != "" {
		sb.WriteString(runbook.Description + "\n\n")
	}

	// SLA
	if runbook.SLA != nil {
		sb.WriteString("## SLA\n\n")
		sb.WriteString(fmt.Sprintf("- **Priority:** %s\n", runbook.SLA.Priority))
		sb.WriteString(fmt.Sprintf("- **Response Time:** %s\n", runbook.SLA.ResponseTime))
		sb.WriteString(fmt.Sprintf("- **Resolution Time:** %s\n\n", runbook.SLA.ResolutionTime))
	}

	// Triggers
	if len(runbook.Triggers) > 0 {
		sb.WriteString("## Triggers\n\n")
		for _, t := range runbook.Triggers {
			sb.WriteString(fmt.Sprintf("- **%s**: %s\n  - Condition: `%s`\n",
				t.Type, t.Description, t.Condition))
		}
		sb.WriteString("\n")
	}

	// Contacts
	if len(runbook.Contacts) > 0 {
		sb.WriteString("## Contacts\n\n")
		sb.WriteString("| Name | Role | Email | Primary |\n")
		sb.WriteString("|------|------|-------|---------|-------------|\n")
		for _, c := range runbook.Contacts {
			primary := ""
			if c.Primary {
				primary = "✓"
			}
			sb.WriteString(fmt.Sprintf("| %s | %s | %s | %s |\n",
				c.Name, c.Role, c.Email, primary))
		}
		sb.WriteString("\n")
	}

	// Escalation
	if len(runbook.Escalation) > 0 {
		sb.WriteString("## Escalation Path\n\n")
		for _, e := range runbook.Escalation {
			sb.WriteString(fmt.Sprintf("### Level %d (after %d minutes)\n\n", e.Level, e.WaitMinutes))
			sb.WriteString("**Actions:**\n")
			for _, action := range e.Actions {
				sb.WriteString("- " + action + "\n")
			}
			sb.WriteString("\n")
		}
	}

	// Procedures
	for _, proc := range runbook.Procedures {
		r.renderProcedureMarkdown(&sb, proc)
	}

	_, err := w.Write([]byte(sb.String()))
	return err
}

// warningIcon returns an icon for warning level.
func warningIcon(level WarningLevel) string {
	switch level {
	case WarnCaution:
		return "⚡"
	case WarnWarning:
		return "⚠️"
	case WarnDanger:
		return "🚨"
	case WarnCritical:
		return "☠️"
	default:
		return "ℹ️"
	}
}

// riskBadge returns a badge for risk level.
func riskBadge(level RiskLevel) string {
	switch level {
	case RiskLow:
		return "🟢 Low"
	case RiskMedium:
		return "🟡 Medium"
	case RiskHigh:
		return "🟠 High"
	case RiskCritical:
		return "🔴 Critical"
	default:
		return "Unknown"
	}
}
