// Package main implements permafrost-v3 — experimental OneDrive optimizations.
//
// Key discoveries applied:
//   - HTTP/1.1 for CDN downloads (Go's HTTP/2 has flow-control bugs: #54330, #47840, #63520)
//   - Parallel byte-range downloads for large files (adaptive 1-8 streams)
//   - 60MB upload chunks (max per Microsoft, was 10MB)
//   - 4MB read/write buffers (was 1MB)
//   - Batch metadata prefetch (/$batch for 20 downloadUrls)
//   - Dual transport: HTTP/2 for Graph API, HTTP/1.1 for CDN bulk data
//   - Shared TLS session cache (0-RTT resumption across connections)
//   - Connection pooling on CDN transport (reuse idle HTTP/1.1 connections)
//   - Pipelined metadata+download (overlap URL resolution with data transfer)
package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"log"
	mrand "math/rand/v2"
	"net"
	"net/http"
	"net/url"
	"os"
	"runtime"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"golang.org/x/net/http2"
)

const (
	graphBase      = "https://graph.microsoft.com/v1.0"
	userAgent      = "ISV|FairForge|Vaultaire/1.0"
	chunkAlign     = 320 * 1024
	chunkSizeSmall = 10 * 1024 * 1024 // 10MB (v2 default)
	chunkSizeLarge = 60 * 1024 * 1024 // 60MB (max per Microsoft)
	simpleMax      = 4 * 1024 * 1024
	rangeStreams   = 8 // parallel byte-range streams per file (legacy constant)
)

// devNull is a write sink that does NOT implement io.ReaderFrom.
// io.Discard implements ReaderFrom with 8KB internal pool buffers, which
// silently bypasses any buffer passed to io.CopyBuffer. This type forces
// CopyBuffer to use our 1MB pooled buffer instead.
type devNull struct{}

func (devNull) Write(p []byte) (int, error) { return len(p), nil }

var drain devNull
var copyPool = sync.Pool{New: func() any { b := make([]byte, 1<<20); return &b }}

func pooledDrain(src io.Reader) (int64, error) {
	bp := copyPool.Get().(*[]byte)
	defer copyPool.Put(bp)
	return io.CopyBuffer(drain, src, *bp)
}

// Shared TLS session cache: enables 0-RTT resumption across connections.
// All three transports share this so a CDN edge seen by one download can be
// resumed by the next without a full TLS handshake.
var tlsSessionCache = tls.NewLRUClientSessionCache(256)

// adaptiveRangeStreams picks optimal stream count based on file size.
// Benchmarked 2026-04-20: 8 streams is 32% faster than 4 at 100MB.
// Threshold lowered from 200MB to 50MB based on Test 3 data.
func adaptiveRangeStreams(fileSize int64) int {
	switch {
	case fileSize < 10*1024*1024:
		return 1
	case fileSize < 25*1024*1024:
		return 2
	case fileSize < 50*1024*1024:
		return 4
	default:
		return 8
	}
}

// ── DNS cache + Dual transport ───────────────────────────────────────────────

var dnsCache sync.Map

func cachedDialContext(base *net.Dialer) func(ctx context.Context, network, addr string) (net.Conn, error) {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(addr)
		if err != nil {
			return base.DialContext(ctx, network, addr)
		}
		if cached, ok := dnsCache.Load(host); ok {
			addrs := cached.([]string)
			if len(addrs) > 0 {
				return base.DialContext(ctx, network, net.JoinHostPort(addrs[0], port))
			}
		}
		addrs, err := net.DefaultResolver.LookupHost(ctx, host)
		if err != nil || len(addrs) == 0 {
			return base.DialContext(ctx, network, addr)
		}
		dnsCache.Store(host, addrs)
		return base.DialContext(ctx, network, net.JoinHostPort(addrs[0], port))
	}
}

func sharedDialContext() func(ctx context.Context, network, addr string) (net.Conn, error) {
	return cachedDialContext(&net.Dialer{
		Timeout:   10 * time.Second,
		KeepAlive: 30 * time.Second,
	})
}

// graphTransport: HTTP/2 for small JSON API calls (metadata, auth, folder ops)
func graphTransport() *http.Transport {
	t := &http.Transport{
		MaxIdleConns:        200,
		MaxIdleConnsPerHost: 200,
		IdleConnTimeout:     90 * time.Second,
		TLSHandshakeTimeout: 10 * time.Second,
		ReadBufferSize:      1 << 20,
		WriteBufferSize:     1 << 20,
		DisableCompression:  true,
		ForceAttemptHTTP2:   true,
		DialContext:         sharedDialContext(),
		TLSClientConfig: &tls.Config{
			MinVersion:         tls.VersionTLS12,
			ClientSessionCache: tlsSessionCache,
		},
	}
	_ = http2.ConfigureTransport(t)
	return t
}

// cdnTransport: HTTP/1.1 for bulk data downloads.
// Go's HTTP/2 has flow-control bugs (#54330, #47840, #63520) that throttle
// concurrent streams on a single connection. Rclone measured 66x improvement
// by forcing HTTP/1.1 with separate TCP connections per request.
func cdnTransport() *http.Transport {
	return &http.Transport{
		TLSNextProto:        make(map[string]func(authority string, c *tls.Conn) http.RoundTripper),
		MaxConnsPerHost:     128,
		MaxIdleConnsPerHost: 32,
		IdleConnTimeout:     60 * time.Second,
		TLSHandshakeTimeout: 10 * time.Second,
		ReadBufferSize:      4 * 1024 * 1024,
		WriteBufferSize:     4 * 1024 * 1024,
		DisableCompression:  true,
		DialContext:         sharedDialContext(),
		TLSClientConfig: &tls.Config{
			MinVersion:         tls.VersionTLS12,
			ClientSessionCache: tlsSessionCache,
		},
	}
}

