// Package realm is a Juicebox realm's state machine (docs/reviews/K5-PROBE.md
// § The realm: states and transitions): one record per (tenant, subject),
// a guess counted durably in Recover2 before its answer leaves, the counter
// reset by a correct Recover3, and a record that runs out of guesses ERASED.
// It holds no network or token code: cmd/recovery-realm (never linked into
// cmd/vaultaire) puts it behind Noise and the tenant JWTs.
//
// Copy rule: a realm we run is attempt-limited by our realm. It is not a
// hardware attempt limit until two realms under different control exist
// (F-19).
package realm

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"errors"
	"fmt"
	"io"

	"github.com/FairForge/vaultaire/internal/recovery"
	"github.com/gtank/ristretto255"
)

// State is a record's state.
type State uint8

const (
	NotRegistered State = iota
	Registered
	NoGuesses
)

func (s State) String() string {
	switch s {
	case Registered:
		return "Registered"
	case NoGuesses:
		return "NoGuesses"
	default:
		return "NotRegistered"
	}
}

// Record is what a realm keeps for one user. In NoGuesses every field but
// State and the two counts is zero.
type Record struct {
	State                     State
	Version                   [16]byte
	OPRFPrivateKey            [32]byte
	OPRFSignedPublicKey       recovery.SignedPublicKey
	UnlockKeyCommitment       [32]byte
	UnlockKeyTag              [16]byte
	EncryptionKeyScalarShare  [32]byte
	EncryptedSecret           [recovery.EncryptedSecretSize]byte
	EncryptedSecretCommitment [16]byte
	AllowedGuesses            uint16
	AttemptedGuesses          uint16
}

// HoldsSecretMaterial reports whether any byte a guesser could use is
// still in the record.
func (r Record) HoldsSecretMaterial() bool {
	erased := Record{State: r.State, AllowedGuesses: r.AllowedGuesses, AttemptedGuesses: r.AttemptedGuesses}
	return r != erased
}

// erased is r after self-destruction: the state and the counts, nothing else.
func (r Record) erased() Record {
	return Record{State: NoGuesses, AllowedGuesses: r.AllowedGuesses, AttemptedGuesses: r.AttemptedGuesses}
}

// ErrConflict is CompareAndSwap's answer when the record changed since it
// was loaded.
var ErrConflict = errors.New("realm: record changed concurrently")

// Store keeps records. Load of an absent record returns the zero Record
// (NotRegistered) and revision 0. CompareAndSwap writes next only when the
// record is still at rev (else ErrConflict) and returns once the write is
// durable; a NotRegistered next deletes the record. A revision is never
// handed out twice, across records and deletes (RP52-1): a stale write
// must conflict even after the record was deleted and registered anew.
type Store interface {
	Load(ctx context.Context, tenant, sub string) (Record, uint64, error)
	CompareAndSwap(ctx context.Context, tenant, sub string, rev uint64, next Record) error
}

// Auth is who a request is for, from a validated tenant token: the
// tenant's name and the opaque subject. Nothing else reaches a realm.
type Auth struct {
	Tenant  string
	Subject string
}

// ErrBadRequest is a request the realm refuses to act on.
var ErrBadRequest = errors.New("realm: bad request")

// maxAttempts bounds the load/compare-and-swap loop of one request.
const maxAttempts = 128

// Realm answers SecretsRequests.
type Realm struct {
	Store Store
	// Rand draws the DLEQ proofs' nonces; nil = crypto/rand.
	Rand io.Reader
	// OnDestroy, when set, is called once for each record that self-destructs.
	OnDestroy func()
}

func (r *Realm) rand() io.Reader {
	if r.Rand != nil {
		return r.Rand
	}
	return rand.Reader
}

