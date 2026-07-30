package drivers

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// blockingAfterN returns n bytes then blocks forever, modelling a body whose
// producer has delivered Content-Length but not yet signalled EOF.
type blockingAfterN struct {
	data []byte
	pos  int
	hit  chan struct{}
}

func (b *blockingAfterN) Read(p []byte) (int, error) {
	if b.pos < len(b.data) {
		n := copy(p, b.data[b.pos:])
		b.pos += n
		return n, nil
	}
	close(b.hit)
	select {} // block forever
}

func TestS3Upload_DoesNotBlockProbingPastContentLength(t *testing.T) {
	body := bytes.Repeat([]byte("p"), 1<<20)
	br := &blockingAfterN{data: body, hit: make(chan struct{})}
	m := newMockUploadClient()

	done := make(chan error, 1)
	go func() {
		done <- s3ParallelUploadInput(context.Background(), m, &s3.PutObjectInput{
			Bucket: aws.String("b"), Key: aws.String("k"),
			Body: br, ContentLength: aws.Int64(int64(len(body))),
		})
	}()

	select {
	case err := <-done:
		require.NoError(t, err)
		assert.Equal(t, 1, m.putObjectCalls)
	case <-time.After(3 * time.Second):
		select {
		case <-br.hit:
			t.Fatal("upload blocked reading past ContentLength — the probe read hangs on bodies that deliver exactly Content-Length without EOF")
		default:
			t.Fatal("upload hung for another reason")
		}
	}
}
