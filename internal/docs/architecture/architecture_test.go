package architecture

import (
	"bytes"
	"strings"
	"testing"
)

func TestNewDocument(t *testing.T) {
	doc := NewDocument("vaultaire-arch", "Vaultaire Architecture").
		Description("System architecture overview").
		Version("1.0").
		Status(StatusApproved).
		Build()

	if doc.ID != "vaultaire-arch" {
		t.Errorf("expected ID 'vaultaire-arch', got %s", doc.ID)
	}
	if doc.Status != StatusApproved {
		t.Error("Status not set")
	}
	if doc.Version != "1.0" {
		t.Error("Version not set")
	}
}

func TestDocumentBuilder_Component(t *testing.T) {
	comp := NewComponent("api", "API Server", TypeService).
		Description("Main API server").
		Technology("Go").
		Technology("Chi").
		Interface("HTTP", "TCP", 8000, "Main API").
		Dependency("database").
		Owner("Platform Team").
		Build()

	doc := NewDocument("test", "Test").
		Component(comp).
		Build()

	if len(doc.Components) != 1 {
		t.Error("Component not added")
	}
	if len(doc.Components[0].Technologies) != 2 {
		t.Error("Technologies not set")
	}
}

func TestDocumentBuilder_Connection(t *testing.T) {
	doc := NewDocument("test", "Test").
		Connection(Connection{
			From:        "api",
			To:          "database",
			Protocol:    "TCP",
			Description: "Database queries",
			Sync:        true,
			DataFlow:    FlowBidirectional,
		}).
		Build()

	if len(doc.Connections) != 1 {
		t.Error("Connection not added")
	}
}

func TestDocumentBuilder_Decision(t *testing.T) {
	decision := NewDecision("001", "Use PostgreSQL").
		Status(DecisionAccepted).
		Context("Need a reliable database").
		Option("PostgreSQL", "Relational database",
			[]string{"ACID compliant", "Well supported"},
			[]string{"Complex scaling"}).
		Option("MongoDB", "Document database",
			[]string{"Flexible schema"},
			[]string{"Eventual consistency"}).
		Decision("Use PostgreSQL for data integrity").
		Consequence("Need to manage schema migrations").
		Date("2025-01-15").
		Build()

	doc := NewDocument("test", "Test").
		Decision(decision).
		Build()

	if len(doc.Decisions) != 1 {
		t.Error("Decision not added")
	}
	if len(doc.Decisions[0].Options) != 2 {
		t.Error("Options not set")
	}
}

func TestDocumentBuilder_Diagram(t *testing.T) {
	doc := NewDocument("test", "Test").
		Diagram(Diagram{
			ID:          "context",
			Title:       "System Context",
			Type:        DiagramContext,
			Description: "High-level system view",
			Source:      "graph TD\n  A[User] --> B[Vaultaire]",
			Format:      "mermaid",
		}).
		Build()

	if len(doc.Diagrams) != 1 {
		t.Error("Diagram not added")
	}
}

func TestNewComponent(t *testing.T) {
	comp := NewComponent("db", "PostgreSQL", TypeDatabase).
		Description("Primary database").
		Technology("PostgreSQL 15").
		Interface("SQL", "TCP", 5432, "SQL queries").
		Build()

	if comp.ID != "db" {
		t.Error("ID not set")
	}
	if comp.Type != TypeDatabase {
		t.Error("Type not set")
	}
	if len(comp.Interfaces) != 1 {
		t.Error("Interface not set")
	}
}

func TestNewDecision(t *testing.T) {
	decision := NewDecision("002", "API Framework").
		Status(DecisionAccepted).
		Context("Need an HTTP framework").
		Decision("Use Chi router").
		Consequence("Lightweight routing").
		Build()

	if decision.ID != "002" {
		t.Error("ID not set")
	}
	if decision.Status != DecisionAccepted {
		t.Error("Status not set")
	}
}

