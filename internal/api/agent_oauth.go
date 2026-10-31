package api

// The MCP authorization server (WP-AGENT-1 PR A): OAuth 2.1 + PKCE (S256)
// with RFC 7591 dynamic client registration and RFC 8414 metadata — what an
// MCP client (Claude, an IDE) runs against mcp.stored.cloud's protected-
// resource metadata (edge/mcp/). The person consents on the dashboard's
// /dashboard/agents/connect (a session, the role to mint a management key,
// a fresh TOTP code); the token endpoint spends the code once and answers
// the agent grant's key as the access token — the grant IS a management key
// (internal/auth/agent_grants.go). A tenant whose agent_grants flag is off
// gets no consent (the dashboard says so), so its grants never exist.

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"time"

	"go.uber.org/zap"

	"github.com/FairForge/vaultaire/internal/audit"
	"github.com/FairForge/vaultaire/internal/auth"
	"github.com/FairForge/vaultaire/internal/hosts"
)

func (s *Server) registerAgentOAuthRoutes() {
	s.router.Get("/.well-known/oauth-authorization-server", s.handleOAuthMetadata)
	s.router.Post("/oauth/register", s.handleOAuthRegister)
	s.router.Get("/oauth/authorize", s.handleOAuthAuthorize)
	s.router.Post("/oauth/token", s.handleOAuthToken)
}

// handleOAuthMetadata is RFC 8414's document.
func (s *Server) handleOAuthMetadata(w http.ResponseWriter, _ *http.Request) {
	h := hosts.Get()
	w.Header().Set("Cache-Control", "public, max-age=3600")
	writeJSON(w, http.StatusOK, map[string]any{
		"issuer":                                h.Canonical,
		"authorization_endpoint":                h.URL("/oauth/authorize"),
		"token_endpoint":                        h.URL("/oauth/token"),
		"registration_endpoint":                 h.URL("/oauth/register"),
		"response_types_supported":              []string{"code"},
		"grant_types_supported":                 []string{"authorization_code"},
		"code_challenge_methods_supported":      []string{"S256"},
		"token_endpoint_auth_methods_supported": []string{"none"},
		"scopes_supported":                      []string{"stored"},
		"service_documentation":                 h.URL("/docs/api#agents"),
	})
}

// oauthError is RFC 6749 § 5.2's error body.
func oauthError(w http.ResponseWriter, status int, code, description string) {
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, status, map[string]string{"error": code, "error_description": description})
}

