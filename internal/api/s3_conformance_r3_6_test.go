package api

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// WP-R3-6 (R3-19, R3-23, R3-24, R3-25, R4 multipart tagging): multipart
// conformance. The query strings are aws-cli 2.28.19's own (captured against
// a local listener; boto3 sends botocore's, the same):
//
//	GET /b/k?uploadId=U&max-parts=2
//	GET /b/k?max-parts=2&part-number-marker=4&uploadId=U
//	GET /b?uploads&delimiter=%2F&prefix=logs%2F&max-uploads=2
//	GET /b?uploads&key-marker=k1&max-uploads=5&upload-id-marker=U1

type listPartsPage struct {
	XMLName              xml.Name `xml:"ListPartsResult"`
	PartNumberMarker     int
	NextPartNumberMarker int
	MaxParts             int
	IsTruncated          bool
	Parts                []struct{ PartNumber int } `xml:"Part"`
}

type listUploadsPage struct {
	XMLName            xml.Name `xml:"ListMultipartUploadsResult"`
	Prefix, Delimiter  string
	KeyMarker          string
	UploadIdMarker     string
	NextKeyMarker      string
	NextUploadIdMarker string
	MaxUploads         int
	IsTruncated        bool
	Uploads            []struct{ Key, UploadId string } `xml:"Upload"`
	CommonPrefixes     []struct{ Prefix string }        `xml:"CommonPrefixes"`
}

func (f *quotaAccountingFixture) getXML(t *testing.T, path string, into any) {
	t.Helper()
	w := doS3Request(f.server, f.tenant, "GET", path, nil)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	require.NoError(t, xml.Unmarshal(w.Body.Bytes(), into), w.Body.String())
}

func partNumbersOf(p listPartsPage) []int {
	var out []int
	for _, x := range p.Parts {
		out = append(out, x.PartNumber)
	}
	return out
}

func uploadKeysOf(p listUploadsPage) []string {
	var out []string
	for _, u := range p.Uploads {
		out = append(out, u.Key)
	}
	return out
}

func conformanceCompleteBody(etags map[int]string) string {
	var b strings.Builder
	b.WriteString("<CompleteMultipartUpload>")
	for n := 1; n <= len(etags); n++ {
		fmt.Fprintf(&b, "<Part><PartNumber>%d</PartNumber><ETag>%s</ETag></Part>", n, etags[n])
	}
	b.WriteString("</CompleteMultipartUpload>")
	return b.String()
}

