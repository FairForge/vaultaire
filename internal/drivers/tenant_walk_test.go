package drivers

import (
	"context"
	"encoding/xml"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/FairForge/vaultaire/internal/common"
	"github.com/FairForge/vaultaire/internal/engine"
)

// WP-R10-3c: engine.TenantWalker on every driver that can list a tenant.
// What is proved per driver, against a fake endpoint that pages:
//   - the walk returns the tenant's keys in EVERY container, on every page;
//   - a neighbour whose id shares the prefix (tenant-ab vs tenant-abc, and
//     tenant-a) is never listed and never deleted;
//   - List of one container does not match a container sharing its prefix
//     (X_b vs X_bc);
//   - Remove deletes exactly the listed key;
//   - "" and ids with '/' are refused before any request;
//   - a listing that fails on a later page, repeats its token or returns a
//     key from outside the prefix is an error, never a short success.

// walkS3 is a fake S3 endpoint: ListObjectsV2 (paged) and DeleteObject over
// one in-memory key set, with switchable faults.
type walkS3 struct {
	mu       sync.Mutex
	keys     map[string]bool
	pageSize int
	requests int
	deleted  []string

	failPage    int    // 1-based list request that answers 500 (0 = never)
	repeatToken bool   // every truncated page hands back the same token
	foreignKey  string // appended to the first page whatever the prefix
}

func newWalkS3(pageSize int, keys ...string) *walkS3 {
	w := &walkS3{keys: map[string]bool{}, pageSize: pageSize}
	for _, k := range keys {
		w.keys[k] = true
	}
	return w
}

func (w *walkS3) has(key string) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.keys[key]
}

func (w *walkS3) remaining() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := make([]string, 0, len(w.keys))
	for k := range w.keys {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func (w *walkS3) ServeHTTP(rw http.ResponseWriter, r *http.Request) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.requests++
	// Path style: /<bucket>/<key…>
	parts := strings.SplitN(strings.TrimPrefix(r.URL.Path, "/"), "/", 2)
	switch {
	case r.Method == http.MethodDelete && len(parts) == 2:
		delete(w.keys, parts[1])
		w.deleted = append(w.deleted, parts[1])
		rw.WriteHeader(http.StatusNoContent)
	case r.Method == http.MethodGet && r.URL.Query().Get("list-type") == "2":
		w.list(rw, r)
	default:
		http.Error(rw, "not faked", http.StatusMethodNotAllowed)
	}
}

func (w *walkS3) list(rw http.ResponseWriter, r *http.Request) {
	prefix := r.URL.Query().Get("prefix")
	// The token is the last key served: listing resumes after it, so keys
	// deleted between two pages do not shift the window.
	after := r.URL.Query().Get("continuation-token")
	var matching []string
	if w.repeatToken {
		after = "" // a gateway that keeps serving the first page
	}
	for k := range w.keys {
		if strings.HasPrefix(k, prefix) && k > after {
			matching = append(matching, k)
		}
	}
	sort.Strings(matching)
	w.failPage--
	if w.failPage == 0 {
		http.Error(rw, "<Error><Code>InternalError</Code><Message>page lost</Message></Error>", http.StatusInternalServerError)
		return
	}
	end := w.pageSize
	if end > len(matching) {
		end = len(matching)
	}
	res := pagedListResult{Xmlns: "http://s3.amazonaws.com/doc/2006-03-01/", Name: "b", Prefix: prefix, MaxKeys: w.pageSize}
	add := func(k string) {
		res.Contents = append(res.Contents, struct {
			Key  string `xml:"Key"`
			Size int64  `xml:"Size"`
		}{Key: k, Size: 1})
	}
	for _, k := range matching[:end] {
		add(k)
	}
	if w.foreignKey != "" && after == "" {
		add(w.foreignKey)
	}
	res.KeyCount = len(res.Contents)
	if end < len(matching) {
		res.IsTruncated = true
		res.NextContinuationToken = matching[end-1]
		if w.repeatToken {
			res.NextContinuationToken = "stuck"
		}
	}
	rw.Header().Set("Content-Type", "application/xml")
	_ = xml.NewEncoder(rw).Encode(res)
}

