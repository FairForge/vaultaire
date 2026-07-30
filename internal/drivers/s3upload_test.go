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

	err := s3ParallelUpload(context.Background(), m, "bucket", "key", "text/plain", bytes.NewReader(data), -1)

	require.NoError(t, err)
	assert.Equal(t, 1, m.putObjectCalls, "small file should be a single PutObject")
	assert.Equal(t, 0, m.createCalls, "small file should not start a multipart upload")
	assert.Equal(t, data, m.putBody, "uploaded bytes must match input")
}

func TestS3ParallelUpload_LargeFile_ParallelMultipart(t *testing.T) {
	data := bytes.Repeat([]byte("x"), 40<<20) // 40 MiB > 16 MiB → 3 parts
	m := newMockUploadClient()

	err := s3ParallelUpload(context.Background(), m, "bucket", "key", "", bytes.NewReader(data), -1)

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

	err := s3ParallelUpload(context.Background(), m, "bucket", "key", "", bytes.NewReader([]byte("small")), -1)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "s3 parallel upload bucket/key")
	assert.Contains(t, err.Error(), "backend down")
}

// --- Write-path efficiency: exactly-part-size objects (WP-EngineWrite) ---

// nonSeekableReader hides Seek/ReadAt so the SDK takes its streaming path —
// this is what the engine actually hands drivers (http body → counting →
// TeeReader for the MD5 ETag), and it behaves very differently from the
// bytes.Reader a direct benchmark passes.
type nonSeekableReader struct{ r io.Reader }

func (n nonSeekableReader) Read(p []byte) (int, error) { return n.r.Read(p) }

func TestS3Upload_ExactPartSizeObjectUsesSinglePut(t *testing.T) {
	// A 16 MiB object is exactly s3UploadPartSize. With an unknown-length,
	// non-seekable body the SDK cannot tell it is the whole object, so it
	// commits to multipart: Create + UploadPart + Complete = 3 round trips
	// where 1 PutObject would do. Passing ContentLength must collapse it.
	body := bytes.Repeat([]byte("x"), s3UploadPartSize)
	m := newMockUploadClient()

	in := &s3.PutObjectInput{
		Bucket:        aws.String("b"),
		Key:           aws.String("k"),
		Body:          nonSeekableReader{bytes.NewReader(body)},
		ContentLength: aws.Int64(int64(len(body))),
	}
	require.NoError(t, s3ParallelUploadInput(context.Background(), m, in))

	assert.Equal(t, 1, m.putObjectCalls, "exact-part-size object should be a single PutObject")
	assert.Equal(t, 0, m.createCalls, "should not open a multipart upload")
	assert.Equal(t, 0, m.completeCalls)
	assert.Equal(t, body, m.putBody, "bytes must round-trip intact")
}

func TestS3Upload_BelowPartSizeStillSinglePut(t *testing.T) {
	body := bytes.Repeat([]byte("y"), 5<<20)
	m := newMockUploadClient()

	in := &s3.PutObjectInput{
		Bucket:        aws.String("b"),
		Key:           aws.String("k"),
		Body:          nonSeekableReader{bytes.NewReader(body)},
		ContentLength: aws.Int64(int64(len(body))),
	}
	require.NoError(t, s3ParallelUploadInput(context.Background(), m, in))

	assert.Equal(t, 1, m.putObjectCalls)
	assert.Equal(t, 0, m.createCalls)
	assert.Equal(t, body, m.putBody)
}

func TestS3Upload_AbovePartSizeStillMultipart(t *testing.T) {
	// Genuinely large objects must keep the parallel multipart path — that is
	// what makes big uploads fast. 40 MiB over a 16 MiB part size = 3 parts.
	body := bytes.Repeat([]byte("z"), 40<<20)
	m := newMockUploadClient()

	in := &s3.PutObjectInput{
		Bucket:        aws.String("b"),
		Key:           aws.String("k"),
		Body:          nonSeekableReader{bytes.NewReader(body)},
		ContentLength: aws.Int64(int64(len(body))),
	}
	require.NoError(t, s3ParallelUploadInput(context.Background(), m, in))

	assert.Equal(t, 0, m.putObjectCalls, "large object must not collapse to PutObject")
	assert.Equal(t, 1, m.createCalls)
	assert.Equal(t, 3, m.uploadParts)
	assert.Equal(t, body, m.reassemble())
}

func TestS3Upload_UnknownLengthFallsBackToStreaming(t *testing.T) {
	// No ContentLength: behaviour must stay correct even if not optimal.
	body := bytes.Repeat([]byte("w"), 3<<20)
	m := newMockUploadClient()

	in := &s3.PutObjectInput{
		Bucket: aws.String("b"),
		Key:    aws.String("k"),
		Body:   nonSeekableReader{bytes.NewReader(body)},
	}
	require.NoError(t, s3ParallelUploadInput(context.Background(), m, in))

	assert.Equal(t, 1, m.putObjectCalls)
	assert.Equal(t, body, m.putBody)
}

// TestDrivers_PropagateContentLength guards the plumbing that makes the
// single-part fast path reachable. The S3 API adapter already passes
// engine.WithContentLength(size) on every PUT; if a driver drops it, every
// exactly-part-size object silently costs three round trips instead of one.
func TestS3Upload_KnownSizeAvoidsMultipartRoundTrips(t *testing.T) {
	sizes := []struct {
		name string
		n    int
	}{
		{"1MiB", 1 << 20},
		{"exactly 16MiB part size", s3UploadPartSize},
	}
	for _, tc := range sizes {
		t.Run(tc.name, func(t *testing.T) {
			body := bytes.Repeat([]byte("q"), tc.n)
			m := newMockUploadClient()

			require.NoError(t, s3ParallelUpload(context.Background(), m, "b", "k", "text/plain",
				nonSeekableReader{bytes.NewReader(body)}, int64(len(body))))

			assert.Equal(t, 1, m.putObjectCalls, "known size must collapse to one PutObject")
			assert.Equal(t, 0, m.createCalls, "no multipart ceremony")
			assert.Equal(t, body, m.putBody)
		})
	}
}

func TestS3Upload_UnknownSizeSentinelKeepsStreaming(t *testing.T) {
	body := bytes.Repeat([]byte("r"), 20<<20)
	m := newMockUploadClient()

	// -1 means "size unknown" — must not take the buffered fast path.
	require.NoError(t, s3ParallelUpload(context.Background(), m, "b", "k", "",
		nonSeekableReader{bytes.NewReader(body)}, -1))

	assert.Equal(t, 1, m.createCalls, "unknown size still streams via multipart")
	assert.Equal(t, body, m.reassemble())
}
