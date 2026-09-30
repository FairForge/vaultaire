package auth

import (
	"crypto/rand"
	"encoding/base32"
	"fmt"
	"strings"

	"github.com/pquerna/otp"
	"github.com/pquerna/otp/totp"
)

// MFAService mints TOTP secrets and backup codes. Backup-code consumption is
// persisted by AuthService.ValidateBackupCode (user_mfa); the in-memory
// variant that used to live here was never called by the product (R5).
type MFAService struct {
	issuer string
}

func NewMFAService(issuer string) *MFAService {
	return &MFAService{issuer: issuer}
}

// GenerateSecret creates a new TOTP secret for a user
func (m *MFAService) GenerateSecret(email string) (string, string, error) {
	key, err := totp.Generate(totp.GenerateOpts{
		Issuer:      m.issuer,
		AccountName: email,
		Period:      30,
		SecretSize:  20,
		Digits:      otp.DigitsSix,
		Algorithm:   otp.AlgorithmSHA1,
	})
	if err != nil {
		return "", "", fmt.Errorf("generate totp key: %w", err)
	}

	return key.Secret(), key.URL(), nil
}

// ValidateCode checks if the provided TOTP code is valid for the secret.
// There is deliberately no test-only shortcut here: a fixed secret/code pair
// that always validated could be enrolled through the setup form and used to
// satisfy the admin-MFA gate (review R5-15). Tests mint real codes with
// totp.GenerateCode.
func (m *MFAService) ValidateCode(secret, code string) bool {
	return totp.Validate(code, secret)
}

// GenerateBackupCodes creates one-time use backup codes
func (m *MFAService) GenerateBackupCodes() ([]string, error) {
	codes := make([]string, 10)

	for i := range codes {
		b := make([]byte, 5)
		if _, err := rand.Read(b); err != nil {
			return nil, fmt.Errorf("generate random bytes: %w", err)
		}

		// Convert to base32 and take first 8 chars
		code := base32.StdEncoding.EncodeToString(b)[:8]
		codes[i] = strings.ToUpper(code)
	}

	return codes, nil
}
