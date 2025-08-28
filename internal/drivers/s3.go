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

// S3Driver implements storage.Backend for S3-compatible storage
type S3Driver struct {
	endpoint  string
	accessKey string
	secretKey string
	region    string
	logger    *zap.Logger
	client    *s3.Client
}

// NewS3Driver creates a new S3 storage driver
func NewS3Driver(endpoint, accessKey, secretKey, region string, logger *zap.Logger) (*S3Driver, error) {
	// Create custom credentials provider
	creds := credentials.NewStaticCredentialsProvider(accessKey, secretKey, "")
	
	// Create config - use us-east-1 for Lyve Cloud regardless of actual region
	cfg, err := config.LoadDefaultConfig(context.TODO(),
		config.WithCredentialsProvider(creds),
		config.WithRegion("us-east-1"), // Lyve Cloud requires us-east-1 for signature
	)
	if err != nil {
		return nil, fmt.Errorf("load config: %w", err)
	}
	
	// Create S3 client with custom endpoint
	client := s3.NewFromConfig(cfg, func(o *s3.Options) {
		o.BaseEndpoint = aws.String(endpoint)
		o.UsePathStyle = false  // Lyve Cloud uses virtual hosted-style
	})
	
	return &S3Driver{
		endpoint:  endpoint,
		accessKey: accessKey,
		secretKey: secretKey,
		region:    region,
		logger:    logger,
		client:    client,
	}, nil
}

// Put stores data in S3
func (d *S3Driver) Put(ctx context.Context, container, artifact string, data io.Reader) error {
	_, err := d.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(container),
		Key:    aws.String(artifact),
		Body:   data,
	})
	if err != nil {
		return fmt.Errorf("put object %s/%s: %w", container, artifact, err)
	}
	return nil
}

// Get retrieves data from S3
func (d *S3Driver) Get(ctx context.Context, container, artifact string) (io.ReadCloser, error) {
	result, err := d.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(container),
		Key:    aws.String(artifact),
	})
	if err != nil {
		return nil, fmt.Errorf("get object %s/%s: %w", container, artifact, err)
	}
	return result.Body, nil
}
