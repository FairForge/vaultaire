package auth

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Review R12: the password floor is enforced at the service chokepoint so
// the JSON API cannot accept what the web form refuses.
func TestCreateUserWithTenant_RejectsShortPassword(t *testing.T) {
	svc := NewAuthService(nil, nil)
	_, _, _, err := svc.CreateUserWithTenant(context.Background(), "short@stored.ge", "abc", "x")
	require.ErrorIs(t, err, ErrPasswordTooShort)

	// OAuth accounts have no password at all and are still allowed.
	_, _, _, err = svc.CreateUserWithTenant(context.Background(), "oauth@stored.ge", "", "x")
	require.NoError(t, err)
}

// R5-15c: a TOTP code is single-use inside its step (RFC 6238 §5.2).
func TestConsumeTOTPCode_RejectsReplay(t *testing.T) {
	svc := NewAuthService(nil, nil)
	assert.True(t, svc.ConsumeTOTPCode("u1", "123456"), "first use is fresh")
	assert.False(t, svc.ConsumeTOTPCode("u1", "123456"), "same code again is a replay")
	assert.True(t, svc.ConsumeTOTPCode("u1", "654321"), "a different code is fresh")
	assert.True(t, svc.ConsumeTOTPCode("u2", "123456"), "another user's identical digits are unrelated")

	// After the window the digits may legitimately recur.
	svc.mfaMu.Lock()
	svc.totpUsed["u1"] = totpUse{code: "654321", at: time.Now().Add(-totpReplayWindow - time.Second)}
	svc.mfaMu.Unlock()
	assert.True(t, svc.ConsumeTOTPCode("u1", "654321"))
}
