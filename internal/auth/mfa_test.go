package auth

import (
	"time"

	"github.com/pquerna/otp/totp"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMFA_GenerateSecret(t *testing.T) {
	t.Run("generates valid TOTP secret", func(t *testing.T) {
		mfa := NewMFAService("stored.ge")

		secret, qrCode, err := mfa.GenerateSecret("user@example.com")
		require.NoError(t, err)

		assert.NotEmpty(t, secret)
		assert.NotEmpty(t, qrCode)
		assert.Contains(t, qrCode, "otpauth://totp")
	})
}

func TestMFA_ValidateCode(t *testing.T) {
	t.Run("validates correct code", func(t *testing.T) {
		mfa := NewMFAService("stored.ge")

		secret, _, err := mfa.GenerateSecret("user@stored.ge")
		require.NoError(t, err)
		code, err := totp.GenerateCode(secret, time.Now())
		require.NoError(t, err)

		valid := mfa.ValidateCode(secret, code)
		assert.True(t, valid)
	})

	t.Run("no hard-coded test backdoor (R5-15)", func(t *testing.T) {
		mfa := NewMFAService("stored.ge")
		// The former "test secret" + "123456" pair must behave like any other
		// secret/code pair: only a real TOTP for the current step validates.
		secret := "JBSWY3DPEHPK3PXP"
		assert.Equal(t, totp.Validate("123456", secret), mfa.ValidateCode(secret, "123456"))
		real, err := totp.GenerateCode(secret, time.Now())
		require.NoError(t, err)
		assert.True(t, mfa.ValidateCode(secret, real))
	})

	t.Run("rejects incorrect code", func(t *testing.T) {
		mfa := NewMFAService("stored.ge")

		secret := "JBSWY3DPEHPK3PXP"
		valid := mfa.ValidateCode(secret, "000000")

		assert.False(t, valid)
	})
}

func TestMFA_BackupCodes(t *testing.T) {
	t.Run("generates backup codes", func(t *testing.T) {
		mfa := NewMFAService("stored.ge")

		codes, err := mfa.GenerateBackupCodes()
		require.NoError(t, err)

		assert.Len(t, codes, 10)
		for _, code := range codes {
			assert.Len(t, code, 8)
		}
	})

	t.Run("validates backup code once", func(t *testing.T) {
		mfa := NewMFAService("stored.ge")

		codes, err := mfa.GenerateBackupCodes()
		require.NoError(t, err)

		// Store codes for user first
		mfa.backupCodes["user-001"] = make(map[string]bool)
		for _, code := range codes {
			mfa.backupCodes["user-001"][code] = false
		}

		// First use should succeed
		valid := mfa.ValidateBackupCode("user-001", codes[0])
		assert.True(t, valid)

		// Second use should fail
		valid = mfa.ValidateBackupCode("user-001", codes[0])
		assert.False(t, valid)
	})
}
