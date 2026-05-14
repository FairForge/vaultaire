package email

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest"
	"go.uber.org/zap/zaptest/observer"
)

func TestLogSender_Send(t *testing.T) {
	core, logs := observer.New(zap.InfoLevel)
	logger := zap.New(core)

	sender := &LogSender{logger: logger, rl: newTestRateLimiter()}
	err := sender.Send(context.Background(), "user@example.com", "Test Subject", "<h1>Hi</h1>", "Hi")

	require.NoError(t, err)
	require.Equal(t, 1, logs.Len())

	entry := logs.All()[0]
	assert.Equal(t, "email (dev mode — not sent)", entry.Message)

	fields := make(map[string]string)
	for _, f := range entry.Context {
		fields[f.Key] = f.String
	}
	assert.Equal(t, "user@example.com", fields["to"])
	assert.Equal(t, "Test Subject", fields["subject"])
	assert.Equal(t, "<h1>Hi</h1>", fields["html"])
	assert.Equal(t, "Hi", fields["text"])
}

func TestLogSender_RateLimit(t *testing.T) {
	logger := zaptest.NewLogger(t)
	sender := &LogSender{logger: logger, rl: newTestRateLimiter()}
	ctx := context.Background()

	for i := 0; i < 10; i++ {
		err := sender.Send(ctx, "flood@example.com", "msg", "<p>x</p>", "x")
		require.NoError(t, err, "send %d should succeed", i)
	}

	err := sender.Send(ctx, "flood@example.com", "msg", "<p>x</p>", "x")
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrRateLimited))

	err = sender.Send(ctx, "other@example.com", "msg", "<p>x</p>", "x")
	require.NoError(t, err, "different recipient should not be rate limited")
}

