// Package tutorials provides utilities for video tutorial documentation.
package tutorials

import (
	"fmt"
	"io"
	"strings"
	"time"
)

// Tutorial represents a video tutorial.
type Tutorial struct {
	ID            string
	Title         string
	Description   string
	Category      Category
	Level         Level
	Duration      time.Duration
	VideoURL      string
	Thumbnail     string
	Chapters      []Chapter
	Transcript    string
	Resources     []Resource
	Prerequisites []string
	Tags          []string
	Author        string
	PublishedAt   time.Time
	UpdatedAt     time.Time
}

// Category categorizes tutorials.
type Category string

const (
	CategoryGettingStarted  Category = "getting-started"
	CategoryConfiguration   Category = "configuration"
	CategoryIntegration     Category = "integration"
	CategoryAdvanced        Category = "advanced"
	CategoryTroubleshooting Category = "troubleshooting"
	CategoryBestPractices   Category = "best-practices"
)

// Level indicates tutorial difficulty.
type Level string

const (
	LevelBeginner     Level = "beginner"
	LevelIntermediate Level = "intermediate"
	LevelAdvanced     Level = "advanced"
)

// Chapter represents a video chapter.
type Chapter struct {
	Title     string
	StartTime time.Duration
	EndTime   time.Duration
	Summary   string
}

// Resource represents an additional resource.
type Resource struct {
	Title       string
	URL         string
	Type        ResourceType
	Description string
}

// ResourceType categorizes resources.
type ResourceType string

const (
	ResourceCode     ResourceType = "code"
	ResourceDocs     ResourceType = "documentation"
	ResourceDownload ResourceType = "download"
	ResourceExternal ResourceType = "external"
)

// TutorialBuilder helps construct tutorials.
type TutorialBuilder struct {
	tutorial *Tutorial
}

// NewTutorial creates a tutorial builder.
func NewTutorial(id, title string) *TutorialBuilder {
	return &TutorialBuilder{
		tutorial: &Tutorial{
			ID:            id,
			Title:         title,
			Chapters:      make([]Chapter, 0),
			Resources:     make([]Resource, 0),
			Prerequisites: make([]string, 0),
			Tags:          make([]string, 0),
			PublishedAt:   time.Now(),
			UpdatedAt:     time.Now(),
		},
	}
}

// Description sets the description.
func (b *TutorialBuilder) Description(desc string) *TutorialBuilder {
	b.tutorial.Description = desc
	return b
}

// Category sets the category.
func (b *TutorialBuilder) Category(cat Category) *TutorialBuilder {
	b.tutorial.Category = cat
	return b
}

// Level sets the level.
func (b *TutorialBuilder) Level(l Level) *TutorialBuilder {
	b.tutorial.Level = l
	return b
}

// Duration sets the duration.
func (b *TutorialBuilder) Duration(d time.Duration) *TutorialBuilder {
	b.tutorial.Duration = d
	return b
}

// VideoURL sets the video URL.
func (b *TutorialBuilder) VideoURL(url string) *TutorialBuilder {
	b.tutorial.VideoURL = url
	return b
}

// Thumbnail sets the thumbnail URL.
func (b *TutorialBuilder) Thumbnail(url string) *TutorialBuilder {
	b.tutorial.Thumbnail = url
	return b
}

// Chapter adds a chapter.
func (b *TutorialBuilder) Chapter(title string, start, end time.Duration, summary string) *TutorialBuilder {
	b.tutorial.Chapters = append(b.tutorial.Chapters, Chapter{
		Title:     title,
		StartTime: start,
		EndTime:   end,
		Summary:   summary,
	})
	return b
}

// Transcript sets the transcript.
func (b *TutorialBuilder) Transcript(t string) *TutorialBuilder {
	b.tutorial.Transcript = t
	return b
}

// Resource adds a resource.
func (b *TutorialBuilder) Resource(title, url string, resType ResourceType, desc string) *TutorialBuilder {
	b.tutorial.Resources = append(b.tutorial.Resources, Resource{
		Title:       title,
		URL:         url,
		Type:        resType,
		Description: desc,
	})
	return b
}

