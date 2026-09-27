package drivers

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Review R7-12: above one part the SDK uploader ignores ContentLength on a
// non-seekable body and commits whatever the reader delivered before EOF. The
// guard today is that Go's server body reports a short Content-Length body as
// io.ErrUnexpectedEOF, which the uploader treats as fatal and aborts. Pin that
// behaviour: a body that dies after more than one part must never complete.
func TestS3Upload_MultipartShortBodyAborts(t *testing.T) {
	client := newMockUploadClient()
	declared := int64(2*s3UploadPartSize + 1024)
	delivered := make([]byte, s3UploadPartSize+4096) // > one part, < declared
	body := io.MultiReader(bytes.NewReader(delivered), errReader{err: io.ErrUnexpectedEOF})

	err := s3ParallelUpload(context.Background(), client, "b", "k", "", body, declared)

	require.Error(t, err)
	assert.True(t, errors.Is(err, io.ErrUnexpectedEOF), "short-body cause must survive wrapping: %v", err)
	assert.Equal(t, 0, client.putObjectCalls, "not a single PutObject")
	assert.Equal(t, 0, client.completeCalls, "the multipart upload must not be completed")
	assert.Equal(t, 1, client.createCalls, "it did go multipart (declared > one part)")
}
