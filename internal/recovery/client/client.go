// Package client registers, recovers and deletes a PIN-protected secret
// across N Juicebox realms (docs/reviews/K5-PROBE.md): the secret is
// encrypted under a key only the PIN plus a threshold of realms rebuild,
// each realm counts guesses on its own, and no realm (nor Vaultaire, which
// only mints tokens) ever sees the PIN or the secret.
//
// The secret is opaque bytes (≤ 128); K2's recovery key is the first thing
// it will carry (F-12). Until two realms under different control exist,
// a vault built on this is attempt-limited by our realm — never "hardware".
package client

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/subtle"
	"errors"
	"fmt"
	"io"
	"sort"
	"sync"

	"github.com/FairForge/vaultaire/internal/recovery"
	"github.com/gtank/ristretto255"
)

// Realm is one realm as the client is configured with it.
type Realm struct {
	ID recovery.RealmID
	// Address and PublicKey are for the network transport: the realm's
	// base URL and its 32-byte Noise static key.
	Address   string
	PublicKey []byte
}

// Transport carries one request to one realm and returns its answer. An
// error is a failure to get an answer (network, auth, a realm refusing to
// talk), never a protocol outcome.
type Transport interface {
	Send(ctx context.Context, realm Realm, req recovery.SecretsRequest) (recovery.SecretsResponse, error)
}

// Config is the realms and thresholds. RecoverThreshold must be a strict
// majority of the realms (as the SDK requires) and RegisterThreshold at
// least RecoverThreshold.
type Config struct {
	Realms            []Realm
	RegisterThreshold int
	RecoverThreshold  int
	PINHashing        recovery.PINHashingMode
	Transport         Transport
	Rand              io.Reader // nil = crypto/rand
}

var (
	// ErrNotRegistered: no secret is registered on enough realms.
	ErrNotRegistered = errors.New("recovery: no secret registered")
	// ErrNoGuesses: the realms have destroyed the record; it cannot come back.
	ErrNoGuesses = errors.New("recovery: no guesses remain, the secret was destroyed")
	// ErrVersionMismatch: the realms do not agree on one registration.
	ErrVersionMismatch = errors.New("recovery: realms disagree on the registration")
	// ErrTransient: not enough realms answered; the same call may succeed later.
	ErrTransient = errors.New("recovery: not enough realms answered")
	// ErrInvalidAuth: a realm refused the token.
	ErrInvalidAuth = errors.New("recovery: a realm refused the token")
	// ErrMisbehavingRealm: answers that fail their proofs or commitments
	// left too few realms.
	ErrMisbehavingRealm = errors.New("recovery: realms returned inconsistent material")
)

// ErrInvalidPin is a wrong PIN; GuessesRemaining is the least any realm
// still allows.
type ErrInvalidPin struct {
	GuessesRemaining uint16
}

func (e ErrInvalidPin) Error() string {
	return fmt.Sprintf("recovery: wrong PIN, %d guesses remaining", e.GuessesRemaining)
}

// checked is a validated configuration with its realms sorted by id: a
// realm's share index is its 1-based position (configuration.rs:104).
type checked struct {
	Config
	realms []Realm
}

func check(cfg Config) (*checked, error) {
	n := len(cfg.Realms)
	switch {
	case n == 0:
		return nil, errors.New("recovery: no realms configured")
	case cfg.Transport == nil:
		return nil, errors.New("recovery: no transport")
	case cfg.RecoverThreshold < 1 || cfg.RecoverThreshold > n:
		return nil, fmt.Errorf("recovery: recover threshold %d of %d realms", cfg.RecoverThreshold, n)
	case cfg.RecoverThreshold <= n/2:
		return nil, fmt.Errorf("recovery: recover threshold %d is not a majority of %d realms", cfg.RecoverThreshold, n)
	case cfg.RegisterThreshold < cfg.RecoverThreshold || cfg.RegisterThreshold > n:
		return nil, fmt.Errorf("recovery: register threshold %d (recover %d, %d realms)", cfg.RegisterThreshold, cfg.RecoverThreshold, n)
	}
	realms := append([]Realm(nil), cfg.Realms...)
	sort.Slice(realms, func(i, j int) bool { return bytes.Compare(realms[i].ID[:], realms[j].ID[:]) < 0 })
	for i := 1; i < n; i++ {
		if realms[i].ID == realms[i-1].ID {
			return nil, fmt.Errorf("recovery: realm %s configured twice", realms[i].ID)
		}
	}
	if cfg.Rand == nil {
		cfg.Rand = rand.Reader
	}
	return &checked{Config: cfg, realms: realms}, nil
}

