package drivers

import (
	"context"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakePageClient answers one ListObjectsV2 page and records the request.
type fakePageClient struct {
	in   *s3.ListObjectsV2Input
	keys []string
	more bool
}

func (f *fakePageClient) ListObjectsV2(_ context.Context, in *s3.ListObjectsV2Input, _ ...func(*s3.Options)) (*s3.ListObjectsV2Output, error) {
	f.in = in
	out := &s3.ListObjectsV2Output{IsTruncated: aws.Bool(f.more)}
	for _, k := range f.keys {
		out.Contents = append(out.Contents, s3types.Object{Key: aws.String(k), Size: aws.Int64(7), LastModified: aws.Time(time.Unix(1700000000, 0))})
	}
	return out, nil
}

// WP-R8-2 B.2: one page of the chunk container, after a key, with sizes and
// last-modified times, keys relative to the container.
func TestS3ListPage_StartsAfterTheCursorAndReturnsRelativeKeys(t *testing.T) {
	base := "t-_global/_global/"
	c := &fakePageClient{keys: []string{base + "_chunks/aa", base + "_chunks/t1/bb"}, more: true}

	objs, more, err := s3ListPage(context.Background(), c, "bucket", base, "_chunks/", "_chunks/a0", 5000)

	require.NoError(t, err)
	assert.True(t, more)
	assert.Equal(t, base+"_chunks/", aws.ToString(c.in.Prefix))
	assert.Equal(t, base+"_chunks/a0", aws.ToString(c.in.StartAfter))
	assert.Equal(t, int32(maxListPage), aws.ToInt32(c.in.MaxKeys), "a page is at most S3's 1000")
	require.Len(t, objs, 2)
	assert.Equal(t, "_chunks/t1/bb", objs[1].Artifact)
	assert.Equal(t, int64(7), objs[1].Size)
	assert.Equal(t, int64(1700000000), objs[1].LastModified.Unix())
}

func TestS3ListPage_AKeyFromOutsideThePrefixIsAnError(t *testing.T) {
	c := &fakePageClient{keys: []string{"t-other/_global/_chunks/aa"}}
	_, _, err := s3ListPage(context.Background(), c, "bucket", "t-_global/_global/", "_chunks/", "", 10)
	assert.Error(t, err)
}
