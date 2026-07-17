package auth

import (
	"crypto/hmac"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
)

// ErrSignatureMismatch is returned when a request's SigV4 signature does not
// verify against the stored secret. The API layer maps it to the S3 error
// code SignatureDoesNotMatch (403).
var ErrSignatureMismatch = errors.New("request signature does not match")

// ErrRequestTimeSkewed is returned when X-Amz-Date is outside the allowed
// clock skew. Maps to the S3 error code RequestTimeTooSkewed (403).
var ErrRequestTimeSkewed = errors.New("request time too skewed")

const unsignedPayload = "UNSIGNED-PAYLOAD"

// sigV4Enforced reports whether full signature verification is required.
// SIGV4_ENFORCE=false is the emergency fallback to pre-verification
// behavior (access-key existence only) if a client canonicalization bug
// surfaces in production. Default is enforced.
func sigV4Enforced() bool {
	return os.Getenv("SIGV4_ENFORCE") != "false"
}

// sigV4Params is the parsed content of an AWS4-HMAC-SHA256 Authorization header.
type sigV4Params struct {
	AccessKey     string
	Date          string // YYYYMMDD from the credential scope
	Region        string
	Service       string
	SignedHeaders string // lowercase, ';'-joined, as sent by the client
	Signature     string // hex
}

// parseSigV4AuthHeader parses
//
//	AWS4-HMAC-SHA256 Credential=AK/date/region/service/aws4_request, SignedHeaders=a;b, Signature=hex
//
// Components may be separated by "," or ", " and appear in any order.
func parseSigV4AuthHeader(h string) (*sigV4Params, error) {
	rest, ok := strings.CutPrefix(h, algorithm)
	if !ok {
		return nil, fmt.Errorf("not a SigV4 authorization header")
	}

	p := &sigV4Params{}
	for _, part := range strings.Split(rest, ",") {
		part = strings.TrimSpace(part)
		switch {
		case strings.HasPrefix(part, "Credential="):
			cred := strings.TrimPrefix(part, "Credential=")
			cp := strings.Split(cred, "/")
			if len(cp) != 5 || cp[4] != aws4Request {
				return nil, fmt.Errorf("malformed credential scope %q", cred)
			}
			p.AccessKey, p.Date, p.Region, p.Service = cp[0], cp[1], cp[2], cp[3]
		case strings.HasPrefix(part, "SignedHeaders="):
			p.SignedHeaders = strings.TrimPrefix(part, "SignedHeaders=")
		case strings.HasPrefix(part, "Signature="):
			p.Signature = strings.TrimPrefix(part, "Signature=")
		}
	}
	if p.AccessKey == "" || p.SignedHeaders == "" || p.Signature == "" {
		return nil, fmt.Errorf("authorization header missing required components")
	}
	return p, nil
}

// verifySigV4 recomputes the request signature from the signed headers and
// the stored secret, and compares it in constant time. The credential
// scope's date/region/service are used verbatim so any region string a
// client signs with is accepted.
func (a *Auth) verifySigV4(r *http.Request, p *sigV4Params, secretKey string) error {
	amzDate := r.Header.Get("X-Amz-Date")
	if amzDate == "" {
		return fmt.Errorf("%w: missing X-Amz-Date header", ErrSignatureMismatch)
	}
	if err := a.validateTimestamp(amzDate); err != nil {
		return fmt.Errorf("%w: %s", ErrRequestTimeSkewed, err)
	}
	if !strings.HasPrefix(amzDate, p.Date) {
		return fmt.Errorf("%w: credential scope date %s does not match request date %s",
			ErrSignatureMismatch, p.Date, amzDate)
	}

	canonical := canonicalRequestV4(r, p.SignedHeaders)
	scope := strings.Join([]string{p.Date, p.Region, p.Service, aws4Request}, "/")
	stringToSign := a.createStringToSign(amzDate, scope, canonical)
	signingKey := a.deriveSigningKey(secretKey, p.Date, p.Region, p.Service)
	expected := hex.EncodeToString(hmacSHA256(signingKey, []byte(stringToSign)))

	if !hmac.Equal([]byte(expected), []byte(p.Signature)) {
		return fmt.Errorf("%w", ErrSignatureMismatch)
	}
	return nil
}

