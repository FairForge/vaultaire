//go:build js && wasm

// Command chunkerwasm is the FROZEN chunker (internal/crypto/chunker.go,
// WP-R8-4: restic/chunker v0.4.0, polynomial 0x2ADD89E3B790BB, min 1 MiB,
// average bits 20, max 16 MiB) compiled to WebAssembly for the edge codec's
// data-plane host (WP-EDGE-2, plan 40.10 slice 2): the same Go code, no
// second implementation, so every boundary the Worker cuts is the boundary
// the origin would cut — pinned by the golden vectors
// (internal/crypto/testdata/chunker_golden.json, asserted byte-identical by
// TestChunker_GoldenBoundaries in Go and edge/s3-ingest/test/chunker.test.js
// in node). Built by scripts/build-client.sh (client `edge-chunker`) into
// edge/s3-ingest/wasm/chunker.wasm; its sha256 is a client_build row of the
// transparency log (contract line 7).
//
// It registers globalThis.storedChunker. A chunker instance is a handle:
// create() → id; push(id, Uint8Array) → the LENGTHS of the chunks completed
// so far (number[]); finish(id) → the remaining lengths (the final chunk);
// free(id). Boundaries reset per instance — Part B chunks each multipart
// part on a fresh instance, so the manifest's chunk list is the parts'
// concatenation and cross-part dedup is by chunk content, never position.
// goldenBody(seed, n) is the golden tests' deterministic body
// (crypto.GoldenBody), generated INSIDE the module so the node test feeds
// the exact bytes Go feeds.
//
// Every function answers {value} or {error}, as sdk/wasm does.
package main

import (
	"errors"
	"fmt"
	"io"
	"sync"
	"syscall/js"

	"github.com/FairForge/vaultaire/internal/crypto"
)

// instance is one streaming chunker: a pipe fed by push, the chunker's
// goroutine cutting it, completed chunk lengths on a buffered channel.
type instance struct {
	w      *io.PipeWriter
	chunks chan int
	errCh  chan error
	done   bool
}

var (
	mu        sync.Mutex
	instances = map[int]*instance{}
	nextID    = 1
)

func newInstance() (int, error) {
	ch, err := crypto.DefaultChunker()
	if err != nil {
		return 0, err
	}
	pr, pw := io.Pipe()
	in := &instance{w: pw, chunks: make(chan int, 4096), errCh: make(chan error, 1)}
	results, err := ch.Chunk(pr)
	if err != nil {
		return 0, err
	}
	go func() {
		for r := range results {
			if r.Err != nil {
				in.errCh <- r.Err
				close(in.chunks)
				return
			}
			in.chunks <- r.Chunk.Size
		}
		close(in.chunks)
	}()
	mu.Lock()
	id := nextID
	nextID++
	instances[id] = in
	mu.Unlock()
	return id, nil
}

func instanceOf(args []js.Value) (*instance, int, error) {
	if len(args) < 1 || args[0].Type() != js.TypeNumber {
		return nil, 0, errors.New("the first argument is the instance id")
	}
	id := args[0].Int()
	mu.Lock()
	in := instances[id]
	mu.Unlock()
	if in == nil {
		return nil, 0, fmt.Errorf("no chunker instance %d", id)
	}
	return in, id, nil
}

// drain collects the lengths completed so far without blocking.
func (in *instance) drain() ([]any, error) {
	var out []any
	for {
		select {
		case n, ok := <-in.chunks:
			if !ok {
				in.done = true
				select {
				case err := <-in.errCh:
					return nil, err
				default:
					return out, nil
				}
			}
			out = append(out, n)
		default:
			return out, nil
		}
	}
}

func bytesOf(v js.Value, what string) []byte {
	if !v.InstanceOf(js.Global().Get("Uint8Array")) {
		panic(what + " is not a Uint8Array")
	}
	b := make([]byte, v.Get("length").Int())
	js.CopyBytesToGo(b, v)
	return b
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

func main() {
	api := map[string]any{
		"create": fn(func([]js.Value) (any, error) { return newInstance() }),
		"push": fn(func(args []js.Value) (any, error) {
			in, _, err := instanceOf(args)
			if err != nil {
				return nil, err
			}
			if len(args) < 2 {
				return nil, errors.New("push takes (id, Uint8Array)")
			}
			b := bytesOf(args[1], "push's bytes")
			// The write blocks only while the chunker consumes: the chunker
			// goroutine runs on the same scheduler, so this returns once the
			// bytes are cut or buffered.
			if _, err := in.w.Write(b); err != nil {
				return nil, err
			}
			return in.drain()
		}),
		"finish": fn(func(args []js.Value) (any, error) {
			in, _, err := instanceOf(args)
			if err != nil {
				return nil, err
			}
			if err := in.w.Close(); err != nil {
				return nil, err
			}
			var out []any
			for n := range in.chunks {
				out = append(out, n)
			}
			in.done = true
			select {
			case err := <-in.errCh:
				return nil, err
			default:
			}
			return out, nil
		}),
		"free": fn(func(args []js.Value) (any, error) {
			in, id, err := instanceOf(args)
			if err != nil {
				return nil, err
			}
			_ = in.w.CloseWithError(errors.New("freed"))
			mu.Lock()
			delete(instances, id)
			mu.Unlock()
			return true, nil
		}),
		"goldenBody": fn(func(args []js.Value) (any, error) {
			if len(args) < 2 || args[0].Type() != js.TypeNumber || args[1].Type() != js.TypeNumber {
				return nil, errors.New("goldenBody takes (seed, n)")
			}
			b := crypto.GoldenBody(uint64(args[0].Int()), args[1].Int()) // #nosec G115 -- a test seed
			a := js.Global().Get("Uint8Array").New(len(b))
			js.CopyBytesToJS(a, b)
			return a, nil
		}),
	}
	js.Global().Set("storedChunker", js.ValueOf(api))
	select {}
}
