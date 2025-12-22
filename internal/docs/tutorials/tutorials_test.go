package tutorials

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

func TestNewTutorial(t *testing.T) {
	tutorial := NewTutorial("getting-started", "Getting Started with Vaultaire").
		Description("Learn the basics").
		Category(CategoryGettingStarted).
		Level(LevelBeginner).
		Duration(15 * time.Minute).
		VideoURL("https://youtube.com/watch?v=abc").
		Author("Isaac").
		Build()

	if tutorial.ID != "getting-started" {
		t.Errorf("expected ID 'getting-started', got %s", tutorial.ID)
	}
	if tutorial.Category != CategoryGettingStarted {
		t.Error("Category not set")
	}
	if tutorial.Level != LevelBeginner {
		t.Error("Level not set")
	}
	if tutorial.Duration != 15*time.Minute {
		t.Error("Duration not set")
	}
}

func TestTutorialBuilder_Chapter(t *testing.T) {
	tutorial := NewTutorial("test", "Test").
		Chapter("Introduction", 0, 2*time.Minute, "Overview of the topic").
		Chapter("Setup", 2*time.Minute, 5*time.Minute, "Setting up the environment").
		Chapter("Demo", 5*time.Minute, 12*time.Minute, "Live demonstration").
		Build()

	if len(tutorial.Chapters) != 3 {
		t.Errorf("expected 3 chapters, got %d", len(tutorial.Chapters))
	}
	if tutorial.Chapters[1].StartTime != 2*time.Minute {
		t.Error("StartTime not set")
	}
}

func TestTutorialBuilder_Resource(t *testing.T) {
	tutorial := NewTutorial("test", "Test").
		Resource("Source Code", "https://github.com/example", ResourceCode, "Example code").
		Resource("Documentation", "https://docs.example.com", ResourceDocs, "Full docs").
		Build()

	if len(tutorial.Resources) != 2 {
		t.Errorf("expected 2 resources, got %d", len(tutorial.Resources))
	}
}

func TestTutorialBuilder_Tags(t *testing.T) {
	tutorial := NewTutorial("test", "Test").
		Tag("s3").
		Tag("storage").
		Tag("api").
		Prerequisite("Basic Go knowledge").
		Build()

	if len(tutorial.Tags) != 3 {
		t.Errorf("expected 3 tags, got %d", len(tutorial.Tags))
	}
	if len(tutorial.Prerequisites) != 1 {
		t.Error("Prerequisites not set")
	}
}

func TestNewSeries(t *testing.T) {
	series := NewSeries("s3-mastery", "S3 API Mastery").
		Description("Complete guide to S3 API").
		Level(LevelIntermediate).
		Tutorial("s3-basics").
		Tutorial("s3-advanced").
		Tutorial("s3-performance").
		TotalDuration(2 * time.Hour).
		Build()

	if series.ID != "s3-mastery" {
		t.Error("ID not set")
	}
	if len(series.Tutorials) != 3 {
		t.Errorf("expected 3 tutorials, got %d", len(series.Tutorials))
	}
	if series.TotalDuration != 2*time.Hour {
		t.Error("TotalDuration not set")
	}
}

