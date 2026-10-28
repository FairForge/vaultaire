//go:build js && wasm

// Command wasm is the key core (sdk/go/e2ee) compiled to WebAssembly for
// the browser and Node (keystone K2): the same Go code, no second
// implementation. It registers globalThis.storedE2EE; sdk/js/e2ee is the
// binding that loads it. Build: `make sdk-wasm` (GOOS=js GOARCH=wasm; Go's
// wasm_exec.js of the same toolchain ships beside the .wasm).
//
// Every function answers {value} or {error}: the binding throws the error.
// Bytes are Uint8Array both ways; a context, a mnemonic and a fingerprint's
// text are strings; a version or a size is a number; an identity, a chunk
// reference and a manifest are plain objects (identityJS, refJS,
// manifestJS).
package main

import (
	"crypto/sha256"
	"fmt"
	"syscall/js"

	"github.com/FairForge/vaultaire/internal/catalogue"
	"github.com/FairForge/vaultaire/sdk/go/e2ee"
)

// catalogues are the loaded public-catalogue filters (WP-EDGE-4 PR D),
// by handle: a filter of ten million chunks is ~12 MB, loaded once per
// isolate and asked per chunk without copying it again.
var (
	catalogues    = map[int]*catalogue.Filter{}
	nextCatalogue int
)

func bytesArg(args []js.Value, i int) []byte {
	if i >= len(args) || args[i].IsUndefined() || args[i].IsNull() {
		return nil
	}
	return bytesOf(args[i], fmt.Sprintf("argument %d", i))
}

func bytesOf(v js.Value, what string) []byte {
	if v.IsUndefined() || v.IsNull() {
		return nil
	}
	if !v.InstanceOf(js.Global().Get("Uint8Array")) {
		panic(what + " is not a Uint8Array")
	}
	b := make([]byte, v.Get("length").Int())
	js.CopyBytesToGo(b, v)
	return b
}

func stringArg(args []js.Value, i int) string {
	if i >= len(args) || args[i].Type() != js.TypeString {
		panic(fmt.Sprintf("argument %d is not a string", i))
	}
	return args[i].String()
}

// uintOf reads a non-negative integer number (a version, a size).
func uintOf(v js.Value, what string) uint64 {
	if v.Type() != js.TypeNumber {
		panic(what + " is not a number")
	}
	f := v.Float()
	if f < 0 || f != float64(uint64(f)) || f > 1<<53 {
		panic(what + " is not a non-negative integer")
	}
	return uint64(f)
}

func toJS(b []byte) js.Value {
	a := js.Global().Get("Uint8Array").New(len(b))
	js.CopyBytesToJS(a, b)
	return a
}

// fn wraps a function: a panic or an error becomes {error}.
func fn(f func(args []js.Value) (any, error)) js.Func {
	return js.FuncOf(func(_ js.Value, args []js.Value) (out any) {
		defer func() {
			if r := recover(); r != nil {
				out = map[string]any{"error": fmt.Sprint(r)}
			}
		}()
		v, err := f(args)
		if err != nil {
			return map[string]any{"error": err.Error()}
		}
		return map[string]any{"value": v}
	})
}

func value(b []byte, err error) (any, error) {
	if err != nil {
		return nil, err
	}
	return toJS(b), nil
}

func identityJS(id *e2ee.Identity) map[string]any {
	return map[string]any{
		"encPrivate": toJS(id.EncPrivate), "encPublic": toJS(id.EncPublic),
		"sigPrivate": toJS(id.SigPrivate), "sigPublic": toJS(id.SigPublic),
	}
}

func identityOf(v js.Value) *e2ee.Identity {
	id, err := e2ee.IdentityFromKeys(bytesOf(v.Get("encPrivate"), "encPrivate"), bytesOf(v.Get("sigPrivate"), "sigPrivate"))
	if err != nil {
		panic(err.Error())
	}
	return id
}

func publicOf(v js.Value) e2ee.PublicIdentity {
	return e2ee.PublicIdentity{EncPublic: bytesOf(v.Get("encPublic"), "encPublic"), SigPublic: bytesOf(v.Get("sigPublic"), "sigPublic")}
}

func refJS(r e2ee.ChunkRef) map[string]any {
	return map[string]any{"id": toJS(r.ID), "hash": toJS(r.Hash), "key": toJS(r.Key), "size": int(r.Size), "sealedSize": int(r.SealedSize)}
}

