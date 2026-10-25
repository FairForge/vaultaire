package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/FairForge/vaultaire/internal/auth"
)

// Stage 1a Prompt 41 item 3 (RP58-1, RP58-8 c): the S3 credential gate's
// predicate and the verifier's format check must agree — every
// Authorization value auth.Auth.ValidateRequest would attempt to parse as
// an AWS scheme is one the gate sends to the S3 API, and SigV4A
// (AWS4-ECDSA-P256-SHA256, the SDKs' multi-region scheme) is S3 credentials
// the gate catches although the verifier refuses it. Before: the verifier's
// bare HasPrefix attempted "AWS4-HMAC-SHA256,Credential=…" and a tab after
// the scheme, which the gate's space-Cut missed — a request that reached a
// web route; and a SigV4A-signed GET of stored.ge/legal/privacy got the
// page. The fix narrows the verifier to scheme + space (RFC 9110 § 11.4;
// every SDK sends it) and gives both sides ONE predicate,
// auth.IsAWSAuthorization.

var sigV4AHeader = "AWS4-ECDSA-P256-SHA256 Credential=AKIDNOPE/20261025/*/s3/aws4_request, SignedHeaders=host;x-amz-date;x-amz-region-set, Signature=3045022100ab"

func TestS3Gate_EveryHeaderTheVerifierAttemptsIsTheGates(t *testing.T) {
	schemes := []string{"AWS4-HMAC-SHA256", "aws4-hmac-sha256", "Aws4-Hmac-Sha256", "AWS4-ECDSA-P256-SHA256", "aws4-ecdsa-p256-sha256", "AWS", "aws", "AWSX", "AWS4-HMAC-SHA256X", "Bearer", "Basic", ""}
	seps := []string{" ", ",", "\t", "", "  ", " \t"}
	payloads := []string{"Credential=AKIDNOPE/20261025/us-east-1/s3/aws4_request, SignedHeaders=host, Signature=00", "AKIDNOPE:c2ln", "", "x"}
	n := 0
	for _, scheme := range schemes {
		for _, sep := range seps {
			for _, payload := range payloads {
				h := scheme + sep + payload
				req := httptest.NewRequest(http.MethodGet, "/legal/privacy", nil)
				req.Header.Set("Authorization", h)
				gate := signedS3Request(req)
				if auth.AttemptsAWSHeader(h) {
					assert.True(t, gate, "%q: the verifier attempts it, so the gate must send it to the S3 API", h)
				}
				assert.Equal(t, auth.IsAWSAuthorization(h), gate, "%q: the gate's header half IS auth's predicate", h)
				n++
			}
		}
	}
	require.Greater(t, n, 200)
	// The shapes the review named, by name.
	for _, tc := range []struct {
		h        string
		verifier bool
		gate     bool
	}{
		{"AWS4-HMAC-SHA256 Credential=…", true, true},
		{"AWS4-HMAC-SHA256,Credential=…", false, false}, // the verifier no longer attempts it (RFC 9110: scheme SP params)
		{"AWS4-HMAC-SHA256\tCredential=…", false, false},
		{"AWS4-HMAC-SHA256Credential=…", false, false},
		{"AWS AKIDNOPE:c2ln", true, true},
		{"AWS4-ECDSA-P256-SHA256 Credential=…", false, true}, // SigV4A: refused by the verifier, caught by the gate
		{"aws4-ecdsa-p256-sha256 Credential=…", false, true},
		{"AWS4-ECDSA-P256-SHA256", false, false}, // no params: no credentials
		{"Bearer eyJ", false, false},
	} {
		assert.Equal(t, tc.verifier, auth.AttemptsAWSHeader(tc.h), "verifier: %q", tc.h)
		assert.Equal(t, tc.gate, auth.IsAWSAuthorization(tc.h), "gate: %q", tc.h)
	}
}

// A SigV4A-signed request is the S3 API's on every route (the matrix test
// adds the header kind too), and the S3 API answers it with its own clean
// refusal — a 4xx XML document, never a page and never a panic.
func TestS3Gate_ASigV4ARequestIsTheS3APIsCleanRefusal(t *testing.T) {
	srv, hits := gateServer(t, prodTodayHosts)
	for _, path := range []string{"/legal/privacy", "/docs", "/", "/dashboard/", "/status"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Host = "stored.ge"
		req.Header.Set("Authorization", sigV4AHeader)
		*hits = nil
		serve(srv.httpServer.Handler, req)
		assert.Equal(t, []string{"GET stored.ge" + path}, *hits, "%s: a SigV4A header is S3 credentials", path)
	}

	// The real S3 handler's answer to it.
	req := httptest.NewRequest(http.MethodGet, "/legal/privacy", nil)
	req.Host = "stored.ge"
	req.Header.Set("Authorization", sigV4AHeader)
	req.Header.Set("X-Amz-Date", "20261025T000000Z")
	w := httptest.NewRecorder()
	srv.handleS3Request(w, req)
	assert.Equal(t, http.StatusForbidden, w.Code)
	assert.Contains(t, w.Header().Get("Content-Type"), "xml")
	assert.Contains(t, w.Body.String(), "<Code>SignatureDoesNotMatch</Code>", "the verifier does not support SigV4A: a signature refusal, in S3's shape")
	assert.False(t, strings.Contains(w.Body.String(), "<html"), "never a page")
}
