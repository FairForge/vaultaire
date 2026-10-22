package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"runtime"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"github.com/FairForge/vaultaire/internal/audit"
	"github.com/FairForge/vaultaire/internal/translog"
	"github.com/FairForge/vaultaire/internal/translog/logstore"
)

// The transparency log's producers and read side (K4 PR 3).
//
//   - server_build: at boot (Start), one entry per distinct (git sha, sha256
//     of os.Executable()); a restart of the same binary appends nothing. It
//     never blocks the boot: a failure is an Error line and
//     vaultaire_translog_append_failures_total{kind="server_build"}.
//   - key_ceremony: POST /api/v1/admin/translog/ceremony — admin JWT AND a
//     fresh TOTP code of the admin's own MFA in the body (consumed once),
//     audited (admin.translog_ceremony), refusals included.
//   - read side, dark behind translog_public (default OFF = 404):
//     GET /translog/checkpoint, GET /translog/tile/… (translog.NewHandler).
//     A SigV4-signed or presigned request on those paths is the S3 API's,
//     flag or not (a bucket named "translog" keeps working with a key).

// flagTranslogPublic opens the read side. Default OFF; global only — the
// owner flips it when trust.stored.cloud is decided (K4b).
const flagTranslogPublic = "translog_public"

// serverBuildTimeout bounds the boot entry (hash the binary + one append).
const serverBuildTimeout = time.Minute

// recordServerBuild appends the server_build entry of the binary at exe.
func recordServerBuild(ctx context.Context, store *logstore.Store, exe string) (int64, bool, error) {
	sum, err := fileSHA256(exe)
	if err != nil {
		logstore.CountAppendFailure(translog.KindServerBuild)
		return 0, false, fmt.Errorf("hash the executable: %w", err)
	}
	return store.Append(ctx, translog.ServerBuild{GitSHA: BuildSHA, BinarySHA256: sum, BuildTime: BuildDate, GoVersion: runtime.Version()})
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path) // #nosec G304 -- os.Executable(), our own binary
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// startServerBuildEntry runs recordServerBuild for this process in the
// background (never blocks the boot).
func (s *Server) startServerBuildEntry() {
	if s.translog == nil {
		return
	}
	logger := s.log()
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), serverBuildTimeout)
		defer cancel()
		exe, err := os.Executable()
		if err != nil {
			logstore.CountAppendFailure(translog.KindServerBuild)
			logger.Error("translog: server build not appended: no executable path", zap.Error(err))
			return
		}
		idx, appended, err := recordServerBuild(ctx, s.translog, exe)
		switch {
		case err != nil:
			logger.Error("translog: server build not appended", zap.Error(err), zap.String("git_sha", BuildSHA))
		case appended:
			logger.Info("translog: server build appended", zap.Int64("index", idx), zap.String("git_sha", BuildSHA))
		default:
			logger.Info("translog: server build already in the log", zap.Int64("index", idx), zap.String("git_sha", BuildSHA))
		}
	}()
}

// registerTranslogReadRoutes mounts the read side.
func (s *Server) registerTranslogReadRoutes() {
	if s.translog == nil {
		return
	}
	h := http.StripPrefix("/translog/", translog.NewHandler(s.translog))
	serve := func(w http.ResponseWriter, r *http.Request) {
		if signedS3Request(r) {
			s.s3Handler()(w, r)
			return
		}
		if s.flags == nil || !s.flags.Enabled(flagTranslogPublic, "") {
			http.NotFound(w, r)
			return
		}
		h.ServeHTTP(w, r)
	}
	s.router.Get("/translog/checkpoint", serve)
	s.router.Get("/translog/tile/*", serve)
}

// s3Handler is the S3 API's catch-all (a test hook replaces it).
func (s *Server) s3Handler() http.HandlerFunc {
	if s.s3Fallback != nil {
		return s.s3Fallback
	}
	return s.handleS3Request
}

