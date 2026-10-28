package dataplane

// aws-chunked decoding on a data-plane host (D-EDGE-31, the Worker's
// src/awschunked.js in Go): the framing aws-cli ≥ 2.23 sends by default
// (x-amz-content-sha256 STREAMING-UNSIGNED-PAYLOAD-TRAILER, Content-Encoding
// aws-chunked) taken apart in the codec's byte path — the chunk-size lines,
// the data, the terminal chunk, the trailer — and the trailer's checksum
// VERIFIED over the decoded bytes (CRC64NVME, aws-cli's default since 2.23,
// or CRC32). A trailer that does not match is the client's 400
// XAmzContentChecksumMismatch (the commit's checksum_mismatch); a framing
// error, or a decoded length that is not the declared one, is the host's
// edge_error. The MOVER path never decodes: it forwards the framing.
//
// Go's hash/crc64 with table 0x9a6c9329ac4bc9b5 is aws-sdk-go-v2's own
// CRC64NVME (the reflected polynomial; init and xorout all ones).

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"hash"
	"hash/crc32"
	"hash/crc64"
	"io"
	"strconv"
	"strings"
)

// The trailers a host verifies (the control plane admits only these).
const (
	TrailerCRC64NVME = "x-amz-checksum-crc64nvme"
	TrailerCRC32     = "x-amz-checksum-crc32"
)

// crc64NVMETable is CRC-64/NVME's table (aws-sdk-go-v2's).
var crc64NVMETable = crc64.MakeTable(0x9a6c9329ac4bc9b5)

// ErrChecksumMismatch: the trailer's checksum is not the decoded body's —
// the client's 400 XAmzContentChecksumMismatch.
var ErrChecksumMismatch = errors.New("aws-chunked: the trailer's checksum is not the body's")

// ErrFraming: the body is not well-framed aws-chunked, or decodes to
// another length than declared.
var ErrFraming = errors.New("aws-chunked: framing")

const (
	awsChunkedLineMax    = 4 << 10
	awsChunkedTrailerMax = 16 << 10
)

// AWSChunkedDecoder reads the decoded body of an aws-chunked stream and
// verifies its trailer at the end (Read returns io.EOF only after the
// trailer verified and the decoded length matched).
type AWSChunkedDecoder struct {
	r        *bufio.Reader
	trailer  string
	h        hash.Hash
	declared int64
	n        int64
	left     int64
	done     bool
	err      error
}

// NewAWSChunkedDecoder decodes r, whose trailer must be trailer (one of
// the verified ones) and whose decoded length must be declared.
func NewAWSChunkedDecoder(r io.Reader, trailer string, declared int64) (*AWSChunkedDecoder, error) {
	var h hash.Hash
	switch strings.ToLower(trailer) {
	case TrailerCRC64NVME:
		h = crc64.New(crc64NVMETable)
	case TrailerCRC32:
		h = crc32.NewIEEE()
	default:
		return nil, fmt.Errorf("%w: trailer %q is not one the host verifies", ErrFraming, trailer)
	}
	return &AWSChunkedDecoder{r: bufio.NewReaderSize(r, 64<<10), trailer: strings.ToLower(trailer), h: h, declared: declared}, nil
}

