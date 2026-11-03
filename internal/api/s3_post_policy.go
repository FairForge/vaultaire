package api

// Browser uploads through S3 presigned POST policies (WP-POST-1, Stage 1a
// Prompt 57; docs/API.md § Browser uploads).
//
// A POST to /{bucket} with a multipart/form-data body and no Authorization
// header is a policy upload: the fields come first, then `file` (S3's order
// rule — every field after it is ignored). The fields are read here, up to
// the file part, and handed to the one verifier's fourth entrance
// (auth.ValidatePostPolicy: the one credential lookup, the verifier's own
// key derivation, every condition held against the form). The file part is
// then the BODY of a request handleS3Request carries on with — the same
// checks a PUT passes after authentication (expiry, permissions, scope,
// agents, Private, system buckets, the key's length, suspension) and the
// PUT's own write path (handlePutObject: placement, the quota reservation,
// the ETag by MD5 stream, the head row), never buffered (Architecture
// Decision 4).
//
// The file's size is exact before a byte of it is read (D-POST-2): `file`
// is the last part, so its bytes are the request's Content-Length less what
// precedes them and less the closing delimiter. It is held to the policy's
// content-length-range before anything is reserved or read, the PUT path
// reserves it like a PUT's declared length, and the body reader proves it
// as it streams — the closing delimiter must follow exactly, or the read
// fails and nothing commits.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/textproto"
	"net/url"
	"strconv"
	"strings"

	"github.com/FairForge/vaultaire/internal/auth"
	"github.com/FairForge/vaultaire/internal/clientip"
)

// maxPostPreData bounds what precedes the file part (AWS: the form's fields
// are at most 20 KB).
const maxPostPreData = 20 << 10

// postTailMax bounds the closing delimiter: "\r\n--" + a boundary of at
// most 70 bytes (RFC 2046) + "--\r\n".
const postTailMax = 80

// postFormBuffer is the reader's look-ahead (the fields fit in it).
const postFormBuffer = 64 << 10

var (
	errPostMalformed   = errors.New("the body is not well-formed multipart/form-data with the file last")
	errPostNoFile      = errors.New("POST requires exactly one file upload per request")
	errPostPreDataSize = fmt.Errorf("the form's fields exceed %d bytes", maxPostPreData)
)

// isPostPolicyRequest: a POST of multipart/form-data to a bucket's path,
// with no S3 credential of another shape (a header or a presigned query).
// Decided from the method, the content type and the path — never by
// reading the body.
func isPostPolicyRequest(r *http.Request) bool {
	if r.Method != http.MethodPost || r.Header.Get("Authorization") != "" || isPresignedRequest(r) {
		return false
	}
	return postPolicyShape(r)
}

// postPolicyShape is the request's shape alone (the credential gate's half,
// signedS3Request): POST, multipart/form-data, one path segment.
func postPolicyShape(r *http.Request) bool {
	if r.Method != http.MethodPost {
		return false
	}
	mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mt != "multipart/form-data" {
		return false
	}
	return postPolicyBucket(r.URL.Path) != ""
}

// postPolicyBucket is the bucket of a /{bucket} (or /{bucket}/) path, ""
// for any other path.
func postPolicyBucket(p string) string {
	p = strings.TrimSuffix(strings.TrimPrefix(p, "/"), "/")
	if p == "" || strings.Contains(p, "/") {
		return ""
	}
	return p
}

// postForm is a read form: its fields (lower-cased names) and its file.
type postForm struct {
	fields   map[string]string
	filename string
	size     int64
	file     io.Reader
}

// byteCounter counts what bufio took from the body, and refuses past a
// limit while the fields are read.
type byteCounter struct {
	r     io.Reader
	n     int64
	limit int64 // 0 = none
}

