// internal/drivers/idrive.go
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

	"github.com/FairForge/vaultaire/internal/engine"
)

// IDriveDriver implements Driver interface for iDrive E2 storage
type IDriveDriver struct {
	accessKey string
	secretKey string
	endpoint  string
	region    string
	client    *s3.Client
	logger    *zap.Logger
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
		accessKey: accessKey,
		secretKey: secretKey,
		endpoint:  endpoint,
		region:    region,
		client:    client,
		logger:    logger,
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
