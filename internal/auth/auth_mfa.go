package auth

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
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

// ErrMFANotEnabled: the account has no second factor to act on.
var ErrMFANotEnabled = errors.New("two-factor authentication is not enabled")

// hashBackupCodes bcrypts the plaintext codes; only the hashes are stored.
func hashBackupCodes(codes []string) ([]string, error) {
	hashed := make([]string, len(codes))
	for i, code := range codes {
		h, err := bcrypt.GenerateFromPassword([]byte(code), bcrypt.DefaultCost)
		if err != nil {
			return nil, fmt.Errorf("hash backup code: %w", err)
		}
		hashed[i] = string(h)
	}
	return hashed, nil
}

// EnableMFA activates MFA for a user and persists to the database.
func (a *AuthService) EnableMFA(ctx context.Context, userID, secret string, backupCodes []string) error {
	a.cacheMu.RLock()
	_, exists := a.userIndex[userID]
	a.cacheMu.RUnlock()
	if !exists {
		return fmt.Errorf("user not found")
	}

	hashedCodes, err := hashBackupCodes(backupCodes)
	if err != nil {
		return err
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

// RegenerateBackupCodes replaces the account's backup codes with codes —
// every old code stops working, in the database first and then in this
// process (WP-R12-15). The caller has already confirmed the user's identity
// (the password, or a fresh sign-in for an OAuth-only account) and shows the
// new codes once. ErrMFANotEnabled when there is no second factor to attach
// them to. The audit row is mfa.backup_codes_regenerated.
func (a *AuthService) RegenerateBackupCodes(ctx context.Context, userID string, codes []string) error {
	a.mfaMu.RLock()
	settings, exists := a.mfaSettings[userID]
	enabled := exists && settings.Enabled
	a.mfaMu.RUnlock()
	if !enabled {
		return ErrMFANotEnabled
	}

	hashedCodes, err := hashBackupCodes(codes)
	if err != nil {
		return err
	}
	if a.sqlDB != nil {
		codesJSON, err := json.Marshal(hashedCodes)
		if err != nil {
			return fmt.Errorf("marshal backup codes: %w", err)
		}
		res, err := a.sqlDB.ExecContext(ctx, `
			UPDATE user_mfa SET backup_codes = $1, updated_at = NOW()
			WHERE user_id = $2 AND enabled = TRUE
		`, string(codesJSON), userID)
		if err != nil {
			return fmt.Errorf("persist regenerated backup codes: %w", err)
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrMFANotEnabled
		}
	}

	a.mfaMu.Lock()
	if settings, ok := a.mfaSettings[userID]; ok {
		settings.BackupCodes = hashedCodes
	}
	a.mfaMu.Unlock()

	a.record(ctx, audit.Entry{UserID: userID, Action: "mfa.backup_codes_regenerated", Resource: "user:" + userID,
		Metadata: map[string]any{"backup_codes": len(codes)}})
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