func walkClient(t *testing.T, w *walkS3) *s3.Client {
	t.Helper()
	srv := httptest.NewServer(w)
	t.Cleanup(srv.Close)
	return s3.New(s3.Options{
		BaseEndpoint: aws.String(srv.URL),
		Region:       "us-east-1",
		Credentials:  credentials.NewStaticCredentialsProvider("ak", "sk", ""),
		UsePathStyle: true,
		Retryer:      aws.NopRetryer{},
	})
}

// walkable is what the sweep uses of a driver.
type walkable interface {
	engine.Driver
	engine.TenantWalker
}

// s3WalkCase builds one S3-class driver on the fake and says how it keys
// (tenant, container, artifact).
type s3WalkCase struct {
	name  string
	build func(c *s3.Client) walkable
	key   func(tenant, container, artifact string) string
}

func s3WalkCases() []s3WalkCase {
	log := zap.NewNop()
	tPrefixed := func(tenant, container, artifact string) string {
		return "t-" + tenant + "/" + container + "/" + artifact
	}
	return []s3WalkCase{
		{"idrive", func(c *s3.Client) walkable { return &IDriveDriver{client: c, bucket: "vaultaire", logger: log} }, tPrefixed},
		{"lyve", func(c *s3.Client) walkable { return &LyveDriver{client: c, region: "us-east-1", logger: log} }, tPrefixed},
		{"geyser", func(c *s3.Client) walkable { return &GeyserDriver{client: c, bucket: "tape", logger: log} }, tPrefixed},
		{"r2", func(c *s3.Client) walkable { return &R2Driver{client: c, bucket: "public", logger: log} }, tPrefixed},
		{"s3compat", func(c *s3.Client) walkable {
			return &S3CompatDriver{client: c, bucket: "data", prefix: "personal-files/vaultaire", logger: log}
		}, func(_, container, artifact string) string {
			return "personal-files/vaultaire/" + container + "/" + artifact
		}},
		{"quotaless", func(c *s3.Client) walkable {
			return &QuotalessDriver{S3Driver: &S3Driver{client: c, logger: log}, rootPath: "personal-files", maxRetries: 1, logger: log}
		}, func(_, container, artifact string) string { return "personal-files/" + container + "/" + artifact }},
	}
}

const (
	walkTenant    = "tenant-ab"
	walkNeighbour = "tenant-abc" // shares the id as a prefix
	walkShorter   = "tenant-a"   // is a prefix of the id
)

type walked struct{ container, artifact string }

func collect(t *testing.T, d engine.TenantWalker, tenant string, remove bool) []walked {
	t.Helper()
	var got []walked
	err := d.WalkTenant(context.Background(), tenant, func(o engine.TenantObject) error {
		got = append(got, walked{o.Container, o.Artifact})
		if remove {
			return o.Remove(context.Background())
		}
		return nil
	})
	require.NoError(t, err)
	return got
}

