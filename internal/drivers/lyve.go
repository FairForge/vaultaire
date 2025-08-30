package drivers

import (
	"context"
	"fmt"
	"io"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"go.uber.org/zap"
)

// LyveDriver implements multi-tenant storage on Seagate Lyve Cloud
// Architecture Decision: 7 shared buckets (one per region) with tenant prefixes
// WHY: Lyve doesn't charge per bucket, only per TB stored
type LyveDriver struct {
	client   *s3.Client
	tenantID string // CRITICAL: Isolates tenant data
	region   string
	logger   *zap.Logger
}

// buildTenantKey ensures ALL operations are prefixed with tenant ID
// Security: Prevents cross-tenant data access
func (d *LyveDriver) buildTenantKey(artifact string) string {
	return fmt.Sprintf("t-%s/%s", d.tenantID, artifact)
}

// getBucket returns the regional bucket name
// Business Logic: 7 total buckets across all customers
func (d *LyveDriver) getBucket() string {
	return fmt.Sprintf("stored-%s", d.region)
}

func NewLyveDriver(accessKey, secretKey, tenantID, region string, logger *zap.Logger) (*LyveDriver, error) {
	// Lyve endpoint format from their docs
	endpoint := fmt.Sprintf("https://s3.%s.lyvecloud.seagate.com", region)

	cfg, err := config.LoadDefaultConfig(context.Background(),
		config.WithCredentialsProvider(
			credentials.NewStaticCredentialsProvider(accessKey, secretKey, ""),
		),
		config.WithRegion(region),
		config.WithEndpointResolverWithOptions(aws.EndpointResolverWithOptionsFunc(
			func(service, region string, options ...interface{}) (aws.Endpoint, error) {
				return aws.Endpoint{
					URL:           endpoint,
					SigningRegion: region,
				}, nil
			},
		)),
	)
	if err != nil {
		return nil, fmt.Errorf("load config: %w", err)
	}

	return &LyveDriver{
		client:   s3.NewFromConfig(cfg),
		tenantID: tenantID,
		region:   region,
		logger:   logger,
	}, nil
}

func (d *LyveDriver) Get(ctx context.Context, container, artifact string) (io.ReadCloser, error) {
	key := d.buildTenantKey(fmt.Sprintf("%s/%s", container, artifact))
	result, err := d.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(d.getBucket()),
		Key:    aws.String(key),
	})
	if err != nil {
		return nil, fmt.Errorf("get object: %w", err)
	}
	return result.Body, nil
}

func (d *LyveDriver) Put(ctx context.Context, container, artifact string, data io.Reader) error {
	key := d.buildTenantKey(fmt.Sprintf("%s/%s", container, artifact))
	_, err := d.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(d.getBucket()),
		Key:    aws.String(key),
		Body:   data,
	})
	return err
}

func (d *LyveDriver) Delete(ctx context.Context, container, artifact string) error {
	key := d.buildTenantKey(fmt.Sprintf("%s/%s", container, artifact))
	_, err := d.client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(d.getBucket()),
		Key:    aws.String(key),
	})
	return err
}

func (d *LyveDriver) List(ctx context.Context, container, prefix string) ([]string, error) {
	fullPrefix := d.buildTenantKey(fmt.Sprintf("%s/%s", container, prefix))
	result, err := d.client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
		Bucket: aws.String(d.getBucket()),
		Prefix: aws.String(fullPrefix),
	})
	if err != nil {
		return nil, err
	}

	var artifacts []string
	for _, obj := range result.Contents {
		artifacts = append(artifacts, *obj.Key)
	}
	return artifacts, nil
}