// uploadTransport: HTTP/2 for uploads (multiplexing helps here)
func uploadTransport() *http.Transport {
	t := &http.Transport{
		MaxIdleConns:        200,
		MaxIdleConnsPerHost: 200,
		IdleConnTimeout:     90 * time.Second,
		TLSHandshakeTimeout: 10 * time.Second,
		ReadBufferSize:      4 * 1024 * 1024,
		WriteBufferSize:     4 * 1024 * 1024,
		DisableCompression:  true,
		ForceAttemptHTTP2:   true,
		DialContext:         sharedDialContext(),
		TLSClientConfig: &tls.Config{
			MinVersion:         tls.VersionTLS12,
			ClientSessionCache: tlsSessionCache,
		},
	}
	_ = http2.ConfigureTransport(t)
	return t
}

// ── Tenant config ─────────────────────────────────────────────────────────────

type tenant struct {
	name, tenantID, clientID, secret, userUPN string
}

func loadTenants() []tenant {
	var out []tenant
	for i := 1; i <= 15; i++ {
		p := fmt.Sprintf("TENANT_%d_", i)
		if os.Getenv(p+"ID") == "" {
			continue
		}
		out = append(out, tenant{
			name:     fmt.Sprintf("tenant-%d", i),
			tenantID: os.Getenv(p + "ID"),
			clientID: os.Getenv(p + "CLIENT_ID"),
			secret:   os.Getenv(p + "SECRET"),
			userUPN:  os.Getenv(p + "USER"),
		})
	}
	return out
}

// ── authClient: triple-transport HTTP client ─────────────────────────────────

type authClient struct {
	t          tenant
	cred       azcore.TokenCredential
	graphHTTP  *http.Client // HTTP/2 for Graph API
	cdnHTTP    *http.Client // HTTP/1.1 for CDN downloads
	uploadHTTP *http.Client // HTTP/2 for uploads

	mu        sync.Mutex
	driveID   string
	totalReqs atomic.Int64
	throttled atomic.Int64
}

func newAuthClient(t tenant) (*authClient, error) {
	cred, err := azidentity.NewClientSecretCredential(t.tenantID, t.clientID, t.secret, nil)
	if err != nil {
		return nil, err
	}
	return &authClient{
		t:          t,
		cred:       cred,
		graphHTTP:  &http.Client{Transport: graphTransport()},
		cdnHTTP:    &http.Client{Transport: cdnTransport()},
		uploadHTTP: &http.Client{Transport: uploadTransport()},
	}, nil
}

func (a *authClient) token(ctx context.Context) (string, error) {
	tok, err := a.cred.GetToken(ctx, policy.TokenRequestOptions{
		Scopes: []string{"https://graph.microsoft.com/.default"},
	})
	if err != nil {
		return "", err
	}
	return tok.Token, nil
}

func decorrelatedJitter(base, previous, cap time.Duration) time.Duration {
	high := previous * 3
	if high > cap {
		high = cap
	}
	if high <= base {
		return base
	}
	return base + time.Duration(mrand.Int64N(int64(high-base)))
}

// graphDo makes an authenticated Graph API call (HTTP/2)
func (a *authClient) graphDo(ctx context.Context, req *http.Request) (*http.Response, error) {
	const maxAttempts = 5
	base := 500 * time.Millisecond
	prev := base

	token, err := a.token(ctx)
	if err != nil {
		return nil, err
	}

	for attempt := 0; attempt < maxAttempts; attempt++ {
		if attempt > 0 && req.GetBody != nil {
			body, _ := req.GetBody()
			req.Body = body
		}
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("User-Agent", userAgent)

		resp, err := a.graphHTTP.Do(req)
		a.totalReqs.Add(1)
		if err != nil {
			if attempt < maxAttempts-1 {
				prev = decorrelatedJitter(base, prev, 30*time.Second)
				time.Sleep(prev)
				continue
			}
			return nil, err
		}
		if resp.StatusCode == 429 || resp.StatusCode == 503 {
			a.throttled.Add(1)
			retryAfter, _ := strconv.Atoi(resp.Header.Get("Retry-After"))
			wait := decorrelatedJitter(base, prev, 30*time.Second)
			if ra := time.Duration(retryAfter) * time.Second; ra > wait {
				wait = ra
			}
			prev = wait
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			if attempt < maxAttempts-1 {
				time.Sleep(wait)
				continue
			}
			return nil, fmt.Errorf("throttled (%d)", resp.StatusCode)
		}
		if resp.StatusCode >= 500 {
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			if attempt < maxAttempts-1 {
				prev = decorrelatedJitter(base, prev, 30*time.Second)
				time.Sleep(prev)
				continue
			}
			return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
		}
		if resp.StatusCode >= 400 {
			b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
			_ = resp.Body.Close()
			return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, b)
		}
		return resp, nil
	}
	return nil, fmt.Errorf("exhausted retries")
}

// ── Graph operations ─────────────────────────────────────────────────────────

