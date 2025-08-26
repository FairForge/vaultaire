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

// QuotalessDriver implements storage.Backend for Quotaless S3-compatible API
type QuotalessDriver struct {
    client   *s3.Client
    bucket   string
    prefix   string
    logger   *zap.Logger
}

// NewQuotalessDriver creates a new Quotaless storage driver
func NewQuotalessDriver(accessKey, secretKey string, logger *zap.Logger) (*QuotalessDriver, error) {
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
        BaseEndpoint: aws.String("https://us.quotaless.cloud:8000"),
        Region:       "us-east-1",
        Credentials: credentials.NewStaticCredentialsProvider(
            accessKey,
            secretKey,
            "",
        ),
        UsePathStyle: true,
        HTTPClient:   httpClient,
    })
    
    return &QuotalessDriver{
        client: client,
        bucket: "data",
        prefix: "personal-files/vaultaire",
        logger: logger,
    }, nil
}

// buildKey constructs the full S3 key with prefix
func (d *QuotalessDriver) buildKey(container, artifact string) string {
    if artifact == "" {
        return path.Join(d.prefix, container)
    }
    return path.Join(d.prefix, container, artifact)
}

// Get retrieves an artifact
func (d *QuotalessDriver) Get(ctx context.Context, container, artifact string) (io.ReadCloser, error) {
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
func (d *QuotalessDriver) Put(ctx context.Context, container, artifact string, data io.Reader) error {
    key := d.buildKey(container, artifact)
    
    _, err := d.client.PutObject(ctx, &s3.PutObjectInput{
        Bucket: aws.String(d.bucket),
        Key:    aws.String(key),
        Body:   data,
    })
    if err != nil {
        return fmt.Errorf("put object %s: %w", key, err)
    }
    
    d.logger.Debug("stored artifact in Quotaless",
        zap.String("key", key),
        zap.String("bucket", d.bucket))
    
    return nil
}

// Delete removes an artifact
func (d *QuotalessDriver) Delete(ctx context.Context, container, artifact string) error {
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
func (d *QuotalessDriver) List(ctx context.Context, container string) ([]string, error) {
    prefix := d.buildKey(container, "") + "/"
    
    result, err := d.client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
        Bucket: aws.String(d.bucket),
        Prefix: aws.String(prefix),
    })
    if err != nil {
        return nil, fmt.Errorf("list objects with prefix %s: %w", prefix, err)
    }
    
    var artifacts []string
    prefixLen := len(prefix)
    
    for _, obj := range result.Contents {
        if obj.Key != nil && len(*obj.Key) > prefixLen {
            // Remove the prefix to get just the artifact name
            artifactPath := (*obj.Key)[prefixLen:]
            artifacts = append(artifacts, artifactPath)
        }
    }
    
    return artifacts, nil
}

// HealthCheck verifies connectivity
func (d *QuotalessDriver) HealthCheck(ctx context.Context) error {
    // Try to list the bucket root - this should always work
    _, err := d.client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
        Bucket:  aws.String(d.bucket),
        Prefix:  aws.String(d.prefix),
        MaxKeys: aws.Int32(1),
    })
    
    if err != nil {
        return fmt.Errorf("health check failed: %w", err)
    }
    
    d.logger.Debug("Quotaless health check passed")
    return nil
}
