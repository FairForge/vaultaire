// internal/drivers/idrive.go
package drivers

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"go.uber.org/zap"

	"github.com/FairForge/vaultaire/internal/common"
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
	accessKey          string
	secretKey          string
	endpoint           string
	region             string
	bucket             string
	client             *s3.Client
	logger             *zap.Logger
	multipartThreshold int64          // Size threshold for multipart uploads
	partSize           int64          // Size of each part in multipart upload
	egressTracker      *EgressTracker // Track bandwidth usage
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
		region = "us-west-1" // Default region
	}

	cfg, err := config.LoadDefaultConfig(context.Background(),
		config.WithRegion(region),
		config.WithCredentialsProvider(
			credentials.NewStaticCredentialsProvider(accessKey, secretKey, ""),
		),
		config.WithHTTPClient(TunedHTTPClient()),
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
		accessKey:          accessKey,
		secretKey:          secretKey,
		endpoint:           endpoint,
		region:             region,
		bucket:             bucket,
		client:             client,
		logger:             logger,
		multipartThreshold: 5 * 1024 * 1024,
		partSize:           5 * 1024 * 1024,
	}, nil
}

func (d *IDriveDriver) getTenantID(ctx context.Context) string {
	if tid, ok := ctx.Value(common.TenantIDKey).(string); ok && tid != "" {
		return tid
	}
	return "default"
}

func (d *IDriveDriver) buildKey(tenantID, container, artifact string) string {
	return fmt.Sprintf("t-%s/%s/%s", tenantID, container, artifact)
}

// Get retrieves an artifact from iDrive
func (d *IDriveDriver) Get(ctx context.Context, container, artifact string) (io.ReadCloser, error) {
	tenantID := d.getTenantID(ctx)
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
	tenantID := d.getTenantID(ctx)
	key := d.buildKey(tenantID, container, artifact)
	options := engine.ApplyPutOptions(opts...)

	// Parallel multipart upload: large files upload as concurrent parts. The
	// uploader streams parts on the fly, so it also avoids the full-object
	// buffering the old unknown-length path did via materialize. Small files
	// still go as a single PutObject.
	if err := s3ParallelUpload(ctx, d.client, d.bucket, key, options.ContentType, data); err != nil {
		return err
	}
	return nil
}

// PutWithSize handles uploads with known size, using multipart for large files
func (d *IDriveDriver) PutWithSize(ctx context.Context, container, artifact string, reader io.Reader, size int64) error {
	if size > d.multipartThreshold {
		return d.putMultipart(ctx, container, artifact, reader, size)
	}
	return d.Put(ctx, container, artifact, reader)
}

// putMultipart handles multipart uploads for large files
func (d *IDriveDriver) putMultipart(ctx context.Context, container, artifact string, reader io.Reader, size int64) error {
	tenantID := d.getTenantID(ctx)
	key := d.buildKey(tenantID, container, artifact)

	createResp, err := d.client.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{
		Bucket: aws.String(d.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return fmt.Errorf("idrive create multipart %s/%s: %w", container, artifact, err)
	}

	uploadID := *createResp.UploadId
	var parts []types.CompletedPart
	partNumber := int32(1)

	// Upload parts
	for {
		// Read part data
		partData := make([]byte, d.partSize)
		n, err := io.ReadFull(reader, partData)
		if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
			// Abort on error
			d.abortMultipartUpload(ctx, container, artifact, uploadID)
			return fmt.Errorf("idrive read part %d: %w", partNumber, err)
		}

		if n == 0 {
			break
		}

		uploadResp, err := d.client.UploadPart(ctx, &s3.UploadPartInput{
			Bucket:     aws.String(d.bucket),
			Key:        aws.String(key),
			PartNumber: aws.Int32(partNumber),
			UploadId:   aws.String(uploadID),
			Body:       bytes.NewReader(partData[:n]),
		})
		if err != nil {
			d.abortMultipartUpload(ctx, d.bucket, key, uploadID)
			return fmt.Errorf("idrive upload part %d: %w", partNumber, err)
		}

		parts = append(parts, types.CompletedPart{
			ETag:       uploadResp.ETag,
			PartNumber: aws.Int32(partNumber),
		})

		partNumber++

		if err == io.EOF || err == io.ErrUnexpectedEOF {
			break
		}
	}

	_, err = d.client.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{
		Bucket:   aws.String(d.bucket),
		Key:      aws.String(key),
		UploadId: aws.String(uploadID),
		MultipartUpload: &types.CompletedMultipartUpload{
			Parts: parts,
		},
	})
	if err != nil {
		d.abortMultipartUpload(ctx, d.bucket, key, uploadID)
		return fmt.Errorf("idrive complete multipart %s: %w", key, err)
	}

	d.logger.Info("multipart upload completed",
		zap.String("bucket", d.bucket),
		zap.String("key", key),
		zap.Int("parts", len(parts)),
	)

	return nil
}

