package api

// WP-R2-4 (+ R2-21, R2-22, R2-23, R8-14, R8-15 = WP-R8-6): HEAD, a 200 and a
// 206 of one object did not agree on their headers — the native 206 omitted
// ETag, Last-Modified, x-amz-meta-*, the storage class and Cache-Control;
// HEAD omitted Accept-Ranges, the version id and GET's default Cache-Control;
// HEAD of an SSE-C object without the key set size and ETag before its 403.
// aws s3 sync, rclone and the SDKs compare them. One writer now
// (object_headers.go): the same set on all three, with exactly these
// differences — Content-Length is the object's on HEAD and 200 and the
// range's on 206, and 206 adds Content-Range.

import (
	"bytes"
	"crypto/md5" // #nosec G501 -- the SSE-C key's MD5, as S3 defines it
	"encoding/base64"
	"encoding/xml"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// paritySkip are the headers that differ by request, not by object.
var paritySkip = map[string]bool{"X-Amz-Request-Id": true, "Date": true, "Content-Length": true, "Content-Range": true}

// objectHeaderSet is the response's headers minus paritySkip.
func objectHeaderSet(h http.Header) map[string]string {
	out := map[string]string{}
	for k, v := range h {
		if !paritySkip[k] {
			out[k] = strings.Join(v, ",")
		}
	}
	return out
}

func hdrKeys(m map[string]string) []string {
	var ks []string
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}

type hdrFixture struct {
	*mpChunkFixture
}

func setupHdrFixture(t *testing.T, withKey bool) *hdrFixture {
	t.Helper()
	f := setupMPChunkFixture(t, withKey)
	f.server.chunkingThreshold = 1 << 30 // stored whole unless a test lowers it
	return &hdrFixture{mpChunkFixture: f}
}

func (f *hdrFixture) do(t *testing.T, method, target string, body []byte, hdr map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	r := httptest.NewRequest(method, target, rd)
	if body != nil {
		r.ContentLength = int64(len(body))
	}
	for k, v := range hdr {
		r.Header.Set(k, v)
	}
	r = r.WithContext(s3Ctx(r.Context(), f.tenant))
	w := httptest.NewRecorder()
	f.server.handleS3Request(w, r)
	return w
}

// putRich PUTs key with every attribute an object can carry.
func (f *hdrFixture) putRich(t *testing.T, key string, body []byte) string {
	t.Helper()
	w := f.do(t, "PUT", "/test-bucket/"+key, body, map[string]string{
		"Content-Type":                    "application/x-parity",
		"x-amz-meta-owner":                "isaac",
		"x-amz-meta-project":              "p13",
		"Content-Disposition":             `attachment; filename="parity.bin"`,
		"Content-Encoding":                "identity",
		"Content-Language":                "en-US",
		"Cache-Control":                   "max-age=60, public",
		"Expires":                         "Wed, 21 Oct 2026 07:28:00 GMT",
		"x-amz-website-redirect-location": "/elsewhere.html",
		"x-amz-tagging":                   "env=prod&team=storage",
		"x-amz-storage-class":             "STANDARD",
	})
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	return w.Header().Get("x-amz-version-id")
}

func TestHeaders_HeadGetAndRangeCarryTheSameObjectHeaders(t *testing.T) {
	// Arrange: a versioned bucket, one object with every attribute.
	f := setupHdrFixture(t, false)
	f.bucketSet(t, "versioning_status = 'Enabled'")
	body := randomBody(t, 64<<10)
	vid := f.putRich(t, "rich.bin", body)
	require.NotEmpty(t, vid)

	// Act
	head := f.do(t, "HEAD", "/test-bucket/rich.bin", nil, nil)
	get := f.do(t, "GET", "/test-bucket/rich.bin", nil, nil)
	rng := f.do(t, "GET", "/test-bucket/rich.bin", nil, map[string]string{"Range": "bytes=100-1123"})

	// Assert: the statuses and bodies.
	require.Equal(t, http.StatusOK, head.Code)
	require.Equal(t, http.StatusOK, get.Code)
	require.Equal(t, http.StatusPartialContent, rng.Code)
	assert.Equal(t, body, get.Body.Bytes())
	assert.Equal(t, body[100:1124], rng.Body.Bytes())

	// The one set, with every attribute in it.
	hs, gs, rs := objectHeaderSet(head.Header()), objectHeaderSet(get.Header()), objectHeaderSet(rng.Header())
	assert.Equal(t, gs, hs, "HEAD = GET: HEAD %v / GET %v", hdrKeys(hs), hdrKeys(gs))
	assert.Equal(t, gs, rs, "206 = 200: 206 %v / 200 %v", hdrKeys(rs), hdrKeys(gs))
	for k, v := range map[string]string{
		"Etag":                            `"` + md5Hex(body) + `"`,
		"Content-Type":                    "application/x-parity",
		"Accept-Ranges":                   "bytes",
		"X-Amz-Storage-Class":             "STANDARD",
		"X-Amz-Version-Id":                vid,
		"X-Amz-Meta-Owner":                "isaac",
		"X-Amz-Meta-Project":              "p13",
		"Content-Disposition":             `attachment; filename="parity.bin"`,
		"Content-Encoding":                "identity",
		"Content-Language":                "en-US",
		"Cache-Control":                   "max-age=60, public",
		"Expires":                         "Wed, 21 Oct 2026 07:28:00 GMT",
		"X-Amz-Website-Redirect-Location": "/elsewhere.html",
		"X-Amz-Tagging-Count":             "2",
	} {
		assert.Equal(t, v, gs[k], k)
	}
	assert.NotEmpty(t, gs["Last-Modified"])

	// Exactly these differences.
	assert.Equal(t, strconv.Itoa(len(body)), head.Header().Get("Content-Length"))
	assert.Equal(t, strconv.Itoa(len(body)), get.Header().Get("Content-Length"))
	assert.Equal(t, "1024", rng.Header().Get("Content-Length"))
	assert.Equal(t, "bytes 100-1123/65536", rng.Header().Get("Content-Range"))
	assert.Empty(t, head.Header().Get("Content-Range"))
	assert.Empty(t, get.Header().Get("Content-Range"))
}

func TestHeaders_ADefaultCacheControlIsTheSameOnHeadAndGet(t *testing.T) {
	f := setupHdrFixture(t, false)
	w := f.do(t, "PUT", "/test-bucket/plain.bin", []byte("plain"), nil)
	require.Equal(t, http.StatusOK, w.Code)

	head := f.do(t, "HEAD", "/test-bucket/plain.bin", nil, nil)
	get := f.do(t, "GET", "/test-bucket/plain.bin", nil, nil)

	assert.Equal(t, "private, no-cache", get.Header().Get("Cache-Control"))
	assert.Equal(t, get.Header().Get("Cache-Control"), head.Header().Get("Cache-Control"))
	assert.Equal(t, objectHeaderSet(get.Header()), objectHeaderSet(head.Header()))
	assert.Empty(t, get.Header().Get("x-amz-version-id"), "an unversioned bucket names no version")
}

func TestHeaders_HeadWithARangeAnswersAsAWSDoes(t *testing.T) {
	// Arrange
	f := setupHdrFixture(t, false)
	body := randomBody(t, 4096)
	require.Equal(t, http.StatusOK, f.do(t, "PUT", "/test-bucket/r.bin", body, nil).Code)
	require.Equal(t, http.StatusOK, f.do(t, "PUT", "/test-bucket/empty.bin", []byte{}, nil).Code)

	// A satisfiable range: 206, the range's length, no body.
	w := f.do(t, "HEAD", "/test-bucket/r.bin", nil, map[string]string{"Range": "bytes=10-19"})
	assert.Equal(t, http.StatusPartialContent, w.Code)
	assert.Equal(t, "10", w.Header().Get("Content-Length"))
	assert.Equal(t, "bytes 10-19/4096", w.Header().Get("Content-Range"))
	assert.Zero(t, w.Body.Len())
	get := f.do(t, "GET", "/test-bucket/r.bin", nil, map[string]string{"Range": "bytes=10-19"})
	assert.Equal(t, objectHeaderSet(get.Header()), objectHeaderSet(w.Header()), "HEAD+Range = GET+Range")

	// Unsatisfiable: 416 with the size.
	for _, method := range []string{"HEAD", "GET"} {
		w = f.do(t, method, "/test-bucket/r.bin", nil, map[string]string{"Range": "bytes=5000-6000"})
		assert.Equal(t, http.StatusRequestedRangeNotSatisfiable, w.Code, method)
		assert.Equal(t, "bytes */4096", w.Header().Get("Content-Range"), method)
		// R2-21: any range of an empty object.
		w = f.do(t, method, "/test-bucket/empty.bin", nil, map[string]string{"Range": "bytes=0-0"})
		assert.Equal(t, http.StatusRequestedRangeNotSatisfiable, w.Code, method+" empty")
		assert.Equal(t, "bytes */0", w.Header().Get("Content-Range"), method+" empty")
	}

	// A multi-range is ignored: 200 whole (RFC 9110 §14.2), HEAD as GET.
	w = f.do(t, "HEAD", "/test-bucket/r.bin", nil, map[string]string{"Range": "bytes=0-1,5-6"})
	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "4096", w.Header().Get("Content-Length"))
	assert.Empty(t, w.Header().Get("Content-Range"))
}