func (c *byteCounter) Read(p []byte) (int, error) {
	if c.limit > 0 && c.n >= c.limit {
		return 0, errPostPreDataSize
	}
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

// readPostForm reads the fields of r's form up to its file part and
// returns the file as an exact-length reader.
func readPostForm(r *http.Request) (*postForm, error) {
	_, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	boundary := params["boundary"]
	if err != nil || boundary == "" || len(boundary) > 70 {
		return nil, errPostMalformed
	}
	counter := &byteCounter{r: r.Body, limit: maxPostPreData + postFormBuffer}
	br := bufio.NewReaderSize(counter, postFormBuffer)
	consumed := func() int64 { return counter.n - int64(br.Buffered()) }
	delim := "--" + boundary

	// The first delimiter line (a preamble before it is skipped).
	for {
		line, err := br.ReadSlice('\n')
		if err != nil {
			return nil, postReadErr(err)
		}
		if strings.TrimRight(string(line), "\r\n") == delim {
			break
		}
		if consumed() > maxPostPreData {
			return nil, errPostPreDataSize
		}
	}
	tp := textproto.NewReader(br)
	form := &postForm{fields: map[string]string{}}
	for {
		hdr, err := tp.ReadMIMEHeader()
		if err != nil {
			return nil, postReadErr(err)
		}
		_, dp, err := mime.ParseMediaType(hdr.Get("Content-Disposition"))
		if err != nil {
			return nil, errPostMalformed
		}
		name := strings.ToLower(dp["name"])
		if name == "" {
			return nil, errPostMalformed
		}
		if name == "file" {
			tail := "\r\n" + delim + "--\r\n"
			form.filename = dp["filename"]
			form.size = r.ContentLength - consumed() - int64(len(tail))
			if form.size < 0 {
				return nil, errPostMalformed
			}
			counter.limit = 0
			form.file = &postFileReader{br: br, left: form.size, tail: []byte(tail)}
			return form, nil
		}
		value, last, err := readFieldValue(br, "\r\n"+delim, consumed)
		if err != nil {
			return nil, err
		}
		if _, dup := form.fields[name]; dup {
			return nil, fmt.Errorf("%w: the field %q is given twice", errPostMalformed, name)
		}
		form.fields[name] = value
		if last {
			return nil, errPostNoFile
		}
	}
}

func postReadErr(err error) error {
	if errors.Is(err, errPostPreDataSize) {
		return errPostPreDataSize
	}
	return errPostMalformed
}

// readFieldValue reads a field's value up to "\r\n--boundary", then the two
// bytes after it: "\r\n" (another part) or "--" (the end — last).
func readFieldValue(br *bufio.Reader, delim string, consumed func() int64) (string, bool, error) {
	var buf bytes.Buffer
	for {
		b, err := br.ReadByte()
		if err != nil {
			return "", false, postReadErr(err)
		}
		buf.WriteByte(b)
		if consumed() > maxPostPreData {
			return "", false, errPostPreDataSize
		}
		if b == delim[len(delim)-1] && bytes.HasSuffix(buf.Bytes(), []byte(delim)) {
			break
		}
	}
	value := buf.String()[:buf.Len()-len(delim)]
	var next [2]byte
	if _, err := io.ReadFull(br, next[:]); err != nil {
		return "", false, errPostMalformed
	}
	switch string(next[:]) {
	case "\r\n":
		return value, false, nil
	case "--":
		return value, true, nil
	}
	return "", false, errPostMalformed
}

// postFileReader yields exactly the file part's left bytes, and proves the
// closing delimiter follows them (and nothing after it) in the same Read
// that delivers the last byte — so a consumer that stops at the declared
// length still sees a malformed tail as an error before it commits.
type postFileReader struct {
	br   *bufio.Reader
	left int64
	tail []byte
	done bool
	err  error
}

func (f *postFileReader) Read(p []byte) (int, error) {
	if f.err != nil {
		return 0, f.err
	}
	if f.done {
		return 0, io.EOF
	}
	if int64(len(p)) > f.left {
		p = p[:f.left]
	}
	n, err := f.br.Read(p)
	f.left -= int64(n)
	if err != nil && !errors.Is(err, io.EOF) {
		f.err = err
		return n, err
	}
	if f.left > 0 {
		if errors.Is(err, io.EOF) {
			f.err = io.ErrUnexpectedEOF
			return n, f.err
		}
		return n, nil
	}
	var buf [postTailMax]byte // the delimiter: CRLF, --, a boundary of ≤ 70 bytes, --, CRLF
	got := buf[:len(f.tail)]
	if _, terr := io.ReadFull(f.br, got); terr != nil || !bytes.Equal(got, f.tail) {
		f.err = fmt.Errorf("%w: the file part does not end at the closing delimiter", errPostMalformed)
		return n, f.err
	}
	if extra, _ := f.br.Peek(1); len(extra) > 0 {
		f.err = fmt.Errorf("%w: bytes after the closing delimiter", errPostMalformed)
		return n, f.err
	}
	f.done = true
	return n, nil
}

// postPolicyUpload is what the dispatch needs of an admitted form.
type postPolicyUpload struct {
	status int // 200, 201 or 204 (success_action_status)
	meta   http.Header
}

type postPolicyKey struct{}

func postPolicyOf(ctx context.Context) *postPolicyUpload {
	p, _ := ctx.Value(postPolicyKey{}).(*postPolicyUpload)
	return p
}

// postPolicyFieldHeaders are the form fields that become the object's
// headers on the PUT path (every one a policy must name).
var postPolicyFieldHeaders = map[string]string{
	"content-type":                 "Content-Type",
	"cache-control":                "Cache-Control",
	"content-disposition":          "Content-Disposition",
	"content-encoding":             "Content-Encoding",
	"content-language":             "Content-Language",
	"expires":                      "Expires",
	"x-amz-storage-class":          "X-Amz-Storage-Class",
	"x-amz-server-side-encryption": "X-Amz-Server-Side-Encryption",
	"x-amz-server-side-encryption-customer-algorithm": "X-Amz-Server-Side-Encryption-Customer-Algorithm",
	"x-amz-server-side-encryption-customer-key":       "X-Amz-Server-Side-Encryption-Customer-Key",
	"x-amz-server-side-encryption-customer-key-md5":   "X-Amz-Server-Side-Encryption-Customer-Key-Md5",
}

// postPolicyPassHeaders are the outer request's headers kept on the inner
// one: the client's address as the proxies vouch for it (the key's IP
// allow-list, the access log), and what the access log records. Nothing
// else of the outer request reaches the write path — every object header
// is a form field the policy named.
var postPolicyPassHeaders = []string{"User-Agent", "X-Forwarded-For", "Cf-Connecting-Ip", "Origin", "Referer", "X-Request-Id"}

// postRefusal is an answer before the write: an S3 error.
type postRefusal struct {
	code     string
	status   int // 0 = the code's
	message  string
	reason   string // an auth failure's metric reason ("" = not one)
	keyKnown bool
}

func (p *postRefusal) write(w http.ResponseWriter, r *http.Request) {
	var opts []ErrorOption
	if p.message != "" {
		opts = append(opts, WithMessage(p.message))
	}
	if p.status != 0 {
		opts = append(opts, WithStatus(p.status))
	}
	WriteS3ErrorWithContext(w, p.code, r.URL.Path, generateRequestID(), opts...)
}

// authorizePostPolicy reads and verifies a policy form; on success it
// returns the key's tenant and scope and the inner request — the file part
// as a POST /{bucket}/{key} of exactly its size, with the form's object
// headers — for handleS3Request to carry on with.
func (s *Server) authorizePostPolicy(r *http.Request) (string, *auth.KeyScope, *http.Request, *postRefusal) {
	bucket := postPolicyBucket(r.URL.Path)
	if r.ContentLength < 0 {
		return "", nil, nil, &postRefusal{code: ErrMissingContentLength}
	}
	form, err := readPostForm(r)
	switch {
	case errors.Is(err, errPostPreDataSize):
		return "", nil, nil, &postRefusal{code: ErrInvalidArgument, message: err.Error(), status: http.StatusBadRequest}
	case errors.Is(err, errPostNoFile):
		return "", nil, nil, &postRefusal{code: ErrInvalidArgument, message: err.Error()}
	case err != nil:
		return "", nil, nil, &postRefusal{code: ErrMalformedPOSTRequest, message: err.Error()}
	}
	fields := form.fields
	fields["bucket"] = bucket
	tenantID, scope, policy, err := s.s3Auth().ValidatePostPolicy(r.Context(), fields, clientip.FromRequest(r))
	if err != nil {
		return "", nil, nil, postAuthRefusal(err)
	}
	if fields["success_action_redirect"] != "" || fields["redirect"] != "" {
		return "", nil, nil, &postRefusal{code: ErrInvalidArgument,
			message: "success_action_redirect is not supported: answer with success_action_status (200, 201 or 204) and redirect in the page"}
	}
	if acl := fields["acl"]; acl != "" && acl != "private" {
		return "", nil, nil, &postRefusal{code: ErrInvalidArgument,
			message: "only acl=private: a bucket's mode decides who reads it (Public, Standard, Private)"}
	}
	if fields["tagging"] != "" {
		return "", nil, nil, &postRefusal{code: ErrNotImplemented, message: "tagging in a POST form is not supported"}
	}
	key := fields["key"]
	if key == "" {
		return "", nil, nil, &postRefusal{code: ErrInvalidArgument, message: "Bucket POST must contain a field named 'key'."}
	}
	if strings.Contains(key, "${filename}") {
		name := form.filename
		if name == "" || name == "." || strings.ContainsAny(name, `/\`) || strings.Contains(name, "..") {
			return "", nil, nil, &postRefusal{code: ErrInvalidArgument,
				message: "the file's name must be a base name (no /, \\ or ..) to replace ${filename}"}
		}
		key = strings.ReplaceAll(key, "${filename}", name)
	}
	if min, max, ok := policy.SizeRange(); ok {
		if form.size > max {
			return "", nil, nil, &postRefusal{code: ErrEntityTooLarge, status: http.StatusBadRequest,
				message: fmt.Sprintf("Your proposed upload exceeds the maximum allowed size: %d bytes, the policy's content-length-range is %d–%d.", form.size, min, max)}
		}
		if form.size < min {
			return "", nil, nil, &postRefusal{code: ErrEntityTooSmall,
				message: fmt.Sprintf("Your proposed upload is smaller than the minimum allowed size: %d bytes, the policy's content-length-range is %d–%d.", form.size, min, max)}
		}
	}

	h := http.Header{}
	for _, k := range postPolicyPassHeaders {
		if v := r.Header.Values(k); len(v) > 0 {
			h[http.CanonicalHeaderKey(k)] = append([]string(nil), v...)
		}
	}
	meta := http.Header{}
	for name, value := range fields {
		if hk, ok := postPolicyFieldHeaders[name]; ok {
			h.Set(hk, value)
		} else if strings.HasPrefix(name, "x-amz-meta-") {
			h.Set(name, value)
			meta.Set(name, value)
		}
	}
	if h.Get("Content-Type") == "" {
		h.Set("Content-Type", "binary/octet-stream")
	}
	status := http.StatusNoContent
	switch fields["success_action_status"] {
	case "200":
		status = http.StatusOK
	case "201":
		status = http.StatusCreated
	}
	inner := r.Clone(context.WithValue(r.Context(), postPolicyKey{}, &postPolicyUpload{status: status, meta: meta}))
	inner.Header = h
	inner.URL = &url.URL{Path: "/" + bucket + "/" + key}
	inner.RequestURI = ""
	inner.ContentLength = form.size
	inner.Body = struct {
		io.Reader
		io.Closer
	}{form.file, r.Body}
	return tenantID, scope, inner, nil
}

// postAuthRefusal maps the verifier's refusal to the S3 answer and the
// auth-failure metric's reason.
func postAuthRefusal(err error) *postRefusal {
	switch {
	case errors.Is(err, auth.ErrPolicyFields):
		return &postRefusal{code: ErrInvalidArgument, message: err.Error()}
	case errors.Is(err, auth.ErrPolicyMalformed):
		return &postRefusal{code: ErrInvalidPolicyDocument, message: err.Error()}
	case errors.Is(err, auth.ErrPolicyExpired):
		return &postRefusal{code: ErrAccessDenied, message: "Invalid according to Policy: Policy expired."}
	case errors.Is(err, auth.ErrPolicyCondition):
		detail := strings.TrimPrefix(strings.TrimPrefix(err.Error(), auth.ErrPolicyCondition.Error()), ": ")
		return &postRefusal{code: ErrAccessDenied, message: "Invalid according to Policy: Policy Condition failed: " + detail}
	case errors.Is(err, auth.ErrIPDenied):
		return &postRefusal{code: ErrAccessDenied, message: "Access denied. This key is restricted by IP address.", reason: "ip_denied", keyKnown: true}
	case errors.Is(err, auth.ErrCredentialUnavailable):
		return &postRefusal{code: ErrInternalError}
	case errors.Is(err, auth.ErrAccessKeyRevoked):
		return &postRefusal{code: ErrInvalidAccessKeyId, reason: "revoked", keyKnown: true}
	case errors.Is(err, auth.ErrSignatureMismatch):
		reason, known := authFailureReason(err)
		return &postRefusal{code: ErrSignatureDoesNotMatch, reason: reason, keyKnown: known}
	}
	reason, known := authFailureReason(err)
	return &postRefusal{code: ErrAccessDenied, reason: reason, keyKnown: known}
}

// postResponseWriter turns the PUT path's success (200 + ETag) into the
// POST's answer — success_action_status, the Location header, the
// x-amz-meta-* fields echoed, and for 201 the PostResponse document; any
// other answer (a refusal) passes through untouched.
type postResponseWriter struct {
	http.ResponseWriter
	held, passed bool
}

func (p *postResponseWriter) WriteHeader(code int) {
	if p.held || p.passed {
		return
	}
	if code == http.StatusOK {
		p.held = true
		return
	}
	p.passed = true
	p.ResponseWriter.WriteHeader(code)
}

func (p *postResponseWriter) Write(b []byte) (int, error) {
	if !p.held && !p.passed {
		p.WriteHeader(http.StatusOK)
	}
	if p.held {
		return len(b), nil // the PUT's success body (none): the POST answers its own
	}
	return p.ResponseWriter.Write(b)
}

// PostResponse is the 201 document.
type PostResponse struct {
	XMLName  xml.Name `xml:"PostResponse"`
	Location string   `xml:"Location"`
	Bucket   string   `xml:"Bucket"`
	Key      string   `xml:"Key"`
	ETag     string   `xml:"ETag"`
}

// handlePostObject is a policy form's write: the PUT's (handlePutObject),
// answered as a POST.
func (s *Server) handlePostObject(w http.ResponseWriter, r *http.Request, req *S3Request) {
	up := postPolicyOf(r.Context())
	pw := &postResponseWriter{ResponseWriter: w}
	s.handlePutObject(pw, r, req)
	if pw.passed {
		return
	}
	scheme := "https"
	if r.TLS == nil && !strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https") && r.URL.Scheme != "https" {
		scheme = "http"
	}
	location := scheme + "://" + r.Host + "/" + url.PathEscape(req.Bucket) + "/" + escapeKeyPath(req.Object)
	h := w.Header()
	h.Set("Location", location)
	for k, v := range up.meta {
		h[k] = v
	}
	switch up.status {
	case http.StatusCreated:
		body, err := xml.Marshal(PostResponse{Location: location, Bucket: req.Bucket, Key: req.Object, ETag: h.Get("ETag")})
		if err != nil {
			WriteS3Error(w, ErrInternalError, r.URL.Path, generateRequestID())
			return
		}
		h.Set("Content-Type", "application/xml")
		h.Set("Content-Length", strconv.Itoa(len(xml.Header)+len(body)))
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, xml.Header)
		_, _ = w.Write(body)
	default:
		w.WriteHeader(up.status)
	}
}

// escapeKeyPath escapes each segment of a key for a URL path.
func escapeKeyPath(key string) string {
	parts := strings.Split(key, "/")
	for i, p := range parts {
		parts[i] = url.PathEscape(p)
	}
	return strings.Join(parts, "/")
}
