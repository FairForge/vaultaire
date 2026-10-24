package api

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"go.uber.org/zap"

	"github.com/FairForge/vaultaire/internal/account"
	"github.com/FairForge/vaultaire/internal/audit"
	"github.com/FairForge/vaultaire/internal/dashboard"
	dashauth "github.com/FairForge/vaultaire/internal/dashboard/auth"
	dashmw "github.com/FairForge/vaultaire/internal/dashboard/middleware"
	"github.com/FairForge/vaultaire/internal/recovery/secret"
)

// The recovery vault's Vaultaire side (keystone K5 PR 4, docs/reviews/
// K5-PROBE.md): POST /api/v1/recovery/tokens mints, for the signed-in user,
// one short-lived token per configured realm. The client then talks to the
// realms directly — Vaultaire never sees the PIN or the secret, and links
// none of the realm (TestRecoveryRealm_NotLinkedIntoVaultaire).
//
// The token's sub is base64url(HMAC-SHA256(recovery_subject_key,
// "stored/recovery/v1" ‖ user_id)): never a user id, e-mail or tenant id
// (ARCHITECTURE_CONTRACT line 12). A realm learns a tenant name ("stored"),
// that opaque sub and ciphertext.
//
// Dark: the recovery_vault flag (default OFF, global only) is checked before
// anything else, so the route is a 404 to everyone while it is off. Until
// two realms under different control exist the vault is attempt-limited by
// our realm, never "hardware" (F-19).

// flagRecoveryVault opens POST /api/v1/recovery/tokens. Default OFF; global.
const flagRecoveryVault = "recovery_vault"

const (
	recoverySubjectLabel  = "stored/recovery/v1"
	recoveryTokenLifetime = 10 * time.Minute // the realms refuse more than 24 h
	recoveryTokensPerHour = 10

	credentialRecoverySubjectKey = "recovery_subject_key"
	envRecoverySubjectKey        = "RECOVERY_SUBJECT_KEY"
	credentialRecoveryTokenKeys  = "recovery_token_keys"
	envRecoveryTokenKeys         = "RECOVERY_TOKEN_KEYS"
	envRecoveryRealmsFile        = "RECOVERY_REALMS_FILE"

	minRecoveryKeyBytes = 32
)

var (
	reRecoveryKid   = regexp.MustCompile(`^([a-zA-Z0-9]+):([0-9]+)$`)
	reRealmIDHex    = regexp.MustCompile(`^[0-9a-f]{32}$`)
	rePublicKeyHex  = regexp.MustCompile(`^[0-9a-f]{64}$`)
	errRecoveryConf = errors.New("recovery vault configuration unusable")
)

// recoveryRealmFile is one entry of RECOVERY_REALMS_FILE.
type recoveryRealmFile struct {
	ID        string `json:"id"`
	Address   string `json:"address"`
	PublicKey string `json:"public_key"`
	Kid       string `json:"kid"`
}

type recoveryRealm struct {
	recoveryRealmFile
	tenant string // the kid's tenant: the token's iss
	key    []byte // HS256, from recovery_token_keys by realm id
}

// recoveryVault is the token minter; nil when nothing is configured.
type recoveryVault struct {
	subjectKey []byte
	realms     []recoveryRealm
	limiter    *perUserLimiter
	now        func() time.Time
}