func TestWalkTenant_S3Class_ListsTheTenantOnlyAndRemovesExactlyWhatItListed(t *testing.T) {
	for _, tc := range s3WalkCases() {
		t.Run(tc.name, func(t *testing.T) {
			// Arrange — two buckets whose names share a prefix, a container
			// no table remembers, and three neighbours.
			mine := []string{
				tc.key(walkTenant, walkTenant+"_b", "k1"),
				tc.key(walkTenant, walkTenant+"_b", "dir/k2"),
				tc.key(walkTenant, walkTenant+"_b", "dir/k3"),
				tc.key(walkTenant, walkTenant+"_bc", "k4"),
				tc.key(walkTenant, walkTenant+"_forgotten", "k5"),
			}
			theirs := []string{
				tc.key(walkNeighbour, walkNeighbour+"_b", "k1"),
				tc.key(walkNeighbour, walkNeighbour+"_bc", "k4"),
				tc.key(walkShorter, walkShorter+"_b", "k1"),
				tc.key("default", "_global", "_chunks/h0"),
			}
			fake := newWalkS3(2, append(append([]string{}, mine...), theirs...)...)
			d := tc.build(walkClient(t, fake))
			ctx := common.WithTenantID(context.Background(), walkTenant)

			// Act + Assert — List of one container stops at the container boundary.
			names, err := d.List(ctx, walkTenant+"_b", "")
			require.NoError(t, err)
			assert.ElementsMatch(t, []string{"k1", "dir/k2", "dir/k3"}, names, "X_b must not match X_bc")

			// The walk sees every container of the tenant, on every page, and nobody else's.
			got := collect(t, d, walkTenant, false)
			assert.ElementsMatch(t, []walked{
				{walkTenant + "_b", "k1"}, {walkTenant + "_b", "dir/k2"}, {walkTenant + "_b", "dir/k3"},
				{walkTenant + "_bc", "k4"}, {walkTenant + "_forgotten", "k5"},
			}, got)

			// The pair addresses the same object as the listed key: Delete by
			// pair removes it (the fallback path), Remove removes the rest.
			require.NoError(t, d.Delete(ctx, walkTenant+"_b", "k1"))
			assert.False(t, fake.has(mine[0]), "Delete(container, artifact) addresses the walked key")
			collect(t, d, walkTenant, true)
			assert.Equal(t, func() []string { s := append([]string{}, theirs...); sort.Strings(s); return s }(), fake.remaining(),
				"every key of the tenant is gone and every neighbour key survives")
			for _, k := range fake.deleted {
				assert.Contains(t, mine, k, "a delete was sent for a key that is not the tenant's")
			}
		})
	}
}

// On a fixed-bucket backend the chunk blobs a tenant's requests wrote sit
// under that tenant's prefix: the walk reports them (container `_global`) so
// the caller can leave them — it must be able to tell.
func TestWalkTenant_FixedBucket_ReportsTheChunkContainerAsItsOwnContainer(t *testing.T) {
	fake := newWalkS3(10,
		"t-"+walkTenant+"/_global/_chunks/aa",
		"t-"+walkTenant+"/"+walkTenant+"_b/k1",
		"t-"+walkTenant+"/loose-key", // no container segment: nothing this code writes
	)
	d := &IDriveDriver{client: walkClient(t, fake), bucket: "vaultaire", logger: zap.NewNop()}

	got := collect(t, d, walkTenant, false)

	assert.ElementsMatch(t, []walked{{"_global", "_chunks/aa"}, {walkTenant + "_b", "k1"}, {"", "loose-key"}}, got)
}

func TestWalkTenant_RefusesAnEmptyOrPathLikeTenantBeforeAnyRequest(t *testing.T) {
	for _, tc := range s3WalkCases() {
		t.Run(tc.name, func(t *testing.T) {
			fake := newWalkS3(10, tc.key("default", "c", "k"), tc.key("x", "x_c", "k"))
			d := tc.build(walkClient(t, fake))
			for _, bad := range []string{"", "a/b", "/", "tenant-ab/"} {
				err := d.WalkTenant(context.Background(), bad, func(engine.TenantObject) error {
					t.Fatalf("walk of %q handed out an object", bad)
					return nil
				})
				assert.ErrorIs(t, err, ErrWalkTenantID, "tenant %q", bad)
			}
			assert.Zero(t, fake.requests, "no request may leave for a refused tenant")
		})
	}
	local := NewLocalDriver(t.TempDir(), zap.NewNop())
	for _, bad := range []string{"", "a/b"} {
		assert.ErrorIs(t, local.WalkTenant(context.Background(), bad, func(engine.TenantObject) error { return nil }), ErrWalkTenantID)
	}
}

