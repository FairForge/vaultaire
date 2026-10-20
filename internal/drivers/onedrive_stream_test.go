package drivers

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/FairForge/vaultaire/internal/common"
	"github.com/FairForge/vaultaire/internal/engine"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// WP-R7-4 R7-07: Get of an object of 10 MiB or more opened 2–8 ranges and
// io.ReadAll'd EVERY range before the pipe emitted them — the whole object in
// RAM per GET. Put without a length materialised the body and io.ReadAll'd it
// again. Now the ranges stream through a bounded window and the unknown-length
// Put uploads from the spooled body.

// odPattern is the byte at offset i of the generated objects.
func odPattern(i int64) byte { return byte((i*31 + i>>13) % 251) }

// odPatternReader yields odPattern bytes for [off, end) and counts what it gave.
type odPatternReader struct {
	off, end  int64
	delivered *atomic.Int64
}

func (p *odPatternReader) Read(b []byte) (int, error) {
	if p.off >= p.end {
		return 0, io.EOF
	}
	n := int64(len(b))
	if rest := p.end - p.off; n > rest {
		n = rest
	}
	for i := int64(0); i < n; i++ {
		b[i] = odPattern(p.off + i)
	}
	p.off += n
	if p.delivered != nil {
		p.delivered.Add(n)
	}
	return int(n), nil
}

// rangeCDN serves a generated object of size bytes at dlURL, honouring Range.
type rangeCDN struct {
	size      int64
	delivered atomic.Int64
	ignore    bool // answer every range 200 with the whole object
	mu        sync.Mutex
	ranges    []string
}

func (c *rangeCDN) RoundTrip(req *http.Request) (*http.Response, error) {
	rg := req.Header.Get("Range")
	c.mu.Lock()
	c.ranges = append(c.ranges, rg)
	c.mu.Unlock()
	if rg == "" || c.ignore {
		return &http.Response{StatusCode: 200, ContentLength: c.size, Header: make(http.Header), Request: req,
			Body: io.NopCloser(&odPatternReader{0, c.size, &c.delivered})}, nil
	}
	var s, e int64
	if _, err := fmt.Sscanf(rg, "bytes=%d-%d", &s, &e); err != nil {
		return nil, err
	}
	h := make(http.Header)
	h.Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", s, e, c.size))
	return &http.Response{StatusCode: 206, ContentLength: e - s + 1, Header: h, Request: req,
		Body: io.NopCloser(&odPatternReader{s, e + 1, &c.delivered})}, nil
}

// fleetWithCDN is a one-account fleet whose Graph metadata names dlURL and
// whose CDN is cdn.
func fleetWithCDN(ctx context.Context, t *testing.T, size int64, cdn http.RoundTripper) *OneDriveDriver {
	t.Helper()
	d, stubs := stubFleet(1, zap.NewNop())
	path := d.ObjectKey(ctx, "bkt", "big.bin")
	stubs[0].routes["GET "+itemURL(d.tenants[0], path)] =
		odRoute{200, `{"@microsoft.graph.downloadUrl":"https://cdn.example.test/big","size":` + strconv.FormatInt(size, 10) + `}`}
	d.tenants[0].cdnHTTP = &http.Client{Transport: cdn}
	return d
}

func patternSum(size int64) [32]byte {
	h := sha256.New()
	_, _ = io.Copy(h, &odPatternReader{0, size, nil})
	var out [32]byte
	copy(out[:], h.Sum(nil))
	return out
}

func TestOneDriveGet_RangesStreamThroughABoundedWindow(t *testing.T) {
	// Arrange: a 128 MiB object (8 streams).
	ctx := common.WithTenantID(context.Background(), "x")
	const size = 128 << 20
	cdn := &rangeCDN{size: size}
	d := fleetWithCDN(ctx, t, size, cdn)

	// Act: read 1 MiB, then let the fetchers run as far as they will.
	rc, err := d.Get(ctx, "bkt", "big.bin")
	require.NoError(t, err)
	defer func() { _ = rc.Close() }()
	first := make([]byte, 1<<20)
	_, err = io.ReadFull(rc, first)
	require.NoError(t, err)
	var last int64 = -1
	for i := 0; i < 50 && cdn.delivered.Load() != last; i++ {
		last = cdn.delivered.Load()
		time.Sleep(40 * time.Millisecond)
	}

	// Assert: what was fetched ahead is bounded by the window, never the
	// object (the old ranges read all 128 MiB while the caller held 1 MiB).
	assert.LessOrEqual(t, cdn.delivered.Load(), int64(odRangeWindow(size)+(1<<20)),
		"fetched ahead of a reader that took 1 MiB: at most the window")
	assert.Less(t, cdn.delivered.Load(), int64(size))

	// And the stream is the object, in order.
	h := sha256.New()
	h.Write(first)
	_, err = io.Copy(h, rc)
	require.NoError(t, err)
	var got [32]byte
	copy(got[:], h.Sum(nil))
	assert.Equal(t, patternSum(size), got)
}

