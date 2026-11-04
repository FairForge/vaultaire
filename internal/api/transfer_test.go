package api

// WP-XFER-1 PR A (Stage 1a Prompt 59): transfer in. The source is an
// httptest S3 fake behind transferAllowPrivateTargets (the webhooks'
// test-only escape) — never a real vendor.

import (
	"bytes"
	"context"
	"crypto/md5" // #nosec G501 -- an S3 ETag
	"encoding/hex"
	"encoding/xml"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"github.com/FairForge/vaultaire/internal/account"
	"github.com/FairForge/vaultaire/internal/auth"
	"github.com/FairForge/vaultaire/internal/common"
	"github.com/FairForge/vaultaire/internal/crypto"
)

// fakeS3Source is a minimal S3: ListObjectsV2 (prefix, start-after,
// max-keys), GET with Range and If-Match, signed or not (it checks only that
// an Authorization header names the expected key id).
type fakeS3Source struct {
	t       *testing.T
	srv     *httptest.Server
	mu      sync.Mutex
	objects map[string]fakeSrcObject
	keyID   string
	gets    []string // "key@offset" of every GET
	lists   int
	denyGet bool // every GET answers 403
	fail5xx map[string]int
}

type fakeSrcObject struct {
	body        []byte
	contentType string
	meta        map[string]string
	etag        string
}

func newFakeS3Source(t *testing.T) *fakeS3Source {
	f := &fakeS3Source{t: t, objects: map[string]fakeSrcObject{}, keyID: "SRCKEY" + strconv.FormatInt(time.Now().UnixNano()%1e6, 10), fail5xx: map[string]int{}}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeS3Source) put(key string, body []byte, ct string, meta map[string]string) string {
	sum := md5.Sum(body) // #nosec G401
	etag := hex.EncodeToString(sum[:])
	f.mu.Lock()
	f.objects[key] = fakeSrcObject{body: body, contentType: ct, meta: meta, etag: etag}
	f.mu.Unlock()
	return etag
}

// putMultipartETag stores an object whose ETag is a multipart one (not its MD5).
func (f *fakeS3Source) putMultipartETag(key string, body []byte) string {
	etag := "0123456789abcdef0123456789abcdef-3"
	f.mu.Lock()
	f.objects[key] = fakeSrcObject{body: body, contentType: "application/octet-stream", etag: etag}
	f.mu.Unlock()
	return etag
}

