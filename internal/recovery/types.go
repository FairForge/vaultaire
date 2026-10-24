package recovery

import (
	"encoding/hex"
	"fmt"
	"strings"
	"time"
)

// The Juicebox messages exactly as docs/reviews/K5-PROBE.md § The messages
// pins them (rust/realm/api/src/requests.rs at juicebox-sdk 2e0f544e), with
// their CBOR: structs are maps keyed by field name in declaration order, a
// unit variant is its name, a variant with data is {name: data}.

// RealmID names a realm (16 bytes; 32 hex characters in a configuration and
// in a token's aud).
type RealmID [16]byte

func (r RealmID) String() string { return hex.EncodeToString(r[:]) }

// ParseRealmID reads 32 hex characters; dashes are ignored, as the SDK does.
func ParseRealmID(s string) (RealmID, error) {
	var r RealmID
	b, err := hex.DecodeString(strings.ReplaceAll(s, "-", ""))
	if err != nil || len(b) != len(r) {
		return r, fmt.Errorf("recovery: realm id %q is not 16 hex bytes", s)
	}
	copy(r[:], b)
	return r, nil
}

// SignedPublicKey is a realm's OPRF public key share, signed with the
// registration's one-time Ed25519 key over this realm's id.
type SignedPublicKey struct {
	PublicKey    [32]byte // compressed ristretto255
	VerifyingKey [32]byte // Ed25519
	Signature    [64]byte
}

// Policy is the registration's allowance of guesses.
type Policy struct {
	NumGuesses uint16
}

// Op names the six requests.
type Op uint8

const (
	OpRegister1 Op = iota + 1
	OpRegister2
	OpRecover1
	OpRecover2
	OpRecover3
	OpDelete
)

var opNames = map[Op]string{
	OpRegister1: "Register1", OpRegister2: "Register2", OpRecover1: "Recover1",
	OpRecover2: "Recover2", OpRecover3: "Recover3", OpDelete: "Delete",
}

func (o Op) String() string {
	if n, ok := opNames[o]; ok {
		return n
	}
	return fmt.Sprintf("Op(%d)", uint8(o))
}

func opNamed(name string) (Op, bool) {
	for o, n := range opNames {
		if n == name {
			return o, true
		}
	}
	return 0, false
}

// Register2Request carries this realm's part of a registration.
type Register2Request struct {
	Version                   [16]byte
	OPRFPrivateKey            [32]byte // this realm's share of the root OPRF key
	OPRFSignedPublicKey       SignedPublicKey
	UnlockKeyCommitment       [32]byte
	UnlockKeyTag              [16]byte
	EncryptionKeyScalarShare  [32]byte
	EncryptedSecret           [EncryptedSecretSize]byte
	EncryptedSecretCommitment [16]byte
	Policy                    Policy
}

// Recover2Request is the blinded guess.
type Recover2Request struct {
	Version      [16]byte
	BlindedInput [32]byte
}

// Recover3Request is the proof the guess was right.
type Recover3Request struct {
	Version      [16]byte
	UnlockKeyTag [16]byte
}

// SecretsRequest is one request to a realm (SecretsRequest in the SDK);
// the pointer matching Op is set.
type SecretsRequest struct {
	Op        Op
	Register2 *Register2Request
	Recover2  *Recover2Request
	Recover3  *Recover3Request
}

// NeedsForwardSecrecy reports a request that must never ride in a Noise
// handshake (requests.rs:147): the ones carrying key material or a guess.
func (r SecretsRequest) NeedsForwardSecrecy() bool {
	return r.Op == OpRegister2 || r.Op == OpRecover2 || r.Op == OpRecover3
}

// Status is the outcome variant of a response.
type Status uint8

const (
	StatusOK Status = iota
	StatusNotRegistered
	StatusNoGuesses
	StatusVersionMismatch
	StatusBadUnlockKeyTag
)

var statusNames = map[Status]string{
	StatusOK: "Ok", StatusNotRegistered: "NotRegistered", StatusNoGuesses: "NoGuesses",
	StatusVersionMismatch: "VersionMismatch", StatusBadUnlockKeyTag: "BadUnlockKeyTag",
}

func (s Status) String() string { return statusNames[s] }