// loadRecoveryVault reads the three settings. None set = nil, nil (the
// route answers 503 while the flag is on). Any set but unusable — or only
// some set — is an error the boot makes fatal. Errors never quote a key.
func loadRecoveryVault(getenv func(string) string, readFile func(string) ([]byte, error)) (*recoveryVault, error) {
	subj, err := secret.Load(getenv, readFile, credentialRecoverySubjectKey, envRecoverySubjectKey)
	if err != nil {
		return nil, fmt.Errorf("recovery subject key: %w", err)
	}
	keys, err := secret.Load(getenv, readFile, credentialRecoveryTokenKeys, envRecoveryTokenKeys)
	if err != nil {
		return nil, fmt.Errorf("recovery token keys: %w", err)
	}
	realmsPath := getenv(envRecoveryRealmsFile)
	set := 0
	for _, isSet := range []bool{subj.Bytes != nil, keys.Bytes != nil, realmsPath != ""} {
		if isSet {
			set++
		}
	}
	if set == 0 {
		return nil, nil
	}
	if set != 3 {
		return nil, fmt.Errorf("%w: the subject key (credential %s / %s), the token keys (credential %s / %s) and %s go together",
			errRecoveryConf, credentialRecoverySubjectKey, envRecoverySubjectKey, credentialRecoveryTokenKeys, envRecoveryTokenKeys, envRecoveryRealmsFile)
	}

	v := &recoveryVault{limiter: newPerUserLimiter(recoveryTokensPerHour, time.Hour), now: time.Now}
	if v.subjectKey, err = hex.DecodeString(string(subj.Bytes)); err != nil || len(v.subjectKey) < minRecoveryKeyBytes {
		return nil, fmt.Errorf("%w: the recovery subject key (from the %s) is not hex of at least %d bytes", errRecoveryConf, subj.Source, minRecoveryKeyBytes)
	}
	var keyHex map[string]string
	if err := json.Unmarshal(keys.Bytes, &keyHex); err != nil {
		return nil, fmt.Errorf("%w: the recovery token keys (from the %s) are not a JSON object {realm id: hex key}", errRecoveryConf, keys.Source)
	}
	raw, err := readFile(realmsPath)
	if err != nil {
		return nil, fmt.Errorf("%w: %s cannot be read", errRecoveryConf, envRecoveryRealmsFile)
	}
	var realms []recoveryRealmFile
	if err := json.Unmarshal(raw, &realms); err != nil || len(realms) == 0 {
		return nil, fmt.Errorf("%w: %s is not a JSON array of realms", errRecoveryConf, envRecoveryRealmsFile)
	}
	seen := map[string]bool{}
	for _, r := range realms {
		r.ID, r.PublicKey = strings.ToLower(r.ID), strings.ToLower(r.PublicKey)
		u, uerr := url.Parse(r.Address)
		m := reRecoveryKid.FindStringSubmatch(r.Kid)
		switch {
		case !reRealmIDHex.MatchString(r.ID):
			return nil, fmt.Errorf("%w: realm id %q is not 32 hex characters", errRecoveryConf, r.ID)
		case seen[r.ID]:
			return nil, fmt.Errorf("%w: realm %s listed twice", errRecoveryConf, r.ID)
		case uerr != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "":
			return nil, fmt.Errorf("%w: realm %s: address %q is not an http(s) URL", errRecoveryConf, r.ID, r.Address)
		case !rePublicKeyHex.MatchString(r.PublicKey):
			return nil, fmt.Errorf("%w: realm %s: public_key is not 64 hex characters", errRecoveryConf, r.ID)
		case m == nil:
			return nil, fmt.Errorf("%w: realm %s: kid %q is not <tenant>:<version>", errRecoveryConf, r.ID, r.Kid)
		}
		seen[r.ID] = true
		k, err := hex.DecodeString(strings.TrimSpace(keyHex[r.ID]))
		if err != nil || len(k) < minRecoveryKeyBytes {
			return nil, fmt.Errorf("%w: no usable token key for realm %s (hex of at least %d bytes)", errRecoveryConf, r.ID, minRecoveryKeyBytes)
		}
		if !strings.HasSuffix(r.Address, "/") {
			r.Address += "/" // the SDK joins "req" onto it
		}
		v.realms = append(v.realms, recoveryRealm{recoveryRealmFile: r, tenant: m[1], key: k})
	}
	return v, nil
}

// recoveryVaultOrFatal loads the minter and stops the process when a source
// is set but unusable (the master key's rule).
func recoveryVaultOrFatal(logger *zap.Logger, getenv func(string) string, readFile func(string) ([]byte, error)) *recoveryVault {
	v, err := loadRecoveryVault(getenv, readFile)
	if err != nil {
		logger.Fatal("recovery vault: refusing to start", zap.Error(err))
		return nil
	}
	if v == nil {
		logger.Info("recovery vault: not configured — POST /api/v1/recovery/tokens answers 503 if recovery_vault is on")
		return nil
	}
	ids := make([]string, len(v.realms))
	for i, r := range v.realms {
		ids[i] = r.ID
	}
	logger.Info("recovery vault: token minter ready", zap.Strings("realms", ids))
	return v
}

