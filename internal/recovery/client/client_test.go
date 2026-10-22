package client_test

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/FairForge/vaultaire/internal/recovery"
	"github.com/FairForge/vaultaire/internal/recovery/client"
	"github.com/FairForge/vaultaire/internal/recovery/realm"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// inProc is a transport straight into realm.Realm values; every message
// goes through its wire encoding (the padded form for answers), as it
// would over the network.
type inProc struct {
	mu     sync.Mutex
	realms map[recovery.RealmID]*realm.Realm
	stores map[recovery.RealmID]*realm.MemoryStore
	down   map[recovery.RealmID]bool
	auth   realm.Auth
	tamper func(recovery.RealmID, *recovery.SecretsResponse)
}

func newInProc(n int) (*inProc, []client.Realm) {
	p := &inProc{
		realms: map[recovery.RealmID]*realm.Realm{}, stores: map[recovery.RealmID]*realm.MemoryStore{},
		down: map[recovery.RealmID]bool{}, auth: realm.Auth{Tenant: "stored", Subject: "opaque-sub"},
	}
	var cfg []client.Realm
	for i := n; i >= 1; i-- { // configured out of id order on purpose: the client sorts
		id := recovery.RealmID{byte(i), byte(i)}
		st := realm.NewMemoryStore()
		p.stores[id] = st
		p.realms[id] = &realm.Realm{Store: st}
		cfg = append(cfg, client.Realm{ID: id})
	}
	return p, cfg
}

func (p *inProc) setDown(id recovery.RealmID, down bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.down[id] = down
}

func (p *inProc) Send(ctx context.Context, r client.Realm, req recovery.SecretsRequest) (recovery.SecretsResponse, error) {
	p.mu.Lock()
	down, tamper := p.down[r.ID], p.tamper
	p.mu.Unlock()
	if down {
		return recovery.SecretsResponse{}, errors.New("connection refused")
	}
	b, err := req.MarshalCBOR()
	if err != nil {
		return recovery.SecretsResponse{}, err
	}
	onWire, err := recovery.UnmarshalSecretsRequest(b)
	if err != nil {
		return recovery.SecretsResponse{}, err
	}
	resp, err := p.realms[r.ID].Handle(ctx, p.auth, onWire)
	if err != nil {
		return recovery.SecretsResponse{}, err
	}
	pb, err := resp.MarshalPadded()
	if err != nil {
		return recovery.SecretsResponse{}, err
	}
	back, err := recovery.UnmarshalPadded(pb)
	if tamper != nil {
		tamper(r.ID, &back)
	}
	return back, err
}

func (p *inProc) record(t *testing.T, id recovery.RealmID) realm.Record {
	t.Helper()
	rec, _, err := p.stores[id].Load(context.Background(), p.auth.Tenant, p.auth.Subject)
	require.NoError(t, err)
	return rec
}

func cfgFor(p *inProc, realms []client.Realm, register, recoverT int) client.Config {
	return client.Config{Realms: realms, RegisterThreshold: register, RecoverThreshold: recoverT,
		PINHashing: recovery.FastInsecure, Transport: p}
}

var (
	pin       = []byte("2580")
	wrongPin  = []byte("0000")
	userInfo  = []byte("opaque-sub")
	theSecret = []byte("a K2 recovery key: opaque bytes, at most 128")
)

func TestRecovery_RegisterRecover_ThresholdOfN(t *testing.T) {
	// Arrange: 3 realms, recover threshold 2.
	ctx := context.Background()
	p, realms := newInProc(3)
	cfg := cfgFor(p, realms, 3, 2)
	require.NoError(t, client.Register(ctx, cfg, pin, theSecret, userInfo, 10))
	for _, r := range realms {
		rec := p.record(t, r.ID)
		assert.Equal(t, realm.Registered, rec.State)
		assert.NotContains(t, string(rec.EncryptedSecret[:]), string(theSecret), "a realm holds ciphertext only")
	}

	// Act + Assert: all up, then any one realm down.
	got, err := client.Recover(ctx, cfg, pin, userInfo)
	require.NoError(t, err)
	assert.Equal(t, theSecret, got)
	for _, r := range realms {
		p.setDown(r.ID, true)
		got, err := client.Recover(ctx, cfg, pin, userInfo)
		require.NoError(t, err, "realm %s down", r.ID)
		assert.Equal(t, theSecret, got)
		p.setDown(r.ID, false)
	}

	// Two of three down: below the threshold, and it says so.
	p.setDown(realms[0].ID, true)
	p.setDown(realms[1].ID, true)
	_, err = client.Recover(ctx, cfg, pin, userInfo)
	assert.ErrorIs(t, err, client.ErrTransient)
	p.setDown(realms[0].ID, false)
	p.setDown(realms[1].ID, false)

	// Delete: nothing left to recover.
	require.NoError(t, client.Delete(ctx, cfg))
	_, err = client.Recover(ctx, cfg, pin, userInfo)
	assert.ErrorIs(t, err, client.ErrNotRegistered)
}