func manifestJS(m *e2ee.Manifest) map[string]any {
	chunks := make([]any, len(m.Chunks))
	for i, c := range m.Chunks {
		chunks[i] = refJS(c)
	}
	return map[string]any{
		"spaceId": toJS(m.SpaceID), "objectId": toJS(m.ObjectID), "version": float64(m.Version),
		"previous": toJS(m.Previous), "compressor": int(m.Compressor), "totalSize": float64(m.TotalSize), "chunks": chunks,
	}
}

func manifestOf(v js.Value) *e2ee.Manifest {
	m := &e2ee.Manifest{
		SpaceID: bytesOf(v.Get("spaceId"), "spaceId"), ObjectID: bytesOf(v.Get("objectId"), "objectId"),
		Version: uintOf(v.Get("version"), "version"), Previous: bytesOf(v.Get("previous"), "previous"),
		TotalSize: uintOf(v.Get("totalSize"), "totalSize"),
	}
	if c := v.Get("compressor"); !c.IsUndefined() {
		m.Compressor = byte(uintOf(c, "compressor"))
	}
	chunks := v.Get("chunks")
	for i := 0; i < chunks.Length(); i++ {
		c := chunks.Index(i)
		m.Chunks = append(m.Chunks, e2ee.ChunkRef{
			ID: bytesOf(c.Get("id"), "id"), Hash: bytesOf(c.Get("hash"), "hash"), Key: bytesOf(c.Get("key"), "key"),
			Size: uint32(uintOf(c.Get("size"), "size")), SealedSize: uint32(uintOf(c.Get("sealedSize"), "sealedSize")),
		})
	}
	return m
}

func kdfParams(v js.Value) e2ee.KDFParams {
	switch {
	case v.Type() == js.TypeString && v.String() == "sensitive":
		return e2ee.Sensitive
	case v.Type() == js.TypeString && v.String() == "interactive":
		return e2ee.Interactive
	case v.Type() == js.TypeObject:
		return e2ee.KDFParams{MemoryKiB: uint32(uintOf(v.Get("memoryKiB"), "memoryKiB")),
			Iterations: uint32(uintOf(v.Get("iterations"), "iterations")), Parallelism: uint8(uintOf(v.Get("parallelism"), "parallelism"))}
	}
	panic(`parameters are "sensitive", "interactive" or {memoryKiB, iterations, parallelism}`)
}

