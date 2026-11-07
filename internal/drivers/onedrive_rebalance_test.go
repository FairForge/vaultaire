package drivers

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"testing"

	"github.com/FairForge/vaultaire/internal/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// WP-FLEET-5 PR B: the rebalance (onedrive_rebalance.go) on a fake fleet.

// fleetStore is one account holding files by path under vaultaire/, answering
// what a rebalance asks of Graph: children listings with size and
// quickXorHash, item metadata by path (id, size, hash, download URL), the
// pre-authenticated download, the simple upload, permanentDelete.
type fleetStore struct {
	*markerStore
	listings  int
	puts      int
	deletes   int
	serveBody map[string]string // CDN body overrides (a corrupt read)
}

func newFleetStores(names ...string) []*fleetStore {
	out := make([]*fleetStore, len(names))
	for i, n := range names {
		out[i] = &fleetStore{markerStore: &markerStore{name: n, files: map[string]string{}}, serveBody: map[string]string{}}
	}
	return out
}

func qxOf(body string) string {
	h := NewQuickXorHash()
	_, _ = h.Write([]byte(body))
	return base64.StdEncoding.EncodeToString(h.Sum(nil))
}

func (f *fleetStore) itemJSON(p string) string {
	body := f.files[p]
	return fmt.Sprintf(`{"id":"id:%s","size":%d,"file":{"hashes":{"quickXorHash":%q}},"@microsoft.graph.downloadUrl":"https://cdn.example.test/%s/%s"}`,
		p, len(body), qxOf(body), f.name, p)
}

func (f *fleetStore) RoundTrip(req *http.Request) (*http.Response, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	u := req.URL.String()
	pathPart := req.URL.Path
	switch {
	case req.Method == http.MethodGet && strings.HasSuffix(pathPart, ":/children"):
		f.listings++
		folder := pathPart[strings.Index(pathPart, "/items/root:/")+len("/items/root:/"):]
		folder = strings.TrimSuffix(folder, ":/children")
		folder = strings.TrimPrefix(folder, odRootFolder)
		folder = strings.TrimPrefix(folder, "/")
		var items []string
		seen := map[string]bool{}
		for p := range f.files {
			rest := p
			if folder != "" {
				if !strings.HasPrefix(p, folder+"/") {
					continue
				}
				rest = strings.TrimPrefix(p, folder+"/")
			}
			name, _, isDir := strings.Cut(rest, "/")
			if seen[name] {
				continue
			}
			seen[name] = true
			if isDir {
				items = append(items, fmt.Sprintf(`{"name":%q,"folder":{}}`, name))
			} else {
				items = append(items, fmt.Sprintf(`{"name":%q,"size":%d,"file":{"hashes":{"quickXorHash":%q}}}`, name, len(f.files[p]), qxOf(f.files[p])))
			}
		}
		if len(items) == 0 && folder != "" {
			return odResp(req, 404, `{"error":{"code":"itemNotFound"}}`, nil), nil
		}
		sort.Strings(items)
		return odResp(req, 200, `{"value":[`+strings.Join(items, ",")+`]}`, nil), nil
	case req.Method == http.MethodGet && strings.Contains(pathPart, "/items/root:/"+odRootFolder+"/"):
		f.gets++
		p := pathPart[strings.Index(pathPart, "/items/root:/"+odRootFolder+"/")+len("/items/root:/"+odRootFolder+"/"):]
		p = strings.TrimSuffix(p, ":/content")
		if _, ok := f.files[p]; !ok {
			return odResp(req, 404, `{"error":{"code":"itemNotFound"}}`, nil), nil
		}
		return odResp(req, 200, f.itemJSON(p), nil), nil
	case req.Method == http.MethodGet && strings.HasPrefix(u, "https://cdn.example.test/"+f.name+"/"):
		p := strings.TrimPrefix(u, "https://cdn.example.test/"+f.name+"/")
		body, ok := f.serveBody[p]
		if !ok {
			body = f.files[p]
		}
		return odResp(req, 200, body, nil), nil
	case req.Method == http.MethodPut && strings.HasSuffix(pathPart, ":/content"):
		i := strings.Index(pathPart, "/items/ROOT:/")
		p := strings.TrimSuffix(pathPart[i+len("/items/ROOT:/"):], ":/content")
		b, _ := io.ReadAll(req.Body)
		f.files[p] = string(b)
		f.puts++
		return odResp(req, 201, f.itemJSON(p), nil), nil
	case req.Method == http.MethodPost && strings.HasSuffix(pathPart, "/permanentDelete"):
		id := strings.TrimSuffix(pathPart[strings.Index(pathPart, "/items/")+len("/items/"):], "/permanentDelete")
		p := strings.TrimPrefix(id, "id:")
		if _, ok := f.files[p]; !ok {
			return odResp(req, 404, `{"error":{"code":"itemNotFound"}}`, nil), nil
		}
		delete(f.files, p)
		f.deletes++
		return odResp(req, 204, "", nil), nil
	}
	return odResp(req, 404, `{"error":{"code":"itemNotFound"}}`, nil), nil
}