func (a *authClient) getDriveID(ctx context.Context) (string, error) {
	a.mu.Lock()
	if a.driveID != "" {
		d := a.driveID
		a.mu.Unlock()
		return d, nil
	}
	a.mu.Unlock()

	u := fmt.Sprintf("%s/users/%s/drives", graphBase, url.PathEscape(a.t.userUPN))
	req, _ := http.NewRequestWithContext(ctx, "GET", u, nil)
	resp, err := a.graphDo(ctx, req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	var out struct {
		Value []struct {
			ID string `json:"id"`
		} `json:"value"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	if len(out.Value) == 0 {
		return "", fmt.Errorf("no drives for %s", a.t.userUPN)
	}
	a.mu.Lock()
	a.driveID = out.Value[0].ID
	a.mu.Unlock()
	return a.driveID, nil
}

func (a *authClient) createFolder(ctx context.Context, driveID, name string) (string, error) {
	u := fmt.Sprintf("%s/drives/%s/items/root/children", graphBase, driveID)
	body, _ := json.Marshal(map[string]any{
		"name": name, "folder": map[string]any{},
		"@microsoft.graph.conflictBehavior": "rename",
	})
	req, _ := http.NewRequestWithContext(ctx, "POST", u, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(body)), nil }
	resp, err := a.graphDo(ctx, req)
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

func (a *authClient) deleteItem(ctx context.Context, driveID, itemID string) {
	u := fmt.Sprintf("%s/drives/%s/items/%s", graphBase, driveID, itemID)
	req, _ := http.NewRequestWithContext(ctx, "DELETE", u, nil)
	resp, _ := a.graphDo(ctx, req)
	if resp != nil {
		_ = resp.Body.Close()
	}
}

// ── Upload ───────────────────────────────────────────────────────────────────

func (a *authClient) simpleUpload(ctx context.Context, driveID, parentID, name string, data []byte) error {
	u := fmt.Sprintf("%s/drives/%s/items/%s:/%s:/content",
		graphBase, driveID, parentID, url.PathEscape(name))
	req, _ := http.NewRequestWithContext(ctx, "PUT", u, bytes.NewReader(data))
	req.ContentLength = int64(len(data))
	req.Header.Set("Content-Type", "application/octet-stream")
	req.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(data)), nil }
	resp, err := a.graphDo(ctx, req)
	if err != nil {
		return err
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	return nil
}

func (a *authClient) chunkedUpload(ctx context.Context, driveID, parentID, name string, data []byte, chunkSz int) error {
	sessionURL := fmt.Sprintf("%s/drives/%s/items/%s:/%s:/createUploadSession",
		graphBase, driveID, parentID, url.PathEscape(name))
	sb, _ := json.Marshal(map[string]any{
		"item": map[string]any{"@microsoft.graph.conflictBehavior": "replace"},
	})
	req, _ := http.NewRequestWithContext(ctx, "POST", sessionURL, bytes.NewReader(sb))
	req.Header.Set("Content-Type", "application/json")
	req.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(sb)), nil }
	resp, err := a.graphDo(ctx, req)
	if err != nil {
		return err
	}
	var session struct {
		UploadURL string `json:"uploadUrl"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&session)
	_ = resp.Body.Close()

	total := len(data)
	for offset := 0; offset < total; offset += chunkSz {
		end := offset + chunkSz
		if end > total {
			end = total
		}
		chunk := data[offset:end]
		if err := a.putChunk(ctx, session.UploadURL, chunk, offset, end-1, total); err != nil {
			return err
		}
	}
	return nil
}

func (a *authClient) putChunk(ctx context.Context, uploadURL string, chunk []byte, start, endInc, total int) error {
	for attempt := 0; attempt < 3; attempt++ {
		req, _ := http.NewRequestWithContext(ctx, "PUT", uploadURL, bytes.NewReader(chunk))
		req.ContentLength = int64(len(chunk))
		req.Header.Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, endInc, total))
		resp, err := a.uploadHTTP.Do(req)
		a.totalReqs.Add(1)
		if err != nil {
			time.Sleep(time.Duration(1<<attempt) * time.Second)
			continue
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			return nil
		}
		if resp.StatusCode == 429 || resp.StatusCode >= 500 {
			time.Sleep(time.Duration(1<<attempt) * time.Second)
			continue
		}
		return fmt.Errorf("chunk HTTP %d", resp.StatusCode)
	}
	return fmt.Errorf("chunk exhausted retries")
}

func (a *authClient) upload(ctx context.Context, driveID, parentID, name string, data []byte) error {
	if len(data) <= simpleMax {
		return a.simpleUpload(ctx, driveID, parentID, name, data)
	}
	return a.chunkedUpload(ctx, driveID, parentID, name, data, chunkSizeLarge)
}

// ── Download: three strategies ───────────────────────────────────────────────

// getDownloadURL fetches the pre-authenticated CDN URL from item metadata.
func (a *authClient) getDownloadURL(ctx context.Context, driveID, parentID, name string) (string, int64, error) {
	u := fmt.Sprintf("%s/drives/%s/items/%s:/%s",
		graphBase, driveID, parentID, url.PathEscape(name))
	req, _ := http.NewRequestWithContext(ctx, "GET", u, nil)
	resp, err := a.graphDo(ctx, req)
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
		return "", 0, fmt.Errorf("no downloadUrl")
	}
	return meta.DownloadURL, meta.Size, nil
}

