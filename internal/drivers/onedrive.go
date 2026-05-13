// internal/drivers/onedrive.go
//
// OneDrive fleet storage driver using Microsoft Graph API.
// Ported from cmd/permafrost-v3/main.go (production-quality OneDrive
// implementation) with these key optimizations:
//
//   - Raw HTTP + azidentity (no Graph SDK — binary -77% smaller)
//   - Dual transport: HTTP/2 for Graph API, HTTP/1.1 for CDN downloads
//     (Go HTTP/2 flow-control bugs #54330, #47840, #63520)
//   - Multi-tenant fleet: round-robin with throttle tracking
//   - 60MB upload chunks (Microsoft's max, 35% faster than 10MB)
//   - 4MB read/write buffers
//   - Decorrelated jitter backoff on 429/503
package drivers

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/FairForge/vaultaire/internal/common"
	"github.com/FairForge/vaultaire/internal/engine"
	"go.uber.org/zap"
	"golang.org/x/net/http2"

	mrand "math/rand/v2"
)

const (
	graphBase   = "https://graph.microsoft.com/v1.0"
	odUserAgent = "ISV|FairForge|Vaultaire/1.0"

	simpleUploadMax = 4 << 20  // 4 MB — above this, use upload session
	odChunkSize     = 60 << 20 // 60 MB — Microsoft's max per chunk PUT
	odChunkAlign    = 320 * 1024

	odRootFolder = "vaultaire"
)

// Fleet-wide TLS session cache: enables 0-RTT resumption across tenants.
// Shared so a CDN edge seen by one tenant can be resumed by the next.
var odTLSSessionCache = tls.NewLRUClientSessionCache(256)

// Fleet-wide DNS cache to avoid repeated lookups on reconnection.
var odDNSCache sync.Map

// Pooled 1MB buffers for draining response bodies efficiently.
// io.Discard implements ReaderFrom with 8KB internal buffers, which
// bypasses any buffer passed to CopyBuffer. This sink forces CopyBuffer
// to use our pooled buffer.
type odDevNull struct{}

func (odDevNull) Write(p []byte) (int, error) { return len(p), nil }

var odDrain odDevNull
var odCopyPool = sync.Pool{New: func() any { b := make([]byte, 1<<20); return &b }}

func odPooledDrain(src io.Reader) {
	bp := odCopyPool.Get().(*[]byte)
	_, _ = io.CopyBuffer(odDrain, src, *bp)
	odCopyPool.Put(bp)
}

// OneDriveDriver implements engine.Driver for a fleet of OneDrive tenants.
type OneDriveDriver struct {
	tenants []*odTenant
	idx     atomic.Uint64
	logger  *zap.Logger
}

type odTenant struct {
	name     string
	tenantID string
	clientID string
	secret   string
	userUPN  string

	cred       azcore.TokenCredential
	graphHTTP  *http.Client
	cdnHTTP    *http.Client
	uploadHTTP *http.Client

	mu           sync.Mutex
	driveID      string
	rootFolderID string

	// Token cache with mutex to prevent concurrent refresh storms.
	// Azure throttles parallel token requests from the same client.
	tokenMu   sync.Mutex
	cachedTok string
	tokExpiry time.Time

	throttledUntil atomic.Int64
	totalReqs      atomic.Int64
	throttleCount  atomic.Int64

	// RateLimit tracking from Microsoft Graph response headers.
	rateLimitRemaining atomic.Int64
	rateLimitReset     atomic.Int64
}

