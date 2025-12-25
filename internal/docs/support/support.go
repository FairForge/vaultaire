// Package support provides utilities for support documentation.
package support

import (
	"fmt"
	"io"
	"strings"
	"time"
)

// KnowledgeBase represents a support knowledge base.
type KnowledgeBase struct {
	ID          string
	Title       string
	Description string
	Articles    []Article
	UpdatedAt   time.Time
}

// Article represents a knowledge base article.
type Article struct {
	ID         string
	Title      string
	Summary    string
	Content    string
	Category   ArticleCategory
	Tags       []string
	Author     string
	Views      int
	Helpful    int
	NotHelpful int
	Related    []string
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

// ArticleCategory categorizes articles.
type ArticleCategory string

const (
	CategoryHowTo        ArticleCategory = "how-to"
	CategoryReference    ArticleCategory = "reference"
	CategoryConcept      ArticleCategory = "concept"
	CategoryTutorial     ArticleCategory = "tutorial"
	CategoryTroubleshoot ArticleCategory = "troubleshooting"
)

// ContactMethod represents a support contact method.
type ContactMethod struct {
	Name         string
	Type         ContactType
	Value        string
	Hours        string
	ResponseTime string
	Priority     int
}

// ContactType categorizes contact methods.
type ContactType string

const (
	ContactEmail     ContactType = "email"
	ContactChat      ContactType = "chat"
	ContactPhone     ContactType = "phone"
	ContactTicket    ContactType = "ticket"
	ContactCommunity ContactType = "community"
)

// SupportPlan represents a support plan.
type SupportPlan struct {
	Name         string
	Description  string
	Features     []string
	ResponseTime ResponseSLA
	Channels     []ContactType
	Price        string
}

// ResponseSLA represents response time SLAs.
type ResponseSLA struct {
	Critical time.Duration
	High     time.Duration
	Medium   time.Duration
	Low      time.Duration
}

// SupportResource represents a support resource.
type SupportResource struct {
	Name        string
	Type        ResourceType
	URL         string
	Description string
	Icon        string
}

// ResourceType categorizes support resources.
type ResourceType string

const (
	ResourceDocumentation ResourceType = "documentation"
	ResourceCommunity     ResourceType = "community"
	ResourceStatus        ResourceType = "status"
	ResourceChangelog     ResourceType = "changelog"
	ResourceAPI           ResourceType = "api"
)

// ArticleBuilder helps construct articles.
type ArticleBuilder struct {
	article *Article
}

// NewArticle creates an article builder.
func NewArticle(id, title string) *ArticleBuilder {
	return &ArticleBuilder{
		article: &Article{
			ID:        id,
			Title:     title,
			Tags:      make([]string, 0),
			Related:   make([]string, 0),
			CreatedAt: time.Now(),
			UpdatedAt: time.Now(),
		},
	}
}

// Summary sets the summary.
func (b *ArticleBuilder) Summary(s string) *ArticleBuilder {
	b.article.Summary = s
	return b
}

// Content sets the content.
func (b *ArticleBuilder) Content(c string) *ArticleBuilder {
	b.article.Content = c
	return b
}

// Category sets the category.
func (b *ArticleBuilder) Category(c ArticleCategory) *ArticleBuilder {
	b.article.Category = c
	return b
}

// Tag adds a tag.
func (b *ArticleBuilder) Tag(t string) *ArticleBuilder {
	b.article.Tags = append(b.article.Tags, t)
	return b
}

// Author sets the author.
func (b *ArticleBuilder) Author(a string) *ArticleBuilder {
	b.article.Author = a
	return b
}

// Related adds a related article.
func (b *ArticleBuilder) Related(id string) *ArticleBuilder {
	b.article.Related = append(b.article.Related, id)
	return b
}

// Stats sets view and feedback stats.
func (b *ArticleBuilder) Stats(views, helpful, notHelpful int) *ArticleBuilder {
	b.article.Views = views
	b.article.Helpful = helpful
	b.article.NotHelpful = notHelpful
	return b
}

// Build returns the completed article.
func (b *ArticleBuilder) Build() *Article {
	return b.article
}

// KnowledgeBaseBuilder helps construct knowledge bases.
type KnowledgeBaseBuilder struct {
	kb *KnowledgeBase
}

// NewKnowledgeBase creates a knowledge base builder.
func NewKnowledgeBase(id, title string) *KnowledgeBaseBuilder {
	return &KnowledgeBaseBuilder{
		kb: &KnowledgeBase{
			ID:        id,
			Title:     title,
			Articles:  make([]Article, 0),
			UpdatedAt: time.Now(),
		},
	}
}

// Description sets the description.
func (b *KnowledgeBaseBuilder) Description(d string) *KnowledgeBaseBuilder {
	b.kb.Description = d
	return b
}

// Article adds an article.
func (b *KnowledgeBaseBuilder) Article(a Article) *KnowledgeBaseBuilder {
	b.kb.Articles = append(b.kb.Articles, a)
	return b
}

// Build returns the completed knowledge base.
func (b *KnowledgeBaseBuilder) Build() *KnowledgeBase {
	return b.kb
}

// SupportPage represents a support landing page.
type SupportPage struct {
	Title           string
	Description     string
	ContactMethods  []ContactMethod
	Plans           []SupportPlan
	Resources       []SupportResource
	PopularArticles []Article
}

// SupportPageBuilder helps construct support pages.
type SupportPageBuilder struct {
	page *SupportPage
}

// NewSupportPage creates a support page builder.
func NewSupportPage(title string) *SupportPageBuilder {
	return &SupportPageBuilder{
		page: &SupportPage{
			Title:           title,
			ContactMethods:  make([]ContactMethod, 0),
			Plans:           make([]SupportPlan, 0),
			Resources:       make([]SupportResource, 0),
			PopularArticles: make([]Article, 0),
		},
	}
}

// Description sets the description.
func (b *SupportPageBuilder) Description(d string) *SupportPageBuilder {
	b.page.Description = d
	return b
}

// Contact adds a contact method.
func (b *SupportPageBuilder) Contact(c ContactMethod) *SupportPageBuilder {
	b.page.ContactMethods = append(b.page.ContactMethods, c)
	return b
}

// Plan adds a support plan.
func (b *SupportPageBuilder) Plan(p SupportPlan) *SupportPageBuilder {
	b.page.Plans = append(b.page.Plans, p)
	return b
}

// Resource adds a resource.
func (b *SupportPageBuilder) Resource(r SupportResource) *SupportPageBuilder {
	b.page.Resources = append(b.page.Resources, r)
	return b
}

// PopularArticle adds a popular article.
func (b *SupportPageBuilder) PopularArticle(a Article) *SupportPageBuilder {
	b.page.PopularArticles = append(b.page.PopularArticles, a)
	return b
}

// Build returns the completed support page.
func (b *SupportPageBuilder) Build() *SupportPage {
	return b.page
}

// SupportRenderer renders support documentation.
type SupportRenderer struct{}

// NewSupportRenderer creates a renderer.
func NewSupportRenderer() *SupportRenderer {
	return &SupportRenderer{}
}

// RenderArticle renders an article to Markdown.
func (r *SupportRenderer) RenderArticle(w io.Writer, a *Article) error {
	var sb strings.Builder

	sb.WriteString("# " + a.Title + "\n\n")

	if a.Summary != "" {
		sb.WriteString("*" + a.Summary + "*\n\n")
	}

	// Metadata
	sb.WriteString("---\n")
	fmt.Fprintf(&sb, "Category: %s\n", a.Category)
	if a.Author != "" {
		fmt.Fprintf(&sb, "Author: %s\n", a.Author)
	}
	fmt.Fprintf(&sb, "Updated: %s\n", a.UpdatedAt.Format("2006-01-02"))
	sb.WriteString("---\n\n")

	// Content
	if a.Content != "" {
		sb.WriteString(a.Content + "\n\n")
	}

	// Tags
	if len(a.Tags) > 0 {
		sb.WriteString("**Tags:** ")
		for i, tag := range a.Tags {
			if i > 0 {
				sb.WriteString(", ")
			}
			sb.WriteString("`" + tag + "`")
		}
		sb.WriteString("\n\n")
	}

	// Related
	if len(a.Related) > 0 {
		sb.WriteString("## Related Articles\n\n")
		for _, rel := range a.Related {
			sb.WriteString("- [" + rel + "](#" + rel + ")\n")
		}
		sb.WriteString("\n")
	}

	// Feedback
	sb.WriteString("---\n\n")
	sb.WriteString("*Was this article helpful?* 👍 👎\n")

	_, err := w.Write([]byte(sb.String()))
	return err
}

// RenderKnowledgeBase renders a knowledge base to Markdown.
func (r *SupportRenderer) RenderKnowledgeBase(w io.Writer, kb *KnowledgeBase) error {
	var sb strings.Builder

	sb.WriteString("# " + kb.Title + "\n\n")
	if kb.Description != "" {
		sb.WriteString(kb.Description + "\n\n")
	}

	// Group by category
	categories := make(map[ArticleCategory][]Article)
	for _, a := range kb.Articles {
		categories[a.Category] = append(categories[a.Category], a)
	}

	// Table of contents
	sb.WriteString("## Categories\n\n")
	for cat, articles := range categories {
		icon := categoryIcon(cat)
		fmt.Fprintf(&sb, "- %s [%s](#%s) (%d articles)\n",
			icon, cat, cat, len(articles))
	}
	sb.WriteString("\n")

	// Articles by category
	for cat, articles := range categories {
		icon := categoryIcon(cat)
		fmt.Fprintf(&sb, "## %s %s\n\n", icon, cat)

		sb.WriteString("| Article | Views |\n")
		sb.WriteString("|---------|-------|\n")
		for _, a := range articles {
			fmt.Fprintf(&sb, "| [%s](#%s) | %d |\n", a.Title, a.ID, a.Views)
		}
		sb.WriteString("\n")
	}

	_, err := w.Write([]byte(sb.String()))
	return err
}

// RenderSupportPage renders a support page to Markdown.
func (r *SupportRenderer) RenderSupportPage(w io.Writer, page *SupportPage) error {
	var sb strings.Builder

	sb.WriteString("# " + page.Title + "\n\n")
	if page.Description != "" {
		sb.WriteString(page.Description + "\n\n")
	}

	// Contact Methods
	if len(page.ContactMethods) > 0 {
		sb.WriteString("## Contact Us\n\n")
		for _, c := range page.ContactMethods {
			icon := contactIcon(c.Type)
			fmt.Fprintf(&sb, "### %s %s\n\n", icon, c.Name)
			fmt.Fprintf(&sb, "- **Contact:** %s\n", c.Value)
			if c.Hours != "" {
				fmt.Fprintf(&sb, "- **Hours:** %s\n", c.Hours)
			}
			if c.ResponseTime != "" {
				fmt.Fprintf(&sb, "- **Response Time:** %s\n", c.ResponseTime)
			}
			sb.WriteString("\n")
		}
	}

	// Support Plans
	if len(page.Plans) > 0 {
		sb.WriteString("## Support Plans\n\n")
		for _, p := range page.Plans {
			fmt.Fprintf(&sb, "### %s\n\n", p.Name)
			sb.WriteString(p.Description + "\n\n")
			if p.Price != "" {
				fmt.Fprintf(&sb, "**Price:** %s\n\n", p.Price)
			}
			if len(p.Features) > 0 {
				sb.WriteString("**Features:**\n")
				for _, f := range p.Features {
					sb.WriteString("- ✅ " + f + "\n")
				}
				sb.WriteString("\n")
			}
		}
	}

	// Resources
	if len(page.Resources) > 0 {
		sb.WriteString("## Resources\n\n")
		for _, res := range page.Resources {
			icon := res.Icon
			if icon == "" {
				icon = resourceIcon(res.Type)
			}
			fmt.Fprintf(&sb, "- %s [%s](%s)", icon, res.Name, res.URL)
			if res.Description != "" {
				sb.WriteString(" - " + res.Description)
			}
			sb.WriteString("\n")
		}
		sb.WriteString("\n")
	}

	// Popular Articles
	if len(page.PopularArticles) > 0 {
		sb.WriteString("## Popular Articles\n\n")
		for _, a := range page.PopularArticles {
			fmt.Fprintf(&sb, "- [%s](#%s)", a.Title, a.ID)
			if a.Summary != "" {
				sb.WriteString(" - " + a.Summary)
			}
			sb.WriteString("\n")
		}
		sb.WriteString("\n")
	}

	_, err := w.Write([]byte(sb.String()))
	return err
}

// categoryIcon returns an icon for article category.
func categoryIcon(c ArticleCategory) string {
	switch c {
	case CategoryHowTo:
		return "📝"
	case CategoryReference:
		return "📚"
	case CategoryConcept:
		return "💡"
	case CategoryTutorial:
		return "🎓"
	case CategoryTroubleshoot:
		return "🔧"
	default:
		return "📄"
	}
}

// contactIcon returns an icon for contact type.
func contactIcon(t ContactType) string {
	switch t {
	case ContactEmail:
		return "📧"
	case ContactChat:
		return "💬"
	case ContactPhone:
		return "📞"
	case ContactTicket:
		return "🎫"
	case ContactCommunity:
		return "👥"
	default:
		return "📱"
	}
}

// resourceIcon returns an icon for resource type.
func resourceIcon(t ResourceType) string {
	switch t {
	case ResourceDocumentation:
		return "📚"
	case ResourceCommunity:
		return "👥"
	case ResourceStatus:
		return "🟢"
	case ResourceChangelog:
		return "📋"
	case ResourceAPI:
		return "🔌"
	default:
		return "🔗"
	}
}
