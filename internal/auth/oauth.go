// internal/auth/oauth.go
package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// OAuthConfig configures an OAuth 2.0 provider
type OAuthConfig struct {
	ProviderName string        `json:"provider_name"`
	ClientID     string        `json:"client_id"`
	ClientSecret string        `json:"client_secret"`
	RedirectURL  string        `json:"redirect_url"`
	AuthURL      string        `json:"auth_url"`
	TokenURL     string        `json:"token_url"`
	UserInfoURL  string        `json:"userinfo_url"`
	Scopes       []string      `json:"scopes"`
	UsePKCE      bool          `json:"use_pkce"`
	Timeout      time.Duration `json:"timeout"`
}

// DefaultOAuthConfig returns sensible defaults
func DefaultOAuthConfig() *OAuthConfig {
	return &OAuthConfig{
		Timeout: 30 * time.Second,
		UsePKCE: false,
	}
}

// Validate checks if the configuration is valid
func (c *OAuthConfig) Validate() error {
	if c.ClientID == "" {
		return errors.New("oauth: client ID is required")
	}
	if c.ClientSecret == "" && !c.UsePKCE {
		return errors.New("oauth: client secret is required")
	}
	if c.RedirectURL == "" {
		return errors.New("oauth: redirect URL is required")
	}
	return nil
}

// ApplyDefaults fills in default values
func (c *OAuthConfig) ApplyDefaults() {
	defaults := DefaultOAuthConfig()
	if c.Timeout == 0 {
		c.Timeout = defaults.Timeout
	}
}

// OAuthToken represents an OAuth access token
type OAuthToken struct {
	AccessToken  string    `json:"access_token"`
	TokenType    string    `json:"token_type"`
	RefreshToken string    `json:"refresh_token,omitempty"`
	ExpiresIn    int       `json:"expires_in,omitempty"`
	ExpiresAt    time.Time `json:"expires_at,omitempty"`
	Scope        string    `json:"scope,omitempty"`
}

// IsExpired checks if the token has expired
func (t *OAuthToken) IsExpired() bool {
	if t.ExpiresAt.IsZero() {
		return false
	}
	return time.Now().After(t.ExpiresAt)
}

// HasRefreshToken checks if a refresh token is available
func (t *OAuthToken) HasRefreshToken() bool {
	return t.RefreshToken != ""
}

// OAuthUser represents user info from OAuth provider
type OAuthUser struct {
	ID            string `json:"sub"`
	Email         string `json:"email"`
	EmailVerified bool   `json:"email_verified"`
	Name          string `json:"name"`
	GivenName     string `json:"given_name"`
	FamilyName    string `json:"family_name"`
	Picture       string `json:"picture"`
	Locale        string `json:"locale"`
}

// OAuthProviderInfo contains provider information
type OAuthProviderInfo struct {
	Type string
	Name string
}

// OAuthProvider handles OAuth 2.0 authentication
type OAuthProvider struct {
	config     *OAuthConfig
	httpClient *http.Client
}

// NewOAuthProvider creates a new OAuth provider
func NewOAuthProvider(config *OAuthConfig) (*OAuthProvider, error) {
	if config == nil {
		return nil, errors.New("oauth: config is required")
	}

	config.ApplyDefaults()

	if err := config.Validate(); err != nil {
		return nil, err
	}

	return &OAuthProvider{
		config: config,
		httpClient: &http.Client{
			Timeout: config.Timeout,
		},
	}, nil
}

// BuildAuthURL builds the authorization URL
func (p *OAuthProvider) BuildAuthURL(state string) string {
	if state == "" {
		state = p.GenerateState()
	}

	params := url.Values{}
	params.Set("client_id", p.config.ClientID)
	params.Set("redirect_uri", p.config.RedirectURL)
	params.Set("response_type", "code")
	params.Set("state", state)

	if len(p.config.Scopes) > 0 {
		params.Set("scope", strings.Join(p.config.Scopes, " "))
	}

	return fmt.Sprintf("%s?%s", p.config.AuthURL, params.Encode())
}