// NewOneDriveFleetDriver creates a multi-tenant OneDrive driver.
// Reads TENANT_N_ID, TENANT_N_CLIENT_ID, TENANT_N_SECRET, TENANT_N_USER
// environment variables for N=1..15.
func NewOneDriveFleetDriver(logger *zap.Logger) (*OneDriveDriver, error) {
	var tenants []*odTenant
	for i := 1; i <= 15; i++ {
		p := fmt.Sprintf("TENANT_%d_", i)
		tid := os.Getenv(p + "ID")
		if tid == "" {
			continue
		}
		clientID := os.Getenv(p + "CLIENT_ID")
		secret := os.Getenv(p + "SECRET")
		userUPN := os.Getenv(p + "USER")
		if clientID == "" || secret == "" || userUPN == "" {
			logger.Warn("incomplete OneDrive tenant config, skipping",
				zap.Int("tenant", i))
			continue
		}

		cred, err := azidentity.NewClientSecretCredential(tid, clientID, secret, nil)
		if err != nil {
			logger.Warn("failed to create OneDrive credential",
				zap.Int("tenant", i), zap.Error(err))
			continue
		}

		dial := odCachedDialContext()

		t := &odTenant{
			name:       fmt.Sprintf("tenant-%d", i),
			tenantID:   tid,
			clientID:   clientID,
			secret:     secret,
			userUPN:    userUPN,
			cred:       cred,
			graphHTTP:  &http.Client{Transport: odGraphTransport(dial, odTLSSessionCache)},
			cdnHTTP:    &http.Client{Transport: odCDNTransport(dial, odTLSSessionCache)},
			uploadHTTP: &http.Client{Transport: odUploadTransport(dial, odTLSSessionCache)},
		}
		tenants = append(tenants, t)
	}

	if len(tenants) == 0 {
		return nil, fmt.Errorf("onedrive: no valid tenant configurations found")
	}

	d := &OneDriveDriver{
		tenants: tenants,
		logger:  logger,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for _, t := range tenants {
		driveID, err := t.getDriveID(ctx)
		if err != nil {
			logger.Warn("OneDrive tenant init failed — will retry on first use",
				zap.String("tenant", t.name), zap.Error(err))
			continue
		}
		if _, err := t.ensureRootFolder(ctx, driveID); err != nil {
			logger.Warn("OneDrive root folder creation failed",
				zap.String("tenant", t.name), zap.Error(err))
		}
	}

	logger.Info("OneDrive fleet initialized", zap.Int("tenants", len(tenants)))
	return d, nil
}

func (d *OneDriveDriver) Name() string { return "onedrive" }

func (d *OneDriveDriver) TenantCount() int { return len(d.tenants) }

func (d *OneDriveDriver) pickTenant() *odTenant {
	now := time.Now().Unix()
	n := len(d.tenants)
	start := int(d.idx.Add(1) - 1)
	for i := 0; i < n; i++ {
		t := d.tenants[(start+i)%n]
		if t.throttledUntil.Load() <= now {
			return t
		}
	}
	return d.tenants[start%n]
}

func (d *OneDriveDriver) buildPath(ctx context.Context, container, artifact string) string {
	tenantID := "default"
	if tid, ok := ctx.Value(common.TenantIDKey).(string); ok && tid != "" {
		tenantID = tid
	}
	return fmt.Sprintf("t-%s/%s/%s", tenantID, container, artifact)
}

// Put stores an artifact via OneDrive Graph API.
// Files <4MB use simple PUT; larger files use chunked upload sessions.
func (d *OneDriveDriver) Put(ctx context.Context, container, artifact string, data io.Reader, opts ...engine.PutOption) error {
	t := d.pickTenant()
	driveID, err := t.getDriveID(ctx)
	if err != nil {
		return fmt.Errorf("onedrive get drive: %w", err)
	}
	rootID, err := t.ensureRootFolder(ctx, driveID)
	if err != nil {
		return fmt.Errorf("onedrive ensure folder: %w", err)
	}

	path := d.buildPath(ctx, container, artifact)
	options := engine.ApplyPutOptions(opts...)

	// When ContentLength is known (from S3 API adapter), stream directly
	// without buffering the entire body. This eliminates the double-copy
	// that materialize() + io.ReadAll() would cause.
	if options.ContentLength > 0 {
		d.logger.Debug("onedrive put (streaming)",
			zap.String("tenant", t.name),
			zap.String("path", path),
			zap.Int64("size", options.ContentLength),
		)
		if options.ContentLength <= simpleUploadMax {
			buf, err := io.ReadAll(data)
			if err != nil {
				return fmt.Errorf("onedrive read body: %w", err)
			}
			return t.simpleUpload(ctx, driveID, rootID, path, buf)
		}
		return t.streamingChunkedUpload(ctx, driveID, rootID, path, data, options.ContentLength)
	}

	// Fallback: materialize to determine size
	body, size, cleanup, err := materialize(data)
	if err != nil {
		return fmt.Errorf("onedrive buffer: %w", err)
	}
	defer cleanup()

	d.logger.Debug("onedrive put (materialized)",
		zap.String("tenant", t.name),
		zap.String("path", path),
		zap.Int64("size", size),
	)

	if size <= simpleUploadMax {
		buf, err := io.ReadAll(body)
		if err != nil {
			return fmt.Errorf("onedrive read body: %w", err)
		}
		return t.simpleUpload(ctx, driveID, rootID, path, buf)
	}

	buf, err := io.ReadAll(body)
	if err != nil {
		return fmt.Errorf("onedrive read body: %w", err)
	}
	return t.chunkedUpload(ctx, driveID, rootID, path, buf)
}

// Get retrieves an artifact from OneDrive.
// Fetches the pre-authenticated CDN URL from item metadata, then downloads
// via HTTP/1.1 (bypassing Go's HTTP/2 flow-control bugs).
func (d *OneDriveDriver) Get(ctx context.Context, container, artifact string) (io.ReadCloser, error) {
	t := d.pickTenant()
	driveID, err := t.getDriveID(ctx)
	if err != nil {
		return nil, fmt.Errorf("onedrive get drive: %w", err)
	}

	path := d.buildPath(ctx, container, artifact)
	dlURL, fileSize, err := t.getDownloadURL(ctx, driveID, path)
	if err != nil {
		return nil, fmt.Errorf("onedrive get metadata %s: %w", path, err)
	}

	streams := odAdaptiveStreams(fileSize)
	if streams > 1 {
		return t.downloadRanges(ctx, dlURL, fileSize, streams)
	}

	req, err := http.NewRequestWithContext(ctx, "GET", dlURL, nil)
	if err != nil {
		return nil, fmt.Errorf("onedrive create download request: %w", err)
	}
	resp, err := t.cdnHTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("onedrive download %s: %w", path, err)
	}
	if resp.StatusCode != http.StatusOK {
		odPooledDrain(resp.Body)
		_ = resp.Body.Close()
		return nil, fmt.Errorf("onedrive download %s: HTTP %d", path, resp.StatusCode)
	}
	return resp.Body, nil
}

