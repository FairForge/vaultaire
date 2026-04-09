package auth

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	"golang.org/x/crypto/bcrypt"
)

// MFASettings represents a user's MFA configuration.
type MFASettings struct {
	UserID      string
	Secret      string
	Enabled     bool
	BackupCodes []string // Hashed backup codes
}

// EnableMFA activates MFA for a user and persists to the database.
func (a *AuthService) EnableMFA(ctx context.Context, userID, secret string, backupCodes []string) error {
	if _, exists := a.userIndex[userID]; !exists {
		return fmt.Errorf("user not found")
	}

	// Hash backup codes before storing.
	hashedCodes := make([]string, len(backupCodes))
	for i, code := range backupCodes {
		hashed, err := bcrypt.GenerateFromPassword([]byte(code), bcrypt.DefaultCost)
		if err != nil {
			return fmt.Errorf("hash backup code: %w", err)
		}
		hashedCodes[i] = string(hashed)
	}

	a.mfaMu.Lock()
	a.mfaSettings[userID] = &MFASettings{
		UserID:      userID,
		Secret:      secret,
		Enabled:     true,
		BackupCodes: hashedCodes,
	}
	a.mfaMu.Unlock()

	if a.sqlDB != nil {
		codesJSON, err := json.Marshal(hashedCodes)
		if err != nil {
			return fmt.Errorf("marshal backup codes: %w", err)
		}
		_, err = a.sqlDB.ExecContext(ctx, `
			INSERT INTO user_mfa (user_id, secret, enabled, backup_codes, created_at, updated_at)
			VALUES ($1, $2, TRUE, $3, NOW(), NOW())
			ON CONFLICT (user_id) DO UPDATE SET
				secret = EXCLUDED.secret,
				enabled = TRUE,
				backup_codes = EXCLUDED.backup_codes,
				updated_at = NOW()
		`, userID, secret, string(codesJSON))
		if err != nil {
			return fmt.Errorf("persist mfa settings: %w", err)
		}
	}

	return nil
}

// DisableMFA deactivates MFA for a user.
func (a *AuthService) DisableMFA(ctx context.Context, userID string) error {
	a.mfaMu.Lock()
	delete(a.mfaSettings, userID)
	a.mfaMu.Unlock()

	if a.sqlDB != nil {
		_, err := a.sqlDB.ExecContext(ctx, `
			DELETE FROM user_mfa WHERE user_id = $1
		`, userID)
		if err != nil {
			return fmt.Errorf("disable mfa in db: %w", err)
		}
	}

	return nil
}

// IsMFAEnabled checks if a user has MFA enabled.
func (a *AuthService) IsMFAEnabled(_ context.Context, userID string) (bool, error) {
	a.mfaMu.RLock()
	defer a.mfaMu.RUnlock()

	if settings, exists := a.mfaSettings[userID]; exists {
		return settings.Enabled, nil
	}
	return false, nil
}

// GetMFASecret retrieves the TOTP secret for a user with MFA enabled.
func (a *AuthService) GetMFASecret(_ context.Context, userID string) (string, error) {
	a.mfaMu.RLock()
	defer a.mfaMu.RUnlock()

	if settings, exists := a.mfaSettings[userID]; exists && settings.Enabled {
		return settings.Secret, nil
	}
	return "", fmt.Errorf("MFA not enabled for user")
}

// ValidateBackupCode checks and consumes a single-use backup code.
func (a *AuthService) ValidateBackupCode(ctx context.Context, userID, code string) (bool, error) {
	a.mfaMu.Lock()
	defer a.mfaMu.Unlock()

	settings, exists := a.mfaSettings[userID]
	if !exists || !settings.Enabled {
		return false, nil
	}

	for i, hashedCode := range settings.BackupCodes {
		if bcrypt.CompareHashAndPassword([]byte(hashedCode), []byte(code)) == nil {
			// Code matches — remove it (single use).
			settings.BackupCodes = append(settings.BackupCodes[:i], settings.BackupCodes[i+1:]...)

			// Persist updated codes.
			if a.sqlDB != nil {
				codesJSON, _ := json.Marshal(settings.BackupCodes)
				_, _ = a.sqlDB.ExecContext(ctx, `
					UPDATE user_mfa SET backup_codes = $1, updated_at = NOW()
					WHERE user_id = $2
				`, string(codesJSON), userID)
			}

			return true, nil
		}
	}

	return false, nil
}

// LoadMFAFromDB loads MFA settings from the user_mfa table into memory.
// Called during startup alongside LoadFromDB.
func (a *AuthService) LoadMFAFromDB(ctx context.Context) error {
	if a.sqlDB == nil {
		return nil
	}

	rows, err := a.sqlDB.QueryContext(ctx, `
		SELECT user_id, secret, enabled, backup_codes
		FROM user_mfa
		WHERE enabled = TRUE
	`)
	if err != nil {
		return fmt.Errorf("load mfa settings: %w", err)
	}
	defer func() { _ = rows.Close() }()

	a.mfaMu.Lock()
	defer a.mfaMu.Unlock()

	for rows.Next() {
		var (
			s         MFASettings
			codesJSON sql.NullString
		)
		if err := rows.Scan(&s.UserID, &s.Secret, &s.Enabled, &codesJSON); err != nil {
			return fmt.Errorf("scan mfa settings: %w", err)
		}
		if codesJSON.Valid && codesJSON.String != "" {
			_ = json.Unmarshal([]byte(codesJSON.String), &s.BackupCodes)
		}
		a.mfaSettings[s.UserID] = &s
	}

	return rows.Err()
}
