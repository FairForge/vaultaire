package api

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/FairForge/vaultaire/internal/drivers"
	"github.com/FairForge/vaultaire/internal/engine"
	"github.com/FairForge/vaultaire/internal/tenant"
	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

type listV2Result struct {
	XMLName               xml.Name `xml:"ListBucketResult"`
	Name                  string   `xml:"Name"`
	Prefix                string   `xml:"Prefix"`
	Delimiter             string   `xml:"Delimiter"`
	MaxKeys               int      `xml:"MaxKeys"`
	KeyCount              int      `xml:"KeyCount"`
	IsTruncated           bool     `xml:"IsTruncated"`
	ContinuationToken     string   `xml:"ContinuationToken"`
	NextContinuationToken string   `xml:"NextContinuationToken"`
	StartAfter            string   `xml:"StartAfter"`
	Contents              []struct {
		Key          string `xml:"Key"`
		Size         int64  `xml:"Size"`
		ETag         string `xml:"ETag"`
		LastModified string `xml:"LastModified"`
		StorageClass string `xml:"StorageClass"`
	} `xml:"Contents"`
	CommonPrefixes []struct {
		Prefix string `xml:"Prefix"`
	} `xml:"CommonPrefixes"`
}

func setupListTestServer(t *testing.T) (*Server, *tenant.Tenant, func()) {
	t.Helper()
	logger := zap.NewNop()
	eng := engine.NewEngine(nil, logger, nil)

	tempDir, err := os.MkdirTemp("", "vaultaire-list-test-*")
	require.NoError(t, err)

	driver := drivers.NewLocalDriver(tempDir, logger)
	eng.AddDriver("local", driver)
	eng.SetPrimary("local")

	server := &Server{
		logger:   logger,
		router:   chi.NewRouter(),
		engine:   eng,
		testMode: true,
	}
	server.router.HandleFunc("/", server.handleS3Request)

	testTenant := &tenant.Tenant{
		ID:        "test-tenant",
		Namespace: "tenant/test-tenant/",
	}

	cleanup := func() { _ = os.RemoveAll(tempDir) }
	return server, testTenant, cleanup
}

func putListTestObject(server *Server, t *tenant.Tenant, bucket, key, content string) {
	req := httptest.NewRequest("PUT", "/"+bucket+"/"+key,
		bytes.NewReader([]byte(content)))
	ctx := tenant.WithTenant(req.Context(), t)
	req = req.WithContext(ctx)
	server.handleS3Request(httptest.NewRecorder(), req)
}

func listObjects(server *Server, t *tenant.Tenant, path string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("GET", path, nil)
	ctx := tenant.WithTenant(req.Context(), t)
	req = req.WithContext(ctx)
	w := httptest.NewRecorder()
	server.handleS3Request(w, req)
	return w
}

func parseListResult(t *testing.T, w *httptest.ResponseRecorder) listV2Result {
	t.Helper()
	var result listV2Result
	err := xml.Unmarshal(w.Body.Bytes(), &result)
	require.NoError(t, err, "XML parse failed: %s", w.Body.String())
	return result
}

func TestS3_ListObjects(t *testing.T) {
	server, testTenant, cleanup := setupListTestServer(t)
	defer cleanup()

	objects := []string{"file1.txt", "file2.txt", "dir/file3.txt"}
	for _, key := range objects {
		putListTestObject(server, testTenant, "test-bucket", key, "content of "+key)
	}

	w := listObjects(server, testTenant, "/test-bucket")
	assert.Equal(t, 200, w.Code)

	result := parseListResult(t, w)
	assert.Equal(t, "test-bucket", result.Name)
	assert.Equal(t, 3, len(result.Contents))
	assert.Equal(t, 3, result.KeyCount)
	assert.False(t, result.IsTruncated)
	assert.Equal(t, 1000, result.MaxKeys)
}

