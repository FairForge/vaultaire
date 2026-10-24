package translog

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// K4c-3b (Stage 1a Prompt 36; plan 43.6: "client build hashes … clients
// refuse an unlisted build"): the client_build entry is a typed struct with
// an allow-list — client from a CLOSED list, fixed-shape version, commit,
// sha256, toolchain and recipe. No free text can enter the public log.
func TestClientBuildEntry_ClosedListAndShape(t *testing.T) {
	// Arrange
	good := ClientBuild{Client: ClientSDKWasm, Version: "0.1.0",
		SourceCommit: strings.Repeat("ab", 20), SHA256: strings.Repeat("a7", 32),
		Toolchain: "go1.26.10", Recipe: "scripts/build-client.sh"}

	// Act
	body, err := Encode(good, time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC))

	// Assert: the good entry encodes and decodes to itself; its dedup key is
	// (client, sha256).
	require.NoError(t, err)
	d, err := Decode(body)
	require.NoError(t, err)
	assert.Equal(t, good, d.Entry)
	assert.Equal(t, "client_build:sdk-wasm:"+strings.Repeat("a7", 32), good.DedupKey())
	js := good
	js.Client = ClientSDKJS
	js.Toolchain = "none"
	_, err = Encode(js, time.Now())
	assert.NoError(t, err, `"none" for a file published as-is`)

	// Every departure from the shape is refused.
	for name, mutate := range map[string]func(*ClientBuild){
		"an unknown client":    func(b *ClientBuild) { b.Client = "sdk-evil" },
		"a free-text version":  func(b *ClientBuild) { b.Version = "latest and greatest" },
		"a short commit":       func(b *ClientBuild) { b.SourceCommit = "abc123" },
		"an upper-case sha":    func(b *ClientBuild) { b.SHA256 = strings.ToUpper(b.SHA256) },
		"a free-text tool":     func(b *ClientBuild) { b.Toolchain = "whatever I had installed" },
		"a recipe outside":     func(b *ClientBuild) { b.Recipe = "https://evil.example/build.sh" },
		"a shell in a recipe":  func(b *ClientBuild) { b.Recipe = "scripts/x.sh; rm -rf /" },
		"an empty client":      func(b *ClientBuild) { b.Client = "" },
		"an e-mail in version": func(b *ClientBuild) { b.Version = "isaac@example.com" },
	} {
		bad := good
		mutate(&bad)
		_, err := Encode(bad, time.Now())
		assert.Error(t, err, name)
	}

	// An unknown field in a decoded body is refused (the allow-list).
	_, err = Decode([]byte(`{"kind":"client_build","v":1,"at":"2026-10-23T12:00:00Z","entry":{"client":"sdk-wasm","version":"0.1.0","source_commit":"` +
		strings.Repeat("ab", 20) + `","sha256":"` + strings.Repeat("07", 32) + `","toolchain":"go1.26.10","recipe":"scripts/build-client.sh","statement":"hi"}}`))
	assert.Error(t, err)
	assert.Contains(t, Kinds(), KindClientBuild)
	assert.Equal(t, []string{"client", "version", "source_commit", "sha256", "toolchain", "recipe"}, AllowedFields(KindClientBuild))
}
