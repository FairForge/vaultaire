// Package faq provides utilities for FAQ documentation.
package faq

import (
	"fmt"
	"io"
	"strings"
	"time"
)

// FAQ represents a frequently asked questions section.
type FAQ struct {
	ID          string
	Title       string
	Description string
	Categories  []Category
	UpdatedAt   time.Time
}

// Category represents a FAQ category.
type Category struct {
	ID          string
	Name        string
	Description string
	Icon        string
	Questions   []Question
	Order       int
}

// Question represents a FAQ question.
type Question struct {
	ID         string
	Question   string
	Answer     string
	Category   string
	Tags       []string
	Related    []string
	Helpful    int
	NotHelpful int
	UpdatedAt  time.Time
}

// FAQBuilder helps construct FAQs.
type FAQBuilder struct {
	faq *FAQ
}

// NewFAQ creates a FAQ builder.
func NewFAQ(id, title string) *FAQBuilder {
	return &FAQBuilder{
		faq: &FAQ{
			ID:         id,
			Title:      title,
			Categories: make([]Category, 0),
			UpdatedAt:  time.Now(),
		},
	}
}

// Description sets the description.
func (b *FAQBuilder) Description(desc string) *FAQBuilder {
	b.faq.Description = desc
	return b
}

// Category adds a category.
func (b *FAQBuilder) Category(c Category) *FAQBuilder {
	b.faq.Categories = append(b.faq.Categories, c)
	return b
}

// Build returns the completed FAQ.
func (b *FAQBuilder) Build() *FAQ {
	return b.faq
}

// CategoryBuilder helps construct categories.
type CategoryBuilder struct {
	category *Category
}

// NewCategory creates a category builder.
func NewCategory(id, name string) *CategoryBuilder {
	return &CategoryBuilder{
		category: &Category{
			ID:        id,
			Name:      name,
			Questions: make([]Question, 0),
		},
	}
}

// Description sets the description.
func (b *CategoryBuilder) Description(desc string) *CategoryBuilder {
	b.category.Description = desc
	return b
}

// Icon sets the icon.
func (b *CategoryBuilder) Icon(icon string) *CategoryBuilder {
	b.category.Icon = icon
	return b
}

// Order sets the display order.
func (b *CategoryBuilder) Order(order int) *CategoryBuilder {
	b.category.Order = order
	return b
}

// Question adds a question.
func (b *CategoryBuilder) Question(q Question) *CategoryBuilder {
	b.category.Questions = append(b.category.Questions, q)
	return b
}

// Build returns the completed category.
func (b *CategoryBuilder) Build() Category {
	return *b.category
}

// QuestionBuilder helps construct questions.
type QuestionBuilder struct {
	question *Question
}

// NewQuestion creates a question builder.
func NewQuestion(id, question, answer string) *QuestionBuilder {
	return &QuestionBuilder{
		question: &Question{
			ID:        id,
			Question:  question,
			Answer:    answer,
			Tags:      make([]string, 0),
			Related:   make([]string, 0),
			UpdatedAt: time.Now(),
		},
	}
}

// Category sets the category.
func (b *QuestionBuilder) Category(cat string) *QuestionBuilder {
	b.question.Category = cat
	return b
}

// Tag adds a tag.
func (b *QuestionBuilder) Tag(tag string) *QuestionBuilder {
	b.question.Tags = append(b.question.Tags, tag)
	return b
}

// Related adds a related question ID.
func (b *QuestionBuilder) Related(id string) *QuestionBuilder {
	b.question.Related = append(b.question.Related, id)
	return b
}

// Feedback sets feedback counts.
func (b *QuestionBuilder) Feedback(helpful, notHelpful int) *QuestionBuilder {
	b.question.Helpful = helpful
	b.question.NotHelpful = notHelpful
	return b
}

// Build returns the completed question.
func (b *QuestionBuilder) Build() Question {
	return *b.question
}

