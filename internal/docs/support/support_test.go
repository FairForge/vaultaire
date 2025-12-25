package support

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

func TestNewArticle(t *testing.T) {
	article := NewArticle("getting-started", "Getting Started Guide").
		Summary("Learn how to get started").
		Content("## Step 1\n\nDo this first.").
		Category(CategoryHowTo).
		Author("Isaac").
		Tag("beginner").
		Tag("setup").
		Related("installation").
		Stats(150, 42, 3).
		Build()

	if article.ID != "getting-started" {
		t.Errorf("expected ID 'getting-started', got %s", article.ID)
	}
	if article.Category != CategoryHowTo {
		t.Error("Category not set")
	}
	if len(article.Tags) != 2 {
		t.Errorf("expected 2 tags, got %d", len(article.Tags))
	}
	if article.Views != 150 {
		t.Error("Views not set")
	}
}

func TestNewKnowledgeBase(t *testing.T) {
	a1 := NewArticle("a1", "Article 1").Category(CategoryHowTo).Build()
	a2 := NewArticle("a2", "Article 2").Category(CategoryReference).Build()

	kb := NewKnowledgeBase("main", "Knowledge Base").
		Description("Help documentation").
		Article(*a1).
		Article(*a2).
		Build()

	if kb.ID != "main" {
		t.Error("ID not set")
	}
	if len(kb.Articles) != 2 {
		t.Errorf("expected 2 articles, got %d", len(kb.Articles))
	}
}

func TestNewSupportPage(t *testing.T) {
	page := NewSupportPage("Support Center").
		Description("Get help with Vaultaire").
		Contact(ContactMethod{
			Name:         "Email Support",
			Type:         ContactEmail,
			Value:        "support@stored.ge",
			Hours:        "24/7",
			ResponseTime: "< 24 hours",
		}).
		Plan(SupportPlan{
			Name:        "Basic",
			Description: "Community support",
			Features:    []string{"Forum access", "Documentation"},
			Price:       "Free",
		}).
		Resource(SupportResource{
			Name:        "Documentation",
			Type:        ResourceDocumentation,
			URL:         "https://docs.stored.ge",
			Description: "Full documentation",
		}).
		PopularArticle(*NewArticle("setup", "Quick Setup").Summary("Get started fast").Build()).
		Build()

	if page.Title != "Support Center" {
		t.Error("Title not set")
	}
	if len(page.ContactMethods) != 1 {
		t.Error("Contact not added")
	}
	if len(page.Plans) != 1 {
		t.Error("Plan not added")
	}
	if len(page.Resources) != 1 {
		t.Error("Resource not added")
	}
	if len(page.PopularArticles) != 1 {
		t.Error("PopularArticle not added")
	}
}

func TestSupportRenderer_RenderArticle(t *testing.T) {
	article := NewArticle("upload-files", "How to Upload Files").
		Summary("Learn to upload files to Vaultaire").
		Content("## Prerequisites\n\nYou need an API key.\n\n## Steps\n\n1. Get your key\n2. Upload file").
		Category(CategoryHowTo).
		Author("Isaac").
		Tag("upload").
		Tag("s3").
		Related("create-bucket").
		Stats(100, 25, 2).
		Build()

	renderer := NewSupportRenderer()
	var buf bytes.Buffer

	err := renderer.RenderArticle(&buf, article)
	if err != nil {
		t.Fatalf("RenderArticle failed: %v", err)
	}

	output := buf.String()

	if !strings.Contains(output, "# How to Upload Files") {
		t.Error("Title not in output")
	}
	if !strings.Contains(output, "Category: how-to") {
		t.Error("Category not in output")
	}
	if !strings.Contains(output, "`upload`") {
		t.Error("Tags not in output")
	}
	if !strings.Contains(output, "## Related Articles") {
		t.Error("Related section not in output")
	}
	if !strings.Contains(output, "Was this article helpful") {
		t.Error("Feedback prompt not in output")
	}

	t.Logf("Article:\n%s", output)
}

func TestSupportRenderer_RenderKnowledgeBase(t *testing.T) {
	a1 := Article{ID: "a1", Title: "How to Upload", Category: CategoryHowTo, Views: 100}
	a2 := Article{ID: "a2", Title: "API Reference", Category: CategoryReference, Views: 50}
	a3 := Article{ID: "a3", Title: "Upload Troubleshooting", Category: CategoryTroubleshoot, Views: 75}

	kb := NewKnowledgeBase("kb", "Help Center").
		Description("Find answers to your questions").
		Article(a1).
		Article(a2).
		Article(a3).
		Build()

	renderer := NewSupportRenderer()
	var buf bytes.Buffer

	err := renderer.RenderKnowledgeBase(&buf, kb)
	if err != nil {
		t.Fatalf("RenderKnowledgeBase failed: %v", err)
	}

	output := buf.String()

	if !strings.Contains(output, "# Help Center") {
		t.Error("Title not in output")
	}
	if !strings.Contains(output, "## Categories") {
		t.Error("Categories section not in output")
	}
	if !strings.Contains(output, "📝") {
		t.Error("Category icon not in output")
	}

	t.Logf("Knowledge Base:\n%s", output)
}

