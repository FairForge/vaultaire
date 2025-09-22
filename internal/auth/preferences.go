package auth

import (
	"context"
	"fmt"
)

// UserPreferences contains user preferences
type UserPreferences struct {
	Theme              string `json:"theme"` // dark, light, auto
	EmailNotifications bool   `json:"email_notifications"`
	FileView           string `json:"file_view"` // list, grid
	ShowHiddenFiles    bool   `json:"show_hidden_files"`
	AutoBackup         bool   `json:"auto_backup"`
	CompressUploads    bool   `json:"compress_uploads"`
	EncryptByDefault   bool   `json:"encrypt_by_default"`
	DefaultRegion      string `json:"default_region"`
	UploadChunkSize    int    `json:"upload_chunk_size"` // MB
}

// GetUserPreferences retrieves user preferences
func (a *AuthService) GetUserPreferences(ctx context.Context, userID string) (*UserPreferences, error) {
	if _, exists := a.userIndex[userID]; !exists {
		return nil, fmt.Errorf("user not found")
	}

	// Check if user has saved preferences
	if a.preferences == nil {
		a.preferences = make(map[string]*UserPreferences)
	}

	if prefs, exists := a.preferences[userID]; exists {
		return prefs, nil
	}

	// Return defaults
	return &UserPreferences{
		Theme:              "dark",
		EmailNotifications: true,
		FileView:           "list",
		ShowHiddenFiles:    false,
		AutoBackup:         false,
		CompressUploads:    false,
		EncryptByDefault:   false,
		DefaultRegion:      "us-east",
		UploadChunkSize:    5, // 5MB default
	}, nil
}

// SetUserPreferences saves user preferences
func (a *AuthService) SetUserPreferences(ctx context.Context, userID string, prefs UserPreferences) error {
	if _, exists := a.userIndex[userID]; !exists {
		return fmt.Errorf("user not found")
	}

	if a.preferences == nil {
		a.preferences = make(map[string]*UserPreferences)
	}

	a.preferences[userID] = &prefs

	return nil
}
