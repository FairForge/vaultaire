package api

import (
	"crypto/md5" // #nosec G501 -- the S3 ETag of a single-part object IS the MD5 of its bytes; compared, never a security primitive
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"regexp"

	"github.com/prometheus/client_golang/prometheus"
)

// WP-R8-3 (R8-10): the whole-object read path checked the SIZE of what it
// served and nothing else — a corrupted, swapped or (length-lax) truncated
// blob was served 200 under the recorded ETag. A single-part object's ETag is
// the MD5 of its bytes (the plaintext's, for a sealed object), so the body a
// whole 200 sends is hashed as it streams and compared with the row at its
// end.
//
// The status line and every byte but the last are out by then: HTTP has no
// trailer the S3 clients read, so the one honest signal is the cut — the
// reader withholds the object's LAST byte until the hash has matched, and on
// a mismatch the response is cut (cutUnlessClientGone → abortResponse): a
// Content-Length client sees a short read, an HTTP/2 client a stream reset,
// and aws-cli, rclone and the SDKs retry a cut body. Never a clean end of
// wrong bytes.
//
// What is not verified (counted in vaultaire_object_integrity_unverified_total
// once per response): a Range (206) — only the whole body hashes to the
// ETag; a multipart object (`-N`: the MD5 of the parts' MD5s); a row whose
// ETag is not an MD5 at all. A chunked object verifies every chunk itself
// (three ways) and is neither.

var (
	objectIntegrityFailures = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "vaultaire_object_integrity_failures_total",
		Help: "Whole-object GETs whose body did not match the row: md5 (the bytes do not hash to the recorded single-part ETag) | size (the backend gave fewer or more bytes than the row records). The response was cut before its last byte.",
	}, []string{"path", "reason"})
	objectIntegrityUnverified = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "vaultaire_object_integrity_unverified_total",
		Help: "Whole-object responses served without a body check, once per response: range (a 206) | multipart (a -N ETag) | etag (the recorded ETag is not an MD5) | unverified (path edge: a read the Worker served — only its length was checked). Chunked objects verify every chunk and are not counted.",
	}, []string{"path", "reason"})
)

// Paths of the two counters.
const (
	integrityPathPlain    = "plain"
	integrityPathParallel = "parallel"
	integrityPathSealed   = "sealed"
	integrityPathParity   = "parity"
	integrityPathCDN      = "cdn"
	// integrityPathEdge: a read the Worker served from the backend's
	// presigned GET (WP-EDGE-2 Part 0, RP66-2). Beyond the Worker's length
	// check nothing verifies those bytes, so every served edge read counts
	// here with reason "unverified" — the one reason of this path.
	integrityPathEdge = "edge"
)

// Every series exists at 0 from boot. Prometheus's increase() and rate()
// compare the samples of a series, so a counter CREATED by its first event is
// born at 1 and shows no rise: ObjectIntegrityFailure (increase[1h] > 0) never
// saw a path's first failure — proven with `promtool test rules` on the box
// (WP-P13 post-merge review, RP14-9). The same pre-initialisation as
// sseBlobsOpened's.
func init() {
	for _, p := range []string{integrityPathPlain, integrityPathParallel, integrityPathSealed, integrityPathParity, integrityPathCDN} {
		for _, r := range []string{"md5", "size"} {
			objectIntegrityFailures.WithLabelValues(p, r)
		}
		for _, r := range []string{"range", "multipart", "etag"} {
			objectIntegrityUnverified.WithLabelValues(p, r)
		}
	}
	objectIntegrityUnverified.WithLabelValues(integrityPathEdge, "unverified")
	// host=slc verifies a whole edge read (RP74-4): its failures too.
	for _, r := range []string{"md5", "size"} {
		objectIntegrityFailures.WithLabelValues(integrityPathEdge, r)
	}
}

var (
	singlePartETag = regexp.MustCompile(`^[0-9a-f]{32}$`)
	multipartETag  = regexp.MustCompile(`^[0-9a-f]{32}-[0-9]+$`)
)

