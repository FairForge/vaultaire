// internal/drivers/geyser.go
//
// Geyser storage driver for Spectra Logic Vail (LTO-9 tape).
//
// Key constraint: Vail requires Content-Length on every PUT (HTTP 411
// otherwise). The engine wraps bodies in TeeReader chains that strip
// length info, so we must materialise the data before uploading.
//
// For objects <= spillThreshold (64 MB) we buffer in RAM.
// For larger objects we spill to a temp file on disk.
package drivers

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/FairForge/vaultaire/internal/common"
	"github.com/FairForge/vaultaire/internal/engine"
	"github.com/aws/aws-sdk-go-v2/aws"
	awshttp "github.com/aws/aws-sdk-go-v2/aws/transport/http"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"go.uber.org/zap"
)

const (
	// spillThreshold is the maximum size buffered in RAM. Objects larger
	// than this are spilled to a temporary file to avoid OOM on GB+
	// archive uploads.
	spillThreshold = 64 << 20 // 64 MB
)

// GeyserDriver implements the engine.Driver interface for Geyser's
// S3-compatible tape storage (Spectra Logic Vail).
type GeyserDriver struct {
	client   *s3.Client
	bucket   string
	tenantID string
	logger   *zap.Logger
	endpoint string
}

// GeyserOption configures a GeyserDriver.
type GeyserOption func(*geyserOpts)

type geyserOpts struct {
	endpoint string
}

// WithGeyserEndpoint overrides the default LA endpoint.
// Use "https://lon1.geyserdata.com" for London.
func WithGeyserEndpoint(ep string) GeyserOption {
	return func(o *geyserOpts) { o.endpoint = ep }
}

func NewGeyserDriver(accessKey, secretKey, bucket, tenantID string, logger *zap.Logger, options ...GeyserOption) (*GeyserDriver, error) {
	opts := &geyserOpts{
		endpoint: "https://la1.geyserdata.com",
	}
	for _, o := range options {
		o(opts)
	}

	// Tape operations can be slow — use generous timeouts.
	httpClient := awshttp.NewBuildableClient().WithTransportOptions(func(t *http.Transport) {
		t.ResponseHeaderTimeout = 5 * time.Minute
		t.MaxIdleConnsPerHost = 10
		t.IdleConnTimeout = 90 * time.Second
	})

	cfg, err := config.LoadDefaultConfig(context.Background(),
		config.WithCredentialsProvider(
			credentials.NewStaticCredentialsProvider(accessKey, secretKey, ""),
		),
		config.WithRegion("us-west-2"),
		config.WithHTTPClient(httpClient),
	)
	if err != nil {
		return nil, fmt.Errorf("load config: %w", err)
	}

	return &GeyserDriver{
		client: s3.NewFromConfig(cfg, func(o *s3.Options) {
			o.BaseEndpoint = aws.String(opts.endpoint)
			o.UsePathStyle = true // Geyser requires path-style
		}),
		bucket:   bucket,
		tenantID: tenantID,
		logger:   logger,
		endpoint: opts.endpoint,
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

	// Geyser's Spectra Vail gateway requires Content-Length on every PUT
	// (HTTP 411 otherwise). We materialise the data to know the size.
	body, contentLength, cleanup, err := materialize(data)
	if err != nil {
		return fmt.Errorf("geyser buffer %s: %w", key, err)
	}
	defer cleanup()

	d.logger.Debug("geyser put",
		zap.String("tenant_id", tenantID),
		zap.String("bucket", d.bucket),
		zap.String("key", key),
		zap.Int64("size", contentLength),
	)

	_, err = d.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:        aws.String(d.bucket),
		Key:           aws.String(key),
		Body:          body,
		ContentLength: &contentLength,
	})
	if err != nil {
		return fmt.Errorf("geyser put %s: %w", key, err)
	}
	return nil
}

// materialize reads the entire body to determine its length (required by
// Geyser/Spectra Vail). Small objects (<=64 MB) are buffered in RAM;
// larger objects are spilled to a temp file to avoid OOM.
func materialize(data io.Reader) (body io.ReadSeeker, size int64, cleanup func(), err error) {
	buf := &bytes.Buffer{}
	n, err := io.CopyN(buf, data, spillThreshold+1)
	if err != nil && err != io.EOF {
		return nil, 0, func() {}, fmt.Errorf("reading body: %w", err)
	}

	if n <= spillThreshold {
		// Fits in memory.
		return bytes.NewReader(buf.Bytes()), n, func() {}, nil
	}

	// Large object — spill to temp file.
	f, err := os.CreateTemp("", "geyser-put-*")
	if err != nil {
		return nil, 0, func() {}, fmt.Errorf("create temp file: %w", err)
	}
	cleanupFn := func() {
		_ = f.Close()
		_ = os.Remove(f.Name())
	}

	// Write what we already buffered, then stream the rest.
	if _, err := f.Write(buf.Bytes()); err != nil {
		cleanupFn()
		return nil, 0, func() {}, fmt.Errorf("write temp: %w", err)
	}
	written, err := io.Copy(f, data)
	if err != nil {
		cleanupFn()
		return nil, 0, func() {}, fmt.Errorf("spill to disk: %w", err)
	}
	total := n + written

	if _, err := f.Seek(0, io.SeekStart); err != nil {
		cleanupFn()
		return nil, 0, func() {}, fmt.Errorf("seek temp: %w", err)
	}

	return f, total, cleanupFn, nil
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
