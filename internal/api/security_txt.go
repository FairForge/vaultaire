package api

import (
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/FairForge/vaultaire/internal/hosts"
)

// /.well-known/security.txt (RFC 9116) — launch sequence 5.5.6: researchers
// get a disclosure address before they post to LET. Served from the binary,
// registered before the S3 catch-all (it used to fall through to the S3
// handler and answer 403 AccessDenied).
//
// Contact, Canonical and the Policy default come from the one host source
// (internal/hosts, WP-DOMAIN-1): security@<canonical domain> and the
// canonical origin — stored.cloud on prod. The Expires field is required by the
// RFC and must be under a year out; it is computed once per process start
// (start of that UTC day + 364 days), so a deploy refreshes it and a binary
// that somehow runs for a year serves a stale-but-not-lying date (the day it
// was computed is what the RFC is really after — the file must be re-checked
// at least yearly). Policy defaults to the AUP (which already names the
// abuse/CSAM/DMCA process) and can be pointed at a dedicated disclosure page
// via SECURITY_POLICY_URL once one exists.

const securityTxtValidity = 364 * 24 * time.Hour

// securityTxtBody renders the file for a given generation time, host set and
// policy URL. Pure so the test can pin the time.
func securityTxtBody(now time.Time, h hosts.Hosts, policyURL string) string {
	day := now.UTC().Truncate(24 * time.Hour)
	expires := day.Add(securityTxtValidity).Format(time.RFC3339)
	var b strings.Builder
	fmt.Fprintf(&b, "Contact: mailto:%s\n", h.SecurityEmail())
	fmt.Fprintf(&b, "Expires: %s\n", expires)
	fmt.Fprintf(&b, "Preferred-Languages: en\n")
	fmt.Fprintf(&b, "Canonical: %s\n", h.URL("/.well-known/security.txt"))
	if policyURL != "" {
		fmt.Fprintf(&b, "Policy: %s\n", policyURL)
	}
	return b.String()
}

// initSecurityTxt renders the body once at boot.
func (s *Server) initSecurityTxt() {
	policy := os.Getenv("SECURITY_POLICY_URL")
	if policy == "" {
		policy = s.hostSet().URL("/legal/aup")
	}
	s.securityTxt = securityTxtBody(time.Now(), s.hostSet(), policy)
}

func (s *Server) handleSecurityTxt(w http.ResponseWriter, r *http.Request) {
	if s.securityTxt == "" {
		s.initSecurityTxt()
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=86400")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if r.Method == http.MethodHead {
		w.Header().Set("Content-Length", fmt.Sprintf("%d", len(s.securityTxt)))
		w.WriteHeader(http.StatusOK)
		return
	}
	_, _ = w.Write([]byte(s.securityTxt))
}