func TestOneDriveGet_ARangeAnsweredWithTheWholeObjectIsAnError(t *testing.T) {
	ctx := common.WithTenantID(context.Background(), "x")
	const size = 24 << 20
	d := fleetWithCDN(ctx, t, size, &rangeCDN{size: size, ignore: true})

	rc, err := d.Get(ctx, "bkt", "big.bin")
	require.NoError(t, err)
	_, err = io.Copy(io.Discard, rc)
	_ = rc.Close()

	require.Error(t, err, "a 200 to a Range is never spliced into the stream")
}

func TestOneDriveGet_ASmallObjectIsOneStream(t *testing.T) {
	ctx := common.WithTenantID(context.Background(), "x")
	const size = 3 << 20
	cdn := &rangeCDN{size: size}
	d := fleetWithCDN(ctx, t, size, cdn)

	rc, err := d.Get(ctx, "bkt", "big.bin")
	require.NoError(t, err)
	b, err := io.ReadAll(rc)
	_ = rc.Close()

	require.NoError(t, err)
	assert.Len(t, b, size)
	assert.Equal(t, []string{""}, cdn.ranges, "one plain GET")
}

// uploadSink is a Graph fleet account that takes an upload session and
// verifies every fragment against odPattern.
type uploadSink struct {
	mu        sync.Mutex
	fragments []string
	bad       error
	received  int64
}

func (u *uploadSink) RoundTrip(req *http.Request) (*http.Response, error) {
	switch {
	case req.Method == http.MethodPost && strings.HasSuffix(req.URL.Path, ":/createUploadSession"):
		return odResp(req, 200, `{"uploadUrl":"https://upload.example.test/session"}`, nil), nil
	case req.Method == http.MethodPut && req.URL.Host == "upload.example.test":
		cr := req.Header.Get("Content-Range")
		var s, e, total int64
		_, _ = fmt.Sscanf(cr, "bytes %d-%d/%d", &s, &e, &total)
		buf := make([]byte, 1<<20)
		off := s
		for {
			n, err := req.Body.Read(buf)
			for i := 0; i < n; i++ {
				if buf[i] != odPattern(off+int64(i)) {
					u.mu.Lock()
					u.bad = fmt.Errorf("byte %d is wrong", off+int64(i))
					u.mu.Unlock()
					break
				}
			}
			off += int64(n)
			if err != nil {
				break
			}
		}
		u.mu.Lock()
		u.fragments = append(u.fragments, cr)
		u.received += off - s
		u.mu.Unlock()
		return odResp(req, 202, `{}`, nil), nil
	}
	return odResp(req, 404, `{"error":{"code":"itemNotFound"}}`, nil), nil
}

func uploadFleet(sink *uploadSink) *OneDriveDriver {
	d, _ := stubFleet(1, zap.NewNop())
	d.tenants[0].graphHTTP = &http.Client{Transport: sink}
	d.tenants[0].uploadHTTP = &http.Client{Transport: sink}
	d.tenants[0].rootFolderID = "ROOT"
	return d
}

func TestOneDrivePut_AnUnknownLengthUploadsFromTheSpooledBodyWithoutASecondCopy(t *testing.T) {
	if testing.Short() {
		t.Skip("writes a 400 MiB spool file")
	}
	// Arrange: a 400 MiB body of unknown length (no ContentLength option).
	ctx := common.WithTenantID(context.Background(), "x")
	const size = 400 << 20
	sink := &uploadSink{}
	d := uploadFleet(sink)
	before := totalAlloc()

	// Act
	err := d.Put(ctx, "bkt", "big.bin", io.MultiReader(&odPatternReader{0, size, nil}))

	// Assert: every byte arrived, in 60 MiB fragments, and the driver never
	// held the body in memory (it used to io.ReadAll the spool file: 400 MiB
	// and more allocated on top of the spool's own 64 MiB).
	require.NoError(t, err)
	require.NoError(t, sink.bad)
	assert.Equal(t, int64(size), sink.received)
	assert.Len(t, sink.fragments, 7, "6 × 60 MiB + 40 MiB")
	assert.Equal(t, fmt.Sprintf("bytes 0-%d/%d", odChunkSize-1, size), sink.fragments[0])
	// What the whole Put allocated: materialize's one 64 MiB spool (RP17-2:
	// it grew a bytes.Buffer to ~256 MiB, ~512 under -race, to hold 64 MiB)
	// plus one 60 MiB fragment buffer (pooled), never the body (a
	// whole-body copy alone is >= size).
	assert.Less(t, totalAlloc()-before, uint64(140<<20), "the spool and one fragment, no whole-body buffer")
}

