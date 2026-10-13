package api

// WP-R8-5 (keystone K1): every SSE-S3 write seals as it streams and every
// read opens as it streams — blob format 0x03, internal/crypto/stream.go. One
// segment (1 MiB) of plaintext and one of ciphertext are in memory per
// stream, never the object; so no size is refused for encryption.
//
// Writers (PUT, CompleteMultipartUpload, CopyObject, generated objects, the
// key_reencrypt job) wrap their plaintext stream in an sseWriteSealer: the
// MD5 tee stays before it (the ETag is the plaintext's), the backend PUT
// carries SealedLength(size), the row the plaintext size and the key version.
//
// Readers (the S3 GET, /cdn, CopyObject's source, key_reencrypt) go through
// openSealed: a whole read opens the stream (header and first segment
// verified before it returns, so a caller's status line follows a verified
// segment); a Range reads the header with one ranged GET and then only the
// segments the range spans (one more ranged GET), discarding the bytes of the
// first segment before the range — where the backend has no ranged read the
// engine reads the whole stream and discards (engine.GetRange's fallback). A
// 0x01/0x02 blob (written before WP-R8-5, ≤ 256 MiB) is opened whole by the
// compatibility opener and the range sliced from it.

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/FairForge/vaultaire/internal/crypto"
	"github.com/FairForge/vaultaire/internal/engine"
	"github.com/FairForge/vaultaire/internal/usage"
	"github.com/prometheus/client_golang/prometheus"
	"go.uber.org/zap"
)

// sseBlobsOpened counts the SSE-S3 blobs opened, by format: an operator sees
// whether any 0x01/0x02 blob is still read (a rotation re-seals them).
// Registered in initMetrics, every format at 0.
var sseBlobsOpened = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "vaultaire_sse_blobs_opened_total",
	Help: "SSE-S3 blobs opened, by blob format (0x01/0x02: whole-buffer, before WP-R8-5; 0x03: streaming).",
}, []string{"format"})

func init() {
	for _, f := range []string{"0x01", "0x02", "0x03"} {
		sseBlobsOpened.WithLabelValues(f)
	}
}

// countSSEOpens wires the service's open hook to the metric.
func countSSEOpens(svc *crypto.SSEService) {
	if svc != nil {
		svc.OnOpen(func(format string) { sseBlobsOpened.WithLabelValues(format).Inc() })
	}
}

// storedBlobLength is the length of a whole object's stored blob: its size,
// or the sealed length of an encrypted one (a 0x03 SSE-S3 or SSE-C blob; an
// old-format SSE-C blob's nonce and tag). A 0x01/0x02 SSE-S3 blob written
// before WP-R8-5 is size + SSEOverheadBytes and is not this length: the
// readers that size by it then fall back as for any other size mismatch,
// and the vault parity job fails such a blob as before (prod holds none).
func storedBlobLength(encAlgo string, size int64, sse *crypto.SSEService) int64 {
	switch {
	case encAlgo == "":
		return size
	case encAlgo == crypto.SSECAlgorithm:
		return size + crypto.SSECOverhead
	case encAlgo == crypto.SSECAlgorithmV3:
		return crypto.SSECSealedLength(size)
	case sse != nil:
		return sse.SealedLength(size)
	}
	return crypto.StreamCiphertextLength(crypto.StreamHeaderSSES3, crypto.StreamSegmentSize, size)
}

// errSealedUnreadable: an SSE-S3 object's stored bytes do not open — a
// tampered or truncated blob, a key that does not unwrap, a row whose size
// is not its blob's. Never the client's fault: a 500.
var errSealedUnreadable = errors.New("SSE-S3 object does not open")

// sseWriteSealer seals a write's plaintext stream (exactly size bytes) and
// records what the blob is sealed with.
type sseWriteSealer struct {
	r       io.Reader
	version int
}

// newSSEWriteSealer starts a seal of size bytes of src under the tenant's
// current key version. It reads nothing from src before the first Read; it
// fails only when the tenant key cannot be had.
func newSSEWriteSealer(ctx context.Context, svc *crypto.SSEService, tenantID string, src io.Reader, size int64) (*sseWriteSealer, error) {
	r, v, err := svc.Seal(ctx, tenantID, src, size)
	if err != nil {
		return nil, fmt.Errorf("seal: %w", err)
	}
	return &sseWriteSealer{r: r, version: v}, nil
}

func (s *sseWriteSealer) Read(p []byte) (int, error) { return s.r.Read(p) }

// encryption is what the stored blob is sealed with (nil: plaintext).
func (s *sseWriteSealer) encryption() objectEncryption {
	if s == nil {
		return objectEncryption{}
	}
	v := s.version
	return objectEncryption{Algorithm: crypto.SSEAlgorithm, KeyVersion: &v}
}

