package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/FairForge/vaultaire/internal/drivers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeGraph is a tenant's Graph for one user with several drives: items
// held by drive and path, the copy action (cross-drive, conflict = fail,
// finished after `copyPolls` monitor reads), permanentDelete, children
// pages, the marker's content.
type fakeGraph struct {
	mu        sync.Mutex
	srv       *httptest.Server
	drives    []drive
	items     map[string]map[string]*fakeItem // drive id → path ("" = root) → item
	nextID    int
	copyPolls int
	failCopy  bool
	calls     []string
	hashless  map[string]bool // paths whose Graph hash is "not computed yet"
}

type fakeItem struct {
	id     string
	folder bool
	body   []byte
}

func newFakeGraph(t *testing.T, drives ...drive) *fakeGraph {
	f := &fakeGraph{drives: drives, items: map[string]map[string]*fakeItem{}, hashless: map[string]bool{}}
	for _, d := range drives {
		f.items[d.ID] = map[string]*fakeItem{"": {id: "root-" + d.ID, folder: true}}
	}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeGraph) client() *graph {
	g := newGraph(f.srv.URL+"/v1.0", func(context.Context) (string, error) { return "tok", nil })
	g.wait = func(context.Context, time.Duration) error { return nil }
	return g
}

func (f *fakeGraph) put(driveID, path string, body []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.mkdirs(driveID, parentOf(path))
	f.nextID++
	f.items[driveID][path] = &fakeItem{id: fmt.Sprintf("i%d", f.nextID), body: body}
}

func (f *fakeGraph) mkdirs(driveID, path string) {
	if path == "" {
		return
	}
	f.mkdirs(driveID, parentOf(path))
	if _, ok := f.items[driveID][path]; !ok {
		f.nextID++
		f.items[driveID][path] = &fakeItem{id: fmt.Sprintf("i%d", f.nextID), folder: true}
	}
}

func (f *fakeGraph) find(id string) (driveID, path string, it *fakeItem) {
	for d, m := range f.items {
		for p, i := range m {
			if i.id == id {
				return d, p, i
			}
		}
	}
	return "", "", nil
}

func qx(b []byte) string {
	h := drivers.NewQuickXorHash()
	_, _ = h.Write(b)
	return base64.StdEncoding.EncodeToString(h.Sum(nil))
}