func TestHeaders_HeadOfANonCurrentVersionCarriesItsOwnAttributes(t *testing.T) {
	// Arrange: v1 rich, v2 plain.
	f := setupHdrFixture(t, false)
	f.bucketSet(t, "versioning_status = 'Enabled'")
	v1body := randomBody(t, 3000)
	v1 := f.putRich(t, "doc.bin", v1body)
	require.Equal(t, http.StatusOK, f.do(t, "PUT", "/test-bucket/doc.bin", []byte("v2"), map[string]string{"Content-Type": "text/plain"}).Code)

	// Act
	head := f.do(t, "HEAD", "/test-bucket/doc.bin?versionId="+v1, nil, nil)
	get := f.do(t, "GET", "/test-bucket/doc.bin?versionId="+v1, nil, nil)

	// Assert: v1's attributes, the same set on both (tags are the key's:
	// per-version tags are not kept, so neither carries v1's count).
	require.Equal(t, http.StatusOK, head.Code)
	require.Equal(t, http.StatusOK, get.Code)
	assert.Equal(t, v1body, get.Body.Bytes())
	assert.Equal(t, objectHeaderSet(get.Header()), objectHeaderSet(head.Header()))
	assert.Equal(t, "application/x-parity", head.Header().Get("Content-Type"))
	assert.Equal(t, "isaac", head.Header().Get("x-amz-meta-owner"))
	assert.Equal(t, "max-age=60, public", head.Header().Get("Cache-Control"))
	assert.Equal(t, v1, head.Header().Get("x-amz-version-id"))
	assert.Equal(t, "bytes", head.Header().Get("Accept-Ranges"))
	assert.Equal(t, "3000", head.Header().Get("Content-Length"))
}