// abortMultipartUpload cancels a multipart upload
func (d *IDriveDriver) abortMultipartUpload(ctx context.Context, container, artifact, uploadID string) {
	_, err := d.client.AbortMultipartUpload(ctx, &s3.AbortMultipartUploadInput{
		Bucket:   aws.String(container),
		Key:      aws.String(artifact),
		UploadId: aws.String(uploadID),
	})
	if err != nil {
		d.logger.Error("failed to abort multipart upload",
			zap.String("container", container),
			zap.String("artifact", artifact),
			zap.String("uploadID", uploadID),
			zap.Error(err),
		)
	}
}

// GetStream returns a streaming reader for downloads
func (d *IDriveDriver) GetStream(ctx context.Context, container, artifact string, offset, length int64) (io.ReadCloser, error) {
	tenantID := d.getTenantID(ctx)
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
	tenantID := d.getTenantID(ctx)
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
	tenantID := d.getTenantID(ctx)
	fullPrefix := d.buildKey(tenantID, container, prefix)
	basePrefix := d.buildKey(tenantID, container, "")

	var artifacts []string
	paginator := s3.NewListObjectsV2Paginator(d.client, &s3.ListObjectsV2Input{
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
	tenantID := d.getTenantID(ctx)
	key := d.buildKey(tenantID, container, artifact)

	_, err := d.client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(d.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		if strings.Contains(err.Error(), "NotFound") || strings.Contains(err.Error(), "404") {
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

func (d *IDriveDriver) ValidateAuth(ctx context.Context) error {
	// Try to list buckets - this requires valid authentication
	_, err := d.client.ListBuckets(ctx, &s3.ListBucketsInput{})
	if err != nil {
		return fmt.Errorf("idrive authentication failed: %w", err)
	}

	d.logger.Info("iDrive authentication validated")
	return nil
}

// LoadIDriveConfig loads iDrive configuration from environment
func LoadIDriveConfig() (accessKey, secretKey, endpoint, region string) {
	accessKey = os.Getenv("IDRIVE_ACCESS_KEY")
	secretKey = os.Getenv("IDRIVE_SECRET_KEY")
	endpoint = os.Getenv("IDRIVE_ENDPOINT")
	region = os.Getenv("IDRIVE_REGION")

	// Defaults
	if endpoint == "" {
		endpoint = "https://e2-us-west-1.idrive.com"
	}
	if region == "" {
		region = "us-west-1"
	}

	return
}

// NewIDriveDriverFromConfig creates an iDrive driver from environment config
func NewIDriveDriverFromConfig(logger *zap.Logger) (*IDriveDriver, error) {
	accessKey, secretKey, endpoint, region := LoadIDriveConfig()

	if accessKey == "" || secretKey == "" {
		return nil, fmt.Errorf("idrive: IDRIVE_ACCESS_KEY and IDRIVE_SECRET_KEY required")
	}

	return NewIDriveDriver(accessKey, secretKey, endpoint, region, logger)
}