func TestResendSender_Send(t *testing.T) {
	var gotBody resendRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "Bearer test-api-key", r.Header.Get("Authorization"))
		assert.Equal(t, "application/json", r.Header.Get("Content-Type"))

		body, _ := io.ReadAll(r.Body)
		require.NoError(t, json.Unmarshal(body, &gotBody))

		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"msg_123"}`))
	}))
	defer server.Close()

	sender := &ResendSender{apiKey: "test-api-key", from: "noreply@stored.ge", rl: newTestRateLimiter()}
	origSend := sender.Send

	// Patch the URL by wrapping Send — instead, use httptest.NewServer and
	// override the Resend URL for testing. Since we can't easily override a
	// const URL, we'll test the request building via the mock server.
	_ = origSend

	// For the actual test, use a custom HTTP client approach. Since ResendSender
	// uses http.DefaultClient, we swap the transport.
	origTransport := http.DefaultTransport
	http.DefaultTransport = &rewriteTransport{base: origTransport, target: server.URL}
	defer func() { http.DefaultTransport = origTransport }()

	err := sender.Send(context.Background(), "user@example.com", "Hello", "<h1>Hi</h1>", "Hi")
	require.NoError(t, err)

	assert.Equal(t, "noreply@stored.ge", gotBody.From)
	assert.Equal(t, []string{"user@example.com"}, gotBody.To)
	assert.Equal(t, "Hello", gotBody.Subject)
	assert.Equal(t, "<h1>Hi</h1>", gotBody.HTML)
	assert.Equal(t, "Hi", gotBody.Text)
}

func TestResendSender_APIError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = w.Write([]byte(`{"message":"invalid from address"}`))
	}))
	defer server.Close()

	sender := &ResendSender{apiKey: "test-key", from: "bad@x", rl: newTestRateLimiter()}

	origTransport := http.DefaultTransport
	http.DefaultTransport = &rewriteTransport{base: origTransport, target: server.URL}
	defer func() { http.DefaultTransport = origTransport }()

	err := sender.Send(context.Background(), "user@example.com", "Subj", "<p>x</p>", "x")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "422")
	assert.Contains(t, err.Error(), "invalid from address")
}

func TestSMTPSender_BuildMessage(t *testing.T) {
	msg := buildMIMEMessage("noreply@stored.ge", "user@example.com", "Test Subject", "<h1>Hi</h1>", "Hi")
	s := string(msg)

	assert.Contains(t, s, "From: noreply@stored.ge")
	assert.Contains(t, s, "To: user@example.com")
	assert.Contains(t, s, "Subject: Test Subject")
	assert.Contains(t, s, "MIME-Version: 1.0")
	assert.Contains(t, s, "multipart/alternative")
	assert.Contains(t, s, "text/plain")
	assert.Contains(t, s, "text/html")
	assert.Contains(t, s, "<h1>Hi</h1>")
	assert.Contains(t, s, "Hi")
}

func TestRenderVerification(t *testing.T) {
	html, text, err := RenderVerification("https://stored.ge", "abc123", "user@example.com")
	require.NoError(t, err)

	assert.Contains(t, html, "https://stored.ge/verify?token=abc123")
	assert.Contains(t, html, "user@example.com")
	assert.Contains(t, html, "Verify")

	assert.Contains(t, text, "https://stored.ge/verify?token=abc123")
	assert.NotContains(t, text, "<")
}

func TestRenderPasswordReset(t *testing.T) {
	html, text, err := RenderPasswordReset("https://stored.ge", "reset-tok", "user@example.com")
	require.NoError(t, err)

	assert.Contains(t, html, "https://stored.ge/reset-password?token=reset-tok")
	assert.Contains(t, html, "user@example.com")
	assert.Contains(t, html, "Reset")

	assert.Contains(t, text, "https://stored.ge/reset-password?token=reset-tok")
	assert.NotContains(t, text, "<")
}

func TestRenderWelcome(t *testing.T) {
	html, text, err := RenderWelcome("user@example.com", "AKID12345")
	require.NoError(t, err)

	assert.Contains(t, html, "AKID12345")
	assert.Contains(t, html, "Welcome")
	assert.Contains(t, html, "stored.ge")

	assert.Contains(t, text, "AKID12345")
	assert.NotContains(t, text, "<")
}

func TestNewSender_EnvRouting(t *testing.T) {
	logger := zaptest.NewLogger(t)

	// Default: LogSender.
	s := NewSender(logger)
	assert.IsType(t, &LogSender{}, s)

	// Resend with key.
	t.Setenv("EMAIL_PROVIDER", "resend")
	t.Setenv("RESEND_API_KEY", "re_test123")
	s = NewSender(logger)
	assert.IsType(t, &ResendSender{}, s)

	// Resend without key falls back.
	t.Setenv("RESEND_API_KEY", "")
	s = NewSender(logger)
	assert.IsType(t, &LogSender{}, s)

	// SMTP with config.
	t.Setenv("EMAIL_PROVIDER", "smtp")
	t.Setenv("SMTP_HOST", "smtp.example.com")
	t.Setenv("SMTP_PORT", "587")
	s = NewSender(logger)
	assert.IsType(t, &SMTPSender{}, s)

	// SMTP without config falls back.
	t.Setenv("SMTP_HOST", "")
	s = NewSender(logger)
	assert.IsType(t, &LogSender{}, s)

	// Unknown provider.
	t.Setenv("EMAIL_PROVIDER", "mailgun")
	s = NewSender(logger)
	assert.IsType(t, &LogSender{}, s)
}

// rewriteTransport redirects HTTPS requests to the test server.
type rewriteTransport struct {
	base   http.RoundTripper
	target string
}

func (t *rewriteTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if strings.Contains(req.URL.Host, "resend.com") {
		u, _ := http.NewRequest(req.Method, t.target+req.URL.Path, req.Body)
		u.Header = req.Header
		return t.base.RoundTrip(u)
	}
	return t.base.RoundTrip(req)
}

func newTestRateLimiter() *rateLimiter {
	return &rateLimiter{
		window:   make(map[string][]time.Time),
		limit:    10,
		interval: time.Minute,
	}
}
