// internal/perf/payload_test.go
package perf

import (
	"bytes"
	"testing"
)

func TestDefaultPayloadConfig(t *testing.T) {
	config := DefaultPayloadConfig()

	if config.MaxPayloadSize != 100*1024*1024 {
		t.Errorf("expected 100MB, got %d", config.MaxPayloadSize)
	}
	if config.ChunkSize != 64*1024 {
		t.Errorf("expected 64KB, got %d", config.ChunkSize)
	}
}

func TestNewPayloadOptimizer(t *testing.T) {
	opt := NewPayloadOptimizer(nil)

	if opt == nil {
		t.Fatal("expected non-nil")
	}
	if opt.config == nil {
		t.Error("expected default config")
	}
}

func TestPayloadOptimizerOptimize(t *testing.T) {
	opt := NewPayloadOptimizer(nil)

	data := []byte("hello world")
	payload, err := opt.Optimize(data)
	if err != nil {
		t.Fatalf("optimize failed: %v", err)
	}

	if payload.OriginalSize != 11 {
		t.Errorf("expected 11, got %d", payload.OriginalSize)
	}
	if !bytes.Equal(payload.Original, data) {
		t.Error("original mismatch")
	}
}

func TestPayloadOptimizerChunking(t *testing.T) {
	config := &PayloadConfig{
		ChunkSize: 10,
	}
	opt := NewPayloadOptimizer(config)

	data := bytes.Repeat([]byte("a"), 25)
	payload, _ := opt.Optimize(data)

	if !payload.IsChunked {
		t.Error("expected chunked")
	}
	if len(payload.Chunks) != 3 {
		t.Errorf("expected 3 chunks, got %d", len(payload.Chunks))
	}
}

func TestPayloadOptimizerReassemble(t *testing.T) {
	opt := NewPayloadOptimizer(nil)

	chunks := [][]byte{
		[]byte("hello "),
		[]byte("world"),
	}

	result := opt.Reassemble(chunks)
	if string(result) != "hello world" {
		t.Errorf("expected 'hello world', got '%s'", result)
	}
}

func TestPayloadOptimizerStats(t *testing.T) {
	opt := NewPayloadOptimizer(nil)

	_, _ = opt.Optimize([]byte("test"))
	_, _ = opt.Optimize([]byte("data"))

	stats := opt.Stats()
	if stats.TotalPayloads != 2 {
		t.Errorf("expected 2, got %d", stats.TotalPayloads)
	}
	if stats.TotalBytes != 8 {
		t.Errorf("expected 8, got %d", stats.TotalBytes)
	}
}

func TestJSONOptimizerCompact(t *testing.T) {
	opt := NewJSONOptimizer()

	input := []byte(`{
		"name": "test",
		"value": 123
	}`)

	compact, err := opt.Compact(input)
	if err != nil {
		t.Fatalf("compact failed: %v", err)
	}

	expected := `{"name":"test","value":123}`
	if string(compact) != expected {
		t.Errorf("expected %s, got %s", expected, compact)
	}
}

func TestJSONOptimizerIsValid(t *testing.T) {
	opt := NewJSONOptimizer()

	if !opt.IsValid([]byte(`{"valid": true}`)) {
		t.Error("expected valid")
	}
	if opt.IsValid([]byte(`{invalid}`)) {
		t.Error("expected invalid")
	}
}

func TestStreamChunker(t *testing.T) {
	chunker := NewStreamChunker(10)

	data := bytes.Repeat([]byte("x"), 25)
	chunks := chunker.Chunk(data)

	if len(chunks) != 3 {
		t.Errorf("expected 3 chunks, got %d", len(chunks))
	}
	if len(chunks[0]) != 10 {
		t.Errorf("expected first chunk 10, got %d", len(chunks[0]))
	}
	if len(chunks[2]) != 5 {
		t.Errorf("expected last chunk 5, got %d", len(chunks[2]))
	}
}

func TestStreamChunkerEmpty(t *testing.T) {
	chunker := NewStreamChunker(10)

	chunks := chunker.Chunk([]byte{})
	if chunks != nil {
		t.Error("expected nil for empty")
	}
}

func TestStreamChunkerChunkSize(t *testing.T) {
	chunker := NewStreamChunker(32 * 1024)

	if chunker.ChunkSize() != 32*1024 {
		t.Errorf("expected 32KB, got %d", chunker.ChunkSize())
	}
}

func TestStreamChunkerDefault(t *testing.T) {
	chunker := NewStreamChunker(0)

	if chunker.ChunkSize() != 64*1024 {
		t.Errorf("expected default 64KB, got %d", chunker.ChunkSize())
	}
}

func TestPayloadValidator(t *testing.T) {
	v := NewPayloadValidator(100)

	if err := v.Validate([]byte("hello")); err != nil {
		t.Errorf("expected valid: %v", err)
	}

	if v.MaxSize() != 100 {
		t.Errorf("expected 100, got %d", v.MaxSize())
	}
}

func TestPayloadValidatorTooLarge(t *testing.T) {
	v := NewPayloadValidator(10)

	err := v.Validate(bytes.Repeat([]byte("x"), 100))
	if err != ErrPayloadTooLarge {
		t.Errorf("expected too large, got %v", err)
	}
}

func TestPayloadValidatorEmpty(t *testing.T) {
	v := NewPayloadValidator(100)

	err := v.Validate([]byte{})
	if err != ErrPayloadEmpty {
		t.Errorf("expected empty, got %v", err)
	}
}

func TestPayloadError(t *testing.T) {
	err := &PayloadError{msg: "test error"}
	if err.Error() != "test error" {
		t.Error("error message mismatch")
	}
}

func TestOptimizedPayloadFields(t *testing.T) {
	p := &OptimizedPayload{
		Original:       []byte("test"),
		OriginalSize:   4,
		CompressedSize: 2,
		IsChunked:      true,
		Chunks:         [][]byte{[]byte("te"), []byte("st")},
	}

	if p.OriginalSize != 4 {
		t.Error("unexpected original size")
	}
	if len(p.Chunks) != 2 {
		t.Error("unexpected chunk count")
	}
}
