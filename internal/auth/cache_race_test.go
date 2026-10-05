package auth

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// WP-R5-6: the in-process credential maps (users, userIndex, tenants,
// apiKeys, keyIndex, profiles, preferences, mfaSettings) are read on every
// dashboard login, API-key check and JWT validation and written by
// registration, key rotation/revocation, eviction and the settings forms.
// Go aborts the process on a concurrent map write it detects; the race
// detector (`make test` runs -race) catches the ones it does not. This test
// is the proof the WP asked for: every reader runs while keys are rotated,
// revoked and created, accounts registered and evicted, and settings written.
func TestAuthService_CredentialCacheIsRaceFree(t *testing.T) {
	svc := NewAuthService(nil, nil)
	svc.SetVerifySecret("verify-secret")
	ctx := context.Background()
	user, tenant, primary, err := svc.CreateUserWithTenant(ctx, "race@stored.ge", "a-long-password", "Race Co")
	require.NoError(t, err)
	jwtToken, err := svc.GenerateJWT(user)
	require.NoError(t, err)
	require.NoError(t, svc.EnableMFA(ctx, user.ID, "JBSWY3DPEHPK3PXP", []string{"CODE1"}))

	// Keys the writers rotate and revoke while the readers look them up.
	const nKeys = 8
	keys := make([]*APIKey, nKeys)
	for i := range keys {
		keys[i], err = svc.GenerateAPIKey(ctx, user.ID, fmt.Sprintf("k%d", i), &KeyCreateOptions{Permissions: []string{OpGetObject}})
		require.NoError(t, err)
	}

	const rounds = 60
	var wg sync.WaitGroup
	stop := make(chan struct{})
	reader := func(f func()) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					f()
				}
			}
		}()
	}

	// Readers: every lookup the request path and the dashboard make.
	reader(func() { _, _ = svc.ValidateS3Request(ctx, primary.Key) })
	reader(func() { _, _ = svc.ValidateS3Request(ctx, keys[0].Key) })
	reader(func() { _, _ = svc.ValidateAPIKey(ctx, keys[1].Key, keys[1].Secret) })
	reader(func() { _, _ = svc.ListAPIKeys(ctx, user.ID) })
	reader(func() { _, _ = svc.GetPrimaryAPIKey(ctx, tenant.ID) })
	reader(func() { _, _ = svc.GetOwnedAPIKey(ctx, user.ID, keys[2].ID) })
	reader(func() { _, _ = svc.GetUserByID(ctx, user.ID) })
	reader(func() { _, _ = svc.GetUserByEmail(ctx, user.Email) })
	reader(func() { _ = svc.GetUserIDByTenantID(ctx, tenant.ID) })
	reader(func() { _, _ = svc.ValidateJWT(jwtToken) })
	reader(func() { _, _ = svc.ValidatePassword(ctx, "nobody@stored.ge", "wrong") }) // the lookup, not bcrypt
	reader(func() { _, _ = svc.GetUserProfile(ctx, user.ID) })
	reader(func() { _, _ = svc.GetUserPreferences(ctx, user.ID) })
	reader(func() { _ = svc.IsEmailVerified(ctx, user.ID) })
	reader(func() { _, _ = svc.GenerateEmailVerifyToken(ctx, user.ID) })
	reader(func() { _, _ = svc.IsMFAEnabled(ctx, user.ID) })
	reader(func() { _, _ = svc.GetMFASecret(ctx, user.ID) })
	reader(func() { _, _ = svc.ValidateBackupCode(ctx, user.ID, "nope") })

	// Writers.
	var ww sync.WaitGroup
	writer := func(f func(i int)) {
		ww.Add(1)
		go func() {
			defer ww.Done()
			for i := 0; i < rounds; i++ {
				f(i)
			}
		}()
	}
	writer(func(i int) { // rotate a key and keep rotating its successor
		k := keys[3]
		if nk, err := svc.RotateAPIKey(ctx, user.ID, k.ID); err == nil {
			keys[3] = nk
		}
	})
	writer(func(i int) { // revoke fresh keys
		if k, err := svc.GenerateAPIKey(ctx, user.ID, "tmp", nil); err == nil {
			_ = svc.RevokeAPIKey(ctx, user.ID, k.ID)
		}
	})
	writer(func(i int) { // set expiry on a live key
		_ = svc.SetAPIKeyExpiration(ctx, user.ID, keys[4].ID, time.Now().Add(time.Hour))
	})
	writer(func(i int) { // registrations and evictions of other accounts (bcrypt: a few are enough)
		if i%10 != 0 {
			return
		}
		u, tn, _, err := svc.CreateUserFromOAuth(ctx, fmt.Sprintf("race-%d@stored.ge", i), "x", "google", fmt.Sprintf("g-%d", i))
		if err == nil && i%20 == 0 {
			svc.Evict(u.ID, tn.ID)
		}
	})
	writer(func(i int) { // the settings forms
		_ = svc.SetUserPreferences(ctx, user.ID, UserPreferences{Theme: "light"})
		_ = svc.UpdateUserProfile(ctx, user.ID, ProfileUpdate{DisplayName: fmt.Sprintf("n%d", i)})
	})
	writer(func(i int) { // the second factor's replay guard and codes
		svc.ConsumeTOTPCode(user.ID, fmt.Sprintf("%06d", i))
		if i%100 == 0 {
			_ = svc.RegenerateBackupCodes(ctx, user.ID, []string{"A"})
		}
	})
	writer(func(i int) { // email verification completes
		if i%40 == 0 {
			if tok, err := svc.GenerateEmailVerifyToken(ctx, user.ID); err == nil {
				_ = svc.VerifyEmail(ctx, tok)
			}
		}
	})

	ww.Wait()
	close(stop)
	wg.Wait()

	// The service is still coherent: the primary pair authenticates, the
	// rotated key's latest successor is the live one, the others are dead.
	tn, err := svc.ValidateS3Request(ctx, primary.Key)
	require.NoError(t, err)
	require.Equal(t, tenant.ID, tn.ID)
	live, err := svc.GetOwnedAPIKey(ctx, user.ID, keys[3].ID)
	require.NoError(t, err)
	require.Nil(t, live.RevokedAt)
	all, err := svc.ListAPIKeys(ctx, user.ID)
	require.NoError(t, err)
	var revoked int
	for _, k := range all {
		if k.RevokedAt != nil {
			revoked++
		}
	}
	require.GreaterOrEqual(t, revoked, rounds, "every rotation and revocation left a revoked row")
}