func TestOneDrivePut_AKnownLengthStreamsToo(t *testing.T) {
	ctx := common.WithTenantID(context.Background(), "x")
	const size = 70 << 20
	sink := &uploadSink{}
	d := uploadFleet(sink)

	err := d.Put(ctx, "bkt", "big.bin", &odPatternReader{0, size, nil}, engine.WithContentLength(size))

	require.NoError(t, err)
	require.NoError(t, sink.bad)
	assert.Equal(t, int64(size), sink.received)
	assert.Len(t, sink.fragments, 2)
}

// R7-06: keys with '/' are nested folders, and List returned the container's
// direct children only — folder names in place of keys, nested keys never.
// The contract: keys relative to the container, prefix honoured, all pages,
// the union of the fleet.

type odTree map[string][]string // folder path under the container → children ("name/" = folder)

func childrenURLOf(tn *odTenant, rel string) string {
	p := odRootFolder + "/t-x/c"
	if rel != "" {
		p += "/" + rel
	}
	return graphBase + "/drives/" + tn.driveID + "/items/root:/" + odEscapePath(p) + ":/children?$top=999"
}

func (tr odTree) install(stub *odRouteStub, tn *odTenant) {
	for folder, kids := range tr {
		var items []string
		for _, k := range kids {
			if strings.HasSuffix(k, "/") {
				items = append(items, `{"name":"`+strings.TrimSuffix(k, "/")+`","folder":{"childCount":1}}`)
			} else {
				items = append(items, `{"name":"`+k+`","file":{}}`)
			}
		}
		stub.routes["GET "+childrenURLOf(tn, folder)] = odRoute{200, `{"value":[` + strings.Join(items, ",") + `]}`}
	}
}

func TestOneDriveList_IsRecursiveRelativeToTheContainerAndTheFleetsUnion(t *testing.T) {
	ctx := common.WithTenantID(context.Background(), "x")
	d, stubs := stubFleet(2, zap.NewNop())
	odTree{
		"":            {"photos/", "k.txt", "other/"},
		"photos":      {"2024/", "a.jpg"},
		"photos/2024": {"b.jpg"},
		"other":       {"z.bin"},
	}.install(stubs[0], d.tenants[0])
	odTree{
		"":       {"photos/"},
		"photos": {"c.jpg", "a.jpg"},
	}.install(stubs[1], d.tenants[1])

	all, err := d.List(ctx, "c", "")
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"k.txt", "photos/a.jpg", "photos/2024/b.jpg", "photos/c.jpg", "other/z.bin"}, all,
		"every key, relative to the container, once")

	for _, st := range stubs {
		st.mu.Lock()
		st.calls = nil
		st.mu.Unlock()
	}
	some, err := d.List(ctx, "c", "photos/2")
	require.NoError(t, err)
	assert.Equal(t, []string{"photos/2024/b.jpg"}, some)
	for _, call := range stubs[0].calls {
		assert.NotContains(t, call, "/t-x/c/other", "a folder outside the prefix is never listed")
	}
}

func TestOneDriveList_FollowsEveryPageOfANestedFolder(t *testing.T) {
	ctx := common.WithTenantID(context.Background(), "x")
	d, stubs := stubFleet(1, zap.NewNop())
	tn := d.tenants[0]
	next := graphBase + "/drives/" + tn.driveID + "/items/F/children?$skiptoken=2"
	odTree{"": {"deep/"}}.install(stubs[0], tn)
	stubs[0].routes["GET "+childrenURLOf(tn, "deep")] = odRoute{200, `{"value":[{"name":"1","file":{}}],"@odata.nextLink":"` + next + `"}`}
	stubs[0].routes["GET "+next] = odRoute{200, `{"value":[{"name":"2","file":{}}]}`}

	keys, err := d.List(ctx, "c", "")

	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"deep/1", "deep/2"}, keys)
}

