package payments

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/FairForge/vaultaire/internal/email"
	"github.com/FairForge/vaultaire/internal/hosts"
	"go.uber.org/zap"
)

// Mailer sends the three payment mails (WP-PAY-3, docs/PAYMENTS.md § 3.8)
// through the one email.Sender: the receipt of a newly settled row, a
// renewal reminder step, the house-lapsed notice. Every send is best
// effort for its caller: a webhook never answers 500 because a mail
// failed, the lapse job's Revoke has committed before its mail goes.
type Mailer struct {
	DB      *sql.DB
	Sender  email.Sender
	BaseURL string
	Legal   hosts.LegalEntity
	Logger  *zap.Logger
}

// NewMailer builds one; nil Sender = nothing is sent (every call succeeds).
func NewMailer(db *sql.DB, sender email.Sender, baseURL string, legal hosts.LegalEntity, logger *zap.Logger) *Mailer {
	if logger == nil {
		logger = zap.NewNop()
	}
	return &Mailer{DB: db, Sender: sender, BaseURL: strings.TrimRight(baseURL, "/"), Legal: legal, Logger: logger}
}

func (m *Mailer) renewURL() string { return m.BaseURL + "/dashboard/billing" }

func (m *Mailer) tenantEmail(ctx context.Context, tenantID string) (string, error) {
	var e sql.NullString
	if err := m.DB.QueryRowContext(ctx, `SELECT email FROM tenants WHERE id = $1`, tenantID).Scan(&e); err != nil {
		return "", fmt.Errorf("read e-mail of tenant %s: %w", tenantID, err)
	}
	if !e.Valid || e.String == "" {
		return "", fmt.Errorf("tenant %s has no e-mail", tenantID)
	}
	return e.String, nil
}

// SendReceipt mails the receipt of a settled row to the tenant.
func (m *Mailer) SendReceipt(ctx context.Context, tenantID, paymentID string) error {
	if m == nil || m.Sender == nil || m.DB == nil {
		return nil
	}
	r, err := ReadReceipt(ctx, m.DB, tenantID, paymentID)
	if err != nil {
		return err
	}
	if r.Email == "" {
		return errors.New("the tenant has no e-mail")
	}
	d := email.ReceiptMail{BaseURL: m.BaseURL, Email: r.Email, InvoiceNumber: r.InvoiceNumber, Amount: r.Amount(),
		PaidAt: r.PaidAt.UTC().Format("2 Jan 2006"), Period: r.PeriodWords(), Issuer: m.Legal.Name,
		ReceiptURL: m.BaseURL + "/dashboard/billing/receipts/" + r.ID}
	if r.Order != nil {
		d.House = r.Order.Words()
	}
	for _, l := range r.Lines {
		d.Lines = append(d.Lines, email.ReceiptLine{Label: l.Label, TB: l.TB, Money: l.Money()})
	}
	html, text, err := email.RenderPaymentReceipt(d)
	if err != nil {
		return fmt.Errorf("render receipt %s: %w", r.InvoiceNumber, err)
	}
	return m.Sender.Send(ctx, r.Email, "Your Stored receipt "+r.InvoiceNumber, html, text)
}

// SendReminder mails one reminder step.
func (m *Mailer) SendReminder(ctx context.Context, d DueReminder) error {
	if m == nil || m.Sender == nil {
		return nil
	}
	if d.Email == "" {
		return errors.New("the tenant has no e-mail")
	}
	data := email.ReminderMail{BaseURL: m.BaseURL, Email: d.Email, House: d.Order.Words(),
		PaidThrough: d.PaidThrough.UTC().Format("2 Jan 2006"), GraceEnds: d.GraceEnds.UTC().Format("2 Jan 2006"),
		RenewURL: m.renewURL(), DaysLeft: d.DaysLeft, InGrace: d.Step == StepInGrace, LastDay: d.Step == StepLastDay}
	html, text, err := email.RenderRenewalReminder(data)
	if err != nil {
		return fmt.Errorf("render reminder %s: %w", d.Step, err)
	}
	subject := fmt.Sprintf("Your Stored house is paid for %d more days", d.DaysLeft)
	switch d.Step {
	case StepInGrace:
		subject = fmt.Sprintf("Your Stored house is in its grace period: %d days left", d.DaysLeft)
	case StepLastDay:
		subject = "Last day: your Stored house ends tomorrow"
	}
	return m.Sender.Send(ctx, d.Email, subject, html, text)
}

// SendHouseLapsed mails the notice the lapse job sends after its Revoke
// committed.
func (m *Mailer) SendHouseLapsed(ctx context.Context, tenantID string, e Entitlement) error {
	if m == nil || m.Sender == nil || m.DB == nil {
		return nil
	}
	to, err := m.tenantEmail(ctx, tenantID)
	if err != nil {
		return err
	}
	html, text, err := email.RenderHouseLapsed(email.LapsedMail{BaseURL: m.BaseURL, Email: to, House: e.Order.Words(),
		PaidThrough: e.PaidThrough.UTC().Format("2 Jan 2006"), RenewURL: m.renewURL()})
	if err != nil {
		return fmt.Errorf("render house lapsed: %w", err)
	}
	return m.Sender.Send(ctx, to, "Your Stored house has ended — nothing was deleted", html, text)
}

// Detached returns a context for a send after the caller's request has
// been answered: never cancelled with it, bounded to 15 s.
func Detached(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
}