func TestMultipartConformance(t *testing.T) {
	f := setupQuotaAccountingFixture(t, 1<<30)

	t.Run("ListParts pages", func(t *testing.T) {
		id := f.createUpload(t, "paged.bin", nil)
		for n := 1; n <= 5; n++ {
			require.Equal(t, http.StatusOK, f.putPart(t, "paged.bin", id, n, 1024).Code)
		}
		var p listPartsPage
		f.getXML(t, "/test-bucket/paged.bin?uploadId="+id+"&max-parts=2", &p)
		assert.Equal(t, []int{1, 2}, partNumbersOf(p))
		assert.True(t, p.IsTruncated)
		assert.Equal(t, 2, p.NextPartNumberMarker)
		assert.Equal(t, 2, p.MaxParts)

		p = listPartsPage{}
		f.getXML(t, "/test-bucket/paged.bin?max-parts=2&part-number-marker=4&uploadId="+id, &p)
		assert.Equal(t, []int{5}, partNumbersOf(p))
		assert.False(t, p.IsTruncated)
		assert.Equal(t, 4, p.PartNumberMarker)

		p = listPartsPage{}
		f.getXML(t, "/test-bucket/paged.bin?uploadId="+id, &p)
		assert.Equal(t, []int{1, 2, 3, 4, 5}, partNumbersOf(p))
		assert.Equal(t, 1000, p.MaxParts, "the default page")
	})

	t.Run("ListMultipartUploads pages, prefix and delimiter", func(t *testing.T) {
		for _, k := range []string{"logs/a", "logs/b", "logs/sub/c", "logs/sub/d", "other/x"} {
			f.createUpload(t, k, nil)
		}
		var p listUploadsPage
		f.getXML(t, "/test-bucket?uploads&delimiter=%2F&prefix=logs%2F&max-uploads=2", &p)
		assert.Equal(t, []string{"logs/a", "logs/b"}, uploadKeysOf(p))
		assert.Empty(t, p.CommonPrefixes)
		assert.True(t, p.IsTruncated)
		assert.Equal(t, "logs/b", p.NextKeyMarker)
		assert.Equal(t, "logs/", p.Prefix)
		assert.Equal(t, "/", p.Delimiter)
		assert.Equal(t, 2, p.MaxUploads)

		p = listUploadsPage{}
		f.getXML(t, "/test-bucket?uploads&delimiter=%2F&prefix=logs%2F&key-marker=logs%2Fb&max-uploads=2", &p)
		assert.Empty(t, p.Uploads)
		require.Len(t, p.CommonPrefixes, 1)
		assert.Equal(t, "logs/sub/", p.CommonPrefixes[0].Prefix, "one prefix for both uploads under it")
		assert.False(t, p.IsTruncated)

		p = listUploadsPage{}
		f.getXML(t, "/test-bucket?uploads&prefix=other%2F", &p)
		assert.Equal(t, []string{"other/x"}, uploadKeysOf(p))
	})

	t.Run("ListMultipartUploads key-marker and upload-id-marker", func(t *testing.T) {
		first := f.createUpload(t, "same", nil)
		second := f.createUpload(t, "same", nil)
		var p listUploadsPage
		f.getXML(t, "/test-bucket?uploads&key-marker=same&max-uploads=5&upload-id-marker="+first+"&prefix=same", &p)
		require.Len(t, p.Uploads, 1, "the uploads of that key after the marker")
		assert.Equal(t, second, p.Uploads[0].UploadId)
		assert.Equal(t, first, p.UploadIdMarker)

		p = listUploadsPage{}
		f.getXML(t, "/test-bucket?uploads&key-marker=same&prefix=same", &p)
		assert.Empty(t, p.Uploads, "a key-marker alone skips every upload of that key")

		p = listUploadsPage{}
		f.getXML(t, "/test-bucket?uploads&prefix=same&max-uploads=1", &p)
		assert.True(t, p.IsTruncated)
		assert.Equal(t, "same", p.NextKeyMarker)
		assert.Equal(t, first, p.NextUploadIdMarker)
	})

	t.Run("a non-last part under 5 MiB is EntityTooSmall", func(t *testing.T) {
		prev := multipartMinPartBytes
		multipartMinPartBytes = 5 << 20 // the production rule (TestMain lowers it for the package's 64 KiB fixtures)
		t.Cleanup(func() { multipartMinPartBytes = prev })

		id := f.createUpload(t, "small.bin", nil)
		etags := map[int]string{}
		for n := 1; n <= 2; n++ {
			w := f.putPart(t, "small.bin", id, n, 1024)
			require.Equal(t, http.StatusOK, w.Code)
			etags[n] = w.Header().Get("ETag")
		}
		w := f.completeReq("small.bin", id, conformanceCompleteBody(etags))
		assert.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
		assert.Equal(t, ErrEntityTooSmall, s3Code(t, w))
		assert.Equal(t, "active", f.uploadStatus(t, id), "the upload stays: the client can fix and retry")

		w = f.putPart(t, "small.bin", id, 1, 5<<20)
		require.Equal(t, http.StatusOK, w.Code)
		etags[1] = w.Header().Get("ETag")
		w = f.completeReq("small.bin", id, conformanceCompleteBody(etags))
		assert.Equal(t, http.StatusOK, w.Code, "5 MiB then a short last part: %s", w.Body.String())

		one := f.createUpload(t, "one.bin", nil)
		w = f.putPart(t, "one.bin", one, 1, 10)
		require.Equal(t, http.StatusOK, w.Code)
		w = f.completeReq("one.bin", one, conformanceCompleteBody(map[int]string{1: w.Header().Get("ETag")}))
		assert.Equal(t, http.StatusOK, w.Code, "a single part may be any size")
	})

	t.Run("Location takes the configured endpoint's scheme", func(t *testing.T) {
		prev := f.server.hosts.S3
		f.server.hosts.S3 = "https://s3.stored.cloud"
		t.Cleanup(func() { f.server.hosts.S3 = prev })
		id := f.createUpload(t, "dir/loc%20key.bin", nil)
		w := f.putPart(t, "dir/loc%20key.bin", id, 1, 10)
		require.Equal(t, http.StatusOK, w.Code)
		w = f.completeReq("dir/loc%20key.bin", id, conformanceCompleteBody(map[int]string{1: w.Header().Get("ETag")}))
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		var res CompleteMultipartUploadResult
		require.NoError(t, xml.Unmarshal(w.Body.Bytes(), &res))
		assert.True(t, strings.HasPrefix(res.Location, "https://"), res.Location)
		assert.True(t, strings.HasSuffix(res.Location, "/test-bucket/dir/loc%20key.bin"), res.Location)
	})

	t.Run("x-amz-tagging on Create is the object's at Complete", func(t *testing.T) {
		id := f.createUpload(t, "tagged.bin", map[string]string{"x-amz-tagging": "project=stored&tier=vault"})
		w := f.putPart(t, "tagged.bin", id, 1, 10)
		require.Equal(t, http.StatusOK, w.Code)
		w = f.completeReq("tagged.bin", id, conformanceCompleteBody(map[int]string{1: w.Header().Get("ETag")}))
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		w = doS3Request(f.server, f.tenant, "GET", "/test-bucket/tagged.bin?tagging", nil)
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		assert.Contains(t, w.Body.String(), "<Key>project</Key><Value>stored</Value>")
		assert.Contains(t, w.Body.String(), "<Key>tier</Key><Value>vault</Value>")

		r := httptest.NewRequest("POST", "/test-bucket/badtag.bin?uploads", nil)
		r.Header.Set("x-amz-tagging", "a=1&a=2")
		r = r.WithContext(s3Ctx(r.Context(), f.tenant))
		bad := httptest.NewRecorder()
		f.server.handleS3Request(bad, r)
		assert.Equal(t, http.StatusBadRequest, bad.Code, "an invalid tag set is refused at Create, as before")
	})

	t.Run("CopyObject's answer has its Content-Length", func(t *testing.T) {
		require.Equal(t, http.StatusOK, f.put(t, "copy-src.bin", bytes.Repeat([]byte("c"), 4096)))
		w := f.copyObject(t, "copy-src.bin", "copy-dst.bin")
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		assert.Equal(t, fmt.Sprint(w.Body.Len()), w.Header().Get("Content-Length"))
	})
}
