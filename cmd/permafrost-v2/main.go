// Package main implements permafrost-v2 — a fully-optimized OneDrive benchmark.
//
// Optimizations over v1 (permafrost-stress):
//   - Raw HTTP data path (no Microsoft Graph SDK overhead)
//   - 1MB read/write buffers (was 256KB)
//   - http2.ConfigureTransport for explicit HTTP/2 settings
//   - Chunked uploads for files >=4MB (10MB chunks aligned to 320KiB)
//   - Downloads via @microsoft.graph.downloadUrl (skips 302 redirect)
//   - Pre-warming to remove TLS handshake from benchmark timings
//   - RateLimit-* header tracking per response (Microsoft returns at >=80% usage)
//   - Decorrelated jitter backoff: sleep = min(cap, random(base, prev*3))
//   - Honors Retry-After on 429/503
//   - User-Agent: ISV|FairForge|Vaultaire/1.0 (prioritized by Microsoft)
//   - New: parallel download test
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
	graphBase  = "https://graph.microsoft.com/v1.0"
	userAgent  = "ISV|FairForge|Vaultaire/1.0"
	chunkAlign = 320 * 1024       // OneDrive upload chunks must be multiples of 320 KiB
	chunkSize  = 10 * 1024 * 1024 // 10MB chunks (already aligned: 10240 * 1024 = 32 * 320 * 1024)
	simpleMax  = 4 * 1024 * 1024  // Switch to chunked upload above this
)