// subject is the opaque, stable per-user id a realm keys its record by.
func (v *recoveryVault) subject(userID string) string {
	mac := hmac.New(sha256.New, v.subjectKey)
	mac.Write([]byte(recoverySubjectLabel))
	mac.Write([]byte(userID))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

type recoveryRealmToken struct {
	ID        string `json:"id"`
	Address   string `json:"address"`
	PublicKey string `json:"public_key"`
	Token     string `json:"token"`
}

func (v *recoveryVault) mint(userID string) ([]recoveryRealmToken, error) {
	sub := v.subject(userID)
	now := v.now()
	out := make([]recoveryRealmToken, 0, len(v.realms))
	for _, r := range v.realms {
		s, err := r.token(sub, now)
		if err != nil {
			return nil, err
		}
		out = append(out, recoveryRealmToken{ID: r.ID, Address: r.Address, PublicKey: r.PublicKey, Token: s})
	}
	return out, nil
}

// token is the realm's HS256 token for sub: the one set of claims both the
// route and the erasure (recovery_erasure.go) send, never a longer life.
func (r recoveryRealm) token(sub string, now time.Time) (string, error) {
	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"iss": r.tenant, "sub": sub, "aud": r.ID, "scope": "user",
		"iat": now.Unix(), "nbf": now.Unix(), "exp": now.Add(recoveryTokenLifetime).Unix(),
	})
	tok.Header["kid"] = r.Kid
	s, err := tok.SignedString(r.key)
	if err != nil {
		return "", fmt.Errorf("sign the token for realm %s: %w", r.ID, err)
	}
	return s, nil
}

// perUserLimiter allows n events per user per window (in-process: a slot
// switch resets it, which at worst grants one more window).
type perUserLimiter struct {
	mu     sync.Mutex
	n      int
	window time.Duration
	seen   map[string][]time.Time
}

func newPerUserLimiter(n int, window time.Duration) *perUserLimiter {
	return &perUserLimiter{n: n, window: window, seen: map[string][]time.Time{}}
}

// allow records an event for user at now, or returns how long until one is
// allowed.
func (l *perUserLimiter) allow(user string, now time.Time) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	cut := now.Add(-l.window)
	kept := l.seen[user][:0]
	for _, t := range l.seen[user] {
		if t.After(cut) {
			kept = append(kept, t)
		}
	}
	if len(kept) >= l.n {
		l.seen[user] = kept
		return false, kept[0].Sub(cut)
	}
	l.seen[user] = append(kept, now)
	if len(l.seen) > 100_000 { // bound the map: drop users with nothing in the window
		for u, ts := range l.seen {
			if len(ts) == 0 || !ts[len(ts)-1].After(cut) {
				delete(l.seen, u)
			}
		}
	}
	return true, 0
}

// registerRecoveryRoutes mounts POST /api/v1/recovery/tokens beside the
// translog read side: dark (404) unless recovery_vault is on, then a user
// JWT or a dashboard session (with the dashboard's session-bound CSRF
// token).
func (s *Server) registerRecoveryRoutes() {
	csrf := dashmw.NewCSRF(dashboard.CSRFKeyFromSecret(os.Getenv("JWT_SECRET")), s.baseURL)
	authed := s.recoveryAuth(csrf, http.HandlerFunc(s.handleRecoveryTokens))
	s.router.Post("/api/v1/recovery/tokens", func(w http.ResponseWriter, r *http.Request) {
		if s.flags == nil || !s.flags.Enabled(flagRecoveryVault, "") {
			http.NotFound(w, r)
			return
		}
		authed.ServeHTTP(w, r)
	})
}

// recoveryAuth accepts a user JWT (Authorization: Bearer) or a dashboard
// session cookie; a session request must also pass the dashboard's CSRF
// check (it is a state-changing POST a page sends).
func (s *Server) recoveryAuth(csrf func(http.Handler) http.Handler, next http.Handler) http.Handler {
	viaJWT := s.requireJWT(next)
	viaSession := csrf(next)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			viaJWT.ServeHTTP(w, r)
			return
		}
		c, err := r.Cookie(dashauth.SessionCookieName)
		if err != nil || c.Value == "" || s.sessionStore == nil {
			writeManagementError(w, ErrTypeAuthentication, "missing_credentials", "sign in, or send a user token", "")
			return
		}
		sd, err := s.sessionStore.Get(r.Context(), c.Value)
		if err != nil || sd == nil {
			writeManagementError(w, ErrTypeAuthentication, "invalid_session", "the session has ended: sign in again", "")
			return
		}
		viaSession.ServeHTTP(w, r.WithContext(contextWithUser(r.Context(), sd.UserID, sd.Email, sd.TenantID)))
	})
}

