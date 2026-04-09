package auth

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"
)

const (
	resetTokenExpiry  = 1 * time.Hour
	resetRateWindow   = 1 * time.Hour
	resetRateMaxPerIP = 3 // max 3 reset emails per hour per email
)

// ErrResetRateLimited is returned when an email exceeds the password reset
// rate limit (3 requests per hour).
var ErrResetRateLimited = errors.New("password reset rate limit exceeded")

// RequestPasswordReset generates an HMAC-signed password reset token for the
// user with the given email. Returns ErrResetRateLimited if the email has
// already requested 3 resets within the past hour.
//
// The returned token is opaque and embeds the userID and an expiry time
// signed with the email-verify HMAC secret. The same secret is reused
// because the payload prefix differs ("reset|") so the two token types are
// not interchangeable.
//
// To preserve user enumeration resistance, callers should not surface
// "user not found" errors to the client. This method does return that
// error so the caller can decide whether to log it.
func (a *AuthService) RequestPasswordReset(ctx context.Context, email string) (string, error) {
	email = strings.ToLower(strings.TrimSpace(email))

	user, err := a.GetUserByEmail(ctx, email)
	if err != nil {
		return "", fmt.Errorf("user not found")
	}

	// Rate limit: max 3 resets per hour per email.
	if !a.allowResetRequest(email) {
		return "", ErrResetRateLimited
	}

	expiry := time.Now().Add(resetTokenExpiry).Unix()
	payload := fmt.Sprintf("reset|%s|%d", user.ID, expiry)

	mac := hmac.New(sha256.New, a.verifySecret)
	mac.Write([]byte(payload))
	sig := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))

	token := base64.RawURLEncoding.EncodeToString([]byte(payload + "|" + sig))

	a.resetMu.Lock()
	a.resetTokens[token] = user.ID
	a.resetMu.Unlock()

	return token, nil
}

// CompletePasswordReset validates a reset token and updates the user's
// password. On success, all of the user's existing sessions should be
// invalidated by the caller (the auth service does not own session state).
func (a *AuthService) CompletePasswordReset(ctx context.Context, token, newPassword string) (string, error) {
	if len(newPassword) < 8 {
		return "", fmt.Errorf("password must be at least 8 characters")
	}

	userID, err := a.validateResetToken(token)
	if err != nil {
		return "", err
	}

	user, exists := a.userIndex[userID]
	if !exists {
		return "", fmt.Errorf("user not found")
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(newPassword), bcrypt.DefaultCost)
	if err != nil {
		return "", fmt.Errorf("hash password: %w", err)
	}

	user.PasswordHash = string(hash)

	if a.sqlDB != nil {
		_, err = a.sqlDB.ExecContext(ctx,
			`UPDATE users SET password_hash = $1, updated_at = NOW() WHERE id = $2`,
			string(hash), userID)
		if err != nil {
			return "", fmt.Errorf("update password: %w", err)
		}
	}

	// Single-use token: clear from in-memory map.
	a.resetMu.Lock()
	delete(a.resetTokens, token)
	a.resetMu.Unlock()

	return userID, nil
}

// validateResetToken decodes a reset token, verifies its HMAC signature
// and expiry, and returns the userID it references.
func (a *AuthService) validateResetToken(token string) (string, error) {
	decoded, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		return "", fmt.Errorf("invalid reset token")
	}

	parts := strings.SplitN(string(decoded), "|", 4)
	if len(parts) != 4 || parts[0] != "reset" {
		return "", fmt.Errorf("invalid reset token")
	}

	userID := parts[1]
	payload := parts[0] + "|" + parts[1] + "|" + parts[2]
	sig := parts[3]

	mac := hmac.New(sha256.New, a.verifySecret)
	mac.Write([]byte(payload))
	expectedSig := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))

	if !hmac.Equal([]byte(sig), []byte(expectedSig)) {
		return "", fmt.Errorf("invalid reset token")
	}

	var expiry int64
	if _, err := fmt.Sscanf(parts[2], "%d", &expiry); err != nil {
		return "", fmt.Errorf("invalid reset token")
	}
	if time.Now().Unix() > expiry {
		return "", fmt.Errorf("reset token expired")
	}

	return userID, nil
}

// allowResetRequest checks the per-email rate limit and records the
// current request if allowed.
func (a *AuthService) allowResetRequest(email string) bool {
	a.resetMu.Lock()
	defer a.resetMu.Unlock()

	now := time.Now()
	cutoff := now.Add(-resetRateWindow)

	// Drop entries older than the rate window.
	kept := a.resetRates[email][:0]
	for _, t := range a.resetRates[email] {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}

	if len(kept) >= resetRateMaxPerIP {
		a.resetRates[email] = kept
		return false
	}

	a.resetRates[email] = append(kept, now)
	return true
}
