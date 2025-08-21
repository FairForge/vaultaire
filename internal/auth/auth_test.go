package auth

import (
	"context"
	"testing"
)

func TestAuth_CreateUser(t *testing.T) {
	auth := NewAuthService(nil) // Will need database

	user, err := auth.CreateUser(context.Background(), "test@example.com", "password123")
	if err != nil {
		t.Fatalf("Failed to create user: %v", err)
	}

	if user.Email != "test@example.com" {
		t.Errorf("Expected email test@example.com, got %s", user.Email)
	}

	if user.ID == "" {
		t.Error("Expected user ID to be set")
	}
}

func TestAuth_ValidatePassword(t *testing.T) {
	auth := NewAuthService(nil)

	// Create user
	user, _ := auth.CreateUser(context.Background(), "test@example.com", "password123")

	// Test correct password
	valid, err := auth.ValidatePassword(context.Background(), user.Email, "password123")
	if err != nil {
		t.Fatalf("Failed to validate password: %v", err)
	}
	if !valid {
		t.Error("Expected password to be valid")
	}

	// Test incorrect password
	valid, err = auth.ValidatePassword(context.Background(), user.Email, "wrongpassword")
	if err != nil {
		t.Fatalf("Failed to validate password: %v", err)
	}
	if valid {
		t.Error("Expected password to be invalid")
	}
}

func TestAuth_GenerateAPIKey(t *testing.T) {
	auth := NewAuthService(nil)

	// Create user
	user, _ := auth.CreateUser(context.Background(), "test@example.com", "password123")

	// Generate API key
	apiKey, err := auth.GenerateAPIKey(context.Background(), user.ID, "Test Key")
	if err != nil {
		t.Fatalf("Failed to generate API key: %v", err)
	}

	if apiKey.Key == "" {
		t.Error("Expected API key to be set")
	}

	if apiKey.Secret == "" {
		t.Error("Expected API secret to be set")
	}

	if len(apiKey.Key) < 20 {
		t.Error("API key too short")
	}
}

func TestAuth_ValidateAPIKey(t *testing.T) {
	auth := NewAuthService(nil)

	// Create user and API key
	user, _ := auth.CreateUser(context.Background(), "test@example.com", "password123")
	apiKey, _ := auth.GenerateAPIKey(context.Background(), user.ID, "Test Key")

	// Validate correct key
	validUser, err := auth.ValidateAPIKey(context.Background(), apiKey.Key, apiKey.Secret)
	if err != nil {
		t.Fatalf("Failed to validate API key: %v", err)
	}

	if validUser.ID != user.ID {
		t.Errorf("Expected user ID %s, got %s", user.ID, validUser.ID)
	}

	// Validate incorrect key
	_, err = auth.ValidateAPIKey(context.Background(), "wrong-key", "wrong-secret")
	if err == nil {
		t.Error("Expected error for invalid API key")
	}
}

func TestAuth_GenerateJWT(t *testing.T) {
	auth := NewAuthService(nil)

	// Create user
	user, _ := auth.CreateUser(context.Background(), "test@example.com", "password123")

	// Generate JWT
	token, err := auth.GenerateJWT(user)
	if err != nil {
		t.Fatalf("Failed to generate JWT: %v", err)
	}

	if token == "" {
		t.Error("Expected JWT token to be set")
	}

	// Validate JWT
	claims, err := auth.ValidateJWT(token)
	if err != nil {
		t.Fatalf("Failed to validate JWT: %v", err)
	}

	if claims.UserID != user.ID {
		t.Errorf("Expected user ID %s, got %s", user.ID, claims.UserID)
	}
}
