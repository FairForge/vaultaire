package api

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/xml"
	"fmt"
	"hash/crc32"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// WP-R15-1: a STREAMING-AWS4-HMAC-SHA256-PAYLOAD(-TRAILER) body is what was
// signed, chunk by chunk, through the live entrance (handleS3Request →
// auth.ValidateRequest → the aws-chunked decoder). The verifier's fixtures
// (AWS's published example, the minio-go captures) are internal/auth's
// TestAWSChunked_SignedChunksVerified; here the same rule is held end to
// end, signed now with chunkSign — which reproduces AWS's documented chunk
// signatures (the first subtest), so it is the spec's formula.

func hmacHex(key []byte, s string) string {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(s))
	return hex.EncodeToString(m.Sum(nil))
}

func sigV4Key(secret, date, region string) []byte {
	k := hmacSum([]byte("AWS4"+secret), date)
	k = hmacSum(k, region)
	k = hmacSum(k, "s3")
	return hmacSum(k, "aws4_request")
}

func hmacSum(key []byte, s string) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(s))
	return m.Sum(nil)
}

// chunkSign is the spec's chunk signature.
func chunkSign(key []byte, ts, scope, prev string, chunk []byte) string {
	empty := sha256.Sum256(nil)
	sum := sha256.Sum256(chunk)
	return hmacHex(key, "AWS4-HMAC-SHA256-PAYLOAD\n"+ts+"\n"+scope+"\n"+prev+"\n"+hex.EncodeToString(empty[:])+"\n"+hex.EncodeToString(sum[:]))
}

// signedChunkPUT is a client's signed streaming request: the seed signed
// by aws-sdk-go-v2's signer, each chunk (chunkSize bytes) by chunkSign; a
// non-empty trailer (name → value) makes it a -TRAILER stream with
// x-amz-trailer-signature. mutate edits the framed body after signing.
func (f *edgeFixture) signedChunkPUT(path string, query url.Values, payload []byte, chunkSize int, trailer [2]string, mutate func([]byte) []byte) *http.Request {
	f.t.Helper()
	marker := "STREAMING-AWS4-HMAC-SHA256-PAYLOAD"
	if trailer[0] != "" {
		marker += "-TRAILER"
	}
	var chunks [][]byte
	for off := 0; off < len(payload); off += chunkSize {
		chunks = append(chunks, payload[off:min(off+chunkSize, len(payload))])
	}
	chunks = append(chunks, nil)
	framedLen := 0
	for _, c := range chunks {
		framedLen += len(fmt.Sprintf("%x", len(c))) + len(";chunk-signature=") + 64 + 2 + len(c) + 2
	}
	if trailer[0] != "" {
		framedLen += -2 + len(trailer[0]) + 1 + len(trailer[1]) + 2 + len("x-amz-trailer-signature:") + 64 + 2 + 2
	}

	u := "https://" + f.host + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	r, err := http.NewRequest(http.MethodPut, u, nil)
	require.NoError(f.t, err)
	r.ContentLength = int64(framedLen)
	r.Header.Set("Content-Length", fmt.Sprint(framedLen))
	r.Header.Set("Content-Encoding", "aws-chunked")
	r.Header.Set("X-Amz-Decoded-Content-Length", fmt.Sprint(len(payload)))
	if trailer[0] != "" {
		r.Header.Set("X-Amz-Trailer", trailer[0])
	}
	r.Header.Set("X-Amz-Content-Sha256", marker)
	now := time.Now().UTC()
	require.NoError(f.t, v4.NewSigner().SignHTTP(context.Background(), aws.Credentials{AccessKeyID: f.ak, SecretAccessKey: f.sk},
		r, marker, "s3", "us-east-1", now))
	_, seed, ok := strings.Cut(r.Header.Get("Authorization"), "Signature=")
	require.True(f.t, ok)

	ts, date := r.Header.Get("X-Amz-Date"), now.Format("20060102")
	scope := date + "/us-east-1/s3/aws4_request"
	key := sigV4Key(f.sk, date, "us-east-1")
	var body bytes.Buffer
	prev := seed
	for _, c := range chunks {
		prev = chunkSign(key, ts, scope, prev, c)
		fmt.Fprintf(&body, "%x;chunk-signature=%s\r\n", len(c), prev)
		body.Write(c)
		if len(c) > 0 || trailer[0] == "" {
			body.WriteString("\r\n")
		}
	}
	if trailer[0] != "" {
		line := trailer[0] + ":" + trailer[1] + "\n"
		sum := sha256.Sum256([]byte(line))
		sig := hmacHex(key, "AWS4-HMAC-SHA256-TRAILER\n"+ts+"\n"+scope+"\n"+prev+"\n"+hex.EncodeToString(sum[:]))
		body.WriteString(trailer[0] + ":" + trailer[1] + "\r\nx-amz-trailer-signature:" + sig + "\r\n\r\n")
	}
	require.Equal(f.t, framedLen, body.Len(), "the signed Content-Length")
	raw := body.Bytes()
	if mutate != nil {
		raw = mutate(append([]byte(nil), raw...))
	}
	r.Body = httpBody(raw)
	r.RemoteAddr = "203.0.113.9:4444"
	return r
}

