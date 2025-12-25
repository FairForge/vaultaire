package faq

import (
	"bytes"
	"strings"
	"testing"
)

func TestNewFAQ(t *testing.T) {
	faq := NewFAQ("main-faq", "Frequently Asked Questions").
		Description("Common questions about Vaultaire").
		Build()

	if faq.ID != "main-faq" {
		t.Errorf("expected ID 'main-faq', got %s", faq.ID)
	}
	if faq.Description == "" {
		t.Error("Description not set")
	}
}

func TestFAQBuilder_Category(t *testing.T) {
	cat := NewCategory("general", "General Questions").
		Description("Common questions").
		Icon("❓").
		Order(1).
		Build()

	faq := NewFAQ("test", "Test").
		Category(cat).
		Build()

	if len(faq.Categories) != 1 {
		t.Error("Category not added")
	}
	if faq.Categories[0].Icon != "❓" {
		t.Error("Icon not set")
	}
}

func TestNewCategory(t *testing.T) {
	q1 := NewQuestion("q1", "What is Vaultaire?", "A storage platform.").Build()
	q2 := NewQuestion("q2", "How do I start?", "Run the installer.").Build()

	cat := NewCategory("getting-started", "Getting Started").
		Description("Start here").
		Icon("🚀").
		Order(1).
		Question(q1).
		Question(q2).
		Build()

	if cat.ID != "getting-started" {
		t.Error("ID not set")
	}
	if len(cat.Questions) != 2 {
		t.Errorf("expected 2 questions, got %d", len(cat.Questions))
	}
}

func TestNewQuestion(t *testing.T) {
	q := NewQuestion("pricing", "How much does it cost?", "Starting at $3.99/TB.").
		Category("billing").
		Tag("pricing").
		Tag("billing").
		Related("storage-tiers").
		Related("free-tier").
		Feedback(42, 3).
		Build()

	if q.ID != "pricing" {
		t.Error("ID not set")
	}
	if q.Category != "billing" {
		t.Error("Category not set")
	}
	if len(q.Tags) != 2 {
		t.Errorf("expected 2 tags, got %d", len(q.Tags))
	}
	if len(q.Related) != 2 {
		t.Errorf("expected 2 related, got %d", len(q.Related))
	}
	if q.Helpful != 42 {
		t.Error("Helpful not set")
	}
}

func TestFAQRegistry(t *testing.T) {
	reg := NewFAQRegistry()

	cat1 := &Category{ID: "general", Name: "General", Icon: "❓"}
	cat2 := &Category{ID: "billing", Name: "Billing", Icon: "💰"}
	reg.AddCategory(cat1)
	reg.AddCategory(cat2)

	q1 := &Question{ID: "q1", Question: "What is it?", Category: "general", Tags: []string{"basics"}}
	q2 := &Question{ID: "q2", Question: "How much?", Category: "billing", Tags: []string{"pricing"}}
	q3 := &Question{ID: "q3", Question: "Is it fast?", Category: "general", Tags: []string{"performance"}, Helpful: 10}
	q4 := &Question{ID: "q4", Question: "Best practices?", Category: "general", Helpful: 25}

	reg.AddQuestion(q1)
	reg.AddQuestion(q2)
	reg.AddQuestion(q3)
	reg.AddQuestion(q4)

	// GetQuestion
	q, ok := reg.GetQuestion("q1")
	if !ok {
		t.Error("Question not found")
	}
	if q.Question != "What is it?" {
		t.Error("Wrong question returned")
	}

	// GetCategory
	c, ok := reg.GetCategory("general")
	if !ok {
		t.Error("Category not found")
	}
	if c.Name != "General" {
		t.Error("Wrong category returned")
	}

	// ListQuestions
	all := reg.ListQuestions()
	if len(all) != 4 {
		t.Errorf("expected 4 questions, got %d", len(all))
	}

	// ListCategories
	cats := reg.ListCategories()
	if len(cats) != 2 {
		t.Errorf("expected 2 categories, got %d", len(cats))
	}

	// ByCategory
	general := reg.ByCategory("general")
	if len(general) != 3 {
		t.Errorf("expected 3 general questions, got %d", len(general))
	}

	// ByTag
	basics := reg.ByTag("basics")
	if len(basics) != 1 {
		t.Errorf("expected 1 basics question, got %d", len(basics))
	}

	// Search
	results := reg.Search("fast")
	if len(results) != 1 {
		t.Errorf("expected 1 search result, got %d", len(results))
	}

	// MostHelpful
	helpful := reg.MostHelpful(2)
	if len(helpful) != 2 {
		t.Errorf("expected 2 helpful questions, got %d", len(helpful))
	}
	if helpful[0].Helpful != 25 {
		t.Error("MostHelpful not sorted correctly")
	}
}