func (f *fakeS3Source) serve(w http.ResponseWriter, r *http.Request) {
	if !strings.Contains(r.Header.Get("Authorization"), "Credential="+f.keyID+"/") {
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `<Error><Code>InvalidAccessKeyId</Code></Error>`)
		return
	}
	parts := strings.SplitN(strings.TrimPrefix(r.URL.Path, "/"), "/", 2)
	if parts[0] != "src-bucket" {
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `<Error><Code>NoSuchBucket</Code></Error>`)
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(parts) == 1 || parts[1] == "" {
		f.lists++
		q := r.URL.Query()
		prefix, after := q.Get("prefix"), q.Get("start-after")
		max, _ := strconv.Atoi(q.Get("max-keys"))
		if max <= 0 {
			max = 1000
		}
		var keys []string
		for k := range f.objects {
			if strings.HasPrefix(k, prefix) && k > after {
				keys = append(keys, k)
			}
		}
		sort.Strings(keys)
		truncated := len(keys) > max
		if truncated {
			keys = keys[:max]
		}
		var b strings.Builder
		b.WriteString(`<?xml version="1.0" encoding="UTF-8"?><ListBucketResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/"><Name>src-bucket</Name>`)
		fmt.Fprintf(&b, "<KeyCount>%d</KeyCount><IsTruncated>%t</IsTruncated>", len(keys), truncated)
		for _, k := range keys {
			o := f.objects[k]
			var ek bytes.Buffer
			_ = xml.EscapeText(&ek, []byte(k))
			fmt.Fprintf(&b, `<Contents><Key>%s</Key><ETag>"%s"</ETag><Size>%d</Size></Contents>`, ek.String(), o.etag, len(o.body))
		}
		b.WriteString(`</ListBucketResult>`)
		w.Header().Set("Content-Type", "application/xml")
		_, _ = io.WriteString(w, b.String())
		return
	}
	key, _ := url.PathUnescape(parts[1])
	o, ok := f.objects[key]
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `<Error><Code>NoSuchKey</Code></Error>`)
		return
	}
	if f.denyGet {
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `<Error><Code>AccessDenied</Code></Error>`)
		return
	}
	if f.fail5xx[key] > 0 {
		f.fail5xx[key]--
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, `<Error><Code>InternalError</Code></Error>`)
		return
	}
	if im := r.Header.Get("If-Match"); im != "" && strings.Trim(im, `"`) != o.etag {
		w.WriteHeader(http.StatusPreconditionFailed)
		_, _ = io.WriteString(w, `<Error><Code>PreconditionFailed</Code></Error>`)
		return
	}
	var a, b int64 = 0, int64(len(o.body)) - 1
	if rg := r.Header.Get("Range"); rg != "" {
		_, _ = fmt.Sscanf(rg, "bytes=%d-%d", &a, &b)
		if b >= int64(len(o.body)) {
			b = int64(len(o.body)) - 1
		}
		if a > b {
			w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
			return
		}
	}
	f.gets = append(f.gets, fmt.Sprintf("%s@%d", key, a))
	w.Header().Set("ETag", `"`+o.etag+`"`)
	if o.contentType != "" {
		w.Header().Set("Content-Type", o.contentType)
	}
	for k, v := range o.meta {
		w.Header().Set("x-amz-meta-"+k, v)
	}
	w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", a, b, len(o.body)))
	w.Header().Set("Content-Length", strconv.FormatInt(b-a+1, 10))
	w.WriteHeader(http.StatusPartialContent)
	_, _ = w.Write(o.body[a : b+1])
}

func (f *fakeS3Source) getsOf(key string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, g := range f.gets {
		if strings.HasPrefix(g, key+"@") {
			n++
		}
	}
	return n
}

// transferFixture is the edge fixture (an origin with a fake backend, a
// tenant, a bucket, the quota manager) and a fake source.
type transferFixture struct {
	*edgeFixture
	src *fakeS3Source
	job *TransferJob
}

func setupTransferFixture(t *testing.T) *transferFixture {
	t.Helper()
	transferAllowPrivateTargets.Store(true)
	t.Cleanup(func() { transferAllowPrivateTargets.Store(false) })
	f := setupEdgeFixture(t)
	f.srv.transfer = newTransferRuntime(transferConfig{rateMax: 1 << 30, originShare: 1 << 30})
	j := newTransferJob(f.srv)
	require.NotNil(t, j)
	j.JobName = "transfer_test_" + f.tenantID
	j.onlyTenant = f.tenantID
	t.Cleanup(func() {
		_, _ = f.db.Exec(`DELETE FROM transfer_items WHERE tenant_id = $1`, f.tenantID)
		_, _ = f.db.Exec(`DELETE FROM transfer_jobs WHERE tenant_id = $1`, f.tenantID)
		_, _ = f.db.Exec(`DELETE FROM audit_logs WHERE tenant_id = $1`, f.tenantID)
	})
	return &transferFixture{edgeFixture: f, src: newFakeS3Source(t), job: j}
}

func (f *transferFixture) create(prefix string) transferView {
	f.t.Helper()
	var in transferCreate
	in.Source.Endpoint, in.Source.Bucket, in.Source.Prefix = f.src.srv.URL, "src-bucket", prefix
	in.Source.AccessKey, in.Source.SecretKey = f.src.keyID, "the-source-secret-"+f.tenantID
	in.Bucket = f.bucket
	v, status, errType, code, msg, _ := f.srv.createTransfer(context.Background(), f.tenantID, f.userID, in)
	require.Empty(f.t, errType, "%d %s %s", status, code, msg)
	return v
}