func TestListV2_MaxKeysAndPagination(t *testing.T) {
	server, testTenant, cleanup := setupListTestServer(t)
	defer cleanup()

	for i := 0; i < 10; i++ {
		putListTestObject(server, testTenant, "page-bucket", fmt.Sprintf("obj-%02d.txt", i),
			fmt.Sprintf("content %d", i))
	}

	// Page 1: max-keys=3
	w := listObjects(server, testTenant, "/page-bucket?max-keys=3")
	assert.Equal(t, 200, w.Code)
	r := parseListResult(t, w)

	assert.Equal(t, 3, r.KeyCount)
	assert.True(t, r.IsTruncated)
	assert.NotEmpty(t, r.NextContinuationToken)
	assert.Equal(t, 3, r.MaxKeys)

	keys := make([]string, len(r.Contents))
	for i, c := range r.Contents {
		keys[i] = c.Key
	}
	assert.Equal(t, []string{"obj-00.txt", "obj-01.txt", "obj-02.txt"}, keys)

	// Page 2: use continuation token
	w = listObjects(server, testTenant,
		"/page-bucket?max-keys=3&continuation-token="+r.NextContinuationToken)
	assert.Equal(t, 200, w.Code)
	r2 := parseListResult(t, w)

	assert.Equal(t, 3, r2.KeyCount)
	assert.True(t, r2.IsTruncated)
	assert.NotEmpty(t, r2.NextContinuationToken)
	assert.Equal(t, r.NextContinuationToken, r2.ContinuationToken)

	keys2 := make([]string, len(r2.Contents))
	for i, c := range r2.Contents {
		keys2[i] = c.Key
	}
	assert.Equal(t, []string{"obj-03.txt", "obj-04.txt", "obj-05.txt"}, keys2)

	// Page 3
	w = listObjects(server, testTenant,
		"/page-bucket?max-keys=3&continuation-token="+r2.NextContinuationToken)
	r3 := parseListResult(t, w)
	assert.Equal(t, 3, r3.KeyCount)
	assert.True(t, r3.IsTruncated)

	// Page 4 (last page, 1 remaining)
	w = listObjects(server, testTenant,
		"/page-bucket?max-keys=3&continuation-token="+r3.NextContinuationToken)
	r4 := parseListResult(t, w)
	assert.Equal(t, 1, r4.KeyCount)
	assert.False(t, r4.IsTruncated)
	assert.Empty(t, r4.NextContinuationToken)
	assert.Equal(t, "obj-09.txt", r4.Contents[0].Key)
}

func TestListV2_FullPaginationCollectsAllKeys(t *testing.T) {
	server, testTenant, cleanup := setupListTestServer(t)
	defer cleanup()

	expected := make([]string, 20)
	for i := 0; i < 20; i++ {
		key := fmt.Sprintf("key-%02d", i)
		expected[i] = key
		putListTestObject(server, testTenant, "full-bucket", key, "data")
	}

	var allKeys []string
	token := ""
	pages := 0

	for {
		path := "/full-bucket?max-keys=7"
		if token != "" {
			path += "&continuation-token=" + token
		}
		w := listObjects(server, testTenant, path)
		assert.Equal(t, 200, w.Code)
		r := parseListResult(t, w)

		for _, c := range r.Contents {
			allKeys = append(allKeys, c.Key)
		}
		pages++

		if !r.IsTruncated {
			break
		}
		token = r.NextContinuationToken
		require.NotEmpty(t, token, "IsTruncated=true but no NextContinuationToken")
	}

	assert.Equal(t, expected, allKeys, "all keys should be returned exactly once across pages")
	assert.Equal(t, 3, pages, "20 keys / 7 per page = 3 pages")
}

func TestListV2_StartAfter(t *testing.T) {
	server, testTenant, cleanup := setupListTestServer(t)
	defer cleanup()

	for _, key := range []string{"alpha.txt", "bravo.txt", "charlie.txt", "delta.txt"} {
		putListTestObject(server, testTenant, "sa-bucket", key, "data")
	}

	w := listObjects(server, testTenant, "/sa-bucket?start-after=bravo.txt")
	assert.Equal(t, 200, w.Code)
	r := parseListResult(t, w)

	assert.Equal(t, "bravo.txt", r.StartAfter)
	assert.Equal(t, 2, r.KeyCount)
	assert.Equal(t, "charlie.txt", r.Contents[0].Key)
	assert.Equal(t, "delta.txt", r.Contents[1].Key)
}

func TestListV2_Prefix(t *testing.T) {
	server, testTenant, cleanup := setupListTestServer(t)
	defer cleanup()

	for _, key := range []string{"logs/app.log", "logs/error.log", "data/file.csv", "readme.txt"} {
		putListTestObject(server, testTenant, "prefix-bucket", key, "data")
	}

	w := listObjects(server, testTenant, "/prefix-bucket?prefix=logs/")
	assert.Equal(t, 200, w.Code)
	r := parseListResult(t, w)

	assert.Equal(t, "logs/", r.Prefix)
	assert.Equal(t, 2, r.KeyCount)
	keys := []string{r.Contents[0].Key, r.Contents[1].Key}
	assert.Contains(t, keys, "logs/app.log")
	assert.Contains(t, keys, "logs/error.log")
}

