// Package guides provides utilities for user documentation.
package guides

import (
	"fmt"
	"html/template"
	"io"
	"strings"
	"time"
)

// Guide represents a user guide document.
type Guide struct {
	ID          string
	Title       string
	Description string
	Category    Category
	Sections    []Section
	Tags        []string
	Author      string
	CreatedAt   time.Time
	UpdatedAt   time.Time
	Version     string
}

// Category represents a guide category.
type Category string

const (
	CategoryGettingStarted  Category = "getting-started"
	CategoryAuthentication  Category = "authentication"
	CategoryStorage         Category = "storage"
	CategoryAPI             Category = "api"
	CategorySDK             Category = "sdk"
	CategoryBilling         Category = "billing"
	CategorySecurity        Category = "security"
	CategoryTroubleshooting Category = "troubleshooting"
)

// Section represents a guide section.
type Section struct {
	ID          string
	Title       string
	Content     string
	Subsections []Subsection
	CodeBlocks  []CodeBlock
	Notes       []Note
	Order       int
}

// Subsection represents a subsection.
type Subsection struct {
	ID      string
	Title   string
	Content string
}

// CodeBlock represents a code example.
type CodeBlock struct {
	Language    string
	Code        string
	Description string
	Copyable    bool
}

// Note represents a callout note.
type Note struct {
	Type    NoteType
	Title   string
	Content string
}

// NoteType indicates the type of note.
type NoteType string

const (
	NoteTypeInfo    NoteType = "info"
	NoteTypeWarning NoteType = "warning"
	NoteTypeTip     NoteType = "tip"
	NoteTypeDanger  NoteType = "danger"
)

// GuideBuilder helps construct guides.
type GuideBuilder struct {
	guide *Guide
}

// NewGuide creates a guide builder.
func NewGuide(id, title string) *GuideBuilder {
	return &GuideBuilder{
		guide: &Guide{
			ID:        id,
			Title:     title,
			Sections:  make([]Section, 0),
			Tags:      make([]string, 0),
			CreatedAt: time.Now(),
			UpdatedAt: time.Now(),
			Version:   "1.0",
		},
	}
}

// Description sets the guide description.
func (gb *GuideBuilder) Description(desc string) *GuideBuilder {
	gb.guide.Description = desc
	return gb
}

// Category sets the guide category.
func (gb *GuideBuilder) Category(cat Category) *GuideBuilder {
	gb.guide.Category = cat
	return gb
}

// Author sets the guide author.
func (gb *GuideBuilder) Author(author string) *GuideBuilder {
	gb.guide.Author = author
	return gb
}

// Version sets the guide version.
func (gb *GuideBuilder) Version(version string) *GuideBuilder {
	gb.guide.Version = version
	return gb
}

// Tags adds tags.
func (gb *GuideBuilder) Tags(tags ...string) *GuideBuilder {
	gb.guide.Tags = append(gb.guide.Tags, tags...)
	return gb
}

// Section adds a section.
func (gb *GuideBuilder) Section(id, title, content string) *GuideBuilder {
	section := Section{
		ID:      id,
		Title:   title,
		Content: content,
		Order:   len(gb.guide.Sections),
	}
	gb.guide.Sections = append(gb.guide.Sections, section)
	return gb
}

// Build returns the completed guide.
func (gb *GuideBuilder) Build() *Guide {
	return gb.guide
}

// SectionBuilder helps build sections.
type SectionBuilder struct {
	section *Section
}

// NewSection creates a section builder.
func NewSection(id, title string) *SectionBuilder {
	return &SectionBuilder{
		section: &Section{
			ID:          id,
			Title:       title,
			Subsections: make([]Subsection, 0),
			CodeBlocks:  make([]CodeBlock, 0),
			Notes:       make([]Note, 0),
		},
	}
}

// Content sets the section content.
func (sb *SectionBuilder) Content(content string) *SectionBuilder {
	sb.section.Content = content
	return sb
}

// Subsection adds a subsection.
func (sb *SectionBuilder) Subsection(id, title, content string) *SectionBuilder {
	sb.section.Subsections = append(sb.section.Subsections, Subsection{
		ID:      id,
		Title:   title,
		Content: content,
	})
	return sb
}

// Code adds a code block.
func (sb *SectionBuilder) Code(language, code, description string) *SectionBuilder {
	sb.section.CodeBlocks = append(sb.section.CodeBlocks, CodeBlock{
		Language:    language,
		Code:        code,
		Description: description,
		Copyable:    true,
	})
	return sb
}

// Info adds an info note.
func (sb *SectionBuilder) Info(title, content string) *SectionBuilder {
	sb.section.Notes = append(sb.section.Notes, Note{
		Type:    NoteTypeInfo,
		Title:   title,
		Content: content,
	})
	return sb
}

// Warning adds a warning note.
func (sb *SectionBuilder) Warning(title, content string) *SectionBuilder {
	sb.section.Notes = append(sb.section.Notes, Note{
		Type:    NoteTypeWarning,
		Title:   title,
		Content: content,
	})
	return sb
}

