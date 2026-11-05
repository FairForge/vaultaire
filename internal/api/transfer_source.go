package api

// WP-XFER-1 (Stage 1a Prompt 59 PR A): the transfer's SOURCE — an
// S3-compatible endpoint a tenant names, read with a key it made. The
// endpoint is the tenant's word, so it must never reach the box or a private
// network (Step 0 answer 2, docs/reviews/WP-STRIPE-11.md):
//
//   - at create: https only, a host and an optional port, no userinfo, no
//     path beyond "/", no query, no fragment (validTransferEndpoint);
//   - at DIAL time, every connection: the dialer's Control hook sees the
//     address actually being connected and refuses a private or special one
//     (isPrivateOrSpecialIP, the webhooks' rule) — a DNS answer that changes
//     between the create-time check and the dial cannot get round it;
//   - redirects refused, the environment's proxy never used, TLS verified.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go"
)

// transferAllowPrivateTargets is the tests' escape (webhookAllowPrivateTargets'
// twin): an httptest source listens on 127.0.0.1. Never set in production.
var transferAllowPrivateTargets atomic.Bool

// errTransferPrivateTarget: the dial was refused — the address is the box's
// or a private network's.
var errTransferPrivateTarget = errors.New("transfer source: the address is private or special")

// errTransferWrongRange: the source answered a ranged GET with other bytes
// than the span asked — a 200 with the whole object, another span, or an
// object of another size than its listing said (RP98-4). The reader would
// take the first n bytes of whatever came back as [off, off+n): wrong bytes
// stored as "copied" wherever no MD5 can catch it (a multipart copy, an ETag
// that is not an MD5). Such an answer fails the item, never a spliced copy.
var errTransferWrongRange = errors.New("transfer source: the answer is not the byte range asked")

// errTransferCut: the copy's reader was stopped by the run's end (the
// context, or a token bucket that will not wait past the deadline) — not the
// source's failure and not the destination's (RP98-1).
var errTransferCut = errors.New("transfer: the copy was cut at the run's end")

// validTransferEndpoint checks a source endpoint's shape at create, and a
// literal IP at once; the dialer checks every resolved address again.
func validTransferEndpoint(raw string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return "", fmt.Errorf("the endpoint is not a URL: %w", err)
	}
	switch {
	case u.Scheme != "https" && !(transferAllowPrivateTargets.Load() && u.Scheme == "http"):
		return "", errors.New("the endpoint must be https://")
	case u.Host == "" || u.Hostname() == "":
		return "", errors.New("the endpoint names no host")
	case u.User != nil:
		return "", errors.New("the endpoint may not carry credentials")
	case u.RawQuery != "" || u.ForceQuery || u.Fragment != "":
		return "", errors.New("the endpoint may not carry a query or a fragment")
	case u.Path != "" && u.Path != "/":
		return "", errors.New("the endpoint is a host, not a path (name the bucket separately)")
	}
	if ip := net.ParseIP(u.Hostname()); ip != nil && !transferAllowPrivateTargets.Load() && isPrivateOrSpecialIP(ip) {
		return "", errTransferPrivateTarget
	}
	return u.Scheme + "://" + u.Host, nil
}

// transferDialControl refuses a connection to a private or special address
// — the address the socket is about to connect to, after DNS.
func transferDialControl(_, address string, _ syscall.RawConn) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return fmt.Errorf("%w: %s", errTransferPrivateTarget, address)
	}
	ip := net.ParseIP(host)
	if ip == nil || (!transferAllowPrivateTargets.Load() && isPrivateOrSpecialIP(ip)) {
		return fmt.Errorf("%w: %s", errTransferPrivateTarget, host)
	}
	return nil
}

