package runbooks

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

func TestNewRunbook(t *testing.T) {
	rb := NewRunbook("disk-full", "Disk Full Response").
		Description("Handle disk full alerts").
		Category(CategoryIncident).
		Priority(PriorityCritical).
		Owner("SRE Team").
		Build()

	if rb.ID != "disk-full" {
		t.Errorf("expected ID 'disk-full', got %s", rb.ID)
	}
	if rb.Category != CategoryIncident {
		t.Error("Category not set")
	}
	if rb.Priority != PriorityCritical {
		t.Error("Priority not set")
	}
}

func TestRunbookBuilder_Trigger(t *testing.T) {
	rb := NewRunbook("test", "Test").
		Trigger(Trigger{
			Type:        TriggerAlert,
			Condition:   "disk_usage > 90%",
			Description: "Disk usage exceeds threshold",
			AlertName:   "DiskFullAlert",
		}).
		Build()

	if len(rb.Triggers) != 1 {
		t.Error("Trigger not added")
	}
	if rb.Triggers[0].AlertName != "DiskFullAlert" {
		t.Error("AlertName not set")
	}
}

func TestRunbookBuilder_Prerequisite(t *testing.T) {
	rb := NewRunbook("test", "Test").
		Prerequisite("SSH access to servers", true, "ssh user@server 'echo ok'").
		Prerequisite("kubectl configured", false, "kubectl get nodes").
		Build()

	if len(rb.Prerequisites) != 2 {
		t.Errorf("expected 2 prerequisites, got %d", len(rb.Prerequisites))
	}
	if !rb.Prerequisites[0].Required {
		t.Error("Required not set")
	}
}

func TestRunbookBuilder_Verification(t *testing.T) {
	rb := NewRunbook("test", "Test").
		Verification("Check service health", "curl localhost/health", "200 OK", true).
		Build()

	if len(rb.Verification) != 1 {
		t.Error("Verification not added")
	}
	if !rb.Verification[0].Automated {
		t.Error("Automated not set")
	}
}

func TestRunbookBuilder_RollbackStep(t *testing.T) {
	rb := NewRunbook("test", "Test").
		RollbackStep(1, "If deployment fails", "Revert to previous version", "kubectl rollout undo").
		Build()

	if len(rb.Rollback) != 1 {
		t.Error("RollbackStep not added")
	}
}

func TestRunbookBuilder_Reference(t *testing.T) {
	rb := NewRunbook("test", "Test").
		Reference("Architecture Docs", "https://docs.example.com/arch", "documentation").
		Build()

	if len(rb.References) != 1 {
		t.Error("Reference not added")
	}
}

func TestNewProcedure(t *testing.T) {
	proc := NewProcedure("Clean Up Disk").
		Description("Remove unnecessary files").
		Duration(15*time.Minute).
		Automated().
		Step("Find large files", "du -sh /* | sort -hr | head", "List of files").
		StepWithWarning("Remove logs", "rm -rf /var/log/*.gz", "Logs removed", "Cannot undo").
		StepWithRetry("Restart service", "systemctl restart app", "Service running", 3).
		Build()

	if proc.Name != "Clean Up Disk" {
		t.Error("Name not set")
	}
	if !proc.Automated {
		t.Error("Automated not set")
	}
	if len(proc.Steps) != 3 {
		t.Errorf("expected 3 steps, got %d", len(proc.Steps))
	}
	if proc.Steps[1].Warning == "" {
		t.Error("Warning not set on step")
	}
	if !proc.Steps[2].Retryable {
		t.Error("Retryable not set")
	}
}

func TestNewEscalation(t *testing.T) {
	esc := NewEscalation(30*time.Minute).
		Level(1, "On-Call Engineer", "oncall@example.com", "PagerDuty", 15*time.Minute, []string{"Investigate", "Mitigate"}).
		Level(2, "Team Lead", "lead@example.com", "Phone", 30*time.Minute, []string{"Escalate to management"}).
		Build()

	if esc.DefaultSLA != 30*time.Minute {
		t.Error("DefaultSLA not set")
	}
	if len(esc.Levels) != 2 {
		t.Errorf("expected 2 levels, got %d", len(esc.Levels))
	}
}

func TestRunbookLibrary(t *testing.T) {
	lib := NewRunbookLibrary()

	rb1 := NewRunbook("disk-full", "Disk Full").
		Category(CategoryIncident).
		Priority(PriorityCritical).
		Build()

	rb2 := NewRunbook("deploy", "Deployment").
		Category(CategoryDeployment).
		Priority(PriorityMedium).
		Build()

	rb3 := NewRunbook("backup", "Backup").
		Category(CategoryBackup).
		Priority(PriorityMedium).
		Build()

	lib.Add(rb1)
	lib.Add(rb2)
	lib.Add(rb3)

	// Get
	rb, ok := lib.Get("disk-full")
	if !ok {
		t.Error("Runbook not found")
	}
	if rb.Title != "Disk Full" {
		t.Error("Wrong runbook returned")
	}

	// List
	all := lib.List()
	if len(all) != 3 {
		t.Errorf("expected 3 runbooks, got %d", len(all))
	}

	// ByCategory
	incidents := lib.ByCategory(CategoryIncident)
	if len(incidents) != 1 {
		t.Errorf("expected 1 incident runbook, got %d", len(incidents))
	}

	// ByPriority
	medium := lib.ByPriority(PriorityMedium)
	if len(medium) != 2 {
		t.Errorf("expected 2 medium priority runbooks, got %d", len(medium))
	}
}

