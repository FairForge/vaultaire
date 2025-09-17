// internal/drivers/lyve.go
package drivers

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/FairForge/vaultaire/internal/common"
	"github.com/FairForge/vaultaire/internal/engine"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"go.uber.org/zap"
)

type LyveDriver struct {
	client   *s3.Client
	tenantID string // Default tenant, can be overridden by context
	region   string
	logger   *zap.Logger
}

func NewLyveDriver(accessKey, secretKey, tenantID, region string, logger *zap.Logger) (*LyveDriver, error) {
	if region == "" {
		region = "us-east-1"
	}

	endpoint := fmt.Sprintf("https://s3.%s.global.lyve.seagate.com", region)

	cfg, err := config.LoadDefaultConfig(context.Background(),
		config.WithCredentialsProvider(
			credentials.NewStaticCredentialsProvider(accessKey, secretKey, ""),
		),
		config.WithRegion(region),
	)
	if err != nil {
		return nil, fmt.Errorf("load config: %w", err)
	}

	return &LyveDriver{
		client:   s3.NewFromConfig(cfg, func(o *s3.Options) { o.BaseEndpoint = aws.String(endpoint); o.UsePathStyle = true }),
		tenantID: tenantID,
		region:   region,
		logger:   logger,
	}, nil
}

// getTenantID extracts tenant from context or uses default
func (d *LyveDriver) getTenantID(ctx context.Context) string {
	if tid := ctx.Value(common.TenantIDKey); tid != nil {
		if t, ok := tid.(string); ok {
			return t
		}
	}
	// If no tenant in context and no default, this is an error condition
	if d.tenantID == "" {
		// Log warning or panic - requests MUST have tenant context
		d.logger.Warn("no tenant ID in context or driver")
		return "default" // Fallback, but this shouldn't happen
	}
	return d.tenantID
}
func (d *LyveDriver) buildTenantKey(tenantID, container, artifact string) string {
	return fmt.Sprintf("t-%s/%s/%s", tenantID, container, artifact)
}

func (d *LyveDriver) getBucket() string {
	return fmt.Sprintf("stored-%s", d.region)
}

func (d *LyveDriver) Get(ctx context.Context, container, artifact string) (io.ReadCloser, error) {
	tenantID := d.getTenantID(ctx)
	key := d.buildTenantKey(tenantID, container, artifact)
	bucket := d.getBucket()

	result, err := d.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return nil, fmt.Errorf("get object: %w", err)
	}

	return result.Body, nil
}

func (d *LyveDriver) Put(ctx context.Context, container, artifact string, data io.Reader, opts ...engine.PutOption) error {
	tenantID := d.getTenantID(ctx)
	key := d.buildTenantKey(tenantID, container, artifact)
	bucket := d.getBucket()

	// Apply options
	options := &engine.PutOptions{}
	for _, opt := range opts {
		opt(options)
	}

	input := &s3.PutObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(key),
		Body:   data,
	}

	// Apply metadata from options
	if options.ContentType != "" {
		input.ContentType = aws.String(options.ContentType)
	}
	if options.CacheControl != "" {
		input.CacheControl = aws.String(options.CacheControl)
	}
	if options.ContentEncoding != "" {
		input.ContentEncoding = aws.String(options.ContentEncoding)
	}
	if options.ContentLanguage != "" {
		input.ContentLanguage = aws.String(options.ContentLanguage)
	}
	if len(options.UserMetadata) > 0 {
		input.Metadata = options.UserMetadata
	}

	_, err := d.client.PutObject(ctx, input)
	if err != nil {
		return fmt.Errorf("put object: %w", err)
	}

	return nil
}

func (d *LyveDriver) Delete(ctx context.Context, container, artifact string) error {
	tenantID := d.getTenantID(ctx)
	key := d.buildTenantKey(tenantID, container, artifact)
	bucket := d.getBucket()

	_, err := d.client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return fmt.Errorf("delete object: %w", err)
	}

	return nil
}

func (d *LyveDriver) List(ctx context.Context, container string, prefix string) ([]string, error) {
	tenantID := d.getTenantID(ctx)
	keyPrefix := fmt.Sprintf("t-%s/%s/", tenantID, container)
	if prefix != "" {
		keyPrefix = fmt.Sprintf("t-%s/%s/%s", tenantID, container, prefix)
	}
	bucket := d.getBucket()

	result, err := d.client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
		Bucket: aws.String(bucket),
		Prefix: aws.String(keyPrefix),
	})
	if err != nil {
		return nil, fmt.Errorf("list objects: %w", err)
	}

	// Strip the tenant/container prefix when returning keys
	// Use the tenantID we actually used, not d.tenantID
	tenantPrefix := fmt.Sprintf("t-%s/%s/", tenantID, container)
	keys := make([]string, 0, len(result.Contents))
	for _, obj := range result.Contents {
		if obj.Key != nil {
			// Remove the tenant/container prefix to return just the artifact name
			key := strings.TrimPrefix(*obj.Key, tenantPrefix)
			keys = append(keys, key)
		}
	}

	return keys, nil
}

func (d *LyveDriver) Name() string {
	return "lyve"
}

func (d *LyveDriver) HealthCheck(ctx context.Context) error {
	bucket := d.getBucket()
	_, err := d.client.HeadBucket(ctx, &s3.HeadBucketInput{
		Bucket: aws.String(bucket),
	})
	return err
}
