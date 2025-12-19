package admin

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

func TestNewAdminGuide(t *testing.T) {
	guide := NewAdminGuide("install", "Installation Guide").
		Description("How to install Vaultaire").
		Category(CategoryInstallation).
		Audience(AudienceSysAdmin).
		Version("2.0").
		Build()

	if guide.ID != "install" {
		t.Errorf("expected ID 'install', got %s", guide.ID)
	}
	if guide.Category != CategoryInstallation {
		t.Error("Category not set")
	}
	if guide.Audience != AudienceSysAdmin {
		t.Error("Audience not set")
	}
}

func TestAdminGuideBuilder_Requirement(t *testing.T) {
	guide := NewAdminGuide("test", "Test").
		Requirement(RequirementHardware, "CPU", "4 cores", "8 cores").
		Requirement(RequirementSoftware, "Go", "1.21", "1.22").
		Build()

	if len(guide.Requirements) != 2 {
		t.Errorf("expected 2 requirements, got %d", len(guide.Requirements))
	}
}

func TestAdminGuideBuilder_Warning(t *testing.T) {
	guide := NewAdminGuide("test", "Test").
		Warning(WarnDanger, "Data Loss", "This will delete all data").
		Warning(WarnCaution, "Performance", "May impact performance").
		Build()

	if len(guide.Warnings) != 2 {
		t.Errorf("expected 2 warnings, got %d", len(guide.Warnings))
	}
}

func TestNewProcedure(t *testing.T) {
	proc := NewProcedure("backup", "Database Backup").
		Purpose("Create a backup of the database").
		Duration(30*time.Minute).
		Risk(RiskMedium).
		Prerequisite("Database is running").
		Prerequisite("Sufficient disk space").
		Step("Stop writes", "SET GLOBAL read_only = ON", "Query OK").
		Step("Create dump", "pg_dump -Fc vaultaire > backup.dump", "File created").
		Verify("Check file size", "ls -la backup.dump", "File > 0 bytes").
		RollbackStep("Resume writes", "SET GLOBAL read_only = OFF", "Query OK").
		Build()

	if proc.ID != "backup" {
		t.Error("ID not set")
	}
	if proc.Duration != 30*time.Minute {
		t.Error("Duration not set")
	}
	if proc.RiskLevel != RiskMedium {
		t.Error("RiskLevel not set")
	}
	if len(proc.Prerequisites) != 2 {
		t.Errorf("expected 2 prerequisites, got %d", len(proc.Prerequisites))
	}
	if len(proc.Steps) != 2 {
		t.Errorf("expected 2 steps, got %d", len(proc.Steps))
	}
	if len(proc.Verification) != 1 {
		t.Error("Verification not set")
	}
	if len(proc.Rollback) != 1 {
		t.Error("Rollback not set")
	}
}

func TestProcedureBuilder_StepWithWarning(t *testing.T) {
	proc := NewProcedure("test", "Test").
		StepWithWarning("Delete data", "rm -rf /data", "Files deleted", "Cannot be undone!").
		Build()

	if proc.Steps[0].Warning != "Cannot be undone!" {
		t.Error("Warning not set on step")
	}
}

func TestNewRunbook(t *testing.T) {
	runbook := NewRunbook("disk-full", "Disk Full Alert").
		Description("Handle disk full alerts").
		Trigger(TriggerAlert, "disk_usage > 90%", "Disk usage exceeds threshold").
		Contact("John Doe", "SRE Lead", "john@example.com", true).
		SLA(15*time.Minute, 4*time.Hour, "P2").
		Build()

	if runbook.ID != "disk-full" {
		t.Error("ID not set")
	}
	if len(runbook.Triggers) != 1 {
		t.Error("Trigger not set")
	}
	if len(runbook.Contacts) != 1 {
		t.Error("Contact not set")
	}
	if runbook.SLA == nil {
		t.Error("SLA not set")
	}
	if runbook.SLA.Priority != "P2" {
		t.Error("SLA priority not set")
	}
}

func TestRunbookBuilder_EscalationLevel(t *testing.T) {
	contacts := []Contact{{Name: "Manager", Email: "mgr@example.com"}}
	actions := []string{"Page on-call", "Create incident"}

	runbook := NewRunbook("test", "Test").
		EscalationLevel(1, 15, contacts, actions).
		EscalationLevel(2, 30, contacts, []string{"Page VP"}).
		Build()

	if len(runbook.Escalation) != 2 {
		t.Errorf("expected 2 escalation levels, got %d", len(runbook.Escalation))
	}
	if runbook.Escalation[0].WaitMinutes != 15 {
		t.Error("WaitMinutes not set")
	}
}

func TestAdminRenderer_RenderGuideMarkdown(t *testing.T) {
	proc := NewProcedure("install", "Install Vaultaire").
		Purpose("Install Vaultaire server").
		Duration(15*time.Minute).
		Risk(RiskLow).
		Prerequisite("Docker installed").
		Step("Pull image", "docker pull vaultaire/server", "Image downloaded").
		Verify("Check running", "docker ps", "Container listed").
		RollbackStep("Remove container", "docker rm -f vaultaire", "Container removed").
		Build()

	guide := NewAdminGuide("install", "Installation Guide").
		Description("Complete installation guide").
		Category(CategoryInstallation).
		Audience(AudienceSysAdmin).
		Requirement(RequirementHardware, "RAM", "4GB", "8GB").
		Warning(WarnCaution, "Backup First", "Backup existing data").
		Procedure(proc).
		Build()

	renderer := NewAdminRenderer()
	var buf bytes.Buffer

	err := renderer.RenderGuideMarkdown(&buf, guide)
	if err != nil {
		t.Fatalf("RenderGuideMarkdown failed: %v", err)
	}

	output := buf.String()

	if !strings.Contains(output, "# Installation Guide") {
		t.Error("Title not in output")
	}
	if !strings.Contains(output, "## Requirements") {
		t.Error("Requirements section not in output")
	}
	if !strings.Contains(output, "## Install Vaultaire") {
		t.Error("Procedure not in output")
	}
	if !strings.Contains(output, "### Rollback Procedure") {
		t.Error("Rollback not in output")
	}

	t.Logf("Guide Markdown:\n%s", output)
}

