package api

import (
	"crypto/subtle"
	"database/sql"
	"fmt"

	"github.com/FairForge/vaultaire/internal/crypto"
	"net/http"
	"strconv"
	"time"
)

// WP-R2-4: HEAD, a 200 and a 206 of one object did not agree on their
// headers (R2-22: the native 206 omitted the identity headers until
// 2026-10-04; HEAD omitted Accept-Ranges, the version id on GET's default and
// GET's default Cache-Control) — and aws s3 sync, rclone and the SDKs compare
// them. objectHeaders is the one writer: built once from the row (the head
// row, or a version's own row — R61-8), written by HEAD, headVersion, the
// whole and ranged GET, the sealed and parallel readers and the chunked GET.
// The responses differ in exactly two ways: Content-Length is the object's
// on HEAD and 200 and the range's on 206, and a 206 adds Content-Range. A 304
// carries ETag, Last-Modified and — RFC 9110 §15.4.5: a 304 MUST carry what
// a 200 would have sent of Cache-Control and Expires, so a cache refreshes
// its freshness — Cache-Control and Expires; nothing else of the object. /cdn keeps its own writer: its Cache-Control (the bucket's
// max-age), its Content-Disposition rule and CORS are the public surface's,
// and no S3 client compares a /cdn response with a HEAD.

// objectHeaders is what an object's responses say about it.
type objectHeaders struct {
	ETag         string
	LastModified time.Time
	ContentType  string
	Size         int64
	StorageClass string // engine.CustomerStorageClass(floor, backend)
	// VersionID is the version the response names: the requested one, the
	// latest, "null" on a versioned bucket for an object with no version
	// row; "" on a bucket that was never versioned (no header, as AWS).
	VersionID string
	Metadata  []byte // the row's metadata JSON (x-amz-meta-*)
	TagCount  int
	// Disposition is the value to send: ?response-content-disposition over
	// the stored one (sanitized when written).
	Disposition, Encoding, Language        string
	CacheControl, Expires, WebsiteRedirect string
	// SSE is "AES256" for SSE-S3 and chunk encryption, "ssec" for SSE-C, ""
	// for none. SSECKeyMD5 is the row's key digest (094), sent back as
	// x-amz-server-side-encryption-customer-key-MD5 as AWS does (WP-SSEC-1).
	SSE        string
	SSECKeyMD5 string
	// The Object Lock headers (RP17-1): what the key's object_locks row says
	// — LockMode and LockRetainUntil for a retention, LegalHold "ON" or
	// "OFF" when a row exists at all. All empty without a row: AWS sends no
	// lock header then, never an empty one. The row is the KEY's (there is
	// no per-version row; checkObjectLock enforces it on every version), so
	// a version names the same lock.
	LockMode        string
	LockRetainUntil time.Time
	LegalHold       string
}

// lockHeadersOf turns a LEFT-JOINed object_locks row into the three
// values: no row (hold NULL) = none of them.
func lockHeadersOf(mode string, until sql.NullTime, hold sql.NullBool) (lockMode string, retainUntil time.Time, legalHold string) {
	if !hold.Valid {
		return "", time.Time{}, ""
	}
	legalHold = "OFF"
	if hold.Bool {
		legalHold = "ON"
	}
	if mode != "" && until.Valid {
		lockMode, retainUntil = mode, until.Time
	}
	return lockMode, retainUntil, legalHold
}

// objectLockJoin is the join every object query makes to carry the key's
// lock row (object_head_cache or object_versions aliased as the table it
// joins from); objectLockColumns the three columns it selects.
const objectLockColumns = `COALESCE(l.retention_mode, ''), l.retain_until_date, l.legal_hold`

// The modes a response writes the object's headers in.
type headerMode int

const (
	headersHead        headerMode = iota // HEAD: the whole object's length, no body
	headersWhole                         // GET 200
	headersRange                         // GET or HEAD 206 of rng
	headersNotModified                   // 304: ETag and Last-Modified only
)

// defaultObjectCacheControl is what a response says for an object stored
// without a Cache-Control.
const defaultObjectCacheControl = "private, no-cache"

