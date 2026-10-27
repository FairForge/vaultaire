package drivers

// The edge writer of the S3-class drivers (engine.EdgeWriter, WP-EDGE-1
// Part A): presigned PutObject / UploadPart links the Worker on
// s3.stored.cloud streams a client's body to, the small multipart
// bookkeeping calls the control plane makes itself, and (Part B) the
// presigned GetObject link the Worker streams an object back from. Implemented ONCE here
// over the aws-sdk-go-v2 client and attached to the fixed-bucket driver
// (idrive, idrive-<region>, wasabi — NewFixedBucketS3Driver) and to
// LyveDriver; each hands in its own bucket and key rule, so a presigned
// link addresses exactly the key the driver's Put would.
//
// Every link signs content-length and, when the client sent one,
// content-md5 (and content-type when set): the SDK's presign client puts
// them in X-Amz-SignedHeaders (Step 0.1, docs/reviews/WP-EDGE-1A.md), and
// both backends enforce them (Step 0.2). A presign is not a request (no
// bound of the request classes applies); Create / Complete / Abort are
// requests of the meta and complete classes (s3OpTimeouts) with the SDK's
// per-path attempts (sdkCallOptions).

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"

	"github.com/FairForge/vaultaire/internal/engine"
)

// edgePresignMaxTTL bounds a link's life: the control plane asks 15 min and
// the admit row expires 5 min later; nothing longer is ever signed.
const edgePresignMaxTTL = time.Hour

// s3EdgeTarget is one driver's bucket and key for an edge call.
type s3EdgeTarget struct {
	name   string // the driver's own name, for errors
	client *s3.Client
	bucket string
	key    string
}

func edgeTTL(p engine.EdgePresign) time.Duration {
	ttl := p.TTL
	if ttl <= 0 || ttl > edgePresignMaxTTL {
		ttl = edgePresignMaxTTL
	}
	return ttl
}

// signedHeaderNames reads X-Amz-SignedHeaders off a presigned URL: what
// the Worker must send exactly as signed (the api's checker refuses a link
// that does not name content-length).
func signedHeaderNames(presigned string) ([]string, error) {
	u, err := url.Parse(presigned)
	if err != nil {
		return nil, fmt.Errorf("parse presigned url: %w", err)
	}
	sh := u.Query().Get("X-Amz-SignedHeaders")
	if sh == "" {
		return nil, fmt.Errorf("presigned url names no signed headers")
	}
	return strings.Split(sh, ";"), nil
}

func (t s3EdgeTarget) presignPut(ctx context.Context, p engine.EdgePresign) (string, []string, error) {
	in := &s3.PutObjectInput{Bucket: aws.String(t.bucket), Key: aws.String(t.key), ContentLength: aws.Int64(p.ContentLength)}
	if p.ContentMD5 != "" {
		in.ContentMD5 = aws.String(p.ContentMD5)
	}
	if p.ContentType != "" {
		in.ContentType = aws.String(p.ContentType)
	}
	out, err := s3.NewPresignClient(t.client).PresignPutObject(ctx, in, s3.WithPresignExpires(edgeTTL(p)))
	if err != nil {
		return "", nil, fmt.Errorf("%s presign put %s: %w", t.name, t.key, err)
	}
	names, err := signedHeaderNames(out.URL)
	if err != nil {
		return "", nil, fmt.Errorf("%s presign put %s: %w", t.name, t.key, err)
	}
	return out.URL, names, nil
}

