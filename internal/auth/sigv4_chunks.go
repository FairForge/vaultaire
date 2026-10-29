package auth

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
)

// WP-R15-1 (R15-06): the body of a SigV4 streaming upload is signed chunk by
// chunk. The seed signature (the Authorization header's) binds the headers
// and the declared decoded length; each chunk then carries
//
//	HMAC(signingKey, "AWS4-HMAC-SHA256-PAYLOAD\n" + date + "\n" + scope + "\n" +
//	     previousSignature + "\n" + sha256("") + "\n" + sha256(chunk))
//
// chained from the seed, the terminal 0-byte chunk included, and a
// -TRAILER stream ends with x-amz-trailer-signature over its trailing
// headers:
//
//	HMAC(signingKey, "AWS4-HMAC-SHA256-TRAILER\n" + date + "\n" + scope + "\n" +
//	     previousSignature + "\n" + sha256(canonical trailing headers))
//
// Both used to be stripped and never checked. The verifier hands the request
// a ChunkVerifier (StreamChunkVerifier) whose signing key lives only in a
// closure here: the API layer's aws-chunked decoder feeds it each chunk's
// hash and signature and fails the read on a mismatch. The secret never
// leaves this package and the key is never logged.

const (
	streamingSignedPayload        = "STREAMING-AWS4-HMAC-SHA256-PAYLOAD"
	streamingSignedPayloadTrailer = "STREAMING-AWS4-HMAC-SHA256-PAYLOAD-TRAILER"
	chunkSigningAlgorithm         = "AWS4-HMAC-SHA256-PAYLOAD"
	trailerSigningAlgorithm       = "AWS4-HMAC-SHA256-TRAILER"
)

// ErrChunkSignature: a chunk (or the trailer) of a signed streaming body
// whose signature is missing or does not verify. The API layer answers 403
// SignatureDoesNotMatch and commits nothing.
var ErrChunkSignature = errors.New("aws-chunked chunk signature does not match")

// emptySHA256Hex is sha256("") — the fixed line of every chunk's string to sign.
const emptySHA256Hex = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

// IsSignedStreaming reports whether the declared payload hash is one of the
// two streaming markers whose chunks are signed.
func IsSignedStreaming(r *http.Request) bool {
	v := r.Header.Get("X-Amz-Content-Sha256")
	return v == streamingSignedPayload || v == streamingSignedPayloadTrailer
}

// ChunkVerifier checks one signed streaming body, in order. Not safe for
// concurrent use (one decoder reads one body).
type ChunkVerifier struct {
	// sign is HMAC-SHA256 under the request's signing key, hex — the key is
	// captured here and nowhere else.
	sign    func(stringToSign string) string
	header  string // date + "\n" + scope + "\n"
	prev    string // the previous signature: the seed, then each chunk's
	trailer bool   // a -TRAILER stream: x-amz-trailer-signature is required
	failed  bool
}

// SignedTrailer reports whether the stream must end with a signed trailer.
func (v *ChunkVerifier) SignedTrailer() bool { return v.trailer }

// Chunk verifies the next chunk: signature is its chunk-signature extension
// ("" when the chunk carried none), payloadSHA256 the sha256 of its payload
// (of nothing for the terminal chunk).
func (v *ChunkVerifier) Chunk(signature string, payloadSHA256 []byte) error {
	if v.failed {
		return ErrChunkSignature
	}
	if signature == "" {
		v.failed = true
		return fmt.Errorf("%w: a chunk carries no chunk-signature", ErrChunkSignature)
	}
	want := v.sign(chunkSigningAlgorithm + "\n" + v.header + v.prev + "\n" + emptySHA256Hex + "\n" + hex.EncodeToString(payloadSHA256))
	if !hmac.Equal([]byte(want), []byte(signature)) {
		v.failed = true
		return ErrChunkSignature
	}
	v.prev = signature
	return nil
}

// Trailer verifies x-amz-trailer-signature (signature, "" when absent) over
// the canonical trailing headers: each "name:value\n", lower-case name,
// trimmed value, in the order sent, the signature line excluded.
func (v *ChunkVerifier) Trailer(canonical []byte, signature string) error {
	if v.failed {
		return ErrChunkSignature
	}
	if signature == "" {
		v.failed = true
		return fmt.Errorf("%w: the stream carries no x-amz-trailer-signature", ErrChunkSignature)
	}
	sum := sha256.Sum256(canonical)
	want := v.sign(trailerSigningAlgorithm + "\n" + v.header + v.prev + "\n" + hex.EncodeToString(sum[:]))
	if !hmac.Equal([]byte(want), []byte(signature)) {
		v.failed = true
		return ErrChunkSignature
	}
	v.prev = signature
	return nil
}

// streamChunkVerifier is the verifier of a request whose seed signature
// verified (verifySigV4 returned nil), or nil when its payload is not a
// signed stream.
func (a *Auth) streamChunkVerifier(r *http.Request, p *sigV4Params, secretKey string) *ChunkVerifier {
	if !IsSignedStreaming(r) {
		return nil
	}
	key := a.deriveSigningKey(secretKey, p.Date, p.Region, p.Service)
	return &ChunkVerifier{
		sign:    func(s string) string { return hex.EncodeToString(hmacSHA256(key, []byte(s))) },
		header:  requestAmzDate(r) + "\n" + p.Date + "/" + p.Region + "/" + p.Service + "/" + aws4Request + "\n",
		prev:    p.Signature,
		trailer: r.Header.Get("X-Amz-Content-Sha256") == streamingSignedPayloadTrailer,
	}
}

// signedStreamBody carries a request's ChunkVerifier on its body, the way
// payloadVerifyReader binds a whole payload: the request the verifier saw is
// the one the handler reads.
type signedStreamBody struct {
	io.ReadCloser
	v *ChunkVerifier
}

// StreamChunkVerifier is the verifier ValidateRequest attached to a signed
// streaming request's body, or nil (an unsigned stream, a presigned or
// UNSIGNED-PAYLOAD request, a server without a database).
func StreamChunkVerifier(body io.Reader) *ChunkVerifier {
	if b, ok := body.(*signedStreamBody); ok {
		return b.v
	}
	return nil
}
