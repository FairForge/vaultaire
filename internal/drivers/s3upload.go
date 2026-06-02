package drivers

import (
	"context"
	"fmt"
	"io"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/feature/s3/manager"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// Parallel multipart upload tuning for S3-compatible backends.
//
// A plain PutObject streams an object over a single connection; for a large file
// that caps throughput at one stream. manager.Uploader instead splits objects
// larger than the part size into parts and uploads up to s3UploadConcurrency of
// them in parallel, which materially speeds large single-file uploads. Objects
// smaller than the part size still go as one PutObject. Parts are read on the fly,
// so there is no full-object buffering.
//
// We deliberately use feature/s3/manager (stable v1, battle-tested) rather than
// feature/s3/transfermanager, which is still pre-1.0 (v0.x) and too unstable for
// the write path. The manager *module* carries a forward-looking deprecation
// notice, but its symbols are not individually deprecated, so this is not flagged
// by staticcheck.
const (
	s3UploadPartSize    = 16 << 20 // 16 MiB per part
	s3UploadConcurrency = 8        // parallel parts in flight
)

// s3ParallelUpload uploads body to bucket/key using the SDK's parallel multipart
// uploader. contentType is optional. client is the manager.UploadAPIClient
// interface, which *s3.Client satisfies (and tests can mock). On any failure the
// uploader aborts the multipart upload so no orphaned parts are billed.
func s3ParallelUpload(ctx context.Context, client manager.UploadAPIClient, bucket, key, contentType string, body io.Reader) error {
	uploader := manager.NewUploader(client, func(u *manager.Uploader) {
		u.PartSize = s3UploadPartSize
		u.Concurrency = s3UploadConcurrency
	})

	in := &s3.PutObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(key),
		Body:   body,
	}
	if contentType != "" {
		in.ContentType = aws.String(contentType)
	}

	if _, err := uploader.Upload(ctx, in); err != nil {
		return fmt.Errorf("s3 parallel upload %s/%s: %w", bucket, key, err)
	}
	return nil
}
