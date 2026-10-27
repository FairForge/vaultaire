package api

// WP-EDGE-1 Part A (Stage 1a Prompt 44): the control plane of the edge
// mover at the handlers — the admit's decisions and the commit's rows,
// compared with a PUT through the origin of the same bytes. The backend is
// a fake EdgeWriter the tests "upload" to by hand (what the Worker does);
// the SigV4 descriptors are signed with the real signer against a real
// api_keys row.

import (
	"bytes"
	"context"
	"crypto/md5" // #nosec G501 — the S3 ETag
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/FairForge/vaultaire/internal/common"
	"github.com/FairForge/vaultaire/internal/config"
	"github.com/FairForge/vaultaire/internal/crypto"
	"github.com/FairForge/vaultaire/internal/drivers"
	"github.com/FairForge/vaultaire/internal/engine"
	"github.com/FairForge/vaultaire/internal/flags"
	"github.com/FairForge/vaultaire/internal/tenant"
	"github.com/FairForge/vaultaire/internal/testutil"
	"github.com/FairForge/vaultaire/internal/usage"
	promtestutil "github.com/prometheus/client_golang/prometheus/testutil"
)

// edgeFakeDriver is an in-memory fixed-bucket backend that is an
// EdgeWriter: the tests put the body where the Worker would.
type edgeFakeDriver struct {
	mu          sync.Mutex
	objects     map[string][]byte // t-<tenant>/<container>/<artifact> → bytes
	uploads     map[string]map[int]string
	presigns    atomic.Int32
	heads       atomic.Int32              // HeadObject calls (RP68-3's sample)
	ranges      atomic.Int32              // GetRangeInfo calls (RP70-3's first-segment open)
	streaming   engine.EdgeStreamingProps // what the fake does with a header-signed aws-chunked write
	signs       atomic.Int32              // SignPut / SignUploadPart calls
	rangeLens   []int64                   // the length each GetRangeInfo asked for
	creates     atomic.Int32
	aborts      atomic.Int32
	deletes     []string
	failPresign atomic.Bool
	// completeErr, when set, is CompleteMultipart's answer (RP72-4).
	completeErr error
	// deleteGate, when set, holds every Delete until it is closed;
	// deleteEntered is closed once a Delete has started (RP64-1's test).
	deleteGate    chan struct{}
	deleteEntered chan struct{}
	deleteOnce    sync.Once
}

func newEdgeFakeDriver() *edgeFakeDriver {
	return &edgeFakeDriver{objects: map[string][]byte{}, uploads: map[string]map[int]string{}}
}

func (d *edgeFakeDriver) key(ctx context.Context, container, artifact string) (string, error) {
	tid := common.GetTenantID(ctx)
	if tid == "" {
		return "", drivers.ErrNoTenant
	}
	return "t-" + tid + "/" + container + "/" + artifact, nil
}