// Prerequisite adds a prerequisite.
func (b *TutorialBuilder) Prerequisite(prereq string) *TutorialBuilder {
	b.tutorial.Prerequisites = append(b.tutorial.Prerequisites, prereq)
	return b
}

// Tag adds a tag.
func (b *TutorialBuilder) Tag(tag string) *TutorialBuilder {
	b.tutorial.Tags = append(b.tutorial.Tags, tag)
	return b
}

// Author sets the author.
func (b *TutorialBuilder) Author(author string) *TutorialBuilder {
	b.tutorial.Author = author
	return b
}

// PublishedAt sets the publish date.
func (b *TutorialBuilder) PublishedAt(t time.Time) *TutorialBuilder {
	b.tutorial.PublishedAt = t
	return b
}

// Build returns the completed tutorial.
func (b *TutorialBuilder) Build() *Tutorial {
	return b.tutorial
}

// Series represents a tutorial series.
type Series struct {
	ID            string
	Title         string
	Description   string
	Tutorials     []string // Tutorial IDs in order
	TotalDuration time.Duration
	Level         Level
}

// SeriesBuilder helps construct series.
type SeriesBuilder struct {
	series *Series
}

// NewSeries creates a series builder.
func NewSeries(id, title string) *SeriesBuilder {
	return &SeriesBuilder{
		series: &Series{
			ID:        id,
			Title:     title,
			Tutorials: make([]string, 0),
		},
	}
}

// Description sets the description.
func (b *SeriesBuilder) Description(desc string) *SeriesBuilder {
	b.series.Description = desc
	return b
}

// Level sets the level.
func (b *SeriesBuilder) Level(l Level) *SeriesBuilder {
	b.series.Level = l
	return b
}

// Tutorial adds a tutorial to the series.
func (b *SeriesBuilder) Tutorial(id string) *SeriesBuilder {
	b.series.Tutorials = append(b.series.Tutorials, id)
	return b
}

// TotalDuration sets the total duration.
func (b *SeriesBuilder) TotalDuration(d time.Duration) *SeriesBuilder {
	b.series.TotalDuration = d
	return b
}

// Build returns the completed series.
func (b *SeriesBuilder) Build() *Series {
	return b.series
}

// TutorialLibrary manages tutorials.
type TutorialLibrary struct {
	tutorials map[string]*Tutorial
	series    map[string]*Series
}

// NewTutorialLibrary creates a tutorial library.
func NewTutorialLibrary() *TutorialLibrary {
	return &TutorialLibrary{
		tutorials: make(map[string]*Tutorial),
		series:    make(map[string]*Series),
	}
}

// AddTutorial adds a tutorial.
func (l *TutorialLibrary) AddTutorial(t *Tutorial) {
	l.tutorials[t.ID] = t
}

// GetTutorial retrieves a tutorial.
func (l *TutorialLibrary) GetTutorial(id string) (*Tutorial, bool) {
	t, ok := l.tutorials[id]
	return t, ok
}

// AddSeries adds a series.
func (l *TutorialLibrary) AddSeries(s *Series) {
	l.series[s.ID] = s
}

// GetSeries retrieves a series.
func (l *TutorialLibrary) GetSeries(id string) (*Series, bool) {
	s, ok := l.series[id]
	return s, ok
}

// ListTutorials returns all tutorials.
func (l *TutorialLibrary) ListTutorials() []*Tutorial {
	result := make([]*Tutorial, 0, len(l.tutorials))
	for _, t := range l.tutorials {
		result = append(result, t)
	}
	return result
}

// ListSeries returns all series.
func (l *TutorialLibrary) ListSeries() []*Series {
	result := make([]*Series, 0, len(l.series))
	for _, s := range l.series {
		result = append(result, s)
	}
	return result
}

