// Package architecture provides utilities for architecture documentation.
package architecture

import (
	"fmt"
	"io"
	"strings"
)

// Document represents an architecture document.
type Document struct {
	ID          string
	Title       string
	Description string
	Version     string
	Status      Status
	Components  []Component
	Connections []Connection
	Decisions   []Decision
	Diagrams    []Diagram
}

// Status indicates document status.
type Status string

const (
	StatusDraft    Status = "draft"
	StatusReview   Status = "review"
	StatusApproved Status = "approved"
	StatusArchived Status = "archived"
)

// Component represents a system component.
type Component struct {
	ID           string
	Name         string
	Type         ComponentType
	Description  string
	Technologies []string
	Interfaces   []Interface
	Dependencies []string
	Owner        string
}

// ComponentType categorizes components.
type ComponentType string

const (
	TypeService      ComponentType = "service"
	TypeDatabase     ComponentType = "database"
	TypeCache        ComponentType = "cache"
	TypeQueue        ComponentType = "queue"
	TypeGateway      ComponentType = "gateway"
	TypeStorage      ComponentType = "storage"
	TypeExternal     ComponentType = "external"
	TypeLoadBalancer ComponentType = "load-balancer"
)

// Interface represents a component interface.
type Interface struct {
	Name        string
	Protocol    string
	Port        int
	Description string
}

// Connection represents a connection between components.
type Connection struct {
	From        string
	To          string
	Protocol    string
	Description string
	Sync        bool
	DataFlow    DataFlow
}

// DataFlow indicates data flow direction.
type DataFlow string

const (
	FlowUnidirectional DataFlow = "unidirectional"
	FlowBidirectional  DataFlow = "bidirectional"
)

// Decision represents an architecture decision record (ADR).
type Decision struct {
	ID           string
	Title        string
	Status       DecisionStatus
	Context      string
	Options      []Option
	Decision     string
	Consequences []string
	Date         string
}

// DecisionStatus indicates ADR status.
type DecisionStatus string

const (
	DecisionProposed   DecisionStatus = "proposed"
	DecisionAccepted   DecisionStatus = "accepted"
	DecisionDeprecated DecisionStatus = "deprecated"
	DecisionSuperseded DecisionStatus = "superseded"
)

// Option represents a decision option.
type Option struct {
	Name        string
	Description string
	Pros        []string
	Cons        []string
}

// Diagram represents an architecture diagram.
type Diagram struct {
	ID          string
	Title       string
	Type        DiagramType
	Description string
	Source      string
	Format      string
}

// DiagramType categorizes diagrams.
type DiagramType string

const (
	DiagramContext    DiagramType = "context"
	DiagramContainer  DiagramType = "container"
	DiagramComponent  DiagramType = "component"
	DiagramDeployment DiagramType = "deployment"
	DiagramSequence   DiagramType = "sequence"
	DiagramDataFlow   DiagramType = "data-flow"
)

// DocumentBuilder helps construct architecture documents.
type DocumentBuilder struct {
	doc *Document
}

// NewDocument creates a document builder.
func NewDocument(id, title string) *DocumentBuilder {
	return &DocumentBuilder{
		doc: &Document{
			ID:          id,
			Title:       title,
			Status:      StatusDraft,
			Components:  make([]Component, 0),
			Connections: make([]Connection, 0),
			Decisions:   make([]Decision, 0),
			Diagrams:    make([]Diagram, 0),
		},
	}
}

// Description sets the description.
func (b *DocumentBuilder) Description(desc string) *DocumentBuilder {
	b.doc.Description = desc
	return b
}

// Version sets the version.
func (b *DocumentBuilder) Version(ver string) *DocumentBuilder {
	b.doc.Version = ver
	return b
}

// Status sets the status.
func (b *DocumentBuilder) Status(s Status) *DocumentBuilder {
	b.doc.Status = s
	return b
}

// Component adds a component.
func (b *DocumentBuilder) Component(c Component) *DocumentBuilder {
	b.doc.Components = append(b.doc.Components, c)
	return b
}

// Connection adds a connection.
func (b *DocumentBuilder) Connection(c Connection) *DocumentBuilder {
	b.doc.Connections = append(b.doc.Connections, c)
	return b
}

// Decision adds a decision.
func (b *DocumentBuilder) Decision(d Decision) *DocumentBuilder {
	b.doc.Decisions = append(b.doc.Decisions, d)
	return b
}

// Diagram adds a diagram.
func (b *DocumentBuilder) Diagram(d Diagram) *DocumentBuilder {
	b.doc.Diagrams = append(b.doc.Diagrams, d)
	return b
}

// Build returns the completed document.
func (b *DocumentBuilder) Build() *Document {
	return b.doc
}

// ComponentBuilder helps construct components.
type ComponentBuilder struct {
	comp *Component
}