// tunedTransport builds an http.Transport optimized for high-concurrency Graph API usage.
func tunedTransport() *http.Transport {
	t := &http.Transport{
		MaxIdleConns:        200,
		MaxIdleConnsPerHost: 200,
		IdleConnTimeout:     90 * time.Second,
		TLSHandshakeTimeout: 10 * time.Second,
		ReadBufferSize:      1 << 20, // 1MB
		WriteBufferSize:     1 << 20, // 1MB
		DisableCompression:  true,
		ForceAttemptHTTP2:   true,
		DialContext: (&net.Dialer{
			Timeout:   10 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12},
	}
	_ = http2.ConfigureTransport(t)
	return t
}

// ── Tenant config ─────────────────────────────────────────────────────────────

type tenant struct {
	name     string
	tenantID string
	clientID string
	secret   string
	userUPN  string
}

func loadTenants() []tenant {
	var tenants []tenant
	for i := 1; i <= 15; i++ {
		p := fmt.Sprintf("TENANT_%d_", i)
		if os.Getenv(p+"ID") == "" {
			continue
		}
		tenants = append(tenants, tenant{
			name:     fmt.Sprintf("tenant-%d", i),
			tenantID: os.Getenv(p + "ID"),
			clientID: os.Getenv(p + "CLIENT_ID"),
			secret:   os.Getenv(p + "SECRET"),
			userUPN:  os.Getenv(p + "USER"),
		})
	}
	return tenants
}

// ── authClient: raw HTTP with auth + retry + throttle handling ────────────────

type rateLimitState struct {
	limit      int
	remaining  int
	resetAt    time.Time
	lastUpdate time.Time
}

type authClient struct {
	t    tenant
	cred azcore.TokenCredential
	http *http.Client

	mu        sync.Mutex
	driveID   string
	rateLimit rateLimitState

	totalReqs     atomic.Int64
	totalRU       atomic.Int64
	throttledReq  atomic.Int64
	rateLimitSeen atomic.Int64
}

func newAuthClient(t tenant) (*authClient, error) {
	cred, err := azidentity.NewClientSecretCredential(t.tenantID, t.clientID, t.secret, nil)
	if err != nil {
		return nil, err
	}
	return &authClient{
		t:    t,
		cred: cred,
		http: &http.Client{Transport: tunedTransport()},
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

// decorrelatedJitter: sleep = min(cap, random_between(base, previous*3))
func decorrelatedJitter(base, previous, cap time.Duration) time.Duration {
	high := previous * 3
	if high > cap {
		high = cap
	}
	if high <= base {
		return base
	}
	span := int64(high - base)
	return base + time.Duration(mrand.Int64N(span))
}

func parseRetryAfter(resp *http.Response) time.Duration {
	v := resp.Header.Get("Retry-After")
	if v == "" {
		return 0
	}
	if s, err := strconv.Atoi(v); err == nil {
		return time.Duration(s) * time.Second
	}
	return 0
}

func (a *authClient) updateRateLimit(resp *http.Response) {
	seen := false
	limit := resp.Header.Get("RateLimit-Limit")
	remaining := resp.Header.Get("RateLimit-Remaining")
	reset := resp.Header.Get("RateLimit-Reset")
	if limit == "" && remaining == "" && reset == "" {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if n, err := strconv.Atoi(limit); err == nil {
		a.rateLimit.limit = n
		seen = true
	}
	if n, err := strconv.Atoi(remaining); err == nil {
		a.rateLimit.remaining = n
		seen = true
	}
	if n, err := strconv.Atoi(reset); err == nil {
		a.rateLimit.resetAt = time.Now().Add(time.Duration(n) * time.Second)
		seen = true
	}
	a.rateLimit.lastUpdate = time.Now()
	if seen {
		a.rateLimitSeen.Add(1)
	}
}

// do executes an authenticated HTTP request with throttle/retry handling.
// For body-bearing requests, pass a func via req.GetBody for safe retries.
func (a *authClient) do(ctx context.Context, req *http.Request, ruCost int) (*http.Response, error) {
	const maxAttempts = 5
	baseBackoff := 500 * time.Millisecond
	maxBackoff := 30 * time.Second
	prevBackoff := baseBackoff

	token, err := a.token(ctx)
	if err != nil {
		return nil, fmt.Errorf("get token: %w", err)
	}

	for attempt := 0; attempt < maxAttempts; attempt++ {
		if attempt > 0 && req.GetBody != nil {
			body, err := req.GetBody()
			if err != nil {
				return nil, fmt.Errorf("rewind body: %w", err)
			}
			req.Body = body
		}
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("User-Agent", userAgent)
		req.Header.Set("Accept", "application/json")

		resp, err := a.http.Do(req)
		a.totalReqs.Add(1)
		if err != nil {
			if attempt < maxAttempts-1 {
				prevBackoff = decorrelatedJitter(baseBackoff, prevBackoff, maxBackoff)
				time.Sleep(prevBackoff)
				continue
			}
			return nil, err
		}

		a.updateRateLimit(resp)
		a.totalRU.Add(int64(ruCost))

		if resp.StatusCode == 429 || resp.StatusCode == 503 {
			a.throttledReq.Add(1)
			retryAfter := parseRetryAfter(resp)
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			if attempt < maxAttempts-1 {
				wait := decorrelatedJitter(baseBackoff, prevBackoff, maxBackoff)
				if retryAfter > wait {
					wait = retryAfter
				}
				prevBackoff = wait
				time.Sleep(wait)
				continue
			}
			return nil, fmt.Errorf("throttled (%d) after %d attempts", resp.StatusCode, maxAttempts)
		}

		if resp.StatusCode >= 500 && resp.StatusCode < 600 {
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			if attempt < maxAttempts-1 {
				prevBackoff = decorrelatedJitter(baseBackoff, prevBackoff, maxBackoff)
				time.Sleep(prevBackoff)
				continue
			}
			return nil, fmt.Errorf("HTTP %d after %d attempts", resp.StatusCode, maxAttempts)
		}

		if resp.StatusCode >= 400 {
			b, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
			_ = resp.Body.Close()
			return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(b))
		}

		return resp, nil
	}
	return nil, fmt.Errorf("exhausted retries")
}

// ── Graph operations via raw HTTP ────────────────────────────────────────────

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
	resp, err := a.do(ctx, req, 1)
	if err != nil {
		return "", fmt.Errorf("list drives: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	var out struct {
		Value []struct {
			ID string `json:"id"`
		} `json:"value"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}
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
		"name":                              name,
		"folder":                            map[string]any{},
		"@microsoft.graph.conflictBehavior": "rename",
	})
	req, _ := http.NewRequestWithContext(ctx, "POST", u, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(body)), nil }
	resp, err := a.do(ctx, req, 2)
	if err != nil {
		return "", fmt.Errorf("create folder: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	var out struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}
	return out.ID, nil
}

func (a *authClient) deleteItem(ctx context.Context, driveID, itemID string) error {
	u := fmt.Sprintf("%s/drives/%s/items/%s", graphBase, driveID, itemID)
	req, _ := http.NewRequestWithContext(ctx, "DELETE", u, nil)
	resp, err := a.do(ctx, req, 2)
	if err != nil {
		return err
	}
	_ = resp.Body.Close()
	return nil
}

// simpleUpload: PUT directly to content endpoint. Works up to 250MB on personal OneDrive.
func (a *authClient) simpleUpload(ctx context.Context, driveID, parentID, name string, data []byte) error {
	u := fmt.Sprintf("%s/drives/%s/items/%s:/%s:/content",
		graphBase, driveID, parentID, url.PathEscape(name))
	req, _ := http.NewRequestWithContext(ctx, "PUT", u, bytes.NewReader(data))
	req.ContentLength = int64(len(data))
	req.Header.Set("Content-Type", "application/octet-stream")
	req.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(data)), nil }
	resp, err := a.do(ctx, req, 2)
	if err != nil {
		return err
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	return nil
}

// chunkedUpload: upload session with 10MB chunks aligned to 320 KiB.
// Chunk PUTs use the pre-authenticated uploadUrl — no Authorization header.
func (a *authClient) chunkedUpload(ctx context.Context, driveID, parentID, name string, data []byte) error {
	// Create upload session
	sessionURL := fmt.Sprintf("%s/drives/%s/items/%s:/%s:/createUploadSession",
		graphBase, driveID, parentID, url.PathEscape(name))
	sessionBody, _ := json.Marshal(map[string]any{
		"item": map[string]any{
			"@microsoft.graph.conflictBehavior": "replace",
		},
	})
	req, _ := http.NewRequestWithContext(ctx, "POST", sessionURL, bytes.NewReader(sessionBody))
	req.Header.Set("Content-Type", "application/json")
	req.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(sessionBody)), nil }
	resp, err := a.do(ctx, req, 2)
	if err != nil {
		return fmt.Errorf("create upload session: %w", err)
	}
	var session struct {
		UploadURL string `json:"uploadUrl"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&session); err != nil {
		_ = resp.Body.Close()
		return err
	}
	_ = resp.Body.Close()

	total := len(data)
	for offset := 0; offset < total; offset += chunkSize {
		end := offset + chunkSize
		if end > total {
			end = total
		}
		chunk := data[offset:end]
		if err := a.putChunk(ctx, session.UploadURL, chunk, offset, end-1, total); err != nil {
			return fmt.Errorf("chunk [%d-%d]: %w", offset, end-1, err)
		}
	}
	return nil
}

func (a *authClient) putChunk(ctx context.Context, uploadURL string, chunk []byte, start, endInc, total int) error {
	const maxAttempts = 3
	baseBackoff := 500 * time.Millisecond
	prev := baseBackoff
	for attempt := 0; attempt < maxAttempts; attempt++ {
		req, _ := http.NewRequestWithContext(ctx, "PUT", uploadURL, bytes.NewReader(chunk))
		req.ContentLength = int64(len(chunk))
		req.Header.Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, endInc, total))
		// Note: NO Authorization header — uploadURL is pre-authenticated
		resp, err := a.http.Do(req)
		a.totalReqs.Add(1)
		if err != nil {
			if attempt < maxAttempts-1 {
				prev = decorrelatedJitter(baseBackoff, prev, 30*time.Second)
				time.Sleep(prev)
				continue
			}
			return err
		}
		a.updateRateLimit(resp)
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			return nil
		}
		if resp.StatusCode == 429 || resp.StatusCode == 503 {
			a.throttledReq.Add(1)
			retryAfter := parseRetryAfter(resp)
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			if attempt < maxAttempts-1 {
				wait := decorrelatedJitter(baseBackoff, prev, 30*time.Second)
				if retryAfter > wait {
					wait = retryAfter
				}
				prev = wait
				time.Sleep(wait)
				continue
			}
			return fmt.Errorf("throttled on chunk")
		}
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		_ = resp.Body.Close()
		return fmt.Errorf("chunk HTTP %d: %s", resp.StatusCode, string(b))
	}
	return fmt.Errorf("chunk exhausted retries")
}

