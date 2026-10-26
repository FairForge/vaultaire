package drivers

import (
	"context"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/FairForge/vaultaire/internal/common"
	"github.com/FairForge/vaultaire/internal/engine"
)

// signedHeadersOf reads X-Amz-SignedHeaders off a presigned URL.
func signedHeadersOf(t *testing.T, u string) []string {
	t.Helper()
	parsed, err := url.Parse(u)
	require.NoError(t, err)
	return strings.Split(parsed.Query().Get("X-Amz-SignedHeaders"), ";")
}

// TestEdgePresign_SignsLengthAndChecksum (WP-EDGE-1 Part A, Step 0.1 /
// 0.2): every link the fixed-bucket driver and lyve hand out signs
// content-length and, when the client sent one, content-md5 — the owner's
// 2026-10-25 finding (a host-only link accepts any body) can never regress.
func TestEdgePresign_SignsLengthAndChecksum(t *testing.T) {
	fixed, err := NewFixedBucketS3Driver("idrive", "AKIAFAKE", "fakesecret", "https://fake.idrive.invalid", "us-central-1", "vaultaire", zap.NewNop())
	require.NoError(t, err)
	lyve, err := NewLyveDriver("AKIAFAKE", "fakesecret", "", "us-west-1", zap.NewNop())
	require.NoError(t, err)
	ctx := common.WithTenantID(context.Background(), "tenant-edge")
	for name, ew := range map[string]engine.EdgeWriter{"fixed-bucket": fixed, "lyve": lyve} {
		t.Run(name, func(t *testing.T) {
			u, signed, err := ew.PresignPut(ctx, "tenant-edge_bkt", "k@00000000000000000000000000", engine.EdgePresign{ContentLength: 123, ContentMD5: "AQEBAQEBAQEBAQEBAQEBAQ==", ContentType: "text/plain", TTL: 15 * time.Minute})
			require.NoError(t, err)
			assert.ElementsMatch(t, []string{"content-length", "content-md5", "content-type", "host"}, signed)
			assert.ElementsMatch(t, signed, signedHeadersOf(t, u), "the reported set is the URL's")
			assert.Contains(t, u, "/t-tenant-edge/tenant-edge_bkt/k%4000000000000000000000000000", "the driver's own key rule")

			u2, signed2, err := ew.PresignPut(ctx, "tenant-edge_bkt", "k", engine.EdgePresign{ContentLength: 5})
			require.NoError(t, err)
			assert.ElementsMatch(t, []string{"content-length", "host"}, signed2, "no md5 sent = none signed")
			assert.NotContains(t, signedHeadersOf(t, u2), "content-md5")

			u3, signed3, err := ew.PresignUploadPart(ctx, "tenant-edge_bkt", "k", "upload-1", 3, engine.EdgePresign{ContentLength: 77, ContentMD5: "AQEBAQEBAQEBAQEBAQEBAQ=="})
			require.NoError(t, err)
			assert.ElementsMatch(t, []string{"content-length", "content-md5", "host"}, signed3)
			parsed, _ := url.Parse(u3)
			assert.Equal(t, "upload-1", parsed.Query().Get("uploadId"))
			assert.Equal(t, "3", parsed.Query().Get("partNumber"))

			// No tenant on the context: refused like every fixed-bucket call.
			_, _, err = ew.PresignPut(context.Background(), "c", "k", engine.EdgePresign{ContentLength: 1})
			assert.ErrorIs(t, err, ErrNoTenant)
		})
	}
}