// transferHTTPClient is the source's client: the dial-time guard, no
// proxy, no redirects.
func transferHTTPClient() *http.Client {
	dialer := &net.Dialer{Timeout: 15 * time.Second, KeepAlive: 30 * time.Second, Control: transferDialControl}
	tr := &http.Transport{
		Proxy:                 nil,
		DialContext:           dialer.DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          32,
		MaxIdleConnsPerHost:   8,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   15 * time.Second,
		ResponseHeaderTimeout: 60 * time.Second,
		ExpectContinueTimeout: time.Second,
	}
	return &http.Client{Transport: tr, CheckRedirect: func(*http.Request, []*http.Request) error {
		return errors.New("transfer source: a redirect is refused")
	}}
}

// transferSource is one job's source.
type transferSource struct {
	client *s3.Client
	bucket string
	prefix string
}

func newTransferSource(endpoint, region, bucket, prefix, accessKey, secret string) *transferSource {
	if region == "" {
		region = "us-east-1"
	}
	c := s3.New(s3.Options{
		Region:           region,
		BaseEndpoint:     aws.String(endpoint),
		UsePathStyle:     true,
		Credentials:      credentials.NewStaticCredentialsProvider(accessKey, secret, ""),
		HTTPClient:       transferHTTPClient(),
		RetryMaxAttempts: 2,
	})
	return &transferSource{client: c, bucket: bucket, prefix: prefix}
}

// transferListed is one object of a source listing.
type transferListed struct {
	Key  string
	ETag string // unquoted, lower-case
	Size int64
}

// list is one page from after (exclusive): the objects, the last key the
// source listed (a folder marker included — the cursor must move past the
// markers too, RP98-6) and whether more remain.
func (src *transferSource) list(ctx context.Context, after string, max int32) ([]transferListed, string, bool, error) {
	in := &s3.ListObjectsV2Input{Bucket: aws.String(src.bucket), MaxKeys: aws.Int32(max)}
	if src.prefix != "" {
		in.Prefix = aws.String(src.prefix)
	}
	if after != "" {
		in.StartAfter = aws.String(after)
	}
	out, err := src.client.ListObjectsV2(ctx, in)
	if err != nil {
		return nil, "", false, err
	}
	items := make([]transferListed, 0, len(out.Contents))
	last := ""
	for _, o := range out.Contents {
		k := aws.ToString(o.Key)
		last = k
		if strings.HasSuffix(k, "/") && aws.ToInt64(o.Size) == 0 {
			continue // a "folder" marker: no object to copy
		}
		items = append(items, transferListed{Key: k, ETag: normETag(aws.ToString(o.ETag)), Size: aws.ToInt64(o.Size)})
	}
	return items, last, aws.ToBool(out.IsTruncated), nil
}

// transferObjectAttrs are the attributes a copy carries to the PUT.
type transferObjectAttrs struct {
	header http.Header
}

// rangeGet reads [off, off+n) of key pinned to etag (If-Match: a source
// object changed under the copy is a 412, never spliced bytes). The answer
// must BE that span of an object of total bytes (RP98-4): its Content-Range
// is checked, so a source — or a proxy in front of it — that ignores Range
// fails the item instead of handing the copy other bytes. total < 0 skips the
// total's check (an empty object's one-byte probe).
func (src *transferSource) rangeGet(ctx context.Context, key, etag string, off, n, total int64) (io.ReadCloser, *s3.GetObjectOutput, error) {
	in := &s3.GetObjectInput{Bucket: aws.String(src.bucket), Key: aws.String(key),
		Range: aws.String(fmt.Sprintf("bytes=%d-%d", off, off+n-1))}
	if etag != "" {
		in.IfMatch = aws.String(`"` + etag + `"`)
	}
	out, err := src.client.GetObject(ctx, in)
	if err != nil {
		return nil, nil, err
	}
	if err := rangeAnswered(aws.ToString(out.ContentRange), off, n, total); err != nil {
		_ = out.Body.Close()
		return nil, nil, err
	}
	return out.Body, out, nil
}

