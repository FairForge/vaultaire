package drivers

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"sync"

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
//
// size is the known body length. Any value <= 0 is treated as "unknown" and
// falls back to streaming — PutOptions.ContentLength is zero when unset, and a
// genuinely empty object costs one PutObject through the uploader either way.
// Pass it whenever the caller has it: it lets objects that fit in one part skip
// the multipart round trips entirely (see putSinglePartIfSmall).
func s3ParallelUpload(ctx context.Context, client manager.UploadAPIClient, bucket, key, contentType string, body io.Reader, size int64) error {
	in := &s3.PutObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(key),
		Body:   body,
	}
	if contentType != "" {
		in.ContentType = aws.String(contentType)
	}
	if size > 0 {
		in.ContentLength = aws.Int64(size)
	}
	return s3ParallelUploadInput(ctx, client, in)
}

// s3ParallelUploadInput is the full-input variant of s3ParallelUpload for
// callers that need to carry extra object metadata (CacheControl,
// ContentEncoding, user metadata, ...) through the parallel uploader.
func s3ParallelUploadInput(ctx context.Context, client manager.UploadAPIClient, in *s3.PutObjectInput) error {
	if done, err := putSinglePartIfSmall(ctx, client, in); done {
		return err
	}

	uploader := uploaderFor(client)
	if _, err := uploader.Upload(ctx, in); err != nil { //nolint:staticcheck // manager.Uploader is deprecated in favor of transfermanager; migration is a post-launch WP
		return fmt.Errorf("s3 parallel upload %s/%s: %w", aws.ToString(in.Bucket), aws.ToString(in.Key), err)
	}
	return nil
}

// uploaderCache holds one manager.Uploader per client. The uploader owns a
// pooled set of part buffers that the SDK explicitly reuses *between* upload
// calls; constructing a fresh uploader per PUT threw that pool away every
// time and re-allocated Concurrency+1 part buffers (9 × 16 MiB) per object.
// manager.Uploader is safe for concurrent use.
var uploaderCache sync.Map // manager.UploadAPIClient -> *manager.Uploader

func uploaderFor(client manager.UploadAPIClient) *manager.Uploader { //nolint:staticcheck // see migration note above
	if u, ok := uploaderCache.Load(client); ok {
		return u.(*manager.Uploader) //nolint:staticcheck // see migration note above
	}
	created := manager.NewUploader(client, func(u *manager.Uploader) { //nolint:staticcheck // see migration note above
		u.PartSize = s3UploadPartSize
		u.Concurrency = s3UploadConcurrency
	})
	actual, _ := uploaderCache.LoadOrStore(client, created)
	return actual.(*manager.Uploader) //nolint:staticcheck // see migration note above
}

// putSinglePartIfSmall short-circuits objects that fit in one part into a
// single PutObject, and reports whether it handled the upload.
//
// Without this, an object of *exactly* PartSize costs three round trips
// (CreateMultipartUpload + UploadPart + CompleteMultipartUpload) because the
// SDK reads a full part off a non-seekable body, cannot tell the stream ended,
// and commits to multipart. The engine always hands drivers a non-seekable
// reader (http body → counting reader → TeeReader for the MD5 ETag), so every
// 16 MiB write paid that penalty — on a backend with ~40 ms RTT the two extra
// round trips dominate the transfer itself.
//
// Requires a known ContentLength. The body is read into one exactly-sized
// buffer rather than via io.ReadAll, whose doubling growth reallocates and
// recopies a 16 MiB body about fifteen times.
func putSinglePartIfSmall(ctx context.Context, client manager.UploadAPIClient, in *s3.PutObjectInput) (bool, error) {
	size := aws.ToInt64(in.ContentLength)
	if in.ContentLength == nil || size < 0 || size > s3UploadPartSize {
		return false, nil
	}

	buf := make([]byte, size)
	n, err := io.ReadFull(in.Body, buf)
	switch {
	case err == nil || errors.Is(err, io.EOF):
		// Exactly the declared length (or an empty body declared as 0).
	case errors.Is(err, io.ErrUnexpectedEOF):
		// Body was shorter than declared — upload what actually arrived
		// rather than sending a padded object.
		buf = buf[:n]
		in.ContentLength = aws.Int64(int64(n))
	default:
		return true, fmt.Errorf("s3 upload %s/%s: read body: %w",
			aws.ToString(in.Bucket), aws.ToString(in.Key), err)
	}

	// Deliberately NO read past ContentLength to check for a longer body: a
	// producer that has delivered exactly Content-Length but has not yet
	// signalled EOF would block that read forever and hang the upload — see
	// TestS3Upload_DoesNotBlockProbingPastContentLength, which caught this.
	// ContentLength is authoritative: HTTP frames the request body by it, the
	// S3 layer already rejects bodies whose measured length disagrees with the
	// declared one, and the SDK would itself send exactly ContentLength bytes.
	in.Body = bytes.NewReader(buf)
	if _, err := client.PutObject(ctx, in); err != nil {
		return true, fmt.Errorf("s3 upload %s/%s: %w",
			aws.ToString(in.Bucket), aws.ToString(in.Key), err)
	}
	return true, nil
}