// rebalanceFleet builds a driver over the stores, grouped into tenants
// (group k = every store at position ≡ k mod tenants).
func rebalanceFleet(stores []*fleetStore, tenants int) *OneDriveDriver {
	d := &OneDriveDriver{logger: zap.NewNop()}
	for i, st := range stores {
		tn := stubTenant(st.name, st)
		tn.tenantID = fmt.Sprintf("tid-%d", i%tenants)
		tn.userUPN = st.name + "@fleet.test"
		tn.rootFolderID = "ROOT"
		d.tenants = append(d.tenants, tn)
	}
	return d
}

func filesOn(st *fleetStore) (n int) {
	st.mu.Lock()
	defer st.mu.Unlock()
	for p := range st.files {
		if !strings.HasPrefix(p, odFleetFolder+"/") {
			n++
		}
	}
	return n
}

func TestRebalance_MovesAVersionOneFleetToVersionTwoAndCommits(t *testing.T) {
	ctx := common.WithTenantID(context.Background(), "cust")
	stores := newFleetStores("a", "b", "c", "d")
	// A version-1 fleet: markers written as before the field, 40 shards
	// placed by hash % slots.
	v1 := rebalanceFleet(stores, 2)
	_, err := v1.CheckFleetFingerprint(ctx, "")
	require.NoError(t, err)
	for _, st := range stores {
		rewriteMarker(t, st.markerStore, func(m *fleetMarker) { m.PlacementVersion = 0; m.Tenants = nil })
	}
	v1 = rebalanceFleet(stores, 2)
	_, err = v1.CheckFleetFingerprint(ctx, "")
	require.NoError(t, err)
	require.Equal(t, 1, v1.placement().version)
	want := map[string]string{}
	for g := 0; g < 10; g++ {
		for j := 0; j < 4; j++ {
			key := fmt.Sprintf("%08x/etag/p%d", g*7919, j)
			body := strings.Repeat(fmt.Sprintf("%d-%d|", g, j), 50)
			want["t-cust/parity/"+key] = body
			require.NoError(t, v1.Put(ctx, "parity", key, strings.NewReader(body)))
		}
	}

	// The plan: every file not at its version-2 home.
	tool := rebalanceFleet(stores, 2)
	plan, err := tool.RebalancePlan(ctx, 2)
	require.NoError(t, err)
	assert.Equal(t, 40, plan.Files)
	assert.Equal(t, 1, plan.Laid)
	assert.Zero(t, plan.Target)
	assert.True(t, plan.Placed, "placed under version 1: authoritative today")
	require.NotEmpty(t, plan.Moves)
	assert.Less(t, len(plan.Moves), 40, "some files are already where version 2 puts them")
	assert.Empty(t, plan.Strays, "every file is at its version-1 home")
	v2 := tool.placementOver(2)
	for _, m := range plan.Moves {
		assert.Equal(t, tool.tenants[m.To].identity(), v2.home(m.Path).identity, m.Path)
		assert.NotEqual(t, m.From, m.To)
		assert.Equal(t, qxOf(want[m.Path]), m.Hash)
	}

	// -run: the target set, then every move.
	require.NoError(t, tool.RebalanceSetTarget(ctx, 2))
	mid := rebalanceFleet(stores, 2)
	_, err = mid.CheckFleetFingerprint(ctx, "")
	require.NoError(t, err)
	assert.False(t, mid.authoritative(), "a rebalance is running")
	assert.Equal(t, 2, mid.placement().version)
	for _, m := range plan.Moves {
		require.NoError(t, tool.RebalanceMove(ctx, m), m.Path)
	}
	// A cut run resumes: every move again is a no-op (the source is gone).
	for _, m := range plan.Moves {
		require.NoError(t, tool.RebalanceMove(ctx, m), m.Path)
	}
	again, err := tool.RebalancePlan(ctx, 2)
	require.NoError(t, err)
	assert.Empty(t, again.Moves)
	assert.Equal(t, 40, again.Files, "nothing lost, nothing doubled")

	// -commit.
	done, err := tool.RebalanceCommit(ctx, 2)
	require.NoError(t, err)
	assert.Empty(t, done.Moves)
	fresh := rebalanceFleet(stores, 2)
	fp, err := fresh.CheckFleetFingerprint(ctx, "")
	require.NoError(t, err)
	assert.Equal(t, FleetMatch, fp.State)
	assert.True(t, fresh.authoritative())
	assert.Equal(t, 2, fresh.placement().version)
	m := stores[0].marker(t)
	assert.Equal(t, 2, m.PlacementVersion)
	assert.Zero(t, m.PlacementTarget)
	assert.Len(t, m.Tenants, 2)

	// Every object reads back at its home, one request each.
	for key, body := range want {
		rel := strings.TrimPrefix(key, "t-cust/parity/")
		before := 0
		for _, st := range stores {
			before += st.getCount()
		}
		rc, err := fresh.Get(ctx, "parity", rel)
		require.NoError(t, err, key)
		got, err := io.ReadAll(rc)
		require.NoError(t, err)
		assert.Equal(t, body, string(got), key)
		after := 0
		for _, st := range stores {
			after += st.getCount()
		}
		assert.Equal(t, 1, after-before, "one metadata request: authoritative")
	}
	total := 0
	for _, st := range stores {
		total += filesOn(st)
	}
	assert.Equal(t, 40, total)
}

