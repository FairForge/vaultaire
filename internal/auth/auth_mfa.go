package auth

import (
	"context"
	"fmt"
	"sync"

	"golang.org/x/crypto/bcrypt"
)

// MFA settings stored in memory (like your existing auth)
type MFASettings struct {
	UserID      string
	Secret      string
	Enabled     bool
	BackupCodes []string // Hashed backup codes
}

// Add to AuthService - extend the existing one
var (
	mfaSettings = make(map[string]*MFASettings) // userID -> settings
	mfaMutex    sync.RWMutex
)

// EnableMFA activates MFA for a user
func (a *AuthService) EnableMFA(ctx context.Context, userID string, secret string, backupCodes []string) error {
	// Verify user exists
	if _, exists := a.userIndex[userID]; !exists {
		return fmt.Errorf("user not found")
	}

	// Hash backup codes
	hashedCodes := make([]string, len(backupCodes))
	for i, code := range backupCodes {
		hashed, err := bcrypt.GenerateFromPassword([]byte(code), bcrypt.DefaultCost)
		if err != nil {
			return fmt.Errorf("hash backup code: %w", err)
		}
		hashedCodes[i] = string(hashed)
	}

	mfaMutex.Lock()
	defer mfaMutex.Unlock()

	mfaSettings[userID] = &MFASettings{
		UserID:      userID,
		Secret:      secret,
		Enabled:     true,
		BackupCodes: hashedCodes,
	}

	return nil
}

// DisableMFA deactivates MFA for a user
func (a *AuthService) DisableMFA(ctx context.Context, userID string) error {
	mfaMutex.Lock()
	defer mfaMutex.Unlock()

	if settings, exists := mfaSettings[userID]; exists {
		settings.Enabled = false
	}

	return nil
}

// IsMFAEnabled checks if user has MFA enabled
func (a *AuthService) IsMFAEnabled(ctx context.Context, userID string) (bool, error) {
	mfaMutex.RLock()
	defer mfaMutex.RUnlock()

	if settings, exists := mfaSettings[userID]; exists {
		return settings.Enabled, nil
	}

	return false, nil
}

// GetMFASecret retrieves the secret for a user
func (a *AuthService) GetMFASecret(ctx context.Context, userID string) (string, error) {
	mfaMutex.RLock()
	defer mfaMutex.RUnlock()

	if settings, exists := mfaSettings[userID]; exists && settings.Enabled {
		return settings.Secret, nil
	}

	return "", fmt.Errorf("MFA not enabled for user")
}

// ValidateBackupCode checks a backup code for a user
func (a *AuthService) ValidateBackupCode(ctx context.Context, userID string, code string) (bool, error) {
	mfaMutex.Lock()
	defer mfaMutex.Unlock()

	settings, exists := mfaSettings[userID]
	if !exists || !settings.Enabled {
		return false, nil
	}

	// Check each hashed backup code
	for i, hashedCode := range settings.BackupCodes {
		err := bcrypt.CompareHashAndPassword([]byte(hashedCode), []byte(code))
		if err == nil {
			// Code matches - remove it (single use)
			settings.BackupCodes = append(settings.BackupCodes[:i], settings.BackupCodes[i+1:]...)
			return true, nil
		}
	}

	return false, nil
}
