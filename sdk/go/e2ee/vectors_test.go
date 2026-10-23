package e2ee

import (
	"encoding/hex"
	"encoding/json"
	"os"
	"testing"

	"github.com/stretchr/testify/require"
)

// vectorsPath is the one file of golden vectors (K2b): Go's tests and the
// node cross-test (testdata/cross.mjs) both read it. A section is written
// once by the e2eegen generator (vectors_gen_test.go), which never rewrites
// a section that exists; the deterministic ones are recomputed by an
// independent implementation (testdata/vectors_check.py, PyNaCl and the
// Python standard library) before they are committed.
const vectorsPath = "testdata/vectors.json"

// hexb is a byte string in the vectors file.
type hexb []byte

func (h hexb) MarshalJSON() ([]byte, error) { return json.Marshal(hex.EncodeToString(h)) }

func (h *hexb) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	v, err := hex.DecodeString(s)
	if err != nil {
		return err
	}
	*h = v
	return nil
}

// boxVector: a 0x01 box with a fixed nonce.
type boxVector struct {
	Key       hexb `json:"key"`
	Nonce     hexb `json:"nonce"`
	AD        hexb `json:"ad"`
	Plaintext hexb `json:"plaintext"`
	Msg       hexb `json:"msg"`
}

// sealedVector: a 0x03 sealed box produced once (HPKE's encapsulation is
// random, so only the open side can be pinned).
type sealedVector struct {
	Seed      hexb `json:"seed"`
	Pub       hexb `json:"pub"`
	Info      hexb `json:"info"`
	AD        hexb `json:"ad"`
	Plaintext hexb `json:"plaintext"`
	Msg       hexb `json:"msg"`
}

// argon2Vector: Argon2id(password, salt) under fixed parameters (RFC 9106;
// recomputed with libsodium and OpenSSL by testdata/vectors_check.py).
type argon2Vector struct {
	Password    hexb   `json:"password"`
	Salt        hexb   `json:"salt"`
	MemoryKiB   uint32 `json:"memory_kib"`
	Iterations  uint32 `json:"iterations"`
	Parallelism uint8  `json:"parallelism"`
	KEK         hexb   `json:"kek"`
}

// passwordWrapVector: a 0x04 password wrap with a fixed salt and nonce.
type passwordWrapVector struct {
	Password    hexb   `json:"password"`
	MasterKey   hexb   `json:"master_key"`
	MemoryKiB   uint32 `json:"memory_kib"`
	Iterations  uint32 `json:"iterations"`
	Parallelism uint8  `json:"parallelism"`
	Salt        hexb   `json:"salt"`
	Nonce       hexb   `json:"nonce"`
	Wrapped     hexb   `json:"wrapped"`
}

// keyWrapVector: one key wrapped under another in a 0x01 box with a fixed
// nonce, under the AD label of its role.
type keyWrapVector struct {
	WrappingKey hexb   `json:"wrapping_key"`
	Key         hexb   `json:"key"`
	Nonce       hexb   `json:"nonce"`
	AD          string `json:"ad"`
	Wrapped     hexb   `json:"wrapped"`
}

// bip39Vector: BIP-39's published vectors (trezor/python-mnemonic
// vectors.json, MIT), the 256-bit entries: entropy → the 24 words.
type bip39Vector struct {
	Entropy  hexb   `json:"entropy"`
	Mnemonic string `json:"mnemonic"`
}

// identityVector: an identity from fixed seeds — its public keys, its
// fingerprint, one deterministic Ed25519 signature, and its two wraps under
// a master key with fixed nonces.
type identityVector struct {
	EncPrivate        hexb   `json:"enc_private"`
	SigPrivate        hexb   `json:"sig_private"`
	EncPublic         hexb   `json:"enc_public"`
	SigPublic         hexb   `json:"sig_public"`
	Fingerprint       hexb   `json:"fingerprint"`
	FingerprintString string `json:"fingerprint_string"`
	SignContext       string `json:"sign_context"`
	SignMessage       hexb   `json:"sign_message"`
	Signature         hexb   `json:"signature"`
	MasterKey         hexb   `json:"master_key"`
	NonceEnc          hexb   `json:"nonce_enc"`
	NonceSig          hexb   `json:"nonce_sig"`
	WrappedEnc        hexb   `json:"wrapped_enc"`
	WrappedSig        hexb   `json:"wrapped_sig"`
}

type vectors struct {
	Comment      string                     `json:"comment"`
	Box          *boxVector                 `json:"box,omitempty"`
	Sealed       *sealedVector              `json:"sealed,omitempty"`
	Argon2id     []argon2Vector             `json:"argon2id,omitempty"`
	PasswordWrap *passwordWrapVector        `json:"password_wrap,omitempty"`
	KeyWraps     map[string]*keyWrapVector  `json:"key_wraps,omitempty"`
	BIP39Source  string                     `json:"bip39_source,omitempty"`
	BIP39        []bip39Vector              `json:"bip39,omitempty"`
	Identities   map[string]*identityVector `json:"identities,omitempty"`
}

func loadVectors(t *testing.T) vectors {
	t.Helper()
	raw, err := os.ReadFile(vectorsPath)
	require.NoError(t, err)
	var v vectors
	require.NoError(t, json.Unmarshal(raw, &v))
	return v
}