// contextWithUser carries a session's user the way requireJWT carries a
// token's.
func contextWithUser(ctx context.Context, userID, email, tenantID string) context.Context {
	ctx = context.WithValue(ctx, userIDKey, userID)
	ctx = context.WithValue(ctx, emailKey, email)
	ctx = context.WithValue(ctx, tenantIDKey, tenantID)
	return audit.WithActor(ctx, userID)
}

func (s *Server) handleRecoveryTokens(w http.ResponseWriter, r *http.Request) {
	userID, _ := r.Context().Value(userIDKey).(string)
	tenantID, _ := r.Context().Value(tenantIDKey).(string)
	if userID == "" {
		writeManagementError(w, ErrTypeAuthentication, "missing_credentials", "user not found in token", "")
		return
	}
	v := s.recovery
	if v == nil {
		writeManagementError(w, ErrTypeUnavailable, "recovery_not_configured", "the recovery vault is not configured", "")
		return
	}
	if ok, wait := v.limiter.allow(userID, v.now()); !ok {
		w.Header().Set("Retry-After", strconv.Itoa(int(wait.Seconds())+1))
		writeManagementError(w, ErrTypeRateLimit, "rate_limited", fmt.Sprintf("at most %d token requests an hour", recoveryTokensPerHour), "")
		return
	}
	// The mint and the erasure agree (RP54-1, the WP-P38 post-merge
	// review): the deletion runner's stage b3 Deletes the user's record at
	// every realm and reads the recovery.tokens_issued row to know a token
	// may still be live (recovery_erasure.go). So, in ONE transaction: the
	// user row under FOR SHARE — an account whose erasure is due gets no
	// token (during the grace period nothing is blocked, D-16) — then the
	// trail row, committed before a token leaves. No trail, no token.
	if s.db == nil {
		writeManagementError(w, ErrTypeUnavailable, "recovery_unavailable", "the recovery vault needs the database", "")
		return
	}
	tx, err := s.db.BeginTx(r.Context(), nil)
	if err != nil {
		s.logger.Error("recovery tokens: begin", zap.Error(err))
		writeManagementError(w, ErrTypeUnavailable, "recovery_unavailable", "could not read the account", "")
		return
	}
	defer func() { _ = tx.Rollback() }()
	exists, due, err := account.DueShared(r.Context(), tx, userID, v.now())
	if err != nil {
		s.logger.Error("recovery tokens: read the account", zap.Error(err))
		writeManagementError(w, ErrTypeUnavailable, "recovery_unavailable", "could not read the account", "")
		return
	}
	if !exists || due {
		writeManagementError(w, ErrTypeConflict, "account_erasure_due",
			"the account's deletion date has passed: no recovery token is issued while it is erased", "")
		return
	}
	realms, err := v.mint(userID)
	if err != nil {
		s.logger.Error("recovery tokens", zap.Error(err))
		writeManagementError(w, ErrTypeAPI, "mint_failed", "could not mint the realm tokens", "")
		return
	}
	// The audit row names the user (it is the operator's trail) and never
	// the opaque subject: the trail must not become the join table.
	if err := audit.RecordTx(r.Context(), tx, audit.Entry{UserID: userID, TenantID: tenantID, EventType: "recovery",
		Action: "recovery.tokens_issued", Metadata: map[string]any{"realms": len(realms)}}); err != nil {
		s.logger.Error("recovery tokens: the trail could not be written — nothing issued", zap.Error(err))
		writeManagementError(w, ErrTypeUnavailable, "trail_not_written", "the token issue could not be recorded; nothing was issued", "")
		return
	}
	if err := tx.Commit(); err != nil {
		s.logger.Error("recovery tokens: the trail could not be committed — nothing issued", zap.Error(err))
		writeManagementError(w, ErrTypeUnavailable, "trail_not_written", "the token issue could not be recorded; nothing was issued", "")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]any{"realms": realms})
}