// allowed is which outcomes each request can have (requests.rs:157-290).
var allowed = map[Op][]Status{
	OpRegister1: {StatusOK},
	OpRegister2: {StatusOK},
	OpRecover1:  {StatusOK, StatusNotRegistered, StatusNoGuesses},
	OpRecover2:  {StatusOK, StatusVersionMismatch, StatusNotRegistered, StatusNoGuesses},
	OpRecover3:  {StatusOK, StatusVersionMismatch, StatusNotRegistered, StatusBadUnlockKeyTag, StatusNoGuesses},
	OpDelete:    {StatusOK},
}

func validOutcome(o Op, s Status) bool {
	for _, a := range allowed[o] {
		if a == s {
			return true
		}
	}
	return false
}

// Recover2OK is a realm's evaluation of the blinded guess, with its proof.
type Recover2OK struct {
	OPRFSignedPublicKey SignedPublicKey
	BlindedOutput       [32]byte
	Proof               Proof
	UnlockKeyCommitment [32]byte
	NumGuesses          uint16
	GuessCount          uint16 // the attempts counted so far, this one included
}

// Recover3OK is what a realm releases once the PIN is proven.
type Recover3OK struct {
	EncryptionKeyScalarShare  [32]byte
	EncryptedSecret           [EncryptedSecretSize]byte
	EncryptedSecretCommitment [16]byte
}

// SecretsResponse is a realm's answer (SecretsResponse in the SDK).
type SecretsResponse struct {
	Op               Op
	Status           Status
	Version          [16]byte // Recover1 Ok
	Recover2         *Recover2OK
	Recover3         *Recover3OK
	GuessesRemaining uint16 // Recover3 BadUnlockKeyTag
}

// ---- encoding ----

func appendSigned(b []byte, s SignedPublicKey) []byte {
	b = appendMapHead(b, 3)
	b = appendBytes(appendText(b, "public_key"), s.PublicKey[:])
	b = appendBytes(appendText(b, "verifying_key"), s.VerifyingKey[:])
	return appendBytes(appendText(b, "signature"), s.Signature[:])
}

func appendProof(b []byte, p Proof) []byte {
	b = appendMapHead(b, 2)
	b = appendBytes(appendText(b, "c"), p.C[:])
	return appendBytes(appendText(b, "beta_z"), p.BetaZ[:])
}

// MarshalCBOR encodes the request as the SDK does.
func (r SecretsRequest) MarshalCBOR() ([]byte, error) {
	switch r.Op {
	case OpRegister1, OpRecover1, OpDelete:
		return appendText(nil, r.Op.String()), nil
	case OpRegister2:
		q := r.Register2
		if q == nil {
			return nil, fmt.Errorf("recovery: Register2 without a body")
		}
		b := appendVariant(nil, "Register2")
		b = appendMapHead(b, 9)
		b = appendBytes(appendText(b, "version"), q.Version[:])
		b = appendBytes(appendText(b, "oprf_private_key"), q.OPRFPrivateKey[:])
		b = appendSigned(appendText(b, "oprf_signed_public_key"), q.OPRFSignedPublicKey)
		b = appendBytes(appendText(b, "unlock_key_commitment"), q.UnlockKeyCommitment[:])
		b = appendBytes(appendText(b, "unlock_key_tag"), q.UnlockKeyTag[:])
		b = appendBytes(appendText(b, "encryption_key_scalar_share"), q.EncryptionKeyScalarShare[:])
		b = appendBytes(appendText(b, "encrypted_secret"), q.EncryptedSecret[:])
		b = appendBytes(appendText(b, "encrypted_secret_commitment"), q.EncryptedSecretCommitment[:])
		b = appendMapHead(appendText(b, "policy"), 1)
		return appendUint(appendText(b, "num_guesses"), uint64(q.Policy.NumGuesses)), nil
	case OpRecover2:
		if r.Recover2 == nil {
			return nil, fmt.Errorf("recovery: Recover2 without a body")
		}
		b := appendMapHead(appendVariant(nil, "Recover2"), 2)
		b = appendBytes(appendText(b, "version"), r.Recover2.Version[:])
		return appendBytes(appendText(b, "oprf_blinded_input"), r.Recover2.BlindedInput[:]), nil
	case OpRecover3:
		if r.Recover3 == nil {
			return nil, fmt.Errorf("recovery: Recover3 without a body")
		}
		b := appendMapHead(appendVariant(nil, "Recover3"), 2)
		b = appendBytes(appendText(b, "version"), r.Recover3.Version[:])
		return appendBytes(appendText(b, "unlock_key_tag"), r.Recover3.UnlockKeyTag[:]), nil
	}
	return nil, fmt.Errorf("recovery: unknown request %v", r.Op)
}