func (f *fakeGraph) itemJSON(driveID, path string, it *fakeItem) map[string]any {
	m := map[string]any{"id": it.id, "name": baseOf(path)}
	if it.folder {
		var size int64
		kids := 0
		for p, c := range f.items[driveID] {
			if strings.HasPrefix(p, path+"/") || (path == "" && p != "") {
				size += int64(len(c.body))
				if parentOf(p) == path {
					kids++
				}
			}
		}
		m["size"], m["folder"] = size, map[string]int{"childCount": kids}
		return m
	}
	h := qx(it.body)
	if f.hashless[path] {
		h = ""
	}
	m["size"], m["file"] = len(it.body), map[string]any{"hashes": map[string]string{"quickXorHash": h}}
	return m
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func (f *fakeGraph) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p := strings.TrimPrefix(r.URL.Path, "/v1.0")
	f.calls = append(f.calls, r.Method+" "+p)
	notFound := func() { writeJSON(w, 404, map[string]any{"error": map[string]string{"code": "itemNotFound"}}) }
	seg := strings.Split(strings.TrimPrefix(p, "/"), "/")
	switch {
	case strings.HasPrefix(p, "/monitor/"):
		if f.copyPolls > 0 {
			f.copyPolls--
			writeJSON(w, 202, map[string]any{"status": "inProgress", "percentageComplete": 50})
			return
		}
		writeJSON(w, 200, map[string]any{"status": "completed", "percentageComplete": 100})
	case len(seg) >= 3 && seg[0] == "users" && seg[2] == "drives":
		writeJSON(w, 200, map[string]any{"value": f.drives})
	case len(seg) >= 2 && seg[0] == "drives":
		driveID := seg[1]
		rest := strings.Join(seg[2:], "/")
		switch {
		case rest == "root" && r.Method == http.MethodGet:
			writeJSON(w, 200, f.itemJSON(driveID, "", f.items[driveID][""]))
		case strings.HasPrefix(rest, "root:/"):
			path := strings.TrimPrefix(rest, "root:/")
			if strings.HasSuffix(path, ":/content") && r.Method == http.MethodPut {
				path = strings.TrimSuffix(path, ":/content")
				b, _ := io.ReadAll(r.Body)
				if old, ok := f.items[driveID][path]; ok && r.URL.Query().Get("@microsoft.graph.conflictBehavior") == "fail" {
					_ = old
					writeJSON(w, 409, map[string]any{"error": map[string]string{"code": "nameAlreadyExists"}})
					return
				}
				f.mkdirs(driveID, parentOf(path))
				if old, ok := f.items[driveID][path]; ok {
					old.body = b
				} else {
					f.nextID++
					f.items[driveID][path] = &fakeItem{id: fmt.Sprintf("i%d", f.nextID), body: b}
				}
				writeJSON(w, 201, f.itemJSON(driveID, path, f.items[driveID][path]))
				return
			}
			it, ok := f.items[driveID][path]
			if !ok {
				notFound()
				return
			}
			writeJSON(w, 200, f.itemJSON(driveID, path, it))
		case len(seg) >= 4 && seg[2] == "items":
			d, path, it := f.find(seg[3])
			if it == nil || d != driveID {
				notFound()
				return
			}
			action := strings.Join(seg[4:], "/")
			switch {
			case action == "children":
				var out []map[string]any
				for cp, c := range f.items[driveID] {
					if cp != "" && parentOf(cp) == path {
						out = append(out, f.itemJSON(driveID, cp, c))
					}
				}
				writeJSON(w, 200, map[string]any{"value": out})
			case action == "content":
				w.WriteHeader(200)
				_, _ = w.Write(it.body)
			case action == "permanentDelete" && r.Method == http.MethodPost:
				for cp := range f.items[driveID] {
					if cp == path || strings.HasPrefix(cp, path+"/") {
						delete(f.items[driveID], cp)
					}
				}
				w.WriteHeader(204)
			case action == "copy" && r.Method == http.MethodPost:
				if f.failCopy {
					writeJSON(w, 400, map[string]any{"error": map[string]string{"code": "invalidRequest"}})
					return
				}
				var body struct {
					ParentReference struct{ DriveID, ID string } `json:"parentReference"`
					Name            string                       `json:"name"`
				}
				_ = json.NewDecoder(r.Body).Decode(&body)
				dd, dparent, _ := f.find(body.ParentReference.ID)
				target := body.Name
				if dparent != "" {
					target = dparent + "/" + body.Name
				}
				if _, exists := f.items[dd][target]; exists {
					writeJSON(w, 409, map[string]any{"error": map[string]string{"code": "nameAlreadyExists"}})
					return
				}
				for cp, c := range f.items[driveID] {
					if cp == path || strings.HasPrefix(cp, path+"/") {
						f.nextID++
						f.items[dd][target+strings.TrimPrefix(cp, path)] = &fakeItem{id: fmt.Sprintf("i%d", f.nextID), folder: c.folder, body: c.body}
					}
				}
				w.Header().Set("Location", f.srv.URL+"/monitor/1")
				w.WriteHeader(202)
			default:
				notFound()
			}
		default:
			notFound()
		}
	default:
		notFound()
	}
}

var (
	cacheDrive = drive{ID: "b!CACHE", Name: "PersonalCacheLibrary", DriveType: "business"}
	odDrive    = drive{ID: "b!OD", Name: "OneDrive", DriveType: "business"}
)

func marker(t *testing.T, index int, driveID string) []byte {
	t.Helper()
	b, err := json.Marshal(map[string]any{"version": 1, "index": index, "fleet": []string{"a", "b", "c"}, "digest": "dg", "drive_id": driveID})
	require.NoError(t, err)
	return b
}

// seedCacheTree is prod's slot 1/3 shape: the fleet's tree in the cache
// library, nothing in the OneDrive library.
func seedCacheTree(t *testing.T, f *fakeGraph) {
	f.put(cacheDrive.ID, "vaultaire/t-tenant-1/c__parity/d1/e1/p0", []byte("shard zero"))
	f.put(cacheDrive.ID, "vaultaire/t-tenant-1/c__parity/d1/e1/p1", []byte("shard one!"))
	f.put(cacheDrive.ID, "vaultaire/t-tenant-2/b/k.txt", bytes.Repeat([]byte("x"), 1000))
	f.put(cacheDrive.ID, markerPath, marker(t, 0, ""))
}

func runMode(t *testing.T, f *fakeGraph, mode string, mut ...func(*options)) (string, error) {
	t.Helper()
	o := options{slot: 1, mode: mode, pollEvery: time.Millisecond}
	for _, m := range mut {
		m(&o)
	}
	var out bytes.Buffer
	err := run(context.Background(), &out, f.client(), "u@fleet.test", o)
	return out.String(), err
}

