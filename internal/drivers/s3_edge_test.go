package drivers

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
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

// TestEdgeSignPut_HeaderSignedStreamingTrailer (WP-EDGE-2 Part C, the SPEED
// bench § 1): an aws-chunked write the mover forwards untouched goes to the
// backend under a HEADER-signed request the control plane mints — iDrive
// refuses aws-chunked on any presigned (query) link. The signature covers
// the four aws-chunked headers, the streaming payload hash, x-amz-date and
// the host, never content-length (aws-cli sends none: Transfer-Encoding:
// chunked), and the SDK's own signer reproduces it.
func TestEdgeSignPut_HeaderSignedStreamingTrailer(t *testing.T) {
	fixed, err := NewFixedBucketS3Driver("idrive", "AKIAFAKE", "fakesecret", "https://fake.idrive.invalid", "us-central-1", "vaultaire", zap.NewNop())
	require.NoError(t, err)
	lyve, err := NewLyveDriver("AKIAFAKE", "fakesecret", "", "us-west-1", zap.NewNop())
	require.NoError(t, err)
	ctx := common.WithTenantID(context.Background(), "tenant-edge")
	for name, tc := range map[string]struct {
		ew     engine.EdgeWriter
		region string
		props  engine.EdgeStreamingProps
	}{
		"fixed-bucket": {fixed, "us-central-1", engine.EdgeStreamingProps{Supported: true, VerifiesTrailer: true}},
		"lyve":         {lyve, "us-west-1", engine.EdgeStreamingProps{Supported: true, SignatureMaxAge: 15 * time.Minute}},
	} {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, tc.props, tc.ew.EdgeStreaming(), "Step 0.3 + the bench: what the backend does with it")
			headers, u, err := tc.ew.SignPut(ctx, "tenant-edge_bkt", "k@00000000000000000000000000", 268435456, "x-amz-checksum-crc64nvme")
			require.NoError(t, err)
			parsed, err := url.Parse(u)
			require.NoError(t, err)
			assert.Empty(t, parsed.Query().Get("X-Amz-Signature"), "the plain object URL, never a presigned one")
			assert.Contains(t, parsed.EscapedPath(), "/t-tenant-edge/tenant-edge_bkt/k%4000000000000000000000000000", "the driver's own key rule")
			for name, want := range map[string]string{
				"x-amz-content-sha256":         "STREAMING-UNSIGNED-PAYLOAD-TRAILER",
				"content-encoding":             "aws-chunked",
				"x-amz-decoded-content-length": "268435456",
				"x-amz-trailer":                "x-amz-checksum-crc64nvme",
				"host":                         parsed.Host,
			} {
				assert.Equal(t, want, headers[name], name)
			}
			require.NotEmpty(t, headers["x-amz-date"])
			_, hasLength := headers["content-length"]
			assert.False(t, hasLength, "the encoded length is the client's; never signed")
			auth := headers["authorization"]
			assert.Contains(t, auth, "Credential=AKIAFAKE/")
			assert.Contains(t, auth, "/"+tc.region+"/s3/aws4_request")
			assert.Contains(t, auth, "SignedHeaders=content-encoding;host;x-amz-content-sha256;x-amz-date;x-amz-decoded-content-length;x-amz-trailer,")

			// The SDK's own signer, on an identical request at the same
			// x-amz-date, reproduces the authorization.
			at, err := time.Parse("20060102T150405Z", headers["x-amz-date"])
			require.NoError(t, err)
			r, err := http.NewRequest(http.MethodPut, u, nil)
			require.NoError(t, err)
			for k, v := range headers {
				if k != "authorization" && k != "host" && k != "x-amz-date" {
					r.Header.Set(k, v)
				}
			}
			require.NoError(t, v4.NewSigner().SignHTTP(context.Background(), aws.Credentials{AccessKeyID: "AKIAFAKE", SecretAccessKey: "fakesecret"},
				r, "STREAMING-UNSIGNED-PAYLOAD-TRAILER", "s3", tc.region, at))
			assert.Equal(t, r.Header.Get("Authorization"), auth)

			// An UploadPart the same way, under its upload id and part number.
			ph, pu, err := tc.ew.SignUploadPart(ctx, "tenant-edge_bkt", "k", "upload-1", 3, 8388608, "x-amz-checksum-crc32")
			require.NoError(t, err)
			pp, _ := url.Parse(pu)
			assert.Equal(t, "upload-1", pp.Query().Get("uploadId"))
			assert.Equal(t, "3", pp.Query().Get("partNumber"))
			assert.Empty(t, pp.Query().Get("X-Amz-Signature"))
			assert.Equal(t, "x-amz-checksum-crc32", ph["x-amz-trailer"])
			assert.Equal(t, "8388608", ph["x-amz-decoded-content-length"])

			_, _, err = tc.ew.SignPut(context.Background(), "c", "k", 1, "x-amz-checksum-crc32")
			assert.ErrorIs(t, err, ErrNoTenant)
		})
	}
	// A fixed-bucket backend no probe has measured (wasabi) offers none.
	wasabi, err := NewFixedBucketS3Driver("wasabi", "AKIAFAKE", "fakesecret", "https://fake.wasabi.invalid", "us-west-1", "vaultaire", zap.NewNop())
	require.NoError(t, err)
	assert.False(t, wasabi.EdgeStreaming().Supported)
}
