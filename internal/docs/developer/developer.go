// Package developer provides utilities for developer documentation.
package developer

import (
	"fmt"
	"io"
	"strings"
	"time"
)

// APIReference represents API documentation.
type APIReference struct {
	ID          string
	Title       string
	Description string
	BaseURL     string
	Version     string
	Endpoints   []Endpoint
	Models      []Model
	Examples    []Example
	SDKs        []SDK
	Changelog   []ChangelogEntry
}

// Endpoint represents an API endpoint.
type Endpoint struct {
	Method      string
	Path        string
	Summary     string
	Description string
	Tags        []string
	Parameters  []Parameter
	RequestBody *RequestBody
	Responses   []Response
	Examples    []Example
	Deprecated  bool
}

// Parameter represents an API parameter.
type Parameter struct {
	Name        string
	In          string // path, query, header, cookie
	Type        string
	Required    bool
	Description string
	Default     string
	Example     string
}

// RequestBody represents a request body.
type RequestBody struct {
	ContentType string
	Schema      string
	Description string
	Example     string
	Required    bool
}

// Response represents an API response.
type Response struct {
	StatusCode  int
	Description string
	ContentType string
	Schema      string
	Example     string
}

// Model represents a data model.
type Model struct {
	Name        string
	Description string
	Fields      []Field
	Example     string
}

// Field represents a model field.
type Field struct {
	Name        string
	Type        string
	Required    bool
	Description string
	Default     string
	Constraints string
}

// Example represents a code example.
type Example struct {
	Title       string
	Description string
	Language    string
	Code        string
}

// SDK represents an SDK.
type SDK struct {
	Name       string
	Language   string
	Repository string
	InstallCmd string
	Version    string
	QuickStart string
}

// ChangelogEntry represents a changelog entry.
type ChangelogEntry struct {
	Version   string
	Date      time.Time
	Type      ChangeType
	Changes   []string
	Breaking  bool
	Migration string
}

// ChangeType indicates the type of change.
type ChangeType string

const (
	ChangeAdded      ChangeType = "added"
	ChangeChanged    ChangeType = "changed"
	ChangeDeprecated ChangeType = "deprecated"
	ChangeRemoved    ChangeType = "removed"
	ChangeFixed      ChangeType = "fixed"
	ChangeSecurity   ChangeType = "security"
)

// APIReferenceBuilder helps construct API references.
type APIReferenceBuilder struct {
	ref *APIReference
}

// NewAPIReference creates an API reference builder.
func NewAPIReference(id, title string) *APIReferenceBuilder {
	return &APIReferenceBuilder{
		ref: &APIReference{
			ID:        id,
			Title:     title,
			Endpoints: make([]Endpoint, 0),
			Models:    make([]Model, 0),
			Examples:  make([]Example, 0),
			SDKs:      make([]SDK, 0),
			Changelog: make([]ChangelogEntry, 0),
		},
	}
}

// Description sets the description.
func (b *APIReferenceBuilder) Description(desc string) *APIReferenceBuilder {
	b.ref.Description = desc
	return b
}

// BaseURL sets the base URL.
func (b *APIReferenceBuilder) BaseURL(url string) *APIReferenceBuilder {
	b.ref.BaseURL = url
	return b
}

// Version sets the version.
func (b *APIReferenceBuilder) Version(ver string) *APIReferenceBuilder {
	b.ref.Version = ver
	return b
}

// Endpoint adds an endpoint.
func (b *APIReferenceBuilder) Endpoint(e Endpoint) *APIReferenceBuilder {
	b.ref.Endpoints = append(b.ref.Endpoints, e)
	return b
}

// Model adds a model.
func (b *APIReferenceBuilder) Model(m Model) *APIReferenceBuilder {
	b.ref.Models = append(b.ref.Models, m)
	return b
}

// Example adds an example.
func (b *APIReferenceBuilder) Example(e Example) *APIReferenceBuilder {
	b.ref.Examples = append(b.ref.Examples, e)
	return b
}

// SDK adds an SDK.
func (b *APIReferenceBuilder) SDK(s SDK) *APIReferenceBuilder {
	b.ref.SDKs = append(b.ref.SDKs, s)
	return b
}

// ChangelogEntry adds a changelog entry.
func (b *APIReferenceBuilder) ChangelogEntry(e ChangelogEntry) *APIReferenceBuilder {
	b.ref.Changelog = append(b.ref.Changelog, e)
	return b
}

// Build returns the completed reference.
func (b *APIReferenceBuilder) Build() *APIReference {
	return b.ref
}

// EndpointBuilder helps construct endpoints.
type EndpointBuilder struct {
	endpoint *Endpoint
}

