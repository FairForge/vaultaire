package auth

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/FairForge/vaultaire/internal/audit"

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

	// The database first, then this process's copy: a write the database
	// refused must not leave the account "enabled" in memory only — the
	// sign-in flow would ask for a code until the next restart, and then
	// stop (LoadMFAFromDB finds no row). WP-R12-8.
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

	a.mfaMu.Lock()
	a.mfaSettings[userID] = &MFASettings{
		UserID:      userID,
		Secret:      secret,
		Enabled:     true,
		BackupCodes: hashedCodes,
	}
	a.mfaMu.Unlock()

	a.record(ctx, audit.Entry{UserID: userID, Action: "mfa.enabled", Resource: "user:" + userID,
		Metadata: map[string]any{"backup_codes": len(backupCodes)}})
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

	a.record(ctx, audit.Entry{UserID: userID, Action: "mfa.disabled", Resource: "user:" + userID})
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

// totpReplayWindow: a TOTP code is accepted once. RFC 6238 §5.2 — "the
// verifier MUST NOT accept the second attempt of the OTP after the
// successful validation has been issued for the first OTP". The dashboard
// used to accept the same six digits again for the rest of the 30 s step
// (R5-15c, proven live): a shoulder-surfed or intercepted code was reusable.
const totpReplayWindow = 90 * time.Second

// ConsumeTOTPCode records that code was accepted for userID and reports
// whether it was fresh. Every code accepted inside totpReplayWindow is
// remembered — not just the last one: totp.Validate allows the adjacent 30 s
// steps (skew 1), so two or three codes are valid at any moment and a guard
// that kept only the latest let an earlier code be replayed once another had
// been used (post-merge R12-38). A repeat returns false; the caller treats
// that as a failed factor.
func (a *AuthService) ConsumeTOTPCode(userID, code string) bool {
	a.mfaMu.Lock()
	defer a.mfaMu.Unlock()
	if a.totpUsed == nil {
		a.totpUsed = make(map[string]map[string]time.Time)
	}
	now := time.Now()
	used := a.totpUsed[userID]
	if at, ok := used[code]; ok && now.Sub(at) < totpReplayWindow {
		return false
	}
	// Bound the maps: entries older than the window are dead.
	if len(a.totpUsed) > 4096 || len(used) > 8 {
		for uid, codes := range a.totpUsed {
			for c, at := range codes {
				if now.Sub(at) >= totpReplayWindow {
					delete(codes, c)
				}
			}
			if len(codes) == 0 {
				delete(a.totpUsed, uid)
			}
		}
		used = a.totpUsed[userID]
	}
	if used == nil {
		used = make(map[string]time.Time)
		a.totpUsed[userID] = used
	}
	used[code] = now
	return true
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
