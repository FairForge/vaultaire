package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/FairForge/vaultaire/internal/drivers"
	"github.com/FairForge/vaultaire/internal/engine"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// The x-amz-checksum-* a client sends — as an aws-chunked trailer
// (aws-cli ≥ 2.23's default over TLS: STREAMING-UNSIGNED-PAYLOAD-TRAILER) or
// as a request header (aws-cli over plain HTTP) — is checked on the origin
// path over the decoded bytes, before the commit. The fixtures are raw
// aws-cli 2.28.19 captures (testdata/aws-checksum): the values are the SDK's
// own (crc32, crc32c, crc64nvme, sha1, sha256).

type awsCapture struct {
	Method, Path, Host string
	ContentLength      int64
	Header             http.Header
	Body               []byte
}

func loadAWSCapture(t *testing.T, name string) awsCapture {
	t.Helper()
	raw, err := os.ReadFile("testdata/aws-checksum/" + name) // #nosec G304 -- test fixture
	require.NoError(t, err)
	var c awsCapture
	require.NoError(t, json.Unmarshal(raw, &c))
	return c
}

// replay hands the adapter the captured request (past the auth layer: the
// capture's signature is for its own day and host), body optionally edited.
func (c awsCapture) replay(t *testing.T, f *adapterTestFixture, key string, edit func([]byte) []byte) *httptest.ResponseRecorder {
	t.Helper()
	body := append([]byte(nil), c.Body...)
	if edit != nil {
		body = edit(body)
	}
	return adapterPut(t, f, key, body, func(r *http.Request) {
		for k, v := range c.Header {
			if strings.EqualFold(k, "Authorization") {
				continue
			}
			r.Header[k] = v
		}
		r.ContentLength = c.ContentLength
		if c.ContentLength < 0 {
			r.ContentLength = -1
			r.TransferEncoding = []string{"chunked"}
		}
	})
}

func TestAWSChunked_TrailerChecksumVerified(t *testing.T) {
	f := setupAdapterFixture(t)
	for _, tc := range []struct{ file, header, value string }{
		{"awscli-trailer-crc32.json", "x-amz-checksum-crc32", "JHOjjg=="},
		{"awscli-trailer-crc32c.json", "x-amz-checksum-crc32c", "vmXXqQ=="},
		{"awscli-trailer-crc64nvme.json", "x-amz-checksum-crc64nvme", "JK0/pFSlpjA="},
		{"awscli-trailer-sha1.json", "x-amz-checksum-sha1", "xvMS0FDJy5aFRLHebnHRXbiO4+8="},
		{"awscli-trailer-sha256.json", "x-amz-checksum-sha256", "9sbXkyPqXAd9+4/R/aVq9FQ2e8eMueQUsixb4lbwQhk="},
	} {
		t.Run(tc.header, func(t *testing.T) {
			c := loadAWSCapture(t, tc.file)
			require.Equal(t, "STREAMING-UNSIGNED-PAYLOAD-TRAILER", c.Header.Get("X-Amz-Content-Sha256"))
			require.Equal(t, tc.header, c.Header.Get("X-Amz-Trailer"))
			require.Contains(t, string(c.Body), tc.header+":"+tc.value, "the SDK's value")

			key := strings.TrimSuffix(tc.file, ".json")
			w := c.replay(t, f, key, nil)
			require.Equal(t, http.StatusOK, w.Code, w.Body.String())
			_, _, ok := putHeadRow(t, f, key)
			assert.True(t, ok)

			// The same body with a wrong trailer value: 400, nothing committed.
			w = c.replay(t, f, key+"-bad", func(b []byte) []byte {
				return bytes.Replace(b, []byte(tc.header+":"+tc.value), []byte(tc.header+":"+strings.Repeat("A", len(tc.value)-1)+"="), 1)
			})
			require.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
			assert.Equal(t, ErrXAmzContentChecksumMismatch, s3Code(t, w))
			_, _, ok = putHeadRow(t, f, key+"-bad")
			assert.False(t, ok, "nothing committed")

			// The right trailer over other bytes: the same refusal.
			w = c.replay(t, f, key+"-flip", func(b []byte) []byte {
				i := bytes.Index(b, []byte("\r\n")) + 2
				b[i] ^= 0x01
				return b
			})
			require.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
			assert.Equal(t, ErrXAmzContentChecksumMismatch, s3Code(t, w))
			_, _, ok = putHeadRow(t, f, key+"-flip")
			assert.False(t, ok)
		})
	}

	t.Run("a declared trailer that never arrives", func(t *testing.T) {
		c := loadAWSCapture(t, "awscli-trailer-crc32.json")
		w := c.replay(t, f, "missing-trailer", func(b []byte) []byte {
			i := bytes.Index(b, []byte("x-amz-checksum-crc32:"))
			return append(b[:i], "\r\n"...)
		})
		require.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
		assert.Equal(t, ErrXAmzContentChecksumMismatch, s3Code(t, w))
	})

	for _, tc := range []struct{ file, header string }{
		{"awscli-header-crc64nvme.json", "X-Amz-Checksum-Crc64nvme"},
		{"awscli-header-sha256.json", "X-Amz-Checksum-Sha256"},
	} {
		t.Run("request header "+tc.header, func(t *testing.T) {
			c := loadAWSCapture(t, tc.file)
			require.NotEmpty(t, c.Header.Get(tc.header))
			key := strings.TrimSuffix(tc.file, ".json")
			w := c.replay(t, f, key, nil)
			require.Equal(t, http.StatusOK, w.Code, w.Body.String())

			w = c.replay(t, f, key+"-flip", func(b []byte) []byte { b[7] ^= 0x01; return b })
			require.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
			assert.Equal(t, ErrXAmzContentChecksumMismatch, s3Code(t, w))
			_, _, ok := putHeadRow(t, f, key+"-flip")
			assert.False(t, ok, "nothing committed")
		})
	}
}

// Prod 2026-10-29 (Stage 1a Prompt 52E PR A's proof): a wrong CRC32 trailer
// was stored through iDrive while the local driver refused it — the decoder
// was never read to its end. The body is drained before the commit.
func TestAWSChunked_TrailerCheckedWhenTheDriverStopsAtTheDeclaredLength(t *testing.T) {
	f := setupAdapterFixture(t)
	eng := engine.NewEngine(nil, zap.NewNop(), nil)
	// exactLengthDriver (prompt_2b3_d2_test.go) reads what putSinglePartIfSmall
	// reads: the declared length, never past it.
	eng.AddDriver("local", &exactLengthDriver{drivers.NewLocalDriver(f.tempDir, zap.NewNop())})
	eng.SetPrimary("local")
	f.adapter = NewS3ToEngine(eng, f.db, zap.NewNop())

	c := loadAWSCapture(t, "awscli-trailer-crc32.json")
	w := c.replay(t, f, "exact-ok", nil)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	w = c.replay(t, f, "exact-bad", func(b []byte) []byte {
		return bytes.Replace(b, []byte("x-amz-checksum-crc32:JHOjjg=="), []byte("x-amz-checksum-crc32:AAAAAA=="), 1)
	})
	require.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
	assert.Equal(t, ErrXAmzContentChecksumMismatch, s3Code(t, w))
	_, _, ok := putHeadRow(t, f, "exact-bad")
	assert.False(t, ok, "nothing committed")
}