// Handle runs one request. An error means no answer may be sent; it does
// NOT mean nothing changed (a Recover2 whose increment was committed and
// whose store then failed has spent its guess).
func (r *Realm) Handle(ctx context.Context, auth Auth, req recovery.SecretsRequest) (recovery.SecretsResponse, error) {
	if auth.Tenant == "" || auth.Subject == "" {
		return recovery.SecretsResponse{}, fmt.Errorf("%w: no tenant or subject", ErrBadRequest)
	}
	if req.Op == recovery.OpRegister1 {
		return recovery.SecretsResponse{Op: recovery.OpRegister1}, nil
	}
	for attempt := 0; attempt < maxAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return recovery.SecretsResponse{}, err
		}
		rec, rev, err := r.Store.Load(ctx, auth.Tenant, auth.Subject)
		if err != nil {
			return recovery.SecretsResponse{}, fmt.Errorf("realm load: %w", err)
		}
		resp, next, write, err := r.step(rec, req)
		if err != nil {
			return recovery.SecretsResponse{}, err
		}
		if !write {
			return resp, nil
		}
		// The write is durable before the answer exists for the caller: a
		// Recover2's guess is spent even when the store fails after it.
		if err := r.Store.CompareAndSwap(ctx, auth.Tenant, auth.Subject, rev, next); err != nil {
			if errors.Is(err, ErrConflict) {
				continue
			}
			return recovery.SecretsResponse{}, fmt.Errorf("realm commit %v: %w", req.Op, err)
		}
		if next.State == NoGuesses && rec.State != NoGuesses && r.OnDestroy != nil {
			r.OnDestroy()
		}
		return resp, nil
	}
	return recovery.SecretsResponse{}, fmt.Errorf("realm %v: %w after %d attempts", req.Op, ErrConflict, maxAttempts)
}