// readLine reads one CRLF-terminated line of at most max bytes.
func (d *AWSChunkedDecoder) readLine(max int) (string, error) {
	var b []byte
	for {
		frag, err := d.r.ReadSlice('\n')
		b = append(b, frag...)
		if len(b) > max {
			return "", fmt.Errorf("%w: a line longer than %d bytes", ErrFraming, max)
		}
		if err == nil {
			break
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		if errors.Is(err, io.EOF) {
			return "", fmt.Errorf("%w: the stream ended inside a line: %w", ErrFraming, io.ErrUnexpectedEOF)
		}
		return "", err
	}
	if !bytes.HasSuffix(b, []byte("\r\n")) {
		return "", fmt.Errorf("%w: a line not ended by CRLF", ErrFraming)
	}
	return string(b[:len(b)-2]), nil
}

func (d *AWSChunkedDecoder) fail(err error) (int, error) {
	d.err = err
	return 0, err
}

// Read implements io.Reader.
func (d *AWSChunkedDecoder) Read(p []byte) (int, error) {
	if d.err != nil {
		return 0, d.err
	}
	if d.done {
		return 0, io.EOF
	}
	for d.left == 0 {
		line, err := d.readLine(awsChunkedLineMax)
		if err != nil {
			return d.fail(err)
		}
		if i := strings.IndexByte(line, ';'); i >= 0 {
			line = line[:i] // a chunk extension: the unsigned variant signs nothing
		}
		size, err := strconv.ParseInt(strings.TrimSpace(line), 16, 64)
		if err != nil || size < 0 {
			return d.fail(fmt.Errorf("%w: chunk size %q", ErrFraming, line))
		}
		if size == 0 {
			if err := d.finish(); err != nil {
				return d.fail(err)
			}
			d.done = true
			return 0, io.EOF
		}
		d.left = size
	}
	if int64(len(p)) > d.left {
		p = p[:d.left]
	}
	n, err := d.r.Read(p)
	d.left -= int64(n)
	d.n += int64(n)
	_, _ = d.h.Write(p[:n])
	if err != nil && !errors.Is(err, io.EOF) {
		return n, d.setErr(err)
	}
	if errors.Is(err, io.EOF) && d.left > 0 {
		return n, d.setErr(fmt.Errorf("%w: the stream ended inside a chunk: %w", ErrFraming, io.ErrUnexpectedEOF))
	}
	if d.left == 0 {
		var crlf [2]byte
		if _, err := io.ReadFull(d.r, crlf[:]); err != nil || crlf != [2]byte{'\r', '\n'} {
			return n, d.setErr(fmt.Errorf("%w: a chunk is not followed by CRLF", ErrFraming))
		}
	}
	if d.declared >= 0 && d.n > d.declared {
		return n, d.setErr(fmt.Errorf("%w: the body decodes past its declared %d bytes", ErrFraming, d.declared))
	}
	return n, nil
}

func (d *AWSChunkedDecoder) setErr(err error) error {
	d.err = err
	return err
}

// finish reads the trailer section after the terminal chunk and verifies
// the checksum and the decoded length.
func (d *AWSChunkedDecoder) finish() error {
	var got string
	read := 0
	for {
		line, err := d.readLine(awsChunkedTrailerMax)
		if err != nil {
			return err
		}
		read += len(line) + 2
		if read > awsChunkedTrailerMax {
			return fmt.Errorf("%w: a trailer longer than %d bytes", ErrFraming, awsChunkedTrailerMax)
		}
		if line == "" {
			break
		}
		name, value, ok := strings.Cut(line, ":")
		if !ok {
			return fmt.Errorf("%w: a trailer line without a colon", ErrFraming)
		}
		if strings.EqualFold(strings.TrimSpace(name), d.trailer) {
			got = strings.TrimSpace(value)
		}
	}
	if d.declared >= 0 && d.n != d.declared {
		return fmt.Errorf("%w: the body decodes to %d bytes, %d declared", ErrFraming, d.n, d.declared)
	}
	if got == "" {
		return fmt.Errorf("%w: the trailer %s is missing", ErrFraming, d.trailer)
	}
	want, err := base64.StdEncoding.DecodeString(got)
	if err != nil {
		return fmt.Errorf("%w: the trailer's value is not base64", ErrFraming)
	}
	if !bytes.Equal(want, d.h.Sum(nil)) {
		return ErrChecksumMismatch
	}
	return nil
}

// FrameAWSChunked frames data as aws-chunked with its trailer (chunk bytes
// per data chunk) — what aws-cli sends; the tests' and the lab's encoder.
func FrameAWSChunked(data []byte, trailer string, chunk int) []byte {
	var sum []byte
	switch strings.ToLower(trailer) {
	case TrailerCRC32:
		sum = binary.BigEndian.AppendUint32(nil, crc32.ChecksumIEEE(data))
	default:
		sum = binary.BigEndian.AppendUint64(nil, crc64.Checksum(data, crc64NVMETable))
	}
	var b bytes.Buffer
	for o := 0; o < len(data); o += chunk {
		n := min(chunk, len(data)-o)
		fmt.Fprintf(&b, "%x\r\n", n)
		b.Write(data[o : o+n])
		b.WriteString("\r\n")
	}
	fmt.Fprintf(&b, "0\r\n%s:%s\r\n\r\n", trailer, base64.StdEncoding.EncodeToString(sum))
	return b.Bytes()
}
