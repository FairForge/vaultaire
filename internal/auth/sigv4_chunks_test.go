package auth

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// WP-R15-1. The fixtures are (1) AWS's published example "Transferring
// Payload in Multiple Chunks" (the documented secret, 64 KiB chunks, the
// documented seed and chunk signatures) and (2) raw captures of real
// clients against a known test secret: aws-cli and boto3 never sign chunks
// (botocore sends STREAMING-UNSIGNED-PAYLOAD-TRAILER over TLS and a payload
// hash over plain HTTP — captured, internal/api/testdata/aws-checksum), so
// the signed captures are minio-go's: `mc cp` over plain HTTP
// (STREAMING-AWS4-HMAC-SHA256-PAYLOAD) and minio-go's StreamingSignV4 with
// a CRC32C trailer (…-PAYLOAD-TRAILER). The body is accepted byte-identical.

const captureSecret = "vaultaire-test-secret-do-not-use-0000000000"

// captureSHA256 is the sha256 of the 70,000-byte object both captures carry.
const captureSHA256 = "f6c6d79323ea5c077dfb8fd1fda56af454367bc78cb9e414b22c5be256f04219"

// pinClock makes the skew check read t (a fixture signed in the past).
func pinClock(t *testing.T, at time.Time) {
	t.Helper()
	prev := verifierNow
	verifierNow = func() time.Time { return at }
	t.Cleanup(func() { verifierNow = prev })
}

// awsChunkExample is the request and body of AWS's documented example.
func awsChunkExample(t *testing.T) (*http.Request, []byte) {
	t.Helper()
	sigs := []string{
		"ad80c730a21e5b8d04586a2213dd63b9a0e99e0e2307b0ade35a65485a288648",
		"0055627c9e194cb4542bae2aa5492e3c1575bbb81b612b7d234b86a503ef5497",
		"b6c6ea8a5354eaf15b3cb7646744f4275b71ea724fed81ceb9323e279d449df9",
	}
	var body bytes.Buffer
	for i, n := range []int{65536, 1024, 0} {
		fmt.Fprintf(&body, "%x;chunk-signature=%s\r\n%s\r\n", n, sigs[i], strings.Repeat("a", n))
	}
	r, err := http.NewRequest(http.MethodPut, "https://s3.amazonaws.com/examplebucket/chunkObject.txt", bytes.NewReader(body.Bytes()))
	require.NoError(t, err)
	r.Host = "s3.amazonaws.com"
	r.ContentLength = int64(body.Len())
	r.Header.Set("Content-Length", "66824")
	r.Header.Set("Content-Encoding", "aws-chunked")
	r.Header.Set("X-Amz-Content-Sha256", streamingSignedPayload)
	r.Header.Set("X-Amz-Date", "20130524T000000Z")
	r.Header.Set("X-Amz-Decoded-Content-Length", "66560")
	r.Header.Set("X-Amz-Storage-Class", "REDUCED_REDUNDANCY")
	r.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential=AKIAIOSFODNN7EXAMPLE/20130524/us-east-1/s3/aws4_request,"+
		"SignedHeaders=content-encoding;content-length;host;x-amz-content-sha256;x-amz-date;x-amz-decoded-content-length;x-amz-storage-class,"+
		"Signature=4f232c4386841ef735655705268965c44a0e4690baa4adea153f7db9fa80a0a9")
	require.Equal(t, 66824, body.Len(), "the example's Content-Length")
	return r, body.Bytes()
}

// capturedRequest replays a captured request (testdata) as the origin
// received it.
func capturedRequest(t *testing.T, name string) (*http.Request, []byte) {
	t.Helper()
	raw, err := os.ReadFile("testdata/sigv4-streaming/" + name) // #nosec G304 -- test fixture
	require.NoError(t, err)
	var c struct {
		Method, Path, Host string
		ContentLength      int64
		Header             http.Header
		Body               []byte
	}
	require.NoError(t, json.Unmarshal(raw, &c))
	r, err := http.NewRequest(c.Method, "http://"+c.Host+c.Path, bytes.NewReader(c.Body))
	require.NoError(t, err)
	r.Host, r.ContentLength, r.Header = c.Host, c.ContentLength, c.Header
	return r, c.Body
}

