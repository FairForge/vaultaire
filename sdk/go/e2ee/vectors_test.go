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

type vectors struct {
	Comment string        `json:"comment"`
	Box     *boxVector    `json:"box,omitempty"`
	Sealed  *sealedVector `json:"sealed,omitempty"`
}

func loadVectors(t *testing.T) vectors {
	t.Helper()
	raw, err := os.ReadFile(vectorsPath)
	require.NoError(t, err)
	var v vectors
	require.NoError(t, json.Unmarshal(raw, &v))
	return v
}