func TestHeaders_AVersionIdNamingADeleteMarkerIs405(t *testing.T) {
	// Arrange
	f := setupHdrFixture(t, false)
	f.bucketSet(t, "versioning_status = 'Enabled'")
	require.Equal(t, http.StatusOK, f.do(t, "PUT", "/test-bucket/gone.bin", []byte("x"), nil).Code)
	del := f.do(t, "DELETE", "/test-bucket/gone.bin", nil, nil)
	require.Equal(t, http.StatusNoContent, del.Code)
	marker := del.Header().Get("x-amz-version-id")
	require.NotEmpty(t, marker)

	for _, method := range []string{"HEAD", "GET"} {
		// Act
		w := f.do(t, method, "/test-bucket/gone.bin?versionId="+marker, nil, nil)

		// Assert: AWS answers 405 MethodNotAllowed for a delete marker's id.
		assert.Equal(t, http.StatusMethodNotAllowed, w.Code, method)
		assert.Equal(t, "true", w.Header().Get("x-amz-delete-marker"), method)
		assert.Equal(t, marker, w.Header().Get("x-amz-version-id"), method)
		if method == "GET" {
			assert.Contains(t, w.Body.String(), "<Code>MethodNotAllowed</Code>")
		}
	}
	// Without a version id the key is a 404 with the marker named (kept).
	w := f.do(t, "GET", "/test-bucket/gone.bin", nil, nil)
	assert.Equal(t, http.StatusNotFound, w.Code)
	assert.Equal(t, "true", w.Header().Get("x-amz-delete-marker"))
}

