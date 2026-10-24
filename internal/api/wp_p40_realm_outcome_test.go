package api

import (
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/FairForge/vaultaire/internal/recovery"
	"github.com/FairForge/vaultaire/internal/recovery/client"
)

// RP56-6 (the WP-P39 review, row 2): HTTPTransport.post wrapped a failure of
// its own Token() as ErrInvalidAuth, so realmOutcome counted OUR mint
// failure as the realm's invalid_auth — a page for a realm that was never
// asked. It is ours, like a deadline of our own: no realm outcome ("", counted
// as nothing); the tenant is still deferred with the reason.
func TestRealmOutcome_OurTokenMintFailureIsNotTheRealms(t *testing.T) {
	var asked atomic.Int32
	realm := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		asked.Add(1)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	t.Cleanup(realm.Close)
	var id recovery.RealmID
	id[0] = 0x40
	key, err := ecdh.X25519().GenerateKey(rand.Reader)
	require.NoError(t, err)
	cr := client.Realm{ID: id, Address: realm.URL, PublicKey: key.PublicKey().Bytes()}
	mintErr := errors.New("sign the realm token: no key")
	tr := &client.HTTPTransport{HTTP: realm.Client(), Token: func(context.Context, client.Realm) (string, error) { return "", mintErr }}

	ctx := context.Background()
	err = client.Delete(ctx, client.Config{Realms: []client.Realm{cr}, RegisterThreshold: 1, RecoverThreshold: 1, Transport: tr})
	require.Error(t, err)
	assert.ErrorIs(t, err, client.ErrTokenMint)
	assert.ErrorIs(t, err, mintErr)
	assert.Zero(t, asked.Load(), "nothing was sent: the token was never minted")
	assert.Equal(t, "", realmOutcome(ctx, err), "our own failure, never the realm's invalid_auth")

	// The realm's own refusal stays invalid_auth.
	tr.Token = func(context.Context, client.Realm) (string, error) { return "tok", nil }
	err = client.Delete(ctx, client.Config{Realms: []client.Realm{cr}, RegisterThreshold: 1, RecoverThreshold: 1, Transport: tr})
	require.Error(t, err)
	assert.NotErrorIs(t, err, client.ErrTokenMint)
	assert.Equal(t, realmInvalidAuth, realmOutcome(ctx, err), "a 401 from the realm: %v", err)
}