// sseCKeyRefusal is AWS's answer to a GET or HEAD of an SSE-C object with a
// key of another MD5 than the one the object was written under: 403, from
// the row alone (RP14-11, 094), before any backend call.
const sseCKeyRefusal = "The provided SSE-C key does not match the key the object was encrypted with."

// ssecKeyMismatch reports whether r carries a complete SSE-C key whose MD5
// differs from the one the row recorded. A row with none (written before
// 094), or a request whose headers do not parse (the opener's 400), is
// never a mismatch here.
func ssecKeyMismatch(r *http.Request, recorded string) bool {
	if recorded == "" {
		return false
	}
	key, err := crypto.ParseSSECHeaders(r)
	if err != nil || key == nil {
		return false
	}
	defer clear(key)
	return subtle.ConstantTimeCompare([]byte(crypto.SSECKeyMD5(key)), []byte(recorded)) != 1
}

// sseCRefusal is AWS's answer to a GET or HEAD of an SSE-C object that
// does not carry the customer's key (R8-14).
const sseCRefusal = "The object was stored using a form of Server Side Encryption. The correct parameters must be provided to retrieve the object."

// write sets the object's headers on w for mode (rng is the range of a
// headersRange). It writes no status line.
func (h objectHeaders) write(w http.ResponseWriter, mode headerMode, rng *httpRange) {
	hd := w.Header()
	if h.ETag != "" {
		hd.Set("ETag", fmt.Sprintf(`"%s"`, normalizeETag(h.ETag)))
	}
	lm := h.LastModified
	if lm.IsZero() {
		lm = time.Now()
	}
	hd.Set("Last-Modified", lm.UTC().Format(http.TimeFormat))
	cc := h.CacheControl
	if cc == "" {
		cc = defaultObjectCacheControl
	}
	if mode == headersNotModified {
		setEchoHeaders(hd, cc, h.Expires, "")
		return
	}
	hd.Set("Content-Type", h.ContentType)
	switch mode {
	case headersRange:
		hd.Set("Content-Length", strconv.FormatInt(rng.length, 10))
		hd.Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", rng.start, rng.end, h.Size))
	case headersHead:
		if h.Size >= 0 {
			hd.Set("Content-Length", strconv.FormatInt(h.Size, 10))
		}
	default:
		// A whole GET of an empty object writes no body: net/http says 0.
		if h.Size > 0 {
			hd.Set("Content-Length", strconv.FormatInt(h.Size, 10))
		}
	}
	hd.Set("Accept-Ranges", "bytes")
	if h.StorageClass != "" {
		hd.Set("x-amz-storage-class", h.StorageClass)
	}
	if h.VersionID != "" {
		hd.Set("x-amz-version-id", h.VersionID)
	}
	setS3MetadataHeaders(w, h.Metadata)
	if h.TagCount > 0 {
		hd.Set("x-amz-tagging-count", strconv.Itoa(h.TagCount))
	}
	if cd := sanitizeContentDisposition(h.Disposition); cd != "" {
		hd.Set("Content-Disposition", cd)
	}
	if h.Encoding != "" {
		hd.Set("Content-Encoding", h.Encoding)
	}
	if h.Language != "" {
		hd.Set("Content-Language", h.Language)
	}
	setEchoHeaders(hd, cc, h.Expires, h.WebsiteRedirect)
	if h.LockMode != "" && !h.LockRetainUntil.IsZero() {
		hd.Set("x-amz-object-lock-mode", h.LockMode)
		hd.Set("x-amz-object-lock-retain-until-date", h.LockRetainUntil.UTC().Format(time.RFC3339))
	}
	if h.LegalHold != "" {
		hd.Set("x-amz-object-lock-legal-hold", h.LegalHold)
	}
	switch h.SSE {
	case "":
	case "ssec":
		hd.Set("x-amz-server-side-encryption-customer-algorithm", "AES256")
		if h.SSECKeyMD5 != "" {
			hd.Set("x-amz-server-side-encryption-customer-key-MD5", h.SSECKeyMD5)
		}
	default:
		hd.Set("x-amz-server-side-encryption", h.SSE)
	}
}

// sseOf is the SSE value of an object recorded with encAlgo.
func sseOf(encAlgo string, ssec bool) string {
	switch {
	case encAlgo == "":
		return ""
	case ssec:
		return "ssec"
	}
	return "AES256"
}
