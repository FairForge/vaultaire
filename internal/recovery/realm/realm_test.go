package realm_test

import (
	"context"
	"crypto/rand"
	"errors"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/FairForge/vaultaire/internal/recovery"
	"github.com/FairForge/vaultaire/internal/recovery/client"
	"github.com/FairForge/vaultaire/internal/recovery/realm"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var who = realm.Auth{Tenant: "stored", Subject: "opaque-sub"}

// one is a transport to a single realm, through the wire encoding.
type one struct{ r *realm.Realm }

func (o one) Send(ctx context.Context, _ client.Realm, req recovery.SecretsRequest) (recovery.SecretsResponse, error) {
	b, err := req.MarshalCBOR()
	if err != nil {
		return recovery.SecretsResponse{}, err
	}
	onWire, err := recovery.UnmarshalSecretsRequest(b)
	if err != nil {
		return recovery.SecretsResponse{}, err
	}
	return o.r.Handle(ctx, who, onWire)
}

func singleRealm(r *realm.Realm) client.Config {
	return client.Config{Realms: []client.Realm{{ID: recovery.RealmID{7}}}, RegisterThreshold: 1, RecoverThreshold: 1,
		PINHashing: recovery.FastInsecure, Transport: one{r}}
}

func register(t *testing.T, r *realm.Realm, guesses uint16) {
	t.Helper()
	require.NoError(t, client.Register(context.Background(), singleRealm(r), []byte("2580"), []byte("s"), []byte("u"), guesses))
}

func randomGuess(t *testing.T, st realm.Store) recovery.SecretsRequest {
	t.Helper()
	rec, _, err := st.Load(context.Background(), who.Tenant, who.Subject)
	require.NoError(t, err)
	_, blinded, err := recovery.OPRFBlind(rand.Reader, []byte("a guess"))
	require.NoError(t, err)
	return recovery.SecretsRequest{Op: recovery.OpRecover2, Recover2: &recovery.Recover2Request{Version: rec.Version, BlindedInput: blinded}}
}

func stores(t *testing.T) map[string]func() realm.Store {
	return map[string]func() realm.Store{
		"memory": func() realm.Store { return realm.NewMemoryStore() },
		"bolt": func() realm.Store {
			st, err := realm.OpenBoltStore(filepath.Join(t.TempDir(), "realm.db"))
			require.NoError(t, err)
			t.Cleanup(func() { _ = st.Close() })
			return st
		},
	}
}

func TestRecovery_ConcurrentRecover2_NeverExceedsAllowedGuesses(t *testing.T) {
	for name, open := range stores(t) {
		t.Run(name, func(t *testing.T) {
			// Arrange: an allowance of 10, 64 guessers at once.
			st := open()
			r := &realm.Realm{Store: st}
			register(t, r, 10)
			guess := randomGuess(t, st)

			// Act
			var evaluated, refused atomic.Int32
			var wg sync.WaitGroup
			for i := 0; i < 64; i++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					resp, err := r.Handle(context.Background(), who, guess)
					if err != nil {
						t.Errorf("handle: %v", err)
						return
					}
					if resp.Status == recovery.StatusOK && resp.Recover2 != nil {
						evaluated.Add(1)
					} else {
						refused.Add(1)
					}
				}()
			}
			wg.Wait()

			// Assert: never more OPRF evaluations than the allowance.
			assert.LessOrEqual(t, evaluated.Load(), int32(10))
			assert.Equal(t, int32(10), evaluated.Load(), "and every allowed guess was served")
			assert.Equal(t, int32(54), refused.Load())
			rec, _, err := st.Load(context.Background(), who.Tenant, who.Subject)
			require.NoError(t, err)
			assert.Equal(t, realm.NoGuesses, rec.State)
			assert.LessOrEqual(t, rec.AttemptedGuesses, uint16(10))
			assert.False(t, rec.HoldsSecretMaterial())
		})
	}
}

// failAfterCommit commits, then reports a failure — a crash between the
// durable write and the answer.
type failAfterCommit struct {
	realm.Store
	armed atomic.Bool
}

var errDiskGone = errors.New("the disk went away after the commit")

func (f *failAfterCommit) CompareAndSwap(ctx context.Context, tenant, sub string, rev uint64, next realm.Record) error {
	if err := f.Store.CompareAndSwap(ctx, tenant, sub, rev, next); err != nil {
		return err
	}
	if f.armed.Load() {
		return errDiskGone
	}
	return nil
}

func TestRecovery_CrashBetweenIncrementAndResponse(t *testing.T) {
	for name, open := range stores(t) {
		t.Run(name, func(t *testing.T) {
			st := &failAfterCommit{Store: open()}
			r := &realm.Realm{Store: st}
			register(t, r, 10)
			guess := randomGuess(t, st)
			st.armed.Store(true)

			resp, err := r.Handle(context.Background(), who, guess)

			require.ErrorIs(t, err, errDiskGone, "no answer may leave")
			assert.Nil(t, resp.Recover2, "no evaluation was released")
			rec, _, err := st.Load(context.Background(), who.Tenant, who.Subject)
			require.NoError(t, err)
			assert.Equal(t, uint16(1), rec.AttemptedGuesses, "the guess is spent all the same")
		})
	}
}