func TestWalkTenant_AListingThatCannotBeCompletedIsAnError(t *testing.T) {
	keys := func(tc s3WalkCase) []string {
		var ks []string
		for i := 0; i < 7; i++ {
			ks = append(ks, tc.key(walkTenant, walkTenant+"_b", "k"+strconv.Itoa(i)))
		}
		return ks
	}
	for _, tc := range s3WalkCases() {
		t.Run(tc.name+"/page 3 fails", func(t *testing.T) {
			fake := newWalkS3(2, keys(tc)...)
			fake.failPage = 3
			d := tc.build(walkClient(t, fake))
			seen := 0
			err := d.WalkTenant(context.Background(), walkTenant, func(engine.TenantObject) error { seen++; return nil })
			require.Error(t, err, "a lost page must not read as the end of the listing")
			assert.Equal(t, 4, seen, "two pages were handed out before the failure")
		})
		t.Run(tc.name+"/token does not advance", func(t *testing.T) {
			fake := newWalkS3(2, keys(tc)...)
			fake.repeatToken = true
			d := tc.build(walkClient(t, fake))
			err := d.WalkTenant(context.Background(), walkTenant, func(engine.TenantObject) error { return nil })
			require.Error(t, err, "s3ListPaginator would stop here and report a finished listing")
			assert.Contains(t, err.Error(), "did not advance")
		})
		t.Run(tc.name+"/key from outside the prefix", func(t *testing.T) {
			fake := newWalkS3(10, keys(tc)[:1]...)
			fake.foreignKey = tc.key(walkNeighbour, walkNeighbour+"_b", "theirs")
			d := tc.build(walkClient(t, fake))
			var got []walked
			err := d.WalkTenant(context.Background(), walkTenant, func(o engine.TenantObject) error {
				got = append(got, walked{o.Container, o.Artifact})
				return nil
			})
			require.Error(t, err)
			assert.Contains(t, err.Error(), "outside the prefix")
			assert.Equal(t, []walked{{walkTenant + "_b", "k0"}}, got, "the foreign key is never handed out")
		})
	}
}

func TestWalkTenant_Local_EveryFileOfTheTenantAndNoNeighbour(t *testing.T) {
	// Arrange
	base := t.TempDir()
	write := func(rel string) string {
		p := filepath.Join(base, filepath.FromSlash(rel))
		require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o750))
		require.NoError(t, os.WriteFile(p, []byte("x"), 0o600))
		return p
	}
	mine := []string{
		write(walkTenant + "_b/k1"),
		write(walkTenant + "_b/dir/k2"),
		write(walkTenant + "_bc/k3"),
		write(walkTenant + "_forgotten/k4"),
		write(walkTenant + "_b/.tmp-123456"), // an AtomicWrite temp file List hides
		write(walkTenant + "_b/legacy.meta"), // a sidecar List hides
	}
	theirs := []string{
		write(walkNeighbour + "_b/k1"),
		write(walkShorter + "_b/k1"),
		write("_global/_chunks/h1"),
		write(walkTenant + "x_b/k1"),
	}
	d := NewLocalDriver(base, zap.NewNop())

	// Act
	listed, err := d.List(context.Background(), walkTenant+"_b", "")
	require.NoError(t, err)
	got := collect(t, d, walkTenant, true)

	// Assert
	assert.ElementsMatch(t, []string{"k1", "dir/k2"}, listed, "List: one container, hidden files not objects")
	assert.Len(t, got, len(mine), "the walk hands out hidden files too: they are bytes of the tenant")
	for _, p := range mine {
		_, err := os.Stat(p)
		assert.True(t, os.IsNotExist(err), "%s must be gone", p)
	}
	for _, p := range theirs {
		_, err := os.Stat(p)
		assert.NoError(t, err, "%s is not the tenant's and must survive", p)
	}
	assert.NoError(t, d.WalkTenant(context.Background(), "nobody", func(engine.TenantObject) error {
		t.Fatal("a tenant with nothing has nothing to walk")
		return nil
	}))
}