// seedVerify runs the seed signature and returns the stream's verifier.
func seedVerify(t *testing.T, r *http.Request, secret string) *ChunkVerifier {
	t.Helper()
	ts, err := time.Parse(timeFormat, r.Header.Get("X-Amz-Date"))
	require.NoError(t, err)
	pinClock(t, ts)
	a := NewAuth(nil, zap.NewNop(), nil)
	p, err := parseSigV4AuthHeader(r.Header.Get("Authorization"))
	require.NoError(t, err)
	require.NoError(t, a.verifySigV4(r, p, secret, ""), "the seed signature")
	v := a.streamChunkVerifier(r, p, secret)
	require.NotNil(t, v)
	return v
}

// unframe is the aws-chunked framing in its plainest form (the API layer's
// decoder is internal/api's awsChunkedReader, tested against the same
// verifier there): each chunk's signature checked as it ends, then the
// trailer's.
func unframe(body []byte, v *ChunkVerifier) ([]byte, error) {
	br := bufio.NewReader(bytes.NewReader(body))
	var out bytes.Buffer
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			return nil, err
		}
		size, ext, _ := strings.Cut(strings.TrimRight(line, "\r\n"), ";")
		sig, _ := strings.CutPrefix(ext, "chunk-signature=")
		n, err := strconv.ParseInt(size, 16, 32)
		if err != nil {
			return nil, err
		}
		chunk := make([]byte, n)
		if _, err := io.ReadFull(br, chunk); err != nil {
			return nil, err
		}
		sum := sha256.Sum256(chunk)
		if err := v.Chunk(sig, sum[:]); err != nil {
			return nil, err
		}
		out.Write(chunk)
		if n == 0 {
			break
		}
		if _, err := br.Discard(2); err != nil {
			return nil, err
		}
	}
	if !v.SignedTrailer() {
		return out.Bytes(), nil
	}
	var canonical bytes.Buffer
	var sig string
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			return nil, err
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			if sig != "" {
				break
			}
			continue // minio-go writes "name:value\n\r\n" before the signature line
		}
		name, value, _ := strings.Cut(line, ":")
		if strings.EqualFold(name, "x-amz-trailer-signature") {
			sig = strings.TrimSpace(value)
			continue
		}
		canonical.WriteString(strings.ToLower(name) + ":" + strings.TrimSpace(value) + "\n")
	}
	return out.Bytes(), v.Trailer(canonical.Bytes(), sig)
}