func ssecTestHeaders(key []byte) map[string]string {
	sum := md5.Sum(key) // #nosec G401 -- S3 defines the key's MD5
	return map[string]string{
		"x-amz-server-side-encryption-customer-algorithm": "AES256",
		"x-amz-server-side-encryption-customer-key":       base64.StdEncoding.EncodeToString(key),
		"x-amz-server-side-encryption-customer-key-MD5":   base64.StdEncoding.EncodeToString(sum[:]),
	}
}

func TestHeaders_AnSSECObjectWithoutItsKeyIs400WithNoObjectHeaders(t *testing.T) {
	// Arrange
	f := setupHdrFixture(t, false)
	key := bytes.Repeat([]byte{0x5a}, 32)
	body := randomBody(t, 2000)
	w := f.do(t, "PUT", "/test-bucket/ssec.bin", body, ssecTestHeaders(key))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	wrong := bytes.Repeat([]byte{0x11}, 32)

	for _, method := range []string{"HEAD", "GET"} {
		// Act: no key.
		w := f.do(t, method, "/test-bucket/ssec.bin", nil, nil)

		// Assert: AWS's 400, decided before any object header.
		assert.Equal(t, http.StatusBadRequest, w.Code, method)
		for _, h := range []string{"ETag", "Last-Modified", "x-amz-storage-class", "Accept-Ranges", "x-amz-server-side-encryption-customer-algorithm"} {
			assert.Empty(t, w.Header().Get(h), "%s: %s", method, h)
		}
		assert.NotEqual(t, "2000", w.Header().Get("Content-Length"), method)
		if method == "GET" {
			var e struct {
				Code    string `xml:"Code"`
				Message string `xml:"Message"`
			}
			require.NoError(t, xml.Unmarshal(w.Body.Bytes(), &e))
			assert.Equal(t, "InvalidRequest", e.Code)
			assert.Equal(t, "The object was stored using a form of Server Side Encryption. The correct parameters must be provided to retrieve the object.", e.Message)
		}
	}

	// With the key: 200, the same object headers on both.
	head := f.do(t, "HEAD", "/test-bucket/ssec.bin", nil, ssecTestHeaders(key))
	get := f.do(t, "GET", "/test-bucket/ssec.bin", nil, ssecTestHeaders(key))
	require.Equal(t, http.StatusOK, head.Code)
	require.Equal(t, http.StatusOK, get.Code, get.Body.String())
	assert.Equal(t, body, get.Body.Bytes())
	assert.Equal(t, "AES256", head.Header().Get("x-amz-server-side-encryption-customer-algorithm"))
	assert.Equal(t, objectHeaderSet(get.Header()), objectHeaderSet(head.Header()))

	// A wrong key: 403 on both, from the row's key MD5 (RP14-11, 094 — HEAD
	// answered 200 until then: cache-only, and the key could only be checked
	// against the blob; the deviation is closed for rows written since).
	assert.Equal(t, http.StatusForbidden, f.do(t, "GET", "/test-bucket/ssec.bin", nil, ssecTestHeaders(wrong)).Code)
	assert.Equal(t, http.StatusForbidden, f.do(t, "HEAD", "/test-bucket/ssec.bin", nil, ssecTestHeaders(wrong)).Code)
}

