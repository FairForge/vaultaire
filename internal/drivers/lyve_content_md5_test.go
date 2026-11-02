package drivers

import (
	"bytes"
	"crypto/md5" // #nosec G501 -- Content-MD5 is the S3 integrity header, compared, never a security primitive
	"encoding/base64"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/FairForge/vaultaire/internal/engine"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// 2026-11-02 (bench-results/LYVE-WORKER-2026-11-02.md, IDRIVE-WORKER-2026-11-02.md):
// Lyve stores a body under a wrong CRC32 / SHA-256 aws-chunked trailer and a
// wrong CRC header — the shape the Go SDK sends by default over HTTPS — so
// nothing on Lyve checked the bytes the origin wrote. It does refuse a wrong
// Content-MD5 (400 BadDigest), with the trailer still on. The driver now
// sends Content-MD5 on every PutObject and every UploadPart.

// md5Recorder checks each PUT's Content-MD5 against the bytes it received,
// then hands the request on.
type md5Recorder struct {
	next http.Handler
	mu   sync.Mutex
	puts []md5Seen
}

type md5Seen struct {
	key, header, actual string
	part                string
}

func (m *md5Recorder) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPut {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		sum := md5.Sum(body) // #nosec G401 -- see the import
		m.mu.Lock()
		m.puts = append(m.puts, md5Seen{key: r.URL.Path, header: r.Header.Get("Content-MD5"),
			actual: base64.StdEncoding.EncodeToString(sum[:]), part: r.URL.Query().Get("partNumber")})
		m.mu.Unlock()
		r.Body = io.NopCloser(bytes.NewReader(body))
		r.ContentLength = int64(len(body))
	}
	m.next.ServeHTTP(w, r)
}

func newLyveMD5Driver(t *testing.T) (*LyveDriver, *md5Recorder) {
	t.Helper()
	rec := &md5Recorder{next: newStallS3()}
	srv := httptest.NewServer(rec)
	t.Cleanup(srv.Close)
	cfg := aws.Config{Region: "us-west-1", Credentials: credentials.NewStaticCredentialsProvider("ak", "sk", ""),
		Retryer: func() aws.Retryer { return aws.NopRetryer{} }}
	d := &LyveDriver{client: newLyveS3Client(cfg, srv.URL), region: "us-west-1", logger: zap.NewNop()}
	d.setPutTimeout(30 * time.Second)
	return d, rec
}

func TestLyvePut_SendsContentMD5OnASinglePut(t *testing.T) {
	d, rec := newLyveMD5Driver(t)
	body := randomBody(t, 4096)

	require.NoError(t, d.Put(tctx(), "c", "small.bin", bytes.NewReader(body), engine.WithContentLength(int64(len(body)))))

	require.Len(t, rec.puts, 1, "a body under one part is one PutObject")
	assert.NotEmpty(t, rec.puts[0].header, "the PutObject carries Content-MD5")
	assert.Equal(t, rec.puts[0].actual, rec.puts[0].header, "and it is the MD5 of the bytes sent")
}

func TestLyvePut_SendsEachPartsOwnContentMD5(t *testing.T) {
	d, rec := newLyveMD5Driver(t)
	size := s3UploadPartSize + s3UploadPartSize/2 // two parts
	body := randomBody(t, size)

	// An unseekable body, as the engine hands drivers (http body → counting
	// reader → TeeReader): the uploader buffers each part, and each part's
	// MD5 is its own.
	require.NoError(t, d.Put(tctx(), "c", "large.bin", io.MultiReader(bytes.NewReader(body)), engine.WithContentLength(int64(size))))

	var parts []md5Seen
	for _, p := range rec.puts {
		if strings.HasSuffix(p.key, "large.bin") && p.part != "" {
			parts = append(parts, p)
		}
	}
	require.Len(t, parts, 2, "a multipart upload of two parts")
	for _, p := range parts {
		assert.NotEmpty(t, p.header, "part %s carries Content-MD5", p.part)
		assert.Equal(t, p.actual, p.header, "part %s: the MD5 of that part's bytes, not the object's", p.part)
	}
}