// odAdaptiveStreams picks optimal parallel stream count based on file size.
// Benchmarked in permafrost-v3: 8 streams is 32% faster than 4 at 100MB.
func odAdaptiveStreams(fileSize int64) int {
	switch {
	case fileSize < 10<<20:
		return 1
	case fileSize < 25<<20:
		return 2
	case fileSize < 50<<20:
		return 4
	default:
		return 8
	}
}

// downloadRanges splits a file into N parallel byte-range downloads via
// HTTP/1.1 CDN transport, then pipes them out in order. Each range downloads
// concurrently; the pipe writer emits ranges sequentially so the caller
// receives a correctly ordered stream.
func (t *odTenant) downloadRanges(ctx context.Context, dlURL string, fileSize int64, streams int) (io.ReadCloser, error) {
	pr, pw := io.Pipe()
	rangeSize := fileSize / int64(streams)

	type rangeResult struct {
		data []byte
		err  error
	}
	results := make([]chan rangeResult, streams)
	for i := range results {
		results[i] = make(chan rangeResult, 1)
	}

	for i := 0; i < streams; i++ {
		start := int64(i) * rangeSize
		end := start + rangeSize - 1
		if i == streams-1 {
			end = fileSize - 1
		}
		go func(idx int, s, e int64) {
			req, _ := http.NewRequestWithContext(ctx, "GET", dlURL, nil)
			req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", s, e))
			resp, err := t.cdnHTTP.Do(req)
			if err != nil {
				results[idx] <- rangeResult{err: err}
				return
			}
			defer func() { _ = resp.Body.Close() }()
			if resp.StatusCode != 206 && resp.StatusCode != 200 {
				odPooledDrain(resp.Body)
				results[idx] <- rangeResult{err: fmt.Errorf("range HTTP %d", resp.StatusCode)}
				return
			}
			data, err := io.ReadAll(resp.Body)
			results[idx] <- rangeResult{data: data, err: err}
		}(i, start, end)
	}

	go func() {
		defer func() { _ = pw.Close() }()
		for _, ch := range results {
			r := <-ch
			if r.err != nil {
				_ = pw.CloseWithError(r.err)
				return
			}
			if _, err := pw.Write(r.data); err != nil {
				return
			}
		}
	}()

	return pr, nil
}