// blobOpener opens a sealed object's stored bytes: SSE-S3 under the tenant's
// key, SSE-C under the customer's.
type blobOpener interface {
	// whole opens a whole blob (the first segment verified before it returns).
	whole(ctx context.Context, src io.Reader) (io.Reader, error)
	// rangeOf opens the planned segments of a 0x03 blob.
	rangeOf(ctx context.Context, h *crypto.StreamHeader, size int64, rg crypto.StreamRange, src io.Reader) (io.Reader, error)
	// streaming: the blob is (or may be, by its first byte) a 0x03 blob a
	// range can be read from by segment. An old-format SSE-C blob is not:
	// its first byte is a random nonce byte, never a format marker.
	streaming() bool
}

// sseS3Opener opens SSE-S3 blobs of one tenant: 0x03, or 0x01/0x02 by the
// compatibility opener (their first bytes are those markers).
type sseS3Opener struct {
	svc      *crypto.SSEService
	tenantID string
}

func (o sseS3Opener) whole(ctx context.Context, src io.Reader) (io.Reader, error) {
	return o.svc.Open(ctx, o.tenantID, src)
}

func (o sseS3Opener) rangeOf(ctx context.Context, h *crypto.StreamHeader, size int64, rg crypto.StreamRange, src io.Reader) (io.Reader, error) {
	return o.svc.OpenRange(ctx, o.tenantID, h, size, rg, src)
}

func (sseS3Opener) streaming() bool { return true }

// sseCOpener opens SSE-C blobs under the customer's key: the format is the
// row's (crypto.SSECAlgorithmV3 = streaming, crypto.SSECAlgorithm = old).
type sseCOpener struct {
	key    []byte
	legacy bool
}

func (o sseCOpener) whole(_ context.Context, src io.Reader) (io.Reader, error) {
	if o.legacy {
		return crypto.SSECOpenLegacy(o.key, src)
	}
	return crypto.SSECOpen(o.key, src)
}

func (o sseCOpener) rangeOf(_ context.Context, h *crypto.StreamHeader, size int64, rg crypto.StreamRange, src io.Reader) (io.Reader, error) {
	return crypto.SSECOpenRange(o.key, h, size, rg, src)
}

func (o sseCOpener) streaming() bool { return !o.legacy }

// sealedSource is where a sealed object's stored bytes are read from.
type sealedSource struct {
	// whole opens the stored blob from its first byte.
	whole func() (io.ReadCloser, error)
	// ranged opens stored bytes [off, off+n); nil when there is no ranged
	// read.
	ranged func(off, n int64) (io.ReadCloser, error)
	// rangeFallback says whether a ranged read that failed with err may be
	// retried as a whole read (nil: always).
	rangeFallback func(err error) bool
}

// sealedRead is a request for plaintext of a sealed object: [Off, Off+N),
// or the whole object when Ranged is false.
type sealedRead struct {
	// Size is the object's plaintext size (its row's size_bytes).
	Size   int64
	Ranged bool
	Off, N int64
}

// openSealed opens the plaintext an SSE-S3 read asks for. The header and the
// first segment are verified before it returns: an error here is answered
// before any status line (errSealedUnreadable → 500; a source's own error —
// unavailable, missing, archived — as the plain path answers it). The
// reader it returns fails, never ends short, when a later segment does not
// open or the plaintext is not the row's size.
func openSealed(ctx context.Context, op blobOpener, src sealedSource, req sealedRead) (io.ReadCloser, error) {
	if req.Ranged && src.ranged != nil && op.streaming() {
		rc, err := openSealedRange(ctx, op, src, req)
		switch {
		case err == nil:
			return rc, nil
		case errors.Is(err, errSealedLegacy):
			// A 0x01/0x02 blob: opened whole below.
		case errors.Is(err, errSealedUnreadable):
			return nil, err
		case src.rangeFallback != nil && !src.rangeFallback(err):
			return nil, err
		}
	}
	stored, err := src.whole()
	if err != nil {
		return nil, err
	}
	pt, err := op.whole(ctx, stored)
	if err != nil {
		_ = stored.Close()
		return nil, fmt.Errorf("%w: %w", errSealedUnreadable, err)
	}
	if !req.Ranged {
		return &readCloser{Reader: &plaintextSized{r: pt, want: req.Size}, Closer: stored}, nil
	}
	if req.Off > 0 {
		if _, err := io.CopyN(io.Discard, pt, req.Off); err != nil {
			_ = stored.Close()
			return nil, fmt.Errorf("%w: skip to %d: %w", errSealedUnreadable, req.Off, err)
		}
	}
	return &readCloser{Reader: &plaintextSized{r: io.LimitReader(pt, req.N), want: req.N}, Closer: stored}, nil
}

// errSealedLegacy: the blob is not 0x03 (no ranged open).
var errSealedLegacy = errors.New("SSE-S3 blob is not a streaming one")

