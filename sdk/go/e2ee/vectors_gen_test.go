//go:build e2eegen

package e2ee

import (
	"bytes"
	"crypto/sha256"
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

	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetIndent("", "  ")
	require.NoError(t, enc.Encode(v))
	require.NoError(t, os.WriteFile(vectorsPath, buf.Bytes(), 0o600))
}