// MarshalCBOR encodes the response as a realm does.
func (r SecretsResponse) MarshalCBOR() ([]byte, error) {
	if !validOutcome(r.Op, r.Status) {
		return nil, fmt.Errorf("recovery: %v cannot answer %v", r.Op, r.Status)
	}
	b := appendVariant(nil, r.Op.String())
	switch {
	case r.Status == StatusOK && r.Op == OpRecover1:
		b = appendMapHead(appendVariant(b, "Ok"), 1)
		return appendBytes(appendText(b, "version"), r.Version[:]), nil
	case r.Status == StatusOK && r.Op == OpRecover2:
		ok := r.Recover2
		if ok == nil {
			return nil, fmt.Errorf("recovery: Recover2 Ok without a body")
		}
		b = appendMapHead(appendVariant(b, "Ok"), 6)
		b = appendSigned(appendText(b, "oprf_signed_public_key"), ok.OPRFSignedPublicKey)
		b = appendBytes(appendText(b, "oprf_blinded_result"), ok.BlindedOutput[:])
		b = appendProof(appendText(b, "oprf_proof"), ok.Proof)
		b = appendBytes(appendText(b, "unlock_key_commitment"), ok.UnlockKeyCommitment[:])
		b = appendUint(appendText(b, "num_guesses"), uint64(ok.NumGuesses))
		return appendUint(appendText(b, "guess_count"), uint64(ok.GuessCount)), nil
	case r.Status == StatusOK && r.Op == OpRecover3:
		ok := r.Recover3
		if ok == nil {
			return nil, fmt.Errorf("recovery: Recover3 Ok without a body")
		}
		b = appendMapHead(appendVariant(b, "Ok"), 3)
		b = appendBytes(appendText(b, "encryption_key_scalar_share"), ok.EncryptionKeyScalarShare[:])
		b = appendBytes(appendText(b, "encrypted_secret"), ok.EncryptedSecret[:])
		return appendBytes(appendText(b, "encrypted_secret_commitment"), ok.EncryptedSecretCommitment[:]), nil
	case r.Status == StatusBadUnlockKeyTag:
		b = appendMapHead(appendVariant(b, "BadUnlockKeyTag"), 1)
		return appendUint(appendText(b, "guesses_remaining"), uint64(r.GuessesRemaining)), nil
	default:
		return appendText(b, r.Status.String()), nil
	}
}

// ---- decoding ----

func decodeSigned(it item) (SignedPublicKey, error) {
	var s SignedPublicKey
	f, err := it.fields("public_key", "verifying_key", "signature")
	if err != nil {
		return s, err
	}
	if err := firstErr(f["public_key"].bytesN(s.PublicKey[:]), f["verifying_key"].bytesN(s.VerifyingKey[:]), f["signature"].bytesN(s.Signature[:])); err != nil {
		return s, err
	}
	return s, nil
}

func firstErr(errs ...error) error {
	for _, e := range errs {
		if e != nil {
			return e
		}
	}
	return nil
}

// scalarField and pointField read and validate, as the SDK's
// deserializers do (a non-canonical scalar or point is refused).
func scalarField(it item, dst *[32]byte) error {
	if err := it.bytesN(dst[:]); err != nil {
		return err
	}
	if _, err := ScalarFromBytes(*dst); err != nil {
		return fmt.Errorf("%w: %w", ErrDecode, err)
	}
	return nil
}

func pointField(it item, dst *[32]byte) error {
	if err := it.bytesN(dst[:]); err != nil {
		return err
	}
	if _, err := ElementFromBytes(*dst); err != nil {
		return fmt.Errorf("%w: %w", ErrDecode, err)
	}
	return nil
}

func u16(it item) (uint16, error) {
	n, err := it.uint(16)
	return uint16(n), err
}