func TestRunbookRenderer_RenderRunbook(t *testing.T) {
	proc := NewProcedure("Cleanup").
		Description("Clean up disk space").
		Duration(10*time.Minute).
		Step("Check disk", "df -h", "Usage displayed").
		StepWithWarning("Remove temp", "rm -rf /tmp/*", "Temp cleared", "May affect running processes").
		Build()

	esc := NewEscalation(30*time.Minute).
		Level(1, "On-Call", "oncall@example.com", "Slack", 15*time.Minute, []string{"Triage"}).
		Build()

	rb := NewRunbook("disk-full", "Disk Full Response").
		Description("Handle disk full alerts").
		Category(CategoryIncident).
		Priority(PriorityCritical).
		Owner("SRE Team").
		LastTested(time.Now().Add(-24*time.Hour)).
		Trigger(Trigger{
			Type:        TriggerAlert,
			Condition:   "disk > 90%",
			Description: "High disk usage",
		}).
		Prerequisite("SSH access", true, "ssh server 'echo ok'").
		Procedure(proc).
		Verification("Check disk usage", "df -h /", "Usage < 80%", true).
		RollbackStep(1, "If cleanup fails", "Add disk space", "lvextend -L +10G /dev/data").
		Escalation(esc).
		Reference("Disk Management Guide", "https://docs.example.com/disk", "guide").
		Build()

	renderer := NewRunbookRenderer()
	var buf bytes.Buffer

	err := renderer.RenderRunbook(&buf, rb)
	if err != nil {
		t.Fatalf("RenderRunbook failed: %v", err)
	}

	output := buf.String()

	if !strings.Contains(output, "# Disk Full Response") {
		t.Error("Title not in output")
	}
	if !strings.Contains(output, "🔴 P1") {
		t.Error("Priority badge not in output")
	}
	if !strings.Contains(output, "## Triggers") {
		t.Error("Triggers section not in output")
	}
	if !strings.Contains(output, "## Prerequisites") {
		t.Error("Prerequisites section not in output")
	}
	if !strings.Contains(output, "## Procedures") {
		t.Error("Procedures section not in output")
	}
	if !strings.Contains(output, "## Verification") {
		t.Error("Verification section not in output")
	}
	if !strings.Contains(output, "## Rollback") {
		t.Error("Rollback section not in output")
	}
	if !strings.Contains(output, "## Escalation") {
		t.Error("Escalation section not in output")
	}
	if !strings.Contains(output, "## References") {
		t.Error("References section not in output")
	}

	t.Logf("Runbook:\n%s", output)
}

func TestRunbookRenderer_RenderLibraryIndex(t *testing.T) {
	lib := NewRunbookLibrary()
	lib.Add(NewRunbook("rb1", "Runbook 1").Category(CategoryIncident).Priority(PriorityCritical).Owner("Team A").Build())
	lib.Add(NewRunbook("rb2", "Runbook 2").Category(CategoryIncident).Priority(PriorityHigh).Owner("Team B").Build())

	renderer := NewRunbookRenderer()
	var buf bytes.Buffer

	err := renderer.RenderLibraryIndex(&buf, lib)
	if err != nil {
		t.Fatalf("RenderLibraryIndex failed: %v", err)
	}

	output := buf.String()

	if !strings.Contains(output, "# Runbook Library") {
		t.Error("Title not in output")
	}
	if !strings.Contains(output, "Runbook 1") {
		t.Error("Runbook 1 not in output")
	}

	t.Logf("Library Index:\n%s", output)
}

func TestPriorityBadge(t *testing.T) {
	tests := []struct {
		priority Priority
		contains string
	}{
		{PriorityCritical, "P1"},
		{PriorityHigh, "P2"},
		{PriorityMedium, "P3"},
		{PriorityLow, "P4"},
	}

	for _, tt := range tests {
		result := priorityBadge(tt.priority)
		if !strings.Contains(result, tt.contains) {
			t.Errorf("priorityBadge(%s) = %s, expected to contain %s", tt.priority, result, tt.contains)
		}
	}
}

func TestCategory(t *testing.T) {
	categories := []Category{
		CategoryIncident,
		CategoryDeployment,
		CategoryMaintenance,
		CategoryRecovery,
		CategoryScaling,
		CategorySecurity,
		CategoryBackup,
	}

	for _, c := range categories {
		if c == "" {
			t.Error("category should not be empty")
		}
	}
}

func TestTriggerType(t *testing.T) {
	types := []TriggerType{
		TriggerAlert,
		TriggerScheduled,
		TriggerManual,
		TriggerAutomatic,
	}

	for _, tt := range types {
		if tt == "" {
			t.Error("trigger type should not be empty")
		}
	}
}

func TestStep(t *testing.T) {
	step := Step{
		Number:     1,
		Action:     "Do something",
		Command:    "cmd",
		Expected:   "result",
		Warning:    "careful",
		Timeout:    30 * time.Second,
		Retryable:  true,
		MaxRetries: 3,
	}

	if step.Timeout != 30*time.Second {
		t.Error("Timeout not set")
	}
}

func TestEscalationLevel(t *testing.T) {
	level := EscalationLevel{
		Level:    1,
		Name:     "Primary",
		Contact:  "team@example.com",
		Method:   "Email",
		WaitTime: 15 * time.Minute,
		Actions:  []string{"Investigate", "Mitigate"},
	}

	if len(level.Actions) != 2 {
		t.Error("Actions not set")
	}
}
