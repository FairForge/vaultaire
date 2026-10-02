package drivers

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"strings"

	"github.com/FairForge/vaultaire/internal/engine"
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
	insecure := strings.EqualFold(os.Getenv("S3COMPAT_INSECURE_TLS"), "true") ||
		os.Getenv("S3COMPAT_INSECURE_TLS") == "1"
	if insecure {
		logger.Warn("S3-compatible TLS verification disabled via S3COMPAT_INSECURE_TLS")
	}

	var httpClient *http.Client
	if insecure {
		httpClient = TunedHTTPClient(WithInsecureTLS())
	} else {
		httpClient = TunedHTTPClient()
	}

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

// ObjectKey is the key a call would address (engine.KeyAddresser). It does
// not depend on the tenant in the context: the container carries it.
func (d *S3CompatDriver) ObjectKey(_ context.Context, container, artifact string) string {
	return d.buildKey(container, artifact)
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
func (d *S3CompatDriver) Put(ctx context.Context, container, artifact string, data io.Reader, opts ...engine.PutOption) error {
	key := d.buildKey(container, artifact)
	options := engine.ApplyPutOptions(opts...)

	// Parallel multipart upload: large files upload as concurrent parts instead of
	// a single PutObject stream; small files still go as one PutObject.
	if err := s3ParallelUpload(ctx, d.client, d.bucket, key, options.ContentType, data, options.ContentLength); err != nil {
		return err
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

// List returns the artifact names under container that start with prefix,
// relative to the container, across every page (Review R7-06: this used to
// strip len(prefix) bytes off the key — the artifact prefix, not the
// `<root>/<container>/` key prefix — and read one page).
func (d *S3CompatDriver) List(ctx context.Context, container, prefix string) ([]string, error) {
	keyPrefix := d.buildKey(container, "") + "/"

	var artifacts []string
	paginator := s3ListPaginator(d.client, &s3.ListObjectsV2Input{
		Bucket: aws.String(d.bucket),
		Prefix: aws.String(keyPrefix + prefix),
	})
	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("list objects %s%s: %w", keyPrefix, prefix, err)
		}
		for _, obj := range page.Contents {
			key := aws.ToString(obj.Key)
			// A sibling container sharing the prefix (c1 vs c1-other) cannot
			// match keyPrefix because of the trailing slash; the container's
			// own directory marker (key == keyPrefix) yields an empty name.
			if !strings.HasPrefix(key, keyPrefix) || len(key) == len(keyPrefix) {
				continue
			}
			artifacts = append(artifacts, key[len(keyPrefix):])
		}
	}

	return artifacts, nil
}

// Exists checks if an artifact exists
func (d *S3CompatDriver) Exists(ctx context.Context, container, artifact string) (bool, error) {
	key := d.buildKey(container, artifact)

	_, err := d.client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(d.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		if s3IsNotFound(err) {
			return false, nil
		}
		return false, fmt.Errorf("exists %s: %w", key, err)
	}
	return true, nil
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