func (f *transferFixture) runUntilDone(id string) transferView {
	f.t.Helper()
	for i := 0; i < 20; i++ {
		_, err := f.job.RunOnce(context.Background())
		require.NoError(f.t, err)
		v := f.view(id)
		if v.State != transferPending && v.State != transferRunning {
			return v
		}
	}
	f.t.Fatalf("transfer %s never finished", id)
	return transferView{}
}

func (f *transferFixture) view(id string) transferView {
	f.t.Helper()
	tr, err := f.srv.loadTransfer(context.Background(), f.tenantID, id)
	require.NoError(f.t, err)
	return viewOf(tr)
}

func (f *transferFixture) destBytes(key string) []byte {
	f.t.Helper()
	h, ok := f.head(key)
	require.True(f.t, ok, "a head row for %s", key)
	container, blob := objectBlobAddress(f.tenantID, f.bucket, key, h.sk)
	rc, err := f.drv.Get(common.WithTenantID(context.Background(), f.tenantID), container, blob)
	require.NoError(f.t, err)
	defer func() { _ = rc.Close() }()
	b, err := io.ReadAll(rc)
	require.NoError(f.t, err)
	return b
}

func TestTransfer_ResumesFromItsCursor(t *testing.T) {
	f := setupTransferFixture(t)
	for i := 1; i <= 7; i++ {
		f.src.put(fmt.Sprintf("in/obj-%d.bin", i), testBytes(1000+i), "application/x-test", nil)
	}
	v := f.create("in/")
	f.job.stopAfter = 3 // the run is cut after three objects (a restart)
	_, err := f.job.RunOnce(context.Background())
	require.NoError(t, err)
	mid := f.view(v.ID)
	assert.Equal(t, int64(3), mid.Copied)
	assert.Equal(t, int64(7), mid.Listed)
	assert.Equal(t, "in/obj-7.bin", mid.Cursor, "the listing cursor is in the row")
	for i := 1; i <= 3; i++ {
		assert.Equal(t, 1, f.src.getsOf(fmt.Sprintf("in/obj-%d.bin", i)))
	}

	// A new process: a fresh job value, the row's cursor and items.
	j2 := newTransferJob(f.srv)
	j2.JobName, j2.onlyTenant = f.job.JobName, f.tenantID
	f.job = j2
	done := f.runUntilDone(v.ID)
	assert.Equal(t, transferCompleted, done.State)
	assert.Equal(t, int64(7), done.Copied)
	for i := 1; i <= 7; i++ {
		key := fmt.Sprintf("in/obj-%d.bin", i)
		assert.Equal(t, 1, f.src.getsOf(key), "%s read once: the 4th was the first of the new run", key)
		assert.Equal(t, testBytes(1000+i), f.destBytes(key))
	}
}

func TestTransfer_IdempotentOnETag(t *testing.T) {
	f := setupTransferFixture(t)
	prior := [3]int64{transferMultipartMin, transferPartBytes, transferRangeBytes}
	// 3 MiB ranges inside 5 MiB parts: a range crosses every part's edge.
	transferMultipartMin, transferPartBytes, transferRangeBytes = 8<<20, 5<<20, 3<<20
	t.Cleanup(func() { transferMultipartMin, transferPartBytes, transferRangeBytes = prior[0], prior[1], prior[2] })
	f.src.put("a.txt", []byte("alpha"), "text/plain", map[string]string{"owner": "tz"})
	f.src.put("b.txt", []byte("bravo"), "text/plain", nil)
	f.src.putMultipartETag("big.bin", testBytes(12<<20)) // a multipart copy (3 parts), its ETag not an MD5

	first := f.runUntilDone(f.create("").ID)
	require.Equal(t, transferCompleted, first.State, first.LastError)
	assert.Equal(t, int64(3), first.Copied)
	assert.Equal(t, testBytes(12<<20), f.destBytes("big.bin"))
	assert.Equal(t, 4, f.src.getsOf("big.bin"), "four 3 MiB ranges, each read once")
	gets := len(f.src.gets)

	// The same source again: every item skipped, no byte read.
	second := f.runUntilDone(f.create("").ID)
	assert.Equal(t, transferCompleted, second.State)
	assert.Equal(t, int64(3), second.Skipped, "a.txt and b.txt by their MD5 ETag, big.bin by the first transfer's record")
	assert.Zero(t, second.Copied)
	assert.Equal(t, gets, len(f.src.gets), "no GET of a skipped item")

	// A changed source object is copied again.
	f.src.put("a.txt", []byte("alpha, changed"), "text/plain", nil)
	third := f.runUntilDone(f.create("").ID)
	assert.Equal(t, int64(1), third.Copied)
	assert.Equal(t, int64(2), third.Skipped)
	assert.Equal(t, []byte("alpha, changed"), f.destBytes("a.txt"))
}