// FAQRegistry manages FAQ questions.
type FAQRegistry struct {
	questions  map[string]*Question
	categories map[string]*Category
}

// NewFAQRegistry creates a FAQ registry.
func NewFAQRegistry() *FAQRegistry {
	return &FAQRegistry{
		questions:  make(map[string]*Question),
		categories: make(map[string]*Category),
	}
}

// AddQuestion adds a question.
func (r *FAQRegistry) AddQuestion(q *Question) {
	r.questions[q.ID] = q
}

// GetQuestion retrieves a question.
func (r *FAQRegistry) GetQuestion(id string) (*Question, bool) {
	q, ok := r.questions[id]
	return q, ok
}

// AddCategory adds a category.
func (r *FAQRegistry) AddCategory(c *Category) {
	r.categories[c.ID] = c
}

// GetCategory retrieves a category.
func (r *FAQRegistry) GetCategory(id string) (*Category, bool) {
	c, ok := r.categories[id]
	return c, ok
}

// ListQuestions returns all questions.
func (r *FAQRegistry) ListQuestions() []*Question {
	result := make([]*Question, 0, len(r.questions))
	for _, q := range r.questions {
		result = append(result, q)
	}
	return result
}

// ListCategories returns all categories.
func (r *FAQRegistry) ListCategories() []*Category {
	result := make([]*Category, 0, len(r.categories))
	for _, c := range r.categories {
		result = append(result, c)
	}
	return result
}

// ByCategory returns questions by category.
func (r *FAQRegistry) ByCategory(catID string) []*Question {
	result := make([]*Question, 0)
	for _, q := range r.questions {
		if q.Category == catID {
			result = append(result, q)
		}
	}
	return result
}

// ByTag returns questions by tag.
func (r *FAQRegistry) ByTag(tag string) []*Question {
	result := make([]*Question, 0)
	for _, q := range r.questions {
		for _, t := range q.Tags {
			if t == tag {
				result = append(result, q)
				break
			}
		}
	}
	return result
}

// Search searches questions.
func (r *FAQRegistry) Search(query string) []*Question {
	query = strings.ToLower(query)
	result := make([]*Question, 0)
	for _, q := range r.questions {
		if strings.Contains(strings.ToLower(q.Question), query) ||
			strings.Contains(strings.ToLower(q.Answer), query) {
			result = append(result, q)
		}
	}
	return result
}

// MostHelpful returns the most helpful questions.
func (r *FAQRegistry) MostHelpful(limit int) []*Question {
	questions := r.ListQuestions()

	// Simple sort by helpful count
	for i := 0; i < len(questions)-1; i++ {
		for j := i + 1; j < len(questions); j++ {
			if questions[j].Helpful > questions[i].Helpful {
				questions[i], questions[j] = questions[j], questions[i]
			}
		}
	}

	if limit > len(questions) {
		limit = len(questions)
	}
	return questions[:limit]
}

// FAQRenderer renders FAQ documentation.
type FAQRenderer struct{}

// NewFAQRenderer creates a renderer.
func NewFAQRenderer() *FAQRenderer {
	return &FAQRenderer{}
}

// RenderFAQ renders a FAQ to Markdown.
func (r *FAQRenderer) RenderFAQ(w io.Writer, faq *FAQ) error {
	var sb strings.Builder

	sb.WriteString("# " + faq.Title + "\n\n")
	if faq.Description != "" {
		sb.WriteString(faq.Description + "\n\n")
	}

	// Table of contents
	if len(faq.Categories) > 1 {
		sb.WriteString("## Categories\n\n")
		for _, cat := range faq.Categories {
			icon := cat.Icon
			if icon == "" {
				icon = "📁"
			}
			fmt.Fprintf(&sb, "- %s [%s](#%s) (%d questions)\n",
				icon, cat.Name, cat.ID, len(cat.Questions))
		}
		sb.WriteString("\n")
	}

	// Categories and questions
	for _, cat := range faq.Categories {
		r.renderCategory(&sb, cat)
	}

	_, err := w.Write([]byte(sb.String()))
	return err
}

