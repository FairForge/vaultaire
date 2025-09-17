package drivers

import (
	"context"
	"fmt"
	"os"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
	"github.com/tetratelabs/wazero/imports/wasi_snapshot_preview1"
)

type WASMPlugin struct {
	runtime wazero.Runtime
	module  api.Module
}

func LoadWASMPlugin(path string) (*WASMPlugin, error) {
	wasmBytes, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read wasm file: %w", err)
	}

	ctx := context.Background()
	r := wazero.NewRuntime(ctx)

	// Add WASI support
	wasi_snapshot_preview1.MustInstantiate(ctx, r)

	// Compile and instantiate
	module, err := r.Instantiate(ctx, wasmBytes)
	if err != nil {
		return nil, fmt.Errorf("instantiate module: %w", err)
	}

	return &WASMPlugin{
		runtime: r,
		module:  module,
	}, nil
}

func (p *WASMPlugin) Transform(input []byte) ([]byte, error) {
	// Simple uppercase transform as proof of concept
	output := make([]byte, len(input))
	for i, b := range input {
		if b >= 'a' && b <= 'z' {
			output[i] = b - 32
		} else {
			output[i] = b
		}
	}
	return output, nil
}

func (p *WASMPlugin) Close() error {
	return p.runtime.Close(context.Background())
}
