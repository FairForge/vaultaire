package email

import (
	"bytes"
	"embed"
	"html/template"
	"regexp"
	"strings"
)

//go:embed templates/*.html
var templateFS embed.FS

var templates = template.Must(template.ParseFS(templateFS, "templates/*.html"))

var tagRe = regexp.MustCompile(`<[^>]*>`)

func stripTags(html string) string {
	text := tagRe.ReplaceAllString(html, "")
	var lines []string
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			lines = append(lines, line)
		}
	}
	return strings.Join(lines, "\n")
}

func render(name string, data any) (htmlBody, textBody string, err error) {
	var buf bytes.Buffer
	if err := templates.ExecuteTemplate(&buf, name, data); err != nil {
		return "", "", err
	}
	htmlBody = buf.String()
	textBody = stripTags(htmlBody)
	return htmlBody, textBody, nil
}

type verificationData struct {
	BaseURL string
	Token   string
	Email   string
}

// RenderVerification renders the email verification template.
func RenderVerification(baseURL, token, email string) (string, string, error) {
	return render("verification.html", verificationData{
		BaseURL: baseURL,
		Token:   token,
		Email:   email,
	})
}

type passwordResetData struct {
	BaseURL string
	Token   string
	Email   string
}

// RenderPasswordReset renders the password reset template.
func RenderPasswordReset(baseURL, token, email string) (string, string, error) {
	return render("password_reset.html", passwordResetData{
		BaseURL: baseURL,
		Token:   token,
		Email:   email,
	})
}

type welcomeData struct {
	Email     string
	AccessKey string
}

// RenderWelcome renders the welcome email template.
func RenderWelcome(email, accessKey string) (string, string, error) {
	return render("welcome.html", welcomeData{
		Email:     email,
		AccessKey: accessKey,
	})
}
