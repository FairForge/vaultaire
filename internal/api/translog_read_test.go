package api

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/pquerna/otp/totp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/FairForge/vaultaire/internal/auth"
	"github.com/FairForge/vaultaire/internal/config"
	"github.com/FairForge/vaultaire/internal/flags"
	"github.com/FairForge/vaultaire/internal/translog"
	"github.com/FairForge/vaultaire/internal/translog/logstore"
	"github.com/FairForge/vaultaire/internal/translog/logstore/logstoretest"
)

// TestTranslog_BuildEntryOncePerBinary: one server_build entry per distinct
// (git sha, sha256 of the executable); a restart of the same binary appends
// nothing; another binary appends one more.
func TestTranslog_BuildEntryOncePerBinary(t *testing.T) {
	jf := setupJobsFixture(t)
	store := logstore.New(jf.db, nil)
	dir := t.TempDir()
	exe := filepath.Join(dir, "vaultaire")
	content := []byte("binary " + uuid.New().String())
	require.NoError(t, os.WriteFile(exe, content, 0o700))
	sum := sha256.Sum256(content)
	ctx := context.Background()

	count := func(bin string) int {
		var n int
		require.NoError(t, jf.db.QueryRow(`SELECT COUNT(*) FROM translog_entries WHERE kind = 'server_build' AND dedup_key LIKE '%' || $1`, bin).Scan(&n))
		return n
	}
	idx1, appended, err := recordServerBuild(ctx, store, exe)
	require.NoError(t, err)
	assert.True(t, appended)
	idx2, appended, err := recordServerBuild(ctx, store, exe)
	require.NoError(t, err)
	assert.False(t, appended, "a restart of the same binary appends nothing")
	assert.Equal(t, idx1, idx2)
	assert.Equal(t, 1, count(hex.EncodeToString(sum[:])))

	var body []byte
	require.NoError(t, jf.db.QueryRow(`SELECT body FROM translog_entries WHERE idx = $1`, idx1).Scan(&body))
	d, err := translog.Decode(body)
	require.NoError(t, err)
	b := d.Entry.(translog.ServerBuild)
	assert.Equal(t, hex.EncodeToString(sum[:]), b.BinarySHA256)
	assert.Equal(t, BuildSHA, b.GitSHA)

	other := filepath.Join(dir, "vaultaire-2")
	require.NoError(t, os.WriteFile(other, append(content, '!'), 0o700))
	idx3, appended, err := recordServerBuild(ctx, store, other)
	require.NoError(t, err)
	assert.True(t, appended)
	assert.Greater(t, idx3, idx1)

	// A binary that cannot be read is an error (the boot logs it, never blocks).
	_, _, err = recordServerBuild(ctx, store, filepath.Join(dir, "missing"))
	assert.Error(t, err)
}

// The read side: dark behind translog_public (404), and a signed S3 request
// on the same path is the S3 API's, flag or not.
func TestTranslogPublic_FlagOffIs404_OnServes_SignedGoesToS3(t *testing.T) {
	jf := setupJobsFixture(t)
	skey, vkey, err := translog.GenerateKey(rand.Reader)
	require.NoError(t, err)
	signer, err := translog.NewSigner(skey)
	require.NoError(t, err)
	verifier, err := translog.NewVerifier(vkey)
	require.NoError(t, err)

	fl := flags.New(jf.db, zap.NewNop())
	registerFlags(fl)
	s := &Server{logger: zap.NewNop(), router: chi.NewRouter(), db: jf.db, flags: fl, config: &config.Config{}}
	// Its own log (RP37-13): it signs, and on the shared log another
	// package's key could sign the size first.
	s.translog = logstore.New(logstoretest.PrivateLog(t), signer)
	s3Hit := 0
	s.s3Fallback = func(w http.ResponseWriter, r *http.Request) { s3Hit++; w.WriteHeader(http.StatusTeapot) }
	s.registerTranslogReadRoutes()
	_, _, err = s.translog.Append(context.Background(), translog.KeyCeremony{Action: translog.ActionKeyInstalled,
		KeyRole: translog.KeyTranslogSigningKey, KeyFingerprint: "sha256:" + strings.Repeat("1", 64), Participants: 1})
	require.NoError(t, err)
	_, err = s.translog.Checkpoint(context.Background())
	require.NoError(t, err)

	do := func(path string, signed bool) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		if signed {
			req.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential=AKIA/20261022/us-east-1/s3/aws4_request, SignedHeaders=host, Signature=00")
		}
		w := httptest.NewRecorder()
		s.router.ServeHTTP(w, req)
		return w
	}
	// Default OFF: 404 on every path.
	for _, p := range []string{"/translog/checkpoint", "/translog/tile/0/000.p/1", "/translog/tile/entries/000.p/1"} {
		assert.Equal(t, http.StatusNotFound, do(p, false).Code, p)
	}
	assert.Equal(t, http.StatusTeapot, do("/translog/checkpoint", true).Code, "a signed request is the S3 API's")
	assert.Equal(t, http.StatusTeapot, do("/translog/checkpoint?X-Amz-Signature=00&X-Amz-Credential=x", false).Code, "a presigned one too")

	// ON (global row): the checkpoint opens with the key.
	require.NoError(t, fl.Set(context.Background(), flagTranslogPublic, "*", true, "test"))
	t.Cleanup(func() { _ = fl.Unset(context.Background(), flagTranslogPublic, "*") })
	w := do("/translog/checkpoint", false)
	require.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "no-store", w.Header().Get("Cache-Control"))
	_, err = translog.OpenCheckpoint(w.Body.Bytes(), verifier)
	require.NoError(t, err)
	assert.Equal(t, http.StatusTeapot, do("/translog/checkpoint", true).Code)
}

