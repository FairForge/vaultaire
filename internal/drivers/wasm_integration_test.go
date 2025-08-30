package drivers

import (
	"bytes"
	"testing"
)

func TestWASMPlugin_ActualTransform(t *testing.T) {
	plugin, err := LoadWASMPlugin("testdata/transform.wasm")
	if err != nil {
		t.Fatalf("Failed to load plugin: %v", err)
	}
	defer func() { _ = plugin.Close() }()

	testCases := []struct {
		input    []byte
		expected []byte
	}{
		{[]byte("hello"), []byte("HELLO")},
		{[]byte("world"), []byte("WORLD")},
		{[]byte("123abc"), []byte("123ABC")},
	}

	for _, tc := range testCases {
		output, err := plugin.Transform(tc.input)
		if err != nil {
			t.Errorf("Transform failed for %s: %v", tc.input, err)
			continue
		}

		if !bytes.Equal(output, tc.expected) {
			t.Errorf("Expected %s, got %s", tc.expected, output)
		}
	}
}