func TestOneDriveList_AFolderThatFailsFailsTheListing(t *testing.T) {
	ctx := common.WithTenantID(context.Background(), "x")
	d, stubs := stubFleet(1, zap.NewNop())
	tn := d.tenants[0]
	odTree{"": {"deep/"}}.install(stubs[0], tn)
	stubs[0].routes["GET "+childrenURLOf(tn, "deep")] = odRoute{403, `{"error":{"code":"accessDenied"}}`}

	_, err := d.List(ctx, "c", "")

	require.Error(t, err, "never a partial listing read as complete")
}

// The erasure sweep (List + Delete per bucket) used to delete each direct
// child — a folder name deleted its subtree. With List returning keys, the
// sweep deletes every key, then RemoveContainerTree takes the empty folders
// (their names are the customer's key prefixes) on every account.
func TestOneDriveRemoveContainerTree_RemovesTheEmptiedFolderOnEveryAccount(t *testing.T) {
	ctx := common.WithTenantID(context.Background(), "x")
	d, stubs := stubFleet(2, zap.NewNop())
	for i, tn := range d.tenants {
		odTree{"": {"photos/"}, "photos": {}}.install(stubs[i], tn)
		stubs[i].routes["GET "+itemURL(tn, "t-x/c")] = odRoute{200, `{"id":"C-` + tn.name + `"}`}
		stubs[i].routes["DELETE "+graphBase+"/drives/"+tn.driveID+"/items/C-"+tn.name] = odRoute{204, ""}
	}

	require.NoError(t, d.RemoveContainerTree(ctx, "c"))

	for i, tn := range d.tenants {
		assert.Contains(t, stubs[i].calls, "DELETE "+graphBase+"/drives/"+tn.driveID+"/items/C-"+tn.name)
	}
}

func TestOneDriveRemoveContainerTree_NeverWhileAKeyRemains(t *testing.T) {
	ctx := common.WithTenantID(context.Background(), "x")
	d, stubs := stubFleet(2, zap.NewNop())
	odTree{"": {"photos/"}, "photos": {"left.jpg"}}.install(stubs[1], d.tenants[1])

	err := d.RemoveContainerTree(ctx, "c")

	require.ErrorIs(t, err, ErrDirNotEmpty)
	assert.False(t, stubs[0].sawMethod("DELETE"))
	assert.False(t, stubs[1].sawMethod("DELETE"))
}

// R7-18: HealthCheck probed ONE rotating account per call, so a dead account
// was seen 1/N of the time. Every account is probed, the worst reported, the
// dead one named.
func TestOneDriveHealthCheck_ADeadAccountFailsEveryProbeAndIsNamed(t *testing.T) {
	d, stubs := stubFleet(3, zap.NewNop())
	for i, tn := range d.tenants {
		status, body := 200, `{"id":"`+tn.driveID+`"}`
		if i == 1 {
			status, body = 401, `{"error":{"code":"InvalidAuthenticationToken"}}`
		}
		stubs[i].routes["GET "+graphBase+"/drives/"+tn.driveID] = odRoute{status, body}
	}

	for i := 0; i < 3; i++ {
		err := d.HealthCheck(context.Background())
		require.Error(t, err, "probe %d: a dead account is seen on every probe", i)
		assert.Contains(t, err.Error(), d.tenants[1].name)
		assert.NotContains(t, err.Error(), d.tenants[0].name+":")
	}
	for i := range stubs {
		assert.True(t, stubs[i].sawMethod("GET"), "account %d probed", i)
	}
}

func TestOneDriveHealthCheck_AllHealthyIsNil(t *testing.T) {
	d, stubs := stubFleet(3, zap.NewNop())
	for i, tn := range d.tenants {
		stubs[i].routes["GET "+graphBase+"/drives/"+tn.driveID] = odRoute{200, `{}`}
	}
	require.NoError(t, d.HealthCheck(context.Background()))
}

func TestOneDriveHealthCheck_AHungAccountIsBoundedByTheProbe(t *testing.T) {
	d, stubs := stubFleet(2, zap.NewNop())
	stubs[0].routes["GET "+graphBase+"/drives/"+d.tenants[0].driveID] = odRoute{200, `{}`}
	d.tenants[1].graphHTTP = &http.Client{Transport: &odScriptRT{fn: func(_ int, req *http.Request) (*http.Response, error) {
		<-req.Context().Done()
		return nil, req.Context().Err()
	}}}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	start := time.Now()
	err := d.HealthCheck(ctx)

	require.Error(t, err)
	assert.Contains(t, err.Error(), d.tenants[1].name)
	assert.Less(t, time.Since(start), 2*time.Second)
}