// ByCategory returns tutorials by category.
func (l *TutorialLibrary) ByCategory(cat Category) []*Tutorial {
	result := make([]*Tutorial, 0)
	for _, t := range l.tutorials {
		if t.Category == cat {
			result = append(result, t)
		}
	}
	return result
}

// ByLevel returns tutorials by level.
func (l *TutorialLibrary) ByLevel(level Level) []*Tutorial {
	result := make([]*Tutorial, 0)
	for _, t := range l.tutorials {
		if t.Level == level {
			result = append(result, t)
		}
	}
	return result
}

// ByTag returns tutorials by tag.
func (l *TutorialLibrary) ByTag(tag string) []*Tutorial {
	result := make([]*Tutorial, 0)
	for _, t := range l.tutorials {
		for _, tt := range t.Tags {
			if tt == tag {
				result = append(result, t)
				break
			}
		}
	}
	return result
}

// Search searches tutorials by title or description.
func (l *TutorialLibrary) Search(query string) []*Tutorial {
	query = strings.ToLower(query)
	result := make([]*Tutorial, 0)
	for _, t := range l.tutorials {
		if strings.Contains(strings.ToLower(t.Title), query) ||
			strings.Contains(strings.ToLower(t.Description), query) {
			result = append(result, t)
		}
	}
	return result
}

// TutorialRenderer renders tutorial documentation.
type TutorialRenderer struct{}

// NewTutorialRenderer creates a renderer.
func NewTutorialRenderer() *TutorialRenderer {
	return &TutorialRenderer{}
}

// RenderTutorial renders a tutorial to Markdown.
func (r *TutorialRenderer) RenderTutorial(w io.Writer, t *Tutorial) error {
	var sb strings.Builder

	// Header
	level := levelBadge(t.Level)
	fmt.Fprintf(&sb, "# %s %s\n\n", t.Title, level)

	if t.Description != "" {
		sb.WriteString(t.Description + "\n\n")
	}

	// Metadata
	sb.WriteString("---\n")
	fmt.Fprintf(&sb, "Duration: %s\n", formatDuration(t.Duration))
	fmt.Fprintf(&sb, "Category: %s\n", t.Category)
	if t.Author != "" {
		fmt.Fprintf(&sb, "Author: %s\n", t.Author)
	}
	fmt.Fprintf(&sb, "Published: %s\n", t.PublishedAt.Format("2006-01-02"))
	sb.WriteString("---\n\n")

	// Video embed
	if t.VideoURL != "" {
		sb.WriteString("## 🎬 Watch\n\n")
		fmt.Fprintf(&sb, "[Watch Video](%s)\n\n", t.VideoURL)
	}

	// Prerequisites
	if len(t.Prerequisites) > 0 {
		sb.WriteString("## Prerequisites\n\n")
		for _, p := range t.Prerequisites {
			sb.WriteString("- " + p + "\n")
		}
		sb.WriteString("\n")
	}

	// Chapters
	if len(t.Chapters) > 0 {
		sb.WriteString("## Chapters\n\n")
		sb.WriteString("| Time | Chapter | Summary |\n")
		sb.WriteString("|------|---------|----------|\n")
		for _, c := range t.Chapters {
			fmt.Fprintf(&sb, "| %s | %s | %s |\n",
				formatDuration(c.StartTime), c.Title, c.Summary)
		}
		sb.WriteString("\n")
	}

	// Resources
	if len(t.Resources) > 0 {
		sb.WriteString("## Resources\n\n")
		for _, res := range t.Resources {
			icon := resourceIcon(res.Type)
			fmt.Fprintf(&sb, "- %s [%s](%s)", icon, res.Title, res.URL)
			if res.Description != "" {
				sb.WriteString(" - " + res.Description)
			}
			sb.WriteString("\n")
		}
		sb.WriteString("\n")
	}

	// Transcript
	if t.Transcript != "" {
		sb.WriteString("## Transcript\n\n")
		sb.WriteString("<details>\n<summary>Show transcript</summary>\n\n")
		sb.WriteString(t.Transcript + "\n")
		sb.WriteString("</details>\n\n")
	}

	// Tags
	if len(t.Tags) > 0 {
		sb.WriteString("**Tags:** ")
		for i, tag := range t.Tags {
			if i > 0 {
				sb.WriteString(", ")
			}
			sb.WriteString("`" + tag + "`")
		}
		sb.WriteString("\n")
	}

	_, err := w.Write([]byte(sb.String()))
	return err
}

