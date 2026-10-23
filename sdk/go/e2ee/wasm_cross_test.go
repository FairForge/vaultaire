package e2ee

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestE2EE_WASMMatchesNative: the key core compiled to WebAssembly (sdk/wasm)
// and loaded through the real JS binding (sdk/js/e2ee) in Node is the same
// code as native Go — it opens what native Go sealed, native Go opens what
// it sealed, and a convergent chunk is byte-for-byte the same ciphertext on
// both (a phone and the CLI deduplicate against each other). Needs node on
// PATH (skipped otherwise, and in -short).
func TestE2EE_WASMMatchesNative(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the WASM and runs node")
	}
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not on PATH")
	}
	goBin, err := exec.LookPath("go")
	require.NoError(t, err)
	root, err := filepath.Abs(filepath.Join("..", "..", ".."))
	require.NoError(t, err)
	dir := t.TempDir()

	// Build the WASM with the same toolchain, and use that toolchain's wasm_exec.js.
	wasm := filepath.Join(dir, "e2ee.wasm")
	build := exec.Command(goBin, "build", "-o", wasm, "./sdk/wasm") // #nosec G204 -- the go toolchain on PATH, fixed arguments
	build.Dir = root
	build.Env = append(os.Environ(), "GOOS=js", "GOARCH=wasm")
	out, err := build.CombinedOutput()
	require.NoError(t, err, string(out))
	gr := exec.Command(goBin, "env", "GOROOT") // #nosec G204 -- as above
	gr.Dir = root
	goroot, err := gr.Output()
	require.NoError(t, err)
	wasmExec := filepath.Join(strings.TrimSpace(string(goroot)), "lib", "wasm", "wasm_exec.js")
	require.FileExists(t, wasmExec)

	// Native side.
	k, err := RandomKey()
	require.NoError(t, err)
	space, err := RandomKey()
	require.NoError(t, err)
	ad, info := "stored.cloud/e2ee/v1/object-key", "share space-key"
	box, err := Seal(k, []byte("from native"), []byte(ad))
	require.NoError(t, err)
	chunk := bytes.Repeat([]byte("a chunk both sides must seal identically "), 500)
	h := sha256.Sum256(chunk)
	ck, err := ChunkKey(space, h[:])
	require.NoError(t, err)
	nativeChunk, err := SealChunk(ck, chunk)
	require.NoError(t, err)
	priv, pub, err := GenerateKeyPair()
	require.NoError(t, err)
	sealed, err := SealTo(pub, []byte("native to wasm"), []byte(info), []byte(ad))
	require.NoError(t, err)

	x := hex.EncodeToString
	in := map[string]string{
		"key": x(k), "ad": ad, "info": info, "box": x(box), "spaceKey": x(space), "chunk": x(chunk),
		"nativeChunk": x(nativeChunk), "priv": x(priv), "pub": x(pub), "sealed": x(sealed),
	}
	inPath := filepath.Join(dir, "in.json")
	raw, err := json.Marshal(in)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(inPath, raw, 0o600))

	run := exec.Command(node, filepath.Join("testdata", "cross.mjs"), wasmExec, // #nosec G204 -- node on PATH, test fixture paths
		filepath.Join(root, "sdk", "js", "e2ee", "index.mjs"), wasm, inPath)
	res, err := run.Output()
	if ee, ok := err.(*exec.ExitError); ok {
		t.Fatalf("node: %v\n%s", err, ee.Stderr)
	}
	require.NoError(t, err)
	var got map[string]string
	require.NoError(t, json.Unmarshal(res, &got), string(res))

	un := func(s string) []byte {
		b, err := hex.DecodeString(s)
		require.NoError(t, err)
		return b
	}
	assert.Equal(t, "stored.cloud/e2ee/v1", got["version"])
	// Native → WASM.
	assert.Equal(t, "from native", string(un(got["boxOpened"])))
	assert.Equal(t, x(h[:]), got["chunkHash"])
	cid, err := ChunkID(space, h[:])
	require.NoError(t, err)
	assert.Equal(t, x(cid), got["chunkId"])
	assert.Equal(t, x(ck), got["chunkKey"])
	assert.Equal(t, x(nativeChunk), got["chunkSealed"], "a convergent chunk is the same ciphertext in WASM and native Go")
	assert.Equal(t, x(chunk), got["chunkOpened"])
	assert.Equal(t, "native to wasm", string(un(got["sealedOpened"])))
	// WASM → native.
	pt, err := Open(k, un(got["box"]), []byte(ad))
	require.NoError(t, err)
	assert.Equal(t, "from wasm", string(pt))
	pt, err = OpenSealed(priv, un(got["sealedToNative"]), []byte(info), []byte(ad))
	require.NoError(t, err)
	assert.Equal(t, "from wasm to native", string(pt))
	wp, err := PublicKey(un(got["wasmPriv"]))
	require.NoError(t, err)
	assert.Equal(t, got["wasmPub"], x(wp), "a WASM key pair is a valid X-Wing pair natively")
	assert.Equal(t, got["wasmPub"], got["wasmPubAgain"])
	// Errors arrive as thrown Go messages.
	assert.Equal(t, ErrOpen.Error(), got["wrongPurpose"])
	assert.Equal(t, ErrKeySize.Error(), got["shortKey"])
}
