package api

import (
	"crypto/sha1" // #nosec G505 -- x-amz-checksum-sha1 is an S3 wire checksum, not a security primitive
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"hash"
	"hash/crc32"
	"hash/crc64"
	"io"
	"net/http"
	"strings"

	"github.com/FairForge/vaultaire/internal/auth"
)

// The flexible checksums S3 clients send (x-amz-checksum-<alg>: base64 of the
// big-endian digest), checked over the DECODED bytes before a commit — as a
// request header (aws-cli over plain HTTP) or as the aws-chunked trailer
// x-amz-trailer names (aws-cli ≥ 2.23 over TLS, on every upload). The edge
// has its backend check the trailer (edgeVerifiedTrailers); the origin
// discarded it until Stage 1a Prompt 52E.

// errChecksumMismatch: the body does not hash to the x-amz-checksum-* its
// request declared (S3: 400 XAmzContentChecksumMismatch).
var errChecksumMismatch = errors.New("x-amz-checksum does not match the received body")

// crc64NVMETable is CRC-64/NVME (poly 0xAD93D23594C93659, reflected): check
// value 0xAE8B14860A799888 for "123456789".
var crc64NVMETable = crc64.MakeTable(0x9A6C9329AC4BC9B5)

// newChecksumHash is the digest of an x-amz-checksum-<alg> name (lower
// case), or nil for a name this server does not know.
func newChecksumHash(name string) hash.Hash {
	switch name {
	case "x-amz-checksum-crc32":
		return crc32.NewIEEE()
	case "x-amz-checksum-crc32c":
		return crc32.New(crc32.MakeTable(crc32.Castagnoli))
	case "x-amz-checksum-crc64nvme":
		return crc64.New(crc64NVMETable)
	case "x-amz-checksum-sha1":
		return sha1.New() // #nosec G401 -- S3 wire checksum
	case "x-amz-checksum-sha256":
		return sha256.New()
	}
	return nil
}

// payloadChecksum is one declared checksum and its running digest.
type payloadChecksum struct {
	name string
	h    hash.Hash
}

func newPayloadChecksum(name string) *payloadChecksum {
	name = strings.ToLower(strings.TrimSpace(name))
	if h := newChecksumHash(name); h != nil {
		return &payloadChecksum{name: name, h: h}
	}
	return nil
}

// check compares the digest so far with the declared base64 value.
func (c *payloadChecksum) check(declared string) error {
	got := base64.StdEncoding.EncodeToString(c.h.Sum(nil))
	if declared == "" {
		return fmt.Errorf("%w: %s was declared and never sent", errChecksumMismatch, c.name)
	}
	if got != strings.TrimSpace(declared) {
		return fmt.Errorf("%w: %s", errChecksumMismatch, c.name)
	}
	return nil
}

// checksumReader checks a request-header checksum: the terminal read turns
// into errChecksumMismatch when the bytes do not match (the read error fails
// the upload before its commit, as payloadVerifyReader does for a payload
// hash).
type checksumReader struct {
	r        io.Reader
	sum      *payloadChecksum
	declared string
}

func (c *checksumReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	if n > 0 {
		c.sum.h.Write(p[:n])
	}
	if errors.Is(err, io.EOF) {
		if cerr := c.sum.check(c.declared); cerr != nil {
			return n, cerr
		}
	}
	return n, err
}

// requestPayload is the decoded, verified body of a PutObject or UploadPart:
// the aws-chunked framing decoded (a signed stream's chunk signatures and
// signed trailer checked, a declared trailer checksum compared), then every
// x-amz-checksum-* request header this server knows checked over the result.
// An error is a request refused before any byte is read.
func requestPayload(r *http.Request, haveDB bool) (io.Reader, error) {
	var body io.Reader = r.Body
	if isAWSChunked(r) {
		dec := newAWSChunkedReader(r.Body)
		dec.sig = auth.StreamChunkVerifier(r.Body)
		if dec.sig == nil && haveDB && auth.IsSignedStreaming(r) && auth.HeaderAccessKey(r) != "" {
			// A header-signed stream reaches a handler only through
			// ValidateRequest, which attaches its verifier: without one
			// the chunks cannot be checked, so the body is refused.
			return nil, fmt.Errorf("%w: no chunk verifier for a signed stream", errAWSChunkedSignature)
		}
		for _, name := range strings.Split(r.Header.Get("X-Amz-Trailer"), ",") {
			if c := newPayloadChecksum(name); c != nil {
				dec.trailer = c
				break
			}
		}
		body = dec
	}
	for name, vals := range r.Header {
		lower := strings.ToLower(name)
		if !strings.HasPrefix(lower, "x-amz-checksum-") || len(vals) == 0 {
			continue
		}
		if c := newPayloadChecksum(lower); c != nil {
			body = &checksumReader{r: body, sum: c, declared: vals[0]}
		}
	}
	return body, nil
}