func TestRebalance_AHomeAlreadyHoldingTheBytesOnlyLosesTheSource(t *testing.T) {
	ctx := common.WithTenantID(context.Background(), "cust")
	stores := newFleetStores("a", "b")
	d := rebalanceFleet(stores, 2)
	_, err := d.CheckFleetFingerprint(ctx, "")
	require.NoError(t, err)
	stores[0].mu.Lock()
	stores[0].files["t-cust/parity/x/etag/p0"] = "same"
	stores[0].mu.Unlock()
	stores[1].mu.Lock()
	stores[1].files["t-cust/parity/x/etag/p0"] = "same"
	stores[1].mu.Unlock()

	puts := stores[1].puts
	require.NoError(t, d.RebalanceMove(ctx, RebalanceMove{Path: "t-cust/parity/x/etag/p0", Size: 4, Hash: qxOf("same"), From: 0, To: 1}))
	assert.Equal(t, puts, stores[1].puts, "no upload")
	assert.Equal(t, 0, filesOn(stores[0]))
	assert.Equal(t, 1, filesOn(stores[1]))
}

func TestRebalance_AHomeHoldingOtherBytesIsRefusedAndNothingDeleted(t *testing.T) {
	ctx := common.WithTenantID(context.Background(), "cust")
	stores := newFleetStores("a", "b")
	d := rebalanceFleet(stores, 2)
	_, err := d.CheckFleetFingerprint(ctx, "")
	require.NoError(t, err)
	stores[0].mu.Lock()
	stores[0].files["t-cust/parity/x/etag/p0"] = "mine"
	stores[0].mu.Unlock()
	stores[1].mu.Lock()
	stores[1].files["t-cust/parity/x/etag/p0"] = "other"
	stores[1].mu.Unlock()

	err = d.RebalanceMove(ctx, RebalanceMove{Path: "t-cust/parity/x/etag/p0", Size: 4, Hash: qxOf("mine"), From: 0, To: 1})
	require.ErrorIs(t, err, ErrRebalanceTargetDiffers)
	assert.Equal(t, 1, filesOn(stores[0]))
	assert.Equal(t, 1, filesOn(stores[1]))
}