// The ceremony route: admin JWT + a fresh TOTP code of the admin's own
// MFA, audited; refused entries are 400 and append nothing.
func TestTranslogCeremonyRoute_AdminMFAAudited(t *testing.T) {
	jf := setupJobsFixture(t)
	db := jf.db
	adminID, userID := uuid.New().String(), uuid.New().String()
	for id, role := range map[string]string{adminID: "admin", userID: "user"} {
		_, err := db.Exec(`INSERT INTO users (id, email, password_hash, role) VALUES ($1, $2, 'x', $3)`,
			id, fmt.Sprintf("tl-%s-%d@test.local", role, time.Now().UnixNano()), role)
		require.NoError(t, err)
	}
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM audit_logs WHERE user_id IN ($1, $2)`, adminID, userID)
		_, _ = db.Exec(`DELETE FROM user_mfa WHERE user_id IN ($1, $2)`, adminID, userID)
		_, _ = db.Exec(`DELETE FROM users WHERE id IN ($1, $2)`, adminID, userID)
	})
	authSvc := auth.NewAuthService(nil, db)
	s := &Server{logger: zap.NewNop(), router: chi.NewRouter(), db: db, auth: authSvc, mfaService: auth.NewMFAService("Stored"),
		config: &config.Config{}}
	s.translog = logstore.New(db, nil)
	s.router.Route("/api/v1/admin", func(r chi.Router) {
		r.Use(func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				next.ServeHTTP(w, req.WithContext(context.WithValue(req.Context(), userIDKey, req.Header.Get("X-Test-User"))))
			})
		})
		s.registerTranslogAdminRoutes(r)
	})
	fingerprint := uniqueFingerprint(t)
	body := func(code string) string {
		return fmt.Sprintf(`{"action":"key_installed","key_role":"master_key","key_fingerprint":"sha256:%s","participants":2,"mfa_code":%q}`,
			fingerprint, code)
	}
	call := func(as, b string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/translog/ceremony", strings.NewReader(b))
		req.Header.Set("X-Test-User", as)
		w := httptest.NewRecorder()
		s.router.ServeHTTP(w, req)
		return w
	}
	entries := func() int {
		var n int
		require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM translog_entries WHERE kind = 'key_ceremony' AND convert_from(body, 'UTF8') LIKE '%' || $1 || '%'`, fingerprint).Scan(&n))
		return n
	}

	assert.Equal(t, http.StatusForbidden, call(userID, body("000000")).Code, "a tenant user")
	assert.Equal(t, http.StatusForbidden, call(adminID, body("000000")).Code, "an admin without MFA enrolled")
	require.NoError(t, authSvc.EnableMFA(context.Background(), adminID, "JBSWY3DPEHPK3PXP", []string{"b1"}))
	assert.Equal(t, http.StatusUnauthorized, call(adminID, body("000000")).Code, "a wrong code")
	assert.Equal(t, 0, entries())

	code, err := totp.GenerateCode("JBSWY3DPEHPK3PXP", time.Now())
	require.NoError(t, err)
	bad := strings.Replace(body(code), "sha256:", "md5:", 1)
	assert.Equal(t, http.StatusBadRequest, call(adminID, bad).Code, "a refused entry")
	code2, err := totp.GenerateCode("JBSWY3DPEHPK3PXP", time.Now().Add(30*time.Second))
	require.NoError(t, err)
	w := call(adminID, body(code2))
	require.Equal(t, http.StatusCreated, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), `"index":`)
	assert.Equal(t, 1, entries())
	assert.Equal(t, http.StatusUnauthorized, call(adminID, body(code2)).Code, "a code is accepted once")
	assert.Equal(t, 1, entries())

	var audits int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM audit_logs WHERE action = 'admin.translog_ceremony' AND user_id = $1`, adminID).Scan(&audits))
	assert.GreaterOrEqual(t, audits, 2, "the refusals and the append are audited")
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM audit_logs WHERE action = 'admin.translog_ceremony' AND user_id = $1`, userID).Scan(&audits))
	assert.Equal(t, 1, audits, "a non-admin's 403 is audited by the route's own gate")
}