// signedS3Request: the request carries S3 credentials (a header signature or
// a presigned query) — it is addressed to the S3 API.
func signedS3Request(r *http.Request) bool {
	if r.Header.Get("Authorization") != "" {
		return true
	}
	q := r.URL.Query()
	return q.Has("X-Amz-Signature") || q.Has("X-Amz-Credential") || q.Has("Signature") || q.Has("AWSAccessKeyId")
}

// registerTranslogAdminRoutes mounts the ceremony route on the admin group.
func (s *Server) registerTranslogAdminRoutes(r chi.Router) {
	r.Post("/translog/ceremony", s.requireAdmin(s.handleTranslogCeremony))
}

type translogCeremonyRequest struct {
	Ceremony       string `json:"ceremony"`
	KeyFingerprint string `json:"key_fingerprint"`
	Participants   int    `json:"participants"`
	Statement      string `json:"statement"`
	MFACode        string `json:"mfa_code"`
}

// handleTranslogCeremony is POST /api/v1/admin/translog/ceremony: appends a
// key_ceremony entry. 201 {index}; 400 a refused entry; 401 a wrong or
// reused code; 403 not an admin or no MFA enrolled; 503 the log or the MFA
// state cannot be read.
func (s *Server) handleTranslogCeremony(w http.ResponseWriter, r *http.Request) {
	actor, _ := r.Context().Value(userIDKey).(string)
	fail := func(status int, msg string, err error) {
		if err == nil {
			err = errors.New(msg)
		}
		audit.Record(r.Context(), s.db, audit.Entry{UserID: actor, EventType: "admin", Action: "admin.translog_ceremony", Error: err})
		writeJSON(w, status, map[string]string{"error": msg})
	}
	if s.translog == nil || s.auth == nil || s.mfaService == nil {
		fail(http.StatusServiceUnavailable, "the transparency log is not configured", nil)
		return
	}
	var req translogCeremonyRequest
	dec := json.NewDecoder(io.LimitReader(r.Body, 16<<10))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		fail(http.StatusBadRequest, "invalid JSON body", err)
		return
	}
	enabled, err := s.auth.IsMFAEnabled(r.Context(), actor)
	if err != nil {
		fail(http.StatusServiceUnavailable, "the MFA state cannot be read", err)
		return
	}
	if !enabled {
		fail(http.StatusForbidden, "enrol MFA on this admin account first: a ceremony entry needs a fresh code", nil)
		return
	}
	secret, err := s.auth.GetMFASecret(r.Context(), actor)
	if err != nil {
		fail(http.StatusServiceUnavailable, "the MFA state cannot be read", err)
		return
	}
	code := strings.TrimSpace(req.MFACode)
	if code == "" || !s.mfaService.ValidateCode(secret, code) || !s.auth.ConsumeTOTPCode(actor, code) {
		fail(http.StatusUnauthorized, "mfa_code: wrong, expired or already used", nil)
		return
	}
	entry := translog.KeyCeremony{Ceremony: req.Ceremony, KeyFingerprint: req.KeyFingerprint, Participants: req.Participants, Statement: req.Statement}
	if _, err := translog.Encode(entry, time.Now()); err != nil {
		fail(http.StatusBadRequest, err.Error(), err)
		return
	}
	idx, _, err := s.translog.Append(r.Context(), entry)
	if err != nil {
		s.log().Error("translog: ceremony entry not appended", zap.Error(err), zap.String("ceremony", req.Ceremony))
		fail(http.StatusServiceUnavailable, "the entry could not be appended; nothing was written", err)
		return
	}
	audit.Record(r.Context(), s.db, audit.Entry{UserID: actor, EventType: "admin", Action: "admin.translog_ceremony",
		Metadata: map[string]any{"index": idx, "ceremony": req.Ceremony, "key_fingerprint": req.KeyFingerprint, "participants": req.Participants}})
	s.log().Info("translog: ceremony entry appended", zap.Int64("index", idx), zap.String("ceremony", req.Ceremony))
	writeJSON(w, http.StatusCreated, map[string]any{"index": idx, "kind": translog.KindKeyCeremony})
}
