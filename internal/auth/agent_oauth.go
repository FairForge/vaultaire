// internal/auth/agent_oauth.go
//
// The MCP authorization flow (WP-AGENT-1 PR A; OAuth 2.1 with PKCE and
// RFC 7591 dynamic client registration — the flow MCP clients speak): a
// client registers itself (no authority), sends the person to the
// authorization endpoint, the person consents in the dashboard (a session
// and a TOTP code: IssueAgentCode), and the client exchanges the code ONCE
// for the agent grant's key (ExchangeAgentCode). The access token IS the
// grant's management key: there is no second credential kind.
package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/lib/pq"
)

var (
	// ErrOAuthInvalidClient: no registered client has that id, or the
	// registration does not validate.
	ErrOAuthInvalidClient = errors.New("invalid_client")
	// ErrOAuthInvalidRequest: a malformed authorization request.
	ErrOAuthInvalidRequest = errors.New("invalid_request")
	// ErrOAuthInvalidGrant: the code is unknown, used, expired, or does not
	// match the client, the redirect or the PKCE verifier.
	ErrOAuthInvalidGrant = errors.New("invalid_grant")
)

// OAuthClient is a dynamically registered client.
type OAuthClient struct {
	ID           string   `json:"client_id"`
	Name         string   `json:"client_name"`
	RedirectURIs []string `json:"redirect_uris"`
}

// privateScheme is a native app's private-use URI scheme (RFC 8252 § 7.1).
var privateScheme = regexp.MustCompile(`^[a-z][a-z0-9+.-]*$`)

// validRedirectURI: https anywhere; http to a loopback address only (RFC
// 8252 § 7.3); a native app's private-use scheme; never a fragment, and
// never a scheme a browser would run (javascript, data, file, vbscript).
func validRedirectURI(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Fragment != "" || u.Scheme == "" || len(raw) > 2000 {
		return false
	}
	switch s := strings.ToLower(u.Scheme); s {
	case "https":
		return u.Host != ""
	case "http":
		host := u.Hostname()
		ip := net.ParseIP(host)
		return host == "localhost" || (ip != nil && ip.IsLoopback())
	case "javascript", "data", "file", "vbscript", "blob", "about":
		return false
	default:
		return privateScheme.MatchString(s)
	}
}

