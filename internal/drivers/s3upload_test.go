package drivers

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"sync"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// mockUploadClient implements manager.UploadAPIClient, recording calls and
// capturing uploaded bytes so tests can verify integrity. Thread-safe because the
// uploader calls UploadPart from parallel goroutines.
type mockUploadClient struct {
	mu sync.Mutex

	putObjectCalls int
	createCalls    int
	uploadParts    int
	completeCalls  int

	putBody []byte
	parts   map[int32][]byte
	putErr  error
}

func newMockUploadClient() *mockUploadClient {
	return &mockUploadClient{parts: make(map[int32][]byte)}
}

func (m *mockUploadClient) PutObject(_ context.Context, in *s3.PutObjectInput, _ ...func(*s3.Options)) (*s3.PutObjectOutput, error) {
	b, _ := io.ReadAll(in.Body)
	m.mu.Lock()
	defer m.mu.Unlock()
	m.putObjectCalls++
	m.putBody = b
	if m.putErr != nil {
		return nil, m.putErr
	}
	return &s3.PutObjectOutput{}, nil
}

func (m *mockUploadClient) CreateMultipartUpload(_ context.Context, _ *s3.CreateMultipartUploadInput, _ ...func(*s3.Options)) (*s3.CreateMultipartUploadOutput, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.createCalls++
	return &s3.CreateMultipartUploadOutput{UploadId: aws.String("test-upload-id")}, nil
}

func (m *mockUploadClient) UploadPart(_ context.Context, in *s3.UploadPartInput, _ ...func(*s3.Options)) (*s3.UploadPartOutput, error) {
	b, _ := io.ReadAll(in.Body)
	m.mu.Lock()
	defer m.mu.Unlock()
	m.uploadParts++
	m.parts[*in.PartNumber] = b
	return &s3.UploadPartOutput{ETag: aws.String(fmt.Sprintf("\"etag-%d\"", *in.PartNumber))}, nil
}

func (m *mockUploadClient) CompleteMultipartUpload(_ context.Context, _ *s3.CompleteMultipartUploadInput, _ ...func(*s3.Options)) (*s3.CompleteMultipartUploadOutput, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.completeCalls++
	return &s3.CompleteMultipartUploadOutput{}, nil
}

func (m *mockUploadClient) AbortMultipartUpload(_ context.Context, _ *s3.AbortMultipartUploadInput, _ ...func(*s3.Options)) (*s3.AbortMultipartUploadOutput, error) {
	return &s3.AbortMultipartUploadOutput{}, nil
}

// reassemble concatenates the multipart parts in part-number order.
func (m *mockUploadClient) reassemble() []byte {
	m.mu.Lock()
	defer m.mu.Unlock()
	nums := make([]int, 0, len(m.parts))
	for n := range m.parts {
		nums = append(nums, int(n))
	}
	sort.Ints(nums)
	var out []byte
	for _, n := range nums {
		out = append(out, m.parts[int32(n)]...)
	}
	return out
}

func TestS3ParallelUpload_SmallFile_SinglePut(t *testing.T) {
	data := bytes.Repeat([]byte("a"), 1<<20) // 1 MiB < 16 MiB part size
	m := newMockUploadClient()

	err := s3ParallelUpload(context.Background(), m, "bucket", "key", "text/plain", bytes.NewReader(data))

	require.NoError(t, err)
	assert.Equal(t, 1, m.putObjectCalls, "small file should be a single PutObject")
	assert.Equal(t, 0, m.createCalls, "small file should not start a multipart upload")
	assert.Equal(t, data, m.putBody, "uploaded bytes must match input")
}

func TestS3ParallelUpload_LargeFile_ParallelMultipart(t *testing.T) {
	data := bytes.Repeat([]byte("x"), 40<<20) // 40 MiB > 16 MiB → 3 parts
	m := newMockUploadClient()

	err := s3ParallelUpload(context.Background(), m, "bucket", "key", "", bytes.NewReader(data))

	require.NoError(t, err)
	assert.Equal(t, 0, m.putObjectCalls, "large file should not use a single PutObject")
	assert.Equal(t, 1, m.createCalls)
	assert.GreaterOrEqual(t, m.uploadParts, 3, "40 MiB / 16 MiB → at least 3 parts")
	assert.Equal(t, 1, m.completeCalls)
	assert.Equal(t, data, m.reassemble(), "reassembled parts must match input (integrity)")
}

func TestS3ParallelUpload_Error_Wrapped(t *testing.T) {
	m := newMockUploadClient()
	m.putErr = errors.New("backend down")

	err := s3ParallelUpload(context.Background(), m, "bucket", "key", "", bytes.NewReader([]byte("small")))

	require.Error(t, err)
	assert.Contains(t, err.Error(), "s3 parallel upload bucket/key")
	assert.Contains(t, err.Error(), "backend down")
}
