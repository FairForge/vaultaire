package handlers

// The agent consent (WP-AGENT-1 PR A; docs/ACCOUNTS.md § Agents; F-41): the
// page an MCP client's authorization request lands on. It shows who asks
// (the registered client's name and where it returns to), what the agent
// may do and what it never may without the person's approval, the caps its
// S3 keys carry and the grant's lifetime; approving needs a fresh TOTP code
// (a person with two-factor on) and the role that mints a management key.
// The code it issues is exchanged once at /oauth/token (internal/api).

import (
	"database/sql"
	"errors"
	"html/template"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"go.uber.org/zap"

	"github.com/FairForge/vaultaire/internal/audit"
	"github.com/FairForge/vaultaire/internal/auth"
	dashauth "github.com/FairForge/vaultaire/internal/dashboard/auth"
)

// agentNever is what a default agent scope never does (F-41): each needs
// the person's approval, or is a person's own act.
var agentNever = []string{
	"delete a bucket",
	"make a bucket or a file public",
	"turn a bucket's versioning off, or set Object Lock or retention",
	"make keys beyond its own scope",
	"read anything in a Private bucket",
	"export or erase the account, manage members or billing",
}

func oauthRedirectWith(redirect string, v url.Values) string {
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

// gibField parses an optional GiB form field: nil = none.
func gibField(v string) (*int64, bool) {
	v = strings.TrimSpace(v)
	if v == "" {
		return nil, true
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil || f < 0 || f > 1<<20 || math.IsNaN(f) {
		return nil, false
	}
	n := int64(f * (1 << 30))
	return &n, true
}

// HandleAgentConnect is GET and POST /dashboard/agents/connect.
func HandleAgentConnect(tmpl *template.Template, db *sql.DB, authSvc *auth.AuthService, mfa *auth.MFAService,
	enabled func(tenantID string) bool, logger *zap.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sd := dashauth.GetSession(r.Context())
		if sd == nil {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		if r.Method == http.MethodPost {
			if err := r.ParseForm(); err != nil {
				http.Error(w, "the form could not be read", http.StatusBadRequest)
				return
			}
		}
		get := func(k string) string {
			if r.Method == http.MethodPost {
				return r.PostFormValue(k)
			}
			return r.URL.Query().Get(k)
		}
		a := auth.AuthorizationRequest{ClientID: get("client_id"), RedirectURI: get("redirect_uri"), State: get("state"),
			CodeChallenge: get("code_challenge"), CodeChallengeMethod: get("code_challenge_method"), ResponseType: get("response_type")}
		data := sessionData(sd, "agents")
		withCSRF(r.Context(), data)
		data["Req"] = a
		data["Never"] = agentNever
		render := func(status int) {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.Header().Set("Cache-Control", "no-store")
			w.WriteHeader(status)
			if err := tmpl.ExecuteTemplate(w, "base", data); err != nil {
				logger.Error("render agent connect", zap.Error(err))
			}
		}
		if db == nil || enabled == nil || !enabled(sd.TenantID) {
			data["Unavailable"] = "Connecting an AI agent is not available on this account yet."
			render(http.StatusNotFound)
			return
		}
		client, err := auth.ValidateAuthorization(r.Context(), db, a)
		switch {
		case errors.Is(err, auth.ErrOAuthInvalidClient):
			data["Unavailable"] = "This request names an unknown app, or a return address it did not register. Nothing was shared."
			render(http.StatusBadRequest)
			return
		case errors.Is(err, auth.ErrOAuthInvalidRequest):
			http.Redirect(w, r, oauthRedirectWith(a.RedirectURI, url.Values{"error": {"invalid_request"}, "state": {a.State}}), http.StatusSeeOther)
			return
		case err != nil:
			logger.Error("agent connect: validate", zap.Error(err))
			data["Unavailable"] = "Something went wrong. Try connecting again."
			render(http.StatusInternalServerError)
			return
		}
		data["Client"] = client
		if u, err := url.Parse(a.RedirectURI); err == nil {
			data["ReturnsTo"] = u.Scheme + "://" + u.Host
		}
		if on, err := authSvc.IsMFAEnabled(r.Context(), sd.UserID); err != nil || !on {
			data["NeedsMFA"] = true
		}
		if r.Method == http.MethodGet {
			render(http.StatusOK)
			return
		}

		if r.PostFormValue("decision") != "approve" {
			audit.Record(r.Context(), db, audit.Entry{TenantID: sd.TenantID, Actor: sd.UserID, Action: "agent.consent_denied",
				Metadata: map[string]any{"client_id": client.ID, "client_name": client.Name}})
			http.Redirect(w, r, oauthRedirectWith(a.RedirectURI, url.Values{"error": {"access_denied"}, "state": {a.State}}), http.StatusSeeOther)
			return
		}
		fail := func(msg string) {
			data["Error"] = msg
			data["Storage"], data["Egress"], data["Days"] = r.PostFormValue("storage_gib"), r.PostFormValue("egress_gib"), r.PostFormValue("days")
			render(http.StatusUnprocessableEntity)
		}
		if data["NeedsMFA"] == true {
			fail("Turn on two-factor sign-in in Settings first: connecting an agent needs it.")
			return
		}
		code := strings.TrimSpace(r.PostFormValue("totp_code"))
		secret, err := authSvc.GetMFASecret(r.Context(), sd.UserID)
		if code == "" || err != nil || mfa == nil || !mfa.ValidateCode(secret, code) || !authSvc.ConsumeTOTPCode(sd.UserID, code) {
			fail("Enter the current 6-digit code from your authenticator app.")
			return
		}
		storage, ok1 := gibField(r.PostFormValue("storage_gib"))
		egress, ok2 := gibField(r.PostFormValue("egress_gib"))
		if !ok1 || !ok2 {
			fail("A cap is a number of GiB, or empty for none.")
			return
		}
		days, _ := strconv.Atoi(r.PostFormValue("days"))
		if days < 1 || days > 30 {
			days = 30
		}
		issued, err := auth.IssueAgentCode(r.Context(), db, auth.AgentConsent{AuthorizationRequest: a, TenantID: sd.TenantID,
			UserID: sd.UserID, AgentName: client.Name, StorageLimit: storage, EgressLimit: egress, LifetimeDays: days})
		if err != nil {
			logger.Error("agent connect: issue the code", zap.Error(err))
			fail("The connection could not be made. Try again.")
			return
		}
		audit.Record(r.Context(), db, audit.Entry{TenantID: sd.TenantID, Actor: sd.UserID, Action: "agent.consent_given",
			Metadata: map[string]any{"client_id": client.ID, "client_name": client.Name, "days": days,
				"storage_limit_bytes": storage, "egress_limit_bytes": egress}})
		http.Redirect(w, r, oauthRedirectWith(a.RedirectURI, url.Values{"code": {issued}, "state": {a.State}}), http.StatusSeeOther)
	}
}