func (d *edgeFakeDriver) Name() string { return "edgefake" }
func (d *edgeFakeDriver) Get(ctx context.Context, container, artifact string) (io.ReadCloser, error) {
	k, err := d.key(ctx, container, artifact)
	if err != nil {
		return nil, err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	b, ok := d.objects[k]
	if !ok {
		return nil, &engine.NotFoundError{Container: container, Artifact: artifact}
	}
	return io.NopCloser(bytes.NewReader(b)), nil
}
func (d *edgeFakeDriver) Put(ctx context.Context, container, artifact string, data io.Reader, opts ...engine.PutOption) error {
	k, err := d.key(ctx, container, artifact)
	if err != nil {
		return err
	}
	b, err := io.ReadAll(data)
	if err != nil {
		return err
	}
	d.mu.Lock()
	d.objects[k] = b
	d.mu.Unlock()
	// The backend's answer, as the S3-class drivers report it (RP72-1): a
	// single PutObject's ETag is the body's MD5.
	sum := md5.Sum(b) // #nosec G401
	engine.ApplyPutOptions(opts...).Outcome.SetBackendETag(`"` + hex.EncodeToString(sum[:]) + `"`)
	return nil
}
func (d *edgeFakeDriver) Delete(ctx context.Context, container, artifact string) error {
	k, err := d.key(ctx, container, artifact)
	if err != nil {
		return err
	}
	if d.deleteEntered != nil {
		d.deleteOnce.Do(func() { close(d.deleteEntered) })
	}
	if d.deleteGate != nil {
		select {
		case <-d.deleteGate:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.deletes = append(d.deletes, k)
	delete(d.objects, k)
	return nil
}
func (d *edgeFakeDriver) List(context.Context, string, string) ([]string, error) { return nil, nil }
func (d *edgeFakeDriver) Exists(ctx context.Context, container, artifact string) (bool, error) {
	k, err := d.key(ctx, container, artifact)
	if err != nil {
		return false, err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	_, ok := d.objects[k]
	return ok, nil
}
func (d *edgeFakeDriver) HealthCheck(context.Context) error { return nil }

// ObjectKey implements engine.KeyAddresser (the chunk move asks it).
func (d *edgeFakeDriver) ObjectKey(ctx context.Context, container, artifact string) string {
	k, _ := d.key(ctx, container, artifact)
	return k
}

// HeadObject answers the blob's MD5 ETag and size (engine.ObjectHeader).
func (d *edgeFakeDriver) HeadObject(ctx context.Context, container, artifact string) (engine.RangeInfo, error) {
	d.heads.Add(1)
	k, err := d.key(ctx, container, artifact)
	if err != nil {
		return engine.RangeInfo{}, err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	b, ok := d.objects[k]
	if !ok {
		return engine.RangeInfo{}, &engine.NotFoundError{Container: container, Artifact: artifact}
	}
	sum := md5.Sum(b) // #nosec G401
	return engine.RangeInfo{ETag: hex.EncodeToString(sum[:]), Size: int64(len(b))}, nil
}

// GetRangeInfo serves [offset, offset+length) of a blob with its MD5 ETag
// and whole size (engine.VersionedRangeGetter): the codec commit's open of
// the first stored chunk's first segment (RP70-3).
func (d *edgeFakeDriver) GetRangeInfo(ctx context.Context, container, artifact string, offset, length int64) (io.ReadCloser, engine.RangeInfo, error) {
	d.ranges.Add(1)
	k, err := d.key(ctx, container, artifact)
	if err != nil {
		return nil, engine.RangeInfo{}, err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.rangeLens = append(d.rangeLens, length)
	b, ok := d.objects[k]
	if !ok {
		return nil, engine.RangeInfo{}, &engine.NotFoundError{Container: container, Artifact: artifact}
	}
	if offset < 0 || offset >= int64(len(b)) || length <= 0 {
		return nil, engine.RangeInfo{}, fmt.Errorf("InvalidRange (fake): %d+%d of %d", offset, length, len(b))
	}
	end := min(offset+length, int64(len(b)))
	sum := md5.Sum(b) // #nosec G401
	return io.NopCloser(bytes.NewReader(bytes.Clone(b[offset:end]))), engine.RangeInfo{ETag: hex.EncodeToString(sum[:]), Size: int64(len(b))}, nil
}

// signStreaming is the fake's header-signed aws-chunked request: the plain
// URL and the headers a real signer would answer (engine.EdgeWriter.SignPut).
func (d *edgeFakeDriver) signStreaming(key, query string, decoded int64, trailer string) (map[string]string, string, error) {
	d.signs.Add(1)
	if d.failPresign.Load() {
		return nil, "", errors.New("sign refused (test)")
	}
	u := "https://fake.backend.invalid/vaultaire/" + key
	if query != "" {
		u += "?" + query
	}
	return map[string]string{
		"authorization":                "AWS4-HMAC-SHA256 Credential=FAKE/20261027/us-east-1/s3/aws4_request, SignedHeaders=content-encoding;host;x-amz-content-sha256;x-amz-date;x-amz-decoded-content-length;x-amz-trailer, Signature=deadbeef",
		"x-amz-date":                   time.Now().UTC().Format("20060102T150405Z"),
		"x-amz-content-sha256":         "STREAMING-UNSIGNED-PAYLOAD-TRAILER",
		"content-encoding":             "aws-chunked",
		"x-amz-decoded-content-length": fmt.Sprint(decoded),
		"x-amz-trailer":                trailer,
		"host":                         "fake.backend.invalid",
	}, u, nil
}
func (d *edgeFakeDriver) SignPut(ctx context.Context, container, artifact string, decoded int64, trailer string) (map[string]string, string, error) {
	k, err := d.key(ctx, container, artifact)
	if err != nil {
		return nil, "", err
	}
	return d.signStreaming(k, "", decoded, trailer)
}
func (d *edgeFakeDriver) SignUploadPart(ctx context.Context, container, artifact, uploadID string, partNumber int, decoded int64, trailer string) (map[string]string, string, error) {
	k, err := d.key(ctx, container, artifact)
	if err != nil {
		return nil, "", err
	}
	return d.signStreaming(k, fmt.Sprintf("partNumber=%d&uploadId=%s", partNumber, uploadID), decoded, trailer)
}
func (d *edgeFakeDriver) EdgeStreaming() engine.EdgeStreamingProps { return d.streaming }

func (d *edgeFakeDriver) presign(key string, p engine.EdgePresign, query string) (string, []string, error) {
	d.presigns.Add(1)
	if d.failPresign.Load() {
		return "", nil, errors.New("presign refused (test)")
	}
	signed := []string{"content-length", "host"}
	if p.ContentMD5 != "" {
		signed = append(signed, "content-md5")
	}
	if p.ContentType != "" {
		signed = append(signed, "content-type")
	}
	u := "https://fake.backend.invalid/vaultaire/" + key + "?" + query + "X-Amz-Algorithm=AWS4-HMAC-SHA256&X-Amz-Signature=deadbeef&X-Amz-SignedHeaders=" + url.QueryEscape(strings.Join(signed, ";"))
	return u, signed, nil
}
func (d *edgeFakeDriver) PresignPut(ctx context.Context, container, artifact string, p engine.EdgePresign) (string, []string, error) {
	k, err := d.key(ctx, container, artifact)
	if err != nil {
		return "", nil, err
	}
	return d.presign(k, p, "")
}
func (d *edgeFakeDriver) PresignGet(ctx context.Context, container, artifact string, p engine.EdgePresign) (string, []string, error) {
	k, err := d.key(ctx, container, artifact)
	if err != nil {
		return "", nil, err
	}
	d.presigns.Add(1)
	if d.failPresign.Load() {
		return "", nil, errors.New("presign refused (test)")
	}
	signed := []string{"host", "if-match"}
	if p.Range != "" {
		signed = append(signed, "range")
	}
	u := "https://fake.backend.invalid/vaultaire/" + k + "?X-Amz-Algorithm=AWS4-HMAC-SHA256&X-Amz-Signature=deadbeef&X-Amz-SignedHeaders=" + url.QueryEscape(strings.Join(signed, ";")) +
		"&x-probe-if-match=" + url.QueryEscape(p.IfMatch) + "&x-probe-range=" + url.QueryEscape(p.Range)
	return u, signed, nil
}
func (d *edgeFakeDriver) CreateMultipart(ctx context.Context, container, artifact, _ string) (string, error) {
	if _, err := d.key(ctx, container, artifact); err != nil {
		return "", err
	}
	d.creates.Add(1)
	id := "bu-" + uuid.New().String()
	d.mu.Lock()
	d.uploads[id] = map[int]string{}
	d.mu.Unlock()
	return id, nil
}
func (d *edgeFakeDriver) PresignUploadPart(ctx context.Context, container, artifact, uploadID string, partNumber int, p engine.EdgePresign) (string, []string, error) {
	k, err := d.key(ctx, container, artifact)
	if err != nil {
		return "", nil, err
	}
	return d.presign(k, p, fmt.Sprintf("uploadId=%s&partNumber=%d&", uploadID, partNumber))
}
func (d *edgeFakeDriver) CompleteMultipart(ctx context.Context, container, artifact, uploadID string, parts []engine.EdgePart) (string, error) {
	k, err := d.key(ctx, container, artifact)
	if err != nil {
		return "", err
	}
	if d.completeErr != nil {
		return "", d.completeErr
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	up, ok := d.uploads[uploadID]
	if !ok {
		return "", errors.New("NoSuchUpload (fake)")
	}
	var body []byte
	h := md5.New() // #nosec G401
	for _, p := range parts {
		pk := k + "#" + uploadID + "#" + fmt.Sprint(p.Number)
		b, ok := d.objects[pk]
		if !ok || strings.Trim(up[p.Number], `"`) != strings.Trim(p.ETag, `"`) {
			return "", fmt.Errorf("InvalidPart %d (fake)", p.Number)
		}
		sum := md5.Sum(b) // #nosec G401
		h.Write(sum[:])
		body = append(body, b...)
		delete(d.objects, pk)
	}
	d.objects[k] = body
	delete(d.uploads, uploadID)
	return fmt.Sprintf(`"%x-%d"`, h.Sum(nil), len(parts)), nil
}
func (d *edgeFakeDriver) AbortMultipart(ctx context.Context, container, artifact, uploadID string) error {
	if _, err := d.key(ctx, container, artifact); err != nil {
		return err
	}
	d.aborts.Add(1)
	d.mu.Lock()
	delete(d.uploads, uploadID)
	d.mu.Unlock()
	return nil
}

// workerPut is what the Worker does with an admitted PUT: the body lands
// at the storage key on the backend; the ETag is its MD5.
func (d *edgeFakeDriver) workerPut(tenantID, container, storageKey string, body []byte) string {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.objects["t-"+tenantID+"/"+container+"/"+storageKey] = body
	sum := md5.Sum(body) // #nosec G401
	return `"` + hex.EncodeToString(sum[:]) + `"`
}

// workerPutPart is the Worker's UploadPart to the backend upload.
func (d *edgeFakeDriver) workerPutPart(tenantID, container, storageKey, uploadID string, n int, body []byte) string {
	d.mu.Lock()
	defer d.mu.Unlock()
	sum := md5.Sum(body) // #nosec G401
	etag := `"` + hex.EncodeToString(sum[:]) + `"`
	k := "t-" + tenantID + "/" + container + "/" + storageKey
	d.objects[k+"#"+uploadID+"#"+fmt.Sprint(n)] = body
	if d.uploads[uploadID] == nil {
		d.uploads[uploadID] = map[int]string{}
	}
	d.uploads[uploadID][n] = etag
	return etag
}

func (d *edgeFakeDriver) has(tenantID, container, storageKey string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	_, ok := d.objects["t-"+tenantID+"/"+container+"/"+storageKey]
	return ok
}

type edgeFixture struct {
	t        *testing.T
	db       *sql.DB
	srv      *Server
	drv      *edgeFakeDriver
	qm       *usage.QuotaManager
	fl       *flags.Service
	tenantID string
	userID   string
	bucket   string
	ak, sk   string
	host     string
	token    string
}

const edgeTestToken = "edge-test-token-0123456789abcdef"

func setupEdgeFixture(t *testing.T) *edgeFixture {
	t.Helper()
	db, err := sql.Open("postgres", testutil.DSN())
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	require.NoError(t, db.Ping())

	logger := zap.NewNop()
	eng := engine.NewEngine(nil, logger, nil)
	drv := newEdgeFakeDriver()
	eng.AddDriver("edgefake", drv)
	eng.SetPrimary("edgefake")

	id := uuid.New().String()
	f := &edgeFixture{t: t, db: db, drv: drv, tenantID: "edge-" + id[:8], userID: id, bucket: "edge-bkt",
		ak: "VKEDGE" + strings.ToUpper(id[:8]), sk: "SK" + id, host: "s3.test.local", token: edgeTestToken}
	email := "edge-" + id[:8] + "@test.local"
	f.exec(`INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`, f.userID, email)
	f.exec(`INSERT INTO tenants (id, name, email, access_key, secret_key) VALUES ($1, 'Edge Test', $2, $3, $4)`, f.tenantID, email, f.ak, f.sk)
	f.exec(`INSERT INTO api_keys (id, user_id, tenant_id, is_primary, name, key_id, secret_hash, secret_key) VALUES ($1, $2, $3, TRUE, 'primary', $4, 'h', $5)`,
		uuid.New().String(), f.userID, f.tenantID, f.ak, f.sk)
	f.exec(`INSERT INTO buckets (tenant_id, name, visibility) VALUES ($1, $2, 'private')`, f.tenantID, f.bucket)
	f.qm = usage.NewQuotaManager(db)
	require.NoError(t, f.qm.CreateTenant(context.Background(), f.tenantID, "starter", 1<<30))

	f.fl = flags.New(db, logger)
	registerFlags(f.fl)
	require.NoError(t, f.fl.Set(context.Background(), flagEdgeIngest, f.tenantID, true, "test"))
	require.NoError(t, f.fl.Refresh(context.Background()))

	f.srv = &Server{logger: logger, router: chi.NewRouter(), engine: eng, db: db, quotaManager: f.qm, flags: f.fl,
		edge:   edgeConfig{token: []byte(f.token), trustAnyPeer: true, minBytes: 0}, // off: the tests move KiB bodies
		config: &config.Config{Server: config.ServerConfig{Port: 8000}}}
	f.srv.edgeReaper = newEdgeAdmitReaper(f.srv)
	f.srv.multipartReaper = NewMultipartReaper(db, logger)
	f.srv.multipartReaper.engine = eng
	t.Cleanup(func() {
		time.Sleep(50 * time.Millisecond)
		for _, q := range []string{
			`DELETE FROM feature_flags WHERE tenant_id = $1`,
			`DELETE FROM edge_admits WHERE tenant_id = $1`,
			`DELETE FROM retired_blobs WHERE tenant_id = $1`,
			`DELETE FROM events WHERE tenant_id = $1`,
			`DELETE FROM s3_access_log WHERE tenant_id = $1`,
			`DELETE FROM object_locks WHERE tenant_id = $1`,
			`DELETE FROM object_versions WHERE tenant_id = $1`,
			`DELETE FROM multipart_uploads WHERE tenant_id = $1`,
			`DELETE FROM object_head_cache WHERE tenant_id = $1`,
			`DELETE FROM buckets WHERE tenant_id = $1`,
			`DELETE FROM quota_usage_events WHERE tenant_id = $1`,
			`DELETE FROM tenant_quotas WHERE tenant_id = $1`,
			`DELETE FROM tenants WHERE id = $1`,
		} {
			_, _ = db.Exec(q, f.tenantID)
		}
		_, _ = db.Exec(`DELETE FROM api_keys WHERE user_id = $1`, f.userID)
		_, _ = db.Exec(`DELETE FROM users WHERE id = $1`, f.userID)
	})
	return f
}

func (f *edgeFixture) exec(q string, args ...any) {
	f.t.Helper()
	_, err := f.db.Exec(q, args...)
	require.NoError(f.t, err, q)
}

func (f *edgeFixture) used() int64 {
	var n int64
	require.NoError(f.t, f.db.QueryRow(`SELECT storage_used_bytes FROM tenant_quotas WHERE tenant_id = $1`, f.tenantID).Scan(&n))
	return n
}

// signed builds the Worker's description of a client's SigV4-signed PUT:
// the request as the real signer makes it, every header lower-cased.
func (f *edgeFixture) signed(path string, query url.Values, body []byte, hdr map[string]string, payloadHash string) edgeAdmitRequest {
	f.t.Helper()
	u := "https://" + f.host + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	r, err := http.NewRequest(http.MethodPut, u, bytes.NewReader(body))
	require.NoError(f.t, err)
	r.ContentLength = int64(len(body))
	r.Header.Set("Content-Length", fmt.Sprint(len(body)))
	for k, v := range hdr {
		r.Header.Set(k, v)
	}
	if payloadHash == "" {
		sum := sha256.Sum256(body)
		payloadHash = hex.EncodeToString(sum[:])
	}
	r.Header.Set("X-Amz-Content-Sha256", payloadHash)
	require.NoError(f.t, v4.NewSigner().SignHTTP(context.Background(), aws.Credentials{AccessKeyID: f.ak, SecretAccessKey: f.sk},
		r, payloadHash, "s3", "us-east-1", time.Now()))
	headers := map[string]string{"host": f.host}
	for k, v := range r.Header {
		headers[strings.ToLower(k)] = strings.Join(v, ", ")
	}
	op := edgeOpPut
	if query.Get("uploadId") != "" {
		op = edgeOpPart
	}
	return edgeAdmitRequest{Op: op, Method: http.MethodPut, Host: f.host, Path: r.URL.EscapedPath(), Query: r.URL.RawQuery,
		Headers: headers, ClientIP: "203.0.113.9", RequestID: "req-" + uuid.New().String()[:8]}
}

// signedStreaming is the Worker's description of aws-cli's DEFAULT upload
// (≥ 2.23): an aws-chunked body with an unsigned payload and a checksum
// trailer, Transfer-Encoding: chunked — no Content-Length — signed with
// the client's key.
func (f *edgeFixture) signedStreaming(path string, query url.Values, decoded int64, trailer, payloadHash string) edgeAdmitRequest {
	f.t.Helper()
	u := "https://" + f.host + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	r, err := http.NewRequest(http.MethodPut, u, nil)
	require.NoError(f.t, err)
	if payloadHash == "" {
		payloadHash = "STREAMING-UNSIGNED-PAYLOAD-TRAILER"
	}
	r.Header.Set("Content-Encoding", "aws-chunked")
	r.Header.Set("X-Amz-Decoded-Content-Length", fmt.Sprint(decoded))
	r.Header.Set("X-Amz-Trailer", trailer)
	r.Header.Set("X-Amz-Sdk-Checksum-Algorithm", "CRC64NVME")
	r.Header.Set("X-Amz-Content-Sha256", payloadHash)
	require.NoError(f.t, v4.NewSigner().SignHTTP(context.Background(), aws.Credentials{AccessKeyID: f.ak, SecretAccessKey: f.sk},
		r, payloadHash, "s3", "us-east-1", time.Now()))
	headers := map[string]string{"host": f.host}
	for k, v := range r.Header {
		headers[strings.ToLower(k)] = strings.Join(v, ", ")
	}
	op := edgeOpPut
	if query.Get("uploadId") != "" {
		op = edgeOpPart
	}
	return edgeAdmitRequest{Op: op, Method: http.MethodPut, Host: f.host, Path: r.URL.EscapedPath(), Query: r.URL.RawQuery,
		Headers: headers, ClientIP: "203.0.113.9", RequestID: "req-" + uuid.New().String()[:8]}
}

func (f *edgeFixture) call(path string, body any, token string) *httptest.ResponseRecorder {
	f.t.Helper()
	b, err := json.Marshal(body)
	require.NoError(f.t, err)
	r := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(b))
	r.Header.Set("Content-Type", "application/json")
	if token != "" {
		r.Header.Set(edgeTokenHeader, token)
	}
	w := httptest.NewRecorder()
	switch path {
	case "/api/v1/edge/admit":
		f.srv.handleEdgeAdmit(w, r)
	default:
		f.srv.handleEdgeCommit(w, r)
	}
	return w
}

func (f *edgeFixture) admit(req edgeAdmitRequest) edgeAdmitResponse {
	f.t.Helper()
	w := f.call("/api/v1/edge/admit", req, f.token)
	require.Equal(f.t, http.StatusOK, w.Code, w.Body.String())
	var resp edgeAdmitResponse
	require.NoError(f.t, json.Unmarshal(w.Body.Bytes(), &resp))
	return resp
}

func (f *edgeFixture) commit(req edgeCommitRequest) (*edgeRelay, int) {
	f.t.Helper()
	w := f.call("/api/v1/edge/commit", req, f.token)
	if w.Code != http.StatusOK {
		return nil, w.Code
	}
	var relay edgeRelay
	require.NoError(f.t, json.Unmarshal(w.Body.Bytes(), &relay))
	return &relay, w.Code
}

// okCommit is the Worker's report of a backend 200: the backend's answer,
// nothing of the bytes (the Worker is not in the byte path).
func okCommit(admitID string, _ []byte, backendETag string) edgeCommitRequest {
	return edgeCommitRequest{AdmitID: admitID, Outcome: edgeOutcomeOK, BackendStatus: 200, BackendETag: backendETag, DurationMS: 7}
}

func (f *edgeFixture) pending() int {
	var n int
	require.NoError(f.t, f.db.QueryRow(`SELECT COUNT(*) FROM edge_admits WHERE tenant_id = $1 AND state = 'pending'`, f.tenantID).Scan(&n))
	return n
}

func (f *edgeFixture) admitState(id string) string {
	var s string
	require.NoError(f.t, f.db.QueryRow(`SELECT state FROM edge_admits WHERE id = $1`, id).Scan(&s))
	return s
}

type edgeHeadRow struct {
	size                            int64
	etag, backend, floor, ct, sk    string
	meta                            string
	cacheControl, contentDisp, lang string
}

func (f *edgeFixture) head(key string) (edgeHeadRow, bool) {
	var h edgeHeadRow
	err := f.db.QueryRow(`SELECT size_bytes, etag, COALESCE(backend_name,''), floor, content_type, COALESCE(storage_key,''), metadata::text,
		cache_control, content_disposition, content_language FROM object_head_cache WHERE tenant_id = $1 AND bucket = $2 AND object_key = $3`,
		f.tenantID, f.bucket, key).Scan(&h.size, &h.etag, &h.backend, &h.floor, &h.ct, &h.sk, &h.meta, &h.cacheControl, &h.contentDisp, &h.lang)
	if errors.Is(err, sql.ErrNoRows) {
		return h, false
	}
	require.NoError(f.t, err)
	return h, true
}

// originPut writes key through the origin's own PUT handler (the control).
func (f *edgeFixture) originPut(key string, body []byte, hdr map[string]string) *httptest.ResponseRecorder {
	f.t.Helper()
	r := httptest.NewRequest(http.MethodPut, "/"+f.bucket+"/"+key, bytes.NewReader(body))
	r.ContentLength = int64(len(body))
	for k, v := range hdr {
		r.Header.Set(k, v)
	}
	tn := &tenant.Tenant{ID: f.tenantID, Namespace: "tenant/" + f.tenantID + "/"}
	r = r.WithContext(s3Ctx(r.Context(), tn))
	w := httptest.NewRecorder()
	f.srv.testMode = true
	defer func() { f.srv.testMode = false }()
	f.srv.handleS3Request(w, r)
	return w
}

func (f *edgeFixture) container() string { return f.tenantID + "_" + f.bucket }

// --- the routes ---

func TestEdgeRoutes_NotMountedWithoutToken(t *testing.T) {
	db, err := sql.Open("postgres", testutil.DSN())
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	require.NoError(t, db.Ping())
	for _, k := range []string{"EDGE_SERVICE_TOKEN", "EDGE_SERVICE_TOKEN_FILE", "EDGE_TRUST_ANY_PEER", "CREDENTIALS_DIRECTORY"} {
		t.Setenv(k, "")
	}
	srv := NewServer(&config.Config{Server: config.ServerConfig{Port: 0}}, zap.NewNop(),
		engine.NewEngine(nil, zap.NewNop(), &engine.Config{DefaultBackend: "local"}), nil, db)
	for _, p := range []string{"/api/v1/edge/admit", "/api/v1/edge/commit"} {
		r := httptest.NewRequest(http.MethodPost, p, strings.NewReader(`{}`))
		r.Header.Set(edgeTokenHeader, "anything")
		w := httptest.NewRecorder()
		srv.httpServer.Handler.ServeHTTP(w, r)
		assert.NotEqual(t, http.StatusOK, w.Code, "%s is not mounted without a token", p)
	}

	// With a token: a wrong token and a non-Cloudflare peer are 404.
	t.Setenv("EDGE_SERVICE_TOKEN", edgeTestToken)
	srv2 := NewServer(&config.Config{Server: config.ServerConfig{Port: 0}}, zap.NewNop(),
		engine.NewEngine(nil, zap.NewNop(), &engine.Config{DefaultBackend: "local"}), nil, db)
	for _, tc := range []struct{ name, token, peer string }{
		{"wrong token", "nope-nope-nope-nope-nope", "173.245.48.10:1"},
		{"no token", "", "173.245.48.10:1"},
		{"right token, direct peer", edgeTestToken, "203.0.113.5:1"},
	} {
		r := httptest.NewRequest(http.MethodPost, "/api/v1/edge/admit", strings.NewReader(`{"op":"put"}`))
		r.RemoteAddr = tc.peer
		if tc.token != "" {
			r.Header.Set(edgeTokenHeader, tc.token)
		}
		w := httptest.NewRecorder()
		srv2.httpServer.Handler.ServeHTTP(w, r)
		assert.Equal(t, http.StatusNotFound, w.Code, tc.name)
	}
	// The right token from a Cloudflare peer reaches the handler (a bad
	// JSON body is its 400, not a 404).
	r := httptest.NewRequest(http.MethodPost, "/api/v1/edge/admit", strings.NewReader(`not json`))
	r.RemoteAddr = "173.245.48.10:1"
	r.Header.Set(edgeTokenHeader, edgeTestToken)
	w := httptest.NewRecorder()
	srv2.httpServer.Handler.ServeHTTP(w, r)
	assert.Equal(t, http.StatusBadRequest, w.Code, "the handler answered")
}

func TestEdgeRoutes_NeverCapturedByTheS3Gate(t *testing.T) {
	t.Setenv("EDGE_SERVICE_TOKEN", edgeTestToken)
	t.Setenv("EDGE_TRUST_ANY_PEER", "true")
	srv, hits := gateServer(t, map[string]string{"VAULTAIRE_PUBLIC_HOST": "https://stored.test", "VAULTAIRE_ENDPOINT": "https://s3.stored.test"})

	// The client's SigV4 Authorization INSIDE the JSON: the handler's.
	body := `{"op":"put","method":"PUT","host":"s3.stored.test","path":"/b/k","headers":{"authorization":"AWS4-HMAC-SHA256 Credential=AKIDX/20260101/us-east-1/s3/aws4_request, SignedHeaders=host, Signature=00"}}`
	r := httptest.NewRequest(http.MethodPost, "/api/v1/edge/admit", strings.NewReader(body))
	r.Host = "stored.test"
	r.Header.Set(edgeTokenHeader, edgeTestToken)
	w := serve(srv.httpServer.Handler, r)
	assert.Empty(t, *hits, "no AWS credential on the call itself: never the S3 API's")
	assert.NotEqual(t, http.StatusTeapot, w.Code)
	assert.Equal(t, http.StatusOK, w.Code, w.Body.String()) // a decision (a refusal relayed), not the S3 API

	// The index route (WP-EDGE-2): the token gate's, never the S3 API's.
	ri := httptest.NewRequest(http.MethodPost, "/api/v1/edge/index", strings.NewReader(`{"admit_id":"x","chunks":[{"hash":"00","size":1}]}`))
	ri.Host = "stored.test"
	ri.Header.Set(edgeTokenHeader, edgeTestToken)
	wi := serve(srv.httpServer.Handler, ri)
	assert.Empty(t, *hits, "the index call never reaches the S3 API")
	assert.NotEqual(t, http.StatusTeapot, wi.Code)

	// An AWS4 header ON the call: the gate's (Decision 8).
	r2 := sigV4Signed(t, http.MethodPost, "stored.test", "/api/v1/edge/admit")
	r2.Header.Set(edgeTokenHeader, edgeTestToken)
	w2 := serve(srv.httpServer.Handler, r2)
	assert.Equal(t, http.StatusTeapot, w2.Code)
	assert.Equal(t, []string{"POST stored.test/api/v1/edge/admit"}, *hits)
}

// --- the admit ---

func TestEdgeAdmit_FlagOffPassesThrough(t *testing.T) {
	f := setupEdgeFixture(t)
	require.NoError(t, f.fl.Unset(context.Background(), flagEdgeIngest, f.tenantID))
	require.NoError(t, f.fl.Refresh(context.Background()))
	body := testBytes(1000)
	resp := f.admit(f.signed("/"+f.bucket+"/flag-off.bin", nil, body, nil, ""))
	assert.False(t, resp.Edge)
	assert.Nil(t, resp.Refusal)
	assert.Equal(t, "flag_off", resp.Reason)
	assert.Zero(t, f.pending(), "no row")
	assert.Zero(t, f.used(), "nothing reserved")
}

func TestEdgeAdmit_AuthFailuresAreRelayedAsTheOriginsXML(t *testing.T) {
	f := setupEdgeFixture(t)
	body := testBytes(100)
	req := f.signed("/"+f.bucket+"/k.bin", nil, body, nil, "")
	// A tampered Host: the signature covers it.
	req.Host = "evil.test.local"
	req.Headers["host"] = "evil.test.local"
	resp := f.admit(req)
	require.NotNil(t, resp.Refusal)
	assert.Equal(t, http.StatusForbidden, resp.Refusal.Status)
	assert.Contains(t, resp.Refusal.Body, "SignatureDoesNotMatch")
	assert.Equal(t, "application/xml", resp.Refusal.Headers["Content-Type"])

	// A key restricted by IP: the allow-list is applied to the client IP
	// the Worker reports.
	f.exec(`UPDATE api_keys SET ip_allowlist = '{10.0.0.0/8}' WHERE key_id = $1`, f.ak)
	resp = f.admit(f.signed("/"+f.bucket+"/k.bin", nil, body, nil, ""))
	require.NotNil(t, resp.Refusal)
	assert.Equal(t, http.StatusForbidden, resp.Refusal.Status)
	assert.Contains(t, resp.Refusal.Body, "restricted by IP")
	f.exec(`UPDATE api_keys SET ip_allowlist = '{}' WHERE key_id = $1`, f.ak)

	// A scoped key without PutObject.
	f.exec(`UPDATE api_keys SET permissions = '["GetObject"]' WHERE key_id = $1`, f.ak)
	resp = f.admit(f.signed("/"+f.bucket+"/k.bin", nil, body, nil, ""))
	require.NotNil(t, resp.Refusal)
	assert.Contains(t, resp.Refusal.Body, "does not have PutObject access")
	f.exec(`UPDATE api_keys SET permissions = '["*"]' WHERE key_id = $1`, f.ak)

	// An unsigned request is the origin's.
	req = f.signed("/"+f.bucket+"/k.bin", nil, body, nil, "")
	delete(req.Headers, "authorization")
	resp = f.admit(req)
	assert.False(t, resp.Edge)
	assert.Nil(t, resp.Refusal)
	assert.Equal(t, "unsigned", resp.Reason)
	assert.Zero(t, f.pending())
}

func TestEdgeAdmit_OnlyUnsealedWholeWritesQualify(t *testing.T) {
	f := setupEdgeFixture(t)
	body := testBytes(1000)
	pass := func(name string, req edgeAdmitRequest, wantReason string) {
		t.Helper()
		resp := f.admit(req)
		assert.False(t, resp.Edge, name)
		assert.Nil(t, resp.Refusal, name)
		assert.Contains(t, resp.Reason, wantReason, name)
	}
	// SSE-C: sealed at the origin.
	key := strings.Repeat("k", 32)
	kmd5 := md5.Sum([]byte(key)) // #nosec G401
	pass("ssec", f.signed("/"+f.bucket+"/c.bin", nil, body, map[string]string{
		"x-amz-server-side-encryption-customer-algorithm": "AES256",
		"x-amz-server-side-encryption-customer-key":       base64.StdEncoding.EncodeToString([]byte(key)),
		"x-amz-server-side-encryption-customer-key-MD5":   base64.StdEncoding.EncodeToString(kmd5[:]),
	}, ""), "ssec")
	// A request asking for SSE-S3 on a process without a master key: the
	// placement's refusal, relayed (503, as the origin answers).
	resp := f.admit(f.signed("/"+f.bucket+"/s.bin", nil, body, map[string]string{"x-amz-server-side-encryption": "AES256"}, ""))
	require.NotNil(t, resp.Refusal, "sse requested")
	assert.Equal(t, http.StatusServiceUnavailable, resp.Refusal.Status)
	// An sse_enabled bucket: the same refusal.
	f.exec(`UPDATE buckets SET sse_enabled = TRUE WHERE tenant_id = $1 AND name = $2`, f.tenantID, f.bucket)
	resp = f.admit(f.signed("/"+f.bucket+"/s2.bin", nil, body, nil, ""))
	require.NotNil(t, resp.Refusal, "sse bucket")
	assert.Equal(t, http.StatusServiceUnavailable, resp.Refusal.Status)
	f.exec(`UPDATE buckets SET sse_enabled = FALSE WHERE tenant_id = $1 AND name = $2`, f.tenantID, f.bucket)
	// An ssec_required bucket without a key: the origin's 400 follows.
	f.exec(`UPDATE buckets SET ssec_required = TRUE WHERE tenant_id = $1 AND name = $2`, f.tenantID, f.bucket)
	resp = f.admit(f.signed("/"+f.bucket+"/s3.bin", nil, body, nil, ""))
	require.NotNil(t, resp.Refusal, "ssec_required")
	assert.Equal(t, http.StatusBadRequest, resp.Refusal.Status)
	assert.Contains(t, resp.Refusal.Body, "InvalidRequest")
	f.exec(`UPDATE buckets SET ssec_required = FALSE WHERE tenant_id = $1 AND name = $2`, f.tenantID, f.bucket)
	// A chunking-eligible size with the flag on: the chunk path.
	f.srv.chunkingThreshold = 512
	f.srv.gci = crypto.NewGlobalContentIndex(f.db)
	pass("chunked", f.signed("/"+f.bucket+"/big.bin", nil, testBytes(600), nil, ""), "placement_chunked")
	f.srv.gci = nil
	f.srv.chunkingThreshold = 0
	// A public bucket: PUBLIC → r2 (registered, as on prod), which keeps
	// DirectUploader and is no EdgeWriter.
	f.exec(`INSERT INTO buckets (tenant_id, name, visibility) VALUES ($1, 'pub', 'public-read')`, f.tenantID)
	f.exec(`UPDATE tenants SET slug = $2 WHERE id = $1`, f.tenantID, "edge-"+f.tenantID[5:])
	require.NoError(t, f.srv.engine.AddDriver("r2", drivers.NewLocalDriver(t.TempDir(), zap.NewNop())))
	pass("public", f.signed("/pub/p.bin", nil, body, nil, ""), "backend:r2")
	// A backend that is no EdgeWriter: a local driver as the primary.
	tmp := t.TempDir()
	eng2 := engine.NewEngine(nil, zap.NewNop(), nil)
	eng2.AddDriver("local", drivers.NewLocalDriver(tmp, zap.NewNop()))
	eng2.SetPrimary("local")
	saved := f.srv.engine
	f.srv.engine = eng2
	pass("not an edge writer", f.signed("/"+f.bucket+"/l.bin", nil, body, nil, ""), "backend:local")
	f.srv.engine = saved
	// An empty object: nothing to move.
	pass("empty", f.signed("/"+f.bucket+"/e.bin", nil, nil, nil, ""), "empty")
	// An aws-chunked body (aws-cli v2's CRC32 trailer over TLS): the origin's — decoding it would put JavaScript in the byte path.
	pass("aws-chunked", f.signed("/"+f.bucket+"/ch.bin", nil, body, map[string]string{"Content-Encoding": "aws-chunked", "x-amz-decoded-content-length": "900", "x-amz-trailer": "x-amz-checksum-crc32"}, "STREAMING-UNSIGNED-PAYLOAD-TRAILER"), "aws_chunked")
	// A copy: the origin's.
	pass("copy", f.signed("/"+f.bucket+"/cp.bin", nil, body, map[string]string{"x-amz-copy-source": "/b/x"}, ""), "copy")
	assert.Zero(t, f.pending(), "no row from any of these")
	assert.Zero(t, f.used(), "nothing reserved by any of these")
}

func TestEdgeAdmit_ReservesQuotaAndMintsAddress(t *testing.T) {
	f := setupEdgeFixture(t)
	body := testBytes(4096)
	sum := md5.Sum(body) // #nosec G401
	md5b64 := base64.StdEncoding.EncodeToString(sum[:])
	resp := f.admit(f.signed("/"+f.bucket+"/obj.bin", nil, body, map[string]string{"Content-MD5": md5b64, "Content-Type": "image/png",
		"x-amz-meta-origin": "camera"}, ""))
	require.True(t, resp.Edge, "%+v", resp)
	assert.NotEmpty(t, resp.AdmitID)
	assert.True(t, isBlobAddressOf("obj.bin", resp.StorageKey), "storage_key is an address of the key: %s", resp.StorageKey)
	assert.Equal(t, "edgefake", resp.Backend)
	assert.Equal(t, http.MethodPut, resp.Method)
	assert.Equal(t, "4096", resp.SendHeaders["content-length"])
	assert.Equal(t, md5b64, resp.SendHeaders["content-md5"])
	assert.Equal(t, "image/png", resp.SendHeaders["content-type"])
	require.NotNil(t, resp.Expect)
	assert.True(t, resp.Expect.MD5)
	assert.Equal(t, 1, f.pending())
	assert.Equal(t, int64(4096), f.used(), "reserved")
	var state, sk, ct string
	var cl int64
	require.NoError(t, f.db.QueryRow(`SELECT state, storage_key, content_type, content_length FROM edge_admits WHERE id = $1`, resp.AdmitID).Scan(&state, &sk, &ct, &cl))
	assert.Equal(t, "pending", state)
	assert.Equal(t, resp.StorageKey, sk)
	assert.Equal(t, "image/png", ct)
	assert.Equal(t, int64(4096), cl)

	// Past the quota: the S3 XML relayed, nothing pending, nothing held.
	f.exec(`UPDATE tenant_quotas SET storage_limit_bytes = 5000 WHERE tenant_id = $1`, f.tenantID)
	resp2 := f.admit(f.signed("/"+f.bucket+"/over.bin", nil, testBytes(2000), nil, ""))
	require.NotNil(t, resp2.Refusal)
	assert.Equal(t, http.StatusForbidden, resp2.Refusal.Status)
	assert.Contains(t, resp2.Refusal.Body, "QuotaExceeded")
	assert.Equal(t, 1, f.pending())
	assert.Equal(t, int64(4096), f.used())
}

func TestEdgeAdmit_SmallBodiesPassThrough(t *testing.T) {
	// EDGE_MIN_BYTES (the owner's addendum to Prompt 44): below it the
	// origin's path is faster and the bytes do not matter.
	f := setupEdgeFixture(t)
	f.srv.edge.minBytes = 4096
	before := promtestutil.ToFloat64(dataPlaneAdmits.WithLabelValues(edgeHost, edgeOpPut, "pass_through_small"))
	resp := f.admit(f.signed("/"+f.bucket+"/small.bin", nil, testBytes(4095), nil, ""))
	assert.False(t, resp.Edge)
	assert.Nil(t, resp.Refusal)
	assert.Equal(t, edgeReasonSmall, resp.Reason)
	assert.Equal(t, before+1, promtestutil.ToFloat64(dataPlaneAdmits.WithLabelValues(edgeHost, edgeOpPut, "pass_through_small")))
	assert.Zero(t, f.pending())
	assert.Zero(t, f.used())
	resp = f.admit(f.signed("/"+f.bucket+"/exact.bin", nil, testBytes(4096), nil, ""))
	assert.True(t, resp.Edge, "at the threshold the edge takes it: %+v", resp)
	// A part below it is the edge's (D-EDGE-29): EDGE_MIN_BYTES gates
	// PutObject only — a part is never alone, its upload is the unit.
	id := f.createUpload("small.zip")
	resp = f.admit(f.signed("/"+f.bucket+"/small.zip", url.Values{"uploadId": {id}, "partNumber": {"1"}}, testBytes(100), nil, ""))
	assert.True(t, resp.Edge, "a small part is the edge's: %+v", resp)

	logger := zap.NewNop()
	env := func(v string) func(string) string { return func(string) string { return v } }
	assert.Equal(t, int64(4<<20), edgeMinBytesFromEnv(env(""), logger), "default 4 MiB")
	assert.Equal(t, int64(0), edgeMinBytesFromEnv(env("off"), logger))
	assert.Equal(t, int64(256<<10), edgeMinBytesFromEnv(env("256KiB"), logger))
	assert.Equal(t, int64(4<<20), edgeMinBytesFromEnv(env("lots"), logger), "rejected = default")
}

func TestEdgeAdmit_ObjectLockRefused(t *testing.T) {
	f := setupEdgeFixture(t)
	f.exec(`INSERT INTO object_locks (tenant_id, bucket, object_key, retention_mode, retain_until_date) VALUES ($1, $2, 'locked.bin', 'COMPLIANCE', NOW() + interval '1 day')`,
		f.tenantID, f.bucket)
	resp := f.admit(f.signed("/"+f.bucket+"/locked.bin", nil, testBytes(100), nil, ""))
	require.NotNil(t, resp.Refusal)
	assert.Equal(t, http.StatusForbidden, resp.Refusal.Status)
	assert.Contains(t, resp.Refusal.Body, "AccessDenied")
	assert.Zero(t, f.pending())
	assert.Zero(t, f.used())
}

func TestEdgeAdmit_RefusalAfterReservationReleases(t *testing.T) {
	f := setupEdgeFixture(t)
	f.drv.failPresign.Store(true)
	w := f.call("/api/v1/edge/admit", f.signed("/"+f.bucket+"/p.bin", nil, testBytes(3000), nil, ""), f.token)
	assert.Equal(t, http.StatusInternalServerError, w.Code)
	assert.Zero(t, f.pending(), "nothing pending")
	assert.Zero(t, f.used(), "the reservation was given back")
	f.drv.failPresign.Store(false)
}

// --- the commit ---

func TestEdgeCommit_WholeObjectCommitsLikeAPut(t *testing.T) {
	f := setupEdgeFixture(t)
	f.srv.accessLogTracker = NewS3AccessLogTracker(f.db)
	body := testBytes(8192)
	hdr := map[string]string{"Content-Type": "text/plain", "x-amz-meta-origin": "edge-test", "Cache-Control": "max-age=60",
		"Content-Disposition": "attachment; filename=a.txt", "Content-Language": "en"}

	// The control: the same bytes through the origin.
	w := f.originPut("control.bin", body, hdr)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	control, ok := f.head("control.bin")
	require.True(t, ok)

	req := f.signed("/"+f.bucket+"/edge.bin", nil, body, hdr, "")
	resp := f.admit(req)
	require.True(t, resp.Edge, "%+v", resp)
	etag := f.drv.workerPut(f.tenantID, f.container(), resp.StorageKey, body)
	relay, code := f.commit(okCommit(resp.AdmitID, body, etag))
	require.Equal(t, http.StatusOK, code)
	require.Equal(t, http.StatusOK, relay.Status, relay.Body)
	assert.Equal(t, etag, relay.Headers["Etag"])
	assert.Equal(t, "8192", relay.Headers["X-Amz-Object-Size"])
	assert.NotEmpty(t, relay.Headers["X-Amz-Request-Id"])

	got, ok := f.head("edge.bin")
	require.True(t, ok, "head row")
	assert.Equal(t, resp.StorageKey, got.sk)
	got.sk, control.sk = "", ""
	assert.Equal(t, control, got, "the same rows but storage_key")
	assert.Equal(t, "edgefake", got.backend)
	assert.Equal(t, int64(2*8192), f.used(), "bill settled: both objects")
	assert.Equal(t, "committed", f.admitState(resp.AdmitID))
	f.srv.accessLogTracker.Flush()
	var ip string
	var bytesIn int64
	var status int
	require.NoError(t, f.db.QueryRow(`SELECT source_ip, bytes_received, status_code FROM s3_access_log WHERE tenant_id = $1 AND object_key = 'edge.bin' AND operation = 'PutObject'`,
		f.tenantID).Scan(&ip, &bytesIn, &status))
	assert.Equal(t, "203.0.113.9", ip, "the client's IP, not the Worker's")
	assert.Equal(t, int64(8192), bytesIn)
	assert.Equal(t, 200, status)
	var events int
	require.NoError(t, f.db.QueryRow(`SELECT COUNT(*) FROM events WHERE tenant_id = $1 AND type = 'object.created'`, f.tenantID).Scan(&events))
	assert.Equal(t, 2, events, "object.created for both")

	// Versioned: the version row names the address.
	f.exec(`UPDATE buckets SET versioning_status = 'Enabled' WHERE tenant_id = $1 AND name = $2`, f.tenantID, f.bucket)
	resp2 := f.admit(f.signed("/"+f.bucket+"/edge.bin", nil, body, hdr, ""))
	require.True(t, resp2.Edge)
	etag2 := f.drv.workerPut(f.tenantID, f.container(), resp2.StorageKey, body)
	relay2, _ := f.commit(okCommit(resp2.AdmitID, body, etag2))
	require.Equal(t, http.StatusOK, relay2.Status)
	assert.NotEmpty(t, relay2.Headers["X-Amz-Version-Id"])
	var vsk string
	require.NoError(t, f.db.QueryRow(`SELECT storage_key FROM object_versions WHERE tenant_id = $1 AND bucket = $2 AND object_key = 'edge.bin' AND is_latest`,
		f.tenantID, f.bucket).Scan(&vsk))
	assert.Equal(t, resp2.StorageKey, vsk)
}

func TestEdgeCommit_TheBackendsETagIsTheRows(t *testing.T) {
	f := setupEdgeFixture(t)
	body := testBytes(2048)
	// No Content-MD5 signed: the backend's ETag (the body's MD5 on every
	// EdgeWriter backend) is what the row records and the client reads.
	resp := f.admit(f.signed("/"+f.bucket+"/m.bin", nil, body, nil, "UNSIGNED-PAYLOAD"))
	require.True(t, resp.Edge)
	assert.False(t, resp.Expect.MD5)
	etag := f.drv.workerPut(f.tenantID, f.container(), resp.StorageKey, body)
	relay, _ := f.commit(okCommit(resp.AdmitID, body, etag))
	require.Equal(t, http.StatusOK, relay.Status, relay.Body)
	assert.Equal(t, etag, relay.Headers["Etag"])
	got, ok := f.head("m.bin")
	require.True(t, ok)
	assert.Equal(t, strings.Trim(etag, `"`), got.etag)

	// A backend that answers no MD5 ETag: refused, the blob removed, nothing
	// committed under an ETag a read cannot verify.
	resp2 := f.admit(f.signed("/"+f.bucket+"/x.bin", nil, body, nil, ""))
	require.True(t, resp2.Edge)
	f.drv.workerPut(f.tenantID, f.container(), resp2.StorageKey, body)
	relay2, _ := f.commit(okCommit(resp2.AdmitID, body, `"not-an-md5"`))
	assert.Equal(t, http.StatusInternalServerError, relay2.Status)
	assert.False(t, f.drv.has(f.tenantID, f.container(), resp2.StorageKey), "the blob was deleted on the backend")
	_, found := f.head("x.bin")
	assert.False(t, found)
	assert.Equal(t, "failed", f.admitState(resp2.AdmitID))

	// A signed Content-MD5 the backend's ETag does not match (a backend that
	// let other bytes through): deleted and refused.
	sum := md5.Sum(body) // #nosec G401
	resp3 := f.admit(f.signed("/"+f.bucket+"/s.bin", nil, body, map[string]string{"Content-MD5": base64.StdEncoding.EncodeToString(sum[:])}, ""))
	require.True(t, resp3.Edge)
	assert.True(t, resp3.Expect.MD5)
	f.drv.workerPut(f.tenantID, f.container(), resp3.StorageKey, body)
	relay3, _ := f.commit(okCommit(resp3.AdmitID, body, `"00000000000000000000000000000000"`))
	assert.Equal(t, http.StatusBadRequest, relay3.Status)
	assert.Contains(t, relay3.Body, "BadDigest")
	assert.False(t, f.drv.has(f.tenantID, f.container(), resp3.StorageKey))
	assert.Equal(t, int64(2048), f.used(), "only the committed object is billed")
}

func TestEdgeCommit_BackendErrorReleasesAndQueuesOrphan(t *testing.T) {
	f := setupEdgeFixture(t)
	body := testBytes(1024)
	resp := f.admit(f.signed("/"+f.bucket+"/b.bin", nil, body, nil, ""))
	require.True(t, resp.Edge)
	relay, _ := f.commit(edgeCommitRequest{AdmitID: resp.AdmitID, Outcome: edgeOutcomeBackendError, BackendStatus: 503, BackendCode: "SlowDown"})
	assert.Equal(t, http.StatusServiceUnavailable, relay.Status)
	assert.Contains(t, relay.Body, "ServiceUnavailable")
	assert.Equal(t, "30", relay.Headers["Retry-After"])
	assert.Zero(t, f.used(), "released")
	var reason, backend string
	require.NoError(t, f.db.QueryRow(`SELECT reason, backend_id FROM retired_blobs WHERE tenant_id = $1 AND storage_key = $2`, f.tenantID, resp.StorageKey).Scan(&reason, &backend))
	assert.Equal(t, retiredOrphanWrite, reason)
	assert.Equal(t, "edgefake", backend, "queued on THAT backend")

	// A backend 400 BadDigest (the signed Content-MD5 did not match): the client's 400.
	sum := md5.Sum(body) // #nosec G401
	resp2 := f.admit(f.signed("/"+f.bucket+"/d.bin", nil, body, map[string]string{"Content-MD5": base64.StdEncoding.EncodeToString(sum[:])}, ""))
	require.True(t, resp2.Edge)
	relay2, _ := f.commit(edgeCommitRequest{AdmitID: resp2.AdmitID, Outcome: edgeOutcomeBackendError, BackendStatus: 400, BackendCode: "BadDigest"})
	assert.Equal(t, http.StatusBadRequest, relay2.Status)
	assert.Contains(t, relay2.Body, "BadDigest")
	// A short body: the backend's IncompleteBody is the client's.
	resp3 := f.admit(f.signed("/"+f.bucket+"/short.bin", nil, body, nil, ""))
	relay3, _ := f.commit(edgeCommitRequest{AdmitID: resp3.AdmitID, Outcome: edgeOutcomeBackendError, BackendStatus: 400, BackendCode: "IncompleteBody"})
	assert.Equal(t, http.StatusBadRequest, relay3.Status)
	assert.Contains(t, relay3.Body, "IncompleteBody")
	// The subrequest failed before an answer: 503, the address queued.
	resp4 := f.admit(f.signed("/"+f.bucket+"/cut.bin", nil, body, nil, ""))
	relay4, _ := f.commit(edgeCommitRequest{AdmitID: resp4.AdmitID, Outcome: edgeOutcomeEdgeError, BackendCode: "network connection lost"})
	assert.Equal(t, http.StatusServiceUnavailable, relay4.Status)
	assert.Zero(t, f.used())
	assert.Zero(t, f.pending())
}

func TestEdgeCommit_Idempotent(t *testing.T) {
	f := setupEdgeFixture(t)
	body := testBytes(777)
	resp := f.admit(f.signed("/"+f.bucket+"/i.bin", nil, body, nil, ""))
	require.True(t, resp.Edge)
	etag := f.drv.workerPut(f.tenantID, f.container(), resp.StorageKey, body)
	first, _ := f.commit(okCommit(resp.AdmitID, body, etag))
	require.Equal(t, http.StatusOK, first.Status)
	second, _ := f.commit(okCommit(resp.AdmitID, body, etag))
	assert.Equal(t, first, second, "the same answer again")
	var versions int
	require.NoError(t, f.db.QueryRow(`SELECT COUNT(*) FROM object_versions WHERE tenant_id = $1 AND object_key = 'i.bin'`, f.tenantID).Scan(&versions))
	assert.LessOrEqual(t, versions, 1)
	assert.Equal(t, int64(777), f.used(), "billed once")

	_, code := f.commit(okCommit("nope-nope-nope", body, etag))
	assert.Equal(t, http.StatusNotFound, code)

	// Expired: 410, released, the address queued.
	resp2 := f.admit(f.signed("/"+f.bucket+"/x.bin", nil, body, nil, ""))
	f.exec(`UPDATE edge_admits SET expires_at = NOW() - interval '1 minute' WHERE id = $1`, resp2.AdmitID)
	_, code = f.commit(okCommit(resp2.AdmitID, body, etag))
	assert.Equal(t, http.StatusGone, code)
	assert.Equal(t, "expired", f.admitState(resp2.AdmitID))
	assert.Equal(t, int64(777), f.used())
}

func TestEdgeCommit_BucketMadePrivateMeanwhile(t *testing.T) {
	f := setupEdgeFixture(t)
	body := testBytes(512)
	resp := f.admit(f.signed("/"+f.bucket+"/priv.bin", nil, body, nil, ""))
	require.True(t, resp.Edge)
	f.exec(`UPDATE buckets SET ssec_required = TRUE WHERE tenant_id = $1 AND name = $2`, f.tenantID, f.bucket)
	etag := f.drv.workerPut(f.tenantID, f.container(), resp.StorageKey, body)
	relay, _ := f.commit(okCommit(resp.AdmitID, body, etag))
	assert.Equal(t, http.StatusBadRequest, relay.Status)
	assert.Contains(t, relay.Body, ssecRequiredRefusal)
	_, found := f.head("priv.bin")
	assert.False(t, found, "no plaintext row in a Private bucket")
	assert.Zero(t, f.used())
	var queued int
	require.NoError(t, f.db.QueryRow(`SELECT COUNT(*) FROM retired_blobs WHERE tenant_id = $1 AND storage_key = $2`, f.tenantID, resp.StorageKey).Scan(&queued))
	assert.Equal(t, 1, queued, "the blob is an orphan")
}

// --- multipart ---

func (f *edgeFixture) createUpload(key string) string {
	return f.createUploadWith(key, nil)
}

// createUploadWith is createUpload with the client's own headers on the
// Create (the SSE request, an SSE-C key — RP64-2).
func (f *edgeFixture) createUploadWith(key string, hdr map[string]string) string {
	f.t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/"+f.bucket+"/"+key+"?uploads", nil)
	r.Header.Set("Content-Type", "application/zip")
	for k, v := range hdr {
		r.Header.Set(k, v)
	}
	tn := &tenant.Tenant{ID: f.tenantID, Namespace: "tenant/" + f.tenantID + "/"}
	r = r.WithContext(s3Ctx(r.Context(), tn))
	w := httptest.NewRecorder()
	f.srv.testMode = true
	defer func() { f.srv.testMode = false }()
	f.srv.handleS3Request(w, r)
	require.Equal(f.t, http.StatusOK, w.Code, w.Body.String())
	var init InitiateMultipartUploadResult
	require.NoError(f.t, xml.Unmarshal(w.Body.Bytes(), &init))
	f.t.Cleanup(func() { _ = os.RemoveAll(multipartDir(init.UploadID)) })
	return init.UploadID
}

func (f *edgeFixture) s3(method, path string, body []byte, hdr map[string]string) *httptest.ResponseRecorder {
	f.t.Helper()
	r := httptest.NewRequest(method, path, bytes.NewReader(body))
	r.ContentLength = int64(len(body))
	for k, v := range hdr {
		r.Header.Set(k, v)
	}
	tn := &tenant.Tenant{ID: f.tenantID, Namespace: "tenant/" + f.tenantID + "/"}
	r = r.WithContext(s3Ctx(r.Context(), tn))
	w := httptest.NewRecorder()
	f.srv.testMode = true
	defer func() { f.srv.testMode = false }()
	f.srv.handleS3Request(w, r)
	return w
}

func (f *edgeFixture) edgePart(key, uploadID string, n int, body []byte) (edgeAdmitResponse, string) {
	f.t.Helper()
	q := url.Values{"uploadId": {uploadID}, "partNumber": {fmt.Sprint(n)}}
	resp := f.admit(f.signed("/"+f.bucket+"/"+key, q, body, nil, ""))
	require.True(f.t, resp.Edge, "part %d: %+v", n, resp)
	var buid string
	require.NoError(f.t, f.db.QueryRow(`SELECT backend_upload_id FROM multipart_uploads WHERE upload_id = $1`, uploadID).Scan(&buid))
	etag := f.drv.workerPutPart(f.tenantID, f.container(), resp.StorageKey, buid, n, body)
	relay, _ := f.commit(okCommit(resp.AdmitID, body, etag))
	require.Equal(f.t, http.StatusOK, relay.Status, relay.Body)
	return resp, relay.Headers["Etag"]
}

func TestEdgeMultipart_FirstPartCreatesBackendUploadOnce(t *testing.T) {
	f := setupEdgeFixture(t)
	id := f.createUpload("mp.zip")
	body := testBytes(5000)
	reqs := []edgeAdmitRequest{
		f.signed("/"+f.bucket+"/mp.zip", url.Values{"uploadId": {id}, "partNumber": {"1"}}, body, nil, ""),
		f.signed("/"+f.bucket+"/mp.zip", url.Values{"uploadId": {id}, "partNumber": {"2"}}, body, nil, ""),
	}
	var wg sync.WaitGroup
	results := make([]edgeAdmitResponse, 2)
	for i := range reqs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			w := f.call("/api/v1/edge/admit", reqs[i], f.token)
			_ = json.Unmarshal(w.Body.Bytes(), &results[i])
		}(i)
	}
	wg.Wait()
	require.True(t, results[0].Edge && results[1].Edge, "%+v %+v", results[0], results[1])
	assert.Equal(t, int32(1), f.drv.creates.Load(), "one CreateMultipart")
	assert.Equal(t, results[0].StorageKey, results[1].StorageKey, "both parts under the upload's address")
	var edge bool
	var buid, backend string
	require.NoError(t, f.db.QueryRow(`SELECT edge, backend_upload_id, edge_backend FROM multipart_uploads WHERE upload_id = $1`, id).Scan(&edge, &buid, &backend))
	assert.True(t, edge)
	assert.NotEmpty(t, buid)
	assert.Equal(t, "edgefake", backend)
	assert.Equal(t, int64(10000), f.used(), "both parts reserved on the upload")
	var holds int64
	require.NoError(t, f.db.QueryRow(`SELECT reserved_bytes FROM multipart_uploads WHERE upload_id = $1`, id).Scan(&holds))
	assert.Equal(t, int64(10000), holds)

	// A staged part on an edge upload is refused.
	w := f.s3(http.MethodPut, "/"+f.bucket+"/mp.zip?uploadId="+id+"&partNumber=3", body, nil)
	assert.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), "fed through the edge")
	// And an edge part on an upload that holds a staged part passes through.
	id2 := f.createUpload("staged.zip")
	require.Equal(t, http.StatusOK, f.s3(http.MethodPut, "/"+f.bucket+"/staged.zip?uploadId="+id2+"&partNumber=1", body, nil).Code)
	resp := f.admit(f.signed("/"+f.bucket+"/staged.zip", url.Values{"uploadId": {id2}, "partNumber": {"2"}}, body, nil, ""))
	assert.False(t, resp.Edge)
	assert.Equal(t, "staged_parts", resp.Reason)
}

func TestEdgeMultipart_PartCommitRecordsBackendETag(t *testing.T) {
	f := setupEdgeFixture(t)
	id := f.createUpload("p.zip")
	body := testBytes(6000)
	resp, etag := f.edgePart("p.zip", id, 1, body)
	sum := md5.Sum(body) // #nosec G401
	assert.Equal(t, `"`+hex.EncodeToString(sum[:])+`"`, etag)
	var rowETag, backendETag string
	var size int64
	require.NoError(t, f.db.QueryRow(`SELECT etag, backend_etag, size_bytes FROM multipart_parts WHERE upload_id = $1 AND part_number = 1`, id).Scan(&rowETag, &backendETag, &size))
	assert.Equal(t, etag, rowETag)
	assert.Equal(t, etag, backendETag)
	assert.Equal(t, int64(6000), size)
	assert.Equal(t, "committed", f.admitState(resp.AdmitID))
	// The reservation on the upload row equals a staged UploadPart's.
	var holds int64
	require.NoError(t, f.db.QueryRow(`SELECT reserved_bytes FROM multipart_uploads WHERE upload_id = $1`, id).Scan(&holds))
	assert.Equal(t, int64(6000), holds)
	assert.Equal(t, int64(6000), f.used())
	// A re-upload of the part number holds the difference.
	f.edgePart("p.zip", id, 1, testBytes(4000))
	require.NoError(t, f.db.QueryRow(`SELECT reserved_bytes FROM multipart_uploads WHERE upload_id = $1`, id).Scan(&holds))
	assert.Equal(t, int64(4000), holds)
	assert.Equal(t, int64(4000), f.used())
	// ListParts lists it.
	w := f.s3(http.MethodGet, "/"+f.bucket+"/p.zip?uploadId="+id, nil, nil)
	require.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), "<PartNumber>1</PartNumber>")
}