// UnmarshalSecretsRequest decodes one request.
func UnmarshalSecretsRequest(b []byte) (SecretsRequest, error) {
	it, err := decodeCBOR(b)
	if err != nil {
		return SecretsRequest{}, err
	}
	name, data, hasData, err := it.variant()
	if err != nil {
		return SecretsRequest{}, err
	}
	op, ok := opNamed(name)
	if !ok {
		return SecretsRequest{}, fmt.Errorf("%w: unknown request %q", ErrDecode, name)
	}
	unit := op == OpRegister1 || op == OpRecover1 || op == OpDelete
	if unit == hasData {
		return SecretsRequest{}, fmt.Errorf("%w: %s shape", ErrDecode, name)
	}
	r := SecretsRequest{Op: op}
	switch op {
	case OpRegister2:
		f, err := data.fields("version", "oprf_private_key", "oprf_signed_public_key", "unlock_key_commitment",
			"unlock_key_tag", "encryption_key_scalar_share", "encrypted_secret", "encrypted_secret_commitment", "policy")
		if err != nil {
			return r, err
		}
		q := &Register2Request{}
		signed, err := decodeSigned(f["oprf_signed_public_key"])
		if err != nil {
			return r, err
		}
		q.OPRFSignedPublicKey = signed
		pol, err := f["policy"].fields("num_guesses")
		if err != nil {
			return r, err
		}
		if q.Policy.NumGuesses, err = u16(pol["num_guesses"]); err != nil {
			return r, err
		}
		if err := firstErr(
			f["version"].bytesN(q.Version[:]),
			scalarField(f["oprf_private_key"], &q.OPRFPrivateKey),
			f["unlock_key_commitment"].bytesN(q.UnlockKeyCommitment[:]),
			f["unlock_key_tag"].bytesN(q.UnlockKeyTag[:]),
			scalarField(f["encryption_key_scalar_share"], &q.EncryptionKeyScalarShare),
			f["encrypted_secret"].bytesN(q.EncryptedSecret[:]),
			f["encrypted_secret_commitment"].bytesN(q.EncryptedSecretCommitment[:]),
		); err != nil {
			return r, err
		}
		r.Register2 = q
	case OpRecover2:
		f, err := data.fields("version", "oprf_blinded_input")
		if err != nil {
			return r, err
		}
		q := &Recover2Request{}
		if err := firstErr(f["version"].bytesN(q.Version[:]), pointField(f["oprf_blinded_input"], &q.BlindedInput)); err != nil {
			return r, err
		}
		r.Recover2 = q
	case OpRecover3:
		f, err := data.fields("version", "unlock_key_tag")
		if err != nil {
			return r, err
		}
		q := &Recover3Request{}
		if err := firstErr(f["version"].bytesN(q.Version[:]), f["unlock_key_tag"].bytesN(q.UnlockKeyTag[:])); err != nil {
			return r, err
		}
		r.Recover3 = q
	}
	return r, nil
}