func TestTutorialLibrary(t *testing.T) {
	lib := NewTutorialLibrary()

	t1 := NewTutorial("t1", "Tutorial 1").
		Category(CategoryGettingStarted).
		Level(LevelBeginner).
		Tag("basics").
		Build()

	t2 := NewTutorial("t2", "Tutorial 2").
		Category(CategoryAdvanced).
		Level(LevelAdvanced).
		Tag("advanced").
		Build()

	t3 := NewTutorial("t3", "Tutorial 3").
		Category(CategoryGettingStarted).
		Level(LevelBeginner).
		Tag("basics").
		Build()

	lib.AddTutorial(t1)
	lib.AddTutorial(t2)
	lib.AddTutorial(t3)

	s1 := NewSeries("s1", "Series 1").Tutorial("t1").Tutorial("t2").Build()
	lib.AddSeries(s1)

	// GetTutorial
	tutorial, ok := lib.GetTutorial("t1")
	if !ok {
		t.Error("Tutorial not found")
	}
	if tutorial.Title != "Tutorial 1" {
		t.Error("Wrong tutorial returned")
	}

	// GetSeries
	series, ok := lib.GetSeries("s1")
	if !ok {
		t.Error("Series not found")
	}
	if series.Title != "Series 1" {
		t.Error("Wrong series returned")
	}

	// ListTutorials
	all := lib.ListTutorials()
	if len(all) != 3 {
		t.Errorf("expected 3 tutorials, got %d", len(all))
	}

	// ListSeries
	allSeries := lib.ListSeries()
	if len(allSeries) != 1 {
		t.Errorf("expected 1 series, got %d", len(allSeries))
	}

	// ByCategory
	gettingStarted := lib.ByCategory(CategoryGettingStarted)
	if len(gettingStarted) != 2 {
		t.Errorf("expected 2 getting-started tutorials, got %d", len(gettingStarted))
	}

	// ByLevel
	beginners := lib.ByLevel(LevelBeginner)
	if len(beginners) != 2 {
		t.Errorf("expected 2 beginner tutorials, got %d", len(beginners))
	}

	// ByTag
	basics := lib.ByTag("basics")
	if len(basics) != 2 {
		t.Errorf("expected 2 basics tutorials, got %d", len(basics))
	}

	// Search
	results := lib.Search("Tutorial 1")
	if len(results) != 1 {
		t.Errorf("expected 1 search result, got %d", len(results))
	}
}

func TestTutorialRenderer_RenderTutorial(t *testing.T) {
	tutorial := NewTutorial("getting-started", "Getting Started").
		Description("Learn the basics of Vaultaire").
		Category(CategoryGettingStarted).
		Level(LevelBeginner).
		Duration(15*time.Minute).
		VideoURL("https://youtube.com/watch?v=abc").
		Thumbnail("https://img.youtube.com/abc.jpg").
		Author("Isaac").
		Prerequisite("Basic programming knowledge").
		Chapter("Intro", 0, 2*time.Minute, "Welcome").
		Chapter("Setup", 2*time.Minute, 5*time.Minute, "Install SDK").
		Resource("Code", "https://github.com/example", ResourceCode, "Sample code").
		Transcript("Hello and welcome to this tutorial...").
		Tag("s3").
		Tag("storage").
		Build()

	renderer := NewTutorialRenderer()
	var buf bytes.Buffer

	err := renderer.RenderTutorial(&buf, tutorial)
	if err != nil {
		t.Fatalf("RenderTutorial failed: %v", err)
	}

	output := buf.String()

	if !strings.Contains(output, "# Getting Started") {
		t.Error("Title not in output")
	}
	if !strings.Contains(output, "🟢 Beginner") {
		t.Error("Level badge not in output")
	}
	if !strings.Contains(output, "## Prerequisites") {
		t.Error("Prerequisites section not in output")
	}
	if !strings.Contains(output, "## Chapters") {
		t.Error("Chapters section not in output")
	}
	if !strings.Contains(output, "## Resources") {
		t.Error("Resources section not in output")
	}
	if !strings.Contains(output, "## Transcript") {
		t.Error("Transcript section not in output")
	}
	if !strings.Contains(output, "`s3`") {
		t.Error("Tags not in output")
	}

	t.Logf("Tutorial:\n%s", output)
}

func TestTutorialRenderer_RenderSeries(t *testing.T) {
	lib := NewTutorialLibrary()
	lib.AddTutorial(NewTutorial("t1", "Part 1").Duration(10 * time.Minute).Build())
	lib.AddTutorial(NewTutorial("t2", "Part 2").Duration(15 * time.Minute).Build())

	series := NewSeries("mastery", "API Mastery").
		Description("Complete API guide").
		Level(LevelIntermediate).
		Tutorial("t1").
		Tutorial("t2").
		TotalDuration(25 * time.Minute).
		Build()

	renderer := NewTutorialRenderer()
	var buf bytes.Buffer

	err := renderer.RenderSeries(&buf, series, lib)
	if err != nil {
		t.Fatalf("RenderSeries failed: %v", err)
	}

	output := buf.String()

	if !strings.Contains(output, "# API Mastery") {
		t.Error("Title not in output")
	}
	if !strings.Contains(output, "Part 1") {
		t.Error("Tutorial 1 not in output")
	}
	if !strings.Contains(output, "Part 2") {
		t.Error("Tutorial 2 not in output")
	}

	t.Logf("Series:\n%s", output)
}