func TestEdgeMultipart_CompleteUsesBackendComplete(t *testing.T) {
	f := setupEdgeFixture(t)
	id := f.createUpload("c.zip")
	p1, p2 := testBytes(5000), testBytes(3000)
	_, e1 := f.edgePart("c.zip", id, 1, p1)
	resp2, e2 := f.edgePart("c.zip", id, 2, p2)
	xmlBody := fmt.Sprintf(`<CompleteMultipartUpload><Part><PartNumber>1</PartNumber><ETag>%s</ETag></Part><Part><PartNumber>2</PartNumber><ETag>%s</ETag></Part></CompleteMultipartUpload>`, e1, e2)
	w := f.s3(http.MethodPost, "/"+f.bucket+"/c.zip?uploadId="+id, []byte(xmlBody), nil)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	h1, h2 := md5.Sum(p1), md5.Sum(p2) // #nosec G401
	hh := md5.New()                    // #nosec G401
	hh.Write(h1[:])
	hh.Write(h2[:])
	wantETag := fmt.Sprintf("%x-2", hh.Sum(nil))
	assert.Equal(t, `"`+wantETag+`"`, w.Header().Get("ETag"))
	got, ok := f.head("c.zip")
	require.True(t, ok)
	assert.Equal(t, wantETag, got.etag)
	assert.Equal(t, int64(8000), got.size)
	assert.Equal(t, resp2.StorageKey, got.sk, "the upload's address")
	assert.Equal(t, "edgefake", got.backend)
	assert.Equal(t, "application/zip", got.ct)
	var bodyMD5 sql.NullString
	require.NoError(t, f.db.QueryRow(`SELECT body_md5 FROM object_head_cache WHERE tenant_id = $1 AND object_key = 'c.zip'`, f.tenantID).Scan(&bodyMD5))
	assert.False(t, bodyMD5.Valid, "body_md5 NULL: assembled off-box")
	assert.True(t, f.drv.has(f.tenantID, f.container(), got.sk), "the backend assembled it at the address")
	assert.Equal(t, int64(8000), f.used(), "the holding became the object's bill")
	var status string
	var ended sql.NullTime
	require.NoError(t, f.db.QueryRow(`SELECT status, backend_upload_ended_at FROM multipart_uploads WHERE upload_id = $1`, id).Scan(&status, &ended))
	assert.Equal(t, "completed", status)
	assert.True(t, ended.Valid)

	// Abort calls the backend abort.
	id2 := f.createUpload("a.zip")
	f.edgePart("a.zip", id2, 1, p1)
	w = f.s3(http.MethodDelete, "/"+f.bucket+"/a.zip?uploadId="+id2, nil, nil)
	require.Equal(t, http.StatusNoContent, w.Code)
	assert.Equal(t, int32(1), f.drv.aborts.Load())
	require.NoError(t, f.db.QueryRow(`SELECT status, backend_upload_ended_at FROM multipart_uploads WHERE upload_id = $1`, id2).Scan(&status, &ended))
	assert.Equal(t, "aborted", status)
	assert.True(t, ended.Valid)
	assert.Equal(t, int64(8000), f.used(), "the aborted part's holding released")

	// The reaper aborts the backend half of an upload the handler could not.
	id3 := f.createUpload("r.zip")
	f.edgePart("r.zip", id3, 1, p1)
	f.exec(`UPDATE multipart_uploads SET status = 'aborted', reserved_bytes = 0 WHERE upload_id = $1`, id3)
	f.exec(`UPDATE tenant_quotas SET storage_used_bytes = 8000 WHERE tenant_id = $1`, f.tenantID)
	res, err := f.srv.multipartReaper.RunOnce(context.Background())
	require.NoError(t, err)
	assert.GreaterOrEqual(t, res.BackendAborted, 1)
	assert.Equal(t, int32(2), f.drv.aborts.Load())
	require.NoError(t, f.db.QueryRow(`SELECT status, backend_upload_ended_at FROM multipart_uploads WHERE upload_id = $1`, id3).Scan(&status, &ended))
	assert.True(t, ended.Valid)
}

