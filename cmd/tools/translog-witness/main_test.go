package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/mod/sumdb/note"

	"github.com/FairForge/vaultaire/internal/translog"
	"github.com/FairForge/vaultaire/internal/translog/witness"
)

func TestTranslogWitness_OnePass_KeysFromFiles(t *testing.T) {
	// Arrange: a served log, the log's verifier key and a witness key in files.
	skey, vkey, err := translog.GenerateKey(rand.Reader)
	require.NoError(t, err)
	s, err := translog.NewSigner(skey)
	require.NoError(t, err)
	m := &translog.Memory{}
	for i := 0; i < 5; i++ {
		_, err := m.Append([]byte(fmt.Sprintf("entry-%d", i)))
		require.NoError(t, err)
	}
	mux := http.NewServeMux()
	mux.Handle("/translog/", http.StripPrefix("/translog/", translog.NewHandler(&translog.MemorySource{Log: m, Signer: s})))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	tmp := t.TempDir()
	wskey, wvkey, err := translog.GenerateCosignerKey("witness.test/ci")
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(tmp, "log.vkey"), []byte(vkey+"\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(tmp, "w.key"), []byte(wskey+"\n"), 0o600))
	dir := filepath.Join(tmp, "w")
	var out bytes.Buffer

	// Act
	err = run(context.Background(), &out, options{url: srv.URL + "/translog", vkey: "@" + filepath.Join(tmp, "log.vkey"),
		key: "@" + filepath.Join(tmp, "w.key"), dir: dir, once: true})

	// Assert
	require.NoError(t, err)
	assert.Contains(t, out.String(), "size 5")
	assert.Contains(t, out.String(), "cosigned by witness.test/ci")
	cos, err := os.ReadFile(filepath.Join(dir, witness.CosignFile))
	require.NoError(t, err)
	lv, err := translog.NewVerifier(vkey)
	require.NoError(t, err)
	wv, err := translog.NewCosignatureVerifier(wvkey)
	require.NoError(t, err)
	n, err := note.Open(cos, note.VerifierList(lv, wv))
	require.NoError(t, err)
	assert.Len(t, n.Sigs, 2)
	_, lines, err := translog.OpenCosigned(cos, lv, wv)
	require.NoError(t, err)
	require.Len(t, lines, 1, "a c2sp.org/tlog-cosignature line, timestamped")
	assert.WithinDuration(t, time.Now(), lines[0].Time, time.Minute)

	// The log's own key as -key is refused, and so is a plain note key (no
	// cosignature/v1, K4c-2); a missing -key too.
	assert.ErrorContains(t, run(context.Background(), &out, options{url: srv.URL + "/translog", vkey: vkey, key: skey, dir: dir, once: true}), "ML-DSA-44")
	plain, _, err := note.GenerateKey(rand.Reader, "witness.test/plain")
	require.NoError(t, err)
	assert.ErrorContains(t, run(context.Background(), &out, options{url: srv.URL + "/translog", vkey: vkey, key: plain, dir: dir, once: true}), "cosignature/v1")
	assert.ErrorContains(t, run(context.Background(), &out, options{url: srv.URL + "/translog", vkey: vkey, dir: dir, once: true}), "-key is required")
}

// The witness is an operator tool: the product binary never links it.
func TestProductBinaryDoesNotLinkTheWitness(t *testing.T) {
	if testing.Short() {
		t.Skip("runs go list")
	}
	out, err := exec.Command("go", "list", "-deps", "github.com/FairForge/vaultaire/cmd/vaultaire").Output()
	require.NoError(t, err)
	for _, line := range strings.Split(string(out), "\n") {
		assert.NotContains(t, line, "/cmd/tools/", "cmd/vaultaire links an operator tool")
		assert.NotEqual(t, "github.com/FairForge/vaultaire/internal/translog/witness", line, "cmd/vaultaire links the witness")
	}
}