type outcome[T any] struct {
	realm int // index into checked.realms
	val   T
	err   error
}

// fanout runs f against the given realms at once and returns every result.
func fanout[T any](ctx context.Context, c *checked, idx []int, f func(context.Context, int) (T, error)) []outcome[T] {
	out := make([]outcome[T], len(idx))
	var wg sync.WaitGroup
	for i, r := range idx {
		wg.Add(1)
		go func() {
			defer wg.Done()
			v, err := f(ctx, r)
			out[i] = outcome[T]{realm: r, val: v, err: err}
		}()
	}
	wg.Wait()
	return out
}

func all(c *checked) []int {
	idx := make([]int, len(c.realms))
	for i := range idx {
		idx[i] = i
	}
	return idx
}

func (c *checked) send(ctx context.Context, i int, req recovery.SecretsRequest) (recovery.SecretsResponse, error) {
	resp, err := c.Transport.Send(ctx, c.realms[i], req)
	if err != nil {
		if errors.Is(err, ErrInvalidAuth) || errors.Is(err, ErrTransient) {
			return resp, err
		}
		return resp, fmt.Errorf("%w: realm %s: %w", ErrTransient, c.realms[i].ID, err)
	}
	if resp.Op != req.Op {
		return resp, fmt.Errorf("%w: realm %s answered %v to %v", ErrMisbehavingRealm, c.realms[i].ID, resp.Op, req.Op)
	}
	return resp, nil
}

// failure picks the error that explains why fewer than threshold realms
// succeeded: a permanent outcome when the permanent ones alone make the
// threshold unreachable, else the most telling of the rest.
func failure(errs []error, total, threshold int) error {
	count := func(target error) int {
		n := 0
		for _, e := range errs {
			if errors.Is(e, target) {
				n++
			}
		}
		return n
	}
	slack := total - threshold
	if g := count(ErrNoGuesses); g > slack {
		return ErrNoGuesses
	}
	var pin ErrInvalidPin
	for _, e := range errs {
		if errors.As(e, &pin) {
			return pin
		}
	}
	if count(ErrNoGuesses)+count(ErrNotRegistered) > slack {
		return ErrNotRegistered
	}
	for _, target := range []error{ErrVersionMismatch, ErrInvalidAuth, ErrMisbehavingRealm} {
		if count(target) > 0 {
			return target
		}
	}
	return ErrTransient
}