func randomToken(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("oauth: random: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func oauthSHA256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// RegisterOAuthClient registers a client (RFC 7591): 1–8 redirect URIs.
func RegisterOAuthClient(ctx context.Context, db *sql.DB, name string, redirectURIs []string) (*OAuthClient, error) {
	name = strings.TrimSpace(name)
	if len([]rune(name)) > 100 {
		return nil, fmt.Errorf("%w: client_name is longer than 100 characters", ErrOAuthInvalidClient)
	}
	if len(redirectURIs) == 0 || len(redirectURIs) > 8 {
		return nil, fmt.Errorf("%w: 1 to 8 redirect_uris", ErrOAuthInvalidClient)
	}
	for _, u := range redirectURIs {
		if !validRedirectURI(u) {
			return nil, fmt.Errorf("%w: redirect_uri %q is not https, an http loopback address or a native app's scheme", ErrOAuthInvalidClient, u)
		}
	}
	tok, err := randomToken(18)
	if err != nil {
		return nil, err
	}
	c := &OAuthClient{ID: "mcp_" + tok, Name: name, RedirectURIs: append([]string(nil), redirectURIs...)}
	if _, err := db.ExecContext(ctx, `INSERT INTO agent_oauth_clients (client_id, client_name, redirect_uris) VALUES ($1, $2, $3)`,
		c.ID, c.Name, pq.Array(c.RedirectURIs)); err != nil {
		return nil, fmt.Errorf("register an oauth client: %w", err)
	}
	return c, nil
}

// GetOAuthClient reads a registered client.
func GetOAuthClient(ctx context.Context, db *sql.DB, id string) (*OAuthClient, error) {
	c := &OAuthClient{ID: id}
	err := db.QueryRowContext(ctx, `SELECT client_name, redirect_uris FROM agent_oauth_clients WHERE client_id = $1`, id).
		Scan(&c.Name, pq.Array(&c.RedirectURIs))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrOAuthInvalidClient
	}
	if err != nil {
		return nil, fmt.Errorf("read oauth client %s: %w", id, err)
	}
	return c, nil
}

// Allows reports whether uri is one of the client's registered redirect
// URIs, exactly (OAuth 2.1: no partial match).
func (c *OAuthClient) Allows(uri string) bool {
	for _, u := range c.RedirectURIs {
		if u == uri {
			return true
		}
	}
	return false
}

// AuthorizationRequest is what the client asked for (the query of the
// authorization endpoint), validated by ValidateAuthorization.
type AuthorizationRequest struct {
	ClientID, RedirectURI, State, CodeChallenge, CodeChallengeMethod, ResponseType string
}

var pkceChallenge = regexp.MustCompile(`^[A-Za-z0-9_-]{43,128}$`)

// ValidateAuthorization checks a request against its client: the client
// exists, the redirect is registered (else ErrOAuthInvalidClient — the
// redirect cannot be trusted with an error), response_type code, PKCE S256.
func ValidateAuthorization(ctx context.Context, db *sql.DB, a AuthorizationRequest) (*OAuthClient, error) {
	c, err := GetOAuthClient(ctx, db, a.ClientID)
	if err != nil {
		return nil, err
	}
	if !c.Allows(a.RedirectURI) {
		return nil, fmt.Errorf("%w: redirect_uri is not registered for this client", ErrOAuthInvalidClient)
	}
	switch {
	case a.ResponseType != "code":
		return c, fmt.Errorf("%w: response_type must be code", ErrOAuthInvalidRequest)
	case a.CodeChallengeMethod != "S256":
		return c, fmt.Errorf("%w: PKCE with code_challenge_method S256 is required", ErrOAuthInvalidRequest)
	case !pkceChallenge.MatchString(a.CodeChallenge):
		return c, fmt.Errorf("%w: code_challenge is not a base64url S256 challenge", ErrOAuthInvalidRequest)
	}
	return c, nil
}

// AgentConsent is a person's consent to an authorization request.
type AgentConsent struct {
	AuthorizationRequest
	TenantID, UserID, AgentName string
	StorageLimit, EgressLimit   *int64
	LifetimeDays                int // 1..30; 0 = 30
}

// agentCodeLife is how long a consent's code may be exchanged.
const agentCodeLife = 10 * time.Minute

// IssueAgentCode records the consent and returns its single-use code (only
// its sha256 is kept). The caller has checked the person (a session and a
// fresh TOTP code) and their role.
func IssueAgentCode(ctx context.Context, db *sql.DB, c AgentConsent) (string, error) {
	if _, err := ValidateAuthorization(ctx, db, c.AuthorizationRequest); err != nil {
		return "", err
	}
	if c.LifetimeDays <= 0 || c.LifetimeDays > 30 {
		c.LifetimeDays = 30
	}
	for _, v := range []*int64{c.StorageLimit, c.EgressLimit} {
		if v != nil && *v < 0 {
			return "", fmt.Errorf("%w: a cap is negative", ErrAgentGrantInvalid)
		}
	}
	code, err := randomToken(32)
	if err != nil {
		return "", err
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO agent_oauth_codes (code_hash, client_id, tenant_id, user_id, redirect_uri, code_challenge, agent_name,
		                               storage_limit_bytes, egress_limit_bytes, lifetime_days, expires_at)
		VALUES ($1, $2, $3, $4::uuid, $5, $6, $7, $8, $9, $10, NOW() + $11::interval)`,
		oauthSHA256Hex(code), c.ClientID, c.TenantID, c.UserID, c.RedirectURI, c.CodeChallenge, strings.TrimSpace(c.AgentName),
		c.StorageLimit, c.EgressLimit, c.LifetimeDays, agentCodeLife.String()); err != nil {
		return "", fmt.Errorf("issue an agent code: %w", err)
	}
	return code, nil
}

// AgentTokenRequest is the token endpoint's authorization_code grant.
type AgentTokenRequest struct {
	Code, ClientID, RedirectURI, CodeVerifier string
}

// ExchangeAgentCode spends a code ONCE (the UPDATE that marks it used is
// the claim: a second exchange finds it used) and mints the grant it
// consented to. A code whose client, redirect or PKCE verifier does not
// match is spent all the same (RFC 6749 § 4.1.2: a replayed or stolen code
// is burned) and answers ErrOAuthInvalidGrant.
func ExchangeAgentCode(ctx context.Context, db *sql.DB, req AgentTokenRequest) (*AgentGrant, AgentGrantSecrets, error) {
	if req.Code == "" {
		return nil, AgentGrantSecrets{}, ErrOAuthInvalidGrant
	}
	var clientID, tenantID, userID, redirect, challenge, agentName string
	var storage, egress sql.NullInt64
	var days int
	err := db.QueryRowContext(ctx, `
		UPDATE agent_oauth_codes SET used_at = NOW()
		 WHERE code_hash = $1 AND used_at IS NULL AND expires_at > NOW()
		RETURNING client_id, tenant_id, user_id::text, redirect_uri, code_challenge, agent_name,
		          storage_limit_bytes, egress_limit_bytes, lifetime_days`, oauthSHA256Hex(req.Code)).
		Scan(&clientID, &tenantID, &userID, &redirect, &challenge, &agentName, &storage, &egress, &days)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, AgentGrantSecrets{}, ErrOAuthInvalidGrant
	}
	if err != nil {
		return nil, AgentGrantSecrets{}, fmt.Errorf("exchange an agent code: %w", err)
	}
	sum := sha256.Sum256([]byte(req.CodeVerifier))
	verified := base64.RawURLEncoding.EncodeToString(sum[:])
	if req.ClientID != clientID || req.RedirectURI != redirect ||
		subtle.ConstantTimeCompare([]byte(verified), []byte(challenge)) != 1 {
		return nil, AgentGrantSecrets{}, ErrOAuthInvalidGrant
	}
	name := agentName
	if name == "" {
		if c, err := GetOAuthClient(ctx, db, clientID); err == nil {
			name = c.Name
		}
	}
	o := AgentGrantOptions{AgentName: name, ClientID: clientID, GrantedBy: userID, Lifetime: time.Duration(days) * 24 * time.Hour}
	if storage.Valid {
		o.StorageLimit = &storage.Int64
	}
	if egress.Valid {
		o.EgressLimit = &egress.Int64
	}
	g, sec, err := MintAgentGrant(ctx, db, tenantID, o)
	if err != nil {
		return nil, AgentGrantSecrets{}, err
	}
	_, _ = db.ExecContext(ctx, `UPDATE agent_oauth_codes SET grant_id = $2::uuid WHERE code_hash = $1`, oauthSHA256Hex(req.Code), g.ID)
	_, _ = db.ExecContext(ctx, `UPDATE agent_oauth_clients SET last_used_at = NOW() WHERE client_id = $1`, clientID)
	return g, sec, nil
}