// NewEndpoint creates an endpoint builder.
func NewEndpoint(method, path string) *EndpointBuilder {
	return &EndpointBuilder{
		endpoint: &Endpoint{
			Method:     method,
			Path:       path,
			Parameters: make([]Parameter, 0),
			Responses:  make([]Response, 0),
			Examples:   make([]Example, 0),
			Tags:       make([]string, 0),
		},
	}
}

// Summary sets the summary.
func (b *EndpointBuilder) Summary(summary string) *EndpointBuilder {
	b.endpoint.Summary = summary
	return b
}

// Description sets the description.
func (b *EndpointBuilder) Description(desc string) *EndpointBuilder {
	b.endpoint.Description = desc
	return b
}

// Tags adds tags.
func (b *EndpointBuilder) Tags(tags ...string) *EndpointBuilder {
	b.endpoint.Tags = append(b.endpoint.Tags, tags...)
	return b
}

// PathParam adds a path parameter.
func (b *EndpointBuilder) PathParam(name, paramType, desc string) *EndpointBuilder {
	b.endpoint.Parameters = append(b.endpoint.Parameters, Parameter{
		Name:        name,
		In:          "path",
		Type:        paramType,
		Required:    true,
		Description: desc,
	})
	return b
}

// QueryParam adds a query parameter.
func (b *EndpointBuilder) QueryParam(name, paramType, desc string, required bool) *EndpointBuilder {
	b.endpoint.Parameters = append(b.endpoint.Parameters, Parameter{
		Name:        name,
		In:          "query",
		Type:        paramType,
		Required:    required,
		Description: desc,
	})
	return b
}

// HeaderParam adds a header parameter.
func (b *EndpointBuilder) HeaderParam(name, paramType, desc string, required bool) *EndpointBuilder {
	b.endpoint.Parameters = append(b.endpoint.Parameters, Parameter{
		Name:        name,
		In:          "header",
		Type:        paramType,
		Required:    required,
		Description: desc,
	})
	return b
}

// Body sets the request body.
func (b *EndpointBuilder) Body(contentType, schema, desc, example string) *EndpointBuilder {
	b.endpoint.RequestBody = &RequestBody{
		ContentType: contentType,
		Schema:      schema,
		Description: desc,
		Example:     example,
		Required:    true,
	}
	return b
}

// Response adds a response.
func (b *EndpointBuilder) Response(code int, desc, contentType, schema, example string) *EndpointBuilder {
	b.endpoint.Responses = append(b.endpoint.Responses, Response{
		StatusCode:  code,
		Description: desc,
		ContentType: contentType,
		Schema:      schema,
		Example:     example,
	})
	return b
}

// Example adds an example.
func (b *EndpointBuilder) Example(title, lang, code string) *EndpointBuilder {
	b.endpoint.Examples = append(b.endpoint.Examples, Example{
		Title:    title,
		Language: lang,
		Code:     code,
	})
	return b
}

// Deprecated marks the endpoint as deprecated.
func (b *EndpointBuilder) Deprecated() *EndpointBuilder {
	b.endpoint.Deprecated = true
	return b
}

// Build returns the completed endpoint.
func (b *EndpointBuilder) Build() Endpoint {
	return *b.endpoint
}

// ModelBuilder helps construct models.
type ModelBuilder struct {
	model *Model
}

// NewModel creates a model builder.
func NewModel(name string) *ModelBuilder {
	return &ModelBuilder{
		model: &Model{
			Name:   name,
			Fields: make([]Field, 0),
		},
	}
}

// Description sets the description.
func (b *ModelBuilder) Description(desc string) *ModelBuilder {
	b.model.Description = desc
	return b
}

// Field adds a field.
func (b *ModelBuilder) Field(name, fieldType, desc string, required bool) *ModelBuilder {
	b.model.Fields = append(b.model.Fields, Field{
		Name:        name,
		Type:        fieldType,
		Required:    required,
		Description: desc,
	})
	return b
}

// FieldWithConstraints adds a field with constraints.
func (b *ModelBuilder) FieldWithConstraints(name, fieldType, desc, constraints string, required bool) *ModelBuilder {
	b.model.Fields = append(b.model.Fields, Field{
		Name:        name,
		Type:        fieldType,
		Required:    required,
		Description: desc,
		Constraints: constraints,
	})
	return b
}

// Example sets the example.
func (b *ModelBuilder) Example(example string) *ModelBuilder {
	b.model.Example = example
	return b
}

// Build returns the completed model.
func (b *ModelBuilder) Build() Model {
	return *b.model
}

// DeveloperRenderer renders developer documentation.
type DeveloperRenderer struct{}

// NewDeveloperRenderer creates a developer renderer.
func NewDeveloperRenderer() *DeveloperRenderer {
	return &DeveloperRenderer{}
}