// presignGet is the read half (Part B): a GetObject link signing `range`
// (always — the control plane names the exact byte span, the whole object
// included, so the link is bound to it) and `if-match` when the control
// plane knows the backend's ETag (the row's digest, so a blob replaced in
// place — which no Vaultaire write does since WP-R2-1 Part C — answers 412
// instead of other bytes). A link signing neither is never made.
func (t s3EdgeTarget) presignGet(ctx context.Context, p engine.EdgePresign) (string, []string, error) {
	if p.Range == "" && p.IfMatch == "" {
		return "", nil, fmt.Errorf("%s presign get %s: neither a Range nor an If-Match to sign", t.name, t.key)
	}
	in := &s3.GetObjectInput{Bucket: aws.String(t.bucket), Key: aws.String(t.key)}
	if p.IfMatch != "" {
		in.IfMatch = aws.String(p.IfMatch)
	}
	if p.Range != "" {
		in.Range = aws.String(p.Range)
	}
	// Response-checksum validation off for the link: the SDK's default
	// (WhenSupported) puts X-Amz-Checksum-Mode=ENABLED into the presigned
	// query, which Lyve signs differently — every such link answered 403
	// SignatureDoesNotMatch (Step 0.1, docs/reviews/WP-EDGE-1B.md; iDrive
	// accepted it). The Worker reads the body, not a checksum trailer.
	pc := s3.NewPresignClient(t.client, func(o *s3.PresignOptions) {
		o.ClientOptions = append(o.ClientOptions, func(co *s3.Options) {
			co.ResponseChecksumValidation = aws.ResponseChecksumValidationWhenRequired
		})
	})
	out, err := pc.PresignGetObject(ctx, in, s3.WithPresignExpires(edgeTTL(p)))
	if err != nil {
		return "", nil, fmt.Errorf("%s presign get %s: %w", t.name, t.key, err)
	}
	names, err := signedHeaderNames(out.URL)
	if err != nil {
		return "", nil, fmt.Errorf("%s presign get %s: %w", t.name, t.key, err)
	}
	return out.URL, names, nil
}

func (t s3EdgeTarget) createMultipart(ctx context.Context, contentType string) (string, error) {
	in := &s3.CreateMultipartUploadInput{Bucket: aws.String(t.bucket), Key: aws.String(t.key)}
	if contentType != "" {
		in.ContentType = aws.String(contentType)
	}
	out, err := t.client.CreateMultipartUpload(ctx, in, sdkCallOptions(ctx)...)
	if err != nil {
		return "", fmt.Errorf("%s create multipart %s: %w", t.name, t.key, err)
	}
	if out.UploadId == nil || *out.UploadId == "" {
		return "", fmt.Errorf("%s create multipart %s: the backend answered no upload id", t.name, t.key)
	}
	return *out.UploadId, nil
}

func (t s3EdgeTarget) presignUploadPart(ctx context.Context, uploadID string, partNumber int, p engine.EdgePresign) (string, []string, error) {
	if partNumber < 1 || partNumber > 10000 {
		return "", nil, fmt.Errorf("%s presign part %s: part number %d out of range", t.name, t.key, partNumber)
	}
	in := &s3.UploadPartInput{Bucket: aws.String(t.bucket), Key: aws.String(t.key), UploadId: aws.String(uploadID),
		PartNumber: aws.Int32(int32(partNumber)), ContentLength: aws.Int64(p.ContentLength)} // #nosec G115 — bounded above
	if p.ContentMD5 != "" {
		in.ContentMD5 = aws.String(p.ContentMD5)
	}
	out, err := s3.NewPresignClient(t.client).PresignUploadPart(ctx, in, s3.WithPresignExpires(edgeTTL(p)))
	if err != nil {
		return "", nil, fmt.Errorf("%s presign part %s: %w", t.name, t.key, err)
	}
	names, err := signedHeaderNames(out.URL)
	if err != nil {
		return "", nil, fmt.Errorf("%s presign part %s: %w", t.name, t.key, err)
	}
	return out.URL, names, nil
}

func (t s3EdgeTarget) completeMultipart(ctx context.Context, uploadID string, parts []engine.EdgePart) (string, error) {
	if len(parts) == 0 {
		return "", fmt.Errorf("%s complete multipart %s: no parts", t.name, t.key)
	}
	completed := make([]types.CompletedPart, 0, len(parts))
	for _, p := range parts {
		if p.Number < 1 || p.Number > 10000 {
			return "", fmt.Errorf("%s complete multipart %s: part number %d out of range", t.name, t.key, p.Number)
		}
		completed = append(completed, types.CompletedPart{PartNumber: aws.Int32(int32(p.Number)), ETag: aws.String(p.ETag)}) // #nosec G115 — bounded above
	}
	out, err := t.client.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{
		Bucket: aws.String(t.bucket), Key: aws.String(t.key), UploadId: aws.String(uploadID),
		MultipartUpload: &types.CompletedMultipartUpload{Parts: completed},
	}, sdkCallOptions(ctx)...)
	if err != nil {
		return "", fmt.Errorf("%s complete multipart %s: %w", t.name, t.key, err)
	}
	if out.ETag == nil || *out.ETag == "" {
		return "", fmt.Errorf("%s complete multipart %s: the backend answered no ETag", t.name, t.key)
	}
	return *out.ETag, nil
}