// Register stores secret under pin on the configured realms; a later
// Recover with the same pin and userInfo returns it. userInfo is mixed into
// the PIN's salt (use the token's opaque subject); allowedGuesses is each
// realm's allowance of wrong PINs before it destroys the record.
func Register(ctx context.Context, cfg Config, pin, secret, userInfo []byte, allowedGuesses uint16) error {
	c, err := check(cfg)
	if err != nil {
		return err
	}
	if len(secret) > recovery.MaxSecretLength {
		return fmt.Errorf("recovery: secret is %d bytes, at most %d", len(secret), recovery.MaxSecretLength)
	}
	n, t := len(c.realms), c.RecoverThreshold

	phase1 := fanout(ctx, c, all(c), func(ctx context.Context, i int) (struct{}, error) {
		_, err := c.send(ctx, i, recovery.SecretsRequest{Op: recovery.OpRegister1})
		return struct{}{}, err
	})
	if err := atLeast(phase1, c.RegisterThreshold, n); err != nil {
		return err
	}

	var version [16]byte
	if _, err := io.ReadFull(c.Rand, version[:]); err != nil {
		return fmt.Errorf("registration version: %w", err)
	}
	accessKey, seed := recovery.HashPIN(c.PINHashing, pin, version, userInfo)
	root, err := recovery.RandomScalar(c.Rand)
	if err != nil {
		return err
	}
	keyShares, err := recovery.ShareScalar(c.Rand, root, t, n)
	if err != nil {
		return err
	}
	var signingSeed [ed25519.SeedSize]byte
	if _, err := io.ReadFull(c.Rand, signingSeed[:]); err != nil {
		return fmt.Errorf("signing key: %w", err)
	}
	signing := ed25519.NewKeyFromSeed(signingSeed[:]) // used for this registration, then dropped
	clear(signingSeed[:])
	unlockKey, commitment := recovery.UnlockKey(recovery.OPRFEvaluate(root, accessKey[:]))
	encScalar, err := recovery.RandomScalar(c.Rand)
	if err != nil {
		return err
	}
	encShares, err := recovery.ShareScalar(c.Rand, encScalar, t, n)
	if err != nil {
		return err
	}
	encrypted, err := recovery.EncryptSecret(recovery.EncryptionKey(seed, [32]byte(encScalar.Bytes())), secret)
	if err != nil {
		return err
	}

	phase2 := fanout(ctx, c, all(c), func(ctx context.Context, i int) (struct{}, error) {
		id := c.realms[i].ID
		share := [32]byte(encShares[i].Secret.Bytes())
		pub := [32]byte(ristretto255.NewIdentityElement().ScalarBaseMult(keyShares[i].Secret).Bytes())
		_, err := c.send(ctx, i, recovery.SecretsRequest{Op: recovery.OpRegister2, Register2: &recovery.Register2Request{
			Version:                   version,
			OPRFPrivateKey:            [32]byte(keyShares[i].Secret.Bytes()),
			OPRFSignedPublicKey:       recovery.SignPublicKey(signing, id, pub),
			UnlockKeyCommitment:       commitment,
			UnlockKeyTag:              recovery.UnlockKeyTag(unlockKey, id),
			EncryptionKeyScalarShare:  share,
			EncryptedSecret:           encrypted,
			EncryptedSecretCommitment: recovery.EncryptedSecretCommitment(unlockKey, id, share, encrypted),
			Policy:                    recovery.Policy{NumGuesses: allowedGuesses},
		}})
		return struct{}{}, err
	})
	return atLeast(phase2, c.RegisterThreshold, n)
}

func atLeast[T any](res []outcome[T], threshold, total int) error {
	var errs []error
	for _, r := range res {
		if r.err != nil {
			errs = append(errs, r.err)
		}
	}
	if len(res)-len(errs) >= threshold {
		return nil
	}
	return failure(errs, total, threshold)
}

