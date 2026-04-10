package handlers

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/FairForge/vaultaire/internal/auth"
	dashauth "github.com/FairForge/vaultaire/internal/dashboard/auth"
	"github.com/FairForge/vaultaire/internal/dashboard/middleware"
	"go.uber.org/zap"
	"golang.org/x/oauth2"
)

const (
	oauthStateCookie = "oauth_state"
	oauthStateTTL    = 10 * time.Minute
	sessionTTL       = 24 * time.Hour
)

// HandleOAuthLogin redirects the user to the provider's consent screen.
func HandleOAuthLogin(cfg *oauth2.Config, logger *zap.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		state, err := generateState()
		if err != nil {
			logger.Error("generate oauth state", zap.Error(err))
			http.Error(w, "Internal Server Error", http.StatusInternalServerError)
			return
		}

		http.SetCookie(w, &http.Cookie{
			Name:     oauthStateCookie,
			Value:    state,
			Path:     "/",
			HttpOnly: true,
			Secure:   true,
			SameSite: http.SameSiteLaxMode,
			MaxAge:   int(oauthStateTTL.Seconds()),
		})

		url := cfg.AuthCodeURL(state)
		http.Redirect(w, r, url, http.StatusTemporaryRedirect)
	}
}

// HandleOAuthCallback processes the OAuth callback from a provider.
// It exchanges the code for a token, fetches user info, and creates or links
// the user account.
func HandleOAuthCallback(
	cfg *oauth2.Config,
	provider string,
	fetchUser func(ctx context.Context, token *oauth2.Token) (oauthUser, error),
	authSvc *auth.AuthService,
	sessions dashauth.SessionStore,
	db *sql.DB,
	logger *zap.Logger,
) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Validate state.
		cookie, err := r.Cookie(oauthStateCookie)
		if err != nil || cookie.Value == "" {
			logger.Debug("oauth: missing state cookie")
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		if r.URL.Query().Get("state") != cookie.Value {
			logger.Debug("oauth: state mismatch")
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}

		// Clear state cookie.
		http.SetCookie(w, &http.Cookie{
			Name:     oauthStateCookie,
			Value:    "",
			Path:     "/",
			HttpOnly: true,
			Secure:   true,
			SameSite: http.SameSiteLaxMode,
			MaxAge:   -1,
		})

		// Check for error from provider.
		if errParam := r.URL.Query().Get("error"); errParam != "" {
			logger.Info("oauth: provider error", zap.String("error", errParam))
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}

		// Exchange code for token.
		code := r.URL.Query().Get("code")
		if code == "" {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}

		token, err := cfg.Exchange(r.Context(), code)
		if err != nil {
			logger.Error("oauth: exchange code", zap.String("provider", provider), zap.Error(err))
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}

		// Fetch user info from provider.
		ou, err := fetchUser(r.Context(), token)
		if err != nil {
			logger.Error("oauth: fetch user info", zap.String("provider", provider), zap.Error(err))
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}

		if ou.Email == "" {
			logger.Error("oauth: no email from provider", zap.String("provider", provider))
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}

		// Find or create user.
		user, err := findOrCreateOAuthUser(r.Context(), authSvc, db, provider, ou)
		if err != nil {
			logger.Error("oauth: find or create user", zap.String("provider", provider), zap.Error(err))
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}

		// Determine role.
		role := "user"
		if db != nil {
			_ = db.QueryRowContext(r.Context(),
				`SELECT role FROM users WHERE id = $1`, user.ID).Scan(&role)
		}

		// Create session.
		sessionToken, err := sessions.Create(r.Context(), dashauth.SessionData{
			UserID:    user.ID,
			TenantID:  user.TenantID,
			Email:     user.Email,
			Role:      role,
			IPAddress: middleware.ClientIP(r),
			UserAgent: dashauth.TruncateUserAgent(r.UserAgent()),
		}, sessionTTL)
		if err != nil {
			logger.Error("oauth: create session", zap.Error(err))
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}

		dashauth.SetSessionCookie(w, sessionToken, sessionTTL)
		http.Redirect(w, r, "/dashboard", http.StatusSeeOther)
	}
}

// oauthUser holds the normalized user info from an OAuth provider.
type oauthUser struct {
	ID    string
	Email string
	Name  string
}