// errObjectIntegrity: the body did not match its row (integrityError).
var errObjectIntegrity = errors.New("object integrity")

// integrityError says how the body differed: reason md5 or size.
type integrityError struct {
	reason   string
	recorded string // the digest the body was checked against: the ETag, or a -N row's body_md5
	measured string // the MD5 of what was read (md5), or the byte count (size)
}

func (e *integrityError) Error() string {
	return fmt.Sprintf("object integrity: %s: recorded %s, measured %s", e.reason, e.recorded, e.measured)
}

func (e *integrityError) Unwrap() error { return errObjectIntegrity }

// integrityDigest is what a whole 200 is verified against (A.4, 094): the
// row's whole-body digest when it has one (a Complete's `-N` object), else
// its ETag — a single-part MD5, or a `-N` of a row written before 094, which
// stays unverified{multipart}.
func integrityDigest(bodyMD5, etag string) string {
	if bodyMD5 != "" {
		return bodyMD5
	}
	return etag
}

// unverifiedReason is why a whole response with this ETag cannot be checked
// ("" = it can: a single-part MD5).
func unverifiedReason(etag string) string {
	switch {
	case singlePartETag.MatchString(etag):
		return ""
	case multipartETag.MatchString(etag):
		return "multipart"
	default:
		return "etag"
	}
}

// integrityReader hashes src as it is read and compares the hash with the
// row's single-part ETag when the row's size has been read, withholding the
// object's last byte until it matches. A source that ends before size, or
// runs past it, fails as reason "size".
type integrityReader struct {
	src  io.Reader
	etag string
	size int64
	h    hash.Hash
	pos  int64
	err  error // sticky: the integrity failure once seen
	done bool  // verified
}

// newIntegrityReader arms the check for a whole body of size bytes whose row
// records etag; ok is false (src returned as is) when the ETag is not a
// single MD5 or the object is empty (nothing to withhold, nothing to cut).
func newIntegrityReader(src io.Reader, etag string, size int64) (io.Reader, bool) {
	if size <= 0 || unverifiedReason(etag) != "" {
		return src, false
	}
	return &integrityReader{src: src, etag: etag, size: size, h: md5.New()}, true // #nosec G401 -- see the import
}

func (r *integrityReader) Read(p []byte) (int, error) {
	if r.err != nil {
		return 0, r.err
	}
	if r.done {
		// Verified: anything more is past the row's size. The client has its
		// Content-Length already; the engine's expected size refuses a blob
		// of another length before the status line.
		return r.src.Read(p)
	}
	n, err := r.src.Read(p)
	if n > 0 {
		if r.pos+int64(n) > r.size {
			keep := r.size - r.pos - 1 // never the last byte
			if keep < 0 {
				keep = 0
			}
			r.h.Write(p[:keep])
			r.err = &integrityError{reason: "size", recorded: r.etag, measured: fmt.Sprintf("more than %d bytes", r.size)}
			r.pos += keep
			return int(keep), r.err
		}
		r.h.Write(p[:n])
		r.pos += int64(n)
		if r.pos == r.size {
			if got := hex.EncodeToString(r.h.Sum(nil)); got != r.etag {
				r.err = &integrityError{reason: "md5", recorded: r.etag, measured: got}
				return n - 1, r.err // the last byte is withheld
			}
			r.done = true
			return n, err
		}
		if errors.Is(err, io.EOF) {
			r.err = &integrityError{reason: "size", recorded: r.etag, measured: fmt.Sprintf("%d of %d bytes", r.pos, r.size)}
			return n, r.err
		}
		return n, err
	}
	if errors.Is(err, io.EOF) && r.pos < r.size {
		r.err = &integrityError{reason: "size", recorded: r.etag, measured: fmt.Sprintf("%d of %d bytes", r.pos, r.size)}
		return 0, r.err
	}
	return n, err
}