func TestFAQRenderer_RenderFAQ(t *testing.T) {
	q1 := NewQuestion("what-is", "What is Vaultaire?", "A distributed storage platform.").
		Tag("basics").
		Build()

	q2 := NewQuestion("pricing", "How much does it cost?", "Starting at $3.99/TB.").
		Tag("billing").
		Related("free-tier").
		Build()

	cat := NewCategory("general", "General").
		Description("Common questions").
		Icon("❓").
		Question(q1).
		Question(q2).
		Build()

	faq := NewFAQ("main", "FAQ").
		Description("Frequently Asked Questions").
		Category(cat).
		Build()

	renderer := NewFAQRenderer()
	var buf bytes.Buffer

	err := renderer.RenderFAQ(&buf, faq)
	if err != nil {
		t.Fatalf("RenderFAQ failed: %v", err)
	}

	output := buf.String()

	if !strings.Contains(output, "# FAQ") {
		t.Error("Title not in output")
	}
	if !strings.Contains(output, "## ❓ General") {
		t.Error("Category not in output")
	}
	if !strings.Contains(output, "### What is Vaultaire?") {
		t.Error("Question not in output")
	}
	if !strings.Contains(output, "`basics`") {
		t.Error("Tags not in output")
	}
	if !strings.Contains(output, "[free-tier]") {
		t.Error("Related not in output")
	}

	t.Logf("FAQ:\n%s", output)
}

func TestFAQRenderer_RenderRegistry(t *testing.T) {
	reg := NewFAQRegistry()
	reg.AddCategory(&Category{ID: "general", Name: "General", Icon: "❓"})
	reg.AddQuestion(&Question{ID: "q1", Question: "Question 1?", Answer: "Answer 1", Category: "general"})

	renderer := NewFAQRenderer()
	var buf bytes.Buffer

	err := renderer.RenderRegistry(&buf, reg, "Help Center")
	if err != nil {
		t.Fatalf("RenderRegistry failed: %v", err)
	}

	output := buf.String()

	if !strings.Contains(output, "# Help Center") {
		t.Error("Title not in output")
	}
	if !strings.Contains(output, "Question 1?") {
		t.Error("Question not in output")
	}

	t.Logf("Registry:\n%s", output)
}

func TestFAQRenderer_RenderSearchResults(t *testing.T) {
	questions := []*Question{
		{ID: "q1", Question: "How to upload?", Answer: "Use PUT request."},
		{ID: "q2", Question: "How to download?", Answer: "Use GET request."},
	}

	renderer := NewFAQRenderer()
	var buf bytes.Buffer

	err := renderer.RenderSearchResults(&buf, questions, "how to")
	if err != nil {
		t.Fatalf("RenderSearchResults failed: %v", err)
	}

	output := buf.String()

	if !strings.Contains(output, "Search Results for \"how to\"") {
		t.Error("Query not in output")
	}
	if !strings.Contains(output, "Found 2 results") {
		t.Error("Count not in output")
	}

	t.Logf("Search Results:\n%s", output)
}

func TestFAQRenderer_RenderQuickAnswers(t *testing.T) {
	answers := []QuickAnswer{
		{Question: "What is it?", Answer: "A storage platform.", LearnMore: "/docs/overview"},
		{Question: "How much?", Answer: "$3.99/TB", LearnMore: "/pricing"},
	}

	renderer := NewFAQRenderer()
	var buf bytes.Buffer

	err := renderer.RenderQuickAnswers(&buf, answers)
	if err != nil {
		t.Fatalf("RenderQuickAnswers failed: %v", err)
	}

	output := buf.String()

	if !strings.Contains(output, "## Quick Answers") {
		t.Error("Title not in output")
	}
	if !strings.Contains(output, "[Learn more]") {
		t.Error("Learn more link not in output")
	}

	t.Logf("Quick Answers:\n%s", output)
}

func TestFAQRegistry_Uncategorized(t *testing.T) {
	reg := NewFAQRegistry()
	reg.AddQuestion(&Question{ID: "q1", Question: "Uncategorized?", Answer: "Yes"})

	renderer := NewFAQRenderer()
	var buf bytes.Buffer

	err := renderer.RenderRegistry(&buf, reg, "FAQ")
	if err != nil {
		t.Fatalf("failed: %v", err)
	}

	output := buf.String()
	if !strings.Contains(output, "## Other Questions") {
		t.Error("Uncategorized section not in output")
	}
}

func TestQuickAnswer(t *testing.T) {
	qa := QuickAnswer{
		Question:  "What is it?",
		Answer:    "A platform",
		LearnMore: "/docs",
	}

	if qa.LearnMore != "/docs" {
		t.Error("LearnMore not set")
	}
}