// upload picks simple or chunked based on size.
func (a *authClient) upload(ctx context.Context, driveID, parentID, name string, data []byte) error {
	if len(data) <= simpleMax {
		return a.simpleUpload(ctx, driveID, parentID, name, data)
	}
	return a.chunkedUpload(ctx, driveID, parentID, name, data)
}

// downloadViaURL: fetch metadata → pre-authenticated URL → stream directly.
// Skips the 302 redirect that /content GET uses. Zero SDK overhead.
func (a *authClient) downloadViaURL(ctx context.Context, driveID, parentID, name string) (int64, error) {
	// Step 1: item metadata (has @microsoft.graph.downloadUrl)
	metaURL := fmt.Sprintf("%s/drives/%s/items/%s:/%s",
		graphBase, driveID, parentID, url.PathEscape(name))
	req, _ := http.NewRequestWithContext(ctx, "GET", metaURL, nil)
	resp, err := a.do(ctx, req, 1)
	if err != nil {
		return 0, fmt.Errorf("metadata: %w", err)
	}
	var meta map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&meta); err != nil {
		_ = resp.Body.Close()
		return 0, err
	}
	_ = resp.Body.Close()

	dlURL, _ := meta["@microsoft.graph.downloadUrl"].(string)
	if dlURL == "" {
		return 0, fmt.Errorf("no downloadUrl in metadata")
	}

	// Step 2: direct stream download (no auth header)
	req2, _ := http.NewRequestWithContext(ctx, "GET", dlURL, nil)
	resp2, err := a.http.Do(req2)
	if err != nil {
		return 0, fmt.Errorf("stream: %w", err)
	}
	defer resp2.Body.Close() //nolint:errcheck
	if resp2.StatusCode >= 400 {
		return 0, fmt.Errorf("download HTTP %d", resp2.StatusCode)
	}
	return io.Copy(io.Discard, resp2.Body)
}