func (t s3EdgeTarget) abortMultipart(ctx context.Context, uploadID string) error {
	if _, err := t.client.AbortMultipartUpload(ctx, &s3.AbortMultipartUploadInput{
		Bucket: aws.String(t.bucket), Key: aws.String(t.key), UploadId: aws.String(uploadID),
	}, sdkCallOptions(ctx)...); err != nil {
		// An upload already gone is aborted (a retry of a reaper's abort).
		if s3IsNoSuchUpload(err) {
			return nil
		}
		return fmt.Errorf("%s abort multipart %s: %w", t.name, t.key, err)
	}
	return nil
}

// headObject is one HeadObject (the meta class, the SDK's per-path
// attempts): the ETag unquoted and lower-cased, the size.
func (t s3EdgeTarget) headObject(ctx context.Context, container, artifact string) (engine.RangeInfo, error) {
	out, err := t.client.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String(t.bucket), Key: aws.String(t.key)}, sdkCallOptions(ctx)...)
	if err != nil {
		if s3IsNotFound(err) {
			return engine.RangeInfo{}, &engine.NotFoundError{Container: container, Artifact: artifact}
		}
		return engine.RangeInfo{}, fmt.Errorf("%s head %s: %w", t.name, t.key, err)
	}
	info := engine.RangeInfo{ETag: strings.ToLower(strings.Trim(aws.ToString(out.ETag), `"`))}
	if out.ContentLength != nil {
		info.Size = *out.ContentLength
	}
	return info, nil
}

// --- the fixed-bucket driver (idrive, idrive-<region>, wasabi) ---

func (d *IDriveDriver) edgeTarget(ctx context.Context, op, container, artifact string) (s3EdgeTarget, error) {
	tenantID, err := d.getTenantID(ctx, op)
	if err != nil {
		return s3EdgeTarget{}, err
	}
	return s3EdgeTarget{name: d.name, client: d.client, bucket: d.bucket, key: d.buildKey(tenantID, container, artifact)}, nil
}

// PresignPut implements engine.EdgeWriter.
func (d *IDriveDriver) PresignPut(ctx context.Context, container, artifact string, p engine.EdgePresign) (string, []string, error) {
	t, err := d.edgeTarget(ctx, "PresignPut", container, artifact)
	if err != nil {
		return "", nil, err
	}
	return t.presignPut(ctx, p)
}

// PresignGet implements engine.EdgeWriter.
func (d *IDriveDriver) PresignGet(ctx context.Context, container, artifact string, p engine.EdgePresign) (string, []string, error) {
	t, err := d.edgeTarget(ctx, "PresignGet", container, artifact)
	if err != nil {
		return "", nil, err
	}
	return t.presignGet(ctx, p)
}

// CreateMultipart implements engine.EdgeWriter.
func (d *IDriveDriver) CreateMultipart(ctx context.Context, container, artifact, contentType string) (string, error) {
	t, err := d.edgeTarget(ctx, "CreateMultipart", container, artifact)
	if err != nil {
		return "", err
	}
	return t.createMultipart(ctx, contentType)
}

// PresignUploadPart implements engine.EdgeWriter.
func (d *IDriveDriver) PresignUploadPart(ctx context.Context, container, artifact, uploadID string, partNumber int, p engine.EdgePresign) (string, []string, error) {
	t, err := d.edgeTarget(ctx, "PresignUploadPart", container, artifact)
	if err != nil {
		return "", nil, err
	}
	return t.presignUploadPart(ctx, uploadID, partNumber, p)
}

// CompleteMultipart implements engine.EdgeWriter.
func (d *IDriveDriver) CompleteMultipart(ctx context.Context, container, artifact, uploadID string, parts []engine.EdgePart) (string, error) {
	t, err := d.edgeTarget(ctx, "CompleteMultipart", container, artifact)
	if err != nil {
		return "", err
	}
	return t.completeMultipart(ctx, uploadID, parts)
}