func canonicalRequestV4(r *http.Request, signedHeaders string) string {
	// The payload hash the client declared is used verbatim — including the
	// UNSIGNED-PAYLOAD and STREAMING-AWS4-HMAC-SHA256-PAYLOAD markers. The
	// seed signature covers the declaration, not the body bytes.
	payloadHash := r.Header.Get("X-Amz-Content-Sha256")
	if payloadHash == "" {
		payloadHash = unsignedPayload
	}

	names := strings.Split(strings.ToLower(signedHeaders), ";")
	sort.Strings(names)
	var hdrs strings.Builder
	for _, name := range names {
		hdrs.WriteString(name)
		hdrs.WriteByte(':')
		hdrs.WriteString(canonicalHeaderValueV4(r, name))
		hdrs.WriteByte('\n')
	}

	return strings.Join([]string{
		r.Method,
		canonicalURIV4(r.URL.Path),
		canonicalQueryV4(r.URL.Query()),
		hdrs.String(),
		signedHeaders,
		payloadHash,
	}, "\n")
}

// canonicalHeaderValueV4 returns the AWS-normalized value for one signed
// header. Host, Content-Length and Transfer-Encoding are special-cased
// because Go's http server promotes them out of r.Header into struct fields.
func canonicalHeaderValueV4(r *http.Request, name string) string {
	switch name {
	case "host":
		return trimAWSSpaces(r.Host)
	case "content-length":
		if v := r.Header.Get("Content-Length"); v != "" {
			return trimAWSSpaces(v)
		}
		if r.ContentLength >= 0 {
			return strconv.FormatInt(r.ContentLength, 10)
		}
		return ""
	case "transfer-encoding":
		if len(r.TransferEncoding) > 0 {
			return strings.Join(r.TransferEncoding, ",")
		}
		return trimAWSSpaces(r.Header.Get("Transfer-Encoding"))
	}

	vals := r.Header.Values(name)
	trimmed := make([]string, len(vals))
	for i, v := range vals {
		trimmed[i] = trimAWSSpaces(v)
	}
	return strings.Join(trimmed, ",")
}

// trimAWSSpaces implements SigV4 "Trimall": strip leading/trailing
// whitespace and collapse internal runs to a single space.
func trimAWSSpaces(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// canonicalURIV4 re-encodes the decoded request path with AWS URI encoding
// rules (S3 single-encoding: each segment percent-encoded once, slashes
// preserved). This reproduces what spec-compliant clients sign regardless
// of how Go normalized the wire encoding.
func canonicalURIV4(path string) string {
	if path == "" {
		return "/"
	}
	segs := strings.Split(path, "/")
	for i, s := range segs {
		segs[i] = awsURIEncode(s)
	}
	return strings.Join(segs, "/")
}

// canonicalQueryV4 builds the canonical query string: keys sorted, repeated
// values sorted, AWS percent-encoding (space is %20 — never '+').
func canonicalQueryV4(q url.Values) string {
	keys := make([]string, 0, len(q))
	for k := range q {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var pairs []string
	for _, k := range keys {
		ek := awsURIEncode(k)
		vs := append([]string(nil), q[k]...)
		sort.Strings(vs)
		for _, v := range vs {
			pairs = append(pairs, ek+"="+awsURIEncode(v))
		}
	}
	return strings.Join(pairs, "&")
}

// awsURIEncode percent-encodes every byte except the RFC 3986 unreserved
// set (A-Za-z0-9, '-', '_', '.', '~'), with uppercase hex.
func awsURIEncode(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') ||
			(c >= '0' && c <= '9') || c == '-' || c == '_' || c == '.' || c == '~' {
			b.WriteByte(c)
		} else {
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}
