package dataplane

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"hash/crc64"
	"io"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// CRC64NVME's check value (the CRC catalogue: "123456789" →
// 0xAE8B14860A799888) and aws-sdk-go-v2's table are the same function.
func TestCRC64NVME_CheckValue(t *testing.T) {
	assert.Equal(t, uint64(0xAE8B14860A799888), crc64.Checksum([]byte("123456789"), crc64NVMETable))
}

// D-EDGE-31 on a host: the decoder yields the decoded bytes and verifies
// the trailer at the end; a wrong checksum is ErrChecksumMismatch, a body
// that decodes to another length than declared or is badly framed is
// ErrFraming.
func TestAWSChunkedDecoder(t *testing.T) {
	data := bytes.Repeat([]byte("stored-edge-"), 300000) // 3.6 MB, four 1 MiB chunks
	for _, trailer := range []string{TrailerCRC64NVME, TrailerCRC32} {
		framed := FrameAWSChunked(data, trailer, 1<<20)
		d, err := NewAWSChunkedDecoder(bytes.NewReader(framed), trailer, int64(len(data)))
		require.NoError(t, err)
		got, err := io.ReadAll(d)
		require.NoError(t, err, trailer)
		assert.True(t, bytes.Equal(data, got), trailer)
	}

	// The trailer names another checksum (one bit flipped).
	framed := FrameAWSChunked(data, TrailerCRC64NVME, 1<<20)
	i := bytes.LastIndex(framed, []byte(TrailerCRC64NVME+":"))
	end := bytes.Index(framed[i:], []byte("\r\n"))
	val := string(framed[i+len(TrailerCRC64NVME)+1 : i+end])
	sum, _ := base64.StdEncoding.DecodeString(val)
	binary.BigEndian.PutUint64(sum, binary.BigEndian.Uint64(sum)^1)
	bad := append(append(append([]byte{}, framed[:i+len(TrailerCRC64NVME)+1]...), []byte(base64.StdEncoding.EncodeToString(sum))...), framed[i+end:]...)
	d, err := NewAWSChunkedDecoder(bytes.NewReader(bad), TrailerCRC64NVME, int64(len(data)))
	require.NoError(t, err)
	_, err = io.ReadAll(d)
	assert.ErrorIs(t, err, ErrChecksumMismatch)

	// A declared length one byte off: framing (never the client's checksum 400).
	d, _ = NewAWSChunkedDecoder(bytes.NewReader(framed), TrailerCRC64NVME, int64(len(data))+1)
	_, err = io.ReadAll(d)
	assert.ErrorIs(t, err, ErrFraming)
	d, _ = NewAWSChunkedDecoder(bytes.NewReader(framed), TrailerCRC64NVME, int64(len(data))-1)
	_, err = io.ReadAll(d)
	assert.ErrorIs(t, err, ErrFraming)

	// Cut inside a chunk, a size that is not hex, a missing trailer.
	for name, body := range map[string][]byte{
		"cut":        framed[:len(framed)/2],
		"not hex":    []byte("zz\r\nabc\r\n0\r\n\r\n"),
		"no trailer": []byte("3\r\nabc\r\n0\r\n\r\n"),
		"long line":  []byte(strings.Repeat("1", 5000) + "\r\n"),
	} {
		d, _ := NewAWSChunkedDecoder(bytes.NewReader(body), TrailerCRC64NVME, -1)
		_, err := io.ReadAll(d)
		assert.True(t, errors.Is(err, ErrFraming), "%s: %v", name, err)
	}

	// A trailer the host does not verify is refused up front.
	_, err = NewAWSChunkedDecoder(bytes.NewReader(nil), "x-amz-checksum-sha256", 0)
	assert.ErrorIs(t, err, ErrFraming)

	// An empty body: the terminal chunk and the trailer of nothing.
	empty := FrameAWSChunked(nil, TrailerCRC32, 1<<20)
	d, _ = NewAWSChunkedDecoder(bytes.NewReader(empty), TrailerCRC32, 0)
	got, err := io.ReadAll(d)
	require.NoError(t, err)
	assert.Empty(t, got)
}