func TestTransfer_PausesWhenThePoolIsSpent(t *testing.T) {
	f := setupTransferFixture(t)
	f.src.put("1.bin", testBytes(3000), "", nil)
	f.src.put("2.bin", testBytes(3000), "", nil)
	f.exec(`UPDATE tenant_quotas SET storage_limit_bytes = 5000 WHERE tenant_id = $1`, f.tenantID)
	v := f.create("")
	paused := f.runUntilDone(v.ID)
	assert.Equal(t, transferPaused, paused.State)
	assert.Equal(t, "quota", paused.Paused)
	assert.Equal(t, ErrQuotaExceeded, paused.PausedCode, "the 403's code on the progress")
	assert.Equal(t, int64(1), paused.Copied)
	_, ok := f.head("2.bin")
	assert.False(t, ok, "nothing written past the quota")

	// Still no room: a resume pauses again. Room: it completes.
	resume := func() {
		_, err := f.db.Exec(`UPDATE transfer_jobs SET state = 'running', paused_reason = '', paused_error = '' WHERE id = $1 AND state = 'paused'`, v.ID)
		require.NoError(t, err)
	}
	resume()
	assert.Equal(t, transferPaused, f.runUntilDone(v.ID).State)
	f.exec(`UPDATE tenant_quotas SET storage_limit_bytes = 1 << 30 WHERE tenant_id = $1`, f.tenantID)
	resume()
	done := f.runUntilDone(v.ID)
	assert.Equal(t, transferCompleted, done.State)
	assert.Equal(t, int64(2), done.Copied)
	assert.Equal(t, int64(6000), f.used(), "metered as ingest on the destination's quota")
}