func TestAdminRenderer_RenderRunbookMarkdown(t *testing.T) {
	proc := NewProcedure("cleanup", "Disk Cleanup").
		Step("Find large files", "du -sh /* | sort -hr | head", "List of files").
		Step("Remove logs", "rm -rf /var/log/*.gz", "Logs removed").
		Build()

	runbook := NewRunbook("disk-full", "Disk Full Response").
		Description("Steps to handle disk full alerts").
		Trigger(TriggerAlert, "disk > 90%", "High disk usage").
		Contact("SRE Team", "On-call", "sre@example.com", true).
		SLA(15*time.Minute, 2*time.Hour, "P1").
		EscalationLevel(1, 15, nil, []string{"Page backup on-call"}).
		Procedure(proc).
		Build()

	renderer := NewAdminRenderer()
	var buf bytes.Buffer

	err := renderer.RenderRunbookMarkdown(&buf, runbook)
	if err != nil {
		t.Fatalf("RenderRunbookMarkdown failed: %v", err)
	}

	output := buf.String()

	if !strings.Contains(output, "# Runbook: Disk Full Response") {
		t.Error("Title not in output")
	}
	if !strings.Contains(output, "## SLA") {
		t.Error("SLA section not in output")
	}
	if !strings.Contains(output, "## Triggers") {
		t.Error("Triggers section not in output")
	}
	if !strings.Contains(output, "## Escalation Path") {
		t.Error("Escalation not in output")
	}

	t.Logf("Runbook Markdown:\n%s", output)
}

func TestWarningIcon(t *testing.T) {
	tests := []struct {
		level    WarningLevel
		expected string
	}{
		{WarnCaution, "⚡"},
		{WarnWarning, "⚠️"},
		{WarnDanger, "🚨"},
		{WarnCritical, "☠️"},
	}

	for _, tt := range tests {
		result := warningIcon(tt.level)
		if result != tt.expected {
			t.Errorf("warningIcon(%s) = %s, expected %s", tt.level, result, tt.expected)
		}
	}
}

func TestRiskBadge(t *testing.T) {
	tests := []struct {
		level    RiskLevel
		contains string
	}{
		{RiskLow, "Low"},
		{RiskMedium, "Medium"},
		{RiskHigh, "High"},
		{RiskCritical, "Critical"},
	}

	for _, tt := range tests {
		result := riskBadge(tt.level)
		if !strings.Contains(result, tt.contains) {
			t.Errorf("riskBadge(%s) = %s, expected to contain %s", tt.level, result, tt.contains)
		}
	}
}

func TestAdminCategory(t *testing.T) {
	categories := []AdminCategory{
		CategoryInstallation,
		CategoryConfiguration,
		CategoryMaintenance,
		CategoryBackupRestore,
		CategoryMonitoring,
		CategoryScaling,
		CategoryUpgrade,
		CategoryDisasterRecovery,
		CategoryUserManagement,
		CategorySecurityAdmin,
	}

	for _, cat := range categories {
		if cat == "" {
			t.Error("category should not be empty")
		}
	}
}

func TestAudience(t *testing.T) {
	audiences := []Audience{
		AudienceSysAdmin,
		AudienceDevOps,
		AudienceDBA,
		AudienceSecurityOps,
		AudienceSupport,
	}

	for _, aud := range audiences {
		if aud == "" {
			t.Error("audience should not be empty")
		}
	}
}

func TestTriggerType(t *testing.T) {
	types := []TriggerType{
		TriggerAlert,
		TriggerSchedule,
		TriggerManual,
		TriggerIncident,
	}

	for _, tt := range types {
		if tt == "" {
			t.Error("trigger type should not be empty")
		}
	}
}

func TestRequirementType(t *testing.T) {
	types := []RequirementType{
		RequirementHardware,
		RequirementSoftware,
		RequirementNetwork,
		RequirementPermission,
		RequirementKnowledge,
	}

	for _, rt := range types {
		if rt == "" {
			t.Error("requirement type should not be empty")
		}
	}
}

func TestStep(t *testing.T) {
	step := Step{
		Number:     1,
		Action:     "Run command",
		Command:    "echo hello",
		Expected:   "hello",
		Notes:      "Simple test",
		Warning:    "None",
		Screenshot: "step1.png",
	}

	if step.Number != 1 {
		t.Error("Number not set")
	}
	if step.Screenshot != "step1.png" {
		t.Error("Screenshot not set")
	}
}

func TestContact(t *testing.T) {
	contact := Contact{
		Name:    "John",
		Role:    "SRE",
		Email:   "john@example.com",
		Phone:   "555-1234",
		Slack:   "@john",
		OnCall:  true,
		Primary: true,
	}

	if !contact.OnCall {
		t.Error("OnCall not set")
	}
	if contact.Slack != "@john" {
		t.Error("Slack not set")
	}
}
