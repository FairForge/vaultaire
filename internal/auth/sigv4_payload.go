package auth

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"net/http"
	"strings"
)

// ErrContentSHA256Mismatch is returned by the request body reader when the
// received payload does not hash to the value the client declared (and
// signed) in X-Amz-Content-Sha256. The API layer maps it to the S3 error
// code XAmzContentSHA256Mismatch (400).
var ErrContentSHA256Mismatch = errors.New("x-amz-content-sha256 does not match request body")

// ErrInvalidContentSHA256 is returned at auth time when X-Amz-Content-Sha256
// is neither a payload marker (UNSIGNED-PAYLOAD, STREAMING-*) nor a valid
// SHA-256 hex digest. Maps to InvalidArgument (400).
var ErrInvalidContentSHA256 = errors.New("invalid x-amz-content-sha256 header value")

// wrapPayloadVerification binds the request body to the payload hash the
// client signed. verifySigV4 proves the DECLARATION is authentic; this makes
// the body live up to it: r.Body is replaced with a reader that hashes the
// bytes as the handler consumes them and fails the final read when the
// digest differs from the declared value — so a tampered or corrupted
// payload errors out before any handler treats it as complete.
//
// Marker values pass through unverified: UNSIGNED-PAYLOAD carries no body
// digest by design (TLS is the integrity layer), and STREAMING-* payloads
// declare per-chunk signatures inside the aws-chunked framing (verifying
// those needs the signing key mid-stream — not yet implemented). Anything
// else that is not a SHA-256 hex digest is rejected outright.
func wrapPayloadVerification(r *http.Request) error {
	declared := r.Header.Get("X-Amz-Content-Sha256")
	if declared == "" || declared == unsignedPayload || strings.HasPrefix(declared, "STREAMING-") {
		return nil
	}
	// aws-chunked framing: the declared digest covers the DECODED payload,
	// but this wrapper sees the framed bytes (chunk-size lines, CRLFs,
	// trailers) — hashing them guarantees a false mismatch. Skip; the framed
	// path's integrity is the aws-chunked layer's concern.
	if strings.Contains(r.Header.Get("Content-Encoding"), "aws-chunked") {
		return nil
	}
	if !isSHA256Hex(declared) {
		return fmt.Errorf("%w: %q", ErrInvalidContentSHA256, declared)
	}
	r.Body = &payloadVerifyReader{
		body:     r.Body,
		declared: strings.ToLower(declared),
		digest:   sha256.New(),
	}
	return nil
}

func isSHA256Hex(s string) bool {
	if len(s) != 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') && (c < 'A' || c > 'F') {
			return false
		}
	}
	return true
}

// payloadVerifyReader hashes the body as it streams through and turns the
// terminal io.EOF into ErrContentSHA256Mismatch when the computed digest
// differs from the declared one. Bytes read before EOF are passed through
// untouched — streaming consumers may have already processed them, so the
// contract is that the CALLER's operation fails (the read error aborts the
// upload before commit), not that bad bytes are never observed.
type payloadVerifyReader struct {
	body     io.ReadCloser
	declared string
	digest   hash.Hash
	failed   bool
}

func (p *payloadVerifyReader) Read(buf []byte) (int, error) {
	if p.failed {
		return 0, fmt.Errorf("%w", ErrContentSHA256Mismatch)
	}
	n, err := p.body.Read(buf)
	if n > 0 {
		p.digest.Write(buf[:n])
	}
	if errors.Is(err, io.EOF) {
		if computed := hex.EncodeToString(p.digest.Sum(nil)); computed != p.declared {
			p.failed = true
			return n, fmt.Errorf("%w: declared %s, computed %s",
				ErrContentSHA256Mismatch, p.declared, computed)
		}
	}
	return n, err
}

func (p *payloadVerifyReader) Close() error {
	return p.body.Close()
}