func TestTransfer_SourceKeyWrappedAndErasedWithTheJob(t *testing.T) {
	f := setupTransferFixture(t)
	core, logs := observer.New(zap.DebugLevel)
	f.srv.logger = zap.New(core)
	keys, err := crypto.NewTenantKeys(f.db, codecTestMaster, "")
	require.NoError(t, err)
	f.srv.tenantKeys = keys
	f.src.put("k.bin", testBytes(2000), "", nil)
	secret := "the-source-secret-" + f.tenantID

	keyCols := func(id string) (ak, plain sql2, wrapped []byte, ver sql2) {
		require.NoError(t, f.db.QueryRow(`SELECT COALESCE(access_key_id, ''), COALESCE(secret_key, ''), secret_wrapped, COALESCE(secret_key_version::text, '')
			FROM transfer_jobs WHERE id = $1`, id).Scan(&ak, &plain, &wrapped, &ver))
		return
	}
	v := f.create("")
	ak, plain, wrapped, ver := keyCols(v.ID)
	assert.Equal(t, sql2(f.src.keyID), ak)
	assert.Empty(t, plain, "wrapped: no plaintext")
	require.NotEmpty(t, wrapped)
	assert.NotContains(t, string(wrapped), secret)
	assert.NotEmpty(t, ver)
	opened, err := keys.OpenCredential(context.Background(), f.tenantID, v.ID, mustAtoi(t, string(ver)), wrapped)
	require.NoError(t, err)
	assert.Equal(t, secret, string(opened))

	// Completed: every key column NULL.
	assert.Equal(t, transferCompleted, f.runUntilDone(v.ID).State)
	ak, plain, wrapped, ver = keyCols(v.ID)
	assert.Empty(t, ak)
	assert.Empty(t, plain)
	assert.Empty(t, wrapped)
	assert.Empty(t, ver)

	// Cancelled: the same.
	c := f.create("")
	_, err = f.db.Exec(transferEndSQL, c.ID, transferCancelled, "")
	require.NoError(t, err)
	ak, plain, wrapped, _ = keyCols(c.ID)
	assert.Empty(t, ak)
	assert.Empty(t, plain)
	assert.Empty(t, wrapped)

	// Never in a log line, nor in the export.
	for _, e := range logs.All() {
		line := e.Message
		for _, fld := range e.Context {
			line += " " + fld.String + fmt.Sprint(fld.Interface)
		}
		assert.NotContains(t, line, secret)
	}
	var export bytes.Buffer
	js := newJSONStream(&export)
	js.begin()
	for _, sec := range exportSections {
		if sec.Name == "transfers" {
			require.NoError(t, sec.render(context.Background(), f.db, js, exportSubject{TenantID: f.tenantID, UserID: f.userID}, 100))
		}
	}
	require.NoError(t, js.w.Flush())
	assert.Contains(t, export.String(), v.ID)
	assert.NotContains(t, export.String(), secret)
	assert.NotContains(t, export.String(), f.src.keyID)

	// Gone with the account.
	pending := f.create("")
	erased := 0
	for _, rule := range account.Deleted {
		if strings.HasPrefix(rule.Table, "transfer_") {
			_, err = f.db.Exec(rule.SQL, f.tenantID)
			require.NoError(t, err)
			erased++
		}
	}
	assert.Equal(t, 2, erased, "the erasure's rules name both tables")
	var n int
	require.NoError(t, f.db.QueryRow(`SELECT COUNT(*) FROM transfer_jobs WHERE tenant_id = $1`, f.tenantID).Scan(&n))
	assert.Zero(t, n, "%s and the others erased", pending.ID)
	require.NoError(t, f.db.QueryRow(`SELECT COUNT(*) FROM transfer_items WHERE tenant_id = $1`, f.tenantID).Scan(&n))
	assert.Zero(t, n)
}

type sql2 = string

func mustAtoi(t *testing.T, s string) int {
	n, err := strconv.Atoi(s)
	require.NoError(t, err)
	return n
}

func TestTransfer_RefusesAPrivateSourceAtDialTime(t *testing.T) {
	f := setupTransferFixture(t)
	f.src.put("x.bin", testBytes(100), "", nil)
	// The create-time check passed (the escape on, as a public name would)…
	v := f.create("")
	// …and the name now answers a private address: the dial refuses it.
	transferAllowPrivateTargets.Store(false)
	_, err := f.job.RunOnce(context.Background())
	require.NoError(t, err)
	got := f.view(v.ID)
	assert.Equal(t, transferFailed, got.State)
	assert.Contains(t, got.LastError, "private")
	_, ok := f.head("x.bin")
	assert.False(t, ok)

	// A name (no literal IP: the create-time shape check passes) that
	// resolves to the loopback is refused at the dial.
	_, port, _ := net.SplitHostPort(strings.TrimPrefix(f.src.srv.URL, "http://"))
	var in transferCreate
	in.Source.Endpoint, in.Source.Bucket = "https://localhost:"+port, "src-bucket"
	in.Source.AccessKey, in.Source.SecretKey = f.src.keyID, "s"
	in.Bucket = f.bucket
	_, status, _, code, msg, _ := f.srv.createTransfer(context.Background(), f.tenantID, f.userID, in)
	assert.Equal(t, http.StatusBadRequest, status)
	assert.Equal(t, "source_unreachable", code)
	assert.Contains(t, msg, "private")

	// And the shape: a literal private IP, http, credentials, a query.
	for _, bad := range []string{"https://127.0.0.1", "https://10.0.0.1:9000", "http://example.com", "https://u:p@example.com",
		"https://example.com/?x=1", "https://example.com/bucket", "https://[::1]", "https://169.254.169.254"} {
		_, err := validTransferEndpoint(bad)
		assert.Error(t, err, bad)
	}
	assert.Error(t, transferDialControl("tcp", "127.0.0.1:443", nil))
	assert.Error(t, transferDialControl("tcp", "[fd00::1]:443", nil))
	assert.NoError(t, transferDialControl("tcp", "93.184.216.34:443", nil))
}