// UnmarshalSecretsResponse decodes one response.
func UnmarshalSecretsResponse(b []byte) (SecretsResponse, error) {
	it, err := decodeCBOR(b)
	if err != nil {
		return SecretsResponse{}, err
	}
	name, data, hasData, err := it.variant()
	if err != nil {
		return SecretsResponse{}, err
	}
	op, ok := opNamed(name)
	if !ok || !hasData {
		return SecretsResponse{}, fmt.Errorf("%w: unknown response %q", ErrDecode, name)
	}
	r := SecretsResponse{Op: op}
	sname, sdata, sHasData, err := data.variant()
	if err != nil {
		return r, err
	}
	found := false
	for s, n := range statusNames {
		if n == sname {
			r.Status, found = s, true
		}
	}
	if !found || !validOutcome(op, r.Status) {
		return r, fmt.Errorf("%w: %s cannot answer %q", ErrDecode, name, sname)
	}
	wantData := r.Status == StatusBadUnlockKeyTag || (r.Status == StatusOK && (op == OpRecover1 || op == OpRecover2 || op == OpRecover3))
	if wantData != sHasData {
		return r, fmt.Errorf("%w: %s.%s shape", ErrDecode, name, sname)
	}
	switch {
	case r.Status == StatusBadUnlockKeyTag:
		f, err := sdata.fields("guesses_remaining")
		if err != nil {
			return r, err
		}
		r.GuessesRemaining, err = u16(f["guesses_remaining"])
		return r, err
	case !wantData:
		return r, nil
	case op == OpRecover1:
		f, err := sdata.fields("version")
		if err != nil {
			return r, err
		}
		return r, f["version"].bytesN(r.Version[:])
	case op == OpRecover2:
		f, err := sdata.fields("oprf_signed_public_key", "oprf_blinded_result", "oprf_proof", "unlock_key_commitment", "num_guesses", "guess_count")
		if err != nil {
			return r, err
		}
		ok := &Recover2OK{}
		if ok.OPRFSignedPublicKey, err = decodeSigned(f["oprf_signed_public_key"]); err != nil {
			return r, err
		}
		pf, err := f["oprf_proof"].fields("c", "beta_z")
		if err != nil {
			return r, err
		}
		if ok.NumGuesses, err = u16(f["num_guesses"]); err != nil {
			return r, err
		}
		if ok.GuessCount, err = u16(f["guess_count"]); err != nil {
			return r, err
		}
		if err := firstErr(
			pointField(f["oprf_blinded_result"], &ok.BlindedOutput),
			scalarField(pf["c"], &ok.Proof.C),
			scalarField(pf["beta_z"], &ok.Proof.BetaZ),
			f["unlock_key_commitment"].bytesN(ok.UnlockKeyCommitment[:]),
		); err != nil {
			return r, err
		}
		r.Recover2 = ok
	case op == OpRecover3:
		f, err := sdata.fields("encryption_key_scalar_share", "encrypted_secret", "encrypted_secret_commitment")
		if err != nil {
			return r, err
		}
		ok := &Recover3OK{}
		if err := firstErr(
			scalarField(f["encryption_key_scalar_share"], &ok.EncryptionKeyScalarShare),
			f["encrypted_secret"].bytesN(ok.EncryptedSecret[:]),
			f["encrypted_secret_commitment"].bytesN(ok.EncryptedSecretCommitment[:]),
		); err != nil {
			return r, err
		}
		r.Recover3 = ok
	}
	return r, nil
}

// ---- the Noise envelope (requests.rs:20-110) ----

// PaddedResponseSize is MAX_SECRETS_RESPONSE_LENGTH: every response inside
// Noise is padded to it, so its length says nothing about its variant.
const PaddedResponseSize = 436

// MaxRequestBody is BODY_SIZE_LIMIT, the largest request the SDK sends.
const MaxRequestBody = 2048

// MarshalPadded encodes a response as PaddedSecretsResponse.
func (r SecretsResponse) MarshalPadded() ([]byte, error) {
	inner, err := r.MarshalCBOR()
	if err != nil {
		return nil, err
	}
	if len(inner) > PaddedResponseSize {
		return nil, fmt.Errorf("recovery: response of %d bytes exceeds the padding", len(inner))
	}
	padded := make([]byte, PaddedResponseSize)
	copy(padded, inner)
	b := appendMapHead(nil, 2)
	b = appendUint(appendText(b, "unpadded_length"), uint64(len(inner)))
	return appendBytes(appendText(b, "padded_bytes"), padded), nil
}

// UnmarshalPadded decodes a PaddedSecretsResponse.
func UnmarshalPadded(b []byte) (SecretsResponse, error) {
	it, err := decodeCBOR(b)
	if err != nil {
		return SecretsResponse{}, err
	}
	f, err := it.fields("unpadded_length", "padded_bytes")
	if err != nil {
		return SecretsResponse{}, err
	}
	n, err := u16(f["unpadded_length"])
	if err != nil {
		return SecretsResponse{}, err
	}
	var padded [PaddedResponseSize]byte
	if err := f["padded_bytes"].bytesN(padded[:]); err != nil {
		return SecretsResponse{}, err
	}
	if int(n) > len(padded) {
		return SecretsResponse{}, fmt.Errorf("%w: unpadded length %d", ErrDecode, n)
	}
	return UnmarshalSecretsResponse(padded[:n])
}

// HandshakeRequest / HandshakeResponse are the Noise NK messages.
type HandshakeRequest struct {
	ClientEphemeralPublic []byte
	PayloadCiphertext     []byte
}

type HandshakeResponse struct {
	ServerEphemeralPublic []byte
	PayloadCiphertext     []byte
}

