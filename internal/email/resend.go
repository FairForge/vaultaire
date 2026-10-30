package email

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

// ResendSender sends emails via the Resend API (https://resend.com).
// Uses raw net/http, no external SDK dependency.
type ResendSender struct {
	apiKey string
	from   string
	rl     *rateLimiter
}

type resendRequest struct {
	From    string   `json:"from"`
	To      []string `json:"to"`
	Subject string   `json:"subject"`
	HTML    string   `json:"html"`
	Text    string   `json:"text"`
}

// resendBody is the API request's JSON. Resend writes the values into the
// mail's headers itself, so the senders' one guard applies here too
// (ErrHeaderValue, RP84-3).
func resendBody(from, to, subject, htmlBody, textBody string) ([]byte, error) {
	if err := checkHeaderValues(from, to, subject); err != nil {
		return nil, err
	}
	body, err := json.Marshal(resendRequest{From: from, To: []string{to}, Subject: subject, HTML: htmlBody, Text: textBody})
	if err != nil {
		return nil, fmt.Errorf("marshal resend request: %w", err)
	}
	return body, nil
}

func (s *ResendSender) Send(ctx context.Context, to, subject, htmlBody, textBody string) error {
	if err := s.rl.check(to); err != nil {
		return err
	}

	body, err := resendBody(s.from, to, subject, htmlBody, textBody)
	if err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://api.resend.com/emails", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("create resend request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+s.apiKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("resend API request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode >= 400 {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return fmt.Errorf("resend API error (status %d): %s", resp.StatusCode, string(respBody))
	}

	return nil
}
