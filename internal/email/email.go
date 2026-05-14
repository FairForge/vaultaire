package email

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"

	"go.uber.org/zap"
)

// ErrRateLimited is returned when a recipient exceeds the per-recipient
// rate limit of 10 emails per minute.
var ErrRateLimited = errors.New("email rate limit exceeded")

// Sender sends emails. Implementations must be safe for concurrent use.
type Sender interface {
	Send(ctx context.Context, to, subject, htmlBody, textBody string) error
}

// NewSender returns a Sender based on environment configuration.
// EMAIL_PROVIDER selects the backend: "resend", "smtp", or anything else
// falls back to LogSender (logs the full email, zero config).
func NewSender(logger *zap.Logger) Sender {
	from := os.Getenv("EMAIL_FROM")
	if from == "" {
		from = "noreply@stored.ge"
	}

	rl := &rateLimiter{
		window:   make(map[string][]time.Time),
		limit:    10,
		interval: time.Minute,
	}

	switch os.Getenv("EMAIL_PROVIDER") {
	case "resend":
		apiKey := os.Getenv("RESEND_API_KEY")
		if apiKey == "" {
			logger.Warn("EMAIL_PROVIDER=resend but RESEND_API_KEY is empty, falling back to log sender")
			return &LogSender{logger: logger, rl: rl}
		}
		return &ResendSender{apiKey: apiKey, from: from, rl: rl}
	case "smtp":
		host := os.Getenv("SMTP_HOST")
		port := os.Getenv("SMTP_PORT")
		if host == "" || port == "" {
			logger.Warn("EMAIL_PROVIDER=smtp but SMTP_HOST/SMTP_PORT missing, falling back to log sender")
			return &LogSender{logger: logger, rl: rl}
		}
		return &SMTPSender{
			host:     host,
			port:     port,
			user:     os.Getenv("SMTP_USER"),
			password: os.Getenv("SMTP_PASSWORD"),
			from:     from,
			rl:       rl,
		}
	default:
		return &LogSender{logger: logger, rl: rl}
	}
}

// LogSender logs emails at Info level instead of sending them.
// Used in development and as the zero-config fallback.
type LogSender struct {
	logger *zap.Logger
	rl     *rateLimiter
}

func (s *LogSender) Send(_ context.Context, to, subject, htmlBody, textBody string) error {
	if err := s.rl.check(to); err != nil {
		return err
	}
	s.logger.Info("email (dev mode — not sent)",
		zap.String("to", to),
		zap.String("subject", subject),
		zap.String("html", htmlBody),
		zap.String("text", textBody))
	return nil
}

// rateLimiter enforces a per-recipient sliding window rate limit.
type rateLimiter struct {
	mu       sync.Mutex
	window   map[string][]time.Time
	limit    int
	interval time.Duration
}

func (rl *rateLimiter) check(recipient string) error {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	now := time.Now()
	cutoff := now.Add(-rl.interval)

	entries := rl.window[recipient]
	kept := entries[:0]
	for _, t := range entries {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}

	if len(kept) >= rl.limit {
		rl.window[recipient] = kept
		return fmt.Errorf("%w: %s (max %d per %s)", ErrRateLimited, recipient, rl.limit, rl.interval)
	}

	rl.window[recipient] = append(kept, now)
	return nil
}