// --- the reaper ---

func TestEdgeAdmitReaper_ExpiredPendingReleasesAndRetires(t *testing.T) {
	f := setupEdgeFixture(t)
	body := testBytes(3000)
	resp := f.admit(f.signed("/"+f.bucket+"/reap.bin", nil, body, nil, ""))
	require.True(t, resp.Edge)
	id := f.createUpload("reap.zip")
	partResp := f.admit(f.signed("/"+f.bucket+"/reap.zip", url.Values{"uploadId": {id}, "partNumber": {"1"}}, body, nil, ""))
	require.True(t, partResp.Edge)
	assert.Equal(t, int64(6000), f.used())
	// Not due yet: untouched.
	res, err := f.srv.edgeReaper.RunOnce(context.Background())
	require.NoError(t, err)
	assert.Zero(t, res.Expired)
	assert.Equal(t, 2, f.pending())
	f.exec(`UPDATE edge_admits SET expires_at = NOW() - interval '1 minute' WHERE tenant_id = $1`, f.tenantID)
	res, err = f.srv.edgeReaper.RunOnce(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 2, res.Expired)
	assert.Equal(t, int64(6000), res.ReleasedBytes)
	assert.Equal(t, 1, res.Retired, "the PUT's address")
	assert.Zero(t, f.pending())
	assert.Zero(t, f.used(), "both reservations back")
	var reason string
	require.NoError(t, f.db.QueryRow(`SELECT reason FROM retired_blobs WHERE tenant_id = $1 AND storage_key = $2`, f.tenantID, resp.StorageKey).Scan(&reason))
	assert.Equal(t, retiredOrphanWrite, reason)
	var holds int64
	require.NoError(t, f.db.QueryRow(`SELECT reserved_bytes FROM multipart_uploads WHERE upload_id = $1`, id).Scan(&holds))
	assert.Zero(t, holds)
	// Terminal rows past 7 days are deleted; the job's result shape.
	f.exec(`UPDATE edge_admits SET finished_at = NOW() - interval '8 days' WHERE tenant_id = $1`, f.tenantID)
	res, err = f.srv.edgeReaper.RunOnce(context.Background())
	require.NoError(t, err)
	assert.Equal(t, int64(2), res.Deleted)
	rep, err := f.srv.edgeReaper.spec().Run(context.Background())
	require.NoError(t, err)
	_, isResult := rep.Result.(EdgeAdmitReapResult)
	assert.True(t, isResult, "job_runs.result is the structured count")
}

