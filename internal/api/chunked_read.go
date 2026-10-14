package api

// The read side of a chunked object, shared by the S3 GET (handleChunkedGet)
// and every reader that needs the object's bytes as a plain stream: the
// public CDN and a CopyObject whose chunked source must be re-stored whole
// (WP-R3-1: with multipart through the chunk pipeline most large objects are
// chunked, and the CDN asked the engine for a whole blob at <key> that a
// chunked object does not have — 404 for a row that exists).
//
// chunkPlan is the byte plan (which chunks, and the slice of each), and
// startChunkStream the bounded, hedged read-ahead over it (internal/parfetch;
// each chunk fetched, decrypted, decompressed and SHA-256-verified before its
// bytes are handed out). openChunkedObject resolves the manifest and asks
// whether every backend it names can be asked BEFORE the caller writes a
// status line, as the S3 GET does.

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/FairForge/vaultaire/internal/parfetch"
)

// chunkSlice is one chunk of a plan and the bytes of it the plan takes.
type chunkSlice struct {
	desc chunkDesc
	skip int64
	take int64
}

// chunkPlan is the plan of bytes [start, end] of a chunked object (end < 0:
// to the last chunk, each taken whole).
func chunkPlan(descs []chunkDesc, start, end int64) []chunkSlice {
	var plan []chunkSlice
	for _, d := range descs {
		if end < 0 {
			plan = append(plan, chunkSlice{desc: d, take: d.size})
			continue
		}
		chunkStart := d.offset
		chunkEnd := d.offset + d.size - 1
		if chunkEnd < start {
			continue
		}
		if chunkStart > end {
			break
		}
		skip := int64(0)
		if start > chunkStart {
			skip = start - chunkStart
		}
		takeEnd := chunkEnd
		if end < takeEnd {
			takeEnd = end
		}
		plan = append(plan, chunkSlice{desc: d, skip: skip, take: takeEnd - (chunkStart + skip) + 1})
	}
	return plan
}

// startChunkStream streams the plan in order through a bounded, hedged
// read-ahead: up to CHUNK_GET_WINDOW_BYTES of chunks — at most
// CHUNK_GET_PREFETCH of them — are fetched and verified while earlier ones
// are consumed; a chunk still not done after the hedge delay is requested
// again and the first verified copy wins (the content address makes either
// copy valid). The consumer takes strictly in plan order.
func (a *S3ToEngine) startChunkStream(ctx context.Context, tenantID string, plan []chunkSlice) *parfetch.Stream {
	sizes := make([]int64, len(plan))
	for i, p := range plan {
		sizes[i] = p.desc.size
	}
	return parfetch.Start(ctx, a.chunkStreamConfig(), sizes, func(fctx context.Context, i int) ([]byte, error) {
		data, ferr := a.fetchAndVerifyChunk(fctx, plan[i].desc, tenantID)
		if ferr != nil {
			return nil, ferr
		}
		p := plan[i]
		if p.skip != 0 || p.take != int64(len(data)) {
			end := p.skip + p.take
			if end > int64(len(data)) {
				end = int64(len(data))
			}
			data = data[p.skip:end]
		}
		return data, nil
	})
}

// chunkedObjectReader is a chunked object as an io.ReadSeeker: Seek (from the
// start, before the first Read) plans from that offset, so a ranged reader
// fetches only the chunks it spans. A chunk that fails after the first byte
// is a Read error, never a short clean end.
type chunkedObjectReader struct {
	a        *S3ToEngine
	ctx      context.Context
	tenantID string
	descs    []chunkDesc
	size     int64
	start    int64
	r        *parfetch.Reader
	stream   *parfetch.Stream
}

// openChunkedObject resolves the manifest a row of tenantID's bucket/key
// names — at storageKey, the row's address (WP-VM-1; "" = the key, a
// manifest written before 092) — and checks that every backend its chunks
// are on can be asked now (registered, breaker not open):
// engine.ErrAllBackendsUnavailable otherwise — before any byte, so the
// caller answers 503. size is the row's (the plan's total).
func (a *S3ToEngine) openChunkedObject(ctx context.Context, tenantID, bucket, key, storageKey string, size int64) (*chunkedObjectReader, error) {
	if a.gci == nil {
		return nil, errors.New("chunked object read without the content index")
	}
	manifest := manifestAddress(key, storageKey)
	refs, err := a.gci.GetObjectChunks(ctx, tenantID, bucket, manifest)
	if err != nil {
		return nil, fmt.Errorf("get object chunks: %w", err)
	}
	if len(refs) == 0 {
		return nil, fmt.Errorf("no chunk references for %s/%s (manifest %s)", bucket, key, manifest)
	}
	descs, err := a.resolveChunkDescs(ctx, refs)
	if err != nil {
		return nil, err
	}
	if ca, ok := a.engine.(interface{ CanAsk(string) error }); ok {
		seen := map[string]bool{}
		for _, d := range descs {
			if d.backendID == "" || seen[d.backendID] {
				continue
			}
			seen[d.backendID] = true
			if err := ca.CanAsk(d.backendID); err != nil {
				return nil, fmt.Errorf("chunk %s: %w", shortHash(d.plaintextHash), err)
			}
		}
	}
	return &chunkedObjectReader{a: a, ctx: ctx, tenantID: tenantID, descs: descs, size: size}, nil
}

func (c *chunkedObjectReader) Seek(offset int64, whence int) (int64, error) {
	if c.r != nil || whence != io.SeekStart || offset < 0 || offset > c.size {
		return 0, errors.New("chunked object: only an absolute seek before the first read")
	}
	c.start = offset
	return offset, nil
}

func (c *chunkedObjectReader) Read(p []byte) (int, error) {
	if c.r == nil {
		if c.start >= c.size {
			return 0, io.EOF
		}
		c.stream = c.a.startChunkStream(c.ctx, c.tenantID, chunkPlan(c.descs, c.start, c.size-1))
		c.r = parfetch.NewReader(c.stream, nil)
	}
	return c.r.Read(p)
}

// Close stops the read-ahead.
func (c *chunkedObjectReader) Close() error {
	if c.r != nil {
		return c.r.Close()
	}
	return nil
}