func main() {
	b := bytesArg
	api := map[string]any{
		"version": "stored.cloud/e2ee/v1",

		// The constructions.
		"randomKey": fn(func([]js.Value) (any, error) { return value(e2ee.RandomKey()) }),
		"seal":      fn(func(a []js.Value) (any, error) { return value(e2ee.Seal(b(a, 0), b(a, 1), b(a, 2))) }),
		"open":      fn(func(a []js.Value) (any, error) { return value(e2ee.Open(b(a, 0), b(a, 1), b(a, 2))) }),
		// The plain SHA-256 of a chunk: it derives the chunk key and is
		// checked on open, and it never leaves the device — the server is
		// given chunkId (keyed under the space), never this (RP37-15b).
		"deviceOnlyChunkHash": fn(func(a []js.Value) (any, error) {
			h := sha256.Sum256(b(a, 0))
			return value(h[:], nil)
		}),
		"chunkId":   fn(func(a []js.Value) (any, error) { return value(e2ee.ChunkID(b(a, 0), b(a, 1))) }),
		"chunkKey":  fn(func(a []js.Value) (any, error) { return value(e2ee.ChunkKey(b(a, 0), b(a, 1))) }),
		"sealChunk": fn(func(a []js.Value) (any, error) { return value(e2ee.SealChunk(b(a, 0), b(a, 1))) }),
		"openChunk": fn(func(a []js.Value) (any, error) { return value(e2ee.OpenChunk(b(a, 0), b(a, 1), b(a, 2))) }),
		"generateKeyPair": fn(func([]js.Value) (any, error) {
			priv, pub, err := e2ee.GenerateKeyPair()
			if err != nil {
				return nil, err
			}
			return map[string]any{"privateKey": toJS(priv), "publicKey": toJS(pub)}, nil
		}),
		"publicKey":  fn(func(a []js.Value) (any, error) { return value(e2ee.PublicKey(b(a, 0))) }),
		"sealTo":     fn(func(a []js.Value) (any, error) { return value(e2ee.SealTo(b(a, 0), b(a, 1), b(a, 2), b(a, 3))) }),
		"openSealed": fn(func(a []js.Value) (any, error) { return value(e2ee.OpenSealed(b(a, 0), b(a, 1), b(a, 2), b(a, 3))) }),

		// The account root.
		"wrapMasterKeyWithPassword": fn(func(a []js.Value) (any, error) {
			if len(a) < 3 {
				panic(`wrapMasterKeyWithPassword(masterKey, password, "sensitive" | "interactive" | {…})`)
			}
			return value(e2ee.WrapMasterKeyWithPassword(b(a, 0), b(a, 1), kdfParams(a[2])))
		}),
		"openMasterKeyWithPassword": fn(func(a []js.Value) (any, error) { return value(e2ee.OpenMasterKeyWithPassword(b(a, 0), b(a, 1))) }),
		"passwordWrapParams": fn(func(a []js.Value) (any, error) {
			p, err := e2ee.PasswordWrapParams(b(a, 0))
			if err != nil {
				return nil, err
			}
			return map[string]any{"memoryKiB": int(p.MemoryKiB), "iterations": int(p.Iterations), "parallelism": int(p.Parallelism)}, nil
		}),
		"newRecoveryKey": fn(func([]js.Value) (any, error) { return value(e2ee.NewRecoveryKey()) }),
		"recoveryMnemonic": fn(func(a []js.Value) (any, error) {
			s, err := e2ee.RecoveryMnemonic(b(a, 0))
			if err != nil {
				return nil, err
			}
			return s, nil
		}),
		"parseRecoveryMnemonic":     fn(func(a []js.Value) (any, error) { return value(e2ee.ParseRecoveryMnemonic(stringArg(a, 0))) }),
		"wrapMasterKeyWithRecovery": fn(func(a []js.Value) (any, error) { return value(e2ee.WrapMasterKeyWithRecovery(b(a, 0), b(a, 1))) }),
		"openMasterKeyWithRecovery": fn(func(a []js.Value) (any, error) { return value(e2ee.OpenMasterKeyWithRecovery(b(a, 0), b(a, 1))) }),
		"wrapRecoveryKey":           fn(func(a []js.Value) (any, error) { return value(e2ee.WrapRecoveryKey(b(a, 0), b(a, 1))) }),
		"openRecoveryKey":           fn(func(a []js.Value) (any, error) { return value(e2ee.OpenRecoveryKey(b(a, 0), b(a, 1))) }),
		"newVaultKey":               fn(func([]js.Value) (any, error) { return value(e2ee.NewVaultKey()) }),
		"wrapRecoveryKeyForVault":   fn(func(a []js.Value) (any, error) { return value(e2ee.WrapRecoveryKeyForVault(b(a, 0), b(a, 1))) }),
		"openRecoveryKeyFromVault":  fn(func(a []js.Value) (any, error) { return value(e2ee.OpenRecoveryKeyFromVault(b(a, 0), b(a, 1))) }),

		// The identity.
		"generateIdentity": fn(func([]js.Value) (any, error) {
			id, err := e2ee.GenerateIdentity()
			if err != nil {
				return nil, err
			}
			return identityJS(id), nil
		}),
		"identityFromKeys": fn(func(a []js.Value) (any, error) {
			id, err := e2ee.IdentityFromKeys(b(a, 0), b(a, 1))
			if err != nil {
				return nil, err
			}
			return identityJS(id), nil
		}),
		"fingerprint": fn(func(a []js.Value) (any, error) { return value(e2ee.Fingerprint(publicOf(a[0]))) }),
		"fingerprintString": fn(func(a []js.Value) (any, error) {
			s, err := e2ee.FingerprintString(b(a, 0))
			if err != nil {
				return nil, err
			}
			return s, nil
		}),
		"sign": fn(func(a []js.Value) (any, error) { return value(e2ee.Sign(b(a, 0), stringArg(a, 1), b(a, 2))) }),
		"verify": fn(func(a []js.Value) (any, error) {
			if err := e2ee.Verify(b(a, 0), stringArg(a, 1), b(a, 2), b(a, 3)); err != nil {
				return nil, err
			}
			return true, nil
		}),
		"wrapIdentity": fn(func(a []js.Value) (any, error) {
			we, ws, err := e2ee.WrapIdentity(b(a, 0), identityOf(a[1]))
			if err != nil {
				return nil, err
			}
			return map[string]any{"wrappedEnc": toJS(we), "wrappedSig": toJS(ws)}, nil
		}),
		"openIdentity": fn(func(a []js.Value) (any, error) {
			id, err := e2ee.OpenIdentity(b(a, 0), b(a, 1), b(a, 2))
			if err != nil {
				return nil, err
			}
			return identityJS(id), nil
		}),

		// Spaces and shares.
		"newSpaceKey":  fn(func([]js.Value) (any, error) { return value(e2ee.NewSpaceKey()) }),
		"wrapSpaceKey": fn(func(a []js.Value) (any, error) { return value(e2ee.WrapSpaceKey(b(a, 0), b(a, 1), b(a, 2))) }),
		"openSpaceKey": fn(func(a []js.Value) (any, error) { return value(e2ee.OpenSpaceKey(b(a, 0), b(a, 1), b(a, 2))) }),
		"newShare": fn(func(a []js.Value) (any, error) {
			return value(e2ee.NewShare(identityOf(a[0]), publicOf(a[1]), b(a, 2), b(a, 3)))
		}),
		"openShare": fn(func(a []js.Value) (any, error) {
			return value(e2ee.OpenShare(identityOf(a[0]), b(a, 1), publicOf(a[2]), b(a, 3)))
		}),

		// Objects and manifests.
		"newObjectKey":  fn(func([]js.Value) (any, error) { return value(e2ee.NewObjectKey()) }),
		"wrapObjectKey": fn(func(a []js.Value) (any, error) { return value(e2ee.WrapObjectKey(b(a, 0), b(a, 1), b(a, 2), b(a, 3))) }),
		"openObjectKey": fn(func(a []js.Value) (any, error) { return value(e2ee.OpenObjectKey(b(a, 0), b(a, 1), b(a, 2), b(a, 3))) }),
		"prepareChunk": fn(func(a []js.Value) (any, error) {
			ref, sealed, err := e2ee.PrepareChunk(b(a, 0), b(a, 1))
			if err != nil {
				return nil, err
			}
			return map[string]any{"ref": refJS(ref), "sealed": toJS(sealed)}, nil
		}),
		"sealManifest": fn(func(a []js.Value) (any, error) { return value(e2ee.SealManifest(b(a, 0), manifestOf(a[1]))) }),
		"manifestHash": fn(func(a []js.Value) (any, error) { return value(e2ee.ManifestHash(b(a, 0)), nil) }),
		"openManifest": fn(func(a []js.Value) (any, error) {
			var seen *e2ee.ManifestHead
			if len(a) > 5 && !a[5].IsNull() && !a[5].IsUndefined() {
				seen = &e2ee.ManifestHead{Version: uintOf(a[5].Get("version"), "seen.version"), Hash: bytesOf(a[5].Get("hash"), "seen.hash")}
			}
			m, err := e2ee.OpenManifest(b(a, 0), b(a, 1), b(a, 2), b(a, 3), uintOf(a[4], "version"), seen)
			if err != nil {
				return nil, err
			}
			return manifestJS(m), nil
		}),
		"openChunkAt": fn(func(a []js.Value) (any, error) {
			return value(manifestOf(a[0]).OpenChunkAt(int(uintOf(a[1], "index")), b(a, 2)))
		}),

		// The public catalogue's filter (internal/catalogue, the same Go
		// code as the control plane's job): load once, ask per chunk.
		"catalogueLoad": fn(func(a []js.Value) (any, error) {
			f, err := catalogue.Unmarshal(b(a, 0))
			if err != nil {
				return nil, err
			}
			nextCatalogue++
			h := nextCatalogue
			catalogues[h] = f
			return map[string]any{"handle": h, "version": float64(f.Version())}, nil
		}),
		"catalogueFree": fn(func(a []js.Value) (any, error) {
			delete(catalogues, int(uintOf(a[0], "handle")))
			return true, nil
		}),
		"catalogueContains": fn(func(a []js.Value) (any, error) {
			f, ok := catalogues[int(uintOf(a[0], "handle"))]
			if !ok {
				return nil, fmt.Errorf("no catalogue filter with that handle")
			}
			return f.Contains(stringArg(a, 1))
		}),
	}
	js.Global().Set("storedE2EE", js.ValueOf(api))
	select {} // the exported functions live as long as the page
}