// --- the presign checker ---

func TestEdgePresignSigns_HostOnlyFails(t *testing.T) {
	ok := "https://b.invalid/x?X-Amz-Algorithm=AWS4-HMAC-SHA256&X-Amz-Signature=ab&X-Amz-SignedHeaders=content-length%3Bcontent-md5%3Bhost"
	require.NoError(t, edgePresignSigns(ok, "content-length", "content-md5"))
	hostOnly := "https://b.invalid/x?X-Amz-Algorithm=AWS4-HMAC-SHA256&X-Amz-Signature=ab&X-Amz-SignedHeaders=host"
	assert.Error(t, edgePresignSigns(hostOnly, "content-length"))
	assert.Error(t, edgePresignSigns("https://b.invalid/x", "content-length"), "not presigned at all")
}

// --- Prompt 46c PR A: aws-chunked at the edge ---

// aws-cli's DEFAULT upload (STREAMING-UNSIGNED-PAYLOAD-TRAILER, no
// Content-Length) is admitted on its DECODED length and answered a
// HEADER-signed backend request (the plain URL + the signed headers) the
// Worker forwards the body under untouched. Where the backend does not
// verify the trailer (Lyve) and the client sent no Content-MD5, the admit
// counts admitted_unverified (D-EDGE-30).
func TestEdgeAdmit_AWSChunkedIsHeaderSigned(t *testing.T) {
	f := setupEdgeFixture(t)
	f.drv.streaming = engine.EdgeStreamingProps{Supported: true, SignatureMaxAge: 15 * time.Minute} // Lyve's
	const n = 5 << 20
	body := testBytes(n)
	unverified := promtestutil.ToFloat64(dataPlaneAdmits.WithLabelValues(edgeHost, edgeOpPut, "admitted_unverified"))
	resp := f.admit(f.signedStreaming("/"+f.bucket+"/chunked.bin", nil, n, "x-amz-checksum-crc64nvme", ""))
	require.True(t, resp.Edge, "%+v", resp)
	assert.Equal(t, "x-amz-checksum-crc64nvme", resp.StreamingTrailer)
	assert.Equal(t, int64(n), resp.ContentLength, "the decoded length")
	assert.NotContains(t, resp.URL, "X-Amz-Signature", "the plain URL: the signature is in the headers")
	assert.Contains(t, resp.URL, "/t-"+f.tenantID+"/"+f.container()+"/chunked.bin@")
	for _, h := range []string{"authorization", "x-amz-date", "x-amz-content-sha256", "content-encoding", "x-amz-decoded-content-length", "x-amz-trailer", "host"} {
		assert.NotEmpty(t, resp.Headers[h], h)
	}
	assert.Empty(t, resp.SendHeaders, "never a presigned link's headers")
	assert.Equal(t, int64(n), f.used(), "reserved on the decoded length")
	assert.Equal(t, unverified+1, promtestutil.ToFloat64(dataPlaneAdmits.WithLabelValues(edgeHost, edgeOpPut, "admitted_unverified")),
		"Lyve does not verify the trailer: the gap is counted (D-EDGE-30)")

	// The commit is a PUT's: the backend stores the DECODED body.
	etag := f.drv.workerPut(f.tenantID, f.container(), resp.StorageKey, body)
	relay, _ := f.commit(okCommit(resp.AdmitID, body, etag))
	require.Equal(t, http.StatusOK, relay.Status, relay.Body)
	h, ok := f.head("chunked.bin")
	require.True(t, ok)
	assert.Equal(t, int64(n), h.size)

	// With a Content-MD5 the COMMIT verifies the backend's ETag (the decoded
	// body's MD5) against it: the write counts verified wherever it goes,
	// and other bytes are BadDigest, removed.
	verified := promtestutil.ToFloat64(dataPlaneAdmits.WithLabelValues(edgeHost, edgeOpPut, "admitted"))
	sum := md5.Sum(body) // #nosec G401
	r2 := f.admit(f.signedStreamingMD5("/"+f.bucket+"/chunked-md5.bin", n, base64.StdEncoding.EncodeToString(sum[:])))
	require.True(t, r2.Edge, "%+v", r2)
	assert.Equal(t, verified+1, promtestutil.ToFloat64(dataPlaneAdmits.WithLabelValues(edgeHost, edgeOpPut, "admitted")))
	other := testBytes(n)
	other[0] ^= 1
	bad := f.drv.workerPut(f.tenantID, f.container(), r2.StorageKey, other)
	relay2, _ := f.commit(okCommit(r2.AdmitID, other, bad))
	assert.Equal(t, http.StatusBadRequest, relay2.Status)
	assert.Contains(t, relay2.Body, "BadDigest")
}