// handleOAuthRegister is RFC 7591's registration: a public client (no
// secret: PKCE is its proof).
func (s *Server) handleOAuthRegister(w http.ResponseWriter, r *http.Request) {
	if s.db == nil {
		oauthError(w, http.StatusServiceUnavailable, "temporarily_unavailable", "no database")
		return
	}
	var req struct {
		ClientName   string   `json:"client_name"`
		RedirectURIs []string `json:"redirect_uris"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&req); err != nil {
		oauthError(w, http.StatusBadRequest, "invalid_client_metadata", "the body must be a JSON client registration")
		return
	}
	c, err := auth.RegisterOAuthClient(r.Context(), s.db, req.ClientName, req.RedirectURIs)
	if errors.Is(err, auth.ErrOAuthInvalidClient) {
		oauthError(w, http.StatusBadRequest, "invalid_redirect_uri", err.Error())
		return
	}
	if err != nil {
		s.logger.Error("oauth register", zap.Error(err))
		oauthError(w, http.StatusInternalServerError, "server_error", "the client could not be registered")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusCreated, map[string]any{"client_id": c.ID, "client_name": c.Name, "redirect_uris": c.RedirectURIs,
		"client_id_issued_at": time.Now().Unix(), "token_endpoint_auth_method": "none",
		"grant_types": []string{"authorization_code"}, "response_types": []string{"code"}})
}

// handleOAuthAuthorize validates the request and sends the person to the
// dashboard's consent page with it. A bad client or redirect is answered
// here (the redirect cannot be trusted with the error); any other error
// goes back to the client's redirect, as RFC 6749 § 4.1.2.1 says.
func (s *Server) handleOAuthAuthorize(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	a := auth.AuthorizationRequest{ClientID: q.Get("client_id"), RedirectURI: q.Get("redirect_uri"), State: q.Get("state"),
		CodeChallenge: q.Get("code_challenge"), CodeChallengeMethod: q.Get("code_challenge_method"), ResponseType: q.Get("response_type")}
	if s.db == nil {
		http.Error(w, "the authorization server is unavailable", http.StatusServiceUnavailable)
		return
	}
	_, err := auth.ValidateAuthorization(r.Context(), s.db, a)
	switch {
	case errors.Is(err, auth.ErrOAuthInvalidClient):
		http.Error(w, "unknown client or unregistered redirect_uri", http.StatusBadRequest)
		return
	case errors.Is(err, auth.ErrOAuthInvalidRequest):
		http.Redirect(w, r, oauthRedirect(a.RedirectURI, url.Values{"error": {"invalid_request"},
			"error_description": {err.Error()}, "state": {a.State}}), http.StatusFound)
		return
	case err != nil:
		s.logger.Error("oauth authorize", zap.Error(err))
		http.Error(w, "the authorization server failed", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/dashboard/agents/connect?"+r.URL.RawQuery, http.StatusFound)
}

// oauthRedirect is redirect with v added to its query.
func oauthRedirect(redirect string, v url.Values) string {
	u, err := url.Parse(redirect)
	if err != nil {
		return redirect
	}
	q := u.Query()
	for k, vs := range v {
		if len(vs) > 0 && vs[0] != "" {
			q.Set(k, vs[0])
		}
	}
	u.RawQuery = q.Encode()
	return u.String()
}

// handleOAuthToken is the authorization_code grant: the code spent once,
// the agent grant minted, its key the access token. The grant's sandbox key
// rides along as stored_test_token (the rehearsal key, RP88-1).
func (s *Server) handleOAuthToken(w http.ResponseWriter, r *http.Request) {
	if s.db == nil {
		oauthError(w, http.StatusServiceUnavailable, "temporarily_unavailable", "no database")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	if err := r.ParseForm(); err != nil {
		oauthError(w, http.StatusBadRequest, "invalid_request", "the body must be application/x-www-form-urlencoded")
		return
	}
	if gt := r.PostForm.Get("grant_type"); gt != "authorization_code" {
		oauthError(w, http.StatusBadRequest, "unsupported_grant_type", "only authorization_code is supported; a grant is renewed by its person in the dashboard")
		return
	}
	g, sec, err := auth.ExchangeAgentCode(r.Context(), s.db, auth.AgentTokenRequest{Code: r.PostForm.Get("code"),
		ClientID: r.PostForm.Get("client_id"), RedirectURI: r.PostForm.Get("redirect_uri"), CodeVerifier: r.PostForm.Get("code_verifier")})
	switch {
	case errors.Is(err, auth.ErrOAuthInvalidGrant):
		oauthError(w, http.StatusBadRequest, "invalid_grant", "the code is unknown, used, expired, or does not match the client, the redirect or the verifier")
		return
	case err != nil:
		s.logger.Error("oauth token", zap.Error(err))
		oauthError(w, http.StatusInternalServerError, "server_error", "the grant could not be made")
		return
	}
	ctx := audit.WithPrincipal(r.Context(), "agent:"+g.ID)
	ctx = audit.WithOnBehalfOf(ctx, g.GrantedBy)
	audit.Record(ctx, s.db, audit.Entry{TenantID: g.TenantID, Action: "agent.grant_created", Resource: "agent:" + g.ID,
		Metadata: map[string]any{"agent_name": g.AgentName, "client_id": g.ClientID, "expires_at": g.ExpiresAt,
			"storage_limit_bytes": g.StorageLimit, "egress_limit_bytes": g.EgressLimit}})
	expiresIn := int64(auth.AgentGrantLifetime / time.Second)
	if g.ExpiresAt != nil {
		expiresIn = int64(time.Until(*g.ExpiresAt) / time.Second)
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
	writeJSON(w, http.StatusOK, map[string]any{"access_token": sec.Live, "token_type": "Bearer", "expires_in": expiresIn,
		"scope": "stored", "stored_test_token": sec.Test, "stored_agent_grant_id": g.ID})
}