// openSealedRange is the ranged open of a 0x03 blob: its header, then the
// segments the range spans. A 0x01/0x02 blob is errSealedLegacy; a source
// error is returned as is (the caller may retry whole).
func openSealedRange(ctx context.Context, op blobOpener, src sealedSource, req sealedRead) (io.ReadCloser, error) {
	hr, err := src.ranged(0, int64(crypto.MaxStreamHeader))
	if err != nil {
		return nil, err
	}
	var hb [crypto.MaxStreamHeader]byte
	n, err := io.ReadFull(hr, hb[:])
	_ = hr.Close()
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) {
		return nil, err
	}
	if n == 0 || hb[0] != crypto.StreamFormat {
		return nil, errSealedLegacy
	}
	h, err := crypto.ParseStreamHeader(hb[:n])
	if err != nil {
		return nil, fmt.Errorf("%w: %w", errSealedUnreadable, err)
	}
	plan, err := crypto.PlanStreamRange(h, req.Size, req.Off, req.N)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", errSealedUnreadable, err)
	}
	sr, err := src.ranged(plan.CipherOffset, plan.CipherLength)
	if err != nil {
		return nil, err
	}
	pt, err := op.rangeOf(ctx, h, req.Size, plan, io.LimitReader(sr, plan.CipherLength))
	if err != nil {
		_ = sr.Close()
		return nil, fmt.Errorf("%w: %w", errSealedUnreadable, err)
	}
	return &readCloser{Reader: &plaintextSized{r: pt, want: req.N}, Closer: sr}, nil
}

// plaintextSized fails a plaintext stream that ends before want bytes (an
// authentic blob of another size than its row: never a short 200).
type plaintextSized struct {
	r    io.Reader
	want int64
	n    int64
}

func (p *plaintextSized) Read(b []byte) (int, error) {
	k, err := p.r.Read(b)
	p.n += int64(k)
	if errors.Is(err, io.EOF) && p.n != p.want {
		return k, fmt.Errorf("%w: %d plaintext bytes, the row records %d", errSealedUnreadable, p.n, p.want)
	}
	if p.n > p.want {
		return k, fmt.Errorf("%w: more plaintext than the row's %d bytes", errSealedUnreadable, p.want)
	}
	return k, err
}

// readCloser pairs a reader with what closes its source.
type readCloser struct {
	io.Reader
	io.Closer
}

// openSealedGet opens a sealed (SSE-S3 or SSE-C) object for the S3 GET: whole, or the range
// by segment (engine.GetRange: a native ranged read on the fixed-bucket,
// Lyve, R2, Geyser and WebDAV drivers; a whole read with discard elsewhere).
// A vault object whose backend failed — never ErrArchived, which keeps the
// restore semantics — is opened from its parity copy, which holds the stored
// (sealed) bytes: fromParity reports it.
func (a *S3ToEngine) openSealedGet(ctx context.Context, op blobOpener, tenantID, bucket, key, container, blob, etag string,
	size, stored int64, floor, backendName string, rng *httpRange) (rc io.ReadCloser, fromParity bool, err error) {
	sctx := engine.WithExpectedSize(ctx, stored)
	req := sealedRead{Size: size}
	if rng != nil {
		req.Ranged, req.Off, req.N = true, rng.start, rng.length
	}
	src := sealedSource{whole: func() (io.ReadCloser, error) { return a.engine.Get(sctx, container, blob) }}
	if ce, ok := a.engine.(*engine.CoreEngine); ok {
		src.ranged = func(off, n int64) (io.ReadCloser, error) { return ce.GetRange(sctx, container, blob, off, n) }
		// A strict backend (sync) that could not be asked is not asked
		// again whole (Prompt 2b.5 F3.7, as on the plain path).
		src.rangeFallback = func(err error) bool {
			return !errors.Is(err, engine.ErrAllBackendsUnavailable) || !engine.IsStrictTarget(backendName)
		}
	}
	rc, err = openSealed(ctx, op, src, req)
	if err == nil || errors.Is(err, errSealedUnreadable) {
		return rc, false, err
	}
	if floor != usage.FloorVault || a.vaultParity == nil || errors.Is(err, engine.ErrArchived) {
		return nil, false, err
	}
	psrc := sealedSource{
		whole: func() (io.ReadCloser, error) {
			return a.vaultParity.Open(ctx, tenantID, bucket, key, blob, etag, stored, 0, stored)
		},
		ranged: func(off, n int64) (io.ReadCloser, error) {
			return a.vaultParity.Open(ctx, tenantID, bucket, key, blob, etag, stored, off, n)
		},
	}
	prc, perr := openSealed(ctx, op, psrc, req)
	if perr != nil {
		a.logger.Error("vault object: its backend failed and the parity copy cannot serve it",
			zap.String("bucket", bucket), zap.String("key", key), zap.NamedError("backend", err), zap.NamedError("parity", perr))
		vaultParityFallbacks.WithLabelValues("unavailable").Inc()
		return nil, false, err
	}
	a.logger.Warn("vault object served from its parity copy", zap.String("bucket", bucket), zap.String("key", key),
		zap.NamedError("backend", err))
	vaultParityFallbacks.WithLabelValues("served").Inc()
	return prc, true, nil
}