// NewComponent creates a component builder.
func NewComponent(id, name string, compType ComponentType) *ComponentBuilder {
	return &ComponentBuilder{
		comp: &Component{
			ID:           id,
			Name:         name,
			Type:         compType,
			Technologies: make([]string, 0),
			Interfaces:   make([]Interface, 0),
			Dependencies: make([]string, 0),
		},
	}
}

// Description sets the description.
func (b *ComponentBuilder) Description(desc string) *ComponentBuilder {
	b.comp.Description = desc
	return b
}

// Technology adds a technology.
func (b *ComponentBuilder) Technology(tech string) *ComponentBuilder {
	b.comp.Technologies = append(b.comp.Technologies, tech)
	return b
}

// Interface adds an interface.
func (b *ComponentBuilder) Interface(name, protocol string, port int, desc string) *ComponentBuilder {
	b.comp.Interfaces = append(b.comp.Interfaces, Interface{
		Name:        name,
		Protocol:    protocol,
		Port:        port,
		Description: desc,
	})
	return b
}

// Dependency adds a dependency.
func (b *ComponentBuilder) Dependency(dep string) *ComponentBuilder {
	b.comp.Dependencies = append(b.comp.Dependencies, dep)
	return b
}

// Owner sets the owner.
func (b *ComponentBuilder) Owner(owner string) *ComponentBuilder {
	b.comp.Owner = owner
	return b
}

// Build returns the completed component.
func (b *ComponentBuilder) Build() Component {
	return *b.comp
}

// DecisionBuilder helps construct decisions.
type DecisionBuilder struct {
	decision *Decision
}

// NewDecision creates a decision builder.
func NewDecision(id, title string) *DecisionBuilder {
	return &DecisionBuilder{
		decision: &Decision{
			ID:           id,
			Title:        title,
			Status:       DecisionProposed,
			Options:      make([]Option, 0),
			Consequences: make([]string, 0),
		},
	}
}

// Status sets the status.
func (b *DecisionBuilder) Status(s DecisionStatus) *DecisionBuilder {
	b.decision.Status = s
	return b
}

// Context sets the context.
func (b *DecisionBuilder) Context(ctx string) *DecisionBuilder {
	b.decision.Context = ctx
	return b
}

// Option adds an option.
func (b *DecisionBuilder) Option(name, desc string, pros, cons []string) *DecisionBuilder {
	b.decision.Options = append(b.decision.Options, Option{
		Name:        name,
		Description: desc,
		Pros:        pros,
		Cons:        cons,
	})
	return b
}

// Decision sets the decision.
func (b *DecisionBuilder) Decision(dec string) *DecisionBuilder {
	b.decision.Decision = dec
	return b
}

// Consequence adds a consequence.
func (b *DecisionBuilder) Consequence(cons string) *DecisionBuilder {
	b.decision.Consequences = append(b.decision.Consequences, cons)
	return b
}

// Date sets the date.
func (b *DecisionBuilder) Date(date string) *DecisionBuilder {
	b.decision.Date = date
	return b
}

// Build returns the completed decision.
func (b *DecisionBuilder) Build() Decision {
	return *b.decision
}

// ArchitectureRenderer renders architecture documentation.
type ArchitectureRenderer struct{}

// NewArchitectureRenderer creates a renderer.
func NewArchitectureRenderer() *ArchitectureRenderer {
	return &ArchitectureRenderer{}
}

