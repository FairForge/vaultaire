// internal/drivers/lyve.go
package drivers

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/FairForge/vaultaire/internal/engine"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"go.uber.org/zap"
)

type LyveDriver struct {
	client   *s3.Client
	tenantID string
	region   string
	logger   *zap.Logger
}

func NewLyveDriver(accessKey, secretKey, tenantID, region string, logger *zap.Logger) (*LyveDriver, error) {
	if region == "" {
		region = "us-east-1"
	}

	endpoint := fmt.Sprintf("https://s3.%s.lyvecloud.seagate.com", region)

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

func (d *LyveDriver) buildTenantKey(container, artifact string) string {
	return fmt.Sprintf("t-%s/%s/%s", d.tenantID, container, artifact)
}

func (d *LyveDriver) getBucket() string {
	return fmt.Sprintf("stored-%s", d.region)
}

func (d *LyveDriver) Get(ctx context.Context, container, artifact string) (io.ReadCloser, error) {
	key := d.buildTenantKey(container, artifact)
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
	key := d.buildTenantKey(container, artifact)
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
	key := d.buildTenantKey(container, artifact)
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
	keyPrefix := d.buildTenantKey(container, prefix)
	bucket := d.getBucket()

	result, err := d.client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
		Bucket: aws.String(bucket),
		Prefix: aws.String(keyPrefix),
	})
	if err != nil {
		return nil, fmt.Errorf("list objects: %w", err)
	}

	var artifacts []string
	for _, obj := range result.Contents {
		key := aws.ToString(obj.Key)
		// Remove tenant/container prefix to get artifact name
		parts := strings.Split(key, "/")
		if len(parts) >= 3 {
			artifact := strings.Join(parts[2:], "/")
			artifacts = append(artifacts, artifact)
		}
	}

	return artifacts, nil
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