func TestSupportRenderer_RenderSupportPage(t *testing.T) {
	page := NewSupportPage("Get Support").
		Description("We're here to help").
		Contact(ContactMethod{
			Name:         "Email",
			Type:         ContactEmail,
			Value:        "support@stored.ge",
			Hours:        "Mon-Fri 9-5",
			ResponseTime: "< 4 hours",
		}).
		Contact(ContactMethod{
			Name:  "Community Forum",
			Type:  ContactCommunity,
			Value: "https://community.stored.ge",
		}).
		Plan(SupportPlan{
			Name:        "Pro",
			Description: "Priority support",
			Features:    []string{"Priority response", "Phone support"},
			Price:       "$99/mo",
			ResponseTime: ResponseSLA{
				Critical: 1 * time.Hour,
				High:     4 * time.Hour,
			},
		}).
		Resource(SupportResource{
			Name:        "Status Page",
			Type:        ResourceStatus,
			URL:         "https://status.stored.ge",
			Description: "System status",
		}).
		PopularArticle(Article{ID: "quick-start", Title: "Quick Start", Summary: "Get started in 5 minutes"}).
		Build()

	renderer := NewSupportRenderer()
	var buf bytes.Buffer

	err := renderer.RenderSupportPage(&buf, page)
	if err != nil {
		t.Fatalf("RenderSupportPage failed: %v", err)
	}

	output := buf.String()

	if !strings.Contains(output, "# Get Support") {
		t.Error("Title not in output")
	}
	if !strings.Contains(output, "## Contact Us") {
		t.Error("Contact section not in output")
	}
	if !strings.Contains(output, "## Support Plans") {
		t.Error("Plans section not in output")
	}
	if !strings.Contains(output, "## Resources") {
		t.Error("Resources section not in output")
	}
	if !strings.Contains(output, "## Popular Articles") {
		t.Error("Popular Articles section not in output")
	}
	if !strings.Contains(output, "📧") {
		t.Error("Contact icon not in output")
	}

	t.Logf("Support Page:\n%s", output)
}

func TestCategoryIcon(t *testing.T) {
	tests := []struct {
		cat      ArticleCategory
		expected string
	}{
		{CategoryHowTo, "📝"},
		{CategoryReference, "📚"},
		{CategoryConcept, "💡"},
		{CategoryTutorial, "🎓"},
		{CategoryTroubleshoot, "🔧"},
	}

	for _, tt := range tests {
		result := categoryIcon(tt.cat)
		if result != tt.expected {
			t.Errorf("categoryIcon(%s) = %s, expected %s", tt.cat, result, tt.expected)
		}
	}
}

func TestContactIcon(t *testing.T) {
	tests := []struct {
		ct       ContactType
		expected string
	}{
		{ContactEmail, "📧"},
		{ContactChat, "💬"},
		{ContactPhone, "📞"},
		{ContactTicket, "🎫"},
		{ContactCommunity, "👥"},
	}

	for _, tt := range tests {
		result := contactIcon(tt.ct)
		if result != tt.expected {
			t.Errorf("contactIcon(%s) = %s, expected %s", tt.ct, result, tt.expected)
		}
	}
}

func TestResourceIcon(t *testing.T) {
	tests := []struct {
		rt       ResourceType
		expected string
	}{
		{ResourceDocumentation, "📚"},
		{ResourceCommunity, "👥"},
		{ResourceStatus, "🟢"},
		{ResourceChangelog, "📋"},
		{ResourceAPI, "🔌"},
	}

	for _, tt := range tests {
		result := resourceIcon(tt.rt)
		if result != tt.expected {
			t.Errorf("resourceIcon(%s) = %s, expected %s", tt.rt, result, tt.expected)
		}
	}
}

func TestResponseSLA(t *testing.T) {
	sla := ResponseSLA{
		Critical: 1 * time.Hour,
		High:     4 * time.Hour,
		Medium:   8 * time.Hour,
		Low:      24 * time.Hour,
	}

	if sla.Critical != 1*time.Hour {
		t.Error("Critical SLA not set")
	}
}

func TestContactMethod(t *testing.T) {
	cm := ContactMethod{
		Name:         "Live Chat",
		Type:         ContactChat,
		Value:        "https://chat.stored.ge",
		Hours:        "24/7",
		ResponseTime: "< 5 minutes",
		Priority:     1,
	}

	if cm.Priority != 1 {
		t.Error("Priority not set")
	}
}

func TestSupportResource(t *testing.T) {
	res := SupportResource{
		Name:        "API Docs",
		Type:        ResourceAPI,
		URL:         "https://api.stored.ge/docs",
		Description: "API documentation",
		Icon:        "🔌",
	}

	if res.Icon != "🔌" {
		t.Error("Icon not set")
	}
}