// RenderSeries renders a series to Markdown.
func (r *TutorialRenderer) RenderSeries(w io.Writer, s *Series, lib *TutorialLibrary) error {
	var sb strings.Builder

	level := levelBadge(s.Level)
	fmt.Fprintf(&sb, "# %s %s\n\n", s.Title, level)

	if s.Description != "" {
		sb.WriteString(s.Description + "\n\n")
	}

	fmt.Fprintf(&sb, "**Total Duration:** %s | **Videos:** %d\n\n",
		formatDuration(s.TotalDuration), len(s.Tutorials))

	sb.WriteString("## Videos in this Series\n\n")
	for i, id := range s.Tutorials {
		t, ok := lib.GetTutorial(id)
		if !ok {
			fmt.Fprintf(&sb, "%d. %s (not found)\n", i+1, id)
			continue
		}
		fmt.Fprintf(&sb, "%d. [%s](#%s) - %s\n", i+1, t.Title, t.ID, formatDuration(t.Duration))
	}

	_, err := w.Write([]byte(sb.String()))
	return err
}

// RenderLibraryIndex renders a library index to Markdown.
func (r *TutorialRenderer) RenderLibraryIndex(w io.Writer, lib *TutorialLibrary) error {
	var sb strings.Builder

	sb.WriteString("# Video Tutorials\n\n")

	// Series
	series := lib.ListSeries()
	if len(series) > 0 {
		sb.WriteString("## Series\n\n")
		for _, s := range series {
			level := levelBadge(s.Level)
			fmt.Fprintf(&sb, "- [%s](#%s) %s - %d videos, %s\n",
				s.Title, s.ID, level, len(s.Tutorials), formatDuration(s.TotalDuration))
		}
		sb.WriteString("\n")
	}

	// Group tutorials by category
	categories := make(map[Category][]*Tutorial)
	for _, t := range lib.ListTutorials() {
		categories[t.Category] = append(categories[t.Category], t)
	}

	for cat, tutorials := range categories {
		fmt.Fprintf(&sb, "## %s\n\n", cat)
		sb.WriteString("| Tutorial | Level | Duration |\n")
		sb.WriteString("|----------|-------|----------|\n")
		for _, t := range tutorials {
			fmt.Fprintf(&sb, "| [%s](#%s) | %s | %s |\n",
				t.Title, t.ID, t.Level, formatDuration(t.Duration))
		}
		sb.WriteString("\n")
	}

	_, err := w.Write([]byte(sb.String()))
	return err
}

// levelBadge returns a badge for level.
func levelBadge(l Level) string {
	switch l {
	case LevelBeginner:
		return "🟢 Beginner"
	case LevelIntermediate:
		return "🟡 Intermediate"
	case LevelAdvanced:
		return "🔴 Advanced"
	default:
		return ""
	}
}

// resourceIcon returns an icon for resource type.
func resourceIcon(t ResourceType) string {
	switch t {
	case ResourceCode:
		return "💻"
	case ResourceDocs:
		return "📚"
	case ResourceDownload:
		return "⬇️"
	case ResourceExternal:
		return "🔗"
	default:
		return "📎"
	}
}

// formatDuration formats duration for display.
func formatDuration(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm", int(d.Minutes()))
	}
	hours := int(d.Hours())
	mins := int(d.Minutes()) % 60
	if mins == 0 {
		return fmt.Sprintf("%dh", hours)
	}
	return fmt.Sprintf("%dh %dm", hours, mins)
}