// preWarm establishes connections before timed benchmarks.
// First request pays TLS handshake; subsequent requests reuse the connection.
func (a *authClient) preWarm(ctx context.Context) error {
	// The getDriveID call does /me-style GET which establishes HTTP/2 connection
	_, err := a.getDriveID(ctx)
	return err
}

// ── Test harness ──────────────────────────────────────────────────────────────

type clientEntry struct {
	a       *authClient
	driveID string
	folder  string
}

type latencySample struct {
	duration time.Duration
	bytes    int
	err      bool
}

func main() {
	tenants := loadTenants()
	if len(tenants) == 0 {
		log.Fatal("No tenants. Set TENANT_1_ID, TENANT_1_CLIENT_ID, TENANT_1_SECRET, TENANT_1_USER")
	}

	fmt.Println("═══════════════════════════════════════════════════════════════")
	fmt.Println("  PERMAFROST v2 — FULLY OPTIMIZED BENCHMARK")
	fmt.Println("═══════════════════════════════════════════════════════════════")
	fmt.Printf("  Platform:    %s/%s\n", runtime.GOOS, runtime.GOARCH)
	fmt.Printf("  Tenants:     %d\n", len(tenants))
	fmt.Printf("  Transport:   1MB buffers, HTTP/2 explicit, 200 pool, compression off\n")
	fmt.Printf("  Upload:      simple PUT <=4MB, chunked session >4MB (10MB chunks)\n")
	fmt.Printf("  Download:    via @microsoft.graph.downloadUrl (no 302 redirect)\n")
	fmt.Printf("  Auth:        raw HTTP + azidentity (no Graph SDK)\n")
	fmt.Printf("  Throttle:    decorrelated jitter, Retry-After, User-Agent decorated\n")
	fmt.Println("═══════════════════════════════════════════════════════════════")
	fmt.Println()

	// Build and pre-warm one authenticated client per tenant
	var clients []clientEntry
	for _, t := range tenants {
		a, err := newAuthClient(t)
		if err != nil {
			log.Printf("[%s] SKIPPED — auth setup: %v", t.name, err)
			continue
		}
		ctx := context.Background()
		// Pre-warm (also fetches driveID)
		if err := a.preWarm(ctx); err != nil {
			log.Printf("[%s] SKIPPED — prewarm failed: %v", t.name, err)
			continue
		}
		driveID, _ := a.getDriveID(ctx)
		folderID, err := a.createFolder(ctx, driveID,
			fmt.Sprintf("permafrost-v2-%d", time.Now().UnixNano()))
		if err != nil {
			log.Printf("[%s] SKIPPED — folder creation: %v", t.name, err)
			continue
		}
		log.Printf("[%s] Ready (drive=%s..., folder=%s...)", t.name, driveID[:16], folderID[:16])
		clients = append(clients, clientEntry{a, driveID, folderID})
	}
	if len(clients) == 0 {
		log.Fatal("No tenants authenticated")
	}
	fmt.Printf("\n  %d/%d tenants active\n\n", len(clients), len(tenants))

	runFileSizeScaling(clients)
	runWorkerScaling(clients)
	runSequentialDownload(clients)
	runParallelDownload(clients)
	runMixedWorkload(clients)
	runThrottleStress(clients)
	runRawVsChunked(clients)
	runSummary(clients)

	// Async cleanup
	fmt.Println("\nCleaning up test folders...")
	var cleanWg sync.WaitGroup
	for _, ce := range clients {
		cleanWg.Add(1)
		go func(ce clientEntry) {
			defer cleanWg.Done()
			_ = ce.a.deleteItem(context.Background(), ce.driveID, ce.folder)
		}(ce)
	}
	cleanWg.Wait()
	fmt.Println("Done.")
}