// RenderDocument renders a document to Markdown.
func (r *ArchitectureRenderer) RenderDocument(w io.Writer, doc *Document) error {
	var sb strings.Builder

	// Header
	sb.WriteString("# " + doc.Title + "\n\n")
	if doc.Description != "" {
		sb.WriteString(doc.Description + "\n\n")
	}

	// Metadata
	sb.WriteString("---\n")
	sb.WriteString(fmt.Sprintf("Status: %s\n", doc.Status))
	if doc.Version != "" {
		sb.WriteString(fmt.Sprintf("Version: %s\n", doc.Version))
	}
	sb.WriteString("---\n\n")

	// Table of Contents
	sb.WriteString("## Contents\n\n")
	if len(doc.Components) > 0 {
		sb.WriteString("- [Components](#components)\n")
	}
	if len(doc.Connections) > 0 {
		sb.WriteString("- [Connections](#connections)\n")
	}
	if len(doc.Decisions) > 0 {
		sb.WriteString("- [Architecture Decisions](#architecture-decisions)\n")
	}
	if len(doc.Diagrams) > 0 {
		sb.WriteString("- [Diagrams](#diagrams)\n")
	}
	sb.WriteString("\n")

	// Components
	if len(doc.Components) > 0 {
		sb.WriteString("## Components\n\n")
		for _, c := range doc.Components {
			r.renderComponent(&sb, c)
		}
	}

	// Connections
	if len(doc.Connections) > 0 {
		sb.WriteString("## Connections\n\n")
		sb.WriteString("| From | To | Protocol | Description |\n")
		sb.WriteString("|------|-----|----------|-------------|\n")
		for _, conn := range doc.Connections {
			sync := ""
			if !conn.Sync {
				sync = " (async)"
			}
			fmt.Fprintf(&sb, "| %s | %s | %s%s | %s |\n",
				conn.From, conn.To, conn.Protocol, sync, conn.Description)
		}
		sb.WriteString("\n")
	}

	// Decisions
	if len(doc.Decisions) > 0 {
		sb.WriteString("## Architecture Decisions\n\n")
		for _, d := range doc.Decisions {
			r.renderDecision(&sb, d)
		}
	}

	// Diagrams
	if len(doc.Diagrams) > 0 {
		sb.WriteString("## Diagrams\n\n")
		for _, d := range doc.Diagrams {
			fmt.Fprintf(&sb, "### %s\n\n", d.Title)
			if d.Description != "" {
				sb.WriteString(d.Description + "\n\n")
			}
			sb.WriteString(fmt.Sprintf("*Type: %s*\n\n", d.Type))
			if d.Source != "" {
				sb.WriteString("```" + d.Format + "\n" + d.Source + "\n```\n\n")
			}
		}
	}

	_, err := w.Write([]byte(sb.String()))
	return err
}

// renderComponent renders a component to Markdown.
func (r *ArchitectureRenderer) renderComponent(sb *strings.Builder, c Component) {
	fmt.Fprintf(sb, "### %s\n\n", c.Name)

	fmt.Fprintf(sb, "**Type:** %s\n\n", c.Type)

	if c.Description != "" {
		sb.WriteString(c.Description + "\n\n")
	}

	if len(c.Technologies) > 0 {
		sb.WriteString("**Technologies:** " + strings.Join(c.Technologies, ", ") + "\n\n")
	}

	if len(c.Interfaces) > 0 {
		sb.WriteString("**Interfaces:**\n\n")
		sb.WriteString("| Name | Protocol | Port | Description |\n")
		sb.WriteString("|------|----------|------|-------------|\n")
		for _, i := range c.Interfaces {
			fmt.Fprintf(sb, "| %s | %s | %d | %s |\n",
				i.Name, i.Protocol, i.Port, i.Description)
		}
		sb.WriteString("\n")
	}

	if len(c.Dependencies) > 0 {
		sb.WriteString("**Dependencies:** " + strings.Join(c.Dependencies, ", ") + "\n\n")
	}

	if c.Owner != "" {
		sb.WriteString("**Owner:** " + c.Owner + "\n\n")
	}
}

// renderDecision renders a decision to Markdown.
func (r *ArchitectureRenderer) renderDecision(sb *strings.Builder, d Decision) {
	status := decisionStatusBadge(d.Status)
	fmt.Fprintf(sb, "### ADR-%s: %s %s\n\n", d.ID, d.Title, status)

	if d.Date != "" {
		sb.WriteString("*Date: " + d.Date + "*\n\n")
	}

	if d.Context != "" {
		sb.WriteString("#### Context\n\n" + d.Context + "\n\n")
	}

	if len(d.Options) > 0 {
		sb.WriteString("#### Options Considered\n\n")
		for _, opt := range d.Options {
			sb.WriteString("**" + opt.Name + "**\n\n")
			if opt.Description != "" {
				sb.WriteString(opt.Description + "\n\n")
			}
			if len(opt.Pros) > 0 {
				sb.WriteString("✅ Pros:\n")
				for _, pro := range opt.Pros {
					sb.WriteString("- " + pro + "\n")
				}
				sb.WriteString("\n")
			}
			if len(opt.Cons) > 0 {
				sb.WriteString("❌ Cons:\n")
				for _, con := range opt.Cons {
					sb.WriteString("- " + con + "\n")
				}
				sb.WriteString("\n")
			}
		}
	}

	if d.Decision != "" {
		sb.WriteString("#### Decision\n\n" + d.Decision + "\n\n")
	}

	if len(d.Consequences) > 0 {
		sb.WriteString("#### Consequences\n\n")
		for _, c := range d.Consequences {
			sb.WriteString("- " + c + "\n")
		}
		sb.WriteString("\n")
	}
}

// decisionStatusBadge returns a badge for decision status.
func decisionStatusBadge(s DecisionStatus) string {
	switch s {
	case DecisionProposed:
		return "🟡 Proposed"
	case DecisionAccepted:
		return "🟢 Accepted"
	case DecisionDeprecated:
		return "🟠 Deprecated"
	case DecisionSuperseded:
		return "🔵 Superseded"
	default:
		return ""
	}
}