func httpBody(b []byte) io.ReadCloser { return io.NopCloser(bytes.NewReader(b)) }

func (f *edgeFixture) serve(r *http.Request) *httptest.ResponseRecorder {
	f.t.Helper()
	w := httptest.NewRecorder()
	f.srv.handleS3Request(w, r)
	return w
}

func (f *edgeFixture) orphanWrites() int {
	var n int
	require.NoError(f.t, f.db.QueryRow(`SELECT count(*) FROM retired_blobs WHERE tenant_id = $1 AND reason = 'orphan_write'`, f.tenantID).Scan(&n))
	return n
}

func crc32cB64(b []byte) string {
	s := crc32.Checksum(b, crc32.MakeTable(crc32.Castagnoli))
	return base64.StdEncoding.EncodeToString([]byte{byte(s >> 24), byte(s >> 16), byte(s >> 8), byte(s)})
}

func TestAWSChunked_SignedChunksVerified(t *testing.T) {
	t.Run("chunkSign is AWS's documented formula", func(t *testing.T) {
		key := sigV4Key("wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY", "20130524", "us-east-1")
		prev := "4f232c4386841ef735655705268965c44a0e4690baa4adea153f7db9fa80a0a9" // the documented seed
		for i, c := range [][]byte{bytes.Repeat([]byte("a"), 65536), bytes.Repeat([]byte("a"), 1024), nil} {
			prev = chunkSign(key, "20130524T000000Z", "20130524/us-east-1/s3/aws4_request", prev, c)
			assert.Equal(t, []string{
				"ad80c730a21e5b8d04586a2213dd63b9a0e99e0e2307b0ade35a65485a288648",
				"0055627c9e194cb4542bae2aa5492e3c1575bbb81b612b7d234b86a503ef5497",
				"b6c6ea8a5354eaf15b3cb7646744f4275b71ea724fed81ceb9323e279d449df9",
			}[i], prev)
		}
	})

	f := setupEdgeFixture(t)
	payload := testBytes(150_000) // two 64 KiB chunks and a short one

	t.Run("a signed stream is stored as signed", func(t *testing.T) {
		w := f.serve(f.signedChunkPUT("/"+f.bucket+"/signed.bin", nil, payload, 64<<10, [2]string{}, nil))
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		h, ok := f.head("signed.bin")
		require.True(t, ok)
		assert.Equal(t, int64(len(payload)), h.size)
		f.drv.mu.Lock()
		stored := f.drv.objects["t-"+f.tenantID+"/"+f.container()+"/"+h.sk]
		f.drv.mu.Unlock()
		assert.Equal(t, payload, stored, "the decoded bytes")
	})

	t.Run("a signed trailer stream is stored as signed", func(t *testing.T) {
		w := f.serve(f.signedChunkPUT("/"+f.bucket+"/signed-trailer.bin", nil, payload, 64<<10,
			[2]string{"x-amz-checksum-crc32c", crc32cB64(payload)}, nil))
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		_, ok := f.head("signed-trailer.bin")
		assert.True(t, ok)
	})

	refused := func(t *testing.T, key string, r *http.Request) {
		t.Helper()
		before := f.orphanWrites()
		w := f.serve(r)
		require.Equal(t, http.StatusForbidden, w.Code, w.Body.String())
		assert.Equal(t, ErrSignatureDoesNotMatch, s3Code(t, w))
		_, ok := f.head(key)
		assert.False(t, ok, "nothing committed")
		assert.Equal(t, before+1, f.orphanWrites(), "the address that was sent the body is queued")
	}

	t.Run("one payload byte flipped", func(t *testing.T) {
		refused(t, "flipped.bin", f.signedChunkPUT("/"+f.bucket+"/flipped.bin", nil, payload, 64<<10, [2]string{}, func(b []byte) []byte {
			b[len(b)/2] ^= 0x01
			return b
		}))
	})

	t.Run("one byte flipped in the last chunk", func(t *testing.T) {
		refused(t, "flipped-last.bin", f.signedChunkPUT("/"+f.bucket+"/flipped-last.bin", nil, payload, 64<<10, [2]string{}, func(b []byte) []byte {
			i := bytes.LastIndex(b, []byte("0;chunk-signature=")) - 10
			b[i] ^= 0x01
			return b
		}))
	})

	t.Run("a chunk with no chunk-signature", func(t *testing.T) {
		refused(t, "unsigned-chunk.bin", f.signedChunkPUT("/"+f.bucket+"/unsigned-chunk.bin", nil, payload, 64<<10, [2]string{}, func(b []byte) []byte {
			i := bytes.Index(b, []byte(";chunk-signature="))
			return append(b[:i], b[i+len(";chunk-signature=")+64:]...)
		}))
	})

	t.Run("a rewritten trailer", func(t *testing.T) {
		good := crc32cB64(payload)
		refused(t, "trailer.bin", f.signedChunkPUT("/"+f.bucket+"/trailer.bin", nil, payload, 64<<10,
			[2]string{"x-amz-checksum-crc32c", good}, func(b []byte) []byte {
				return bytes.Replace(b, []byte(good), []byte(crc32cB64([]byte("other"))), 1)
			}))
	})

	t.Run("a trailer stream without its trailer signature", func(t *testing.T) {
		refused(t, "trailer-nosig.bin", f.signedChunkPUT("/"+f.bucket+"/trailer-nosig.bin", nil, payload, 64<<10,
			[2]string{"x-amz-checksum-crc32c", crc32cB64(payload)}, func(b []byte) []byte {
				i := bytes.Index(b, []byte("x-amz-trailer-signature:"))
				return append(b[:i], "\r\n"...)
			}))
	})

	t.Run("UploadPart is held to the same rule", func(t *testing.T) {
		create := f.signedHTTP(http.MethodPost, "/"+f.bucket+"/mp.bin", url.Values{"uploads": {""}}, nil, nil)
		create.RemoteAddr = "203.0.113.9:4444"
		w := f.serve(create)
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		var init struct{ UploadId string }
		require.NoError(t, xml.Unmarshal(w.Body.Bytes(), &init))
		q := url.Values{"partNumber": {"1"}, "uploadId": {init.UploadId}}

		w = f.serve(f.signedChunkPUT("/"+f.bucket+"/mp.bin", q, payload, 64<<10, [2]string{}, func(b []byte) []byte {
			b[len(b)/3] ^= 0x01
			return b
		}))
		require.Equal(t, http.StatusForbidden, w.Code, w.Body.String())
		assert.Equal(t, ErrSignatureDoesNotMatch, s3Code(t, w))
		var parts int
		require.NoError(t, f.db.QueryRow(`SELECT count(*) FROM multipart_parts WHERE upload_id = $1`, init.UploadId).Scan(&parts))
		assert.Zero(t, parts, "no part staged")

		w = f.serve(f.signedChunkPUT("/"+f.bucket+"/mp.bin", q, payload, 64<<10, [2]string{}, nil))
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	})

	t.Run("UNSIGNED-PAYLOAD is unchanged", func(t *testing.T) {
		r := f.signedHTTP(http.MethodPut, "/"+f.bucket+"/plain.bin", nil, payload, nil)
		r.RemoteAddr = "203.0.113.9:4444"
		r.Header.Set("X-Amz-Content-Sha256", "UNSIGNED-PAYLOAD")
		require.NoError(t, v4.NewSigner().SignHTTP(context.Background(), aws.Credentials{AccessKeyID: f.ak, SecretAccessKey: f.sk},
			r, "UNSIGNED-PAYLOAD", "s3", "us-east-1", time.Now()))
		w := f.serve(r)
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	})
}

// A signed-chunk stream is the origin's at the edge (the client's key signs
// every chunk; only the origin holds the verifier), as before WP-R15-1.
func TestEdgeAdmit_SignedChunkStreamsPassThrough(t *testing.T) {
	f := setupEdgeFixture(t)
	for _, marker := range []string{"STREAMING-AWS4-HMAC-SHA256-PAYLOAD", "STREAMING-AWS4-HMAC-SHA256-PAYLOAD-TRAILER"} {
		resp := f.admit(f.signedStreaming("/"+f.bucket+"/s-"+uuid.New().String()[:6], nil, 5<<20, "x-amz-checksum-crc64nvme", marker))
		assert.False(t, resp.Edge, marker)
		assert.Equal(t, "aws_chunked", resp.Reason, marker)
	}
	assert.Zero(t, f.used(), "nothing reserved")
}
