package drivers

import (
	"context"
	"fmt"

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