func uniqueFingerprint(t *testing.T) string {
	t.Helper()
	b := make([]byte, 32)
	_, err := rand.Read(b)
	require.NoError(t, err)
	return hex.EncodeToString(b)
}

// ceremonyRoute is the ceremony route on a router whose JWT is an admin
// with MFA enrolled; call posts a body (with an Idempotency-Key when key is
// not empty).
func ceremonyRoute(t *testing.T) (*Server, func(body, key string) *httptest.ResponseRecorder) {
	t.Helper()
	jf := setupJobsFixture(t)
	db := jf.db
	adminID := uuid.New().String()
	_, err := db.Exec(`INSERT INTO users (id, email, password_hash, role) VALUES ($1, $2, 'x', 'admin')`,
		adminID, fmt.Sprintf("tl-route-%d@test.local", time.Now().UnixNano()))
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM audit_logs WHERE user_id = $1`, adminID)
		_, _ = db.Exec(`DELETE FROM user_mfa WHERE user_id = $1`, adminID)
		_, _ = db.Exec(`DELETE FROM users WHERE id = $1`, adminID)
	})
	authSvc := auth.NewAuthService(nil, db)
	require.NoError(t, authSvc.EnableMFA(context.Background(), adminID, "JBSWY3DPEHPK3PXP", []string{"b1"}))
	s := &Server{logger: zap.NewNop(), router: chi.NewRouter(), db: db, auth: authSvc, mfaService: auth.NewMFAService("Stored"),
		config: &config.Config{}}
	s.translog = logstore.New(db, nil)
	s.router.Route("/api/v1/admin", func(r chi.Router) {
		r.Use(func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				next.ServeHTTP(w, req.WithContext(context.WithValue(req.Context(), userIDKey, adminID)))
			})
		})
		s.registerTranslogAdminRoutes(r)
	})
	return s, func(body, key string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/translog/ceremony", strings.NewReader(body))
		if key != "" {
			req.Header.Set("Idempotency-Key", key)
		}
		w := httptest.NewRecorder()
		s.router.ServeHTTP(w, req)
		return w
	}
}

func ceremonyBody(fingerprint, code string) string {
	return fmt.Sprintf(`{"action":"key_generated","key_role":"translog_signing_key","key_fingerprint":"sha256:%s","participants":1,"mfa_code":%q}`,
		fingerprint, code)
}

func countCeremonies(t *testing.T, s *Server, fingerprint string) int {
	t.Helper()
	var n int
	require.NoError(t, s.db.QueryRow(`SELECT COUNT(*) FROM translog_entries WHERE kind = 'key_ceremony' AND convert_from(body, 'UTF8') LIKE '%' || $1 || '%'`,
		fingerprint).Scan(&n))
	return n
}

// RP37-14: a refused entry is refused before the code is consumed — the
// admin's next try with the same code works.
func TestCeremonyRoute_InvalidEntry_CodeNotBurned(t *testing.T) {
	s, call := ceremonyRoute(t)
	code, err := totp.GenerateCode("JBSWY3DPEHPK3PXP", time.Now())
	require.NoError(t, err)
	fp := uniqueFingerprint(t)
	ok := ceremonyBody(fp, code)

	assert.Equal(t, http.StatusBadRequest, call(strings.Replace(ok, "sha256:", "md5:", 1), "").Code, "a refused entry")
	withStatement := strings.Replace(ok, `"mfa_code"`, `"statement":"Installed by Isaac Viera with Jane Doe of 12 Main Street","mfa_code"`, 1)
	assert.Equal(t, http.StatusBadRequest, call(withStatement, "").Code, "free text is no field (RP37-10)")
	w := call(ok, "")
	assert.Equal(t, http.StatusCreated, w.Code, "the code was not burned by the refused entries: %s", w.Body.String())
	assert.Equal(t, 1, countCeremonies(t, s, fp))
}

// RP37-14: a retry after a commit the client never saw returns the first
// answer and appends nothing; the same key for another entry is a 409.
func TestCeremonyRoute_IdempotentRetry_OneEntry(t *testing.T) {
	s, call := ceremonyRoute(t)
	code, err := totp.GenerateCode("JBSWY3DPEHPK3PXP", time.Now())
	require.NoError(t, err)
	fp := uniqueFingerprint(t)
	key := uuid.New().String()

	first := call(ceremonyBody(fp, code), key)
	require.Equal(t, http.StatusCreated, first.Code, first.Body.String())
	again := call(ceremonyBody(fp, code), key)
	assert.Equal(t, http.StatusCreated, again.Code, "a retry returns the first answer: %s", again.Body.String())
	assert.Equal(t, first.Body.String(), again.Body.String())
	assert.Equal(t, "true", again.Header().Get("Idempotency-Replayed"))
	assert.Equal(t, 1, countCeremonies(t, s, fp))

	other := uniqueFingerprint(t)
	assert.Equal(t, http.StatusConflict, call(ceremonyBody(other, code), key).Code, "the key names another entry")
	assert.Equal(t, 0, countCeremonies(t, s, other))
}
