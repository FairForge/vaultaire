package drivers

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net/http"
	"path"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"go.uber.org/zap"
)

// S3CompatDriver implements storage.Backend for S3-compatible S3-compatible API
type S3CompatDriver struct {
	client *s3.Client
	bucket string
	prefix string
	logger *zap.Logger
}

// NewS3CompatDriver creates a new S3-compatible storage driver
func NewS3CompatDriver(accessKey, secretKey string, logger *zap.Logger) (*S3CompatDriver, error) {
	// Create HTTP client that allows self-signed certs
	httpClient := &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				InsecureSkipVerify: true,
			},
		},
	}

	// Create S3 client directly with minimal config
	client := s3.New(s3.Options{
		BaseEndpoint: aws.String("https://us.s3compat.cloud:8000"),
		Region:       "us-east-1",
		Credentials: credentials.NewStaticCredentialsProvider(
			accessKey,
			secretKey,
			"",
		),
		UsePathStyle: true,
		HTTPClient:   httpClient,
	})

	return &S3CompatDriver{
		client: client,
		bucket: "data",
		prefix: "personal-files/vaultaire",
		logger: logger,
	}, nil
}

// buildKey constructs the full S3 key with prefix
func (d *S3CompatDriver) buildKey(container, artifact string) string {
	if artifact == "" {
		return path.Join(d.prefix, container)
	}
	return path.Join(d.prefix, container, artifact)
}

// Get retrieves an artifact
func (d *S3CompatDriver) Get(ctx context.Context, container, artifact string) (io.ReadCloser, error) {
	key := d.buildKey(container, artifact)

	result, err := d.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(d.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return nil, fmt.Errorf("get object %s: %w", key, err)
	}

	return result.Body, nil
}

// Put stores an artifact
func (d *S3CompatDriver) Put(ctx context.Context, container, artifact string, data io.Reader) error {
	key := d.buildKey(container, artifact)

	_, err := d.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(d.bucket),
		Key:    aws.String(key),
		Body:   data,
	})
	if err != nil {
		return fmt.Errorf("put object %s: %w", key, err)
	}

	d.logger.Debug("stored artifact in S3-compatible",
		zap.String("key", key),
		zap.String("bucket", d.bucket))

	return nil
}

// Delete removes an artifact
func (d *S3CompatDriver) Delete(ctx context.Context, container, artifact string) error {
	key := d.buildKey(container, artifact)

	_, err := d.client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(d.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return fmt.Errorf("delete object %s: %w", key, err)
	}

	return nil
}

// List lists artifacts in a container
func (d *S3CompatDriver) List(ctx context.Context, container, prefix string) ([]string, error) {
	s3Prefix := d.buildKey(container, "") + "/"

	result, err := d.client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
		Bucket: aws.String(d.bucket),
		Prefix: aws.String(s3Prefix),
	})
	if err != nil {
		return nil, fmt.Errorf("list objects with s3Prefix %s: %w", prefix, err)
	}

	var artifacts []string
	s3PrefixLen := len(prefix)

	for _, obj := range result.Contents {
		if obj.Key != nil && len(*obj.Key) > s3PrefixLen {
			// Remove the prefix to get just the artifact name
			artifactPath := (*obj.Key)[s3PrefixLen:]
			artifacts = append(artifacts, artifactPath)
		}
	}

	return artifacts, nil
}

// HealthCheck verifies connectivity
func (d *S3CompatDriver) HealthCheck(ctx context.Context) error {
	// Try to list the bucket root - this should always work
	_, err := d.client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
		Bucket:  aws.String(d.bucket),
		Prefix:  aws.String(d.prefix),
		MaxKeys: aws.Int32(1),
	})

	if err != nil {
		return fmt.Errorf("health check failed: %w", err)
	}

	d.logger.Debug("S3-compatible health check passed")
	return nil
}

// Name returns the driver name
func (d *S3CompatDriver) Name() string {
	return "s3compat"
}
