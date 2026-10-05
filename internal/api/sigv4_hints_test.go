package api

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/stretchr/testify/require"
)

// R2 Sippy's multi-part pull signs If-Match with the object's ETag and sends
// the request without the header; the origin used to answer 403 to every
// part and the pull of any object above ~200 MiB failed for ever.
func TestSippyPartGET_IfMatchSignedButNotSentIsServed(t *testing.T) {
	if testing.Short() {
		t.Skip("DB-backed")
	}
	f := setupBypassFixture(t)
	ctx := context.Background()
	body := strings.Repeat("part-bytes ", 1000)
	_, err := f.root().PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String(f.bucket), Key: aws.String("big.bin"), Body: strings.NewReader(body)})
	require.NoError(t, err)
	head, err := f.root().HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String(f.bucket), Key: aws.String("big.bin")})
	require.NoError(t, err)
	etag := aws.ToString(head.ETag)

	// A part GET the way Sippy sends it: signed with If-Match, sent without.
	send := func(ifMatchSigned, ifMatchSent string) *http.Response {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, f.ts.URL+"/"+f.bucket+"/big.bin?response-content-encoding=none", nil)
		require.NoError(t, err)
		req.Header.Set("Range", "bytes=0-99")
		req.Header.Set("If-Match", ifMatchSigned)
		req.Header.Set("X-Amz-Content-Sha256", "UNSIGNED-PAYLOAD")
		signer := v4.NewSigner(func(o *v4.SignerOptions) { o.DisableURIPathEscaping = true })
		require.NoError(t, signer.SignHTTP(ctx, aws.Credentials{AccessKeyID: f.rootAK, SecretAccessKey: f.rootSK}, req, "UNSIGNED-PAYLOAD", "s3", "us-east-1", time.Now().UTC()))
		require.Contains(t, req.Header.Get("Authorization"), "if-match;")
		if ifMatchSent == "" {
			req.Header.Del("If-Match")
		} else {
			req.Header.Set("If-Match", ifMatchSent)
		}
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		t.Cleanup(func() { _ = resp.Body.Close() })
		return resp
	}

	resp := send(etag, "")
	b, _ := io.ReadAll(resp.Body)
	require.Equal(t, http.StatusPartialContent, resp.StatusCode, string(b))
	require.Equal(t, body[:100], string(b))

	// A signed value that is not the object's ETag is still a mismatch, and
	// a sent header that differs from the object's ETag is the usual 412.
	require.Equal(t, http.StatusForbidden, send(`"not-the-etag"`, "").StatusCode)
	require.Equal(t, http.StatusPreconditionFailed, send(`"not-the-etag"`, `"not-the-etag"`).StatusCode)
}
