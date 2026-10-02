// internal/drivers/idrive.go
package drivers

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"go.uber.org/zap"

	"github.com/FairForge/vaultaire/internal/engine"
)

type ContextKey string

const (
	// TenantIDKey is the context key for tenant ID
	TenantIDKey ContextKey = "tenant_id"
)

// IDriveDriver implements Driver interface for iDrive E2 storage.
// Uses a fixed bucket with tenant-prefixed keys (like GeyserDriver).
type IDriveDriver struct {
	accessKey     string
	secretKey     string
	endpoint      string
	region        string
	bucket        string
	client        *s3.Client
	logger        *zap.Logger
	egressTracker *EgressTracker // Track bandwidth usage
}

// NewIDriveDriver creates a new iDrive E2 storage driver.
// All objects are stored in `bucket` with keys prefixed by tenant ID.
func NewIDriveDriver(accessKey, secretKey, endpoint, region string, logger *zap.Logger) (*IDriveDriver, error) {
	bucket := os.Getenv("IDRIVE_BUCKET")
	if bucket == "" {
		bucket = "vaultaire"
	}
	// Validate required parameters
	if endpoint == "" {
		return nil, fmt.Errorf("idrive: endpoint required")
	}
	if accessKey == "" || secretKey == "" {
		return nil, fmt.Errorf("idrive: credentials required")
	}
	if region == "" {
		region = IDriveFallbackRegion
	}

	cfg, err := config.LoadDefaultConfig(context.Background(),
		config.WithRegion(region),
		config.WithCredentialsProvider(
			credentials.NewStaticCredentialsProvider(accessKey, secretKey, ""),
		),
		// HTTP/1.1 pinned (2026-08-05 ALPN audit): iDrive gateways refuse h2
		// at ALPN today — no-op now, insurance against a silent vendor flip
		// onto h2 single-connection multiplexing (see lyve.go / geyser.go).
		config.WithHTTPClient(TunedHTTPClient(WithHTTP1Only())),
	)
	if err != nil {
		return nil, fmt.Errorf("idrive: load aws config: %w", err)
	}

	// Create S3 client with iDrive endpoint
	client := s3.NewFromConfig(cfg, func(o *s3.Options) {
		o.BaseEndpoint = aws.String(endpoint)
		o.UsePathStyle = true // iDrive requires path-style URLs
	})

	logger.Info("iDrive driver initialized",
		zap.String("endpoint", endpoint),
		zap.String("region", region),
	)

	return &IDriveDriver{
		accessKey: accessKey,
		secretKey: secretKey,
		endpoint:  endpoint,
		region:    region,
		bucket:    bucket,
		client:    client,
		logger:    logger,
	}, nil
}

// getTenantID is the tenant the call is made for; a context that names none
// is refused (tenant_ctx.go) — it used to resolve to "default".
func (d *IDriveDriver) getTenantID(ctx context.Context, op string) (string, error) {
	return requireTenant(ctx, d.Name(), op, "", d.logger)
}

// ObjectKey is the key a call would address (engine.KeyAddresser).
func (d *IDriveDriver) ObjectKey(ctx context.Context, container, artifact string) string {
	return d.buildKey(contextTenant(ctx), container, artifact)
}

func (d *IDriveDriver) buildKey(tenantID, container, artifact string) string {
	return fmt.Sprintf("t-%s/%s/%s", tenantID, container, artifact)
}

// Get retrieves an artifact from iDrive
func (d *IDriveDriver) Get(ctx context.Context, container, artifact string) (io.ReadCloser, error) {
	tenantID, tErr := d.getTenantID(ctx, "Get")
	if tErr != nil {
		return nil, tErr
	}
	key := d.buildKey(tenantID, container, artifact)

	result, err := d.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(d.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return nil, fmt.Errorf("idrive get %s/%s: %w", container, artifact, err)
	}

	// Track egress if we have a tracker and tenant ID
	if d.egressTracker != nil {
		if tenantID, ok := ctx.Value(TenantIDKey).(string); ok && tenantID != "" {
			// Wrap the reader to track bytes read
			return &egressTrackingReader{
				ReadCloser: result.Body,
				tracker:    d.egressTracker,
				tenantID:   tenantID,
			}, nil
		}
	}

	return result.Body, nil
}

// egressTrackingReader wraps a reader to track bytes read
type egressTrackingReader struct {
	io.ReadCloser
	tracker   *EgressTracker
	tenantID  string
	bytesRead int64
}

func (r *egressTrackingReader) Read(p []byte) (n int, err error) {
	n, err = r.ReadCloser.Read(p)
	if n > 0 {
		r.bytesRead += int64(n)
		r.tracker.RecordEgress(r.tenantID, int64(n))
	}
	return n, err
}

// GetEgressTracker returns the egress tracker
func (d *IDriveDriver) GetEgressTracker() *EgressTracker {
	return d.egressTracker
}

// SetEgressTracker sets the egress tracker
func (d *IDriveDriver) SetEgressTracker(tracker *EgressTracker) {
	d.egressTracker = tracker
}

