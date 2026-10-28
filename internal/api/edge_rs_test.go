package api

// WP-EDGE-4 PR B (Stage 1a Prompt 48; plan 40.10 slice 3): RS k+m at a Go
// data-plane host, dark (flag edge_rs). A write of at least 8 MiB of an
// edge_rs tenant into an unversioned bucket, admitted for a host that says
// it stripes (feature "rs" — a node or host=slc, never the Worker: the
// SPEED bench put RS in a Worker at 26–29 MB/s), is answered K+M presigned
// shard PUTs spread over the legs with no leg holding more than M; the host
// encodes as the body streams and the commit records the head row on the
// `rs` sentinel and one blob_copies shard row per shard. Any K rebuild it.

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/FairForge/vaultaire/internal/crypto"
	"github.com/FairForge/vaultaire/internal/dataplane"
)

func TestRSShardLegs_NoLegHoldsMoreThanM(t *testing.T) {
	legs := rsShardLegs([]string{"a", "b", "c"}, 4, 2)
	require.Equal(t, []string{"a", "b", "c", "a", "b", "c"}, legs, "data shards first, round robin")
	count := map[string]int{}
	for _, l := range legs {
		count[l]++
	}
	for l, n := range count {
		assert.LessOrEqual(t, n, 2, l)
	}
	assert.Nil(t, rsShardLegs([]string{"a", "b"}, 4, 2), "two legs would hold three shards each: no RS (the mirror)")
	assert.Nil(t, rsShardLegs(nil, 4, 2))
}

// rsFixture is the edge fixture with three fake legs and edge_rs on.
func rsFixture(t *testing.T) (*edgeFixture, []*edgeFakeDriver, *dataplane.Host) {
	f := setupEdgeFixture(t)
	m := f.addMirrorLeg()
	third := newEdgeFakeDriver()
	third.name, third.urlHost = "edgethird", "third.backend.invalid"
	f.srv.engine.AddDriver(third.name, third)
	f.srv.mirrorLegs = []string{"edgemirror", "edgethird"}
	require.NoError(t, f.fl.Set(context.Background(), flagEdgeRS, f.tenantID, true, "test"))
	require.NoError(t, f.fl.Refresh(context.Background()))
	host := f.dataHost()
	host.Client = &http.Client{Transport: edgeFakeMux{f.drv, m, third}}
	host.Features = []string{dataplane.FeatureRS}
	return f, []*edgeFakeDriver{f.drv, m, third}, host
}

func (d *edgeFakeDriver) shardKeys(storageKey string) []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	var out []string
	for k := range d.objects {
		if strings.Contains(k, "/"+storageKey+".s") {
			out = append(out, k)
		}
	}
	return out
}

func (d *edgeFakeDriver) dropShards(storageKey string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	for k := range d.objects {
		if strings.Contains(k, "/"+storageKey+".s") {
			delete(d.objects, k)
		}
	}
}

func TestEdgeRS_AGoHostWritesKPlusMShardsAndAnyKRebuild(t *testing.T) {
	f, legs, host := rsFixture(t)
	body := crypto.GoldenBody(4848, 20<<20+123) // a stripe boundary mid-object
	res, err := host.Write(context.Background(), f.signedHTTP(http.MethodPut, "/"+f.bucket+"/rs.bin", nil, body, nil), bytes.NewReader(body), int64(len(body)))
	require.NoError(t, err)
	require.NotNil(t, res.Admit.RS, "%+v", res.Admit)
	require.Equal(t, http.StatusOK, res.Relay.Status, res.Relay.Body)
	sk := res.Admit.StorageKey
	for _, l := range legs {
		assert.Len(t, l.shardKeys(sk), 2, "%s holds two shards (≤ M)", l.name)
	}
	h, ok := f.head("rs.bin")
	require.True(t, ok)
	assert.Equal(t, rsBackendSentinel, h.backend)
	var shards int
	require.NoError(t, f.db.QueryRow(`SELECT COUNT(*) FROM blob_copies WHERE tenant_id = $1 AND storage_key = $2 AND kind = 'shard' AND state = 'ready' AND rs_k = 4 AND rs_m = 2`,
		f.tenantID, sk).Scan(&shards))
	assert.Equal(t, 6, shards)

	read := func() (int, []byte) {
		w := f.originGet("rs.bin", nil)
		return w.Code, w.Body.Bytes()
	}
	code, got := read()
	require.Equal(t, http.StatusOK, code)
	assert.True(t, bytes.Equal(body, got), "the data shards are the object")

	// A range across a stripe boundary.
	w := f.originGet("rs.bin", map[string]string{"Range": fmt.Sprintf("bytes=%d-%d", 8<<20-10, 8<<20+10)})
	require.Equal(t, http.StatusPartialContent, w.Code)
	assert.Equal(t, body[8<<20-10:8<<20+11], w.Body.Bytes())

	// A whole leg lost (two shards): rebuilt from the other four.
	legs[1].dropShards(sk)
	code, got = read()
	require.Equal(t, http.StatusOK, code)
	assert.True(t, bytes.Equal(body, got), "any K rebuild")
	// A second leg lost: fewer than K — never wrong bytes.
	legs[2].dropShards(sk)
	code, got = read()
	assert.NotEqual(t, http.StatusOK, code)
	assert.False(t, bytes.Equal(body, got))
}