func TestRecovery_BoltStore_DurableAcrossReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "realm.db")
	st, err := realm.OpenBoltStore(path)
	require.NoError(t, err)
	r := &realm.Realm{Store: st}
	register(t, r, 10)
	guess := randomGuess(t, st)
	_, err = r.Handle(context.Background(), who, guess)
	require.NoError(t, err)
	before, rev, err := st.Load(context.Background(), who.Tenant, who.Subject)
	require.NoError(t, err)
	require.NoError(t, st.Close())

	st2, err := realm.OpenBoltStore(path)
	require.NoError(t, err)
	defer func() { _ = st2.Close() }()
	after, rev2, err := st2.Load(context.Background(), who.Tenant, who.Subject)
	require.NoError(t, err)
	assert.Equal(t, before, after)
	assert.Equal(t, rev, rev2)
	assert.Equal(t, uint16(1), after.AttemptedGuesses)
	assert.ErrorIs(t, st2.CompareAndSwap(context.Background(), who.Tenant, who.Subject, rev-1, after), realm.ErrConflict)
}

func TestRealm_StateMachine(t *testing.T) {
	ctx := context.Background()
	st := realm.NewMemoryStore()
	destroyed := 0
	r := &realm.Realm{Store: st, OnDestroy: func() { destroyed++ }}
	do := func(req recovery.SecretsRequest) recovery.SecretsResponse {
		t.Helper()
		resp, err := r.Handle(ctx, who, req)
		require.NoError(t, err)
		return resp
	}
	recover3 := func(tag [16]byte) recovery.SecretsRequest {
		rec, _, _ := st.Load(ctx, who.Tenant, who.Subject)
		return recovery.SecretsRequest{Op: recovery.OpRecover3, Recover3: &recovery.Recover3Request{Version: rec.Version, UnlockKeyTag: tag}}
	}

	// NotRegistered answers.
	assert.Equal(t, recovery.StatusNotRegistered, do(recovery.SecretsRequest{Op: recovery.OpRecover1}).Status)
	assert.Equal(t, recovery.StatusNotRegistered, do(recovery.SecretsRequest{Op: recovery.OpRecover3, Recover3: &recovery.Recover3Request{}}).Status)
	assert.Equal(t, recovery.StatusOK, do(recovery.SecretsRequest{Op: recovery.OpDelete}).Status)

	register(t, r, 2)
	// A wrong tag before any Recover2 spends nothing (as the reference realm).
	resp := do(recover3([16]byte{1}))
	assert.Equal(t, recovery.StatusBadUnlockKeyTag, resp.Status)
	assert.Equal(t, uint16(2), resp.GuessesRemaining)

	// Two guesses, then a wrong tag at 0 remaining: self-destruct.
	guess := randomGuess(t, st)
	assert.Equal(t, uint16(1), do(guess).Recover2.GuessCount)
	assert.Equal(t, uint16(2), do(guess).Recover2.GuessCount)
	resp = do(recover3([16]byte{1}))
	assert.Equal(t, recovery.StatusBadUnlockKeyTag, resp.Status)
	assert.Equal(t, uint16(0), resp.GuessesRemaining)
	rec, _, _ := st.Load(ctx, who.Tenant, who.Subject)
	assert.Equal(t, realm.NoGuesses, rec.State)
	assert.False(t, rec.HoldsSecretMaterial())
	assert.Equal(t, 1, destroyed)
	assert.Equal(t, recovery.StatusNoGuesses, do(recovery.SecretsRequest{Op: recovery.OpRecover1}).Status)
	assert.Equal(t, recovery.StatusNoGuesses, do(guess).Status)
	assert.Equal(t, 1, destroyed, "destroyed once")

	// Delete clears it; a register starts afresh.
	do(recovery.SecretsRequest{Op: recovery.OpDelete})
	assert.Equal(t, recovery.StatusNotRegistered, do(recovery.SecretsRequest{Op: recovery.OpRecover1}).Status)
	register(t, r, 3)
	assert.Equal(t, recovery.StatusOK, do(recovery.SecretsRequest{Op: recovery.OpRecover1}).Status)

	// A Register2 whose public key is not its key share's is refused.
	rec, _, _ = st.Load(ctx, who.Tenant, who.Subject)
	bad := &recovery.Register2Request{Version: rec.Version, OPRFPrivateKey: [32]byte{5}, OPRFSignedPublicKey: rec.OPRFSignedPublicKey}
	_, err := r.Handle(ctx, who, recovery.SecretsRequest{Op: recovery.OpRegister2, Register2: bad})
	assert.ErrorIs(t, err, realm.ErrBadRequest)

	// Records are per (tenant, subject).
	other := realm.Auth{Tenant: "stored", Subject: "someone-else"}
	resp, err = r.Handle(ctx, other, recovery.SecretsRequest{Op: recovery.OpRecover1})
	require.NoError(t, err)
	assert.Equal(t, recovery.StatusNotRegistered, resp.Status)
	_, err = r.Handle(ctx, realm.Auth{Tenant: "stored"}, recovery.SecretsRequest{Op: recovery.OpRecover1})
	assert.ErrorIs(t, err, realm.ErrBadRequest)
}
