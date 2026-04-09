package auth

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"strings"
	"time"
)

const verifyTokenExpiry = 24 * time.Hour

// SetVerifySecret sets the HMAC key used for email verification tokens.
func (a *AuthService) SetVerifySecret(secret string) {
	a.verifySecret = []byte(secret)
}

// GenerateEmailVerifyToken creates an HMAC-signed token for email verification.
// Token format: base64(userID|expiry|signature)
func (a *AuthService) GenerateEmailVerifyToken(ctx context.Context, userID string) (string, error) {
	user, exists := a.userIndex[userID]
	if !exists {
		return "", fmt.Errorf("user not found")
	}

	expiry := time.Now().Add(verifyTokenExpiry).Unix()
	payload := fmt.Sprintf("%s|%d", userID, expiry)

	mac := hmac.New(sha256.New, a.verifySecret)
	mac.Write([]byte(payload))
	sig := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))

	token := base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf("%s|%s", payload, sig)))

	// Store token in memory for lookup.
	a.verifyTokens[token] = userID

	// Persist to DB.
	if a.sqlDB != nil {
		_, err := a.sqlDB.ExecContext(ctx, `
			UPDATE users SET email_verify_token = $1, email_verify_sent_at = NOW()
			WHERE id = $2
		`, token, userID)
		if err != nil {
			return "", fmt.Errorf("persist verify token: %w", err)
		}
	}

	_ = user // avoid unused warning in case we need it later
	return token, nil
}

// VerifyEmail validates the token and marks the user's email as verified.
func (a *AuthService) VerifyEmail(ctx context.Context, token string) error {
	// Decode the token.
	decoded, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		return fmt.Errorf("invalid verification token")
	}

	parts := strings.SplitN(string(decoded), "|", 3)
	if len(parts) != 3 {
		return fmt.Errorf("invalid verification token")
	}

	userID := parts[0]
	payload := parts[0] + "|" + parts[1]
	sig := parts[2]

	// Verify HMAC signature.
	mac := hmac.New(sha256.New, a.verifySecret)
	mac.Write([]byte(payload))
	expectedSig := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))

	if !hmac.Equal([]byte(sig), []byte(expectedSig)) {
		return fmt.Errorf("invalid verification token")
	}

	// Check expiry.
	var expiry int64
	if _, err := fmt.Sscanf(parts[1], "%d", &expiry); err != nil {
		return fmt.Errorf("invalid verification token")
	}
	if time.Now().Unix() > expiry {
		return fmt.Errorf("verification token expired")
	}

	// Mark user as verified.
	user, exists := a.userIndex[userID]
	if !exists {
		return fmt.Errorf("user not found")
	}
	user.EmailVerified = true

	// Clean up token.
	delete(a.verifyTokens, token)

	// Persist to DB.
	if a.sqlDB != nil {
		_, err := a.sqlDB.ExecContext(ctx, `
			UPDATE users SET email_verified = TRUE, email_verify_token = NULL
			WHERE id = $1
		`, userID)
		if err != nil {
			return fmt.Errorf("update email verified: %w", err)
		}
	}

	return nil
}

// IsEmailVerified checks whether a user's email has been verified.
func (a *AuthService) IsEmailVerified(_ context.Context, userID string) bool {
	user, exists := a.userIndex[userID]
	if !exists {
		return false
	}
	return user.EmailVerified
}
