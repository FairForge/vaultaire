package auth

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.uber.org/zap"
)

// Stage 1a Prompt 41 item 3 (RP58-8 c): the verifier attempts a SigV4 header
// only when the scheme is followed by a space — "AWS4-HMAC-SHA256,Credential=…"
// used to pass its HasPrefix and reach the parser while the S3 credential
// gate did not see it as credentials. Both answer the same question now.
func TestValidateRequest_TheSchemeNeedsItsSpace(t *testing.T) {
	a := NewAuth(nil, zap.NewNop())
	for _, tc := range []struct {
		header  string
		wantErr string
	}{
		{"AWS4-HMAC-SHA256,Credential=AKID/20261025/us-east-1/s3/aws4_request, SignedHeaders=host, Signature=00", "invalid authorization format"},
		{"AWS4-HMAC-SHA256\tCredential=AKID/20261025/us-east-1/s3/aws4_request", "invalid authorization format"},
		{"AWS4-HMAC-SHA256Credential=AKID", "invalid authorization format"},
		{"AWS4-ECDSA-P256-SHA256 Credential=AKID/20261025/*/s3/aws4_request, SignedHeaders=host, Signature=00", "invalid authorization format"},
		{"AWS AKID:c2ln", "AWS signature version 2 is not supported"},
	} {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.Header.Set("Authorization", tc.header)
		_, _, err := a.ValidateRequest(req)
		require.Error(t, err, tc.header)
		assert.Contains(t, err.Error(), tc.wantErr, tc.header)
		assert.Equal(t, tc.wantErr == "AWS signature version 2 is not supported", AttemptsAWSHeader(tc.header), "AttemptsAWSHeader agrees with the verifier: %q", tc.header)
	}
	// The space-separated header is attempted (and, with no database, is
	// the test tenant).
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential=AKID/20261025/us-east-1/s3/aws4_request, SignedHeaders=host, Signature=00")
	tenant, _, err := a.ValidateRequest(req)
	require.NoError(t, err)
	assert.Equal(t, "test-tenant", tenant)
	assert.True(t, AttemptsAWSHeader(req.Header.Get("Authorization")))
	assert.Equal(t, "AKID", HeaderAccessKey(req))
}

func TestIsAWSAuthorization(t *testing.T) {
	for h, want := range map[string]bool{
		"AWS4-HMAC-SHA256 Credential=x":       true,
		"aws4-hmac-sha256 Credential=x":       true,
		"AWS4-ECDSA-P256-SHA256 Credential=x": true,
		"AWS AKID:sig":                        true,
		"AWS ":                                true,  // the verifier attempts it (SigV2, refused) — so does the gate
		"  AWS4-HMAC-SHA256 Credential=x":     false, // not what the verifier attempts: net/http trims a wire header's OWS before either sees it
		"AWS4-HMAC-SHA256,Credential=x":       false,
		"AWS4-HMAC-SHA256":                    false,
		"AWS4-ECDSA-P256-SHA256":              false,
		"AWSX foo":                            false,
		"Bearer eyJ":                          false,
		"Basic dTpw":                          false,
		"":                                    false,
	} {
		assert.Equal(t, want, IsAWSAuthorization(h), "%q", h)
	}
}