// AbortMultipart implements engine.EdgeWriter.
func (d *IDriveDriver) AbortMultipart(ctx context.Context, container, artifact, uploadID string) error {
	t, err := d.edgeTarget(ctx, "AbortMultipart", container, artifact)
	if err != nil {
		return err
	}
	return t.abortMultipart(ctx, uploadID)
}

// HeadObject implements engine.ObjectHeader.
func (d *IDriveDriver) HeadObject(ctx context.Context, container, artifact string) (engine.RangeInfo, error) {
	t, err := d.edgeTarget(ctx, "HeadObject", container, artifact)
	if err != nil {
		return engine.RangeInfo{}, err
	}
	return t.headObject(ctx, container, artifact)
}

// --- lyve ---

func (d *LyveDriver) edgeTarget(ctx context.Context, op, container, artifact string) (s3EdgeTarget, error) {
	tenantID, err := d.getTenantID(ctx, op)
	if err != nil {
		return s3EdgeTarget{}, err
	}
	return s3EdgeTarget{name: "lyve", client: d.client, bucket: d.getBucket(), key: d.buildTenantKey(tenantID, container, artifact)}, nil
}

// PresignPut implements engine.EdgeWriter.
func (d *LyveDriver) PresignPut(ctx context.Context, container, artifact string, p engine.EdgePresign) (string, []string, error) {
	t, err := d.edgeTarget(ctx, "PresignPut", container, artifact)
	if err != nil {
		return "", nil, err
	}
	return t.presignPut(ctx, p)
}

// PresignGet implements engine.EdgeWriter.
func (d *LyveDriver) PresignGet(ctx context.Context, container, artifact string, p engine.EdgePresign) (string, []string, error) {
	t, err := d.edgeTarget(ctx, "PresignGet", container, artifact)
	if err != nil {
		return "", nil, err
	}
	return t.presignGet(ctx, p)
}

// CreateMultipart implements engine.EdgeWriter.
func (d *LyveDriver) CreateMultipart(ctx context.Context, container, artifact, contentType string) (string, error) {
	t, err := d.edgeTarget(ctx, "CreateMultipart", container, artifact)
	if err != nil {
		return "", err
	}
	return t.createMultipart(ctx, contentType)
}

// PresignUploadPart implements engine.EdgeWriter.
func (d *LyveDriver) PresignUploadPart(ctx context.Context, container, artifact, uploadID string, partNumber int, p engine.EdgePresign) (string, []string, error) {
	t, err := d.edgeTarget(ctx, "PresignUploadPart", container, artifact)
	if err != nil {
		return "", nil, err
	}
	return t.presignUploadPart(ctx, uploadID, partNumber, p)
}

// CompleteMultipart implements engine.EdgeWriter.
func (d *LyveDriver) CompleteMultipart(ctx context.Context, container, artifact, uploadID string, parts []engine.EdgePart) (string, error) {
	t, err := d.edgeTarget(ctx, "CompleteMultipart", container, artifact)
	if err != nil {
		return "", err
	}
	return t.completeMultipart(ctx, uploadID, parts)
}

// AbortMultipart implements engine.EdgeWriter.
func (d *LyveDriver) AbortMultipart(ctx context.Context, container, artifact, uploadID string) error {
	t, err := d.edgeTarget(ctx, "AbortMultipart", container, artifact)
	if err != nil {
		return err
	}
	return t.abortMultipart(ctx, uploadID)
}

// HeadObject implements engine.ObjectHeader.
func (d *LyveDriver) HeadObject(ctx context.Context, container, artifact string) (engine.RangeInfo, error) {
	t, err := d.edgeTarget(ctx, "HeadObject", container, artifact)
	if err != nil {
		return engine.RangeInfo{}, err
	}
	return t.headObject(ctx, container, artifact)
}

// The two drivers are edge writers.
var (
	_ engine.EdgeWriter   = (*IDriveDriver)(nil)
	_ engine.EdgeWriter   = (*LyveDriver)(nil)
	_ engine.ObjectHeader = (*IDriveDriver)(nil)
	_ engine.ObjectHeader = (*LyveDriver)(nil)
)
