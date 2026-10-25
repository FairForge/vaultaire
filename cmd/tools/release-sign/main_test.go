package main

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/FairForge/vaultaire/internal/release"
	"github.com/FairForge/vaultaire/internal/translog"
)

// WP-SUPPLY-1 A.2/A.4: keygen → sign → verify through the verbs; the dark
// shape (no key source = a refusal that says so, nothing written); a key
// source set but unusable = a refusal; a tampered artefact, a wrong key and
// a truncated signature refused; -log proves the artefact is a build the
// transparency log lists.

func env(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func write(t *testing.T, dir, name string, body []byte) string {
	t.Helper()
	p := filepath.Join(dir, name)
	require.NoError(t, os.WriteFile(p, body, 0o600))
	return p
}

func TestReleaseSign_KeygenSignVerify(t *testing.T) {
	dir := t.TempDir()
	keyFile := filepath.Join(dir, "release.key")

	// keygen: the signer key to -out (0600, never printed), the verifier key on stdout.
	old := os.Stdout
	r, w, err := os.Pipe()
	require.NoError(t, err)
	os.Stdout = w
	kerr := keygen([]string{"-out", keyFile})
	_ = w.Close()
	os.Stdout = old
	require.NoError(t, kerr)
	var vkeyBuf bytes.Buffer
	_, _ = vkeyBuf.ReadFrom(r)
	vkey := strings.TrimSpace(vkeyBuf.String())
	assert.True(t, strings.HasPrefix(vkey, release.Name+"+"), "the verifier key is named for the release: %q", vkey)
	st, err := os.Stat(keyFile)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), st.Mode().Perm())
	assert.Error(t, keygen([]string{"-out", keyFile}), "never over an existing file")
	assert.Error(t, keygen(nil), "-out is required")

	// Three artefacts: a binary, an SBOM, a client.
	bin := write(t, dir, "vaultaire-linux", bytes.Repeat([]byte{0x7f, 'E', 'L', 'F'}, 4096))
	sbom := write(t, dir, "vaultaire.cdx.json", []byte(`{"bomFormat":"CycloneDX"}`))
	wasm := write(t, dir, "e2ee.wasm", []byte("\x00asm\x01\x00\x00\x00"))

	// sign, the key from RELEASE_SIGNING_KEY_FILE.
	require.NoError(t, sign([]string{bin, sbom, wasm}, env(map[string]string{"RELEASE_SIGNING_KEY_FILE": keyFile}), os.ReadFile))
	for _, p := range []string{bin, sbom, wasm} {
		sig, err := os.ReadFile(p + ".sig")
		require.NoError(t, err, p)
		assert.True(t, bytes.HasPrefix(sig, []byte(release.Name+"\nname "+filepath.Base(p)+"\n")), "%s.sig: %s", p, sig)
		var out bytes.Buffer
		require.NoError(t, verify(&out, []string{"-vkey", vkey, p, p + ".sig"}), p)
		assert.Contains(t, out.String(), "OK "+p+": signed as "+filepath.Base(p))
	}

	// verify with the key from a file (@file), the shape a reader uses offline.
	vkeyFile := write(t, dir, "release.vkey", []byte(vkey+"\n"))
	var out bytes.Buffer
	require.NoError(t, verify(&out, []string{"-vkey", "@" + vkeyFile, bin, bin + ".sig"}))

	t.Run("a tampered artefact is refused", func(t *testing.T) {
		b, _ := os.ReadFile(bin)
		b[100] ^= 0x01
		bad := write(t, t.TempDir(), "vaultaire-linux", b)
		err := verify(&bytes.Buffer{}, []string{"-vkey", vkey, bad, bin + ".sig"})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "does not match its signature")
	})
	t.Run("the wrong key is refused", func(t *testing.T) {
		_, other, err := release.GenerateKey(rand.Reader)
		require.NoError(t, err)
		err = verify(&bytes.Buffer{}, []string{"-vkey", other, bin, bin + ".sig"})
		require.Error(t, err)
	})
	t.Run("a truncated signature is refused", func(t *testing.T) {
		sig, _ := os.ReadFile(bin + ".sig")
		cut := write(t, t.TempDir(), "vaultaire-linux.sig", sig[:len(sig)-16])
		require.Error(t, verify(&bytes.Buffer{}, []string{"-vkey", vkey, bin, cut}))
	})
	t.Run("another artefact's signature is refused", func(t *testing.T) {
		require.Error(t, verify(&bytes.Buffer{}, []string{"-vkey", vkey, bin, sbom + ".sig"}))
	})
	t.Run("the log's key is not the release key", func(t *testing.T) {
		_, lv, err := translog.GenerateKey(rand.Reader)
		require.NoError(t, err)
		require.Error(t, verify(&bytes.Buffer{}, []string{"-vkey", lv, bin, bin + ".sig"}))
	})
	t.Run("-vkey is required", func(t *testing.T) {
		require.Error(t, verify(&bytes.Buffer{}, []string{bin, bin + ".sig"}))
	})
}