// Delete removes an artifact from OneDrive.
func (d *OneDriveDriver) Delete(ctx context.Context, container, artifact string) error {
	t := d.pickTenant()
	driveID, err := t.getDriveID(ctx)
	if err != nil {
		return fmt.Errorf("onedrive get drive: %w", err)
	}

	path := d.buildPath(ctx, container, artifact)
	itemID, err := t.getItemID(ctx, driveID, path)
	if err != nil {
		return fmt.Errorf("onedrive resolve %s: %w", path, err)
	}

	u := fmt.Sprintf("%s/drives/%s/items/%s", graphBase, driveID, itemID)
	req, _ := http.NewRequestWithContext(ctx, "DELETE", u, nil)
	resp, err := t.graphDo(ctx, req)
	if err != nil {
		return fmt.Errorf("onedrive delete %s: %w", path, err)
	}
	odPooledDrain(resp.Body)
	_ = resp.Body.Close()
	return nil
}

// List returns artifacts under a container with optional prefix filtering.
func (d *OneDriveDriver) List(ctx context.Context, container string, prefix string) ([]string, error) {
	t := d.pickTenant()
	driveID, err := t.getDriveID(ctx)
	if err != nil {
		return nil, fmt.Errorf("onedrive get drive: %w", err)
	}

	tenantID := "default"
	if tid, ok := ctx.Value(common.TenantIDKey).(string); ok && tid != "" {
		tenantID = tid
	}
	folderPath := fmt.Sprintf("%s/t-%s/%s", odRootFolder, tenantID, container)

	u := fmt.Sprintf("%s/drives/%s/items/root:/%s:/children",
		graphBase, driveID, odEscapePath(folderPath))
	req, _ := http.NewRequestWithContext(ctx, "GET", u, nil)
	resp, err := t.graphDo(ctx, req)
	if err != nil {
		if strings.Contains(err.Error(), "404") {
			return nil, nil
		}
		return nil, fmt.Errorf("onedrive list %s: %w", folderPath, err)
	}
	defer func() { _ = resp.Body.Close() }()

	var result struct {
		Value []struct {
			Name string `json:"name"`
		} `json:"value"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("onedrive list decode: %w", err)
	}

	var artifacts []string
	for _, item := range result.Value {
		if prefix == "" || strings.HasPrefix(item.Name, prefix) {
			artifacts = append(artifacts, item.Name)
		}
	}
	return artifacts, nil
}

// Exists checks if an artifact exists in OneDrive.
func (d *OneDriveDriver) Exists(ctx context.Context, container, artifact string) (bool, error) {
	t := d.pickTenant()
	driveID, err := t.getDriveID(ctx)
	if err != nil {
		return false, fmt.Errorf("onedrive get drive: %w", err)
	}

	path := d.buildPath(ctx, container, artifact)
	u := fmt.Sprintf("%s/drives/%s/items/root:/%s/%s",
		graphBase, driveID, odRootFolder, odEscapePath(path))
	req, _ := http.NewRequestWithContext(ctx, "GET", u, nil)
	resp, err := t.graphDo(ctx, req)
	if err != nil {
		if strings.Contains(err.Error(), "404") {
			return false, nil
		}
		return false, fmt.Errorf("onedrive exists %s: %w", path, err)
	}
	odPooledDrain(resp.Body)
	_ = resp.Body.Close()
	return true, nil
}

// HealthCheck validates connectivity by querying the drive metadata.
func (d *OneDriveDriver) HealthCheck(ctx context.Context) error {
	t := d.pickTenant()
	driveID, err := t.getDriveID(ctx)
	if err != nil {
		return fmt.Errorf("onedrive health: %w", err)
	}

	u := fmt.Sprintf("%s/drives/%s", graphBase, driveID)
	req, _ := http.NewRequestWithContext(ctx, "GET", u, nil)
	resp, err := t.graphDo(ctx, req)
	if err != nil {
		return fmt.Errorf("onedrive health: %w", err)
	}
	odPooledDrain(resp.Body)
	_ = resp.Body.Close()
	return nil
}

// ── Tenant methods ──────────────────────────────────────────────────────────

func (t *odTenant) token(ctx context.Context) (string, error) {
	t.tokenMu.Lock()
	defer t.tokenMu.Unlock()

	if t.cachedTok != "" && time.Now().Before(t.tokExpiry) {
		return t.cachedTok, nil
	}

	tok, err := t.cred.GetToken(ctx, policy.TokenRequestOptions{
		Scopes: []string{"https://graph.microsoft.com/.default"},
	})
	if err != nil {
		return "", fmt.Errorf("onedrive token [%s]: %w", t.name, err)
	}
	t.cachedTok = tok.Token
	t.tokExpiry = tok.ExpiresOn.Add(-5 * time.Minute)
	return tok.Token, nil
}

func (t *odTenant) graphDo(ctx context.Context, req *http.Request) (*http.Response, error) {
	const maxAttempts = 5
	base := 500 * time.Millisecond
	prev := base

	token, err := t.token(ctx)
	if err != nil {
		return nil, err
	}

	for attempt := 0; attempt < maxAttempts; attempt++ {
		if attempt > 0 && req.GetBody != nil {
			body, _ := req.GetBody()
			req.Body = body
		}
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("User-Agent", odUserAgent)

		resp, err := t.graphHTTP.Do(req)
		t.totalReqs.Add(1)
		if err != nil {
			if attempt < maxAttempts-1 {
				prev = odDecorrelatedJitter(base, prev, 30*time.Second)
				time.Sleep(prev)
				continue
			}
			return nil, err
		}
		// Track RateLimit headers for throttle visibility
		if remaining := resp.Header.Get("RateLimit-Remaining"); remaining != "" {
			if n, err := strconv.ParseInt(remaining, 10, 64); err == nil {
				t.rateLimitRemaining.Store(n)
			}
		}
		if reset := resp.Header.Get("RateLimit-Reset"); reset != "" {
			if n, err := strconv.ParseInt(reset, 10, 64); err == nil {
				t.rateLimitReset.Store(time.Now().Add(time.Duration(n) * time.Second).Unix())
			}
		}

		if resp.StatusCode == 429 || resp.StatusCode == 503 {
			t.throttleCount.Add(1)
			retryAfter, _ := strconv.Atoi(resp.Header.Get("Retry-After"))
			wait := odDecorrelatedJitter(base, prev, 30*time.Second)
			if ra := time.Duration(retryAfter) * time.Second; ra > wait {
				wait = ra
			}
			t.throttledUntil.Store(time.Now().Add(wait).Unix())
			prev = wait
			odPooledDrain(resp.Body)
			_ = resp.Body.Close()
			if attempt < maxAttempts-1 {
				time.Sleep(wait)
				continue
			}
			return nil, fmt.Errorf("onedrive throttled [%s] (%d)", t.name, resp.StatusCode)
		}
		if resp.StatusCode >= 500 {
			odPooledDrain(resp.Body)
			_ = resp.Body.Close()
			if attempt < maxAttempts-1 {
				prev = odDecorrelatedJitter(base, prev, 30*time.Second)
				time.Sleep(prev)
				continue
			}
			return nil, fmt.Errorf("onedrive [%s] HTTP %d", t.name, resp.StatusCode)
		}
		if resp.StatusCode >= 400 {
			b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
			_ = resp.Body.Close()
			return nil, fmt.Errorf("onedrive [%s] HTTP %d: %s", t.name, resp.StatusCode, b)
		}
		return resp, nil
	}
	return nil, fmt.Errorf("onedrive [%s] exhausted retries", t.name)
}

func (t *odTenant) getDriveID(ctx context.Context) (string, error) {
	t.mu.Lock()
	if t.driveID != "" {
		d := t.driveID
		t.mu.Unlock()
		return d, nil
	}
	t.mu.Unlock()

	u := fmt.Sprintf("%s/users/%s/drives", graphBase, url.PathEscape(t.userUPN))
	req, _ := http.NewRequestWithContext(ctx, "GET", u, nil)
	resp, err := t.graphDo(ctx, req)
	if err != nil {
		return "", fmt.Errorf("get drives [%s]: %w", t.name, err)
	}
	defer func() { _ = resp.Body.Close() }()

	var out struct {
		Value []struct {
			ID string `json:"id"`
		} `json:"value"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", fmt.Errorf("decode drives [%s]: %w", t.name, err)
	}
	if len(out.Value) == 0 {
		return "", fmt.Errorf("no drives for %s", t.userUPN)
	}

	t.mu.Lock()
	t.driveID = out.Value[0].ID
	t.mu.Unlock()
	return t.driveID, nil
}