// BuildAuthURLWithPKCE builds auth URL with PKCE challenge
func (p *OAuthProvider) BuildAuthURLWithPKCE(state, codeVerifier string) string {
	if state == "" {
		state = p.GenerateState()
	}

	params := url.Values{}
	params.Set("client_id", p.config.ClientID)
	params.Set("redirect_uri", p.config.RedirectURL)
	params.Set("response_type", "code")
	params.Set("state", state)
	params.Set("code_challenge", p.GenerateCodeChallenge(codeVerifier))
	params.Set("code_challenge_method", "S256")

	if len(p.config.Scopes) > 0 {
		params.Set("scope", strings.Join(p.config.Scopes, " "))
	}

	return fmt.Sprintf("%s?%s", p.config.AuthURL, params.Encode())
}

// GenerateState generates a random state parameter
func (p *OAuthProvider) GenerateState() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

// GenerateCodeVerifier generates a PKCE code verifier
func (p *OAuthProvider) GenerateCodeVerifier() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

// GenerateCodeChallenge generates a PKCE code challenge from verifier
func (p *OAuthProvider) GenerateCodeChallenge(verifier string) string {
	h := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(h[:])
}

// ExchangeCode exchanges an authorization code for tokens
func (p *OAuthProvider) ExchangeCode(ctx context.Context, code string) (*OAuthToken, error) {
	if code == "" {
		return nil, errors.New("oauth: authorization code is required")
	}

	data := url.Values{}
	data.Set("grant_type", "authorization_code")
	data.Set("code", code)
	data.Set("redirect_uri", p.config.RedirectURL)
	data.Set("client_id", p.config.ClientID)
	data.Set("client_secret", p.config.ClientSecret)

	return p.requestToken(ctx, data)
}

// ExchangeCodeWithPKCE exchanges code with PKCE verifier
func (p *OAuthProvider) ExchangeCodeWithPKCE(ctx context.Context, code, codeVerifier string) (*OAuthToken, error) {
	if code == "" {
		return nil, errors.New("oauth: authorization code is required")
	}

	data := url.Values{}
	data.Set("grant_type", "authorization_code")
	data.Set("code", code)
	data.Set("redirect_uri", p.config.RedirectURL)
	data.Set("client_id", p.config.ClientID)
	data.Set("code_verifier", codeVerifier)

	return p.requestToken(ctx, data)
}

// RefreshToken refreshes an access token
func (p *OAuthProvider) RefreshToken(ctx context.Context, refreshToken string) (*OAuthToken, error) {
	if refreshToken == "" {
		return nil, errors.New("oauth: refresh token is required")
	}

	data := url.Values{}
	data.Set("grant_type", "refresh_token")
	data.Set("refresh_token", refreshToken)
	data.Set("client_id", p.config.ClientID)
	data.Set("client_secret", p.config.ClientSecret)

	return p.requestToken(ctx, data)
}

// requestToken makes a token request
func (p *OAuthProvider) requestToken(ctx context.Context, data url.Values) (*OAuthToken, error) {
	req, err := http.NewRequestWithContext(ctx, "POST", p.config.TokenURL, strings.NewReader(data.Encode()))
	if err != nil {
		return nil, fmt.Errorf("oauth: failed to create request: %w", err)
	}

	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, err := p.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("oauth: token request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("oauth: failed to read response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("oauth: token request failed with status %d: %s", resp.StatusCode, string(body))
	}

	var token OAuthToken
	if err := json.Unmarshal(body, &token); err != nil {
		return nil, fmt.Errorf("oauth: failed to parse token response: %w", err)
	}

	// Calculate expiration time
	if token.ExpiresIn > 0 {
		token.ExpiresAt = time.Now().Add(time.Duration(token.ExpiresIn) * time.Second)
	}

	return &token, nil
}

