package drivers

import (
	"context"
	"encoding/xml"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/FairForge/vaultaire/internal/common"
)

// Review R7-06: List must return keys relative to the container, filtered by
// prefix, from EVERY page. s3compat stripped the wrong prefix and s3compat,
// s3 (→ quotaless) and geyser read a single ListObjectsV2 page.

// pagedS3 is a fake S3 endpoint whose ListObjectsV2 answers at most pageSize
// keys per page with a continuation token, so any single-page reader
// truncates.
type pagedS3 struct {
	mu       sync.Mutex
	keys     []string
	pageSize int
	pages    int // list requests served
}

type pagedListResult struct {
	XMLName               xml.Name `xml:"ListBucketResult"`
	Xmlns                 string   `xml:"xmlns,attr"`
	Name                  string   `xml:"Name"`
	Prefix                string   `xml:"Prefix"`
	KeyCount              int      `xml:"KeyCount"`
	MaxKeys               int      `xml:"MaxKeys"`
	IsTruncated           bool     `xml:"IsTruncated"`
	NextContinuationToken string   `xml:"NextContinuationToken,omitempty"`
	Contents              []struct {
		Key  string `xml:"Key"`
		Size int64  `xml:"Size"`
	} `xml:"Contents"`
}

func (p *pagedS3) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet || r.URL.Query().Get("list-type") != "2" {
		http.Error(w, "only ListObjectsV2 is faked", http.StatusMethodNotAllowed)
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.pages++
	prefix := r.URL.Query().Get("prefix")
	start := 0
	if tok := r.URL.Query().Get("continuation-token"); tok != "" {
		start, _ = strconv.Atoi(tok)
	}
	var matching []string
	for _, k := range p.keys {
		if strings.HasPrefix(k, prefix) {
			matching = append(matching, k)
		}
	}
	sort.Strings(matching)
	end := start + p.pageSize
	if end > len(matching) {
		end = len(matching)
	}
	res := pagedListResult{Xmlns: "http://s3.amazonaws.com/doc/2006-03-01/", Name: "b", Prefix: prefix, MaxKeys: p.pageSize}
	for _, k := range matching[start:end] {
		res.Contents = append(res.Contents, struct {
			Key  string `xml:"Key"`
			Size int64  `xml:"Size"`
		}{Key: k, Size: 1})
	}
	res.KeyCount = len(res.Contents)
	if end < len(matching) {
		res.IsTruncated = true
		res.NextContinuationToken = strconv.Itoa(end)
	}
	w.Header().Set("Content-Type", "application/xml")
	_ = xml.NewEncoder(w).Encode(res)
}

func newPagedClient(t *testing.T, p *pagedS3) *s3.Client {
	t.Helper()
	srv := httptest.NewServer(p)
	t.Cleanup(srv.Close)
	return s3.New(s3.Options{
		BaseEndpoint: aws.String(srv.URL),
		Region:       "us-east-1",
		Credentials:  credentials.NewStaticCredentialsProvider("ak", "sk", ""),
		UsePathStyle: true,
		Retryer:      aws.NopRetryer{},
	})
}

func TestS3CompatList_StripsKeyPrefixHonoursPrefixReadsAllPages(t *testing.T) {
	p := &pagedS3{pageSize: 2, keys: []string{
		"personal-files/vaultaire/c1/a/1",
		"personal-files/vaultaire/c1/a/2",
		"personal-files/vaultaire/c1/a/3",
		"personal-files/vaultaire/c1/b/4",
		"personal-files/vaultaire/c1/b/5",
		"personal-files/vaultaire/c1-other/a/9", // sibling container sharing the prefix
		"personal-files/vaultaire/c1/",          // the "directory" marker some vendors list
	}}
	d := &S3CompatDriver{client: newPagedClient(t, p), bucket: "data", prefix: "personal-files/vaultaire", logger: zap.NewNop()}

	all, err := d.List(context.Background(), "c1", "")
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"a/1", "a/2", "a/3", "b/4", "b/5"}, all, "relative names, no sibling container, no empty name")
	assert.GreaterOrEqual(t, p.pages, 3, "five keys at two per page needs three pages")

	onlyA, err := d.List(context.Background(), "c1", "a/")
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"a/1", "a/2", "a/3"}, onlyA, "prefix is applied to the artifact part")

	none, err := d.List(context.Background(), "empty", "")
	require.NoError(t, err)
	assert.Empty(t, none)
}

