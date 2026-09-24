// internal/drivers/r2.go
package drivers

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"go.uber.org/zap"

	"github.com/FairForge/vaultaire/internal/common"
	"github.com/FairForge/vaultaire/internal/engine"
)

// R2Driver implements engine.Driver for Cloudflare R2.
//
// Role of record (SMART_TIER_DESIGN.md, revised 2026-09-19): PUBLIC BUCKETS /
// CDN ORIGIN ONLY. R2 charges $0 egress and sits next to the Cloudflare-proxied
// cdn.stored.ge host, so public objects served through the CDN cost nothing
// to read and (once a custom domain exists) can bypass SLC entirely. It is NOT
// a storage tier: the only thing that routes here is a public-read bucket (see
// api.publicBucketStorageClass / engine "PUBLIC" class).
//
// Layout is the fixed-bucket + tenant-prefixed-key pattern shared with iDrive
// and Geyser (one R2 bucket per jurisdiction; keys `t-<tenant>/<container>/
// <artifact>`). A bucket-per-container model cannot work on R2: container
// names carry an underscore (`<tenant>_<bucket>`), which R2 bucket names
// reject, and the account bucket cap is 1000.
type R2Driver struct {
	accountID    string
	jurisdiction string
	endpoint     string
	bucket       string
	client       *s3.Client
	logger       *zap.Logger
}

// R2DefaultBucket is the R2 bucket public objects land in when R2_BUCKET is
// unset. Created 2026-09-24 (default jurisdiction, location hint WNAM).
const R2DefaultBucket = "vaultaire-public"

// r2Jurisdictions are the jurisdiction endpoints R2 documents
// (developers.cloudflare.com/r2/reference/data-location). Note (2026-09-24):
// the account can CREATE `us` buckets via the API, but the
// `<acct>.us.r2.cloudflarestorage.com` S3 endpoint fails the TLS handshake
// (alert 40) from both the Mac and SLC — treat `us` as not usable until
// Cloudflare fixes the endpoint; `default` buckets take a location hint
// (WNAM/ENAM) which is what we actually need.
var r2Jurisdictions = map[string]bool{"default": true, "eu": true, "us": true, "fedramp": true}

// R2Endpoint returns the S3 API endpoint for an account + jurisdiction.
// "" and "default" both mean the account-wide endpoint.
func R2Endpoint(accountID, jurisdiction string) (string, error) {
	j := strings.ToLower(strings.TrimSpace(jurisdiction))
	if j == "" {
		j = "default"
	}
	if !r2Jurisdictions[j] {
		return "", fmt.Errorf("r2: unknown jurisdiction %q (want default|eu|us|fedramp)", jurisdiction)
	}
	if j == "default" {
		return fmt.Sprintf("https://%s.r2.cloudflarestorage.com", accountID), nil
	}
	return fmt.Sprintf("https://%s.%s.r2.cloudflarestorage.com", accountID, j), nil
}

// NewR2Driver creates the R2 backend. jurisdiction "" = default; bucket "" =
// R2DefaultBucket. The bucket must already exist (HealthCheck = HeadBucket).
func NewR2Driver(accountID, accessKey, secretKey, jurisdiction, bucket string, logger *zap.Logger) (*R2Driver, error) {
	if strings.TrimSpace(accountID) == "" {
		return nil, fmt.Errorf("r2: account id required")
	}
	if accessKey == "" || secretKey == "" {
		return nil, fmt.Errorf("r2: credentials required")
	}
	endpoint, err := R2Endpoint(accountID, jurisdiction)
	if err != nil {
		return nil, err
	}
	j := strings.ToLower(strings.TrimSpace(jurisdiction))
	if j == "" {
		j = "default"
	}
	if bucket == "" {
		bucket = R2DefaultBucket
	}

	cfg, err := config.LoadDefaultConfig(context.Background(),
		// R2 signs against region "auto" (any region string is accepted, but
		// "auto" is what Cloudflare documents).
		config.WithRegion("auto"),
		config.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(accessKey, secretKey, "")),
		// HTTP/1.1 pinned like the other S3 backends: Go funnels an h2
		// origin's uploads through one multiplexed connection, which caps
		// aggregate throughput (see geyser.go / lyve.go for the numbers).
		config.WithHTTPClient(TunedHTTPClient(WithHTTP1Only())),
		// R2 does not accept the SDK's default flexible-checksum trailers on
		// streaming uploads (aws-chunked + x-amz-checksum-crc32 → 501); only
		// compute checksums where an operation requires one.
		config.WithRequestChecksumCalculation(aws.RequestChecksumCalculationWhenRequired),
		config.WithResponseChecksumValidation(aws.ResponseChecksumValidationWhenRequired),
	)
	if err != nil {
		return nil, fmt.Errorf("r2: load aws config: %w", err)
	}

	client := s3.NewFromConfig(cfg, func(o *s3.Options) {
		o.BaseEndpoint = aws.String(endpoint)
		o.UsePathStyle = true
	})

	logger.Info("R2 driver initialized (public-bucket/CDN role)",
		zap.String("endpoint", endpoint),
		zap.String("jurisdiction", j),
		zap.String("bucket", bucket))

	return &R2Driver{
		accountID:    accountID,
		jurisdiction: j,
		endpoint:     endpoint,
		bucket:       bucket,
		client:       client,
		logger:       logger,
	}, nil
}