// Recover returns the secret registered under pin and userInfo.
func Recover(ctx context.Context, cfg Config, pin, userInfo []byte) ([]byte, error) {
	c, err := check(cfg)
	if err != nil {
		return nil, err
	}
	n, t := len(c.realms), c.RecoverThreshold

	// Phase 1: which registration (version) do a threshold of realms hold?
	phase1 := fanout(ctx, c, all(c), func(ctx context.Context, i int) ([16]byte, error) {
		resp, err := c.send(ctx, i, recovery.SecretsRequest{Op: recovery.OpRecover1})
		if err != nil {
			return [16]byte{}, err
		}
		switch resp.Status {
		case recovery.StatusOK:
			return resp.Version, nil
		case recovery.StatusNoGuesses:
			return [16]byte{}, ErrNoGuesses
		default:
			return [16]byte{}, ErrNotRegistered
		}
	})
	byVersion := map[[16]byte][]int{}
	var errs []error
	for _, o := range phase1 {
		if o.err != nil {
			errs = append(errs, o.err)
			continue
		}
		byVersion[o.val] = append(byVersion[o.val], o.realm)
	}
	var version [16]byte
	var realms []int
	for v, rs := range byVersion {
		if len(rs) >= t { // a majority threshold: at most one version can reach it
			version, realms = v, rs
		}
	}
	if realms == nil {
		if len(phase1)-len(errs) >= t {
			return nil, ErrVersionMismatch // enough realms registered, on different registrations
		}
		return nil, failure(errs, n, t)
	}
	sort.Ints(realms)

	// Phase 2: the blinded guess. Each realm counts it before answering.
	accessKey, seed := recovery.HashPIN(c.PINHashing, pin, version, userInfo)
	blind, blinded, err := recovery.OPRFBlind(c.Rand, accessKey[:])
	if err != nil {
		return nil, err
	}
	type eval struct {
		resp *recovery.Recover2OK
	}
	phase2 := fanout(ctx, c, realms, func(ctx context.Context, i int) (eval, error) {
		resp, err := c.send(ctx, i, recovery.SecretsRequest{Op: recovery.OpRecover2,
			Recover2: &recovery.Recover2Request{Version: version, BlindedInput: blinded}})
		if err != nil {
			return eval{}, err
		}
		switch resp.Status {
		case recovery.StatusOK:
		case recovery.StatusNoGuesses:
			return eval{}, ErrNoGuesses
		case recovery.StatusVersionMismatch:
			return eval{}, ErrVersionMismatch
		default:
			return eval{}, ErrNotRegistered
		}
		ok := resp.Recover2
		// The realm's key share is the one the registration signed for
		// THIS realm, and it evaluated with it (DLEQ).
		if !ok.OPRFSignedPublicKey.Verify(c.realms[i].ID) {
			return eval{}, fmt.Errorf("%w: realm %s: public key signature", ErrMisbehavingRealm, c.realms[i].ID)
		}
		if !recovery.VerifyDLEQ(blinded, ok.OPRFSignedPublicKey.PublicKey, ok.BlindedOutput, ok.Proof) {
			return eval{}, fmt.Errorf("%w: realm %s: evaluation proof", ErrMisbehavingRealm, c.realms[i].ID)
		}
		if ok.GuessCount > ok.NumGuesses {
			return eval{}, fmt.Errorf("%w: realm %s: guess count", ErrMisbehavingRealm, c.realms[i].ID)
		}
		return eval{resp: ok}, nil
	})
	type agreement struct {
		commitment, verifyingKey [32]byte
	}
	groups := map[agreement][]outcome[eval]{}
	errs = errs[:0]
	for _, o := range phase2 {
		if o.err != nil {
			errs = append(errs, o.err)
			continue
		}
		a := agreement{o.val.resp.UnlockKeyCommitment, o.val.resp.OPRFSignedPublicKey.VerifyingKey}
		groups[a] = append(groups[a], o)
	}
	var group []outcome[eval]
	var agreed agreement
	for a, g := range groups {
		if len(g) >= t {
			group, agreed = g, a
		}
	}
	if group == nil {
		if len(groups) > 1 {
			errs = append(errs, ErrMisbehavingRealm)
		}
		return nil, failure(errs, len(realms), t)
	}
	shares := make([]recovery.ElementShare, len(group))
	remaining := ^uint16(0)
	for j, o := range group {
		w, err := recovery.ElementFromBytes(o.val.resp.BlindedOutput)
		if err != nil {
			return nil, fmt.Errorf("%w: %w", ErrMisbehavingRealm, err)
		}
		shares[j] = recovery.ElementShare{Index: uint32(o.realm + 1), Secret: w}
		remaining = min(remaining, o.val.resp.NumGuesses-o.val.resp.GuessCount)
	}
	combined, err := recovery.RecoverElement(shares)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrMisbehavingRealm, err)
	}
	unlockKey, commitment := recovery.UnlockKey(recovery.OPRFFinalize(accessKey[:], blind, combined))
	if subtle.ConstantTimeCompare(commitment[:], agreed.commitment[:]) != 1 {
		return nil, ErrInvalidPin{GuessesRemaining: remaining}
	}

	// Phase 3: prove the PIN to each realm; each resets its count and
	// releases its share of the encryption scalar and the ciphertext.
	agreeing := make([]int, len(group))
	for j, o := range group {
		agreeing[j] = o.realm
	}
	type release struct {
		share     [32]byte
		encrypted [recovery.EncryptedSecretSize]byte
	}
	phase3 := fanout(ctx, c, agreeing, func(ctx context.Context, i int) (release, error) {
		id := c.realms[i].ID
		resp, err := c.send(ctx, i, recovery.SecretsRequest{Op: recovery.OpRecover3,
			Recover3: &recovery.Recover3Request{Version: version, UnlockKeyTag: recovery.UnlockKeyTag(unlockKey, id)}})
		if err != nil {
			return release{}, err
		}
		switch resp.Status {
		case recovery.StatusOK:
		case recovery.StatusBadUnlockKeyTag:
			return release{}, ErrInvalidPin{GuessesRemaining: resp.GuessesRemaining}
		case recovery.StatusNoGuesses:
			return release{}, ErrNoGuesses
		case recovery.StatusVersionMismatch:
			return release{}, ErrVersionMismatch
		default:
			return release{}, ErrNotRegistered
		}
		ok := resp.Recover3
		want := recovery.EncryptedSecretCommitment(unlockKey, id, ok.EncryptionKeyScalarShare, ok.EncryptedSecret)
		if subtle.ConstantTimeCompare(want[:], ok.EncryptedSecretCommitment[:]) != 1 {
			return release{}, fmt.Errorf("%w: realm %s: secret commitment", ErrMisbehavingRealm, id)
		}
		return release{share: ok.EncryptionKeyScalarShare, encrypted: ok.EncryptedSecret}, nil
	})
	byCiphertext := map[[recovery.EncryptedSecretSize]byte][]recovery.ScalarShare{}
	errs = errs[:0]
	for _, o := range phase3 {
		if o.err != nil {
			errs = append(errs, o.err)
			continue
		}
		s, err := recovery.ScalarFromBytes(o.val.share)
		if err != nil {
			errs = append(errs, ErrMisbehavingRealm)
			continue
		}
		byCiphertext[o.val.encrypted] = append(byCiphertext[o.val.encrypted], recovery.ScalarShare{Index: uint32(o.realm + 1), Secret: s})
	}
	for encrypted, encShares := range byCiphertext {
		if len(encShares) < t {
			continue
		}
		scalar, err := recovery.RecoverScalar(encShares)
		if err != nil {
			return nil, fmt.Errorf("%w: %w", ErrMisbehavingRealm, err)
		}
		return recovery.DecryptSecret(recovery.EncryptionKey(seed, [32]byte(scalar.Bytes())), encrypted)
	}
	return nil, failure(errs, len(agreeing), t)
}

// Delete removes the registration from every realm; it fails if any realm
// did not confirm (a delete needs no PIN, so it is safe to repeat).
func Delete(ctx context.Context, cfg Config) error {
	c, err := check(cfg)
	if err != nil {
		return err
	}
	res := fanout(ctx, c, all(c), func(ctx context.Context, i int) (struct{}, error) {
		_, err := c.send(ctx, i, recovery.SecretsRequest{Op: recovery.OpDelete})
		return struct{}{}, err
	})
	var errs []error
	for _, r := range res {
		if r.err != nil {
			errs = append(errs, r.err)
		}
	}
	return errors.Join(errs...)
}