// signedStreamingMD5 is signedStreaming with the client's Content-MD5.
func (f *edgeFixture) signedStreamingMD5(path string, decoded int64, contentMD5 string) edgeAdmitRequest {
	f.t.Helper()
	u := "https://" + f.host + path
	r, err := http.NewRequest(http.MethodPut, u, nil)
	require.NoError(f.t, err)
	r.Header.Set("Content-Encoding", "aws-chunked")
	r.Header.Set("X-Amz-Decoded-Content-Length", fmt.Sprint(decoded))
	r.Header.Set("X-Amz-Trailer", "x-amz-checksum-crc32")
	r.Header.Set("Content-MD5", contentMD5)
	r.Header.Set("X-Amz-Content-Sha256", "STREAMING-UNSIGNED-PAYLOAD-TRAILER")
	require.NoError(f.t, v4.NewSigner().SignHTTP(context.Background(), aws.Credentials{AccessKeyID: f.ak, SecretAccessKey: f.sk},
		r, "STREAMING-UNSIGNED-PAYLOAD-TRAILER", "s3", "us-east-1", time.Now()))
	headers := map[string]string{"host": f.host}
	for k, v := range r.Header {
		headers[strings.ToLower(k)] = strings.Join(v, ", ")
	}
	return edgeAdmitRequest{Op: edgeOpPut, Method: http.MethodPut, Host: f.host, Path: r.URL.EscapedPath(),
		Headers: headers, ClientIP: "203.0.113.9", RequestID: "req-" + uuid.New().String()[:8]}
}