func appendHandshakeRequest(b []byte, h HandshakeRequest) []byte {
	b = appendMapHead(b, 2)
	b = appendBytes(appendText(b, "client_ephemeral_public"), h.ClientEphemeralPublic)
	return appendBytes(appendText(b, "payload_ciphertext"), h.PayloadCiphertext)
}

func appendHandshakeResponse(b []byte, h HandshakeResponse) []byte {
	b = appendMapHead(b, 2)
	b = appendBytes(appendText(b, "server_ephemeral_public"), h.ServerEphemeralPublic)
	return appendBytes(appendText(b, "payload_ciphertext"), h.PayloadCiphertext)
}

// ClientRequestKind says whether the realm will need the user's record.
type ClientRequestKind uint8

const (
	KindHandshakeOnly ClientRequestKind = iota
	KindSecretsRequest
)

// ClientRequest is the envelope a Noise realm receives at POST /req.
// Exactly one of Handshake and Transport is set.
type ClientRequest struct {
	Realm     RealmID
	AuthToken string
	SessionID uint32
	Kind      ClientRequestKind
	Handshake *HandshakeRequest
	Transport []byte // ciphertext under the session
}

// ClientStatus is the envelope's outcome.
type ClientStatus uint8

const (
	ClientOK ClientStatus = iota
	ClientUnavailable
	ClientInvalidAuth
	ClientMissingSession
	ClientSessionError
	ClientDecodingError
	ClientPayloadTooLarge
	ClientRateLimitExceeded
)

var clientStatusNames = map[ClientStatus]string{
	ClientOK: "Ok", ClientUnavailable: "Unavailable", ClientInvalidAuth: "InvalidAuth",
	ClientMissingSession: "MissingSession", ClientSessionError: "SessionError",
	ClientDecodingError: "DecodingError", ClientPayloadTooLarge: "PayloadTooLarge",
	ClientRateLimitExceeded: "RateLimitExceeded",
}

func (s ClientStatus) String() string { return clientStatusNames[s] }

// ClientResponse is the envelope a Noise realm answers with. For ClientOK
// exactly one of Handshake and Transport is set.
type ClientResponse struct {
	Status          ClientStatus
	Handshake       *HandshakeResponse
	SessionLifetime time.Duration
	Transport       []byte
}

// MarshalCBOR encodes the envelope.
func (r ClientRequest) MarshalCBOR() ([]byte, error) {
	b := appendMapHead(nil, 5)
	b = appendBytes(appendText(b, "realm"), r.Realm[:])
	b = appendText(appendText(b, "auth_token"), r.AuthToken)
	b = appendUint(appendText(b, "session_id"), uint64(r.SessionID))
	kind := "HandshakeOnly"
	if r.Kind == KindSecretsRequest {
		kind = "SecretsRequest"
	}
	b = appendText(appendText(b, "kind"), kind)
	b = appendText(b, "encrypted")
	if r.Handshake != nil {
		b = appendMapHead(appendVariant(b, "Handshake"), 1)
		return appendHandshakeRequest(appendText(b, "handshake"), *r.Handshake), nil
	}
	b = appendMapHead(appendVariant(b, "Transport"), 1)
	return appendBytes(appendText(b, "ciphertext"), r.Transport), nil
}

// UnmarshalClientRequest decodes the envelope.
func UnmarshalClientRequest(b []byte) (ClientRequest, error) {
	var r ClientRequest
	it, err := decodeCBOR(b)
	if err != nil {
		return r, err
	}
	f, err := it.fields("realm", "auth_token", "session_id", "kind", "encrypted")
	if err != nil {
		return r, err
	}
	if err := f["realm"].bytesN(r.Realm[:]); err != nil {
		return r, err
	}
	if r.AuthToken, err = f["auth_token"].text(); err != nil {
		return r, err
	}
	sid, err := f["session_id"].uint(32)
	if err != nil {
		return r, err
	}
	r.SessionID = uint32(sid)
	switch k, _ := f["kind"].text(); k {
	case "HandshakeOnly":
		r.Kind = KindHandshakeOnly
	case "SecretsRequest":
		r.Kind = KindSecretsRequest
	default:
		return r, fmt.Errorf("%w: kind %q", ErrDecode, k)
	}
	name, data, hasData, err := f["encrypted"].variant()
	if err != nil || !hasData {
		return r, fmt.Errorf("%w: encrypted", ErrDecode)
	}
	switch name {
	case "Handshake":
		hf, err := data.fields("handshake")
		if err != nil {
			return r, err
		}
		inner, err := hf["handshake"].fields("client_ephemeral_public", "payload_ciphertext")
		if err != nil {
			return r, err
		}
		h := &HandshakeRequest{}
		if h.ClientEphemeralPublic, err = inner["client_ephemeral_public"].bytesAny(); err != nil {
			return r, err
		}
		if h.PayloadCiphertext, err = inner["payload_ciphertext"].bytesAny(); err != nil {
			return r, err
		}
		r.Handshake = h
	case "Transport":
		tf, err := data.fields("ciphertext")
		if err != nil {
			return r, err
		}
		if r.Transport, err = tf["ciphertext"].bytesAny(); err != nil {
			return r, err
		}
	default:
		return r, fmt.Errorf("%w: encrypted %q", ErrDecode, name)
	}
	return r, nil
}