// Tip adds a tip note.
func (sb *SectionBuilder) Tip(title, content string) *SectionBuilder {
	sb.section.Notes = append(sb.section.Notes, Note{
		Type:    NoteTypeTip,
		Title:   title,
		Content: content,
	})
	return sb
}

// Danger adds a danger note.
func (sb *SectionBuilder) Danger(title, content string) *SectionBuilder {
	sb.section.Notes = append(sb.section.Notes, Note{
		Type:    NoteTypeDanger,
		Title:   title,
		Content: content,
	})
	return sb
}

// Build returns the completed section.
func (sb *SectionBuilder) Build() Section {
	return *sb.section
}

// Library manages a collection of guides.
type Library struct {
	guides     map[string]*Guide
	categories map[Category][]*Guide
}

// NewLibrary creates a guide library.
func NewLibrary() *Library {
	return &Library{
		guides:     make(map[string]*Guide),
		categories: make(map[Category][]*Guide),
	}
}

// Add adds a guide to the library.
func (l *Library) Add(guide *Guide) {
	l.guides[guide.ID] = guide
	l.categories[guide.Category] = append(l.categories[guide.Category], guide)
}

// Get retrieves a guide by ID.
func (l *Library) Get(id string) (*Guide, bool) {
	guide, ok := l.guides[id]
	return guide, ok
}

// GetByCategory retrieves guides by category.
func (l *Library) GetByCategory(cat Category) []*Guide {
	return l.categories[cat]
}

// Search searches guides by query.
func (l *Library) Search(query string) []*Guide {
	query = strings.ToLower(query)
	results := make([]*Guide, 0)

	for _, guide := range l.guides {
		if strings.Contains(strings.ToLower(guide.Title), query) ||
			strings.Contains(strings.ToLower(guide.Description), query) {
			results = append(results, guide)
			continue
		}

		// Search in tags
		for _, tag := range guide.Tags {
			if strings.Contains(strings.ToLower(tag), query) {
				results = append(results, guide)
				break
			}
		}
	}

	return results
}

// All returns all guides.
func (l *Library) All() []*Guide {
	guides := make([]*Guide, 0, len(l.guides))
	for _, g := range l.guides {
		guides = append(guides, g)
	}
	return guides
}

// Categories returns all categories with guides.
func (l *Library) Categories() []Category {
	cats := make([]Category, 0, len(l.categories))
	for cat := range l.categories {
		cats = append(cats, cat)
	}
	return cats
}

// Renderer renders guides to various formats.
type Renderer struct {
	templates map[string]*template.Template
}

// NewRenderer creates a guide renderer.
func NewRenderer() *Renderer {
	return &Renderer{
		templates: make(map[string]*template.Template),
	}
}

// RenderMarkdown renders a guide to Markdown.
func (r *Renderer) RenderMarkdown(w io.Writer, guide *Guide) error {
	var sb strings.Builder

	// Title
	sb.WriteString("# " + guide.Title + "\n\n")

	// Description
	if guide.Description != "" {
		sb.WriteString(guide.Description + "\n\n")
	}

	// Metadata
	sb.WriteString("---\n")
	sb.WriteString(fmt.Sprintf("Category: %s\n", guide.Category))
	if guide.Author != "" {
		sb.WriteString(fmt.Sprintf("Author: %s\n", guide.Author))
	}
	sb.WriteString(fmt.Sprintf("Version: %s\n", guide.Version))
	sb.WriteString(fmt.Sprintf("Updated: %s\n", guide.UpdatedAt.Format("2006-01-02")))
	if len(guide.Tags) > 0 {
		sb.WriteString(fmt.Sprintf("Tags: %s\n", strings.Join(guide.Tags, ", ")))
	}
	sb.WriteString("---\n\n")

	// Table of contents
	if len(guide.Sections) > 1 {
		sb.WriteString("## Table of Contents\n\n")
		for _, section := range guide.Sections {
			sb.WriteString(fmt.Sprintf("- [%s](#%s)\n", section.Title, section.ID))
		}
		sb.WriteString("\n")
	}

	// Sections
	for _, section := range guide.Sections {
		r.renderSectionMarkdown(&sb, section, 2)
	}

	_, err := w.Write([]byte(sb.String()))
	return err
}

// renderSectionMarkdown renders a section to Markdown.
func (r *Renderer) renderSectionMarkdown(sb *strings.Builder, section Section, level int) {
	// Section title
	sb.WriteString(strings.Repeat("#", level) + " " + section.Title + "\n\n")

	// Content
	if section.Content != "" {
		sb.WriteString(section.Content + "\n\n")
	}

	// Notes
	for _, note := range section.Notes {
		r.renderNoteMarkdown(sb, note)
	}

	// Code blocks
	for _, code := range section.CodeBlocks {
		if code.Description != "" {
			sb.WriteString(code.Description + "\n\n")
		}
		sb.WriteString("```" + code.Language + "\n")
		sb.WriteString(code.Code + "\n")
		sb.WriteString("```\n\n")
	}

	// Subsections
	for _, sub := range section.Subsections {
		sb.WriteString(strings.Repeat("#", level+1) + " " + sub.Title + "\n\n")
		sb.WriteString(sub.Content + "\n\n")
	}
}