// RenderAPIReference renders an API reference to Markdown.
func (r *DeveloperRenderer) RenderAPIReference(w io.Writer, ref *APIReference) error {
	var sb strings.Builder

	// Header
	sb.WriteString("# " + ref.Title + "\n\n")
	if ref.Description != "" {
		sb.WriteString(ref.Description + "\n\n")
	}

	// Metadata
	sb.WriteString("---\n")
	if ref.BaseURL != "" {
		sb.WriteString("Base URL: `" + ref.BaseURL + "`\n")
	}
	sb.WriteString("Version: " + ref.Version + "\n")
	sb.WriteString("---\n\n")

	// Table of Contents
	if len(ref.Endpoints) > 0 {
		sb.WriteString("## Endpoints\n\n")
		for _, e := range ref.Endpoints {
			deprecated := ""
			if e.Deprecated {
				deprecated = " ⚠️ Deprecated"
			}
			sb.WriteString(fmt.Sprintf("- [`%s %s`](#%s) - %s%s\n",
				e.Method, e.Path, slugify(e.Method+"-"+e.Path), e.Summary, deprecated))
		}
		sb.WriteString("\n")
	}

	// Endpoints
	for _, e := range ref.Endpoints {
		r.renderEndpoint(&sb, e)
	}

	// Models
	if len(ref.Models) > 0 {
		sb.WriteString("## Models\n\n")
		for _, m := range ref.Models {
			r.renderModel(&sb, m)
		}
	}

	// SDKs
	if len(ref.SDKs) > 0 {
		sb.WriteString("## SDKs\n\n")
		for _, s := range ref.SDKs {
			sb.WriteString(fmt.Sprintf("### %s (%s)\n\n", s.Name, s.Language))
			sb.WriteString("**Install:**\n```bash\n" + s.InstallCmd + "\n```\n\n")
			if s.QuickStart != "" {
				sb.WriteString("**Quick Start:**\n```" + s.Language + "\n" + s.QuickStart + "\n```\n\n")
			}
		}
	}

	// Changelog
	if len(ref.Changelog) > 0 {
		sb.WriteString("## Changelog\n\n")
		for _, c := range ref.Changelog {
			breaking := ""
			if c.Breaking {
				breaking = " 🚨 BREAKING"
			}
			sb.WriteString(fmt.Sprintf("### %s (%s)%s\n\n", c.Version, c.Date.Format("2006-01-02"), breaking))
			sb.WriteString(fmt.Sprintf("**%s**\n\n", c.Type))
			for _, change := range c.Changes {
				sb.WriteString("- " + change + "\n")
			}
			if c.Migration != "" {
				sb.WriteString("\n**Migration:**\n" + c.Migration + "\n")
			}
			sb.WriteString("\n")
		}
	}

	_, err := w.Write([]byte(sb.String()))
	return err
}

// renderEndpoint renders an endpoint to Markdown.
func (r *DeveloperRenderer) renderEndpoint(sb *strings.Builder, e Endpoint) {
	deprecated := ""
	if e.Deprecated {
		deprecated = " ⚠️ DEPRECATED"
	}

	fmt.Fprintf(sb, "### %s %s%s\n\n", e.Method, e.Path, deprecated)

	if e.Summary != "" {
		sb.WriteString("**" + e.Summary + "**\n\n")
	}
	if e.Description != "" {
		sb.WriteString(e.Description + "\n\n")
	}

	// Parameters
	if len(e.Parameters) > 0 {
		sb.WriteString("#### Parameters\n\n")
		sb.WriteString("| Name | In | Type | Required | Description |\n")
		sb.WriteString("|------|----|------|----------|-------------|\n")
		for _, p := range e.Parameters {
			required := ""
			if p.Required {
				required = "✓"
			}
			fmt.Fprintf(sb, "| %s | %s | %s | %s | %s |\n",
				p.Name, p.In, p.Type, required, p.Description)
		}
		sb.WriteString("\n")
	}

	// Request Body
	if e.RequestBody != nil {
		sb.WriteString("#### Request Body\n\n")
		fmt.Fprintf(sb, "Content-Type: `%s`\n\n", e.RequestBody.ContentType)
		if e.RequestBody.Description != "" {
			sb.WriteString(e.RequestBody.Description + "\n\n")
		}
		if e.RequestBody.Example != "" {
			sb.WriteString("```json\n" + e.RequestBody.Example + "\n```\n\n")
		}
	}

	// Responses
	if len(e.Responses) > 0 {
		sb.WriteString("#### Responses\n\n")
		for _, resp := range e.Responses {
			fmt.Fprintf(sb, "**%d** - %s\n\n", resp.StatusCode, resp.Description)
			if resp.Example != "" {
				sb.WriteString("```json\n" + resp.Example + "\n```\n\n")
			}
		}
	}

	// Examples
	if len(e.Examples) > 0 {
		sb.WriteString("#### Examples\n\n")
		for _, ex := range e.Examples {
			fmt.Fprintf(sb, "**%s**\n\n", ex.Title)
			sb.WriteString("```" + ex.Language + "\n" + ex.Code + "\n```\n\n")
		}
	}
}