// MarshalCBOR encodes the envelope's answer.
func (r ClientResponse) MarshalCBOR() ([]byte, error) {
	if r.Status != ClientOK {
		name, ok := clientStatusNames[r.Status]
		if !ok {
			return nil, fmt.Errorf("recovery: unknown client status %d", r.Status)
		}
		return appendText(nil, name), nil
	}
	b := appendVariant(nil, "Ok")
	if r.Handshake != nil {
		b = appendMapHead(appendVariant(b, "Handshake"), 2)
		b = appendHandshakeResponse(appendText(b, "handshake"), *r.Handshake)
		b = appendMapHead(appendText(b, "session_lifetime"), 2)
		b = appendUint(appendText(b, "secs"), uint64(r.SessionLifetime/time.Second))
		return appendUint(appendText(b, "nanos"), uint64(r.SessionLifetime%time.Second)), nil
	}
	b = appendMapHead(appendVariant(b, "Transport"), 1)
	return appendBytes(appendText(b, "ciphertext"), r.Transport), nil
}

// UnmarshalClientResponse decodes the envelope's answer.
func UnmarshalClientResponse(b []byte) (ClientResponse, error) {
	var r ClientResponse
	it, err := decodeCBOR(b)
	if err != nil {
		return r, err
	}
	name, data, hasData, err := it.variant()
	if err != nil {
		return r, err
	}
	if name != "Ok" {
		for s, n := range clientStatusNames {
			if n == name && s != ClientOK && !hasData {
				r.Status = s
				return r, nil
			}
		}
		return r, fmt.Errorf("%w: client response %q", ErrDecode, name)
	}
	if !hasData {
		return r, fmt.Errorf("%w: Ok without a body", ErrDecode)
	}
	inner, body, ok2, err := data.variant()
	if err != nil || !ok2 {
		return r, fmt.Errorf("%w: Ok body", ErrDecode)
	}
	switch inner {
	case "Handshake":
		f, err := body.fields("handshake", "session_lifetime")
		if err != nil {
			return r, err
		}
		hf, err := f["handshake"].fields("server_ephemeral_public", "payload_ciphertext")
		if err != nil {
			return r, err
		}
		h := &HandshakeResponse{}
		if h.ServerEphemeralPublic, err = hf["server_ephemeral_public"].bytesAny(); err != nil {
			return r, err
		}
		if h.PayloadCiphertext, err = hf["payload_ciphertext"].bytesAny(); err != nil {
			return r, err
		}
		lf, err := f["session_lifetime"].fields("secs", "nanos")
		if err != nil {
			return r, err
		}
		secs, err1 := lf["secs"].uint(32) // anything longer is not a session lifetime
		nanos, err2 := lf["nanos"].uint(32)
		if err := firstErr(err1, err2); err != nil || nanos >= uint64(time.Second) {
			return r, fmt.Errorf("%w: session_lifetime", ErrDecode)
		}
		r.Handshake = h
		r.SessionLifetime = time.Duration(secs)*time.Second + time.Duration(nanos)
	case "Transport":
		f, err := body.fields("ciphertext")
		if err != nil {
			return r, err
		}
		if r.Transport, err = f["ciphertext"].bytesAny(); err != nil {
			return r, err
		}
	default:
		return r, fmt.Errorf("%w: Ok %q", ErrDecode, inner)
	}
	return r, nil
}