func TestCopy_CopiesTheTreeAcrossDrivesAndVerifiesIt(t *testing.T) {
	f := newFakeGraph(t, cacheDrive, odDrive)
	seedCacheTree(t, f)
	f.copyPolls = 2

	out, err := runMode(t, f, "copy")

	require.NoError(t, err, out)
	assert.Contains(t, out, "verified: every file's count, bytes and quickXorHash match")
	assert.Equal(t, []byte("shard one!"), f.items[odDrive.ID]["vaultaire/t-tenant-1/c__parity/d1/e1/p1"].body)
	assert.Contains(t, f.items[cacheDrive.ID], "vaultaire/t-tenant-2/b/k.txt", "the source is kept")
}

func TestCopy_AnExistingCopyIsVerifiedAndAPartialOneFilled(t *testing.T) {
	// A run cut after Graph copied part of the tree: the rerun copies only
	// what is missing and verifies.
	f := newFakeGraph(t, cacheDrive, odDrive)
	seedCacheTree(t, f)
	f.put(odDrive.ID, "vaultaire/t-tenant-1/c__parity/d1/e1/p0", []byte("shard zero"))
	f.put(odDrive.ID, markerPath, marker(t, 0, ""))

	out, err := runMode(t, f, "copy")

	require.NoError(t, err, out)
	assert.Contains(t, out, "verified, not copied again")
	assert.Contains(t, out, "fill round 1")
	assert.Contains(t, f.items[odDrive.ID], "vaultaire/t-tenant-2/b/k.txt")
	assert.Contains(t, f.items[odDrive.ID], "vaultaire/t-tenant-1/c__parity/d1/e1/p1")
}

func TestVerify_ADifferentFileIsExitTwoAndNamed(t *testing.T) {
	f := newFakeGraph(t, cacheDrive, odDrive)
	seedCacheTree(t, f)
	_, err := runMode(t, f, "copy")
	require.NoError(t, err)
	f.items[odDrive.ID]["vaultaire/t-tenant-1/c__parity/d1/e1/p1"].body = []byte("shard ONE!")

	out, err := runMode(t, f, "verify")

	require.ErrorIs(t, err, errDiffer)
	assert.Contains(t, out, "hash t-tenant-1/c__parity/d1/e1/p1")
	assert.Contains(t, out, "1 differences")
}

func TestVerify_AFileGraphHasNotHashedIsHashedFromItsBytes(t *testing.T) {
	f := newFakeGraph(t, cacheDrive, odDrive)
	seedCacheTree(t, f)
	_, err := runMode(t, f, "copy")
	require.NoError(t, err)
	f.hashless["vaultaire/t-tenant-2/b/k.txt"] = true

	out, err := runMode(t, f, "verify")

	require.NoError(t, err, out)
	assert.Contains(t, strings.Join(f.calls, "\n"), "/content", "the hashless file was read")
}

func TestCopy_AnExtraOrChangedFileStopsTheCopy(t *testing.T) {
	f := newFakeGraph(t, cacheDrive, odDrive)
	seedCacheTree(t, f)
	f.put(odDrive.ID, "vaultaire/t-tenant-2/b/k.txt", []byte("other"))

	out, err := runMode(t, f, "copy")

	require.ErrorIs(t, err, errDiffer)
	assert.NotContains(t, out, "fill round", "a size difference is never filled over")
}

func TestSwitch_WritesTheDriveOnBothMarkersAndPrintsThePin(t *testing.T) {
	f := newFakeGraph(t, cacheDrive, odDrive)
	seedCacheTree(t, f)
	_, err := runMode(t, f, "copy")
	require.NoError(t, err)

	out, err := runMode(t, f, "switch")

	require.NoError(t, err, out)
	assert.Contains(t, out, "TENANT_1_DRIVE_ID=b!OD")
	var nm, om markerView
	require.NoError(t, json.Unmarshal(f.items[odDrive.ID][markerPath].body, &nm))
	require.NoError(t, json.Unmarshal(f.items[cacheDrive.ID][markerPath].body, &om))
	assert.Equal(t, "b!OD", nm.DriveID)
	assert.Empty(t, nm.MovedTo)
	assert.Equal(t, "b!OD", om.DriveID)
	assert.Equal(t, "b!OD", om.MovedTo)
	assert.Equal(t, []string{"a", "b", "c"}, nm.Fleet, "the rest of the marker kept")

	// A verify after the switch still passes: the markers' content differs.
	_, err = runMode(t, f, "verify")
	require.NoError(t, err)
}

func TestSwitch_RefusesWhileTheSourceHoldsWhatTheCopyLacks(t *testing.T) {
	// The driver keeps writing to the pinned (old) drive until the restart.
	f := newFakeGraph(t, cacheDrive, odDrive)
	seedCacheTree(t, f)
	_, err := runMode(t, f, "copy")
	require.NoError(t, err)
	f.put(cacheDrive.ID, "vaultaire/t-tenant-3/new/shard", []byte("written since"))

	_, err = runMode(t, f, "switch")

	require.ErrorIs(t, err, errDiffer)
	var om markerView
	require.NoError(t, json.Unmarshal(f.items[cacheDrive.ID][markerPath].body, &om))
	assert.Empty(t, om.MovedTo, "nothing switched")
}