// D-EDGE-33: iDrive enforces no clock window on a header signature (Step
// 0.3) — a minted one would be a bearer for the key until the key rotates.
// An aws-chunked UploadPart is admitted there (a replay dies with the
// upload: Complete names every part by the ETag its commit recorded, and
// after Complete or Abort the upload id is gone) and counts admitted (iDrive
// verifies the trailer); an aws-chunked PutObject bound for it stays the
// origin's. The shapes the edge never takes stay the origin's too.
func TestEdgeAdmit_AWSChunkedOnAnUnboundedBackend(t *testing.T) {
	f := setupEdgeFixture(t)
	f.drv.streaming = engine.EdgeStreamingProps{Supported: true, VerifiesTrailer: true} // iDrive's
	const n = 5 << 20
	put := f.admit(f.signedStreaming("/"+f.bucket+"/put.bin", nil, n, "x-amz-checksum-crc64nvme", ""))
	assert.False(t, put.Edge)
	assert.Equal(t, "aws_chunked_unbounded", put.Reason)
	assert.Zero(t, f.used(), "nothing reserved")

	id := f.createUpload("parts.zip")
	admitted := promtestutil.ToFloat64(dataPlaneAdmits.WithLabelValues(edgeHost, edgeOpPart, "admitted"))
	q := url.Values{"uploadId": {id}, "partNumber": {"1"}}
	part := f.admit(f.signedStreaming("/"+f.bucket+"/parts.zip", q, n, "x-amz-checksum-crc64nvme", ""))
	require.True(t, part.Edge, "%+v", part)
	assert.Equal(t, admitted+1, promtestutil.ToFloat64(dataPlaneAdmits.WithLabelValues(edgeHost, edgeOpPart, "admitted")), "iDrive verifies the trailer")
	pu, err := url.Parse(part.URL)
	require.NoError(t, err)
	assert.Equal(t, "1", pu.Query().Get("partNumber"))
	var buid string
	require.NoError(t, f.db.QueryRow(`SELECT backend_upload_id FROM multipart_uploads WHERE upload_id = $1`, id).Scan(&buid))
	assert.Equal(t, buid, pu.Query().Get("uploadId"), "signed for the backend's own upload")
	body := testBytes(n)
	etag := f.drv.workerPutPart(f.tenantID, f.container(), part.StorageKey, buid, 1, body)
	relay, _ := f.commit(okCommit(part.AdmitID, body, etag))
	require.Equal(t, http.StatusOK, relay.Status, relay.Body)

	// The shapes the edge never forwards: a trailer it does not verify,
	// signed chunks (the client's key in every chunk signature), a backend
	// with no measured streaming behaviour.
	pass := func(why string, req edgeAdmitRequest, reason string) {
		t.Helper()
		r := f.admit(req)
		assert.False(t, r.Edge, why)
		assert.Equal(t, reason, r.Reason, why)
	}
	pass("a sha256 trailer", f.signedStreaming("/"+f.bucket+"/sha.bin", url.Values{"uploadId": {id}, "partNumber": {"2"}}, n, "x-amz-checksum-sha256", ""), "aws_chunked_trailer")
	pass("signed chunks", f.signedStreaming("/"+f.bucket+"/sig.bin", url.Values{"uploadId": {id}, "partNumber": {"3"}}, n, "x-amz-checksum-crc32", "STREAMING-AWS4-HMAC-SHA256-PAYLOAD"), "aws_chunked")
	f.drv.streaming = engine.EdgeStreamingProps{}
	pass("an unmeasured backend", f.signedStreaming("/"+f.bucket+"/parts.zip", url.Values{"uploadId": {id}, "partNumber": {"4"}}, n, "x-amz-checksum-crc32", ""), "aws_chunked_backend")
}
