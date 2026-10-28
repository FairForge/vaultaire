//go:build js && wasm

// Command ecwasm is the Reed-Solomon stripe codec of the second copy at the
// edge (internal/ec — the WP-VAULT-1 codec, WP-EDGE-4 PR B) compiled to
// WebAssembly: the same Go code, no second implementation, pinned by the
// golden vectors (internal/ec/testdata/ec_golden.json, asserted in Go and
// in node by edge/s3-ingest/test/ec.test.js). Built by
// scripts/build-client.sh (client `edge-ec`) into ec.wasm; its sha256 is a
// client_build row of the transparency log when it ships.
//
// The 2026-10-27 SPEED bench put RS 4+2 / 8+4 at 26–29 MB/s per stream in a
// Worker (plan 40.10 slice 3): the k+m shapes run on a Go data node or the
// async mover, never in the Worker; this build is for an SDK client and
// for the measurement (WP-EDGE-4 Step 0.2).
//
// It registers globalThis.storedEC:
//   - encodeStripe(k, m, data) → the m parity pieces of one stripe,
//     concatenated (data = the stripe's k pieces concatenated, k×S bytes);
//   - reconstruct(k, m, pieces) → the k data pieces concatenated, from an
//     array of k+m pieces with null for a missing one (any k present);
//   - goldenBody(seed, n) → the golden tests' deterministic body.
//
// Every function answers {value} or {error}, as sdk/wasm does.
package main

import (
	"errors"
	"fmt"
	"syscall/js"

	"github.com/klauspost/reedsolomon"

	"github.com/FairForge/vaultaire/internal/crypto"
	"github.com/FairForge/vaultaire/internal/ec"
)

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

func toJS(b []byte) js.Value {
	a := js.Global().Get("Uint8Array").New(len(b))
	js.CopyBytesToJS(a, b)
	return a
}

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

func geometry(a []js.Value) (int, int, error) {
	if len(a) < 3 {
		return 0, 0, errors.New("k, m and the pieces are required")
	}
	k, m := a[0].Int(), a[1].Int()
	if k < 1 || m < 1 || k+m > 256 {
		return 0, 0, fmt.Errorf("k=%d m=%d is not a geometry", k, m)
	}
	return k, m, nil
}

func main() {
	api := map[string]any{
		"encodeStripe": fn(func(a []js.Value) (any, error) {
			k, m, err := geometry(a)
			if err != nil {
				return nil, err
			}
			data := bytesOf(a[2], "data")
			if len(data) == 0 || len(data)%k != 0 {
				return nil, fmt.Errorf("the stripe is %d bytes: not k equal pieces", len(data))
			}
			s := len(data) / k
			l := ec.Layout{K: k, M: m, Stripe: s, Size: int64(len(data))}
			pieces := make([][]byte, k+m)
			for i := 0; i < k; i++ {
				pieces[i] = data[i*s : (i+1)*s]
			}
			for j := 0; j < m; j++ {
				pieces[k+j] = make([]byte, s)
			}
			if err := l.EncodeStripe(pieces); err != nil {
				return nil, err
			}
			out := make([]byte, 0, m*s)
			for j := 0; j < m; j++ {
				out = append(out, pieces[k+j]...)
			}
			return toJS(out), nil
		}),
		"reconstruct": fn(func(a []js.Value) (any, error) {
			k, m, err := geometry(a)
			if err != nil {
				return nil, err
			}
			arr := a[2]
			if arr.Get("length").Int() != k+m {
				return nil, fmt.Errorf("%d pieces for %d+%d", arr.Get("length").Int(), k, m)
			}
			pieces := make([][]byte, k+m)
			for i := range pieces {
				pieces[i] = bytesOf(arr.Index(i), fmt.Sprintf("piece %d", i))
			}
			enc, err := reedsolomon.New(k, m)
			if err != nil {
				return nil, err
			}
			if err := enc.ReconstructData(pieces); err != nil {
				return nil, err
			}
			var out []byte
			for i := 0; i < k; i++ {
				out = append(out, pieces[i]...)
			}
			return toJS(out), nil
		}),
		"goldenBody": fn(func(a []js.Value) (any, error) {
			return toJS(crypto.GoldenBody(uint64(a[0].Float()), a[1].Int())), nil
		}),
	}
	js.Global().Set("storedEC", js.ValueOf(api))
	select {}
}
