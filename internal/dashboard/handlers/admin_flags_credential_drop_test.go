package handlers

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/FairForge/vaultaire/internal/auth"
	"github.com/FairForge/vaultaire/internal/flags"
	"github.com/stretchr/testify/assert"
)

// credential_plaintext_drop's refusals (plan 43.7 PR C) reach the dashboard
// as the flags service wraps them: 409 with the reason, nothing written.
func TestFlagRefused_CredentialPlaintextDrop(t *testing.T) {
	for code, refusal := range map[string]*auth.PlaintextDropRefusal{
		"master_key_absent":       {Code: "master_key_absent"},
		"credentials_not_wrapped": {Code: "credentials_not_wrapped", Unwrapped: 3},
	} {
		rr := httptest.NewRecorder()
		assert.True(t, flagRefused(rr, fmt.Errorf("%w: %w", flags.ErrChangeRefused, refusal)), code)
		assert.Equal(t, http.StatusConflict, rr.Code, code)
		assert.Contains(t, rr.Body.String(), code)
		assert.Equal(t, "no-store", rr.Header().Get("Cache-Control"))
	}
	rr := httptest.NewRecorder()
	assert.True(t, flagRefused(rr, fmt.Errorf("%w: %w", flags.ErrChangeRefused, &auth.PlaintextDropRefusal{Code: "credentials_not_wrapped", Unwrapped: 3})))
	assert.Contains(t, rr.Body.String(), "3 live S3 secret(s)")
}
