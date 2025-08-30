package drivers

import (
	"bytes"
	"testing"
)

func TestWASMPlugin_Transform(t *testing.T) {
	// Test that we can load and execute a WASM plugin
	plugin, err := LoadWASMPlugin("testdata/transform.wasm")
	if err != nil {
		t.Skip("WASM plugin not found, skipping")
	}

	input := []byte("hello world")
	output, err := plugin.Transform(input)

	if err != nil {
		t.Fatalf("Transform failed: %v", err)
	}

	// Plugin should uppercase the input
	expected := []byte("HELLO WORLD")
	if !bytes.Equal(output, expected) {
		t.Errorf("Expected %s, got %s", expected, output)
	}
}

func TestWASMPlugin_Sandbox(t *testing.T) {
	// Test that plugins are sandboxed and can't access host filesystem
	plugin, err := LoadWASMPlugin("testdata/malicious.wasm")
	if err != nil {
		t.Skip("Test plugin not found")
	}

	// This should fail safely, not crash or access host
	_, err = plugin.Transform([]byte("test"))
	if err == nil {
		t.Error("Expected sandboxed plugin to fail when trying forbidden operations")
	}
}