// ── Test 1: File Size Scaling ─────────────────────────────────────────────────

func runFileSizeScaling(clients []clientEntry) {
	sizes := []int{1, 4, 10, 50, 100}
	filesPerSize := 5

	printHeader("TEST 1: File Size Scaling (simple PUT ≤4MB, chunked session >4MB)")
	fmt.Printf("  %-10s  %-14s  %-14s  %-10s  %-8s\n", "Size", "Avg MB/s", "Fleet MB/s", "Method", "Errors")
	fmt.Println("  ─────────────────────────────────────────────────────────────")

	for _, sizeMB := range sizes {
		data := randomData(sizeMB * 1024 * 1024)
		method := "simple"
		if sizeMB*1024*1024 > simpleMax {
			method = "chunked"
		}
		var totalMBs float64
		var totalErrors int

		for _, ce := range clients {
			mbs, errs := uploadN(context.Background(), ce.a, ce.driveID, ce.folder,
				fmt.Sprintf("size-%dmb", sizeMB), data, filesPerSize, 5)
			totalMBs += mbs
			totalErrors += errs
		}

		avgMBs := totalMBs / float64(len(clients))
		fmt.Printf("  %-10s  %-14.2f  %-14.2f  %-10s  %-8d\n",
			fmt.Sprintf("%d MB", sizeMB), avgMBs, totalMBs, method, totalErrors)
	}
	fmt.Println()
}

// ── Test 2: Worker Count Scaling ──────────────────────────────────────────────

func runWorkerScaling(clients []clientEntry) {
	workerCounts := []int{10, 25, 50, 75, 100}
	fileCount := 50
	data := randomData(1 * 1024 * 1024)

	printHeader("TEST 2: Worker Count Scaling (1MB files, 50 per tenant)")
	fmt.Printf("  %-10s  %-14s  %-14s  %-8s\n", "Workers", "Avg MB/s", "Fleet MB/s", "Errors")
	fmt.Println("  ─────────────────────────────────────────────────")

	for _, w := range workerCounts {
		var totalMBs float64
		var totalErrors int
		var mu sync.Mutex
		var wg sync.WaitGroup

		for _, ce := range clients {
			wg.Add(1)
			go func(ce clientEntry) {
				defer wg.Done()
				mbs, errs := uploadN(context.Background(), ce.a, ce.driveID, ce.folder,
					fmt.Sprintf("workers-%d", w), data, fileCount, w)
				mu.Lock()
				totalMBs += mbs
				totalErrors += errs
				mu.Unlock()
			}(ce)
		}
		wg.Wait()

		avgMBs := totalMBs / float64(len(clients))
		fmt.Printf("  %-10d  %-14.2f  %-14.2f  %-8d\n", w, avgMBs, totalMBs, totalErrors)
	}
	fmt.Println()
}

// ── Test 3: Sequential Download ───────────────────────────────────────────────

func runSequentialDownload(clients []clientEntry) {
	printHeader("TEST 3: Sequential Download (via @microsoft.graph.downloadUrl)")
	fmt.Printf("  %-20s  %-8s  %-10s  %-10s\n", "Tenant", "Size", "MB/s", "Status")
	fmt.Println("  ─────────────────────────────────────────────────")

	sizes := []int{1, 10, 50}

	for _, ce := range clients {
		for _, sizeMB := range sizes {
			data := randomData(sizeMB * 1024 * 1024)
			fileName := fmt.Sprintf("dl-seq-%dmb.bin", sizeMB)

			if err := ce.a.upload(context.Background(), ce.driveID, ce.folder, fileName, data); err != nil {
				fmt.Printf("  %-20s  %-8s  %-10s  ❌ upload\n",
					ce.a.t.name, fmt.Sprintf("%d MB", sizeMB), "-")
				continue
			}

			start := time.Now()
			n, err := ce.a.downloadViaURL(context.Background(), ce.driveID, ce.folder, fileName)
			d := time.Since(start)
			if err != nil {
				fmt.Printf("  %-20s  %-8s  %-10s  ❌ %v\n",
					ce.a.t.name, fmt.Sprintf("%d MB", sizeMB), "-", err)
				continue
			}
			mbs := float64(n) / (1024 * 1024) / d.Seconds()
			fmt.Printf("  %-20s  %-8s  %-10.2f  ✅\n",
				ce.a.t.name, fmt.Sprintf("%d MB", sizeMB), mbs)
		}
	}
	fmt.Println()
}

