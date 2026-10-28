package api

// WP-EDGE-3 step 2: the Go data-plane host (internal/dataplane.Host) over
// the in-process control plane — the mover's PUT and part, the read, and
// the codec's aws-chunked decode — produces the rows the Worker's path
// does.

import (
	"bytes"
	"context"
	"crypto/md5" // #nosec G501 -- the S3 ETag
	"encoding/hex"
	"io"
	"net/http"
	"net/url"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/FairForge/vaultaire/internal/crypto"
	"github.com/FairForge/vaultaire/internal/dataplane"
)

func TestDataHost_MoverPutPartAndRead(t *testing.T) {
	f := setupEdgeFixture(t)
	f.wireEgress(1<<40, 0, nil)
	body := testBytes(9000)
	sum := md5.Sum(body) // #nosec G401
	host := f.dataHost()

	// A PutObject: moved to the presigned link, committed as a PUT.
	res, err := host.Write(context.Background(), f.signedHTTP(http.MethodPut, "/"+f.bucket+"/moved.bin", nil, body, nil), bytes.NewReader(body), int64(len(body)))
	require.NoError(t, err)
	require.True(t, res.Admit.Edge, "%+v", res.Admit)
	require.NotNil(t, res.Relay)
	assert.Equal(t, http.StatusOK, res.Relay.Status, res.Relay.Body)
	assert.Equal(t, `"`+hex.EncodeToString(sum[:])+`"`, res.Relay.Headers["Etag"])
	h, ok := f.head("moved.bin")
	require.True(t, ok)
	assert.Equal(t, int64(len(body)), h.size)
	assert.Equal(t, "edgefake", h.backend)

	// An UploadPart, then the origin's Complete.
	id := f.createUpload("mp.zip")
	var etags []string
	for n, pb := range [][]byte{testBytes(7000), testBytes(5000)} {
		q := url.Values{"uploadId": {id}, "partNumber": {[]string{"1", "2"}[n]}}
		r, err := host.Write(context.Background(), f.signedHTTP(http.MethodPut, "/"+f.bucket+"/mp.zip", q, pb, nil), bytes.NewReader(pb), int64(len(pb)))
		require.NoError(t, err)
		require.True(t, r.Admit.Edge, "%+v", r.Admit)
		require.Equal(t, http.StatusOK, r.Relay.Status, r.Relay.Body)
		etags = append(etags, r.Relay.Headers["Etag"])
	}
	wc := f.s3(http.MethodPost, "/"+f.bucket+"/mp.zip?uploadId="+id,
		[]byte(`<CompleteMultipartUpload><Part><PartNumber>1</PartNumber><ETag>`+etags[0]+`</ETag></Part><Part><PartNumber>2</PartNumber><ETag>`+etags[1]+`</ETag></Part></CompleteMultipartUpload>`), nil)
	require.Equal(t, http.StatusOK, wc.Code, wc.Body.String())

	// A read: the backend's bytes under the origin's headers; metered once.
	before := f.egressRecorded()
	rr, err := host.Read(context.Background(), f.signedHTTP(http.MethodGet, "/"+f.bucket+"/moved.bin", nil, nil, nil))
	require.NoError(t, err)
	require.False(t, rr.PassThrough, "%+v", rr.Admit)
	got, err := io.ReadAll(rr.Body)
	require.NoError(t, err)
	_ = rr.Body.Close()
	assert.True(t, bytes.Equal(body, got))
	assert.Equal(t, http.StatusOK, rr.Status)
	assert.Equal(t, `"`+hex.EncodeToString(sum[:])+`"`, rr.Headers["Etag"], "the origin's headers")
	assert.Equal(t, before+int64(len(body)), f.egressRecorded(), "metered once at the commit")

	// A pass-through (no such key): handed back untouched.
	rr, err = host.Read(context.Background(), f.signedHTTP(http.MethodGet, "/"+f.bucket+"/nope.bin", nil, nil, nil))
	require.NoError(t, err)
	assert.True(t, rr.PassThrough)
	assert.Equal(t, "no_row", rr.Reason)
}

// D-EDGE-31 on a Go host: an aws-chunked body is decoded and its trailer
// verified before the chunker; a trailer that is not the body's is the
// client's 400 XAmzContentChecksumMismatch with everything given back.
func TestDataHost_CodecDecodesAWSChunkedAndVerifiesTheTrailer(t *testing.T) {
	f := setupCodecFixture(t)
	body := crypto.GoldenBody(4701, 2<<20+77)
	framed := dataplane.FrameAWSChunked(body, dataplane.TrailerCRC64NVME, 1<<20)
	put := func(key string, payload []byte) *dataplane.Result {
		t.Helper()
		req := f.signedStreamingHTTP("/"+f.bucket+"/"+key, int64(len(body)), dataplane.TrailerCRC64NVME)
		res, err := f.dataHost().Write(context.Background(), req, bytes.NewReader(payload), -1)
		require.NoError(t, err)
		require.True(t, res.Admit.Edge && res.Admit.Codec != nil, "%+v", res.Admit)
		return res
	}
	res := put("chunked.bin", framed)
	require.Equal(t, http.StatusOK, res.Relay.Status, res.Relay.Body)
	w := f.originGet("chunked.bin", nil)
	require.Equal(t, http.StatusOK, w.Code)
	assert.True(t, bytes.Equal(body, w.Body.Bytes()), "the decoded bytes were chunked")

	bad := bytes.Clone(framed)
	bad[10] ^= 1 // a data byte: the trailer no longer matches
	used := f.used()
	res = put("bad.bin", bad)
	assert.Equal(t, http.StatusBadRequest, res.Relay.Status)
	assert.Contains(t, res.Relay.Body, "XAmzContentChecksumMismatch")
	assert.Equal(t, used, f.used(), "everything given back")
	_, ok := f.head("bad.bin")
	assert.False(t, ok)
}
