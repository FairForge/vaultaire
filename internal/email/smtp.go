package email

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/smtp"
	"strings"
)

// SMTPSender sends emails via a generic SMTP server.
// Supports STARTTLS (port 587) and implicit TLS (port 465).
type SMTPSender struct {
	host     string
	port     string
	user     string
	password string
	from     string
	rl       *rateLimiter
}

func (s *SMTPSender) Send(_ context.Context, to, subject, htmlBody, textBody string) error {
	if err := s.rl.check(to); err != nil {
		return err
	}

	msg := buildMIMEMessage(s.from, to, subject, htmlBody, textBody)
	addr := net.JoinHostPort(s.host, s.port)

	if s.port == "465" {
		return s.sendImplicitTLS(addr, to, msg)
	}
	return s.sendSTARTTLS(addr, to, msg)
}

func (s *SMTPSender) sendSTARTTLS(addr, to string, msg []byte) error {
	var a smtp.Auth
	if s.user != "" {
		a = smtp.PlainAuth("", s.user, s.password, s.host)
	}
	return smtp.SendMail(addr, a, s.from, []string{to}, msg)
}

func (s *SMTPSender) sendImplicitTLS(addr, to string, msg []byte) error {
	conn, err := tls.Dial("tcp", addr, &tls.Config{ServerName: s.host})
	if err != nil {
		return fmt.Errorf("tls dial %s: %w", addr, err)
	}
	defer func() { _ = conn.Close() }()

	c, err := smtp.NewClient(conn, s.host)
	if err != nil {
		return fmt.Errorf("smtp client: %w", err)
	}
	defer func() { _ = c.Close() }()

	if s.user != "" {
		if err := c.Auth(smtp.PlainAuth("", s.user, s.password, s.host)); err != nil {
			return fmt.Errorf("smtp auth: %w", err)
		}
	}
	if err := c.Mail(s.from); err != nil {
		return fmt.Errorf("smtp mail: %w", err)
	}
	if err := c.Rcpt(to); err != nil {
		return fmt.Errorf("smtp rcpt: %w", err)
	}
	w, err := c.Data()
	if err != nil {
		return fmt.Errorf("smtp data: %w", err)
	}
	if _, err := w.Write(msg); err != nil {
		return fmt.Errorf("smtp write: %w", err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("smtp close data: %w", err)
	}
	return c.Quit()
}

const boundary = "----vaultaire-email-boundary"

func buildMIMEMessage(from, to, subject, htmlBody, textBody string) []byte {
	var b strings.Builder
	b.WriteString("From: " + from + "\r\n")
	b.WriteString("To: " + to + "\r\n")
	b.WriteString("Subject: " + subject + "\r\n")
	b.WriteString("MIME-Version: 1.0\r\n")
	b.WriteString("Content-Type: multipart/alternative; boundary=\"" + boundary + "\"\r\n")
	b.WriteString("\r\n")

	b.WriteString("--" + boundary + "\r\n")
	b.WriteString("Content-Type: text/plain; charset=\"utf-8\"\r\n")
	b.WriteString("Content-Transfer-Encoding: 7bit\r\n")
	b.WriteString("\r\n")
	b.WriteString(textBody)
	b.WriteString("\r\n")

	b.WriteString("--" + boundary + "\r\n")
	b.WriteString("Content-Type: text/html; charset=\"utf-8\"\r\n")
	b.WriteString("Content-Transfer-Encoding: 7bit\r\n")
	b.WriteString("\r\n")
	b.WriteString(htmlBody)
	b.WriteString("\r\n")

	b.WriteString("--" + boundary + "--\r\n")
	return []byte(b.String())
}