func TestListV2_Delimiter(t *testing.T) {
	server, testTenant, cleanup := setupListTestServer(t)
	defer cleanup()

	for _, key := range []string{
		"photos/2024/jan.jpg",
		"photos/2024/feb.jpg",
		"photos/2025/mar.jpg",
		"photos/cover.jpg",
		"readme.txt",
	} {
		putListTestObject(server, testTenant, "delim-bucket", key, "data")
	}

	// Root level with delimiter
	w := listObjects(server, testTenant, "/delim-bucket?delimiter=/")
	assert.Equal(t, 200, w.Code)
	r := parseListResult(t, w)

	assert.Equal(t, "/", r.Delimiter)
	assert.Equal(t, 1, len(r.Contents), "only readme.txt at root")
	assert.Equal(t, "readme.txt", r.Contents[0].Key)
	assert.Equal(t, 1, len(r.CommonPrefixes), "photos/ is the only common prefix")
	assert.Equal(t, "photos/", r.CommonPrefixes[0].Prefix)

	// Prefix + delimiter to browse one level deeper
	w = listObjects(server, testTenant, "/delim-bucket?prefix=photos/&delimiter=/")
	assert.Equal(t, 200, w.Code)
	r = parseListResult(t, w)

	assert.Equal(t, "photos/", r.Prefix)
	assert.Equal(t, 1, len(r.Contents), "only photos/cover.jpg is a direct child")
	assert.Equal(t, "photos/cover.jpg", r.Contents[0].Key)
	assert.Equal(t, 2, len(r.CommonPrefixes), "photos/2024/ and photos/2025/")

	cpPrefixes := []string{r.CommonPrefixes[0].Prefix, r.CommonPrefixes[1].Prefix}
	assert.Contains(t, cpPrefixes, "photos/2024/")
	assert.Contains(t, cpPrefixes, "photos/2025/")
}

func TestListV2_EmptyBucket(t *testing.T) {
	server, testTenant, cleanup := setupListTestServer(t)
	defer cleanup()

	w := listObjects(server, testTenant, "/empty-bucket")
	assert.Equal(t, 200, w.Code)
	r := parseListResult(t, w)

	assert.Equal(t, "empty-bucket", r.Name)
	assert.Equal(t, 0, r.KeyCount)
	assert.False(t, r.IsTruncated)
	assert.Empty(t, r.Contents)
}

func TestListV2_MaxKeysZero(t *testing.T) {
	server, testTenant, cleanup := setupListTestServer(t)
	defer cleanup()

	putListTestObject(server, testTenant, "zero-bucket", "file.txt", "data")

	w := listObjects(server, testTenant, "/zero-bucket?max-keys=0")
	assert.Equal(t, 200, w.Code)
	r := parseListResult(t, w)

	assert.Equal(t, 0, r.MaxKeys)
	assert.Equal(t, 0, r.KeyCount)
	assert.True(t, r.IsTruncated)
}

func TestProcessListEntries_NoDelimiter(t *testing.T) {
	entries := []ListV2Entry{
		{Key: "a"}, {Key: "b"}, {Key: "c"}, {Key: "d"}, {Key: "e"},
	}

	contents, cps, trunc, lastKey := processListEntries(entries, "", "", 3)
	assert.Equal(t, 3, len(contents))
	assert.Nil(t, cps)
	assert.True(t, trunc)
	assert.Equal(t, "c", lastKey)
	assert.Equal(t, "a", contents[0].Key)
	assert.Equal(t, "c", contents[2].Key)

	contents2, _, trunc2, lastKey2 := processListEntries(entries, "", "", 10)
	assert.Equal(t, 5, len(contents2))
	assert.False(t, trunc2)
	assert.Equal(t, "e", lastKey2)
}

func TestProcessListEntries_WithDelimiter(t *testing.T) {
	entries := []ListV2Entry{
		{Key: "dir1/a.txt"},
		{Key: "dir1/b.txt"},
		{Key: "dir2/c.txt"},
		{Key: "file.txt"},
	}

	contents, cps, trunc, _ := processListEntries(entries, "", "/", 10)
	assert.Equal(t, 1, len(contents))
	assert.Equal(t, "file.txt", contents[0].Key)
	assert.Equal(t, 2, len(cps))
	assert.Equal(t, "dir1/", cps[0].Prefix)
	assert.Equal(t, "dir2/", cps[1].Prefix)
	assert.False(t, trunc)
}

func TestProcessListEntries_DelimiterWithMaxKeys(t *testing.T) {
	entries := []ListV2Entry{
		{Key: "a/1"}, {Key: "a/2"}, {Key: "a/3"},
		{Key: "b/1"}, {Key: "b/2"},
		{Key: "c/1"},
		{Key: "top.txt"},
	}

	contents, cps, trunc, _ := processListEntries(entries, "", "/", 2)
	assert.Equal(t, 0, len(contents))
	assert.Equal(t, 2, len(cps))
	assert.Equal(t, "a/", cps[0].Prefix)
	assert.Equal(t, "b/", cps[1].Prefix)
	assert.True(t, trunc)
}

func TestContinuationToken_RoundTrip(t *testing.T) {
	keys := []string{"simple-key", "path/with/slashes", "special chars!@#", "日本語キー"}
	for _, key := range keys {
		token := encodeContinuationToken(key)
		decoded, err := decodeContinuationToken(token)
		require.NoError(t, err)
		assert.Equal(t, key, decoded)
	}
}