func TestTutorialRenderer_RenderLibraryIndex(t *testing.T) {
	lib := NewTutorialLibrary()
	lib.AddTutorial(NewTutorial("t1", "Tutorial 1").Category(CategoryGettingStarted).Level(LevelBeginner).Duration(10 * time.Minute).Build())
	lib.AddSeries(NewSeries("s1", "Series 1").Level(LevelIntermediate).Tutorial("t1").TotalDuration(1 * time.Hour).Build())

	renderer := NewTutorialRenderer()
	var buf bytes.Buffer

	err := renderer.RenderLibraryIndex(&buf, lib)
	if err != nil {
		t.Fatalf("RenderLibraryIndex failed: %v", err)
	}

	output := buf.String()

	if !strings.Contains(output, "# Video Tutorials") {
		t.Error("Title not in output")
	}
	if !strings.Contains(output, "## Series") {
		t.Error("Series section not in output")
	}

	t.Logf("Library Index:\n%s", output)
}

func TestLevelBadge(t *testing.T) {
	tests := []struct {
		level    Level
		contains string
	}{
		{LevelBeginner, "Beginner"},
		{LevelIntermediate, "Intermediate"},
		{LevelAdvanced, "Advanced"},
	}

	for _, tt := range tests {
		result := levelBadge(tt.level)
		if !strings.Contains(result, tt.contains) {
			t.Errorf("levelBadge(%s) = %s, expected to contain %s", tt.level, result, tt.contains)
		}
	}
}

func TestResourceIcon(t *testing.T) {
	tests := []struct {
		resType  ResourceType
		expected string
	}{
		{ResourceCode, "💻"},
		{ResourceDocs, "📚"},
		{ResourceDownload, "⬇️"},
		{ResourceExternal, "🔗"},
	}

	for _, tt := range tests {
		result := resourceIcon(tt.resType)
		if result != tt.expected {
			t.Errorf("resourceIcon(%s) = %s, expected %s", tt.resType, result, tt.expected)
		}
	}
}

func TestFormatDuration(t *testing.T) {
	tests := []struct {
		duration time.Duration
		expected string
	}{
		{30 * time.Second, "30s"},
		{5 * time.Minute, "5m"},
		{90 * time.Minute, "1h 30m"},
		{2 * time.Hour, "2h"},
	}

	for _, tt := range tests {
		result := formatDuration(tt.duration)
		if result != tt.expected {
			t.Errorf("formatDuration(%v) = %s, expected %s", tt.duration, result, tt.expected)
		}
	}
}

func TestCategory(t *testing.T) {
	categories := []Category{
		CategoryGettingStarted,
		CategoryConfiguration,
		CategoryIntegration,
		CategoryAdvanced,
		CategoryTroubleshooting,
		CategoryBestPractices,
	}

	for _, c := range categories {
		if c == "" {
			t.Error("category should not be empty")
		}
	}
}

func TestChapter(t *testing.T) {
	chapter := Chapter{
		Title:     "Introduction",
		StartTime: 0,
		EndTime:   2 * time.Minute,
		Summary:   "Overview",
	}

	if chapter.EndTime != 2*time.Minute {
		t.Error("EndTime not set")
	}
}

func TestResource(t *testing.T) {
	res := Resource{
		Title:       "Sample Code",
		URL:         "https://github.com/example",
		Type:        ResourceCode,
		Description: "Example implementation",
	}

	if res.Type != ResourceCode {
		t.Error("Type not set")
	}
}