func TestRebalance_ACorruptReadNeverReplacesTheSource(t *testing.T) {
	ctx := common.WithTenantID(context.Background(), "cust")
	stores := newFleetStores("a", "b")
	d := rebalanceFleet(stores, 2)
	_, err := d.CheckFleetFingerprint(ctx, "")
	require.NoError(t, err)
	stores[0].mu.Lock()
	stores[0].files["t-cust/parity/x/etag/p0"] = "truth"
	stores[0].serveBody["t-cust/parity/x/etag/p0"] = "lies!"
	stores[0].mu.Unlock()

	err = d.RebalanceMove(ctx, RebalanceMove{Path: "t-cust/parity/x/etag/p0", Size: 5, Hash: qxOf("truth"), From: 0, To: 1})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "quickXorHash")
	assert.Equal(t, 1, filesOn(stores[0]), "the source stays")
	assert.Equal(t, 0, filesOn(stores[1]), "the bad copy is gone")
}

func TestRebalance_CommitRefusesWhileFilesRemain(t *testing.T) {
	ctx := common.WithTenantID(context.Background(), "cust")
	stores := newFleetStores("a", "b", "c", "d")
	d := rebalanceFleet(stores, 2)
	_, err := d.CheckFleetFingerprint(ctx, "")
	require.NoError(t, err)
	for _, st := range stores {
		rewriteMarker(t, st.markerStore, func(m *fleetMarker) { m.PlacementVersion = 0; m.Tenants = nil })
	}
	v1 := rebalanceFleet(stores, 2)
	_, err = v1.CheckFleetFingerprint(ctx, "")
	require.NoError(t, err)
	for g := 0; g < 8; g++ {
		require.NoError(t, v1.Put(ctx, "parity", fmt.Sprintf("%d/etag/p0", g), strings.NewReader("s")))
	}

	plan, err := v1.RebalanceCommit(ctx, 2)
	require.ErrorIs(t, err, ErrRebalanceIncomplete)
	require.NotNil(t, plan)
	assert.NotEmpty(t, plan.Moves)
	assert.Zero(t, stores[0].marker(t).PlacementVersion, "nothing rewritten: still the pre-field marker")
}

func TestRebalance_RefusesAFleetWhoseOrderIsNotAccepted(t *testing.T) {
	ctx := context.Background()
	stores := newFleetStores("a", "b", "c")
	full := rebalanceFleet(stores, 3)
	_, err := full.CheckFleetFingerprint(ctx, "")
	require.NoError(t, err)
	shrunk := rebalanceFleet(stores[:2], 2)
	_, err = shrunk.RebalancePlan(ctx, 2)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "PERMAFROST_FLEET_ACCEPT")

	// Accepted: the plan names what the lost account's objects' neighbours
	// need — nothing here (the fleet is empty), and the markers' layout is
	// the recorded one.
	fp, err := shrunk.CheckFleetFingerprint(ctx, "")
	require.NoError(t, err)
	_, err = shrunk.CheckFleetFingerprint(ctx, fp.Digest)
	require.NoError(t, err)
	plan, err := shrunk.RebalancePlan(ctx, 2)
	require.NoError(t, err)
	assert.False(t, plan.Placed)
	assert.Empty(t, plan.Moves)
}
