package api

// WP-R3-1: the staged parts of a CompleteMultipartUpload, read in order as
// ONE stream and re-hashed on the way. A part file is checked against the
// MD5 its UploadPart answered (multipart_parts.etag) the moment its last
// byte is read — with that last byte, so a consumer that reads exactly the
// declared length and never asks again (an S3 SDK with a Content-Length)
// still gets the error and the write fails: nothing is committed. Before, a
// part that changed at rest (same size: the stat check passes) was stored
// under the ETag of the bytes the client sent — the staged-part corruption
// class, R7-12's cousin. Streaming: one part file open at a time, the
// consumer's buffer only.

import (
	"crypto/md5" // #nosec G501 — the S3 part ETag is the part's MD5
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"strings"
)

// errStagedPartCorrupt: a staged part no longer hashes to the ETag its
// UploadPart answered, or is shorter than recorded.
var errStagedPartCorrupt = errors.New("a staged part does not match what was uploaded")

// stagedPartReadHook runs as each part file is opened (tests cut a Complete
// there).
var stagedPartReadHook = func(partNumber int) {}

// stagedPartsReader is the parts of uploadID, in order, as one stream.
type stagedPartsReader struct {
	uploadID string
	parts    []partRecord
	i        int      // the part being read
	f        *os.File // its file, nil between parts
	h        hash.Hash
	left     int64 // bytes of the part still to read
	err      error // sticky
}

func newStagedPartsReader(uploadID string, parts []partRecord) *stagedPartsReader {
	return &stagedPartsReader{uploadID: uploadID, parts: parts}
}

func (s *stagedPartsReader) Read(p []byte) (int, error) {
	if s.err != nil {
		return 0, s.err
	}
	for s.f == nil {
		if s.i >= len(s.parts) {
			return 0, io.EOF
		}
		part := s.parts[s.i]
		stagedPartReadHook(part.PartNumber)
		f, err := os.Open(partFilePath(s.uploadID, part.PartNumber)) // #nosec G304 — path derived from a validated upload id
		if err != nil {
			s.err = fmt.Errorf("open part %d: %w", part.PartNumber, err)
			return 0, s.err
		}
		s.f, s.h, s.left = f, md5.New(), part.Size // #nosec G401 — the S3 part ETag
		if s.left == 0 {
			if err := s.finishPart(); err != nil {
				s.err = err
				return 0, err
			}
		}
	}
	if int64(len(p)) > s.left {
		p = p[:s.left]
	}
	n, err := s.f.Read(p)
	s.h.Write(p[:n])
	s.left -= int64(n)
	if s.left == 0 {
		// The part is complete: checked now, with its last bytes.
		if verr := s.finishPart(); verr != nil {
			s.err = verr
			return n, verr
		}
		return n, nil
	}
	if err == io.EOF {
		s.err = fmt.Errorf("%w: part %d ended %d bytes short", errStagedPartCorrupt, s.parts[s.i].PartNumber, s.left)
		return n, s.err
	}
	if err != nil {
		s.err = fmt.Errorf("read part %d: %w", s.parts[s.i].PartNumber, err)
		return n, s.err
	}
	return n, nil
}

// finishPart checks the part just read against its recorded ETag and moves
// to the next one. An ETag that is not an MD5 (none is written today) is not
// checkable and passes.
func (s *stagedPartsReader) finishPart() error {
	part := s.parts[s.i]
	_ = s.f.Close()
	s.f = nil
	s.i++
	want := strings.ToLower(strings.Trim(part.ETag, `"`))
	if len(want) != 32 {
		return nil
	}
	if got := hex.EncodeToString(s.h.Sum(nil)); got != want {
		return fmt.Errorf("%w: part %d hashes to %s, uploaded as %s", errStagedPartCorrupt, part.PartNumber, got, want)
	}
	return nil
}

// verified reports whether every part was read to its end and matched.
func (s *stagedPartsReader) verified() bool {
	return s.err == nil && s.i == len(s.parts)
}

// Close releases the open part file, if any.
func (s *stagedPartsReader) Close() error {
	if s.f != nil {
		err := s.f.Close()
		s.f = nil
		return err
	}
	return nil
}
