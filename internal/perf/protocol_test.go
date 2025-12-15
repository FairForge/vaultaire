// internal/perf/protocol_test.go
package perf

import (
	"bytes"
	"testing"
)

func TestDefaultProtocolConfig(t *testing.T) {
	config := DefaultProtocolConfig()

	if config.Compression != CompressionGzip {
		t.Errorf("expected gzip, got %s", config.Compression)
	}
	if !config.EnableChecksum {
		t.Error("expected checksum enabled")
	}
	if config.MaxMessageSize != 64*1024*1024 {
		t.Errorf("expected 64MB, got %d", config.MaxMessageSize)
	}
}

func TestNewProtocolOptimizer(t *testing.T) {
	opt := NewProtocolOptimizer(nil)

	if opt == nil {
		t.Fatal("expected non-nil optimizer")
	}
	if opt.config == nil {
		t.Error("expected default config")
	}
}

func TestProtocolEncodeDecode(t *testing.T) {
	opt := NewProtocolOptimizer(&ProtocolConfig{
		Compression:    CompressionNone,
		EnableChecksum: true,
	})

	msg := &Message{
		Type:    1,
		Payload: []byte("hello world"),
	}

	encoded, err := opt.Encode(msg)
	if err != nil {
		t.Fatalf("encode failed: %v", err)
	}

	decoded, err := opt.Decode(encoded)
	if err != nil {
		t.Fatalf("decode failed: %v", err)
	}

	if decoded.Type != 1 {
		t.Errorf("expected type 1, got %d", decoded.Type)
	}
	if !bytes.Equal(decoded.Payload, []byte("hello world")) {
		t.Error("payload mismatch")
	}
}

func TestProtocolCompression(t *testing.T) {
	opt := NewProtocolOptimizer(&ProtocolConfig{
		Compression:    CompressionGzip,
		EnableChecksum: false,
	})

	payload := bytes.Repeat([]byte("hello world "), 1000)
	msg := &Message{Type: 1, Payload: payload}

	encoded, err := opt.Encode(msg)
	if err != nil {
		t.Fatalf("encode failed: %v", err)
	}

	if len(encoded) >= len(payload) {
		t.Error("expected compression to reduce size")
	}

	decoded, err := opt.Decode(encoded)
	if err != nil {
		t.Fatalf("decode failed: %v", err)
	}

	if !bytes.Equal(decoded.Payload, payload) {
		t.Error("payload mismatch after decompression")
	}
}

func TestProtocolChecksum(t *testing.T) {
	opt := NewProtocolOptimizer(&ProtocolConfig{
		Compression:    CompressionNone,
		EnableChecksum: true,
	})

	msg := &Message{Type: 1, Payload: []byte("test data")}
	encoded, _ := opt.Encode(msg)

	// Corrupt data
	encoded[len(encoded)-1] ^= 0xFF

	_, err := opt.Decode(encoded)
	if err != ErrChecksumMismatch {
		t.Errorf("expected checksum error, got %v", err)
	}

	stats := opt.Stats()
	if stats.ChecksumErrors != 1 {
		t.Errorf("expected 1 error, got %d", stats.ChecksumErrors)
	}
}

func TestProtocolStats(t *testing.T) {
	opt := NewProtocolOptimizer(&ProtocolConfig{
		Compression:    CompressionNone,
		EnableChecksum: false,
	})

	msg := &Message{Type: 1, Payload: []byte("test")}
	encoded, _ := opt.Encode(msg)
	_, _ = opt.Decode(encoded)

	stats := opt.Stats()

	if stats.MessagesEncoded != 1 {
		t.Errorf("expected 1 encoded, got %d", stats.MessagesEncoded)
	}
	if stats.MessagesDecoded != 1 {
		t.Errorf("expected 1 decoded, got %d", stats.MessagesDecoded)
	}
}

func TestProtocolEmptyPayload(t *testing.T) {
	opt := NewProtocolOptimizer(&ProtocolConfig{
		Compression:    CompressionNone,
		EnableChecksum: true,
	})

	msg := &Message{Type: 1, Payload: []byte{}}
	encoded, err := opt.Encode(msg)
	if err != nil {
		t.Fatalf("encode failed: %v", err)
	}

	decoded, err := opt.Decode(encoded)
	if err != nil {
		t.Fatalf("decode failed: %v", err)
	}

	if len(decoded.Payload) != 0 {
		t.Error("expected empty payload")
	}
}

func TestMessageFlags(t *testing.T) {
	combined := uint16(FlagCompressed) | uint16(FlagChecksum)

	if combined&uint16(FlagCompressed) == 0 {
		t.Error("compressed flag not set")
	}
	if combined&uint16(FlagChecksum) == 0 {
		t.Error("checksum flag not set")
	}
}

func TestCompressionTypes(t *testing.T) {
	types := []CompressionType{
		CompressionNone,
		CompressionGzip,
		CompressionSnappy,
		CompressionLZ4,
		CompressionZstd,
	}

	for _, ct := range types {
		if ct == "" {
			t.Error("type should not be empty")
		}
	}
}

func TestNewPipeline(t *testing.T) {
	p := NewPipeline(10)

	if p.MaxDepth() != 10 {
		t.Errorf("expected 10, got %d", p.MaxDepth())
	}
	if p.Depth() != 0 {
		t.Error("expected 0")
	}
}

func TestPipelineSubmit(t *testing.T) {
	p := NewPipeline(3)
	defer p.Close()

	ok := p.Submit(&PipelineRequest{ID: 1})
	if !ok {
		t.Error("expected success")
	}

	if p.Depth() != 1 {
		t.Errorf("expected 1, got %d", p.Depth())
	}
}

func TestPipelineMaxDepth(t *testing.T) {
	p := NewPipeline(2)
	defer p.Close()

	p.Submit(&PipelineRequest{ID: 1})
	p.Submit(&PipelineRequest{ID: 2})

	ok := p.Submit(&PipelineRequest{ID: 3})
	if ok {
		t.Error("expected failure at max depth")
	}
}

func TestPipelineComplete(t *testing.T) {
	p := NewPipeline(3)
	defer p.Close()

	p.Submit(&PipelineRequest{ID: 1})
	p.Submit(&PipelineRequest{ID: 2})
	p.Complete()

	if p.Depth() != 1 {
		t.Errorf("expected 1, got %d", p.Depth())
	}
}

func TestPipelinePending(t *testing.T) {
	p := NewPipeline(10)

	p.Submit(&PipelineRequest{ID: 42})
	p.Close()

	req := <-p.Pending()
	if req.ID != 42 {
		t.Errorf("expected 42, got %d", req.ID)
	}
}