func TestAWSChunked_SignedChunksVerified(t *testing.T) {
	t.Run("AWS published example", func(t *testing.T) {
		r, body := awsChunkExample(t)
		v := seedVerify(t, r, testSecret)
		got, err := unframe(body, v)
		require.NoError(t, err)
		assert.Equal(t, strings.Repeat("a", 66560), string(got))
	})

	for _, name := range []string{"mc-signed-chunks.json", "minio-go-signed-trailer.json"} {
		t.Run("capture "+name, func(t *testing.T) {
			r, body := capturedRequest(t, name)
			v := seedVerify(t, r, captureSecret)
			assert.Equal(t, name == "minio-go-signed-trailer.json", v.SignedTrailer())
			got, err := unframe(body, v)
			require.NoError(t, err)
			sum := sha256.Sum256(got)
			assert.Equal(t, captureSHA256, hex.EncodeToString(sum[:]), "byte-identical")
		})
	}

	t.Run("one payload byte flipped", func(t *testing.T) {
		r, body := awsChunkExample(t)
		v := seedVerify(t, r, testSecret)
		tampered := append([]byte(nil), body...)
		tampered[bytes.Index(tampered, []byte("\r\naaaa"))+100] = 'b'
		_, err := unframe(tampered, v)
		require.ErrorIs(t, err, ErrChunkSignature)
		require.ErrorIs(t, v.Chunk("whatever", nil), ErrChunkSignature, "a failed stream stays failed")
	})

	t.Run("a chunk without its chunk-signature", func(t *testing.T) {
		r, body := awsChunkExample(t)
		v := seedVerify(t, r, testSecret)
		stripped := bytes.Replace(body, []byte(";chunk-signature=0055627c9e194cb4542bae2aa5492e3c1575bbb81b612b7d234b86a503ef5497"), nil, 1)
		_, err := unframe(stripped, v)
		require.ErrorIs(t, err, ErrChunkSignature)
	})

	t.Run("chunks replayed out of order", func(t *testing.T) {
		r, _ := awsChunkExample(t)
		v := seedVerify(t, r, testSecret)
		sum := sha256.Sum256([]byte(strings.Repeat("a", 1024)))
		require.ErrorIs(t, v.Chunk("0055627c9e194cb4542bae2aa5492e3c1575bbb81b612b7d234b86a503ef5497", sum[:]), ErrChunkSignature,
			"the second chunk's signature chains from the first")
	})

	t.Run("the trailer is signed", func(t *testing.T) {
		r, body := capturedRequest(t, "minio-go-signed-trailer.json")
		v := seedVerify(t, r, captureSecret)
		_, err := unframe(bytes.Replace(body, []byte("x-amz-checksum-crc32c:/GSlGw=="), []byte("x-amz-checksum-crc32c:AAAAAA=="), 1), v)
		require.ErrorIs(t, err, ErrChunkSignature, "a rewritten trailer")

		r, body = capturedRequest(t, "minio-go-signed-trailer.json")
		v = seedVerify(t, r, captureSecret)
		i := bytes.Index(body, []byte("x-amz-trailer-signature:"))
		_, err = unframe(append(append([]byte(nil), body[:i]...), "\r\n"...), v)
		require.True(t, errors.Is(err, ErrChunkSignature) || errors.Is(err, io.EOF), "no trailer signature: %v", err)
		require.ErrorIs(t, v.Trailer([]byte("x-amz-checksum-crc32c:/GSlGw==\n"), ""), ErrChunkSignature)
	})

	t.Run("the wrong secret fails the seed, not the chunks", func(t *testing.T) {
		r, _ := awsChunkExample(t)
		pinClock(t, time.Date(2013, 5, 24, 0, 0, 0, 0, time.UTC))
		p, err := parseSigV4AuthHeader(r.Header.Get("Authorization"))
		require.NoError(t, err)
		require.ErrorIs(t, NewAuth(nil, zap.NewNop(), nil).verifySigV4(r, p, "not-the-secret", ""), ErrSignatureMismatch)
	})
}

// Only the two signed markers get a verifier; every other payload hash has
// no chunk signatures to check (presigned and UNSIGNED-PAYLOAD requests
// are unchanged).
func TestStreamChunkVerifier_OnlySignedStreams(t *testing.T) {
	a := NewAuth(nil, zap.NewNop(), nil)
	p := &sigV4Params{Date: "20130524", Region: "us-east-1", Service: "s3", Signature: "seed"}
	for marker, want := range map[string]bool{
		streamingSignedPayload:               true,
		streamingSignedPayloadTrailer:        true,
		"STREAMING-UNSIGNED-PAYLOAD-TRAILER": false,
		unsignedPayload:                      false,
		sha256Hex("x"):                       false,
	} {
		r, _ := http.NewRequest(http.MethodPut, "https://h/b/k", nil)
		r.Header.Set("X-Amz-Content-Sha256", marker)
		assert.Equal(t, want, a.streamChunkVerifier(r, p, testSecret) != nil, marker)
	}
	assert.Nil(t, StreamChunkVerifier(io.NopCloser(strings.NewReader(""))))
}
