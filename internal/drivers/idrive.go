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

	"github.com/FairForge/vaultaire/internal/engine"
)

// IDriveDriver implements Driver interface for iDrive E2 storage
type IDriveDriver struct {
	accessKey          string
	secretKey          string
	endpoint           string
	region             string
	client             *s3.Client
	logger             *zap.Logger
	multipartThreshold int64 // Size threshold for multipart uploads
	partSize           int64 // Size of each part in multipart upload

}

// NewIDriveDriver creates a new iDrive E2 storage driver
func NewIDriveDriver(accessKey, secretKey, endpoint, region string, logger *zap.Logger) (*IDriveDriver, error) {
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

	// Configure AWS SDK for iDrive E2
	cfg, err := config.LoadDefaultConfig(context.Background(),
		config.WithRegion(region),
		config.WithCredentialsProvider(
			credentials.NewStaticCredentialsProvider(accessKey, secretKey, ""),
		),
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
		client:             client,
		logger:             logger,
		multipartThreshold: 5 * 1024 * 1024, // 5MB default
		partSize:           5 * 1024 * 1024, // 5MB parts

	}, nil
}

// Get retrieves an artifact from iDrive
func (d *IDriveDriver) Get(ctx context.Context, container, artifact string) (io.ReadCloser, error) {
	result, err := d.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(container),
		Key:    aws.String(artifact),
	})
	if err != nil {
		return nil, fmt.Errorf("idrive get %s/%s: %w", container, artifact, err)
	}

	d.logger.Debug("artifact retrieved",
		zap.String("container", container),
		zap.String("artifact", artifact),
	)
	return result.Body, nil
}

// Put stores an artifact in iDrive
func (d *IDriveDriver) Put(ctx context.Context, container, artifact string, data io.Reader, opts ...engine.PutOption) error {
	// Note: The opts parameter matches your interface but we'll implement option handling later
	_, err := d.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(container),
		Key:    aws.String(artifact),
		Body:   data,
	})
	if err != nil {
		return fmt.Errorf("idrive put %s/%s: %w", container, artifact, err)
	}

	d.logger.Debug("artifact stored",
		zap.String("container", container),
		zap.String("artifact", artifact),
	)
	return nil
}

// PutWithSize handles uploads with known size, using multipart for large files
func (d *IDriveDriver) PutWithSize(ctx context.Context, container, artifact string, reader io.Reader, size int64) error {
	// Use multipart for large files
	if size > d.multipartThreshold {
		return d.putMultipart(ctx, container, artifact, reader, size)
	}

	// Regular upload for small files
	return d.Put(ctx, container, artifact, reader)
}

// putMultipart handles multipart uploads for large files
func (d *IDriveDriver) putMultipart(ctx context.Context, container, artifact string, reader io.Reader, size int64) error {
	// Start multipart upload
	createResp, err := d.client.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{
		Bucket: aws.String(container),
		Key:    aws.String(artifact),
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

		// Upload part
		uploadResp, err := d.client.UploadPart(ctx, &s3.UploadPartInput{
			Bucket:     aws.String(container),
			Key:        aws.String(artifact),
			PartNumber: aws.Int32(partNumber),
			UploadId:   aws.String(uploadID),
			Body:       bytes.NewReader(partData[:n]),
		})
		if err != nil {
			d.abortMultipartUpload(ctx, container, artifact, uploadID)
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

	// Complete multipart upload
	_, err = d.client.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{
		Bucket:   aws.String(container),
		Key:      aws.String(artifact),
		UploadId: aws.String(uploadID),
		MultipartUpload: &types.CompletedMultipartUpload{
			Parts: parts,
		},
	})
	if err != nil {
		d.abortMultipartUpload(ctx, container, artifact, uploadID)
		return fmt.Errorf("idrive complete multipart %s/%s: %w", container, artifact, err)
	}

	d.logger.Info("multipart upload completed",
		zap.String("container", container),
		zap.String("artifact", artifact),
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
	input := &s3.GetObjectInput{
		Bucket: aws.String(container),
		Key:    aws.String(artifact),
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
	_, err := d.client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(container),
		Key:    aws.String(artifact),
	})
	if err != nil {
		return fmt.Errorf("idrive delete %s/%s: %w", container, artifact, err)
	}

	d.logger.Debug("artifact deleted",
		zap.String("container", container),
		zap.String("artifact", artifact),
	)
	return nil
}

// List returns artifacts in a container with optional prefix
func (d *IDriveDriver) List(ctx context.Context, container string, prefix string) ([]string, error) {
	var artifacts []string

	paginator := s3.NewListObjectsV2Paginator(d.client, &s3.ListObjectsV2Input{
		Bucket: aws.String(container),
		Prefix: aws.String(prefix),
	})

	for paginator.HasMorePages() {
		output, err := paginator.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("idrive list %s/%s: %w", container, prefix, err)
		}

		for _, obj := range output.Contents {
			artifacts = append(artifacts, *obj.Key)
		}
	}

	d.logger.Debug("artifacts listed",
		zap.String("container", container),
		zap.String("prefix", prefix),
		zap.Int("count", len(artifacts)),
	)
	return artifacts, nil
}

// Exists checks if an artifact exists
func (d *IDriveDriver) Exists(ctx context.Context, container, artifact string) (bool, error) {
	_, err := d.client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(container),
		Key:    aws.String(artifact),
	})

	if err != nil {
		// Check if it's a "not found" error
		if strings.Contains(err.Error(), "NotFound") ||
			strings.Contains(err.Error(), "404") {
			return false, nil
		}
		return false, fmt.Errorf("idrive exists %s/%s: %w", container, artifact, err)
	}

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