// step is the state machine: the answer, the record to write and whether
// to write it.
func (r *Realm) step(rec Record, req recovery.SecretsRequest) (recovery.SecretsResponse, Record, bool, error) {
	resp := recovery.SecretsResponse{Op: req.Op}
	switch req.Op {
	case recovery.OpRegister2:
		q := req.Register2
		if q == nil {
			return resp, rec, false, fmt.Errorf("%w: Register2 without a body", ErrBadRequest)
		}
		k, err := recovery.ScalarFromBytes(q.OPRFPrivateKey)
		if err != nil {
			return resp, rec, false, fmt.Errorf("%w: %w", ErrBadRequest, err)
		}
		if subtle.ConstantTimeCompare(ristretto255.NewIdentityElement().ScalarBaseMult(k).Bytes(), q.OPRFSignedPublicKey.PublicKey[:]) != 1 {
			return resp, rec, false, fmt.Errorf("%w: the public key is not this key share's", ErrBadRequest)
		}
		next := Record{
			State: Registered, Version: q.Version, OPRFPrivateKey: q.OPRFPrivateKey,
			OPRFSignedPublicKey: q.OPRFSignedPublicKey, UnlockKeyCommitment: q.UnlockKeyCommitment,
			UnlockKeyTag: q.UnlockKeyTag, EncryptionKeyScalarShare: q.EncryptionKeyScalarShare,
			EncryptedSecret: q.EncryptedSecret, EncryptedSecretCommitment: q.EncryptedSecretCommitment,
			AllowedGuesses: q.Policy.NumGuesses,
		}
		return resp, next, true, nil

	case recovery.OpRecover1:
		switch {
		case rec.State == NotRegistered:
			resp.Status = recovery.StatusNotRegistered
			return resp, rec, false, nil
		case rec.State == NoGuesses:
			resp.Status = recovery.StatusNoGuesses
			return resp, rec, false, nil
		case rec.AttemptedGuesses >= rec.AllowedGuesses:
			resp.Status = recovery.StatusNoGuesses
			return resp, rec.erased(), true, nil
		}
		resp.Version = rec.Version
		return resp, rec, false, nil

	case recovery.OpRecover2:
		q := req.Recover2
		if q == nil {
			return resp, rec, false, fmt.Errorf("%w: Recover2 without a body", ErrBadRequest)
		}
		switch {
		case rec.State == NotRegistered:
			resp.Status = recovery.StatusNotRegistered
			return resp, rec, false, nil
		case rec.State == NoGuesses:
			resp.Status = recovery.StatusNoGuesses
			return resp, rec, false, nil
		case rec.AttemptedGuesses >= rec.AllowedGuesses:
			resp.Status = recovery.StatusNoGuesses
			return resp, rec.erased(), true, nil
		case subtle.ConstantTimeCompare(q.Version[:], rec.Version[:]) != 1:
			resp.Status = recovery.StatusVersionMismatch
			return resp, rec, false, nil
		}
		k, err := recovery.ScalarFromBytes(rec.OPRFPrivateKey)
		if err != nil {
			return resp, rec, false, fmt.Errorf("realm: stored key share: %w", err)
		}
		u, err := recovery.ElementFromBytes(q.BlindedInput)
		if err != nil {
			return resp, rec, false, fmt.Errorf("%w: %w", ErrBadRequest, err)
		}
		w := recovery.OPRFBlindEvaluate(k, u)
		proof, err := recovery.ProveDLEQ(r.rand(), k, u, rec.OPRFSignedPublicKey.PublicKey, w)
		if err != nil {
			return resp, rec, false, fmt.Errorf("realm proof: %w", err)
		}
		next := rec
		next.AttemptedGuesses++ // the attempt is counted here, and committed before the answer
		var out [32]byte
		copy(out[:], w.Bytes())
		resp.Recover2 = &recovery.Recover2OK{
			OPRFSignedPublicKey: rec.OPRFSignedPublicKey, BlindedOutput: out, Proof: proof,
			UnlockKeyCommitment: rec.UnlockKeyCommitment,
			NumGuesses:          rec.AllowedGuesses, GuessCount: next.AttemptedGuesses,
		}
		return resp, next, true, nil

	case recovery.OpRecover3:
		q := req.Recover3
		if q == nil {
			return resp, rec, false, fmt.Errorf("%w: Recover3 without a body", ErrBadRequest)
		}
		switch {
		case rec.State == NotRegistered:
			resp.Status = recovery.StatusNotRegistered
			return resp, rec, false, nil
		case rec.State == NoGuesses:
			resp.Status = recovery.StatusNoGuesses
			return resp, rec, false, nil
		case subtle.ConstantTimeCompare(q.Version[:], rec.Version[:]) != 1:
			resp.Status = recovery.StatusVersionMismatch
			return resp, rec, false, nil
		}
		if subtle.ConstantTimeCompare(q.UnlockKeyTag[:], rec.UnlockKeyTag[:]) != 1 {
			resp.Status = recovery.StatusBadUnlockKeyTag
			if rec.AttemptedGuesses < rec.AllowedGuesses {
				resp.GuessesRemaining = rec.AllowedGuesses - rec.AttemptedGuesses
				return resp, rec, false, nil
			}
			return resp, rec.erased(), true, nil // none left: self-destruct
		}
		resp.Recover3 = &recovery.Recover3OK{
			EncryptionKeyScalarShare:  rec.EncryptionKeyScalarShare,
			EncryptedSecret:           rec.EncryptedSecret,
			EncryptedSecretCommitment: rec.EncryptedSecretCommitment,
		}
		if rec.AttemptedGuesses == 0 {
			return resp, rec, false, nil
		}
		next := rec
		next.AttemptedGuesses = 0 // the PIN is proven: the counter resets
		return resp, next, true, nil

	case recovery.OpDelete:
		if rec.State == NotRegistered {
			return resp, rec, false, nil
		}
		return resp, Record{}, true, nil
	}
	return resp, rec, false, fmt.Errorf("%w: unknown request %v", ErrBadRequest, req.Op)
}