func TestHeaders_AnEmptySSECObjectIsStoredAndRead(t *testing.T) {
	// R2-23: a 0-byte SSE-C PUT answered 413.
	f := setupHdrFixture(t, false)
	key := bytes.Repeat([]byte{0x33}, 32)

	put := f.do(t, "PUT", "/test-bucket/empty-ssec.bin", []byte{}, ssecTestHeaders(key))
	require.Equal(t, http.StatusOK, put.Code, put.Body.String())
	head := f.do(t, "HEAD", "/test-bucket/empty-ssec.bin", nil, ssecTestHeaders(key))
	get := f.do(t, "GET", "/test-bucket/empty-ssec.bin", nil, ssecTestHeaders(key))

	assert.Equal(t, http.StatusOK, head.Code)
	assert.Equal(t, "0", head.Header().Get("Content-Length"))
	assert.Equal(t, http.StatusOK, get.Code, get.Body.String())
	assert.Zero(t, get.Body.Len())
}

func TestHeaders_AChunkEncryptedObjectNamesItsEncryptionEverywhere(t *testing.T) {
	// R8-15 / WP-R8-6: the PUT answer and the chunked GET of a chunk-
	// encrypted object said nothing of its encryption (HEAD did).
	f := setupHdrFixture(t, true)
	f.server.chunkingThreshold = 1024
	body := randomBody(t, 3<<20)
	sse := map[string]string{"x-amz-server-side-encryption": "AES256"}

	put := f.do(t, "PUT", "/test-bucket/ce.bin", body, sse)
	require.Equal(t, http.StatusOK, put.Code, put.Body.String())
	var chunked bool
	require.NoError(t, f.db.QueryRow(`SELECT is_chunked FROM object_head_cache WHERE tenant_id = $1 AND object_key = 'ce.bin'`, f.tenantID).Scan(&chunked))
	require.True(t, chunked)
	head := f.do(t, "HEAD", "/test-bucket/ce.bin", nil, nil)
	get := f.do(t, "GET", "/test-bucket/ce.bin", nil, nil)
	rng := f.do(t, "GET", "/test-bucket/ce.bin", nil, map[string]string{"Range": "bytes=0-99"})

	assert.Equal(t, "AES256", put.Header().Get("x-amz-server-side-encryption"), "PUT")
	assert.Equal(t, "AES256", head.Header().Get("x-amz-server-side-encryption"), "HEAD")
	assert.Equal(t, "AES256", get.Header().Get("x-amz-server-side-encryption"), "GET")
	assert.Equal(t, "AES256", rng.Header().Get("x-amz-server-side-encryption"), "GET range")
	assert.Equal(t, body, get.Body.Bytes())
	assert.Equal(t, objectHeaderSet(get.Header()), objectHeaderSet(head.Header()))
	assert.Equal(t, objectHeaderSet(get.Header()), objectHeaderSet(rng.Header()))

	// CompleteMultipartUpload of a chunk-encrypted object.
	create := f.do(t, "POST", "/test-bucket/mp-ce.bin?uploads", nil, sse)
	require.Equal(t, http.StatusOK, create.Code, create.Body.String())
	var init struct {
		UploadID string `xml:"UploadId"`
	}
	require.NoError(t, xml.Unmarshal(create.Body.Bytes(), &init))
	part := f.do(t, "PUT", "/test-bucket/mp-ce.bin?partNumber=1&uploadId="+init.UploadID, body, nil)
	require.Equal(t, http.StatusOK, part.Code, part.Body.String())
	complete := f.do(t, "POST", "/test-bucket/mp-ce.bin?uploadId="+init.UploadID,
		[]byte(`<CompleteMultipartUpload><Part><PartNumber>1</PartNumber><ETag>`+part.Header().Get("ETag")+`</ETag></Part></CompleteMultipartUpload>`), nil)
	require.Equal(t, http.StatusOK, complete.Code, complete.Body.String())
	require.NotContains(t, complete.Body.String(), "<Error>")
	assert.Equal(t, "AES256", complete.Header().Get("x-amz-server-side-encryption"), "Complete")
}

