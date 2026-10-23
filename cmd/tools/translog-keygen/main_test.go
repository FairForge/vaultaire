package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/FairForge/vaultaire/internal/translog"
)

// K4c-2: -cosignature makes a witness's ML-DSA-44 cosignature/v1 key; the
// signer key goes to -out (0600), never to stdout; the log's origin is no
// witness name.
func TestKeygen_Cosignature(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "w.key")
	stdout := captureStdout(t, func() { require.NoError(t, run(out, "witness.test/keygen", true)) })

	skey, err := os.ReadFile(out)
	require.NoError(t, err)
	st, err := os.Stat(out)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), st.Mode().Perm())
	assert.NotContains(t, stdout, "PRIVATE", "the signer key is never printed")
	_, err = translog.NewCosigner(strings.TrimSpace(string(skey)), nil)
	require.NoError(t, err)
	v, err := translog.NewCosignatureVerifier(strings.TrimSpace(stdout))
	require.NoError(t, err)
	assert.Equal(t, "witness.test/keygen", v.Name())

	assert.ErrorContains(t, run(filepath.Join(dir, "x"), translog.Origin, true), "never the log's origin")
}

func captureStdout(t *testing.T, f func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	require.NoError(t, err)
	old := os.Stdout
	os.Stdout = w
	f()
	os.Stdout = old
	require.NoError(t, w.Close())
	b := make([]byte, 1<<16)
	n, _ := r.Read(b)
	return string(b[:n])
}