func TestRecovery_WrongPin_CountsPerRealm_DestroyAtLimit(t *testing.T) {
	ctx := context.Background()
	p, realms := newInProc(3)
	cfg := cfgFor(p, realms, 3, 2)
	require.NoError(t, client.Register(ctx, cfg, pin, theSecret, userInfo, 10))
	ids := []recovery.RealmID{{1, 1}, {2, 2}, {3, 3}}

	// Two wrong guesses while realm 3 is down: realms count on their own.
	p.setDown(ids[2], true)
	for i := 1; i <= 2; i++ {
		_, err := client.Recover(ctx, cfg, wrongPin, userInfo)
		var bad client.ErrInvalidPin
		require.ErrorAs(t, err, &bad)
		assert.Equal(t, uint16(10-i), bad.GuessesRemaining)
	}
	assert.Equal(t, uint16(2), p.record(t, ids[0]).AttemptedGuesses)
	assert.Equal(t, uint16(2), p.record(t, ids[1]).AttemptedGuesses)
	assert.Equal(t, uint16(0), p.record(t, ids[2]).AttemptedGuesses, "a realm that saw no guess counted none")
	p.setDown(ids[2], false)

	// Eight more: realms 1 and 2 reach their limit; realm 3 is at 8.
	var last client.ErrInvalidPin
	for i := 0; i < 8; i++ {
		_, err := client.Recover(ctx, cfg, wrongPin, userInfo)
		require.ErrorAs(t, err, &last)
	}
	assert.Equal(t, uint16(0), last.GuessesRemaining, "the least any realm allows")
	assert.Equal(t, uint16(10), p.record(t, ids[0]).AttemptedGuesses)
	assert.Equal(t, uint16(8), p.record(t, ids[2]).AttemptedGuesses)

	// The right PIN now: two realms self-destruct at Recover1 — below the
	// threshold for ever.
	_, err := client.Recover(ctx, cfg, pin, userInfo)
	assert.ErrorIs(t, err, client.ErrNoGuesses)
	for _, id := range ids[:2] {
		rec := p.record(t, id)
		assert.Equal(t, realm.NoGuesses, rec.State)
		assert.False(t, rec.HoldsSecretMaterial(), "realm %s: erased, not flagged — no share, key, tag or ciphertext", id)
		assert.Equal(t, [recovery.EncryptedSecretSize]byte{}, rec.EncryptedSecret)
		assert.Equal(t, [32]byte{}, rec.OPRFPrivateKey)
	}
	_, err = client.Recover(ctx, cfg, pin, userInfo)
	assert.ErrorIs(t, err, client.ErrNoGuesses, "a correct PIN after destruction")
	assert.True(t, p.record(t, ids[2]).HoldsSecretMaterial(), "realm 3 has guesses left: one realm alone recovers nothing")
}

func TestRecovery_LastGuessIsUsable(t *testing.T) {
	ctx := context.Background()
	p, realms := newInProc(2)
	cfg := cfgFor(p, realms, 2, 2)
	require.NoError(t, client.Register(ctx, cfg, pin, theSecret, userInfo, 2))
	_, err := client.Recover(ctx, cfg, wrongPin, userInfo)
	require.ErrorAs(t, err, &client.ErrInvalidPin{})
	got, err := client.Recover(ctx, cfg, pin, userInfo) // the second of two guesses
	require.NoError(t, err)
	assert.Equal(t, theSecret, got)
}

func TestRecovery_CorrectRecover3_ResetsCounter(t *testing.T) {
	ctx := context.Background()
	p, realms := newInProc(2)
	cfg := cfgFor(p, realms, 2, 2)
	require.NoError(t, client.Register(ctx, cfg, pin, theSecret, userInfo, 10))
	for i := 0; i < 3; i++ {
		_, err := client.Recover(ctx, cfg, wrongPin, userInfo)
		require.ErrorAs(t, err, &client.ErrInvalidPin{})
	}
	for _, r := range realms {
		require.Equal(t, uint16(3), p.record(t, r.ID).AttemptedGuesses)
	}

	got, err := client.Recover(ctx, cfg, pin, userInfo)
	require.NoError(t, err)
	assert.Equal(t, theSecret, got)
	for _, r := range realms {
		assert.Equal(t, uint16(0), p.record(t, r.ID).AttemptedGuesses, "the proven PIN resets the count")
	}
	_, err = client.Recover(ctx, cfg, wrongPin, userInfo)
	var bad client.ErrInvalidPin
	require.ErrorAs(t, err, &bad)
	assert.Equal(t, uint16(9), bad.GuessesRemaining)
}