// ── Test 4: Parallel Download (NEW) ───────────────────────────────────────────

func runParallelDownload(clients []clientEntry) {
	printHeader("TEST 4: Parallel Download (NEW — 10 workers × 50MB files)")

	data := randomData(50 * 1024 * 1024)
	fileCount := 10

	// Seed files on each tenant first
	for _, ce := range clients {
		var seedWg sync.WaitGroup
		sem := make(chan struct{}, 5)
		for i := 0; i < fileCount; i++ {
			seedWg.Add(1)
			go func(n int) {
				defer seedWg.Done()
				sem <- struct{}{}
				defer func() { <-sem }()
				_ = ce.a.upload(context.Background(), ce.driveID, ce.folder,
					fmt.Sprintf("par-dl-%02d.bin", n), data)
			}(i)
		}
		seedWg.Wait()
	}
	fmt.Printf("  Seeded %d × 50MB files per tenant. Testing parallel download...\n\n", fileCount)

	fmt.Printf("  %-20s  %-10s  %-12s  %-10s\n", "Tenant", "Workers", "Fleet MB/s", "p95")
	fmt.Println("  ─────────────────────────────────────────────────")

	workerCounts := []int{1, 5, 10}
	for _, workers := range workerCounts {
		var totalBytes atomic.Int64
		var samples []latencySample
		var samplesMu sync.Mutex
		var wg sync.WaitGroup

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
						start := time.Now()
						b, err := ce.a.downloadViaURL(context.Background(),
							ce.driveID, ce.folder, fmt.Sprintf("par-dl-%02d.bin", n))
						d := time.Since(start)
						s := latencySample{duration: d, bytes: int(b), err: err != nil}
						if !s.err {
							totalBytes.Add(b)
						}
						samplesMu.Lock()
						samples = append(samples, s)
						samplesMu.Unlock()
					}(i)
				}
				innerWg.Wait()
			}(ce)
		}

		start := time.Now()
		wg.Wait()
		d := time.Since(start)

		mbs := float64(totalBytes.Load()) / (1024 * 1024) / d.Seconds()

		// p95
		var durations []time.Duration
		for _, s := range samples {
			if !s.err {
				durations = append(durations, s.duration)
			}
		}
		sort.Slice(durations, func(i, j int) bool { return durations[i] < durations[j] })
		p95 := time.Duration(0)
		if len(durations) > 0 {
			p95 = durations[len(durations)*95/100]
		}

		fmt.Printf("  %-20s  %-10d  %-12.2f  %s\n", "fleet", workers, mbs, p95.Round(time.Millisecond))

		// Reset samples for next worker count
		samples = nil
	}
	fmt.Println()
}

// ── Test 5: Mixed Workload ────────────────────────────────────────────────────