// Strategy A: HTTP/2 download (v2 baseline — for comparison)
func (a *authClient) downloadHTTP2(ctx context.Context, dlURL string) (int64, error) {
	req, _ := http.NewRequestWithContext(ctx, "GET", dlURL, nil)
	resp, err := a.graphHTTP.Do(req) // HTTP/2 transport
	if err != nil {
		return 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	return pooledDrain(resp.Body)
}

// Strategy B: HTTP/1.1 download (rclone's fix for Go HTTP/2 flow-control bug)
func (a *authClient) downloadHTTP1(ctx context.Context, dlURL string) (int64, error) {
	req, _ := http.NewRequestWithContext(ctx, "GET", dlURL, nil)
	resp, err := a.cdnHTTP.Do(req) // HTTP/1.1 transport
	if err != nil {
		return 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	return pooledDrain(resp.Body)
}

// Strategy C: Parallel byte-range download (split file into N parallel streams)
func (a *authClient) downloadRanges(ctx context.Context, dlURL string, fileSize int64, streams int) (int64, error) {
	if fileSize <= 0 || streams <= 1 {
		return a.downloadHTTP1(ctx, dlURL)
	}

	rangeSize := fileSize / int64(streams)
	var totalBytes atomic.Int64
	var firstErr error
	var errOnce sync.Once
	var wg sync.WaitGroup

	for i := 0; i < streams; i++ {
		start := int64(i) * rangeSize
		end := start + rangeSize - 1
		if i == streams-1 {
			end = fileSize - 1
		}
		wg.Add(1)
		go func(s, e int64) {
			defer wg.Done()
			req, _ := http.NewRequestWithContext(ctx, "GET", dlURL, nil)
			req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", s, e))
			resp, err := a.cdnHTTP.Do(req)
			if err != nil {
				errOnce.Do(func() { firstErr = err })
				return
			}
			defer func() { _ = resp.Body.Close() }()
			if resp.StatusCode != 206 && resp.StatusCode != 200 {
				errOnce.Do(func() { firstErr = fmt.Errorf("range HTTP %d", resp.StatusCode) })
				return
			}
			n, _ := pooledDrain(resp.Body)
			totalBytes.Add(n)
		}(start, end)
	}
	wg.Wait()
	return totalBytes.Load(), firstErr
}

// Batch prefetch: fetch up to 20 downloadUrls in one /$batch request
func (a *authClient) batchDownloadURLs(ctx context.Context, driveID, parentID string, names []string) (map[string]string, error) {
	type batchReq struct {
		ID     string `json:"id"`
		Method string `json:"method"`
		URL    string `json:"url"`
	}
	var requests []batchReq
	for i, name := range names {
		if i >= 20 {
			break
		}
		requests = append(requests, batchReq{
			ID:     strconv.Itoa(i),
			Method: "GET",
			URL:    fmt.Sprintf("/drives/%s/items/%s:/%s", driveID, parentID, url.PathEscape(name)),
		})
	}
	body, _ := json.Marshal(map[string]any{"requests": requests})
	req, _ := http.NewRequestWithContext(ctx, "POST", graphBase+"/$batch", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(body)), nil }
	resp, err := a.graphDo(ctx, req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	var batchResp struct {
		Responses []struct {
			ID     string         `json:"id"`
			Status int            `json:"status"`
			Body   map[string]any `json:"body"`
		} `json:"responses"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&batchResp)

	result := make(map[string]string)
	for _, r := range batchResp.Responses {
		idx, _ := strconv.Atoi(r.ID)
		if idx < len(names) {
			if dlURL, ok := r.Body["@microsoft.graph.downloadUrl"].(string); ok {
				result[names[idx]] = dlURL
			}
		}
	}
	return result, nil
}

// ── Test harness ──────────────────────────────────────────────────────────────

type clientEntry struct {
	a       *authClient
	driveID string
	folder  string
}

func main() {
	tenants := loadTenants()
	if len(tenants) == 0 {
		log.Fatal("No tenants. Set TENANT_1_ID etc.")
	}

	fmt.Println("═══════════════════════════════════════════════════════════════")
	fmt.Println("  PERMAFROST v3.1 — OPTIMIZED (conn pool + TLS cache + adaptive)")
	fmt.Println("═══════════════════════════════════════════════════════════════")
	fmt.Printf("  Platform:      %s/%s\n", runtime.GOOS, runtime.GOARCH)
	fmt.Printf("  Tenants:       %d\n", len(tenants))
	fmt.Printf("  API transport: HTTP/2, 1MB buffers (Graph API calls)\n")
	fmt.Printf("  CDN transport: HTTP/1.1, 4MB buffers, 32 idle pool, 128 max conn\n")
	fmt.Printf("  Upload:        HTTP/2, 4MB buffers, 60MB chunks (max)\n")
	fmt.Printf("  Range DL:      adaptive (1/2/4/8 streams by file size)\n")
	fmt.Printf("  TLS cache:     shared 256-entry session cache (0-RTT resumption)\n")
	fmt.Printf("  Batch:         /$batch prefetch (20 URLs/request)\n")
	fmt.Printf("  Pipeline:      Strategy F — overlap metadata + downloads\n")
	fmt.Println("═══════════════════════════════════════════════════════════════")
	fmt.Println()

	var clients []clientEntry
	for _, t := range tenants {
		a, err := newAuthClient(t)
		if err != nil {
			log.Printf("[%s] SKIPPED — %v", t.name, err)
			continue
		}
		ctx := context.Background()
		driveID, err := a.getDriveID(ctx)
		if err != nil {
			log.Printf("[%s] SKIPPED — %v", t.name, err)
			continue
		}
		folderID, err := a.createFolder(ctx, driveID, fmt.Sprintf("v3-%d", time.Now().UnixNano()))
		if err != nil {
			log.Printf("[%s] SKIPPED — %v", t.name, err)
			continue
		}
		log.Printf("[%s] Ready", t.name)
		clients = append(clients, clientEntry{a, driveID, folderID})
	}
	if len(clients) == 0 {
		log.Fatal("No tenants")
	}
	fmt.Printf("\n  %d/%d tenants active\n\n", len(clients), len(tenants))

	runDownloadAB(clients)
	runParallelHTTP1(clients)
	runRangeDownload(clients)
	runBatchPrefetch(clients)
	runUploadChunkAB(clients)
	runFleetDownload(clients)

	fmt.Println("\nCleaning up...")
	for _, ce := range clients {
		ce.a.deleteItem(context.Background(), ce.driveID, ce.folder)
	}
	fmt.Println("Done.")
}

// ── Test 1: HTTP/2 vs HTTP/1.1 Download A/B ──────────────────────────────────

func runDownloadAB(clients []clientEntry) {
	printHeader("TEST 1: HTTP/2 vs HTTP/1.1 Download (single 50MB file, per tenant)")

	data := randomData(50 * 1024 * 1024)
	ce := clients[0]
	fileName := "dl-ab-50mb.bin"
	_ = ce.a.upload(context.Background(), ce.driveID, ce.folder, fileName, data)

	dlURL, _, err := ce.a.getDownloadURL(context.Background(), ce.driveID, ce.folder, fileName)
	if err != nil || dlURL == "" {
		fmt.Printf("  ❌ Failed to get downloadUrl: %v\n\n", err)
		return
	}

	fmt.Printf("  %-20s  %-10s  %-10s\n", "Transport", "MB/s", "Duration")
	fmt.Println("  ─────────────────────────────────────────")

	// HTTP/2 (v2 behavior)
	start := time.Now()
	n1, _ := ce.a.downloadHTTP2(context.Background(), dlURL)
	d1 := time.Since(start)
	fmt.Printf("  %-20s  %-10.2f  %s\n", "HTTP/2 (v2)", float64(n1)/(1024*1024)/d1.Seconds(), d1.Round(time.Millisecond))

	// HTTP/1.1 (rclone fix)
	start = time.Now()
	n2, _ := ce.a.downloadHTTP1(context.Background(), dlURL)
	d2 := time.Since(start)
	fmt.Printf("  %-20s  %-10.2f  %s\n", "HTTP/1.1 (v3)", float64(n2)/(1024*1024)/d2.Seconds(), d2.Round(time.Millisecond))

	diff := (float64(n2)/d2.Seconds())/(float64(n1)/d1.Seconds())*100 - 100
	fmt.Printf("\n  HTTP/1.1 is %+.1f%% vs HTTP/2\n\n", diff)
}

// ── Test 2: Parallel Downloads with HTTP/1.1 ─────────────────────────────────

func runParallelHTTP1(clients []clientEntry) {
	printHeader("TEST 2: Parallel Download — HTTP/1.1 (10 × 50MB, all tenants)")

	data := randomData(50 * 1024 * 1024)
	fileCount := 10

	// Seed files
	for _, ce := range clients {
		var wg sync.WaitGroup
		sem := make(chan struct{}, 5)
		for i := 0; i < fileCount; i++ {
			wg.Add(1)
			go func(n int) {
				defer wg.Done()
				sem <- struct{}{}
				defer func() { <-sem }()
				_ = ce.a.upload(context.Background(), ce.driveID, ce.folder,
					fmt.Sprintf("par-h1-%02d.bin", n), data)
			}(i)
		}
		wg.Wait()
	}
	fmt.Printf("  Seeded %d × 50MB files per tenant.\n\n", fileCount)

	fmt.Printf("  %-10s  %-12s  %-12s  %-10s\n", "Workers", "Fleet MB/s", "Per-tenant", "p95")
	fmt.Println("  ─────────────────────────────────────────────")

	for _, workers := range []int{1, 3, 6, 12, 24} {
		var totalBytes atomic.Int64
		var durations []time.Duration
		var durMu sync.Mutex
		var wg sync.WaitGroup

		fleetStart := time.Now()
		for _, ce := range clients {
			wg.Add(1)
			go func(ce clientEntry) {
				defer wg.Done()
				sem := make(chan struct{}, workers)
				var innerWg sync.WaitGroup
				for i := 0; i < fileCount; i++ {
					innerWg.Add(1)
					go func(n int) {
						defer innerWg.Done()
						sem <- struct{}{}
						defer func() { <-sem }()
						dlURL, _, _ := ce.a.getDownloadURL(context.Background(),
							ce.driveID, ce.folder, fmt.Sprintf("par-h1-%02d.bin", n))
						if dlURL == "" {
							return
						}
						s := time.Now()
						b, _ := ce.a.downloadHTTP1(context.Background(), dlURL)
						d := time.Since(s)
						totalBytes.Add(b)
						durMu.Lock()
						durations = append(durations, d)
						durMu.Unlock()
					}(i)
				}
				innerWg.Wait()
			}(ce)
		}
		wg.Wait()
		fleetDur := time.Since(fleetStart)

		fleetMBs := float64(totalBytes.Load()) / (1024 * 1024) / fleetDur.Seconds()
		perTenant := fleetMBs / float64(len(clients))
		sort.Slice(durations, func(i, j int) bool { return durations[i] < durations[j] })
		p95 := time.Duration(0)
		if len(durations) > 0 {
			p95 = durations[len(durations)*95/100]
		}
		fmt.Printf("  %-10d  %-12.2f  %-12.2f  %s\n", workers, fleetMBs, perTenant, p95.Round(time.Millisecond))
	}
	fmt.Println()
}

// ── Test 3: Parallel Range Download (split file into N streams) ──────────────

func runRangeDownload(clients []clientEntry) {
	printHeader("TEST 3: Range Download — split 1 file into N parallel byte-range streams")

	data := randomData(100 * 1024 * 1024) // 100MB file
	ce := clients[0]
	fileName := "range-100mb.bin"
	_ = ce.a.upload(context.Background(), ce.driveID, ce.folder, fileName, data)

	dlURL, fileSize, err := ce.a.getDownloadURL(context.Background(), ce.driveID, ce.folder, fileName)
	if err != nil || dlURL == "" {
		fmt.Printf("  ❌ Failed to get downloadUrl: %v\n\n", err)
		return
	}
	fmt.Printf("  File: %d MB, CDN URL ready\n\n", fileSize/(1024*1024))

	fmt.Printf("  %-12s  %-12s  %-10s  %-10s\n", "Streams", "MB/s", "Duration", "Transport")
	fmt.Println("  ─────────────────────────────────────────────")

	// Baseline: single stream HTTP/2
	start := time.Now()
	n, _ := ce.a.downloadHTTP2(context.Background(), dlURL)
	d := time.Since(start)
	fmt.Printf("  %-12s  %-12.2f  %-10s  HTTP/2\n", "1 (baseline)", float64(n)/(1024*1024)/d.Seconds(), d.Round(time.Millisecond))

	// Single stream HTTP/1.1
	start = time.Now()
	n, _ = ce.a.downloadHTTP1(context.Background(), dlURL)
	d = time.Since(start)
	fmt.Printf("  %-12s  %-12.2f  %-10s  HTTP/1.1\n", "1", float64(n)/(1024*1024)/d.Seconds(), d.Round(time.Millisecond))

	// Parallel ranges
	for _, streams := range []int{2, 4, 8, 16} {
		start = time.Now()
		n, err := ce.a.downloadRanges(context.Background(), dlURL, fileSize, streams)
		d = time.Since(start)
		status := ""
		if err != nil {
			status = fmt.Sprintf(" (%v)", err)
		}
		fmt.Printf("  %-12d  %-12.2f  %-10s  HTTP/1.1%s\n", streams,
			float64(n)/(1024*1024)/d.Seconds(), d.Round(time.Millisecond), status)
	}

	adaptive := adaptiveRangeStreams(fileSize)
	start = time.Now()
	n, _ = ce.a.downloadRanges(context.Background(), dlURL, fileSize, adaptive)
	d = time.Since(start)
	fmt.Printf("  %-12s  %-12.2f  %-10s  HTTP/1.1 (auto=%d)\n",
		"adaptive", float64(n)/(1024*1024)/d.Seconds(), d.Round(time.Millisecond), adaptive)
	fmt.Println()
}

// ── Test 4: Batch Prefetch + Parallel Download ───────────────────────────────

func runBatchPrefetch(clients []clientEntry) {
	printHeader("TEST 4: Batch Prefetch (/$batch for 10 URLs) + Parallel HTTP/1.1 Download")

	data := randomData(50 * 1024 * 1024)
	ce := clients[0]
	fileCount := 10

	// Seed
	for i := 0; i < fileCount; i++ {
		_ = ce.a.upload(context.Background(), ce.driveID, ce.folder,
			fmt.Sprintf("batch-%02d.bin", i), data)
	}

	var names []string
	for i := 0; i < fileCount; i++ {
		names = append(names, fmt.Sprintf("batch-%02d.bin", i))
	}

	// Individual metadata fetch (baseline)
	start := time.Now()
	var urls1 []string
	for _, name := range names {
		dlURL, _, _ := ce.a.getDownloadURL(context.Background(), ce.driveID, ce.folder, name)
		urls1 = append(urls1, dlURL)
	}
	metaIndividual := time.Since(start)

	// Batch metadata fetch
	start = time.Now()
	urlMap, err := ce.a.batchDownloadURLs(context.Background(), ce.driveID, ce.folder, names)
	metaBatch := time.Since(start)

	fmt.Printf("  Individual metadata:  %s (%d URLs)\n", metaIndividual.Round(time.Millisecond), len(urls1))
	fmt.Printf("  Batch metadata:       %s (%d URLs)  %v\n", metaBatch.Round(time.Millisecond), len(urlMap), err)
	speedup := float64(metaIndividual) / float64(metaBatch)
	fmt.Printf("  Speedup:              %.1fx\n\n", speedup)

	// Parallel download with batch-prefetched URLs
	if len(urlMap) > 0 {
		var totalBytes atomic.Int64
		var wg sync.WaitGroup
		start = time.Now()
		for _, dlURL := range urlMap {
			wg.Add(1)
			go func(u string) {
				defer wg.Done()
				n, _ := ce.a.downloadHTTP1(context.Background(), u)
				totalBytes.Add(n)
			}(dlURL)
		}
		wg.Wait()
		d := time.Since(start)
		fmt.Printf("  Batch+parallel DL:    %.2f MB/s (%s, %d files)\n",
			float64(totalBytes.Load())/(1024*1024)/d.Seconds(), d.Round(time.Millisecond), len(urlMap))
	}
	fmt.Println()
}

// ── Test 5: Upload Chunk Size A/B (10MB vs 60MB) ─────────────────────────────

func runUploadChunkAB(clients []clientEntry) {
	printHeader("TEST 5: Upload Chunk Size A/B (100MB file: 10MB vs 60MB chunks)")

	data := randomData(100 * 1024 * 1024)
	ce := clients[0]

	fmt.Printf("  %-20s  %-10s  %-12s  %-8s\n", "Chunk size", "Duration", "MB/s", "Chunks")
	fmt.Println("  ─────────────────────────────────────────────")

	// 10MB chunks (v2 default)
	start := time.Now()
	_ = ce.a.chunkedUpload(context.Background(), ce.driveID, ce.folder,
		"chunk-ab-10mb.bin", data, chunkSizeSmall)
	d1 := time.Since(start)
	fmt.Printf("  %-20s  %-10s  %-12.2f  %-8d\n", "10 MB", d1.Round(time.Millisecond),
		100.0/d1.Seconds(), (100*1024*1024+chunkSizeSmall-1)/chunkSizeSmall)

	// 60MB chunks (max)
	start = time.Now()
	_ = ce.a.chunkedUpload(context.Background(), ce.driveID, ce.folder,
		"chunk-ab-60mb.bin", data, chunkSizeLarge)
	d2 := time.Since(start)
	fmt.Printf("  %-20s  %-10s  %-12.2f  %-8d\n", "60 MB (max)", d2.Round(time.Millisecond),
		100.0/d2.Seconds(), (100*1024*1024+chunkSizeLarge-1)/chunkSizeLarge)

	diff := (d1.Seconds() - d2.Seconds()) / d1.Seconds() * 100
	fmt.Printf("\n  60MB chunks are %+.1f%% faster (fewer round trips)\n\n", diff)
}

// ── Test 6: Fleet Download with all v3 optimizations ─────────────────────────

func runFleetDownload(clients []clientEntry) {
	printHeader("TEST 6: Fleet Download — ALL optimizations (HTTP/1.1 + range + batch)")

	data := randomData(50 * 1024 * 1024)
	fileCount := 10

	// Seed all tenants
	for _, ce := range clients {
		var wg sync.WaitGroup
		sem := make(chan struct{}, 5)
		for i := 0; i < fileCount; i++ {
			wg.Add(1)
			go func(n int) {
				defer wg.Done()
				sem <- struct{}{}
				defer func() { <-sem }()
				_ = ce.a.upload(context.Background(), ce.driveID, ce.folder,
					fmt.Sprintf("fleet-%02d.bin", n), data)
			}(i)
		}
		wg.Wait()
	}
	fmt.Printf("  Seeded %d × 50MB files per tenant.\n\n", fileCount)

	fmt.Printf("  %-30s  %-12s  %-12s\n", "Strategy", "Fleet MB/s", "Per-tenant")
	fmt.Println("  ─────────────────────────────────────────────────")

	// Strategy A: v2 baseline (HTTP/2, sequential metadata, 5 workers)
	{
		var totalBytes atomic.Int64
		var wg sync.WaitGroup
		start := time.Now()
		for _, ce := range clients {
			wg.Add(1)
			go func(ce clientEntry) {
				defer wg.Done()
				sem := make(chan struct{}, 5)
				var innerWg sync.WaitGroup
				for i := 0; i < fileCount; i++ {
					innerWg.Add(1)
					go func(n int) {
						defer innerWg.Done()
						sem <- struct{}{}
						defer func() { <-sem }()
						dlURL, _, _ := ce.a.getDownloadURL(context.Background(),
							ce.driveID, ce.folder, fmt.Sprintf("fleet-%02d.bin", n))
						if dlURL != "" {
							b, _ := ce.a.downloadHTTP2(context.Background(), dlURL)
							totalBytes.Add(b)
						}
					}(i)
				}
				innerWg.Wait()
			}(ce)
		}
		wg.Wait()
		d := time.Since(start)
		mbs := float64(totalBytes.Load()) / (1024 * 1024) / d.Seconds()
		fmt.Printf("  %-30s  %-12.2f  %-12.2f\n", "A: HTTP/2 5w (v2 baseline)", mbs, mbs/float64(len(clients)))
	}

	// Strategy B: HTTP/1.1, 5 workers
	{
		var totalBytes atomic.Int64
		var wg sync.WaitGroup
		start := time.Now()
		for _, ce := range clients {
			wg.Add(1)
			go func(ce clientEntry) {
				defer wg.Done()
				sem := make(chan struct{}, 5)
				var innerWg sync.WaitGroup
				for i := 0; i < fileCount; i++ {
					innerWg.Add(1)
					go func(n int) {
						defer innerWg.Done()
						sem <- struct{}{}
						defer func() { <-sem }()
						dlURL, _, _ := ce.a.getDownloadURL(context.Background(),
							ce.driveID, ce.folder, fmt.Sprintf("fleet-%02d.bin", n))
						if dlURL != "" {
							b, _ := ce.a.downloadHTTP1(context.Background(), dlURL)
							totalBytes.Add(b)
						}
					}(i)
				}
				innerWg.Wait()
			}(ce)
		}
		wg.Wait()
		d := time.Since(start)
		mbs := float64(totalBytes.Load()) / (1024 * 1024) / d.Seconds()
		fmt.Printf("  %-30s  %-12.2f  %-12.2f\n", "B: HTTP/1.1 5w", mbs, mbs/float64(len(clients)))
	}

	// Strategy C: HTTP/1.1, range download (4 streams per file), 5 files concurrent
	{
		var totalBytes atomic.Int64
		var wg sync.WaitGroup
		start := time.Now()
		for _, ce := range clients {
			wg.Add(1)
			go func(ce clientEntry) {
				defer wg.Done()
				sem := make(chan struct{}, 5)
				var innerWg sync.WaitGroup
				for i := 0; i < fileCount; i++ {
					innerWg.Add(1)
					go func(n int) {
						defer innerWg.Done()
						sem <- struct{}{}
						defer func() { <-sem }()
						dlURL, size, _ := ce.a.getDownloadURL(context.Background(),
							ce.driveID, ce.folder, fmt.Sprintf("fleet-%02d.bin", n))
						if dlURL != "" {
							b, _ := ce.a.downloadRanges(context.Background(), dlURL, size, 4)
							totalBytes.Add(b)
						}
					}(i)
				}
				innerWg.Wait()
			}(ce)
		}
		wg.Wait()
		d := time.Since(start)
		mbs := float64(totalBytes.Load()) / (1024 * 1024) / d.Seconds()
		fmt.Printf("  %-30s  %-12.2f  %-12.2f\n", "C: HTTP/1.1 range(4) 5w", mbs, mbs/float64(len(clients)))
	}

	// Strategy D: Batch prefetch + HTTP/1.1, 10 workers
	{
		var totalBytes atomic.Int64
		var wg sync.WaitGroup
		start := time.Now()
		for _, ce := range clients {
			wg.Add(1)
			go func(ce clientEntry) {
				defer wg.Done()
				var names []string
				for i := 0; i < fileCount; i++ {
					names = append(names, fmt.Sprintf("fleet-%02d.bin", i))
				}
				urlMap, _ := ce.a.batchDownloadURLs(context.Background(), ce.driveID, ce.folder, names)
				sem := make(chan struct{}, 10)
				var innerWg sync.WaitGroup
				for _, dlURL := range urlMap {
					innerWg.Add(1)
					go func(u string) {
						defer innerWg.Done()
						sem <- struct{}{}
						defer func() { <-sem }()
						b, _ := ce.a.downloadHTTP1(context.Background(), u)
						totalBytes.Add(b)
					}(dlURL)
				}
				innerWg.Wait()
			}(ce)
		}
		wg.Wait()
		d := time.Since(start)
		mbs := float64(totalBytes.Load()) / (1024 * 1024) / d.Seconds()
		fmt.Printf("  %-30s  %-12.2f  %-12.2f\n", "D: batch+HTTP/1.1 10w", mbs, mbs/float64(len(clients)))
	}

	// Strategy E: Batch + range(4) + HTTP/1.1, 5 concurrent files
	{
		var totalBytes atomic.Int64
		var wg sync.WaitGroup
		start := time.Now()
		for _, ce := range clients {
			wg.Add(1)
			go func(ce clientEntry) {
				defer wg.Done()
				var names []string
				for i := 0; i < fileCount; i++ {
					names = append(names, fmt.Sprintf("fleet-%02d.bin", i))
				}
				urlMap, _ := ce.a.batchDownloadURLs(context.Background(), ce.driveID, ce.folder, names)
				sem := make(chan struct{}, 5)
				var innerWg sync.WaitGroup
				for _, dlURL := range urlMap {
					innerWg.Add(1)
					go func(u string) {
						defer innerWg.Done()
						sem <- struct{}{}
						defer func() { <-sem }()
						b, _ := ce.a.downloadRanges(context.Background(), u, 50*1024*1024, 4)
						totalBytes.Add(b)
					}(dlURL)
				}
				innerWg.Wait()
			}(ce)
		}
		wg.Wait()
		d := time.Since(start)
		mbs := float64(totalBytes.Load()) / (1024 * 1024) / d.Seconds()
		fmt.Printf("  %-30s  %-12.2f  %-12.2f\n", "E: batch+range(4)+H1 5w", mbs, mbs/float64(len(clients)))
	}

	// Strategy F: Pipelined — overlap metadata fetches with downloads.
	// Instead of "fetch all URLs then download", start downloading as soon
	// as each URL resolves. Uses a channel to pipeline metadata→download.
	{
		var totalBytes atomic.Int64
		var wg sync.WaitGroup
		start := time.Now()
		for _, ce := range clients {
			wg.Add(1)
			go func(ce clientEntry) {
				defer wg.Done()
				type dlJob struct {
					url  string
					size int64
				}
				jobs := make(chan dlJob, fileCount)

				// Producer: fetch metadata and push URLs as they resolve
				go func() {
					defer close(jobs)
					var metaWg sync.WaitGroup
					metaSem := make(chan struct{}, 3)
					for i := 0; i < fileCount; i++ {
						metaWg.Add(1)
						go func(n int) {
							defer metaWg.Done()
							metaSem <- struct{}{}
							defer func() { <-metaSem }()
							dlURL, size, _ := ce.a.getDownloadURL(context.Background(),
								ce.driveID, ce.folder, fmt.Sprintf("fleet-%02d.bin", n))
							if dlURL != "" {
								jobs <- dlJob{dlURL, size}
							}
						}(i)
					}
					metaWg.Wait()
				}()

				// Consumer: download with adaptive Range as URLs arrive
				sem := make(chan struct{}, 5)
				var dlWg sync.WaitGroup
				for job := range jobs {
					dlWg.Add(1)
					go func(j dlJob) {
						defer dlWg.Done()
						sem <- struct{}{}
						defer func() { <-sem }()
						streams := adaptiveRangeStreams(j.size)
						b, _ := ce.a.downloadRanges(context.Background(), j.url, j.size, streams)
						totalBytes.Add(b)
					}(job)
				}
				dlWg.Wait()
			}(ce)
		}
		wg.Wait()
		d := time.Since(start)
		mbs := float64(totalBytes.Load()) / (1024 * 1024) / d.Seconds()
		fmt.Printf("  %-30s  %-12.2f  %-12.2f\n", "F: pipeline+adaptive+H1 5w", mbs, mbs/float64(len(clients)))
	}

	fmt.Println()
}

// ── Helpers ───────────────────────────────────────────────────────────────────

func randomData(size int) []byte {
	data := make([]byte, size)
	_, _ = rand.Read(data)
	return data
}

func printHeader(title string) {
	fmt.Println("═══════════════════════════════════════════════════════════════")
	fmt.Printf("  %s\n", title)
	fmt.Println("═══════════════════════════════════════════════════════════════")
}
