package drivers

// WP-EDGE-1 Part B: the presigned GET link of both EdgeWriter drivers signs
// `if-match` (always) and `range` (when asked), addresses exactly the key
// the driver's own Get would, and refuses to sign without an If-Match. No
// network: a presign is computed locally.

import (
	"context"
	"net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/FairForge/vaultaire/internal/common"
	"github.com/FairForge/vaultaire/internal/engine"
)

func TestEdgePresignGet_SignsRangeAndIfMatch(t *testing.T) {
	logger := zap.NewNop()
	idrive, err := NewFixedBucketS3Driver("idrive", "AKIDTEST", "secret", "https://127.0.0.1:1", "us-central-1", "vaultaire", logger)
	require.NoError(t, err)
	lyve, err := NewLyveDriver("AKIDTEST", "secret", "", "us-west-1", logger)
	require.NoError(t, err)
	ctx := common.WithTenantID(context.Background(), "tenant-abc")

	for _, tc := range []struct {
		name   string
		ew     engine.EdgeWriter
		bucket string
	}{
		{"idrive", idrive, "vaultaire"},
		{"lyve", lyve, lyve.getBucket()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// A whole read: if-match signed, no range.
			u, signed, err := tc.ew.PresignGet(ctx, "tenant-abc_bkt", "dir/obj.bin@01ARZ3NDEKTSV4RRFFQ69G5FAV",
				engine.EdgePresign{IfMatch: `"d41d8cd98f00b204e9800998ecf8427e"`})
			require.NoError(t, err)
			assert.ElementsMatch(t, []string{"host", "if-match"}, signed)
			pu, err := url.Parse(u)
			require.NoError(t, err)
			q := pu.Query()
			assert.Equal(t, "AWS4-HMAC-SHA256", q.Get("X-Amz-Algorithm"))
			assert.NotEmpty(t, q.Get("X-Amz-Signature"))
			assert.Equal(t, "host;if-match", q.Get("X-Amz-SignedHeaders"))
			assert.Empty(t, q.Get("X-Amz-Checksum-Mode"), "Lyve answers 403 SignatureDoesNotMatch to a link carrying it (Step 0.1)")
			assert.Contains(t, pu.Path, "/t-tenant-abc/tenant-abc_bkt/dir/obj.bin@01ARZ3NDEKTSV4RRFFQ69G5FAV", "the driver's own key rule")
			assert.Contains(t, pu.Path, tc.bucket)
			assert.Equal(t, "3600", q.Get("X-Amz-Expires"), "no TTL asked: the hour, never longer")

			// A range: both signed.
			u, signed, err = tc.ew.PresignGet(ctx, "tenant-abc_bkt", "obj.bin",
				engine.EdgePresign{IfMatch: `"abc"`, Range: "bytes=100-299", TTL: 15 * 60 * 1e9})
			require.NoError(t, err)
			assert.ElementsMatch(t, []string{"host", "if-match", "range"}, signed)
			pu, err = url.Parse(u)
			require.NoError(t, err)
			assert.Equal(t, "host;if-match;range", pu.Query().Get("X-Amz-SignedHeaders"))
			assert.Equal(t, "900", pu.Query().Get("X-Amz-Expires"))
			assert.False(t, strings.Contains(pu.RawQuery, "abc"), "the If-Match value travels as a header, never in the link")

			// No If-Match: refused — a link that lets other bytes through is never handed out.
			_, _, err = tc.ew.PresignGet(ctx, "tenant-abc_bkt", "obj.bin", engine.EdgePresign{Range: "bytes=0-1"})
			assert.Error(t, err)
			// No tenant on the context: refused (ErrNoTenant), never t-default.
			_, _, err = tc.ew.PresignGet(context.Background(), "tenant-abc_bkt", "obj.bin", engine.EdgePresign{IfMatch: `"abc"`})
			assert.ErrorIs(t, err, ErrNoTenant)
		})
	}
}
