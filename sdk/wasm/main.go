//go:build js && wasm

// Command wasm is the key core (sdk/go/e2ee) compiled to WebAssembly for
// the browser and Node (keystone K2): the same Go code, no second
// implementation. It registers globalThis.storedE2EE; sdk/js/e2ee is the
// binding that loads it. Build: `make sdk-wasm` (GOOS=js GOARCH=wasm; Go's
// wasm_exec.js of the same toolchain ships beside the .wasm).
//
// Every function takes and returns Uint8Array and answers {value} or
// {error}: the binding throws the error.
package main

import (
	"crypto/sha256"
	"fmt"
	"syscall/js"

	"github.com/FairForge/vaultaire/sdk/go/e2ee"
)

func bytesArg(args []js.Value, i int) []byte {
	if i >= len(args) || args[i].IsUndefined() || args[i].IsNull() {
		return nil
	}
	v := args[i]
	if !v.InstanceOf(js.Global().Get("Uint8Array")) {
		panic(fmt.Sprintf("argument %d is not a Uint8Array", i))
	}
	b := make([]byte, v.Get("length").Int())
	js.CopyBytesToGo(b, v)
	return b
}

func toJS(b []byte) js.Value {
	a := js.Global().Get("Uint8Array").New(len(b))
	js.CopyBytesToJS(a, b)
	return a
}

// fn wraps a function: a panic or an error becomes {error}.
func fn(f func(args []js.Value) (map[string]any, error)) js.Func {
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
		return v
	})
}

func value(b []byte, err error) (map[string]any, error) {
	if err != nil {
		return nil, err
	}
	return map[string]any{"value": toJS(b)}, nil
}

func main() {
	api := map[string]any{
		"version": "stored.cloud/e2ee/v1",
		"randomKey": fn(func([]js.Value) (map[string]any, error) {
			return value(e2ee.RandomKey())
		}),
		"seal": fn(func(a []js.Value) (map[string]any, error) {
			return value(e2ee.Seal(bytesArg(a, 0), bytesArg(a, 1), bytesArg(a, 2)))
		}),
		"open": fn(func(a []js.Value) (map[string]any, error) {
			return value(e2ee.Open(bytesArg(a, 0), bytesArg(a, 1), bytesArg(a, 2)))
		}),
		// The plain SHA-256 of a chunk: it derives the chunk key and is
		// checked on open, and it never leaves the device — the server is
		// given chunkId (keyed under the space), never this (RP37-15b).
		"deviceOnlyChunkHash": fn(func(a []js.Value) (map[string]any, error) {
			h := sha256.Sum256(bytesArg(a, 0))
			return value(h[:], nil)
		}),
		"chunkId": fn(func(a []js.Value) (map[string]any, error) {
			return value(e2ee.ChunkID(bytesArg(a, 0), bytesArg(a, 1)))
		}),
		"chunkKey": fn(func(a []js.Value) (map[string]any, error) {
			return value(e2ee.ChunkKey(bytesArg(a, 0), bytesArg(a, 1)))
		}),
		"sealChunk": fn(func(a []js.Value) (map[string]any, error) {
			return value(e2ee.SealChunk(bytesArg(a, 0), bytesArg(a, 1)))
		}),
		"openChunk": fn(func(a []js.Value) (map[string]any, error) {
			return value(e2ee.OpenChunk(bytesArg(a, 0), bytesArg(a, 1), bytesArg(a, 2)))
		}),
		"generateKeyPair": fn(func([]js.Value) (map[string]any, error) {
			priv, pub, err := e2ee.GenerateKeyPair()
			if err != nil {
				return nil, err
			}
			return map[string]any{"privateKey": toJS(priv), "publicKey": toJS(pub)}, nil
		}),
		"publicKey": fn(func(a []js.Value) (map[string]any, error) {
			return value(e2ee.PublicKey(bytesArg(a, 0)))
		}),
		"sealTo": fn(func(a []js.Value) (map[string]any, error) {
			return value(e2ee.SealTo(bytesArg(a, 0), bytesArg(a, 1), bytesArg(a, 2), bytesArg(a, 3)))
		}),
		"openSealed": fn(func(a []js.Value) (map[string]any, error) {
			return value(e2ee.OpenSealed(bytesArg(a, 0), bytesArg(a, 1), bytesArg(a, 2), bytesArg(a, 3)))
		}),
	}
	js.Global().Set("storedE2EE", js.ValueOf(api))
	select {} // the exported functions live as long as the page
}