// findOrCreateOAuthUser resolves an OAuth login to a User.
// 1. Existing OAuth link → return user
// 2. Existing email match → link OAuth + return user
// 3. No match → create new user + link OAuth
func findOrCreateOAuthUser(ctx context.Context, authSvc *auth.AuthService, db *sql.DB, provider string, ou oauthUser) (*auth.User, error) {
	// Check for existing OAuth link.
	user, err := authSvc.GetUserByOAuth(ctx, provider, ou.ID)
	if err == nil && user != nil {
		return user, nil
	}

	// Check for existing user with same email.
	user, err = authSvc.GetUserByEmail(ctx, ou.Email)
	if err == nil && user != nil {
		// Link this OAuth account to the existing user.
		if linkErr := authSvc.LinkOAuthAccount(ctx, user.ID, provider, ou.ID, ou.Email, ou.Name); linkErr != nil {
			return nil, fmt.Errorf("link oauth: %w", linkErr)
		}
		return user, nil
	}

	// Create new user via OAuth.
	user, _, err = authSvc.CreateUserFromOAuth(ctx, ou.Email, ou.Name, provider, ou.ID)
	if err != nil {
		return nil, fmt.Errorf("create oauth user: %w", err)
	}

	return user, nil
}

// --- Google ---

// FetchGoogleUser exchanges an OAuth token for Google user info.
func FetchGoogleUser(cfg *oauth2.Config) func(ctx context.Context, token *oauth2.Token) (oauthUser, error) {
	return func(ctx context.Context, token *oauth2.Token) (oauthUser, error) {
		client := cfg.Client(ctx, token)
		resp, err := client.Get("https://www.googleapis.com/oauth2/v2/userinfo")
		if err != nil {
			return oauthUser{}, fmt.Errorf("google userinfo request: %w", err)
		}
		defer func() { _ = resp.Body.Close() }()

		body, err := io.ReadAll(resp.Body)
		if err != nil {
			return oauthUser{}, fmt.Errorf("read google response: %w", err)
		}

		var info struct {
			ID    string `json:"id"`
			Email string `json:"email"`
			Name  string `json:"name"`
		}
		if err := json.Unmarshal(body, &info); err != nil {
			return oauthUser{}, fmt.Errorf("parse google userinfo: %w", err)
		}

		return oauthUser{ID: info.ID, Email: info.Email, Name: info.Name}, nil
	}
}

// --- GitHub ---

// FetchGithubUser exchanges an OAuth token for GitHub user info.
func FetchGithubUser(cfg *oauth2.Config) func(ctx context.Context, token *oauth2.Token) (oauthUser, error) {
	return func(ctx context.Context, token *oauth2.Token) (oauthUser, error) {
		client := cfg.Client(ctx, token)

		// Get user profile.
		resp, err := client.Get("https://api.github.com/user")
		if err != nil {
			return oauthUser{}, fmt.Errorf("github user request: %w", err)
		}
		defer func() { _ = resp.Body.Close() }()

		body, err := io.ReadAll(resp.Body)
		if err != nil {
			return oauthUser{}, fmt.Errorf("read github response: %w", err)
		}

		var info struct {
			ID    int    `json:"id"`
			Login string `json:"login"`
			Name  string `json:"name"`
			Email string `json:"email"`
		}
		if err := json.Unmarshal(body, &info); err != nil {
			return oauthUser{}, fmt.Errorf("parse github user: %w", err)
		}

		email := info.Email

		// If email is private, fetch from /user/emails endpoint.
		if email == "" {
			email, err = fetchGithubPrimaryEmail(ctx, client)
			if err != nil {
				return oauthUser{}, err
			}
		}

		name := info.Name
		if name == "" {
			name = info.Login
		}

		return oauthUser{
			ID:    fmt.Sprintf("%d", info.ID),
			Email: email,
			Name:  name,
		}, nil
	}
}

func fetchGithubPrimaryEmail(ctx context.Context, client *http.Client) (string, error) {
	resp, err := client.Get("https://api.github.com/user/emails")
	if err != nil {
		return "", fmt.Errorf("github emails request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("read github emails: %w", err)
	}

	var emails []struct {
		Email    string `json:"email"`
		Primary  bool   `json:"primary"`
		Verified bool   `json:"verified"`
	}
	if err := json.Unmarshal(body, &emails); err != nil {
		return "", fmt.Errorf("parse github emails: %w", err)
	}

	for _, e := range emails {
		if e.Primary && e.Verified {
			return e.Email, nil
		}
	}

	return "", fmt.Errorf("no verified primary email from github")
}

func generateState() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