// GetUserInfo fetches user information from the provider
func (p *OAuthProvider) GetUserInfo(ctx context.Context, accessToken string) (*OAuthUser, error) {
	if p.config.UserInfoURL == "" {
		return nil, errors.New("oauth: userinfo URL not configured")
	}

	req, err := http.NewRequestWithContext(ctx, "GET", p.config.UserInfoURL, nil)
	if err != nil {
		return nil, fmt.Errorf("oauth: failed to create request: %w", err)
	}

	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Accept", "application/json")

	resp, err := p.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("oauth: userinfo request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("oauth: failed to read response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("oauth: userinfo request failed with status %d", resp.StatusCode)
	}

	var user OAuthUser
	if err := json.Unmarshal(body, &user); err != nil {
		return nil, fmt.Errorf("oauth: failed to parse userinfo response: %w", err)
	}

	return &user, nil
}

// Info returns provider information
func (p *OAuthProvider) Info() OAuthProviderInfo {
	return OAuthProviderInfo{
		Type: "oauth",
		Name: p.config.ProviderName,
	}
}

// Predefined provider configurations

// GoogleOAuthConfig returns OAuth config for Google
func GoogleOAuthConfig(clientID, clientSecret, redirectURL string) *OAuthConfig {
	return &OAuthConfig{
		ProviderName: "google",
		ClientID:     clientID,
		ClientSecret: clientSecret,
		RedirectURL:  redirectURL,
		AuthURL:      "https://accounts.google.com/o/oauth2/v2/auth",
		TokenURL:     "https://oauth2.googleapis.com/token",
		UserInfoURL:  "https://openidconnect.googleapis.com/v1/userinfo",
		Scopes:       []string{"openid", "email", "profile"},
		Timeout:      30 * time.Second,
	}
}

// GitHubOAuthConfig returns OAuth config for GitHub
func GitHubOAuthConfig(clientID, clientSecret, redirectURL string) *OAuthConfig {
	return &OAuthConfig{
		ProviderName: "github",
		ClientID:     clientID,
		ClientSecret: clientSecret,
		RedirectURL:  redirectURL,
		AuthURL:      "https://github.com/login/oauth/authorize",
		TokenURL:     "https://github.com/login/oauth/access_token",
		UserInfoURL:  "https://api.github.com/user",
		Scopes:       []string{"user:email", "read:user"},
		Timeout:      30 * time.Second,
	}
}

// MicrosoftOAuthConfig returns OAuth config for Microsoft/Azure AD
func MicrosoftOAuthConfig(clientID, clientSecret, redirectURL, tenant string) *OAuthConfig {
	if tenant == "" {
		tenant = "common"
	}
	return &OAuthConfig{
		ProviderName: "microsoft",
		ClientID:     clientID,
		ClientSecret: clientSecret,
		RedirectURL:  redirectURL,
		AuthURL:      fmt.Sprintf("https://login.microsoftonline.com/%s/oauth2/v2.0/authorize", tenant),
		TokenURL:     fmt.Sprintf("https://login.microsoftonline.com/%s/oauth2/v2.0/token", tenant),
		UserInfoURL:  "https://graph.microsoft.com/oidc/userinfo",
		Scopes:       []string{"openid", "email", "profile", "User.Read"},
		Timeout:      30 * time.Second,
	}
}

// OktaOAuthConfig returns OAuth config for Okta
func OktaOAuthConfig(clientID, clientSecret, redirectURL, domain string) *OAuthConfig {
	return &OAuthConfig{
		ProviderName: "okta",
		ClientID:     clientID,
		ClientSecret: clientSecret,
		RedirectURL:  redirectURL,
		AuthURL:      fmt.Sprintf("https://%s/oauth2/default/v1/authorize", domain),
		TokenURL:     fmt.Sprintf("https://%s/oauth2/default/v1/token", domain),
		UserInfoURL:  fmt.Sprintf("https://%s/oauth2/default/v1/userinfo", domain),
		Scopes:       []string{"openid", "email", "profile"},
		Timeout:      30 * time.Second,
	}
}
