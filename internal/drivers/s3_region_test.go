package drivers

import (
	"testing"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// The generic S3 driver must sign with the region it is given: Backblaze B2
// rejects a SigV4 scope naming another region (us-east-1 vs us-east-005) and
// R2 expects "auto". An empty region keeps the historical us-east-1 default.
func TestNewS3Driver_SignsWithGivenRegion(t *testing.T) {
	cases := []struct{ given, want string }{
		{"us-east-005", "us-east-005"},
		{"auto", "auto"},
		{"", "us-east-1"},
	}
	for _, tc := range cases {
		d, err := NewS3Driver("https://s3.example.invalid", "ak", "sk", tc.given, zap.NewNop())
		require.NoError(t, err)
		require.Equal(t, tc.want, d.client.Options().Region, "given %q", tc.given)
	}
}