func TestEdgeRS_DeleteAndOverwriteRemoveTheShards(t *testing.T) {
	f, legs, host := rsFixture(t)
	body := crypto.GoldenBody(4849, 9<<20)
	put := func(key string) string {
		res, err := host.Write(context.Background(), f.signedHTTP(http.MethodPut, "/"+f.bucket+"/"+key, nil, body, nil), bytes.NewReader(body), int64(len(body)))
		require.NoError(t, err)
		require.Equal(t, http.StatusOK, res.Relay.Status, res.Relay.Body)
		return res.Admit.StorageKey
	}
	sk := put("del.bin")
	w := f.s3(http.MethodDelete, "/"+f.bucket+"/del.bin", nil, nil)
	require.Equal(t, http.StatusNoContent, w.Code, w.Body.String())
	old := put("over.bin")
	put("over.bin") // overwrite: the old object's shards are no one's

	f.exec(`UPDATE blob_copies SET updated_at = NOW() - interval '2 hours' WHERE tenant_id = $1`, f.tenantID)
	res, err := f.copiesJob().RunOnce(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 12, res.Deleted, "%+v", res)
	for _, l := range legs {
		assert.Empty(t, l.shardKeys(sk), "%s: the deleted object's shards", l.name)
		assert.Empty(t, l.shardKeys(old), "%s: the overwritten object's shards", l.name)
	}
	w = f.originGet("over.bin", nil)
	require.Equal(t, http.StatusOK, w.Code)
	assert.True(t, bytes.Equal(body, w.Body.Bytes()), "the new object stands")
}

// Not RS: the flag off, a host that does not stripe (the Worker), a small
// body, a versioned bucket — the mirror (or one link) as before.
func TestEdgeRS_OnlyWhereItCanHold(t *testing.T) {
	f, _, host := rsFixture(t)
	big := crypto.GoldenBody(4850, 9<<20)
	admitOf := func(h *dataplane.Host, key string, body []byte) dataplane.AdmitResponse {
		res, err := h.Write(context.Background(), f.signedHTTP(http.MethodPut, "/"+f.bucket+"/"+key, nil, body, nil), bytes.NewReader(body), int64(len(body)))
		require.NoError(t, err)
		return res.Admit
	}
	worker := f.dataHost()
	worker.Client = host.Client
	assert.Nil(t, admitOf(worker, "w.bin", big).RS, "a host that does not stripe")
	assert.Nil(t, admitOf(host, "small.bin", testBytes(4<<20)).RS, "under 8 MiB: the mirror")
	assert.NotNil(t, admitOf(host, "rs.bin", big).RS, "the control: an RS write")
	f.exec(`UPDATE buckets SET versioning_status = 'Enabled' WHERE tenant_id = $1 AND name = $2`, f.tenantID, f.bucket)
	assert.Nil(t, admitOf(host, "v.bin", big).RS, "a versioned bucket")
	require.NoError(t, f.fl.Set(context.Background(), flagEdgeRS, f.tenantID, false, "test"))
	require.NoError(t, f.fl.Refresh(context.Background()))
	assert.Nil(t, admitOf(host, "off.bin", big).RS, "the flag off")
}

// lyingShard answers one shard's PUT 200 with a plausible ETag and stores
// nothing — a host (or backend) whose word is wrong.
type lyingShard struct {
	next   http.RoundTripper
	suffix string
}

func (l lyingShard) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.Method == http.MethodPut && strings.HasSuffix(strings.SplitN(r.URL.Path, "?", 2)[0], l.suffix) {
		_, _ = io.Copy(io.Discard, r.Body)
		_ = r.Body.Close()
		h := http.Header{}
		h.Set("ETag", `"`+strings.Repeat("ab", 16)+`"`)
		return &http.Response{StatusCode: http.StatusOK, Header: h, Body: io.NopCloser(strings.NewReader("")), Request: r}, nil
	}
	return l.next.RoundTrip(r)
}

// RP76-5: the commit HEADs every shard on its leg — a shard that is not
// there at its size and ETag fails the write (503); no head row.
func TestEdgeRS_TheCommitHeadsEveryShard(t *testing.T) {
	f, _, host := rsFixture(t)
	host.Client = &http.Client{Transport: lyingShard{next: host.Client.Transport, suffix: ".s2"}}
	body := crypto.GoldenBody(4849, 9<<20)
	res, err := host.Write(context.Background(), f.signedHTTP(http.MethodPut, "/"+f.bucket+"/lie.bin", nil, body, nil), bytes.NewReader(body), int64(len(body)))
	require.NoError(t, err)
	require.NotNil(t, res.Admit.RS)
	assert.Equal(t, http.StatusServiceUnavailable, res.Relay.Status, res.Relay.Body)
	_, ok := f.head("lie.bin")
	assert.False(t, ok, "no head row for an object a shard of which is missing")
}