func (t *odTenant) ensureRootFolder(ctx context.Context, driveID string) (string, error) {
	t.mu.Lock()
	if t.rootFolderID != "" {
		id := t.rootFolderID
		t.mu.Unlock()
		return id, nil
	}
	t.mu.Unlock()

	u := fmt.Sprintf("%s/drives/%s/items/root/children", graphBase, driveID)
	body, _ := json.Marshal(map[string]any{
		"name": odRootFolder, "folder": map[string]any{},
		"@microsoft.graph.conflictBehavior": "fail",
	})
	req, _ := http.NewRequestWithContext(ctx, "POST", u, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(body)), nil }

	resp, err := t.graphDo(ctx, req)
	if err != nil {
		// 409 = folder already exists — fetch its ID instead
		if strings.Contains(err.Error(), "409") || strings.Contains(err.Error(), "nameAlreadyExists") {
			return t.getFolderID(ctx, driveID, odRootFolder)
		}
		return "", fmt.Errorf("create root folder [%s]: %w", t.name, err)
	}
	defer func() { _ = resp.Body.Close() }()

	var out struct {
		ID string `json:"id"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&out)

	t.mu.Lock()
	t.rootFolderID = out.ID
	t.mu.Unlock()
	return out.ID, nil
}

func (t *odTenant) getFolderID(ctx context.Context, driveID, name string) (string, error) {
	u := fmt.Sprintf("%s/drives/%s/items/root:/%s", graphBase, driveID, url.PathEscape(name))
	req, _ := http.NewRequestWithContext(ctx, "GET", u, nil)
	resp, err := t.graphDo(ctx, req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()

	var out struct {
		ID string `json:"id"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&out)

	t.mu.Lock()
	t.rootFolderID = out.ID
	t.mu.Unlock()
	return out.ID, nil
}