// rangeAnswered checks a ranged GET's Content-Range against the span asked:
// "bytes <off>-<off+n-1>/<total>". No header at all is a 200 with the whole
// object.
func rangeAnswered(contentRange string, off, n, total int64) error {
	var a, b, size int64
	spec := strings.TrimSpace(contentRange)
	if spec == "" {
		return fmt.Errorf("%w: no Content-Range (the source ignored Range)", errTransferWrongRange)
	}
	if _, err := fmt.Sscanf(spec, "bytes %d-%d/%d", &a, &b, &size); err != nil {
		return fmt.Errorf("%w: Content-Range %q", errTransferWrongRange, truncateForError(spec, 64))
	}
	switch {
	case a != off || b != off+n-1:
		return fmt.Errorf("%w: asked %d-%d, answered %d-%d", errTransferWrongRange, off, off+n-1, a, b)
	case total >= 0 && size != total:
		return fmt.Errorf("%w: the object is %d bytes, its listing said %d", errTransferWrongRange, size, total)
	}
	return nil
}

func truncateForError(s string, max int) string {
	if len(s) > max {
		return s[:max]
	}
	return s
}

// attrsOf are the object attributes of a GET's answer, as PUT headers.
func attrsOf(out *s3.GetObjectOutput) transferObjectAttrs {
	h := http.Header{}
	set := func(k string, v *string) {
		if s := aws.ToString(v); s != "" {
			h.Set(k, s)
		}
	}
	set("Content-Type", out.ContentType)
	set("Cache-Control", out.CacheControl)
	set("Content-Disposition", out.ContentDisposition)
	set("Content-Encoding", out.ContentEncoding)
	set("Content-Language", out.ContentLanguage)
	if out.ExpiresString != nil {
		set("Expires", out.ExpiresString)
	}
	for k, v := range out.Metadata {
		h.Set("x-amz-meta-"+strings.ToLower(k), v)
	}
	return transferObjectAttrs{header: h}
}

func normETag(e string) string { return strings.ToLower(strings.Trim(e, `"`)) }

// transferSourceError classifies a source failure: denied (a 401/403 —
// the job pauses), gone (404/412 — the item fails), or transient.
type transferSourceClass int

const (
	sourceTransient transferSourceClass = iota
	sourceDenied
	sourceGone
	sourceUnreachable
	sourceWrongRange // the answer was not the span asked: the item fails
	sourceCut        // the run's end cut the copy: nobody's failure
)

func classifySourceError(err error) (transferSourceClass, string) {
	var api smithy.APIError
	var resp interface{ HTTPStatusCode() int }
	status := 0
	if errors.As(err, &resp) {
		status = resp.HTTPStatusCode()
	}
	code := ""
	if errors.As(err, &api) {
		code = api.ErrorCode()
	}
	switch {
	case errors.Is(err, errTransferWrongRange):
		return sourceWrongRange, err.Error()
	case errors.Is(err, errTransferCut), errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
		return sourceCut, "cut at the run's end"
	case errors.Is(err, errTransferPrivateTarget):
		return sourceUnreachable, "the source's address is private or special"
	case status == http.StatusUnauthorized || status == http.StatusForbidden ||
		code == "AccessDenied" || code == "InvalidAccessKeyId" || code == "SignatureDoesNotMatch":
		if code == "" {
			code = http.StatusText(status)
		}
		return sourceDenied, code
	case status == http.StatusNotFound || status == http.StatusPreconditionFailed || code == "NoSuchKey" || code == "NoSuchBucket":
		if code == "" {
			code = http.StatusText(status)
		}
		return sourceGone, code
	case status == 0 && code == "":
		var dnsErr *net.DNSError
		var opErr *net.OpError
		if errors.As(err, &dnsErr) || errors.As(err, &opErr) {
			return sourceUnreachable, "the source could not be reached"
		}
	}
	if code == "" && status != 0 {
		code = fmt.Sprintf("HTTP %d", status)
	}
	if code == "" {
		code = "the source did not answer"
	}
	return sourceTransient, code
}
