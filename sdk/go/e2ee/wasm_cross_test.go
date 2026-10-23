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
// code as native Go, for every construction of the hierarchy: WASM checks
// every golden of testdata/vectors.json, opens what native Go sealed
// (password wrap, recovery words, share, manifest and its chunks…), and
// native Go opens what WASM sealed (every wrap, an identity, a signature, a
// share, a manifest); a convergent chunk is byte-for-byte the same
// ciphertext on both (a phone and the CLI deduplicate against each other).
// Needs node on PATH (skipped otherwise, and in -short).
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

	// The hierarchy, sealed natively for WASM to open.
	vec := loadVectors(t)
	alice, err := IdentityFromKeys(vec.Identities["alice"].EncPrivate, vec.Identities["alice"].SigPrivate)
	require.NoError(t, err)
	bob, err := IdentityFromKeys(vec.Identities["bob"].EncPrivate, vec.Identities["bob"].SigPrivate)
	require.NoError(t, err)
	mk, err := RandomKey()
	require.NoError(t, err)
	rk, err := NewRecoveryKey()
	require.NoError(t, err)
	password := "a password typed on both sides"
	pwWrap, err := WrapMasterKeyWithPassword(mk, []byte(password), Interactive)
	require.NoError(t, err)
	mnemonic, err := RecoveryMnemonic(rk)
	require.NoError(t, err)
	spaceID, objectID := []byte("space-cross"), []byte("object-cross")
	share, err := NewShare(alice, bob.Public(), spaceID, space)
	require.NoError(t, err)
	objectKey, err := NewObjectKey()
	require.NoError(t, err)
	m := &Manifest{SpaceID: spaceID, ObjectID: objectID, Version: 1, Previous: make([]byte, 32)}
	var sealedChunks []string
	var chunkTexts []string
	for _, c := range []string{"native chunk one", "native chunk two", "three"} {
		ref, sc, err := PrepareChunk(space, []byte(c))
		require.NoError(t, err)
		m.Chunks = append(m.Chunks, ref)
		m.TotalSize += uint64(ref.Size)
		sealedChunks = append(sealedChunks, hex.EncodeToString(sc))
		chunkTexts = append(chunkTexts, hex.EncodeToString([]byte(c)))
	}
	manifest, err := SealManifest(objectKey, m)
	require.NoError(t, err)

	x := hex.EncodeToString
	in := map[string]any{
		"key": x(k), "ad": ad, "info": info, "box": x(box), "spaceKey": x(space), "chunk": x(chunk),
		"nativeChunk": x(nativeChunk), "priv": x(priv), "pub": x(pub), "sealed": x(sealed),
		"masterKey": x(mk), "recoveryKey": x(rk), "password": password, "passwordWrap": x(pwWrap), "mnemonic": mnemonic,
		"spaceId": x(spaceID), "objectId": x(objectID), "share": x(share), "objectKey": x(objectKey),
		"manifest": x(manifest), "sealedChunks": sealedChunks,
	}
	inPath := filepath.Join(dir, "in.json")
	raw, err := json.Marshal(in)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(inPath, raw, 0o600))
	vectorsAbs, err := filepath.Abs(vectorsPath)
	require.NoError(t, err)

	run := exec.Command(node, filepath.Join("testdata", "cross.mjs"), wasmExec, // #nosec G204 -- node on PATH, test fixture paths
		filepath.Join(root, "sdk", "js", "e2ee", "index.mjs"), wasm, inPath, vectorsAbs)
	res, err := run.Output()
	if ee, ok := err.(*exec.ExitError); ok {
		t.Fatalf("node: %v\n%s", err, ee.Stderr)
	}
	require.NoError(t, err)
	var got struct {
		Version string            `json:"version"`
		Goldens map[string]string `json:"goldens"`
		Opened  map[string]any    `json:"opened"`
		Sealed  map[string]any    `json:"sealed"`
		Errors  map[string]string `json:"errors"`
	}
	require.NoError(t, json.Unmarshal(res, &got), string(res))
	str := func(m map[string]any, k string) string {
		s, ok := m[k].(string)
		require.True(t, ok, "%s missing", k)
		return s
	}
	un := func(s string) []byte {
		b, err := hex.DecodeString(s)
		require.NoError(t, err)
		return b
	}
	assert.Equal(t, "stored.cloud/e2ee/v1", got.Version)

	// Every golden vector, through the binding.
	assert.GreaterOrEqual(t, len(got.Goldens), 25)
	for name, verdict := range got.Goldens {
		assert.Equal(t, "ok", verdict, "golden %s in WASM", name)
	}

	// Native → WASM.
	o := got.Opened
	assert.Equal(t, "from native", string(un(str(o, "box"))))
	assert.Equal(t, x(h[:]), str(o, "chunkHash"))
	cid, err := ChunkID(space, h[:])
	require.NoError(t, err)
	assert.Equal(t, x(cid), str(o, "chunkId"))
	assert.Equal(t, x(ck), str(o, "chunkKey"))
	assert.Equal(t, x(nativeChunk), str(o, "chunkSealed"), "a convergent chunk is the same ciphertext in WASM and native Go")
	assert.Equal(t, x(chunk), str(o, "chunk"))
	assert.Equal(t, "native to wasm", string(un(str(o, "sealed"))))
	assert.Equal(t, x(mk), str(o, "passwordWrap"))
	assert.Equal(t, x(rk), str(o, "recoveryKey"))
	assert.Equal(t, x(space), str(o, "share"))
	assert.Equal(t, []any{chunkTexts[0], chunkTexts[1], chunkTexts[2]}, o["manifestChunks"])

	// WASM → native.
	sl := got.Sealed
	pt, err := Open(k, un(str(sl, "box")), []byte(ad))
	require.NoError(t, err)
	assert.Equal(t, "from wasm", string(pt))
	pt, err = OpenSealed(priv, un(str(sl, "sealedToNative")), []byte(info), []byte(ad))
	require.NoError(t, err)
	assert.Equal(t, "from wasm to native", string(pt))
	wp, err := PublicKey(un(str(sl, "wasmPriv")))
	require.NoError(t, err)
	assert.Equal(t, str(sl, "wasmPub"), x(wp), "a WASM key pair is a valid X-Wing pair natively")
	assert.Equal(t, str(sl, "wasmPub"), str(sl, "wasmPubAgain"))

	got1, err := OpenMasterKeyWithPassword(un(str(sl, "passwordWrap")), []byte(password))
	require.NoError(t, err)
	assert.Equal(t, mk, got1)
	assert.Equal(t, mnemonic, str(sl, "mnemonic"))
	got1, err = OpenMasterKeyWithRecovery(rk, un(str(sl, "recoveryWrap")))
	require.NoError(t, err)
	assert.Equal(t, mk, got1)
	got1, err = OpenRecoveryKey(mk, un(str(sl, "recoveryKeyWrap")))
	require.NoError(t, err)
	assert.Equal(t, rk, got1)
	got1, err = OpenRecoveryKeyFromVault(un(str(sl, "vaultKey")), un(str(sl, "vaultWrap")))
	require.NoError(t, err)
	assert.Equal(t, rk, got1)

	wid, err := OpenIdentity(mk, un(str(sl, "identityEnc")), un(str(sl, "identitySig")))
	require.NoError(t, err)
	assert.Equal(t, str(sl, "identityEncPublic"), x(wid.EncPublic))
	assert.Equal(t, str(sl, "identitySigPublic"), x(wid.SigPublic))
	wfp, err := Fingerprint(wid.Public())
	require.NoError(t, err)
	assert.Equal(t, str(sl, "fingerprint"), x(wfp))
	require.NoError(t, Verify(wid.SigPublic, "cross", []byte("signed in wasm"), un(str(sl, "signature"))))

	got1, err = OpenSpaceKey(mk, spaceID, un(str(sl, "spaceKeyWrap")))
	require.NoError(t, err)
	assert.Equal(t, space, got1)
	got1, err = OpenShare(alice, un(str(sl, "share")), bob.Public(), spaceID)
	require.NoError(t, err)
	assert.Equal(t, space, got1, "a share signed in WASM opens natively")
	wok := un(str(sl, "objectKey"))
	got1, err = OpenObjectKey(space, spaceID, objectID, un(str(sl, "objectKeyWrap")))
	require.NoError(t, err)
	assert.Equal(t, wok, got1)
	wm, err := OpenManifest(wok, un(str(sl, "manifest")), spaceID, objectID, 1, nil)
	require.NoError(t, err)
	chunksAny, ok := sl["manifestChunks"].([]any)
	require.True(t, ok)
	require.Len(t, wm.Chunks, len(chunksAny))
	for i, c := range chunksAny {
		pt, err := wm.OpenChunkAt(i, un(c.(string)))
		require.NoError(t, err)
		assert.Contains(t, []string{"first chunk sealed in wasm", "second one"}, string(pt))
	}

	// Errors arrive as thrown Go messages.
	assert.Equal(t, ErrOpen.Error(), got.Errors["wrongPurpose"])
	assert.Equal(t, ErrKeySize.Error(), got.Errors["shortKey"])
}