// renderModel renders a model to Markdown.
func (r *DeveloperRenderer) renderModel(sb *strings.Builder, m Model) {
	sb.WriteString("### " + m.Name + "\n\n")
	if m.Description != "" {
		sb.WriteString(m.Description + "\n\n")
	}

	if len(m.Fields) > 0 {
		sb.WriteString("| Field | Type | Required | Description |\n")
		sb.WriteString("|-------|------|----------|-------------|\n")
		for _, f := range m.Fields {
			required := ""
			if f.Required {
				required = "✓"
			}
			desc := f.Description
			if f.Constraints != "" {
				desc += " (" + f.Constraints + ")"
			}
			fmt.Fprintf(sb, "| %s | %s | %s | %s |\n",
				f.Name, f.Type, required, desc)
		}
		sb.WriteString("\n")
	}

	if m.Example != "" {
		sb.WriteString("**Example:**\n```json\n" + m.Example + "\n```\n\n")
	}
}

// slugify creates a URL-safe slug.
func slugify(s string) string {
	s = strings.ToLower(s)
	s = strings.ReplaceAll(s, " ", "-")
	s = strings.ReplaceAll(s, "/", "-")
	s = strings.ReplaceAll(s, "{", "")
	s = strings.ReplaceAll(s, "}", "")
	return s
}

// QuickStart represents a quick start guide.
type QuickStart struct {
	Title       string
	Description string
	Steps       []QuickStartStep
	NextSteps   []string
}

// QuickStartStep represents a quick start step.
type QuickStartStep struct {
	Title       string
	Description string
	Code        string
	Language    string
	Output      string
}

// QuickStartBuilder helps construct quick starts.
type QuickStartBuilder struct {
	qs *QuickStart
}

// NewQuickStart creates a quick start builder.
func NewQuickStart(title string) *QuickStartBuilder {
	return &QuickStartBuilder{
		qs: &QuickStart{
			Title:     title,
			Steps:     make([]QuickStartStep, 0),
			NextSteps: make([]string, 0),
		},
	}
}

// Description sets the description.
func (b *QuickStartBuilder) Description(desc string) *QuickStartBuilder {
	b.qs.Description = desc
	return b
}

// Step adds a step.
func (b *QuickStartBuilder) Step(title, desc, code, lang string) *QuickStartBuilder {
	b.qs.Steps = append(b.qs.Steps, QuickStartStep{
		Title:       title,
		Description: desc,
		Code:        code,
		Language:    lang,
	})
	return b
}

// StepWithOutput adds a step with expected output.
func (b *QuickStartBuilder) StepWithOutput(title, desc, code, lang, output string) *QuickStartBuilder {
	b.qs.Steps = append(b.qs.Steps, QuickStartStep{
		Title:       title,
		Description: desc,
		Code:        code,
		Language:    lang,
		Output:      output,
	})
	return b
}

// NextStep adds a next step suggestion.
func (b *QuickStartBuilder) NextStep(step string) *QuickStartBuilder {
	b.qs.NextSteps = append(b.qs.NextSteps, step)
	return b
}

// Build returns the completed quick start.
func (b *QuickStartBuilder) Build() *QuickStart {
	return b.qs
}

// RenderQuickStart renders a quick start to Markdown.
func (r *DeveloperRenderer) RenderQuickStart(w io.Writer, qs *QuickStart) error {
	var sb strings.Builder

	sb.WriteString("# " + qs.Title + "\n\n")
	if qs.Description != "" {
		sb.WriteString(qs.Description + "\n\n")
	}

	for i, step := range qs.Steps {
		sb.WriteString(fmt.Sprintf("## Step %d: %s\n\n", i+1, step.Title))
		if step.Description != "" {
			sb.WriteString(step.Description + "\n\n")
		}
		sb.WriteString("```" + step.Language + "\n" + step.Code + "\n```\n\n")
		if step.Output != "" {
			sb.WriteString("**Output:**\n```\n" + step.Output + "\n```\n\n")
		}
	}

	if len(qs.NextSteps) > 0 {
		sb.WriteString("## Next Steps\n\n")
		for _, ns := range qs.NextSteps {
			sb.WriteString("- " + ns + "\n")
		}
	}

	_, err := w.Write([]byte(sb.String()))
	return err
}