// renderNoteMarkdown renders a note to Markdown.
func (r *Renderer) renderNoteMarkdown(sb *strings.Builder, note Note) {
	prefix := ""
	switch note.Type {
	case NoteTypeInfo:
		prefix = "ℹ️ "
	case NoteTypeWarning:
		prefix = "⚠️ "
	case NoteTypeTip:
		prefix = "💡 "
	case NoteTypeDanger:
		prefix = "🚨 "
	}

	sb.WriteString("> " + prefix + "**" + note.Title + "**\n")
	sb.WriteString("> \n")
	for _, line := range strings.Split(note.Content, "\n") {
		sb.WriteString("> " + line + "\n")
	}
	sb.WriteString("\n")
}

// RenderHTML renders a guide to HTML.
func (r *Renderer) RenderHTML(w io.Writer, guide *Guide) error {
	tmpl := `<!DOCTYPE html>
<html lang="en">
<head>
    <meta charset="UTF-8">
    <meta name="viewport" content="width=device-width, initial-scale=1.0">
    <title>{{.Title}}</title>
    <style>
        body { font-family: -apple-system, BlinkMacSystemFont, 'Segoe UI', Roboto, sans-serif; line-height: 1.6; max-width: 800px; margin: 0 auto; padding: 20px; }
        h1 { border-bottom: 2px solid #333; padding-bottom: 10px; }
        h2 { border-bottom: 1px solid #ddd; padding-bottom: 5px; margin-top: 30px; }
        pre { background: #f5f5f5; padding: 15px; border-radius: 5px; overflow-x: auto; }
        code { background: #f5f5f5; padding: 2px 5px; border-radius: 3px; }
        .note { padding: 15px; border-radius: 5px; margin: 15px 0; }
        .note-info { background: #e7f3fe; border-left: 4px solid #2196F3; }
        .note-warning { background: #fff3e0; border-left: 4px solid #ff9800; }
        .note-tip { background: #e8f5e9; border-left: 4px solid #4caf50; }
        .note-danger { background: #ffebee; border-left: 4px solid #f44336; }
        .metadata { background: #f9f9f9; padding: 10px; border-radius: 5px; font-size: 0.9em; }
        .toc { background: #f5f5f5; padding: 15px; border-radius: 5px; }
        .toc ul { margin: 0; padding-left: 20px; }
    </style>
</head>
<body>
    <h1>{{.Title}}</h1>
    {{if .Description}}<p class="description">{{.Description}}</p>{{end}}

    <div class="metadata">
        <strong>Category:</strong> {{.Category}} |
        <strong>Version:</strong> {{.Version}} |
        <strong>Updated:</strong> {{.UpdatedAt.Format "2006-01-02"}}
        {{if .Author}} | <strong>Author:</strong> {{.Author}}{{end}}
    </div>

    {{if gt (len .Sections) 1}}
    <div class="toc">
        <h3>Table of Contents</h3>
        <ul>
        {{range .Sections}}
            <li><a href="#{{.ID}}">{{.Title}}</a></li>
        {{end}}
        </ul>
    </div>
    {{end}}

    {{range .Sections}}
    <section id="{{.ID}}">
        <h2>{{.Title}}</h2>
        {{if .Content}}<p>{{.Content}}</p>{{end}}

        {{range .Notes}}
        <div class="note note-{{.Type}}">
            <strong>{{.Title}}</strong>
            <p>{{.Content}}</p>
        </div>
        {{end}}

        {{range .CodeBlocks}}
        {{if .Description}}<p>{{.Description}}</p>{{end}}
        <pre><code class="language-{{.Language}}">{{.Code}}</code></pre>
        {{end}}

        {{range .Subsections}}
        <h3 id="{{.ID}}">{{.Title}}</h3>
        <p>{{.Content}}</p>
        {{end}}
    </section>
    {{end}}
</body>
</html>`

	t, err := template.New("guide").Parse(tmpl)
	if err != nil {
		return fmt.Errorf("parsing template: %w", err)
	}

	return t.Execute(w, guide)
}

// TableOfContents generates a table of contents.
func (g *Guide) TableOfContents() []TOCEntry {
	entries := make([]TOCEntry, 0)

	for _, section := range g.Sections {
		entry := TOCEntry{
			ID:    section.ID,
			Title: section.Title,
			Level: 1,
		}

		for _, sub := range section.Subsections {
			entry.Children = append(entry.Children, TOCEntry{
				ID:    sub.ID,
				Title: sub.Title,
				Level: 2,
			})
		}

		entries = append(entries, entry)
	}

	return entries
}

// TOCEntry represents a table of contents entry.
type TOCEntry struct {
	ID       string
	Title    string
	Level    int
	Children []TOCEntry
}
