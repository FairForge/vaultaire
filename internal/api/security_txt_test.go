package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/FairForge/vaultaire/internal/hosts"
)

// 5.5.6: /.well-known/security.txt used to fall through to the S3 catch-all
// and answer 403 AccessDenied (observed on prod 2026-09-23). It must be a
// real RFC 9116 file on the production router.

func TestSecurityTxt_ServedBeforeS3CatchAll(t *testing.T) {
	srv := newTestServerForTimeouts(t) // full production router, nil DB

	rr := httptest.NewRecorder()
	srv.router.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/.well-known/security.txt", nil))

	require.Equal(t, http.StatusOK, rr.Code)
	assert.Equal(t, "text/plain; charset=utf-8", rr.Header().Get("Content-Type"))
	body := rr.Body.String()
	assert.Contains(t, body, "Contact: mailto:"+srv.hosts.SecurityEmail()+"\n")
	assert.Contains(t, body, "Canonical: "+srv.hosts.Canonical+"/.well-known/security.txt\n")
	assert.Contains(t, body, "Policy: "+srv.hosts.Canonical+"/legal/aup\n")
	assert.Contains(t, body, "Preferred-Languages: en\n")
	assert.NotContains(t, body, "<Error>", "must not be an S3 error document")

	// HEAD works too (RFC 9116 clients and uptime checks use it).
	rr = httptest.NewRecorder()
	srv.router.ServeHTTP(rr, httptest.NewRequest(http.MethodHead, "/.well-known/security.txt", nil))
	require.Equal(t, http.StatusOK, rr.Code)
	assert.Empty(t, rr.Body.String())
}

func TestSecurityTxtBody_ExpiresUnderOneYearAndParses(t *testing.T) {
	now := time.Date(2026, 9, 24, 13, 45, 0, 0, time.UTC)
	body := securityTxtBody(now, prodHosts(), "https://stored.cloud/legal/aup")

	var expiresLine string
	for _, l := range strings.Split(body, "\n") {
		if strings.HasPrefix(l, "Expires: ") {
			expiresLine = strings.TrimPrefix(l, "Expires: ")
		}
	}
	require.NotEmpty(t, expiresLine, "Expires is mandatory in RFC 9116")
	exp, err := time.Parse(time.RFC3339, expiresLine)
	require.NoError(t, err)
	assert.True(t, exp.After(now), "must be in the future")
	assert.True(t, exp.Before(now.Add(366*24*time.Hour)), "RFC 9116: Expires must be less than a year out")
	assert.Equal(t, "2027-09-23T00:00:00Z", expiresLine, "day-truncated so the body is stable within a day")
	assert.Contains(t, body, "Policy: https://stored.cloud/legal/aup\n")

	assert.NotContains(t, securityTxtBody(now, prodHosts(), ""), "Policy:")
}

// TestSecurityTxt_CanonicalHostAndContact: Contact, Canonical and the Policy
// default follow the host source (WP-DOMAIN-1) — no domain is typed here.
func TestSecurityTxt_CanonicalHostAndContact(t *testing.T) {
	now := time.Date(2026, 10, 12, 0, 0, 0, 0, time.UTC)
	body := securityTxtBody(now, prodHosts(), "")
	assert.Contains(t, body, "Contact: mailto:security@stored.cloud\n")
	assert.Contains(t, body, "Canonical: https://stored.cloud/.well-known/security.txt\n")
	assert.NotContains(t, body, "stored.ge")

	// The policy default is the canonical origin's AUP; SECURITY_POLICY_URL wins.
	s := &Server{hosts: prodHosts()}
	t.Setenv("SECURITY_POLICY_URL", "")
	s.initSecurityTxt()
	assert.Contains(t, s.securityTxt, "Policy: https://stored.cloud/legal/aup\n")
	t.Setenv("SECURITY_POLICY_URL", "https://stored.cloud/security")
	s.initSecurityTxt()
	assert.Contains(t, s.securityTxt, "Policy: https://stored.cloud/security\n")

	// A development origin still names a real mailbox, never one at localhost.
	dev := securityTxtBody(now, hosts.Defaults(8000), "")
	assert.Contains(t, dev, "Contact: mailto:security@stored.cloud\n")
}
