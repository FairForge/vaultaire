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

// --- payments (WP-PAY-3, docs/PAYMENTS.md § 3.8) ---

// ReceiptLine is one line of a receipt mail.
type ReceiptLine struct {
	Label string
	TB    int
	Money string
}

// ReceiptMail is payment_receipt.html's data (rendered from the ledger
// row by internal/payments; nothing here reads a table).
type ReceiptMail struct {
	BaseURL, Email, InvoiceNumber, Amount, PaidAt, House, Period, ReceiptURL, Issuer string
	Lines                                                                            []ReceiptLine
}

// RenderPaymentReceipt renders the receipt mail.
func RenderPaymentReceipt(d ReceiptMail) (string, string, error) {
	return render("payment_receipt.html", d)
}

// ReminderMail is renewal_reminder.html's data: the copy is chosen by
// InGrace / LastDay (else the before-the-date copy).
type ReminderMail struct {
	BaseURL, Email, House, PaidThrough, GraceEnds, RenewURL string
	DaysLeft                                                int
	InGrace, LastDay                                        bool
}

// RenderRenewalReminder renders one reminder step.
func RenderRenewalReminder(d ReminderMail) (string, string, error) {
	return render("renewal_reminder.html", d)
}

// LapsedMail is house_lapsed.html's data.
type LapsedMail struct {
	BaseURL, Email, House, PaidThrough, RenewURL string
}

// RenderHouseLapsed renders the mail the lapse job sends after its Revoke.
func RenderHouseLapsed(d LapsedMail) (string, string, error) {
	return render("house_lapsed.html", d)
}
