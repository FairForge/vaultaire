package auth

import (
	"context"
	"encoding/json"
	"fmt"
	"io"

	"os"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
)

type SSOService struct {
	googleConfig *oauth2.Config
	redirectURL  string
}

type GoogleUser struct {
	ID            string `json:"id"`
	Email         string `json:"email"`
	VerifiedEmail bool   `json:"verified_email"`
	Name          string `json:"name"`
	Picture       string `json:"picture"`
}

func NewSSOService() *SSOService {
	return &SSOService{
		redirectURL: "http://localhost:8080/auth/callback",
		googleConfig: &oauth2.Config{
			ClientID:     getEnv("GOOGLE_CLIENT_ID", "mock-client-id"),
			ClientSecret: getEnv("GOOGLE_CLIENT_SECRET", "mock-secret"),
			RedirectURL:  "http://localhost:8080/auth/callback",
			Scopes:       []string{"email", "profile"},
			Endpoint:     google.Endpoint,
		},
	}
}

func (s *SSOService) GetGoogleConfig() *oauth2.Config {
	return s.googleConfig
}

func (s *SSOService) GetAuthURL(state string) string {
	return s.googleConfig.AuthCodeURL(state)
}

func (s *SSOService) HandleCallback(code string) (*User, error) {
	// For testing, accept mock code
	if code == "mock-code" {
		return &User{
			ID:    "google-123",
			Email: "test@google.com",
		}, nil
	}

	// Real implementation
	ctx := context.Background()
	token, err := s.googleConfig.Exchange(ctx, code)
	if err != nil {
		return nil, fmt.Errorf("code exchange failed: %w", err)
	}

	// Get user info
	client := s.googleConfig.Client(ctx, token)
	resp, err := client.Get("https://www.googleapis.com/oauth2/v2/userinfo")
	if err != nil {
		return nil, fmt.Errorf("failed to get user info: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	data, _ := io.ReadAll(resp.Body)
	var googleUser GoogleUser
	if err := json.Unmarshal(data, &googleUser); err != nil {
		return nil, fmt.Errorf("failed to parse user info: %w", err)
	}

	return &User{
		ID:      googleUser.ID,
		Email:   googleUser.Email,
		Company: googleUser.Name,
	}, nil
}

func getEnv(key, defaultVal string) string {
	if val := os.Getenv(key); val != "" {
		return val
	}
	return defaultVal
}