// renderCategory renders a category to Markdown.
func (r *FAQRenderer) renderCategory(sb *strings.Builder, cat Category) {
	icon := cat.Icon
	if icon == "" {
		icon = "📁"
	}
	fmt.Fprintf(sb, "## %s %s\n\n", icon, cat.Name)

	if cat.Description != "" {
		sb.WriteString(cat.Description + "\n\n")
	}

	for _, q := range cat.Questions {
		r.renderQuestion(sb, q)
	}
}

// renderQuestion renders a question to Markdown.
func (r *FAQRenderer) renderQuestion(sb *strings.Builder, q Question) {
	sb.WriteString("### " + q.Question + "\n\n")
	sb.WriteString(q.Answer + "\n\n")

	if len(q.Tags) > 0 {
		sb.WriteString("**Tags:** ")
		for i, tag := range q.Tags {
			if i > 0 {
				sb.WriteString(", ")
			}
			sb.WriteString("`" + tag + "`")
		}
		sb.WriteString("\n\n")
	}

	if len(q.Related) > 0 {
		sb.WriteString("**Related:** ")
		for i, rel := range q.Related {
			if i > 0 {
				sb.WriteString(", ")
			}
			sb.WriteString("[" + rel + "](#" + rel + ")")
		}
		sb.WriteString("\n\n")
	}
}

// RenderRegistry renders a registry to Markdown.
func (r *FAQRenderer) RenderRegistry(w io.Writer, reg *FAQRegistry, title string) error {
	var sb strings.Builder

	sb.WriteString("# " + title + "\n\n")

	// Group by category
	categories := reg.ListCategories()
	for _, cat := range categories {
		icon := cat.Icon
		if icon == "" {
			icon = "📁"
		}
		fmt.Fprintf(&sb, "## %s %s\n\n", icon, cat.Name)

		questions := reg.ByCategory(cat.ID)
		for _, q := range questions {
			r.renderQuestion(&sb, *q)
		}
	}

	// Uncategorized
	uncategorized := make([]*Question, 0)
	for _, q := range reg.ListQuestions() {
		if q.Category == "" {
			uncategorized = append(uncategorized, q)
		}
	}
	if len(uncategorized) > 0 {
		sb.WriteString("## Other Questions\n\n")
		for _, q := range uncategorized {
			r.renderQuestion(&sb, *q)
		}
	}

	_, err := w.Write([]byte(sb.String()))
	return err
}

// RenderSearchResults renders search results.
func (r *FAQRenderer) RenderSearchResults(w io.Writer, questions []*Question, query string) error {
	var sb strings.Builder

	fmt.Fprintf(&sb, "# Search Results for \"%s\"\n\n", query)
	fmt.Fprintf(&sb, "Found %d results.\n\n", len(questions))

	for _, q := range questions {
		r.renderQuestion(&sb, *q)
	}

	_, err := w.Write([]byte(sb.String()))
	return err
}

// QuickAnswer represents a quick answer card.
type QuickAnswer struct {
	Question  string
	Answer    string
	LearnMore string
}

// RenderQuickAnswers renders quick answer cards.
func (r *FAQRenderer) RenderQuickAnswers(w io.Writer, answers []QuickAnswer) error {
	var sb strings.Builder

	sb.WriteString("## Quick Answers\n\n")

	for _, qa := range answers {
		sb.WriteString("### " + qa.Question + "\n\n")
		sb.WriteString("> " + qa.Answer + "\n\n")
		if qa.LearnMore != "" {
			sb.WriteString("[Learn more](" + qa.LearnMore + ")\n\n")
		}
	}

	_, err := w.Write([]byte(sb.String()))
	return err
}