func (t *odTenant) getItemID(ctx context.Context, driveID, path string) (string, error) {
	u := fmt.Sprintf("%s/drives/%s/items/root:/%s/%s",
		graphBase, driveID, odRootFolder, odEscapePath(path))
	req, _ := http.NewRequestWithContext(ctx, "GET", u, nil)
	resp, err := t.graphDo(ctx, req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()

	var out struct {
		ID string `json:"id"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return out.ID, nil
}

func (t *odTenant) getDownloadURL(ctx context.Context, driveID, path string) (string, int64, error) {
	u := fmt.Sprintf("%s/drives/%s/items/root:/%s/%s",
		graphBase, driveID, odRootFolder, odEscapePath(path))
	req, _ := http.NewRequestWithContext(ctx, "GET", u, nil)
	resp, err := t.graphDo(ctx, req)
	if err != nil {
		return "", 0, err
	}
	defer func() { _ = resp.Body.Close() }()

	var meta struct {
		DownloadURL string `json:"@microsoft.graph.downloadUrl"`
		Size        int64  `json:"size"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&meta)
	if meta.DownloadURL == "" {
		return "", 0, fmt.Errorf("no downloadUrl for %s", path)
	}
	return meta.DownloadURL, meta.Size, nil
}

// ── Upload ──────────────────────────────────────────────────────────────────

func (t *odTenant) simpleUpload(ctx context.Context, driveID, parentID, path string, data []byte) error {
	u := fmt.Sprintf("%s/drives/%s/items/%s:/%s:/content",
		graphBase, driveID, parentID, odEscapePath(path))
	req, _ := http.NewRequestWithContext(ctx, "PUT", u, bytes.NewReader(data))
	req.ContentLength = int64(len(data))
	req.Header.Set("Content-Type", "application/octet-stream")
	req.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(data)), nil }
	resp, err := t.graphDo(ctx, req)
	if err != nil {
		return fmt.Errorf("onedrive simple upload: %w", err)
	}
	odPooledDrain(resp.Body)
	_ = resp.Body.Close()
	return nil
}

