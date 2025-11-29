// internal/drivers/geyser.go
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

type GeyserDriver struct {
	client   *s3.Client
	bucket   string
	tenantID string
	logger   *zap.Logger
}

func NewGeyserDriver(accessKey, secretKey, bucket, tenantID string, logger *zap.Logger) (*GeyserDriver, error) {
	endpoint := "https://la1.geyserdata.com"

	cfg, err := config.LoadDefaultConfig(context.Background(),
		config.WithCredentialsProvider(
			credentials.NewStaticCredentialsProvider(accessKey, secretKey, ""),
		),
		config.WithRegion("us-west-2"), // Geyser uses us-west-2
	)
	if err != nil {
		return nil, fmt.Errorf("load config: %w", err)
	}

	return &GeyserDriver{
		client: s3.NewFromConfig(cfg, func(o *s3.Options) {
			o.BaseEndpoint = aws.String(endpoint)
			o.UsePathStyle = true // Geyser requires path-style
		}),
		bucket:   bucket,
		tenantID: tenantID,
		logger:   logger,
	}, nil
}

func (d *GeyserDriver) getTenantID(ctx context.Context) string {
	if tid := ctx.Value(common.TenantIDKey); tid != nil {
		if s, ok := tid.(string); ok && s != "" {
			return s
		}
	}
	return d.tenantID
}

func (d *GeyserDriver) buildKey(tenantID, container, artifact string) string {
	return fmt.Sprintf("t-%s/%s/%s", tenantID, container, artifact)
}

func (d *GeyserDriver) Get(ctx context.Context, container, artifact string) (io.ReadCloser, error) {
	tenantID := d.getTenantID(ctx)
	key := d.buildKey(tenantID, container, artifact)

	resp, err := d.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(d.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return nil, fmt.Errorf("geyser get %s: %w", key, err)
	}
	return resp.Body, nil
}

func (d *GeyserDriver) Put(ctx context.Context, container, artifact string, data io.Reader, opts ...engine.PutOption) error {
	tenantID := d.getTenantID(ctx)
	key := d.buildKey(tenantID, container, artifact)

	d.logger.Debug("geyser put",
		zap.String("tenant_id", tenantID),
		zap.String("bucket", d.bucket),
		zap.String("key", key),
	)

	_, err := d.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(d.bucket),
		Key:    aws.String(key),
		Body:   data,
	})
	if err != nil {
		return fmt.Errorf("geyser put %s: %w", key, err)
	}
	return nil
}

func (d *GeyserDriver) Delete(ctx context.Context, container, artifact string) error {
	tenantID := d.getTenantID(ctx)
	key := d.buildKey(tenantID, container, artifact)

	_, err := d.client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(d.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return fmt.Errorf("geyser delete %s: %w", key, err)
	}
	return nil
}

func (d *GeyserDriver) List(ctx context.Context, container string, prefix string) ([]string, error) {
	tenantID := d.getTenantID(ctx)
	fullPrefix := d.buildKey(tenantID, container, prefix)

	resp, err := d.client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
		Bucket: aws.String(d.bucket),
		Prefix: aws.String(fullPrefix),
	})
	if err != nil {
		return nil, fmt.Errorf("geyser list: %w", err)
	}

	var artifacts []string
	basePrefix := d.buildKey(tenantID, container, "")
	for _, obj := range resp.Contents {
		name := strings.TrimPrefix(*obj.Key, basePrefix)
		artifacts = append(artifacts, name)
	}
	return artifacts, nil
}

func (d *GeyserDriver) Name() string {
	return "geyser"
}

func (d *GeyserDriver) HealthCheck(ctx context.Context) error {
	_, err := d.client.HeadBucket(ctx, &s3.HeadBucketInput{
		Bucket: aws.String(d.bucket),
	})
	if err != nil {
		return fmt.Errorf("geyser health check: %w", err)
	}
	return nil
}
