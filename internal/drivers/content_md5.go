package drivers

import (
	"context"
	"crypto/md5" // #nosec G501 -- Content-MD5 is the S3 integrity header the backend verifies, never a security primitive
	"encoding/base64"
	"fmt"
	"io"

	"github.com/aws/smithy-go/middleware"
	smithyhttp "github.com/aws/smithy-go/transport/http"
)

// Content-MD5 on every PutObject and UploadPart, for a backend that verifies
// nothing else the SDK sends (2026-11-02,
// bench-results/LYVE-WORKER-2026-11-02.md § Follow-up and
// IDRIVE-WORKER-2026-11-02.md § integrity): over HTTPS the Go SDK sends a
// PutObject or UploadPart as aws-chunked with an unsigned payload and a
// CRC32 trailer, and Lyve stores a body under a WRONG CRC32 or SHA-256
// trailer (and a wrong CRC header). It refuses a wrong Content-MD5 with 400
// BadDigest, with the trailer still on, so this header is the one check the
// backend applies to the bytes the origin writes.
//
// The driver's two write paths both hand the SDK a seekable body —
// putSinglePartIfSmall a bytes.Reader, manager.Uploader its pooled part
// buffers — so the body is hashed and rewound before it is sent; the cost
// is one more MD5 pass over each part (measured from SLC, 32 MiB × 8, a
// 1 GiB upload: inside the run-to-run noise). A body that cannot be
// rewound, or a request that already names Content-MD5 (an edge presign
// signs the client's), passes untouched; a request with no body (a
// presign, a Create or a Complete) has nothing to hash.

const contentMD5MiddlewareID = "StoredContentMD5"

// contentMD5Middleware sets Content-MD5 on a PutObject or UploadPart whose
// body is seekable and that names none.
var contentMD5Middleware = middleware.BuildMiddlewareFunc(contentMD5MiddlewareID, func(
	ctx context.Context, in middleware.BuildInput, next middleware.BuildHandler,
) (middleware.BuildOutput, middleware.Metadata, error) {
	switch middleware.GetOperationName(ctx) {
	case "PutObject", "UploadPart":
	default:
		return next.HandleBuild(ctx, in)
	}
	req, ok := in.Request.(*smithyhttp.Request)
	if !ok || req.Header.Get("Content-MD5") != "" || req.GetStream() == nil || !req.IsStreamSeekable() {
		return next.HandleBuild(ctx, in)
	}
	sum, err := streamMD5(req.GetStream())
	if err != nil {
		return middleware.BuildOutput{}, middleware.Metadata{}, fmt.Errorf("content-md5: %w", err)
	}
	req.Header.Set("Content-MD5", sum)
	return next.HandleBuild(ctx, in)
})

// streamMD5 is the base64 MD5 of a seekable stream from its current offset,
// which it is rewound to.
func streamMD5(stream io.Reader) (string, error) {
	s, ok := stream.(io.Seeker)
	if !ok {
		return "", fmt.Errorf("the body is not seekable")
	}
	start, err := s.Seek(0, io.SeekCurrent)
	if err != nil {
		return "", fmt.Errorf("read the body's offset: %w", err)
	}
	h := md5.New() // #nosec G401 -- see the import
	if _, err := io.Copy(h, stream); err != nil {
		return "", fmt.Errorf("hash the body: %w", err)
	}
	if _, err := s.Seek(start, io.SeekStart); err != nil {
		return "", fmt.Errorf("rewind the body: %w", err)
	}
	return base64.StdEncoding.EncodeToString(h.Sum(nil)), nil
}

// withContentMD5 adds contentMD5Middleware to an S3 client's stack.
func withContentMD5(stack *middleware.Stack) error {
	return stack.Build.Add(contentMD5Middleware, middleware.After)
}