func TestRecovery_DLEQ_BadProofRejected(t *testing.T) {
	ctx := context.Background()
	flip := func(target recovery.RealmID) func(recovery.RealmID, *recovery.SecretsResponse) {
		return func(id recovery.RealmID, r *recovery.SecretsResponse) {
			if id == target && r.Recover2 != nil {
				r.Recover2.Proof.BetaZ[0] ^= 1
			}
		}
	}

	t.Run("2 of 2: one bad proof stops recovery", func(t *testing.T) {
		p, realms := newInProc(2)
		cfg := cfgFor(p, realms, 2, 2)
		require.NoError(t, client.Register(ctx, cfg, pin, theSecret, userInfo, 10))
		p.tamper = flip(recovery.RealmID{1, 1})
		_, err := client.Recover(ctx, cfg, pin, userInfo)
		assert.ErrorIs(t, err, client.ErrMisbehavingRealm)
	})

	t.Run("2 of 3: the bad realm is left out", func(t *testing.T) {
		p, realms := newInProc(3)
		cfg := cfgFor(p, realms, 3, 2)
		require.NoError(t, client.Register(ctx, cfg, pin, theSecret, userInfo, 10))
		p.tamper = flip(recovery.RealmID{2, 2})
		got, err := client.Recover(ctx, cfg, pin, userInfo)
		require.NoError(t, err)
		assert.Equal(t, theSecret, got)
	})

	t.Run("a share evaluated with another key", func(t *testing.T) {
		p, realms := newInProc(2)
		cfg := cfgFor(p, realms, 2, 2)
		require.NoError(t, client.Register(ctx, cfg, pin, theSecret, userInfo, 10))
		p.tamper = func(id recovery.RealmID, r *recovery.SecretsResponse) {
			if id == (recovery.RealmID{2, 2}) && r.Recover2 != nil {
				r.Recover2.BlindedOutput = r.Recover2.OPRFSignedPublicKey.PublicKey // a valid point, not k·B
			}
		}
		_, err := client.Recover(ctx, cfg, pin, userInfo)
		assert.ErrorIs(t, err, client.ErrMisbehavingRealm)
	})
}

func TestRecovery_VersionMismatchAcrossRealms(t *testing.T) {
	ctx := context.Background()
	p, realms := newInProc(3)
	cfg := cfgFor(p, realms, 3, 2)
	require.NoError(t, client.Register(ctx, cfg, pin, theSecret, userInfo, 10))

	// Realm 1 alone is re-registered (a partial re-registration).
	only1 := client.Config{Realms: []client.Realm{{ID: recovery.RealmID{1, 1}}}, RegisterThreshold: 1, RecoverThreshold: 1,
		PINHashing: recovery.FastInsecure, Transport: p}
	require.NoError(t, client.Register(ctx, only1, []byte("1111"), []byte("another secret"), userInfo, 10))

	// The majority's registration still recovers.
	got, err := client.Recover(ctx, cfg, pin, userInfo)
	require.NoError(t, err)
	assert.Equal(t, theSecret, got)

	// Two realms that hold different registrations: no agreement.
	pair := client.Config{Realms: realms[1:], RegisterThreshold: 2, RecoverThreshold: 2, PINHashing: recovery.FastInsecure, Transport: p}
	require.ElementsMatch(t, []recovery.RealmID{{1, 1}, {2, 2}}, []recovery.RealmID{realms[1].ID, realms[2].ID})
	_, err = client.Recover(ctx, pair, pin, userInfo)
	assert.ErrorIs(t, err, client.ErrVersionMismatch)

	// A realm whose registration changed between Recover1 and Recover2
	// answers VersionMismatch, and spends no guess.
	before := p.record(t, recovery.RealmID{2, 2}).AttemptedGuesses
	resp, err := p.realms[recovery.RealmID{2, 2}].Handle(ctx, p.auth, recovery.SecretsRequest{Op: recovery.OpRecover2,
		Recover2: &recovery.Recover2Request{Version: [16]byte{9}, BlindedInput: p.record(t, recovery.RealmID{2, 2}).OPRFSignedPublicKey.PublicKey}})
	require.NoError(t, err)
	assert.Equal(t, recovery.StatusVersionMismatch, resp.Status)
	assert.Equal(t, before, p.record(t, recovery.RealmID{2, 2}).AttemptedGuesses)
}

func TestRecovery_ConfigRules(t *testing.T) {
	p, realms := newInProc(3)
	for _, bad := range []client.Config{
		{Transport: p, RegisterThreshold: 1, RecoverThreshold: 1},
		cfgFor(p, realms, 3, 1),                    // not a majority
		cfgFor(p, realms, 1, 2),                    // register below recover
		cfgFor(p, append(realms, realms[0]), 3, 3), // a realm twice
	} {
		assert.Error(t, client.Register(context.Background(), bad, pin, theSecret, userInfo, 10))
	}
	assert.Error(t, client.Register(context.Background(), cfgFor(p, realms, 3, 2), pin, make([]byte, 129), userInfo, 10), "a secret over 128 bytes")
}
