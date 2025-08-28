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
		o.UsePathStyle = false // Lyve Cloud uses virtual hosted-style
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

// Delete removes an object from S3
func (d *S3Driver) Delete(ctx context.Context, container, artifact string) error {
	_, err := d.client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(container),
		Key:    aws.String(artifact),
	})
	if err != nil {
		return fmt.Errorf("delete object %s/%s: %w", container, artifact, err)
	}
	return nil
}

// List returns objects in a container with optional prefix
func (d *S3Driver) List(ctx context.Context, container, prefix string) ([]string, error) {
	input := &s3.ListObjectsV2Input{
		Bucket: aws.String(container),
	}
	if prefix != "" {
		input.Prefix = aws.String(prefix)
	}
	
	result, err := d.client.ListObjectsV2(ctx, input)
	if err != nil {
		return nil, fmt.Errorf("list objects in %s: %w", container, err)
	}
	
	var keys []string
	for _, obj := range result.Contents {
		keys = append(keys, *obj.Key)
	}
	return keys, nil
}
