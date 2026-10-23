//go:build e2eegen

package e2ee

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestGenerateVectors fills the sections of testdata/vectors.json that do
// not exist yet and never touches one that does: a golden vector is written
// once, and a change to the code that would change it must turn its test
// red, not rewrite it.
//
//	GOTOOLCHAIN=go1.26.10 go test -tags e2eegen -run TestGenerateVectors ./sdk/go/e2ee/
//	python3 sdk/go/e2ee/testdata/vectors_check.py   # independent recomputation
func TestGenerateVectors(t *testing.T) {
	var v vectors
	if raw, err := os.ReadFile(vectorsPath); err == nil {
		require.NoError(t, json.Unmarshal(raw, &v))
	}
	v.Comment = "sdk/go/e2ee golden vectors (K2b). Written once by vectors_gen_test.go (-tags e2eegen), never rewritten; read by the Go tests and testdata/cross.mjs. Every byte string is hex."
	fill := func(seed string) []byte { // a fixed 32-byte input
		h := sha256.Sum256([]byte("stored.cloud/e2ee/v1/golden/" + seed))
		return h[:]
	}

	if v.Box == nil {
		b := &boxVector{Key: fill("box-key"), Nonce: fill("box-nonce")[:24],
			AD: []byte("stored.cloud/e2ee/v1/golden/box-ad"), Plaintext: []byte("a box pinned on 2026-10-22")}
		msg, err := seal(FormatBox, b.Key, b.Nonce, b.Plaintext, b.AD)
		require.NoError(t, err)
		b.Msg = msg
		v.Box = b
	}
	if v.Sealed == nil {
		s := &sealedVector{Seed: fill("x-wing-seed"), Info: []byte("golden/sealed"),
			AD: []byte("stored.cloud/e2ee/v1/golden/sealed-ad"), Plaintext: []byte("a sealed box pinned on 2026-10-22")}
		pub, err := PublicKey(s.Seed)
		require.NoError(t, err)
		s.Pub = pub
		msg, err := SealTo(pub, s.Plaintext, s.Info, s.AD)
		require.NoError(t, err)
		s.Msg = msg
		v.Sealed = s
	}

	if v.Argon2id == nil {
		for _, c := range []argon2Vector{
			{MemoryKiB: 64 << 10, Iterations: 3, Parallelism: 1},
			{MemoryKiB: 64 << 10, Iterations: 4, Parallelism: 2},
		} {
			c.Password = []byte("correct horse battery staple")
			c.Salt = fill("argon2-salt")[:saltSize]
			c.KEK = deriveKEK(c.Password, c.Salt, KDFParams{c.MemoryKiB, c.Iterations, c.Parallelism})
			v.Argon2id = append(v.Argon2id, c)
		}
	}
	if v.PasswordWrap == nil {
		w := &passwordWrapVector{Password: []byte("Tr0ub4dor&3 is not a passphrase"), MasterKey: fill("master-key"),
			MemoryKiB: Interactive.MemoryKiB, Iterations: Interactive.Iterations, Parallelism: Interactive.Parallelism,
			Salt: fill("password-wrap-salt")[:saltSize], Nonce: fill("password-wrap-nonce")[:24]}
		wrapped, err := wrapMasterKeyWithPassword(w.MasterKey, w.Password, Interactive, w.Salt, w.Nonce)
		require.NoError(t, err)
		w.Wrapped = wrapped
		v.PasswordWrap = w
	}
	if v.KeyWraps == nil {
		v.KeyWraps = map[string]*keyWrapVector{}
	}
	for name, kw := range map[string]struct{ wrapping, key, ad string }{
		"master_under_recovery": {"recovery-key", "master-key", labelMasterWrapRecovery},
		"recovery_under_master": {"master-key", "recovery-key", labelRecoveryKey},
		"recovery_under_vault":  {"vault-key", "recovery-key", labelRecoveryWrapVault},
	} {
		if v.KeyWraps[name] != nil {
			continue
		}
		w := &keyWrapVector{WrappingKey: fill(kw.wrapping), Key: fill(kw.key), Nonce: fill(name + "-nonce")[:24], AD: kw.ad}
		wrapped, err := seal(FormatBox, w.WrappingKey, w.Nonce, w.Key, []byte(w.AD))
		require.NoError(t, err)
		w.Wrapped = wrapped
		v.KeyWraps[name] = w
	}
	if v.BIP39 == nil {
		// trezor/python-mnemonic vectors.json @ master, fetched 2026-10-22
		// (MIT): its 256-bit "english" entries, copied verbatim.
		raw, err := os.ReadFile(os.Getenv("E2EE_TREZOR_VECTORS"))
		require.NoError(t, err, "E2EE_TREZOR_VECTORS = a copy of trezor/python-mnemonic vectors.json")
		var tv struct {
			English [][]string `json:"english"`
		}
		require.NoError(t, json.Unmarshal(raw, &tv))
		for _, e := range tv.English {
			if len(e[0]) == 64 {
				ent, err := hex.DecodeString(e[0])
				require.NoError(t, err)
				v.BIP39 = append(v.BIP39, bip39Vector{Entropy: ent, Mnemonic: e[1]})
			}
		}
		v.BIP39Source = "https://github.com/trezor/python-mnemonic/blob/master/vectors.json (MIT), the 256-bit english entries, fetched 2026-10-22"
	}

	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetIndent("", "  ")
	require.NoError(t, enc.Encode(v))
	require.NoError(t, os.WriteFile(vectorsPath, buf.Bytes(), 0o600))
}