func runMixedWorkload(clients []clientEntry) {
	printHeader("TEST 5: Mixed Workload (upload all tenants + download tenant-0)")

	data := randomData(5 * 1024 * 1024)
	ce0 := clients[0]
	var seedFiles []string
	for i := 0; i < 10; i++ {
		name := fmt.Sprintf("mixed-seed-%04d.bin", i)
		if err := ce0.a.upload(context.Background(), ce0.driveID, ce0.folder, name, data); err == nil {
			seedFiles = append(seedFiles, name)
		}
	}
	fmt.Printf("  Seeded %d × 5MB files. Starting mixed workload...\n", len(seedFiles))

	var uploadMBs float64
	var downloadBytes atomic.Int64
	var uploadErrors, downloadErrors atomic.Int64
	var uploadMu sync.Mutex
	var wg sync.WaitGroup

	start := time.Now()
	for _, ce := range clients {
		wg.Add(1)
		go func(ce clientEntry) {
			defer wg.Done()
			mbs, errs := uploadN(context.Background(), ce.a, ce.driveID, ce.folder,
				"mixed-up", data, 20, 10)
			uploadMu.Lock()
			uploadMBs += mbs
			uploadMu.Unlock()
			uploadErrors.Add(int64(errs))
		}(ce)
	}

	wg.Add(1)
	go func() {
		defer wg.Done()
		for _, name := range seedFiles {
			n, err := ce0.a.downloadViaURL(context.Background(), ce0.driveID, ce0.folder, name)
			if err != nil {
				downloadErrors.Add(1)
			} else {
				downloadBytes.Add(n)
			}
		}
	}()

	wg.Wait()
	d := time.Since(start)
	downloadMBs := float64(downloadBytes.Load()) / (1024 * 1024) / d.Seconds()

	fmt.Printf("  Duration:       %s\n", d.Round(time.Millisecond))
	fmt.Printf("  Upload:         %.2f MB/s (%d errors)\n", uploadMBs, uploadErrors.Load())
	fmt.Printf("  Download:       %.2f MB/s (%d errors)\n", downloadMBs, downloadErrors.Load())
	fmt.Printf("  Aggregate:      %.2f MB/s\n", uploadMBs+downloadMBs)
	fmt.Println()
}

// ── Test 6: Throttle Stress with RateLimit header tracking ────────────────────

func runThrottleStress(clients []clientEntry) {
	printHeader("TEST 6: Throttle Stress (100 workers × 200 × 512KB, watch RateLimit headers)")

	data := randomData(512 * 1024)
	var samples []latencySample
	var mu sync.Mutex
	var throttled atomic.Int64

	ce := clients[0]
	beforeSeen := ce.a.rateLimitSeen.Load()
	sem := make(chan struct{}, 100)
	var wg sync.WaitGroup

	start := time.Now()
	for i := 0; i < 200; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			opStart := time.Now()
			err := ce.a.upload(context.Background(), ce.driveID, ce.folder,
				fmt.Sprintf("throttle-%04d.bin", n), data)
			s := latencySample{duration: time.Since(opStart)}
			if err != nil {
				s.err = true
				throttled.Add(1)
			}
			mu.Lock()
			samples = append(samples, s)
			mu.Unlock()
		}(i)
	}
	wg.Wait()
	total := time.Since(start)

	var durations []time.Duration
	for _, s := range samples {
		if !s.err {
			durations = append(durations, s.duration)
		}
	}
	sort.Slice(durations, func(i, j int) bool { return durations[i] < durations[j] })

	success := len(durations)
	ruUsed := success * 2
	ruPerMin := float64(ruUsed) / total.Minutes()
	afterSeen := ce.a.rateLimitSeen.Load() - beforeSeen

	fmt.Printf("  Duration:         %s\n", total.Round(time.Millisecond))
	fmt.Printf("  Successful:       %d / 200\n", success)
	fmt.Printf("  Throttled:        %d (429/503)\n", throttled.Load())
	fmt.Printf("  RateLimit hdrs:   %d responses (Microsoft sends at ≥80%% usage)\n", afterSeen)
	ce.a.mu.Lock()
	rl := ce.a.rateLimit
	ce.a.mu.Unlock()
	if rl.limit > 0 {
		fmt.Printf("  Last RL state:    limit=%d remaining=%d reset=%s\n",
			rl.limit, rl.remaining, time.Until(rl.resetAt).Round(time.Second))
	}
	if success > 0 {
		fmt.Printf("  p50 latency:      %s\n", durations[success*50/100].Round(time.Millisecond))
		fmt.Printf("  p95 latency:      %s\n", durations[success*95/100].Round(time.Millisecond))
		fmt.Printf("  p99 latency:      %s\n", durations[success*99/100].Round(time.Millisecond))
	}
	fmt.Printf("  Throughput:       %.2f MB/s\n", float64(success)*0.5/total.Seconds())
	fmt.Printf("  RU used:          ~%d\n", ruUsed)
	fmt.Printf("  RU rate:          ~%.0f/min (published limit: 1,250/min per app)\n", ruPerMin)
	if throttled.Load() == 0 {
		fmt.Println("  ✅ No throttling")
	} else {
		fmt.Println("  ⚠️  Throttling detected — retry handler absorbed it")
	}
	fmt.Println()
}

// ── Test 7: Simple vs Chunked A/B (for boundary files) ────────────────────────