func (d *R2Driver) getTenantID(ctx context.Context) string {
	if tid := common.GetTenantID(ctx); tid != "" {
		return tid
	}
	return "default"
}

func (d *R2Driver) buildKey(tenantID, container, artifact string) string {
	return fmt.Sprintf("t-%s/%s/%s", tenantID, container, artifact)
}

// ObjectKey is the R2 key an artifact is stored under — what a presigned /
// custom-domain serve path needs to address the object directly.
func (d *R2Driver) ObjectKey(ctx context.Context, container, artifact string) string {
	return d.buildKey(d.getTenantID(ctx), container, artifact)
}

// Bucket returns the R2 bucket public objects live in.
func (d *R2Driver) Bucket() string { return d.bucket }

func (d *R2Driver) Name() string { return "r2" }

// Put streams the object into R2. Known lengths (the API adapter always
// passes ContentLength) go straight through; larger bodies use the shared
// parallel multipart uploader (16 MiB parts).
func (d *R2Driver) Put(ctx context.Context, container, artifact string, data io.Reader, opts ...engine.PutOption) error {
	key := d.ObjectKey(ctx, container, artifact)
	options := engine.ApplyPutOptions(opts...)
	if err := s3ParallelUpload(ctx, d.client, d.bucket, key, options.ContentType, data, options.ContentLength); err != nil {
		return fmt.Errorf("r2 put %s: %w", key, err)
	}
	return nil
}

func (d *R2Driver) Get(ctx context.Context, container, artifact string) (io.ReadCloser, error) {
	key := d.ObjectKey(ctx, container, artifact)
	out, err := d.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(d.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return nil, fmt.Errorf("r2 get %s: %w", key, err)
	}
	return out.Body, nil
}

// GetRange implements engine.RangeGetter (CDN range requests pass straight
// through instead of downloading + discarding).
func (d *R2Driver) GetRange(ctx context.Context, container, artifact string, offset, length int64) (io.ReadCloser, error) {
	key := d.ObjectKey(ctx, container, artifact)
	in := &s3.GetObjectInput{
		Bucket: aws.String(d.bucket),
		Key:    aws.String(key),
	}
	if offset > 0 || length > 0 {
		rng := fmt.Sprintf("bytes=%d-", offset)
		if length > 0 {
			rng = fmt.Sprintf("bytes=%d-%d", offset, offset+length-1)
		}
		in.Range = aws.String(rng)
	}
	out, err := d.client.GetObject(ctx, in)
	if err != nil {
		return nil, fmt.Errorf("r2 get range %s: %w", key, err)
	}
	return out.Body, nil
}

func (d *R2Driver) Delete(ctx context.Context, container, artifact string) error {
	key := d.ObjectKey(ctx, container, artifact)
	if _, err := d.client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(d.bucket),
		Key:    aws.String(key),
	}); err != nil {
		return fmt.Errorf("r2 delete %s: %w", key, err)
	}
	return nil
}

func (d *R2Driver) List(ctx context.Context, container, prefix string) ([]string, error) {
	tenantID := d.getTenantID(ctx)
	fullPrefix := d.buildKey(tenantID, container, prefix)
	basePrefix := d.buildKey(tenantID, container, "")

	var artifacts []string
	paginator := s3.NewListObjectsV2Paginator(d.client, &s3.ListObjectsV2Input{
		Bucket: aws.String(d.bucket),
		Prefix: aws.String(fullPrefix),
	})
	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("r2 list %s: %w", fullPrefix, err)
		}
		for _, obj := range page.Contents {
			artifacts = append(artifacts, strings.TrimPrefix(aws.ToString(obj.Key), basePrefix))
		}
	}
	return artifacts, nil
}

func (d *R2Driver) Exists(ctx context.Context, container, artifact string) (bool, error) {
	key := d.ObjectKey(ctx, container, artifact)
	_, err := d.client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(d.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		if strings.Contains(err.Error(), "NotFound") || strings.Contains(err.Error(), "404") {
			return false, nil
		}
		return false, fmt.Errorf("r2 exists %s: %w", key, err)
	}
	return true, nil
}

// HealthCheck is a signed HeadBucket — a revoked token fails it (the
// authenticated-probe rule, CLAUDE.md architecture decision 1).
func (d *R2Driver) HealthCheck(ctx context.Context) error {
	if _, err := d.client.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: aws.String(d.bucket)}); err != nil {
		return fmt.Errorf("r2 health check (%s): %w", d.bucket, err)
	}
	return nil
}
