package guides

import (
	"bytes"
	"strings"
	"testing"
)

func TestNewGuide(t *testing.T) {
	guide := NewGuide("quickstart", "Quick Start Guide").
		Description("Get started with Vaultaire").
		Category(CategoryGettingStarted).
		Author("Vaultaire Team").
		Version("1.0").
		Tags("beginner", "tutorial").
		Build()

	if guide.ID != "quickstart" {
		t.Errorf("expected ID 'quickstart', got %s", guide.ID)
	}
	if guide.Title != "Quick Start Guide" {
		t.Errorf("expected title 'Quick Start Guide', got %s", guide.Title)
	}
	if guide.Category != CategoryGettingStarted {
		t.Error("Category not set")
	}
	if len(guide.Tags) != 2 {
		t.Errorf("expected 2 tags, got %d", len(guide.Tags))
	}
}

func TestGuideBuilder_Section(t *testing.T) {
	guide := NewGuide("test", "Test").
		Section("intro", "Introduction", "Welcome to the guide.").
		Section("setup", "Setup", "How to set up.").
		Build()

	if len(guide.Sections) != 2 {
		t.Errorf("expected 2 sections, got %d", len(guide.Sections))
	}
	if guide.Sections[0].ID != "intro" {
		t.Error("First section ID incorrect")
	}
}

func TestNewSection(t *testing.T) {
	section := NewSection("auth", "Authentication").
		Content("This section covers authentication.").
		Subsection("api-keys", "API Keys", "How to use API keys.").
		Subsection("oauth", "OAuth", "How to use OAuth.").
		Code("bash", "curl -H 'X-API-Key: xxx' https://api.stored.ge", "Example request").
		Info("Note", "API keys are tenant-specific.").
		Warning("Security", "Never share your API keys.").
		Build()

	if section.ID != "auth" {
		t.Error("Section ID not set")
	}
	if len(section.Subsections) != 2 {
		t.Errorf("expected 2 subsections, got %d", len(section.Subsections))
	}
	if len(section.CodeBlocks) != 1 {
		t.Errorf("expected 1 code block, got %d", len(section.CodeBlocks))
	}
	if len(section.Notes) != 2 {
		t.Errorf("expected 2 notes, got %d", len(section.Notes))
	}
}

func TestSectionBuilder_Notes(t *testing.T) {
	section := NewSection("test", "Test").
		Info("Info", "Info content").
		Warning("Warning", "Warning content").
		Tip("Tip", "Tip content").
		Danger("Danger", "Danger content").
		Build()

	if len(section.Notes) != 4 {
		t.Errorf("expected 4 notes, got %d", len(section.Notes))
	}

	if section.Notes[0].Type != NoteTypeInfo {
		t.Error("First note should be info")
	}
	if section.Notes[3].Type != NoteTypeDanger {
		t.Error("Last note should be danger")
	}
}

func TestNewLibrary(t *testing.T) {
	library := NewLibrary()

	if library.guides == nil {
		t.Error("guides map not initialized")
	}
	if library.categories == nil {
		t.Error("categories map not initialized")
	}
}

func TestLibrary_Add(t *testing.T) {
	library := NewLibrary()

	guide := NewGuide("test", "Test Guide").
		Category(CategoryGettingStarted).
		Build()

	library.Add(guide)

	if len(library.guides) != 1 {
		t.Error("Guide not added")
	}
}

func TestLibrary_Get(t *testing.T) {
	library := NewLibrary()
	library.Add(NewGuide("test", "Test").Build())

	guide, ok := library.Get("test")
	if !ok {
		t.Error("Guide not found")
	}
	if guide.ID != "test" {
		t.Error("Wrong guide returned")
	}

	_, ok = library.Get("nonexistent")
	if ok {
		t.Error("Should not find nonexistent guide")
	}
}

func TestLibrary_GetByCategory(t *testing.T) {
	library := NewLibrary()
	library.Add(NewGuide("g1", "Guide 1").Category(CategoryGettingStarted).Build())
	library.Add(NewGuide("g2", "Guide 2").Category(CategoryGettingStarted).Build())
	library.Add(NewGuide("g3", "Guide 3").Category(CategoryAPI).Build())

	guides := library.GetByCategory(CategoryGettingStarted)
	if len(guides) != 2 {
		t.Errorf("expected 2 guides, got %d", len(guides))
	}

	guides = library.GetByCategory(CategoryAPI)
	if len(guides) != 1 {
		t.Errorf("expected 1 guide, got %d", len(guides))
	}
}

func TestLibrary_Search(t *testing.T) {
	library := NewLibrary()
	library.Add(NewGuide("quick", "Quick Start").Description("Get started fast").Tags("beginner").Build())
	library.Add(NewGuide("api", "API Reference").Description("API documentation").Tags("advanced").Build())

	results := library.Search("quick")
	if len(results) != 1 {
		t.Errorf("expected 1 result, got %d", len(results))
	}

	results = library.Search("beginner")
	if len(results) != 1 {
		t.Errorf("expected 1 result from tag search, got %d", len(results))
	}

	results = library.Search("nonexistent")
	if len(results) != 0 {
		t.Error("should find no results")
	}
}