func TestSwitch_WithTheTreeOnOneDriveOnlyRecordsItsDrive(t *testing.T) {
	// Slot 2: already on the OneDrive library.
	f := newFakeGraph(t, odDrive, cacheDrive)
	f.put(odDrive.ID, "vaultaire/t-tenant-1/x", []byte("x"))
	f.put(odDrive.ID, markerPath, marker(t, 1, ""))

	out, err := runMode(t, f, "switch")

	require.NoError(t, err, out)
	var m markerView
	require.NoError(t, json.Unmarshal(f.items[odDrive.ID][markerPath].body, &m))
	assert.Equal(t, "b!OD", m.DriveID)
	assert.Equal(t, 1, m.Index)
}

func TestRetire_NeedsTheSwitchTheConfirmationAndEveryOldItemOnTheNewDrive(t *testing.T) {
	f := newFakeGraph(t, cacheDrive, odDrive)
	seedCacheTree(t, f)
	_, err := runMode(t, f, "copy")
	require.NoError(t, err)

	_, err = runMode(t, f, "retire", func(o *options) { o.confirmRetire = true })
	require.ErrorContains(t, err, "run -switch first")

	_, err = runMode(t, f, "switch")
	require.NoError(t, err)
	f.put(odDrive.ID, "vaultaire/t-tenant-9/after-restart", []byte("new write on the new drive"))

	_, err = runMode(t, f, "retire")
	require.ErrorContains(t, err, "-confirm-retire")
	assert.Contains(t, f.items[cacheDrive.ID], "vaultaire")

	out, err := runMode(t, f, "retire", func(o *options) { o.confirmRetire = true })
	require.NoError(t, err, out)
	assert.NotContains(t, f.items[cacheDrive.ID], "vaultaire", "the old tree is gone")
	assert.NotContains(t, f.items[cacheDrive.ID], "vaultaire/t-tenant-2/b/k.txt")
	assert.Contains(t, f.items[odDrive.ID], "vaultaire/t-tenant-2/b/k.txt")
	assert.Contains(t, strings.Join(f.calls, "\n"), "POST /drives/b!CACHE/items/", "permanentDelete")
	assert.NotContains(t, strings.Join(f.calls, "\n"), "DELETE ")
}

func TestRetire_RefusesWhenTheNewDriveLacksAnOldItem(t *testing.T) {
	f := newFakeGraph(t, cacheDrive, odDrive)
	seedCacheTree(t, f)
	_, err := runMode(t, f, "copy")
	require.NoError(t, err)
	_, err = runMode(t, f, "switch")
	require.NoError(t, err)
	delete(f.items[odDrive.ID], "vaultaire/t-tenant-2/b/k.txt")

	_, err = runMode(t, f, "retire", func(o *options) { o.confirmRetire = true })

	require.ErrorIs(t, err, errDiffer)
	assert.Contains(t, f.items[cacheDrive.ID], "vaultaire/t-tenant-2/b/k.txt")
}

func TestDryRun_NamesEveryDriveAndWhatItsTreeHolds(t *testing.T) {
	f := newFakeGraph(t, cacheDrive, odDrive)
	seedCacheTree(t, f)

	out, err := runMode(t, f, "dry-run")

	require.NoError(t, err)
	assert.Contains(t, out, "PersonalCacheLibrary")
	assert.Contains(t, out, "vaultaire/: 4 files")
	assert.Contains(t, out, "vaultaire/: none")
	assert.Contains(t, out, "next: -copy (PersonalCacheLibrary → OneDrive)")
}

func TestTopMissing_AMissingFolderCoversItsChildren(t *testing.T) {
	ds := []diff{{path: "a", kind: "missing"}, {path: "a/b", kind: "missing"}, {path: "c/d", kind: "missing"}}
	assert.Equal(t, []string{"a", "c/d"}, topMissing(ds))
}

func TestRedact_NeverPrintsAQuery(t *testing.T) {
	assert.Equal(t, "https://contoso-my.sharepoint.com/…", redact("https://contoso-my.sharepoint.com/_api/x?tempauth=SECRET"))
	assert.Equal(t, "/v1.0/drives/x", redact("https://graph.microsoft.com/v1.0/drives/x?$select=id"))
	_, err := url.Parse("https://graph.microsoft.com/v1.0")
	require.NoError(t, err)
}