func TestTransfer_WritesThroughThePUTPath(t *testing.T) {
	f := setupTransferFixture(t)
	body := testBytes(4321)
	f.src.put("same.bin", body, "image/png", map[string]string{"camera": "x100"})
	done := f.runUntilDone(f.create("").ID)
	require.Equal(t, transferCompleted, done.State, done.LastError)
	w := f.originPut("by-put.bin", body, map[string]string{"Content-Type": "image/png", "x-amz-meta-camera": "x100"})
	require.Equal(t, http.StatusOK, w.Code)

	type row struct {
		size                     int64
		etag, ct, backend, floor string
		meta                     string
		chunked                  bool
	}
	read := func(key string) row {
		var r row
		require.NoError(t, f.db.QueryRow(`SELECT size_bytes, etag, COALESCE(content_type, ''), COALESCE(backend_name, ''), COALESCE(floor, ''),
			COALESCE(metadata::text, ''), is_chunked FROM object_head_cache WHERE tenant_id = $1 AND bucket = $2 AND object_key = $3`,
			f.tenantID, f.bucket, key).Scan(&r.size, &r.etag, &r.ct, &r.backend, &r.floor, &r.meta, &r.chunked))
		return r
	}
	assert.Equal(t, read("by-put.bin"), read("same.bin"), "the head row a PUT of the same bytes and class gives")
	var versions int
	require.NoError(t, f.db.QueryRow(`SELECT COUNT(*) FROM object_versions WHERE tenant_id = $1 AND bucket = $2 AND object_key = 'same.bin'`,
		f.tenantID, f.bucket).Scan(&versions))
	var putVersions int
	require.NoError(t, f.db.QueryRow(`SELECT COUNT(*) FROM object_versions WHERE tenant_id = $1 AND bucket = $2 AND object_key = 'by-put.bin'`,
		f.tenantID, f.bucket).Scan(&putVersions))
	assert.Equal(t, putVersions, versions, "the version rows a PUT writes")
	var events int
	require.NoError(t, f.db.QueryRow(`SELECT COUNT(*) FROM events WHERE tenant_id = $1 AND type = 'object.created' AND data->>'key' = 'same.bin'`,
		f.tenantID).Scan(&events))
	assert.Equal(t, 1, events, "the PUT's object.created")
	require.NoError(t, f.db.QueryRow(`SELECT COUNT(*) FROM events WHERE tenant_id = $1 AND type = 'transfer.completed'`, f.tenantID).Scan(&events))
	assert.Equal(t, 1, events, "and the transfer's completion")
	assert.Equal(t, body, f.destBytes("same.bin"))
}

// A source 5xx is retried, then the item fails and the job goes on; a GET
// 403 pauses the job (source_denied).
func TestTransfer_RetriesThenFailsAnItemAndPausesOnADeniedGet(t *testing.T) {
	f := setupTransferFixture(t)
	f.src.put("bad.bin", testBytes(100), "", nil)
	f.src.put("good.bin", testBytes(100), "", nil)
	f.src.fail5xx["bad.bin"] = 99
	done := f.runUntilDone(f.create("").ID)
	assert.Equal(t, transferCompleted, done.State)
	assert.Equal(t, int64(1), done.Failed)
	assert.Equal(t, int64(1), done.Copied)

	f.src.denyGet = true
	f.src.put("new.bin", testBytes(50), "", nil)
	p := f.runUntilDone(f.create("").ID)
	assert.Equal(t, transferPaused, p.State)
	assert.Equal(t, "source_denied", p.Paused)
}