func TestLibrary_All(t *testing.T) {
	library := NewLibrary()
	library.Add(NewGuide("g1", "Guide 1").Build())
	library.Add(NewGuide("g2", "Guide 2").Build())

	all := library.All()
	if len(all) != 2 {
		t.Errorf("expected 2 guides, got %d", len(all))
	}
}

func TestLibrary_Categories(t *testing.T) {
	library := NewLibrary()
	library.Add(NewGuide("g1", "Guide 1").Category(CategoryGettingStarted).Build())
	library.Add(NewGuide("g2", "Guide 2").Category(CategoryAPI).Build())

	cats := library.Categories()
	if len(cats) != 2 {
		t.Errorf("expected 2 categories, got %d", len(cats))
	}
}

func TestRenderer_RenderMarkdown(t *testing.T) {
	guide := NewGuide("test", "Test Guide").
		Description("A test guide").
		Category(CategoryGettingStarted).
		Author("Test Author").
		Tags("test", "example").
		Section("intro", "Introduction", "Welcome to the guide.").
		Build()

	renderer := NewRenderer()
	var buf bytes.Buffer

	err := renderer.RenderMarkdown(&buf, guide)
	if err != nil {
		t.Fatalf("RenderMarkdown failed: %v", err)
	}

	output := buf.String()

	if !strings.Contains(output, "# Test Guide") {
		t.Error("Title not in output")
	}
	if !strings.Contains(output, "## Introduction") {
		t.Error("Section not in output")
	}
	if !strings.Contains(output, "Category: getting-started") {
		t.Error("Category not in output")
	}

	t.Logf("Markdown:\n%s", output)
}

func TestRenderer_RenderMarkdown_WithCodeAndNotes(t *testing.T) {
	section := NewSection("example", "Example").
		Content("Here is an example.").
		Code("go", "fmt.Println(\"Hello\")", "Print hello").
		Info("Note", "This is important.").
		Warning("Warning", "Be careful.").
		Build()

	guide := NewGuide("test", "Test").Build()
	guide.Sections = append(guide.Sections, section)

	renderer := NewRenderer()
	var buf bytes.Buffer

	err := renderer.RenderMarkdown(&buf, guide)
	if err != nil {
		t.Fatalf("RenderMarkdown failed: %v", err)
	}

	output := buf.String()

	if !strings.Contains(output, "```go") {
		t.Error("Code block not in output")
	}
	if !strings.Contains(output, "ℹ️") {
		t.Error("Info note not in output")
	}
	if !strings.Contains(output, "⚠️") {
		t.Error("Warning note not in output")
	}
}

func TestRenderer_RenderHTML(t *testing.T) {
	guide := NewGuide("test", "Test Guide").
		Description("A test guide").
		Category(CategoryGettingStarted).
		Section("intro", "Introduction", "Welcome.").
		Build()

	renderer := NewRenderer()
	var buf bytes.Buffer

	err := renderer.RenderHTML(&buf, guide)
	if err != nil {
		t.Fatalf("RenderHTML failed: %v", err)
	}

	output := buf.String()

	if !strings.Contains(output, "<!DOCTYPE html>") {
		t.Error("Not valid HTML")
	}
	if !strings.Contains(output, "<h1>Test Guide</h1>") {
		t.Error("Title not in output")
	}
	if !strings.Contains(output, "<h2>Introduction</h2>") {
		t.Error("Section not in output")
	}
}

func TestGuide_TableOfContents(t *testing.T) {
	guide := NewGuide("test", "Test").Build()

	section1 := NewSection("intro", "Introduction").
		Subsection("overview", "Overview", "").
		Build()
	section2 := NewSection("setup", "Setup").Build()

	guide.Sections = append(guide.Sections, section1, section2)

	toc := guide.TableOfContents()

	if len(toc) != 2 {
		t.Errorf("expected 2 TOC entries, got %d", len(toc))
	}

	if len(toc[0].Children) != 1 {
		t.Errorf("expected 1 child for first section, got %d", len(toc[0].Children))
	}
}

func TestCategory(t *testing.T) {
	categories := []Category{
		CategoryGettingStarted,
		CategoryAuthentication,
		CategoryStorage,
		CategoryAPI,
		CategorySDK,
		CategoryBilling,
		CategorySecurity,
		CategoryTroubleshooting,
	}

	for _, cat := range categories {
		if cat == "" {
			t.Error("category should not be empty")
		}
	}
}

func TestNoteType(t *testing.T) {
	types := []NoteType{
		NoteTypeInfo,
		NoteTypeWarning,
		NoteTypeTip,
		NoteTypeDanger,
	}

	for _, nt := range types {
		if nt == "" {
			t.Error("note type should not be empty")
		}
	}
}

func TestCodeBlock(t *testing.T) {
	cb := CodeBlock{
		Language:    "python",
		Code:        "print('hello')",
		Description: "Print hello",
		Copyable:    true,
	}

	if cb.Language != "python" {
		t.Error("Language not set")
	}
	if !cb.Copyable {
		t.Error("Copyable not set")
	}
}

func TestTOCEntry(t *testing.T) {
	entry := TOCEntry{
		ID:    "intro",
		Title: "Introduction",
		Level: 1,
		Children: []TOCEntry{
			{ID: "overview", Title: "Overview", Level: 2},
		},
	}

	if entry.Level != 1 {
		t.Error("Level not set")
	}
	if len(entry.Children) != 1 {
		t.Error("Children not set")
	}
}