func TestArchitectureRenderer_RenderDocument(t *testing.T) {
	comp := NewComponent("api", "API Server", TypeService).
		Description("Handles HTTP requests").
		Technology("Go").
		Interface("HTTP", "TCP", 8000, "REST API").
		Dependency("db").
		Build()

	decision := NewDecision("001", "Storage Backend").
		Status(DecisionAccepted).
		Context("Need distributed storage").
		Option("S3", "Object storage", []string{"Scalable"}, []string{"Vendor lock-in"}).
		Decision("Use S3-compatible API").
		Consequence("Portable across providers").
		Date("2025-01-01").
		Build()

	doc := NewDocument("arch", "System Architecture").
		Description("Vaultaire system architecture").
		Version("1.0").
		Status(StatusApproved).
		Component(comp).
		Connection(Connection{
			From:        "api",
			To:          "db",
			Protocol:    "TCP",
			Description: "Database access",
			Sync:        true,
		}).
		Decision(decision).
		Diagram(Diagram{
			ID:          "overview",
			Title:       "System Overview",
			Type:        DiagramContainer,
			Description: "Container diagram",
			Source:      "graph LR\n  API --> DB",
			Format:      "mermaid",
		}).
		Build()

	renderer := NewArchitectureRenderer()
	var buf bytes.Buffer

	err := renderer.RenderDocument(&buf, doc)
	if err != nil {
		t.Fatalf("RenderDocument failed: %v", err)
	}

	output := buf.String()

	if !strings.Contains(output, "# System Architecture") {
		t.Error("Title not in output")
	}
	if !strings.Contains(output, "## Components") {
		t.Error("Components section not in output")
	}
	if !strings.Contains(output, "## Connections") {
		t.Error("Connections section not in output")
	}
	if !strings.Contains(output, "## Architecture Decisions") {
		t.Error("Decisions section not in output")
	}
	if !strings.Contains(output, "## Diagrams") {
		t.Error("Diagrams section not in output")
	}
	if !strings.Contains(output, "ADR-001") {
		t.Error("ADR not in output")
	}

	t.Logf("Document:\n%s", output)
}

func TestDecisionStatusBadge(t *testing.T) {
	tests := []struct {
		status   DecisionStatus
		contains string
	}{
		{DecisionProposed, "Proposed"},
		{DecisionAccepted, "Accepted"},
		{DecisionDeprecated, "Deprecated"},
		{DecisionSuperseded, "Superseded"},
	}

	for _, tt := range tests {
		result := decisionStatusBadge(tt.status)
		if !strings.Contains(result, tt.contains) {
			t.Errorf("decisionStatusBadge(%s) = %s, expected to contain %s",
				tt.status, result, tt.contains)
		}
	}
}

func TestComponentType(t *testing.T) {
	types := []ComponentType{
		TypeService,
		TypeDatabase,
		TypeCache,
		TypeQueue,
		TypeGateway,
		TypeStorage,
		TypeExternal,
		TypeLoadBalancer,
	}

	for _, ct := range types {
		if ct == "" {
			t.Error("component type should not be empty")
		}
	}
}

func TestDiagramType(t *testing.T) {
	types := []DiagramType{
		DiagramContext,
		DiagramContainer,
		DiagramComponent,
		DiagramDeployment,
		DiagramSequence,
		DiagramDataFlow,
	}

	for _, dt := range types {
		if dt == "" {
			t.Error("diagram type should not be empty")
		}
	}
}

func TestStatus(t *testing.T) {
	statuses := []Status{
		StatusDraft,
		StatusReview,
		StatusApproved,
		StatusArchived,
	}

	for _, s := range statuses {
		if s == "" {
			t.Error("status should not be empty")
		}
	}
}

func TestDataFlow(t *testing.T) {
	flows := []DataFlow{
		FlowUnidirectional,
		FlowBidirectional,
	}

	for _, f := range flows {
		if f == "" {
			t.Error("data flow should not be empty")
		}
	}
}

func TestInterface(t *testing.T) {
	iface := Interface{
		Name:        "REST",
		Protocol:    "HTTP",
		Port:        8000,
		Description: "REST API endpoint",
	}

	if iface.Port != 8000 {
		t.Error("Port not set")
	}
}

func TestOption(t *testing.T) {
	opt := Option{
		Name:        "Option A",
		Description: "First option",
		Pros:        []string{"Fast", "Simple"},
		Cons:        []string{"Limited"},
	}

	if len(opt.Pros) != 2 {
		t.Error("Pros not set")
	}
}