func TestHeaders_A304CarriesTheValidatorsAndTheCacheHeadersOnly(t *testing.T) {
	// Arrange
	f := setupHdrFixture(t, false)
	f.bucketSet(t, "versioning_status = 'Enabled'")
	f.putRich(t, "c.bin", randomBody(t, 1000))
	etag := f.do(t, "HEAD", "/test-bucket/c.bin", nil, nil).Header().Get("ETag")
	require.NotEmpty(t, etag)

	for _, method := range []string{"GET", "HEAD"} {
		// Act
		w := f.do(t, method, "/test-bucket/c.bin", nil, map[string]string{"If-None-Match": etag})

		// Assert (RFC 9110 §15.4.5: ETag, and Cache-Control / Expires as a
		// 200 would send them; Last-Modified, the other validator)
		require.Equal(t, http.StatusNotModified, w.Code, method)
		assert.Equal(t, etag, w.Header().Get("ETag"), method)
		assert.NotEmpty(t, w.Header().Get("Last-Modified"), method)
		assert.Equal(t, "max-age=60, public", w.Header().Get("Cache-Control"), method)
		assert.Equal(t, "Wed, 21 Oct 2026 07:28:00 GMT", w.Header().Get("Expires"), method)
		for _, h := range []string{"Content-Type", "Content-Length", "x-amz-meta-owner", "Content-Disposition", "x-amz-storage-class", "x-amz-tagging-count", "Accept-Ranges", "x-amz-website-redirect-location", "x-amz-version-id"} {
			assert.Empty(t, w.Header().Get(h), "%s 304: %s", method, h)
		}
		assert.Zero(t, w.Body.Len(), method)
	}
}

func TestHeaders_TheParallelAndTheSealedGetAnswerThePlainGetsHeaders(t *testing.T) {
	// (i) The parallel ranged reader and the sealed reader write the same
	// headers as the single stream — asserted, not assumed.
	for _, sealed := range []bool{false, true} {
		name := map[bool]string{false: "plain", true: "sealed"}[sealed]
		t.Run(name, func(t *testing.T) {
			on := setupSealedParallelFixture(t, true)
			content := randomBytes(9<<20 + 5)
			if sealed {
				on.putSealed(t, "big.bin", content, nil)
			} else {
				on.put(t, "big.bin", content)
			}
			par := on.get(t, "big.bin", nil)
			require.Equal(t, http.StatusOK, par.Code)
			require.Positive(t, on.drv.ranges.Load(), "the parallel reader served it")
			on.adapter.largeGet.minBytes = 1 << 40 // the single stream
			one := on.get(t, "big.bin", nil)
			require.Equal(t, http.StatusOK, one.Code)
			assert.Equal(t, objectHeaderSet(one.Header()), objectHeaderSet(par.Header()))
			assert.Equal(t, one.Header().Get("Content-Length"), par.Header().Get("Content-Length"))
			for _, rh := range []string{"bytes=1000-4999999"} {
				on.adapter.largeGet.minBytes = 4 << 20
				pr := on.get(t, "big.bin", map[string]string{"Range": rh})
				on.adapter.largeGet.minBytes = 1 << 40
				sr := on.get(t, "big.bin", map[string]string{"Range": rh})
				require.Equal(t, http.StatusPartialContent, pr.Code)
				require.Equal(t, http.StatusPartialContent, sr.Code)
				assert.Equal(t, objectHeaderSet(sr.Header()), objectHeaderSet(pr.Header()))
				assert.Equal(t, objectHeaderSet(one.Header()), objectHeaderSet(pr.Header()), "a 206 = a 200")
			}
		})
	}
}