func TestS3DriverList_ReadsAllPages(t *testing.T) {
	p := &pagedS3{pageSize: 2, keys: []string{"p/1", "p/2", "p/3", "p/4", "p/5", "q/6"}}
	d := &S3Driver{client: newPagedClient(t, p), logger: zap.NewNop()}

	got, err := d.List(context.Background(), "bucket", "p/")
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"p/1", "p/2", "p/3", "p/4", "p/5"}, got, "S3Driver returns keys as stored (quotaless strips)")
	assert.GreaterOrEqual(t, p.pages, 3)
}

func TestQuotalessList_ReadsAllPagesAndStrips(t *testing.T) {
	p := &pagedS3{pageSize: 2, keys: []string{
		"personal-files/c/a/1", "personal-files/c/a/2", "personal-files/c/a/3", "personal-files/c/b/4", "personal-files/c/b/5",
	}}
	q := &QuotalessDriver{S3Driver: &S3Driver{client: newPagedClient(t, p), logger: zap.NewNop()}, rootPath: "personal-files", maxRetries: 1, logger: zap.NewNop()}

	got, err := q.List(context.Background(), "c", "")
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"a/1", "a/2", "a/3", "b/4", "b/5"}, got)
	assert.GreaterOrEqual(t, p.pages, 3)
}

func TestGeyserList_ReadsAllPages(t *testing.T) {
	p := &pagedS3{pageSize: 2, keys: []string{
		"t-tn/c/a/1", "t-tn/c/a/2", "t-tn/c/a/3", "t-tn/c/b/4", "t-tn/c/b/5", "t-other/c/a/1",
	}}
	d := &GeyserDriver{client: newPagedClient(t, p), bucket: "b", tenantID: "vaultaire", logger: zap.NewNop()}
	ctx := common.WithTenantID(context.Background(), "tn")

	got, err := d.List(ctx, "c", "")
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"a/1", "a/2", "a/3", "b/4", "b/5"}, got)
	assert.GreaterOrEqual(t, p.pages, 3)

	onlyB, err := d.List(ctx, "c", "b/")
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"b/4", "b/5"}, onlyB)
}

// loopingS3 is a hostile gateway: every page says IsTruncated=true and hands
// back the SAME continuation token. Without StopOnDuplicateToken the SDK
// paginator would loop forever.
type loopingS3 struct {
	mu    sync.Mutex
	pages int
}

func (l *loopingS3) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	l.mu.Lock()
	l.pages++
	l.mu.Unlock()
	res := pagedListResult{Xmlns: "http://s3.amazonaws.com/doc/2006-03-01/", Name: "b", IsTruncated: true, NextContinuationToken: "same-token-forever", MaxKeys: 1, KeyCount: 1}
	res.Contents = append(res.Contents, struct {
		Key  string `xml:"Key"`
		Size int64  `xml:"Size"`
	}{Key: "personal-files/vaultaire/c/only", Size: 1})
	w.Header().Set("Content-Type", "application/xml")
	_ = xml.NewEncoder(w).Encode(res)
}

func TestS3List_TerminatesOnRepeatedContinuationToken(t *testing.T) {
	l := &loopingS3{}
	srv := httptest.NewServer(l)
	t.Cleanup(srv.Close)
	client := s3.New(s3.Options{
		BaseEndpoint: aws.String(srv.URL), Region: "us-east-1",
		Credentials:  credentials.NewStaticCredentialsProvider("ak", "sk", ""),
		UsePathStyle: true, Retryer: aws.NopRetryer{},
	})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Every S3-class driver goes through s3ListPaginator; two representatives.
	sc := &S3CompatDriver{client: client, bucket: "data", prefix: "personal-files/vaultaire", logger: zap.NewNop()}
	got, err := sc.List(ctx, "c", "")
	require.NoError(t, err)
	assert.Equal(t, []string{"only", "only"}, got, "two pages were read (the second repeats), then the walk stops")

	id := &IDriveDriver{client: client, bucket: "vaultaire", logger: zap.NewNop()}
	_, err = id.List(ctx, "c", "")
	require.NoError(t, err)

	l.mu.Lock()
	defer l.mu.Unlock()
	assert.LessOrEqual(t, l.pages, 4, "at most two pages per List against a token that never advances")
	assert.NoError(t, ctx.Err(), "finished well inside the deadline — not a timeout that masked a spin")
}
