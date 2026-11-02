package api

import (
	"bytes"
	"context"
	"crypto/rand"
	"io"
	"testing"
)

// BenchmarkParityRebuild is the codec's own ceiling (WP-VAULT-3 PR 0): a
// 100 MiB object rebuilt from its four parity shards held in memory — no
// network, no leg. The prod rebuild of 2026-11-02 ran at 4.7 MB/s; whatever
// this prints is what is left once the shards arrive at once.
//
//	go test ./internal/api -run xxx -bench ParityRebuild -benchtime 3x
func BenchmarkParityRebuild(b *testing.B) {
	const size = 100 << 20
	l := parityLayout{k: vaultParityK, m: vaultParityM, stripe: vaultParityStripe, size: size}
	body := make([]byte, size)
	if _, err := rand.Read(body); err != nil {
		b.Fatal(err)
	}
	shards := make([]bytes.Buffer, l.m)
	out := make([]io.Writer, l.m)
	for j := range shards {
		out[j] = &shards[j]
	}
	if err := encodeParity(l, bytes.NewReader(body), out); err != nil {
		b.Fatal(err)
	}
	none := func(context.Context, int64, int) ([]byte, error) { return nil, errParityAloneOnly }
	b.SetBytes(size)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		streams := make([]io.ReadCloser, l.m)
		for j := range streams {
			streams[j] = io.NopCloser(bytes.NewReader(shards[j].Bytes()))
		}
		rb := newParityRebuilder(context.Background(), l, streams, none, 0, size)
		n, err := io.Copy(io.Discard, rb)
		if err != nil || n != size {
			b.Fatalf("rebuilt %d of %d bytes: %v", n, size, err)
		}
		_ = rb.Close()
	}
}