func (t *odTenant) chunkedUpload(ctx context.Context, driveID, parentID, path string, data []byte) error {
	sessionURL := fmt.Sprintf("%s/drives/%s/items/%s:/%s:/createUploadSession",
		graphBase, driveID, parentID, odEscapePath(path))
	sb, _ := json.Marshal(map[string]any{
		"item": map[string]any{"@microsoft.graph.conflictBehavior": "replace"},
	})
	req, _ := http.NewRequestWithContext(ctx, "POST", sessionURL, bytes.NewReader(sb))
	req.Header.Set("Content-Type", "application/json")
	req.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(sb)), nil }
	resp, err := t.graphDo(ctx, req)
	if err != nil {
		return fmt.Errorf("onedrive create upload session: %w", err)
	}
	var session struct {
		UploadURL string `json:"uploadUrl"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&session)
	_ = resp.Body.Close()

	if session.UploadURL == "" {
		return fmt.Errorf("onedrive: empty upload session URL")
	}

	total := len(data)
	for offset := 0; offset < total; offset += odChunkSize {
		end := offset + odChunkSize
		if end > total {
			end = total
		}
		chunk := data[offset:end]
		if err := t.putChunk(ctx, session.UploadURL, chunk, offset, end-1, total); err != nil {
			return fmt.Errorf("onedrive chunk at offset %d: %w", offset, err)
		}
	}
	return nil
}

func (t *odTenant) putChunk(ctx context.Context, uploadURL string, chunk []byte, start, endInc, total int) error {
	for attempt := 0; attempt < 3; attempt++ {
		req, _ := http.NewRequestWithContext(ctx, "PUT", uploadURL, bytes.NewReader(chunk))
		req.ContentLength = int64(len(chunk))
		req.Header.Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, endInc, total))
		// No Authorization header on chunk PUTs — the upload URL is pre-authenticated
		resp, err := t.uploadHTTP.Do(req)
		t.totalReqs.Add(1)
		if err != nil {
			time.Sleep(time.Duration(1<<uint(attempt)) * time.Second)
			continue
		}
		odPooledDrain(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			return nil
		}
		if resp.StatusCode == 429 || resp.StatusCode >= 500 {
			time.Sleep(time.Duration(1<<uint(attempt)) * time.Second)
			continue
		}
		return fmt.Errorf("onedrive chunk HTTP %d", resp.StatusCode)
	}
	return fmt.Errorf("onedrive chunk exhausted retries")
}

// streamingChunkedUpload reads chunks directly from the io.Reader without
// buffering the entire body. Requires totalSize from ContentLength.
// Eliminates the materialize() + io.ReadAll() double-copy.
func (t *odTenant) streamingChunkedUpload(ctx context.Context, driveID, parentID, path string, data io.Reader, totalSize int64) error {
	sessionURL := fmt.Sprintf("%s/drives/%s/items/%s:/%s:/createUploadSession",
		graphBase, driveID, parentID, odEscapePath(path))
	sb, _ := json.Marshal(map[string]any{
		"item": map[string]any{"@microsoft.graph.conflictBehavior": "replace"},
	})
	req, _ := http.NewRequestWithContext(ctx, "POST", sessionURL, bytes.NewReader(sb))
	req.Header.Set("Content-Type", "application/json")
	req.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(sb)), nil }
	resp, err := t.graphDo(ctx, req)
	if err != nil {
		return fmt.Errorf("onedrive create upload session: %w", err)
	}
	var session struct {
		UploadURL string `json:"uploadUrl"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&session)
	_ = resp.Body.Close()

	if session.UploadURL == "" {
		return fmt.Errorf("onedrive: empty upload session URL")
	}

	buf := make([]byte, odChunkSize)
	offset := int64(0)
	for offset < totalSize {
		chunkEnd := offset + int64(odChunkSize)
		if chunkEnd > totalSize {
			chunkEnd = totalSize
		}
		toRead := chunkEnd - offset

		n, err := io.ReadFull(data, buf[:toRead])
		if err != nil && err != io.ErrUnexpectedEOF {
			return fmt.Errorf("onedrive read chunk at offset %d: %w", offset, err)
		}

		if err := t.putChunk(ctx, session.UploadURL, buf[:n], int(offset), int(offset)+n-1, int(totalSize)); err != nil {
			return fmt.Errorf("onedrive streaming chunk at offset %d: %w", offset, err)
		}
		offset += int64(n)
	}
	return nil
}