// Put stores an artifact in iDrive.
// iDrive E2 requires Content-Length on every PUT (HTTP 411 otherwise).
// When ContentLength is passed via PutOptions (from the S3 API adapter),
// we stream directly without buffering. Otherwise we fall back to
// materialize() to determine size.
func (d *IDriveDriver) Put(ctx context.Context, container, artifact string, data io.Reader, opts ...engine.PutOption) error {
	tenantID, tErr := d.getTenantID(ctx, "Put")
	if tErr != nil {
		return tErr
	}
	key := d.buildKey(tenantID, container, artifact)
	options := engine.ApplyPutOptions(opts...)

	// Parallel multipart upload: large files upload as concurrent parts. The
	// uploader streams parts on the fly, so it also avoids the full-object
	// buffering the old unknown-length path did via materialize. Small files
	// still go as a single PutObject.
	if err := s3ParallelUpload(ctx, d.client, d.bucket, key, options.ContentType, data, options.ContentLength); err != nil {
		return err
	}
	return nil
}

// GetRange reads a byte range directly from iDrive without downloading the
// full object. Implements engine.RangeGetter.
func (d *IDriveDriver) GetRange(ctx context.Context, container, artifact string, offset, length int64) (io.ReadCloser, error) {
	tenantID, tErr := d.getTenantID(ctx, "GetRange")
	if tErr != nil {
		return nil, tErr
	}
	key := d.buildKey(tenantID, container, artifact)

	input := &s3.GetObjectInput{
		Bucket: aws.String(d.bucket),
		Key:    aws.String(key),
	}

	// Add range header if specified
	if offset > 0 || length > 0 {
		rangeHeader := fmt.Sprintf("bytes=%d-", offset)
		if length > 0 {
			rangeHeader = fmt.Sprintf("bytes=%d-%d", offset, offset+length-1)
		}
		input.Range = aws.String(rangeHeader)
	}

	result, err := d.client.GetObject(ctx, input)
	if err != nil {
		return nil, fmt.Errorf("idrive get stream %s/%s: %w", container, artifact, err)
	}

	return result.Body, nil
}

// Delete removes an artifact from iDrive
func (d *IDriveDriver) Delete(ctx context.Context, container, artifact string) error {
	tenantID, tErr := d.getTenantID(ctx, "Delete")
	if tErr != nil {
		return tErr
	}
	key := d.buildKey(tenantID, container, artifact)

	_, err := d.client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(d.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return fmt.Errorf("idrive delete %s: %w", key, err)
	}
	return nil
}

// List returns artifacts in a container with optional prefix
func (d *IDriveDriver) List(ctx context.Context, container string, prefix string) ([]string, error) {
	tenantID, tErr := d.getTenantID(ctx, "List")
	if tErr != nil {
		return nil, tErr
	}
	fullPrefix := d.buildKey(tenantID, container, prefix)
	basePrefix := d.buildKey(tenantID, container, "")

	var artifacts []string
	paginator := s3ListPaginator(d.client, &s3.ListObjectsV2Input{
		Bucket: aws.String(d.bucket),
		Prefix: aws.String(fullPrefix),
	})

	for paginator.HasMorePages() {
		output, err := paginator.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("idrive list %s: %w", fullPrefix, err)
		}
		for _, obj := range output.Contents {
			name := strings.TrimPrefix(*obj.Key, basePrefix)
			artifacts = append(artifacts, name)
		}
	}
	return artifacts, nil
}

// Exists checks if an artifact exists
func (d *IDriveDriver) Exists(ctx context.Context, container, artifact string) (bool, error) {
	tenantID, tErr := d.getTenantID(ctx, "Exists")
	if tErr != nil {
		return false, tErr
	}
	key := d.buildKey(tenantID, container, artifact)

	_, err := d.client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(d.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		if s3IsNotFound(err) {
			return false, nil
		}
		return false, fmt.Errorf("idrive exists %s: %w", key, err)
	}
	return true, nil
}

func (d *IDriveDriver) Name() string {
	return "idrive"
}

func (d *IDriveDriver) HealthCheck(ctx context.Context) error {
	_, err := d.client.HeadBucket(ctx, &s3.HeadBucketInput{
		Bucket: aws.String(d.bucket),
	})
	if err != nil {
		return fmt.Errorf("idrive health check: %w", err)
	}
	return nil
}

// Region returns the region this driver signs for.
func (d *IDriveDriver) Region() string { return d.region }

// EnsureBucket makes sure the driver's fixed bucket exists in its region,
// creating it when HeadBucket says it is absent (WP-R7-1: the reseller
// account has no `vaultaire` bucket outside the primary region, so an
// idrive-<region> driver registered with its own key pair provisions its
// bucket at boot). Any other HeadBucket error — 403 for a wrong key, a
// transport failure — is returned unchanged so the caller can log it; the
// probe reports it thereafter. Returns whether the bucket was created.
func (d *IDriveDriver) EnsureBucket(ctx context.Context) (created bool, err error) {
	_, err = d.client.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: aws.String(d.bucket)})
	if err == nil {
		return false, nil
	}
	if !s3IsNotFound(err) {
		return false, fmt.Errorf("idrive head bucket %s in %s: %w", d.bucket, d.region, err)
	}
	if _, err := d.client.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String(d.bucket)}); err != nil {
		return false, fmt.Errorf("idrive create bucket %s in %s: %w", d.bucket, d.region, err)
	}
	d.logger.Info("iDrive region bucket created", zap.String("bucket", d.bucket), zap.String("region", d.region))
	return true, nil
}

func (d *IDriveDriver) ValidateAuth(ctx context.Context) error {
	// Try to list buckets - this requires valid authentication
	_, err := d.client.ListBuckets(ctx, &s3.ListBucketsInput{})
	if err != nil {
		return fmt.Errorf("idrive authentication failed: %w", err)
	}

	d.logger.Info("iDrive authentication validated")
	return nil
}