// The management API: a developer may read transfers but not create one
// (owner/admin); a source that refuses the key is 400 source_denied naming
// its answer; no answer ever carries the key.
func TestTransfer_ManagementRoutes(t *testing.T) {
	transferAllowPrivateTargets.Store(true)
	t.Cleanup(func() { transferAllowPrivateTargets.Store(false) })
	f := setupPlatformFixture(t).withManagement()
	src := newFakeS3Source(t)
	src.put("one.txt", []byte("one"), "text/plain", nil)
	bucket := f.bucket("xfer")
	t.Cleanup(func() {
		_, _ = f.db.Exec(`DELETE FROM transfer_items WHERE tenant_id = $1`, f.user.TenantID)
		_, _ = f.db.Exec(`DELETE FROM transfer_jobs WHERE tenant_id = $1`, f.user.TenantID)
	})
	secret := "s3cr3t-" + bucket
	body := map[string]any{"bucket": bucket, "source": map[string]any{"endpoint": src.srv.URL, "bucket": "src-bucket",
		"access_key": src.keyID, "secret_key": secret}}

	dev := f.member()
	devJWT, err := f.s.auth.GenerateJWT(&auth.User{ID: dev, TenantID: f.user.TenantID, Email: "dev@platform.example"})
	require.NoError(t, err)
	code, out := f.call(http.MethodPost, "/api/v1/manage/transfers", devJWT, body)
	assert.Equal(t, http.StatusForbidden, code, "a developer may not: %v", out)

	wrong := map[string]any{"bucket": bucket, "source": map[string]any{"endpoint": src.srv.URL, "bucket": "src-bucket",
		"access_key": "NOTTHEKEY", "secret_key": secret}}
	code, out = f.call(http.MethodPost, "/api/v1/manage/transfers", f.jwt, wrong)
	assert.Equal(t, http.StatusBadRequest, code)
	assert.Contains(t, fmt.Sprint(out), "source_denied")
	assert.Contains(t, fmt.Sprint(out), "InvalidAccessKeyId", "the source's answer named")

	code, out = f.call(http.MethodPost, "/api/v1/manage/transfers", f.jwt, body)
	require.Equal(t, http.StatusCreated, code, "%v", out)
	id, _ := out["id"].(string)
	require.NotEmpty(t, id)
	assert.Equal(t, transferPending, out["state"])
	code, list := f.call(http.MethodGet, "/api/v1/manage/transfers", devJWT, nil)
	require.Equal(t, http.StatusOK, code)
	code, one := f.call(http.MethodGet, "/api/v1/manage/transfers/"+id, f.jwt, nil)
	require.Equal(t, http.StatusOK, code)
	code, cancelled := f.call(http.MethodDelete, "/api/v1/manage/transfers/"+id, f.jwt, nil)
	require.Equal(t, http.StatusOK, code)
	assert.Equal(t, transferCancelled, cancelled["state"])
	assert.Equal(t, false, cancelled["source_key_held"])
	code, _ = f.call(http.MethodPost, "/api/v1/manage/transfers/"+id+"/resume", f.jwt, nil)
	assert.Equal(t, http.StatusConflict, code, "only a paused transfer resumes")
	for _, m := range []map[string]any{out, list, one, cancelled} {
		s := fmt.Sprint(m)
		assert.NotContains(t, s, secret)
		assert.NotContains(t, s, src.keyID)
	}
	code, _ = f.call(http.MethodGet, "/api/v1/manage/transfers/"+uuidLike(), f.jwt, nil)
	assert.Equal(t, http.StatusNotFound, code)
}

func uuidLike() string { return "00000000-0000-4000-8000-000000000000" }