// ── Transport setup ─────────────────────────────────────────────────────────

func odCachedDialContext() func(ctx context.Context, network, addr string) (net.Conn, error) {
	dialer := &net.Dialer{
		Timeout:   10 * time.Second,
		KeepAlive: 30 * time.Second,
	}
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(addr)
		if err != nil {
			return dialer.DialContext(ctx, network, addr)
		}
		if cached, ok := odDNSCache.Load(host); ok {
			if addrs, ok := cached.([]string); ok && len(addrs) > 0 {
				return dialer.DialContext(ctx, network, net.JoinHostPort(addrs[0], port))
			}
		}
		addrs, err := net.DefaultResolver.LookupHost(ctx, host)
		if err != nil || len(addrs) == 0 {
			return dialer.DialContext(ctx, network, addr)
		}
		odDNSCache.Store(host, addrs)
		return dialer.DialContext(ctx, network, net.JoinHostPort(addrs[0], port))
	}
}

func odGraphTransport(dial func(ctx context.Context, network, addr string) (net.Conn, error), tlsCache tls.ClientSessionCache) *http.Transport {
	t := &http.Transport{
		MaxIdleConns:        200,
		MaxIdleConnsPerHost: 200,
		IdleConnTimeout:     90 * time.Second,
		TLSHandshakeTimeout: 10 * time.Second,
		ReadBufferSize:      1 << 20,
		WriteBufferSize:     1 << 20,
		DisableCompression:  true,
		ForceAttemptHTTP2:   true,
		DialContext:         dial,
		TLSClientConfig: &tls.Config{
			MinVersion:         tls.VersionTLS12,
			ClientSessionCache: tlsCache,
		},
	}
	_ = http2.ConfigureTransport(t)
	return t
}

// HTTP/1.1 for CDN bulk downloads — bypasses Go HTTP/2 flow-control bugs
func odCDNTransport(dial func(ctx context.Context, network, addr string) (net.Conn, error), tlsCache tls.ClientSessionCache) *http.Transport {
	return &http.Transport{
		TLSNextProto:        make(map[string]func(authority string, c *tls.Conn) http.RoundTripper),
		MaxConnsPerHost:     128,
		MaxIdleConnsPerHost: 32,
		IdleConnTimeout:     60 * time.Second,
		TLSHandshakeTimeout: 10 * time.Second,
		ReadBufferSize:      4 * 1024 * 1024,
		WriteBufferSize:     4 * 1024 * 1024,
		DisableCompression:  true,
		DialContext:         dial,
		TLSClientConfig: &tls.Config{
			MinVersion:         tls.VersionTLS12,
			ClientSessionCache: tlsCache,
		},
	}
}

func odUploadTransport(dial func(ctx context.Context, network, addr string) (net.Conn, error), tlsCache tls.ClientSessionCache) *http.Transport {
	t := &http.Transport{
		MaxIdleConns:        200,
		MaxIdleConnsPerHost: 200,
		IdleConnTimeout:     90 * time.Second,
		TLSHandshakeTimeout: 10 * time.Second,
		ReadBufferSize:      4 * 1024 * 1024,
		WriteBufferSize:     4 * 1024 * 1024,
		DisableCompression:  true,
		ForceAttemptHTTP2:   true,
		DialContext:         dial,
		TLSClientConfig: &tls.Config{
			MinVersion:         tls.VersionTLS12,
			ClientSessionCache: tlsCache,
		},
	}
	_ = http2.ConfigureTransport(t)
	return t
}

// ── Helpers ─────────────────────────────────────────────────────────────────

// odEscapePath escapes each segment of a path individually, preserving
// the "/" separators so Graph API creates proper nested folders.
// Without this, url.PathEscape encodes slashes and creates flat filenames.
func odEscapePath(path string) string {
	parts := strings.Split(path, "/")
	for i, p := range parts {
		parts[i] = url.PathEscape(p)
	}
	return strings.Join(parts, "/")
}

func odDecorrelatedJitter(base, previous, cap time.Duration) time.Duration {
	high := previous * 3
	if high > cap {
		high = cap
	}
	if high <= base {
		return base
	}
	return base + time.Duration(mrand.Int64N(int64(high-base))) // #nosec G404 -- jitter for backoff, not security
}