func TestReleaseSign_DarkWithoutAKeyAndRefusesAnUnusableSource(t *testing.T) {
	dir := t.TempDir()
	bin := write(t, dir, "vaultaire-linux", []byte("bytes"))

	err := sign([]string{bin}, env(nil), os.ReadFile)
	require.ErrorIs(t, err, release.ErrNoSigningKey, "no source: a clear refusal")
	_, statErr := os.Stat(bin + ".sig")
	assert.True(t, os.IsNotExist(statErr), "nothing written")

	err = sign([]string{bin}, env(map[string]string{"RELEASE_SIGNING_KEY_FILE": filepath.Join(dir, "missing")}), os.ReadFile)
	require.ErrorIs(t, err, release.ErrSigningKeySource)
	err = sign([]string{bin}, env(map[string]string{"RELEASE_SIGNING_KEY": "not-a-key"}), os.ReadFile)
	require.ErrorIs(t, err, release.ErrSigningKeySource)
	assert.NotContains(t, err.Error(), "not-a-key")
	_, statErr = os.Stat(bin + ".sig")
	assert.True(t, os.IsNotExist(statErr))

	// A .sig is never signed; one bad artefact writes nothing for the others.
	skey, _, err := release.GenerateKey(rand.Reader)
	require.NoError(t, err)
	e := env(map[string]string{"RELEASE_SIGNING_KEY": skey})
	require.Error(t, sign([]string{bin, bin + ".sig"}, e, os.ReadFile))
	require.Error(t, sign([]string{bin, filepath.Join(dir, "absent")}, e, os.ReadFile))
	_, statErr = os.Stat(bin + ".sig")
	assert.True(t, os.IsNotExist(statErr), "all or none")
}

// serveLog serves a memory log signed by skey (the translog-verify tests' harness).
func serveLog(t *testing.T, m *translog.Memory, skey string) string {
	t.Helper()
	s, err := translog.NewSigner(skey)
	require.NoError(t, err)
	mux := http.NewServeMux()
	mux.Handle("/translog/", http.StripPrefix("/translog/", translog.NewHandler(&translog.MemorySource{Log: m, Signer: s})))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv.URL + "/translog"
}

func TestReleaseSign_VerifyAgainstTheLog(t *testing.T) {
	dir := t.TempDir()
	skey, vkey, err := release.GenerateKey(rand.Reader)
	require.NoError(t, err)
	s, err := release.NewSigner(skey)
	require.NoError(t, err)

	server := write(t, dir, "vaultaire-linux", bytes.Repeat([]byte("server"), 1000))
	wasm := write(t, dir, "e2ee.wasm", bytes.Repeat([]byte("wasm"), 1000))
	unlisted := write(t, dir, "vaultaire-other", bytes.Repeat([]byte("other"), 1000))
	for _, p := range []string{server, wasm, unlisted} {
		sig, err := release.Sign(s, p)
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(p+".sig", sig, 0o600))
	}

	// The log lists the server binary and the wasm client, nothing else.
	m := &translog.Memory{}
	for i := 0; i < 5; i++ {
		_, err := m.Append([]byte(fmt.Sprintf("filler-%d", i)))
		require.NoError(t, err)
	}
	sum := func(p string) string {
		b, _ := os.ReadFile(p)
		h := sha256.Sum256(b)
		return hex.EncodeToString(h[:])
	}
	body, err := translog.Encode(translog.ServerBuild{GitSHA: "abcdef123456", BinarySHA256: sum(server), BuildTime: "2026-10-25T00:00:00Z", GoVersion: "go1.26.10"}, time.Now())
	require.NoError(t, err)
	_, err = m.Append(body)
	require.NoError(t, err)
	body, err = translog.Encode(translog.ClientBuild{Client: translog.ClientSDKWasm, Version: "0.2.0", SourceCommit: strings.Repeat("a", 40),
		SHA256: sum(wasm), Toolchain: "go1.26.10", Recipe: "scripts/build-client.sh"}, time.Now())
	require.NoError(t, err)
	_, err = m.Append(body)
	require.NoError(t, err)
	lskey, lvkey, err := translog.GenerateKey(rand.Reader)
	require.NoError(t, err)
	url := serveLog(t, m, lskey)

	var out bytes.Buffer
	require.NoError(t, verify(&out, []string{"-vkey", vkey, "-log", url, "-log-vkey", lvkey, server, server + ".sig"}))
	assert.Contains(t, out.String(), "server build abcdef123456 listed at entry 5 — inclusion OK")
	out.Reset()
	require.NoError(t, verify(&out, []string{"-vkey", vkey, "-log", url, "-log-vkey", lvkey, "-client", "sdk-wasm", wasm, wasm + ".sig"}))
	assert.Contains(t, out.String(), "client sdk-wasm 0.2.0 listed at entry 6 — inclusion OK")

	err = verify(&bytes.Buffer{}, []string{"-vkey", vkey, "-log", url, "-log-vkey", lvkey, unlisted, unlisted + ".sig"})
	require.Error(t, err, "a signature alone is not enough with -log: the build must be in the log")
	assert.Contains(t, err.Error(), "build not in the log")
	err = verify(&bytes.Buffer{}, []string{"-vkey", vkey, "-log", url, "-log-vkey", lvkey, "-client", "sdk-js", wasm, wasm + ".sig"})
	require.Error(t, err, "the wrong client name")
	err = verify(&bytes.Buffer{}, []string{"-vkey", vkey, "-log", url, server, server + ".sig"})
	require.Error(t, err, "-log without -log-vkey")
	_, otherLog, _ := translog.GenerateKey(rand.Reader)
	err = verify(&bytes.Buffer{}, []string{"-vkey", vkey, "-log", url, "-log-vkey", otherLog, server, server + ".sig"})
	require.Error(t, err, "a checkpoint not signed by the pinned log key")
}