func runRawVsChunked(clients []clientEntry) {
	printHeader("TEST 7: Simple PUT vs Chunked Session (10MB — right at boundary)")

	data := randomData(10 * 1024 * 1024)
	ce := clients[0]

	fmt.Printf("  Using tenant-1 only, 5 uploads each method...\n\n")
	fmt.Printf("  %-20s  %-10s  %-10s\n", "Method", "Duration", "MB/s")
	fmt.Println("  ─────────────────────────────────────")

	// Simple PUT (bypass boundary check)
	startSimple := time.Now()
	for i := 0; i < 5; i++ {
		_ = ce.a.simpleUpload(context.Background(), ce.driveID, ce.folder,
			fmt.Sprintf("ab-simple-%d.bin", i), data)
	}
	dSimple := time.Since(startSimple)
	mbsSimple := float64(5*10) / dSimple.Seconds()
	fmt.Printf("  %-20s  %-10s  %-10.2f\n", "simple PUT", dSimple.Round(time.Millisecond), mbsSimple)

	// Chunked session
	startChunked := time.Now()
	for i := 0; i < 5; i++ {
		_ = ce.a.chunkedUpload(context.Background(), ce.driveID, ce.folder,
			fmt.Sprintf("ab-chunk-%d.bin", i), data)
	}
	dChunked := time.Since(startChunked)
	mbsChunked := float64(5*10) / dChunked.Seconds()
	fmt.Printf("  %-20s  %-10s  %-10.2f\n", "chunked session", dChunked.Round(time.Millisecond), mbsChunked)

	diff := (mbsSimple - mbsChunked) / mbsChunked * 100
	fmt.Printf("\n  Simple is %+.1f%% %s chunked at 10MB\n",
		diff, map[bool]string{true: "faster than", false: "slower than"}[diff > 0])
	fmt.Println()
}

// ── Summary ───────────────────────────────────────────────────────────────────

func runSummary(clients []clientEntry) {
	printHeader("FLEET PROJECTION (15 tenants, linear scale)")
	var totalReqs, totalRU, totalThrottled, totalRLSeen int64
	for _, ce := range clients {
		totalReqs += ce.a.totalReqs.Load()
		totalRU += ce.a.totalRU.Load()
		totalThrottled += ce.a.throttledReq.Load()
		totalRLSeen += ce.a.rateLimitSeen.Load()
	}
	fmt.Printf("  Tested tenants:       %d\n", len(clients))
	fmt.Printf("  Projection factor:    %.1fx\n", 15.0/float64(len(clients)))
	fmt.Printf("  Free storage:         75 TB (15 × 5TB OneDrive)\n")
	fmt.Printf("  Total HTTP requests:  %d\n", totalReqs)
	fmt.Printf("  Total RU consumed:    %d\n", totalRU)
	fmt.Printf("  Total throttled:      %d\n", totalThrottled)
	fmt.Printf("  RateLimit hdrs seen:  %d (>=80%% usage indicator)\n", totalRLSeen)
	fmt.Printf("  Per-tenant avg:       %.1f req, %.1f RU, %.1f throttle\n",
		float64(totalReqs)/float64(len(clients)),
		float64(totalRU)/float64(len(clients)),
		float64(totalThrottled)/float64(len(clients)))
	fmt.Println()
}

// ── Helpers ───────────────────────────────────────────────────────────────────

func uploadN(ctx context.Context, a *authClient, driveID, folderID, prefix string,
	data []byte, count, workers int) (float64, int) {

	var errCount atomic.Int64
	sem := make(chan struct{}, workers)
	var wg sync.WaitGroup

	start := time.Now()
	for i := 0; i < count; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			if err := a.upload(ctx, driveID, folderID,
				fmt.Sprintf("%s-%04d.bin", prefix, n), data); err != nil {
				errCount.Add(1)
			}
		}(i)
	}
	wg.Wait()
	d := time.Since(start)

	errs := int(errCount.Load())
	successMB := float64((count-errs)*len(data)) / (1024 * 1024)
	return successMB / d.Seconds(), errs
}

func randomData(size int) []byte {
	data := make([]byte, size)
	if _, err := rand.Read(data); err != nil {
		log.Fatalf("rand: %v", err)
	}
	return data
}

func printHeader(title string) {
	fmt.Println("═══════════════════════════════════════════════════════════════")
	fmt.Printf("  %s\n", title)
	fmt.Println("═══════════════════════════════════════════════════════════════")
}
