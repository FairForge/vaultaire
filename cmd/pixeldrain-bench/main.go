// cmd/pixeldrain-bench/main.go
//
// Stress test for pixeldrain upload/download throughput.
// Tests sequential and concurrent workloads at various file sizes.
// Emits structured JSON for comparison with bench-compare results.
//
// Usage:
//
//	export PIXELDRAIN_API_KEY="your-api-key"
//	go run ./cmd/pixeldrain-bench                   # full matrix
//	go run ./cmd/pixeldrain-bench -smoke            # quick check
//	go run ./cmd/pixeldrain-bench -concurrent 32    # override concurrency
//
// Flags:
//
//	-out PATH       JSON output file (default bench-results/<host>-pixeldrain-<ts>.json)
//	-smoke          quick mode (1KB + 1MB only)
//	-concurrent N   max concurrent workers for stress tests (default 16)
//	-host NAME      override hostname label
package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"os"

	"golang.org/x/net/http2"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// — Result types (shared with bench-compare) ------------------------------------

type WorkloadResult struct {
	Name        string  `json:"name"`
	Description string  `json:"description,omitempty"`
	Bytes       int64   `json:"bytes,omitempty"`
	Ops         int     `json:"ops,omitempty"`
	Errors      int     `json:"errors,omitempty"`
	DurationMS  int64   `json:"duration_ms"`
	P50MS       int64   `json:"p50_ms,omitempty"`
	P95MS       int64   `json:"p95_ms,omitempty"`
	P99MS       int64   `json:"p99_ms,omitempty"`
	MaxMS       int64   `json:"max_ms,omitempty"`
	MBps        float64 `json:"mb_per_sec,omitempty"`
	OpsPerSec   float64 `json:"ops_per_sec,omitempty"`
	Note        string  `json:"note,omitempty"`
	Skipped     bool    `json:"skipped,omitempty"`
	Error       string  `json:"error,omitempty"`
}

type RunResult struct {
	Host        string           `json:"host"`
	OSArch      string           `json:"os_arch"`
	Provider    string           `json:"provider"`
	BaseURL     string           `json:"base_url"`
	StartedAt   string           `json:"started_at"`
	FinishedAt  string           `json:"finished_at"`
	DurationSec float64          `json:"duration_sec"`
	Smoke       bool             `json:"smoke"`
	Concurrency int              `json:"concurrency"`
	Workloads   []WorkloadResult `json:"workloads"`
}

// — Pixeldrain client -----------------------------------------------------------

const baseURL = "https://pixeldrain.com/api"

type pdClient struct {
	apiKey string
	http   *http.Client
}

type pdFileInfo struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Size       int64  `json:"size"`
	TotalViews int64  `json:"views"`
	SHA256Hash string `json:"hash_sha256"`
}

// devNull is a write sink that does NOT implement io.ReaderFrom.
// io.Discard implements ReaderFrom with 8KB internal pool buffers,
// which silently bypasses any buffer passed to io.CopyBuffer.
// This type forces CopyBuffer to use our 1MB pooled buffer.
type devNull struct{}

func (devNull) Write(p []byte) (int, error) { return len(p), nil }

var drain devNull

var copyPool = sync.Pool{New: func() any { b := make([]byte, 1<<20); return &b }}

func pooledDrain(src io.Reader) (int64, error) {
	bp := copyPool.Get().(*[]byte)
	defer copyPool.Put(bp)
	return io.CopyBuffer(drain, src, *bp)
}

type cyclicReader struct {
	block []byte
	pos   int
	limit int64
	read  int64
}

func newCyclicReader(limit int64) *cyclicReader {
	const blockSize = 16 << 20
	block := make([]byte, blockSize)
	_, _ = rand.Read(block)
	return &cyclicReader{block: block, limit: limit}
}

func (r *cyclicReader) Read(p []byte) (int, error) {
	if r.read >= r.limit {
		return 0, io.EOF
	}
	remaining := r.limit - r.read
	if int64(len(p)) > remaining {
		p = p[:remaining]
	}
	n := 0
	for n < len(p) {
		copied := copy(p[n:], r.block[r.pos:])
		n += copied
		r.pos = (r.pos + copied) % len(r.block)
	}
	r.read += int64(n)
	return n, nil
}

func newClient(apiKey string) *pdClient {
	transport := &http.Transport{
		MaxIdleConns:        200,
		MaxIdleConnsPerHost: 200,
		IdleConnTimeout:     90 * time.Second,
		TLSHandshakeTimeout: 10 * time.Second,
		ReadBufferSize:      1 << 20,
		WriteBufferSize:     1 << 20,
		DialContext: (&net.Dialer{
			Timeout:   10 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		DisableCompression: true,
		// Pixeldrain is HTTP/1.1 only (no HTTP/2 server support).
		// ForceAttemptHTTP2 + ConfigureTransport are harmless but unused.
		ForceAttemptHTTP2: true,
		TLSClientConfig: &tls.Config{
			MinVersion:         tls.VersionTLS12,
			ClientSessionCache: tls.NewLRUClientSessionCache(128),
		},
	}
	_ = http2.ConfigureTransport(transport)
	return &pdClient{
		apiKey: apiKey,
		http: &http.Client{
			Transport: transport,
			Timeout:   30 * time.Minute,
		},
	}
}

func (c *pdClient) upload(ctx context.Context, name string, data io.Reader, size int64) (*pdFileInfo, error) {
	url := fmt.Sprintf("%s/file/%s", baseURL, name)
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, url, data)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	req.SetBasicAuth("", c.apiKey)
	req.ContentLength = size
	req.Header.Set("Content-Type", "application/octet-stream")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("upload: %w", err)
	}
	defer func() { _ = resp.Body.Close() }() //nolint:errcheck

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("upload status %d: %s", resp.StatusCode, truncate(string(body), 200))
	}

	var info pdFileInfo
	if err := json.Unmarshal(body, &info); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	return &info, nil
}

func (c *pdClient) download(ctx context.Context, id string) (io.ReadCloser, int64, error) {
	url := fmt.Sprintf("%s/file/%s", baseURL, id)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, 0, fmt.Errorf("create request: %w", err)
	}
	req.SetBasicAuth("", c.apiKey)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("download: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close()
		return nil, 0, fmt.Errorf("download status %d", resp.StatusCode)
	}
	return resp.Body, resp.ContentLength, nil
}

func (c *pdClient) info(ctx context.Context, id string) (*pdFileInfo, error) {
	url := fmt.Sprintf("%s/file/%s/info", baseURL, id)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	req.SetBasicAuth("", c.apiKey)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("info: %w", err)
	}
	defer func() { _ = resp.Body.Close() }() //nolint:errcheck

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("info status %d", resp.StatusCode)
	}

	var fi pdFileInfo
	if err := json.NewDecoder(resp.Body).Decode(&fi); err != nil {
		return nil, fmt.Errorf("decode: %w", err)
	}
	return &fi, nil
}

func (c *pdClient) delete(ctx context.Context, id string) error {
	url := fmt.Sprintf("%s/file/%s", baseURL, id)
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, url, nil)
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}
	req.SetBasicAuth("", c.apiKey)

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("delete: %w", err)
	}
	_, _ = io.Copy(io.Discard, resp.Body) // drain body to enable connection reuse
	_ = resp.Body.Close()
	return nil
}

func (c *pdClient) downloadRange(ctx context.Context, id string, start, end int64) (int64, error) {
	url := fmt.Sprintf("%s/file/%s", baseURL, id)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, fmt.Errorf("create request: %w", err)
	}
	req.SetBasicAuth("", c.apiKey)
	req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", start, end))

	resp, err := c.http.Do(req)
	if err != nil {
		return 0, fmt.Errorf("range download: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusPartialContent && resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("range status %d", resp.StatusCode)
	}
	n, _ := pooledDrain(resp.Body)
	return n, nil
}

func (c *pdClient) speedtest(ctx context.Context, limit int64) (int64, error) {
	url := fmt.Sprintf("%s/misc/speedtest?limit=%d", baseURL, limit)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, err
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("speedtest status %d", resp.StatusCode)
	}
	n, _ := pooledDrain(resp.Body)
	return n, nil
}

func (c *pdClient) batchInfo(ctx context.Context, ids []string) (int, error) {
	joined := strings.Join(ids, ",")
	url := fmt.Sprintf("%s/file/%s/info", baseURL, joined)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, err
	}
	req.SetBasicAuth("", c.apiKey)

	resp, err := c.http.Do(req)
	if err != nil {
		return 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("batch info status %d", resp.StatusCode)
	}
	var infos []pdFileInfo
	if err := json.NewDecoder(resp.Body).Decode(&infos); err != nil {
		return 0, err
	}
	return len(infos), nil
}

type pdUserFile struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	Size         int64  `json:"size"`
	DateUpload   string `json:"date_upload"`
	DateLastView string `json:"date_last_view"`
}

type pdUserFilesResp struct {
	Files []pdUserFile `json:"files"`
}

func (c *pdClient) userFiles(ctx context.Context) (int, int64, error) {
	url := baseURL + "/user/files"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, 0, err
	}
	req.SetBasicAuth("", c.apiKey)

	resp, err := c.http.Do(req)
	if err != nil {
		return 0, 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return 0, 0, fmt.Errorf("user/files status %d: %s", resp.StatusCode, truncate(string(body), 200))
	}
	var result pdUserFilesResp
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return 0, 0, fmt.Errorf("decode: %w", err)
	}
	var totalBytes int64
	for _, f := range result.Files {
		totalBytes += f.Size
	}
	return len(result.Files), totalBytes, nil
}

type pdListCreateReq struct {
	Title     string         `json:"title"`
	Anonymous bool           `json:"anonymous"`
	Files     []pdListFileID `json:"files"`
}

type pdListFileID struct {
	ID string `json:"id"`
}

func (c *pdClient) createList(ctx context.Context, title string, ids []string) (string, error) {
	files := make([]pdListFileID, len(ids))
	for i, id := range ids {
		files[i] = pdListFileID{ID: id}
	}
	body, _ := json.Marshal(pdListCreateReq{Title: title, Anonymous: false, Files: files})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/list", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.SetBasicAuth("", c.apiKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	rbody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		return "", fmt.Errorf("create list status %d: %s", resp.StatusCode, truncate(string(rbody), 200))
	}
	var result struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(rbody, &result); err != nil {
		return "", err
	}
	return result.ID, nil
}

func (c *pdClient) getList(ctx context.Context, id string) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/list/"+id, nil)
	if err != nil {
		return 0, err
	}
	req.SetBasicAuth("", c.apiKey)

	resp, err := c.http.Do(req)
	if err != nil {
		return 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("get list status %d", resp.StatusCode)
	}
	var result struct {
		Files []json.RawMessage `json:"files"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return 0, err
	}
	return len(result.Files), nil
}

// — Workload context ------------------------------------------------------------

type wlContext struct {
	ctx         context.Context
	client      *pdClient
	concurrency int
	mu          sync.Mutex
	fileIDs     []string
}

func (c *wlContext) track(id string) {
	c.mu.Lock()
	c.fileIDs = append(c.fileIDs, id)
	c.mu.Unlock()
}

// — Random data helpers ---------------------------------------------------------

func randPayload(n int64) []byte {
	buf := make([]byte, n)
	_, _ = rand.Read(buf)
	return buf
}

func hashBytes(data []byte) string {
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:])
}

// — Workloads -------------------------------------------------------------------

type workload struct {
	name string
	fn   func(*wlContext) WorkloadResult
}

var allWorkloads = []workload{
	{"speedtest_1gb", wlSpeedtest},
	{"cold_dial_put_1kb", wlColdDial},
	{"warm_put_1kb", wlWarmPut1KB},
	{"warm_get_1kb", wlWarmGet1KB},
	{"info_1kb", wlInfo},
	{"put_1mb", wlPut1MB},
	{"get_1mb", wlGet1MB},
	{"put_10mb", wlPut10MB},
	{"get_10mb", wlGet10MB},
	{"put_100mb", wlPut100MB},
	{"get_100mb", wlGet100MB},
	{"put_1gb", wlPut1GB},
	{"get_1gb", wlGet1GB},
	{"range_parallel_1gb", wlRangeParallel},
	{"integrity_10mb", wlIntegrity},
	{"concurrent_upload_20s", wlConcurrentUpload},
	{"concurrent_upload_large_20s", wlConcurrentUploadLarge},
	{"concurrent_download_20s", wlConcurrentDownload},
	{"concurrent_download_large_20s", wlConcurrentDownloadLarge},
	{"batch_info", wlBatchInfo},
	{"burst_small_100", wlBurstSmall},
	{"user_files_list", wlUserFiles},
	{"list_roundtrip", wlListRoundtrip},
	{"cache_behavior", wlCacheBehavior},
	{"mixed_readwrite_20s", wlMixedReadWrite},
	{"soak_upload_2m", wlSoakUpload},
	{"soak_download_2m", wlSoakDownload},
}

var smokeWorkloads = map[string]bool{
	"cold_dial_put_1kb": true,
	"warm_put_1kb":      true,
	"warm_get_1kb":      true,
	"put_1mb":           true,
	"get_1mb":           true,
}

func wlColdDial(c *wlContext) WorkloadResult {
	data := randPayload(1024)
	start := time.Now()
	fi, err := c.client.upload(c.ctx, "bench-cold-1kb.bin", bytes.NewReader(data), 1024)
	dur := time.Since(start)
	if err != nil {
		return WorkloadResult{Name: "cold_dial_put_1kb", Error: err.Error(), DurationMS: dur.Milliseconds()}
	}
	c.track(fi.ID)
	return WorkloadResult{
		Name:       "cold_dial_put_1kb",
		Bytes:      1024,
		Ops:        1,
		DurationMS: dur.Milliseconds(),
		MBps:       mbps(1024, dur),
		Note:       fmt.Sprintf("id=%s", fi.ID),
	}
}

func wlWarmPut1KB(c *wlContext) WorkloadResult {
	return putN(c, "warm_put_1kb", 1024, 10)
}

func wlWarmGet1KB(c *wlContext) WorkloadResult {
	return getLatest(c, "warm_get_1kb", 1024)
}

func wlInfo(c *wlContext) WorkloadResult {
	c.mu.Lock()
	ids := append([]string{}, c.fileIDs...)
	c.mu.Unlock()
	if len(ids) == 0 {
		return WorkloadResult{Name: "info_1kb", Skipped: true, Note: "no files uploaded"}
	}

	var latencies []int64
	start := time.Now()
	for i := 0; i < 10; i++ {
		id := ids[len(ids)-1]
		t := time.Now()
		_, err := c.client.info(c.ctx, id)
		lat := time.Since(t).Milliseconds()
		if err != nil {
			return WorkloadResult{Name: "info_1kb", Error: err.Error(), DurationMS: time.Since(start).Milliseconds()}
		}
		latencies = append(latencies, lat)
	}
	dur := time.Since(start)
	return WorkloadResult{
		Name:       "info_1kb",
		Ops:        10,
		DurationMS: dur.Milliseconds(),
		P50MS:      percentile(latencies, 50),
		P95MS:      percentile(latencies, 95),
		P99MS:      percentile(latencies, 99),
		MaxMS:      percentile(latencies, 100),
		OpsPerSec:  float64(10) / dur.Seconds(),
	}
}

func wlPut1MB(c *wlContext) WorkloadResult  { return putN(c, "put_1mb", 1<<20, 5) }
func wlGet1MB(c *wlContext) WorkloadResult  { return getLatest(c, "get_1mb", 1<<20) }
func wlPut10MB(c *wlContext) WorkloadResult { return putN(c, "put_10mb", 10<<20, 3) }
func wlGet10MB(c *wlContext) WorkloadResult { return getLatest(c, "get_10mb", 10<<20) }

func wlPut100MB(c *wlContext) WorkloadResult {
	data := randPayload(100 << 20)
	start := time.Now()
	fi, err := c.client.upload(c.ctx, "bench-100mb.bin", bytes.NewReader(data), 100<<20)
	dur := time.Since(start)
	if err != nil {
		return WorkloadResult{Name: "put_100mb", Error: err.Error(), DurationMS: dur.Milliseconds()}
	}
	c.track(fi.ID)
	return WorkloadResult{
		Name:       "put_100mb",
		Bytes:      100 << 20,
		Ops:        1,
		DurationMS: dur.Milliseconds(),
		MBps:       mbps(100<<20, dur),
		Note:       fmt.Sprintf("id=%s", fi.ID),
	}
}

func wlGet100MB(c *wlContext) WorkloadResult {
	return getLatest(c, "get_100mb", 100<<20)
}

// wlPut1GB streams 1GB from rand.Reader — no heap allocation.
func wlPut1GB(c *wlContext) WorkloadResult {
	const sz = int64(1) << 30
	start := time.Now()
	fi, err := c.client.upload(c.ctx, "bench-1gb.bin", newCyclicReader(sz), sz)
	dur := time.Since(start)
	if err != nil {
		return WorkloadResult{Name: "put_1gb", Error: err.Error(), DurationMS: dur.Milliseconds()}
	}
	c.track(fi.ID)
	return WorkloadResult{
		Name:       "put_1gb",
		Bytes:      sz,
		Ops:        1,
		DurationMS: dur.Milliseconds(),
		MBps:       mbps(sz, dur),
		Note:       fmt.Sprintf("id=%s streaming", fi.ID),
	}
}

func wlGet1GB(c *wlContext) WorkloadResult {
	c.mu.Lock()
	ids := append([]string{}, c.fileIDs...)
	c.mu.Unlock()
	if len(ids) == 0 {
		return WorkloadResult{Name: "get_1gb", Skipped: true, Note: "no files to download"}
	}
	id := ids[len(ids)-1]
	start := time.Now()
	rc, _, err := c.client.download(c.ctx, id)
	if err != nil {
		return WorkloadResult{Name: "get_1gb", Error: err.Error(), DurationMS: time.Since(start).Milliseconds()}
	}
	n, _ := pooledDrain(rc)
	_ = rc.Close()
	dur := time.Since(start)
	return WorkloadResult{
		Name:       "get_1gb",
		Bytes:      n,
		Ops:        1,
		DurationMS: dur.Milliseconds(),
		MBps:       mbps(n, dur),
	}
}

// wlSpeedtest — raw network throughput from pixeldrain's speedtest endpoint.
// No auth overhead, no storage I/O — measures the network ceiling.
func wlSpeedtest(c *wlContext) WorkloadResult {
	const sz = int64(1) << 30
	start := time.Now()
	n, err := c.client.speedtest(c.ctx, sz)
	dur := time.Since(start)
	if err != nil {
		return WorkloadResult{Name: "speedtest_1gb", Error: err.Error(), DurationMS: dur.Milliseconds()}
	}
	return WorkloadResult{
		Name:       "speedtest_1gb",
		Bytes:      n,
		Ops:        1,
		DurationMS: dur.Milliseconds(),
		MBps:       mbps(n, dur),
		Note:       "raw network, no storage I/O",
	}
}

// wlRangeParallel — download a large file using 16 parallel HTTP Range requests.
// This is the download-accelerator pattern (like aria2/axel).
func wlRangeParallel(c *wlContext) WorkloadResult {
	c.mu.Lock()
	ids := append([]string{}, c.fileIDs...)
	c.mu.Unlock()
	if len(ids) == 0 {
		return WorkloadResult{Name: "range_parallel_1gb", Skipped: true, Note: "no files uploaded"}
	}

	id := ids[len(ids)-1]
	fi, err := c.client.info(c.ctx, id)
	if err != nil {
		return WorkloadResult{Name: "range_parallel_1gb", Error: "info: " + err.Error()}
	}
	if fi.Size < 1<<20 {
		return WorkloadResult{Name: "range_parallel_1gb", Skipped: true, Note: fmt.Sprintf("file too small (%d bytes)", fi.Size)}
	}

	const workers = 16
	chunkSize := fi.Size / workers

	var totalBytes atomic.Int64
	var errs atomic.Int32
	var latencies []int64
	var mu sync.Mutex

	start := time.Now()
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			rangeStart := int64(idx) * chunkSize
			rangeEnd := rangeStart + chunkSize - 1
			if idx == workers-1 {
				rangeEnd = fi.Size - 1
			}
			t := time.Now()
			n, err := c.client.downloadRange(c.ctx, id, rangeStart, rangeEnd)
			lat := time.Since(t).Milliseconds()
			if err != nil {
				errs.Add(1)
				return
			}
			totalBytes.Add(n)
			mu.Lock()
			latencies = append(latencies, lat)
			mu.Unlock()
		}(i)
	}
	wg.Wait()
	dur := time.Since(start)

	return WorkloadResult{
		Name:       "range_parallel_1gb",
		Bytes:      totalBytes.Load(),
		Ops:        workers - int(errs.Load()),
		Errors:     int(errs.Load()),
		DurationMS: dur.Milliseconds(),
		P50MS:      percentile(latencies, 50),
		P95MS:      percentile(latencies, 95),
		MBps:       mbps(totalBytes.Load(), dur),
		Note:       fmt.Sprintf("%d ranges on %s (%d MB)", workers, id, fi.Size>>20),
	}
}

// wlConcurrentUploadLarge — 64MB files × 16 workers for 20s.
// Tests whether larger per-file size amortizes connection overhead.
func wlConcurrentUploadLarge(c *wlContext) WorkloadResult {
	const (
		fileSize = 64 << 20
		maxDur   = 20 * time.Second
	)
	payload := randPayload(fileSize)
	deadline := time.Now().Add(maxDur)
	var totalOps int64
	var totalBytes int64
	var errs int64
	var latencies []int64
	var mu sync.Mutex

	sem := make(chan struct{}, c.concurrency)
	var wg sync.WaitGroup

	for time.Now().Before(deadline) {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			if time.Now().After(deadline) {
				return
			}
			fname := fmt.Sprintf("bench-conc-lg-%d.bin", time.Now().UnixNano())
			t := time.Now()
			fi, err := c.client.upload(c.ctx, fname, bytes.NewReader(payload), fileSize)
			lat := time.Since(t).Milliseconds()
			if err != nil {
				atomic.AddInt64(&errs, 1)
				return
			}
			c.track(fi.ID)
			atomic.AddInt64(&totalOps, 1)
			atomic.AddInt64(&totalBytes, fileSize)
			mu.Lock()
			latencies = append(latencies, lat)
			mu.Unlock()
		}()
	}
	wg.Wait()

	dur := time.Since(deadline.Add(-maxDur))
	return WorkloadResult{
		Name:        "concurrent_upload_large_20s",
		Description: fmt.Sprintf("%d × 64MB PUT, %d workers", totalOps, c.concurrency),
		Bytes:       totalBytes,
		Ops:         int(totalOps),
		Errors:      int(errs),
		DurationMS:  dur.Milliseconds(),
		P50MS:       percentile(latencies, 50),
		P95MS:       percentile(latencies, 95),
		P99MS:       percentile(latencies, 99),
		MaxMS:       percentile(latencies, 100),
		MBps:        mbps(totalBytes, dur),
		OpsPerSec:   float64(totalOps) / dur.Seconds(),
	}
}

// wlBatchInfo — query metadata for all tracked files in one request.
func wlBatchInfo(c *wlContext) WorkloadResult {
	c.mu.Lock()
	ids := append([]string{}, c.fileIDs...)
	c.mu.Unlock()
	if len(ids) > 100 {
		ids = ids[:100]
	}
	if len(ids) == 0 {
		return WorkloadResult{Name: "batch_info", Skipped: true, Note: "no files uploaded"}
	}

	start := time.Now()
	count, err := c.client.batchInfo(c.ctx, ids)
	dur := time.Since(start)
	if err != nil {
		return WorkloadResult{Name: "batch_info", Error: err.Error(), DurationMS: dur.Milliseconds()}
	}
	return WorkloadResult{
		Name:       "batch_info",
		Ops:        count,
		DurationMS: dur.Milliseconds(),
		OpsPerSec:  float64(count) / dur.Seconds(),
		Note:       fmt.Sprintf("queried %d, returned %d", len(ids), count),
	}
}

func wlIntegrity(c *wlContext) WorkloadResult {
	data := randPayload(10 << 20)
	expected := hashBytes(data)

	start := time.Now()
	fi, err := c.client.upload(c.ctx, "bench-integrity.bin", bytes.NewReader(data), 10<<20)
	if err != nil {
		return WorkloadResult{Name: "integrity_10mb", Error: err.Error(), DurationMS: time.Since(start).Milliseconds()}
	}
	c.track(fi.ID)

	rc, _, err := c.client.download(c.ctx, fi.ID)
	if err != nil {
		return WorkloadResult{Name: "integrity_10mb", Error: err.Error(), DurationMS: time.Since(start).Milliseconds()}
	}
	downloaded, err := io.ReadAll(rc)
	_ = rc.Close()
	if err != nil {
		return WorkloadResult{Name: "integrity_10mb", Error: err.Error(), DurationMS: time.Since(start).Milliseconds()}
	}
	dur := time.Since(start)

	got := hashBytes(downloaded)
	note := "SHA256 MATCH"
	errs := 0
	if got != expected {
		note = fmt.Sprintf("MISMATCH: want=%s got=%s", expected[:16], got[:16])
		errs = 1
	}

	return WorkloadResult{
		Name:       "integrity_10mb",
		Bytes:      10 << 20,
		Ops:        2,
		Errors:     errs,
		DurationMS: dur.Milliseconds(),
		MBps:       mbps(20<<20, dur),
		Note:       note,
	}
}

func wlConcurrentUpload(c *wlContext) WorkloadResult {
	return concurrentWork(c, "concurrent_upload_20s", 20*time.Second, true)
}

func wlConcurrentDownload(c *wlContext) WorkloadResult {
	data := randPayload(1 << 20)
	fi, err := c.client.upload(c.ctx, "bench-concurrent-src.bin", bytes.NewReader(data), 1<<20)
	if err != nil {
		return WorkloadResult{Name: "concurrent_download_20s", Error: err.Error()}
	}
	c.track(fi.ID)

	return concurrentDownloadWork(c, "concurrent_download_20s", 20*time.Second, fi.ID)
}

func wlConcurrentDownloadLarge(c *wlContext) WorkloadResult {
	const fileSize = 64 << 20
	fi, err := c.client.upload(c.ctx, "bench-concdl-lg-src.bin", newCyclicReader(fileSize), fileSize)
	if err != nil {
		return WorkloadResult{Name: "concurrent_download_large_20s", Error: err.Error()}
	}
	c.track(fi.ID)
	return concurrentDownloadWork(c, "concurrent_download_large_20s", 20*time.Second, fi.ID)
}

func wlBurstSmall(c *wlContext) WorkloadResult {
	const n = 100
	const size = 4096
	var totalBytes int64
	var errs int32
	var latencies []int64
	var mu sync.Mutex

	start := time.Now()
	sem := make(chan struct{}, c.concurrency)
	var wg sync.WaitGroup

	for i := 0; i < n; i++ {
		wg.Add(1)
		sem <- struct{}{}
		go func(idx int) {
			defer wg.Done()
			defer func() { <-sem }()

			data := randPayload(size)
			name := fmt.Sprintf("bench-burst-%04d.bin", idx)
			t := time.Now()
			fi, err := c.client.upload(c.ctx, name, bytes.NewReader(data), size)
			lat := time.Since(t).Milliseconds()
			if err != nil {
				atomic.AddInt32(&errs, 1)
				return
			}
			c.track(fi.ID)
			atomic.AddInt64(&totalBytes, size)
			mu.Lock()
			latencies = append(latencies, lat)
			mu.Unlock()
		}(i)
	}
	wg.Wait()
	dur := time.Since(start)

	return WorkloadResult{
		Name:       "burst_small_100",
		Bytes:      totalBytes,
		Ops:        n,
		Errors:     int(errs),
		DurationMS: dur.Milliseconds(),
		P50MS:      percentile(latencies, 50),
		P95MS:      percentile(latencies, 95),
		P99MS:      percentile(latencies, 99),
		MaxMS:      percentile(latencies, 100),
		MBps:       mbps(totalBytes, dur),
		OpsPerSec:  float64(n-int(errs)) / dur.Seconds(),
	}
}

func wlUserFiles(c *wlContext) WorkloadResult {
	start := time.Now()
	count, totalBytes, err := c.client.userFiles(c.ctx)
	dur := time.Since(start)
	if err != nil {
		return WorkloadResult{Name: "user_files_list", Error: err.Error(), DurationMS: dur.Milliseconds()}
	}
	return WorkloadResult{
		Name:       "user_files_list",
		Ops:        count,
		DurationMS: dur.Milliseconds(),
		OpsPerSec:  float64(count) / dur.Seconds(),
		Note:       fmt.Sprintf("%d files, %d MB total", count, totalBytes>>20),
	}
}

func wlListRoundtrip(c *wlContext) WorkloadResult {
	c.mu.Lock()
	ids := append([]string{}, c.fileIDs...)
	c.mu.Unlock()
	if len(ids) == 0 {
		return WorkloadResult{Name: "list_roundtrip", Skipped: true, Note: "no files"}
	}
	if len(ids) > 100 {
		ids = ids[:100]
	}

	start := time.Now()
	listID, err := c.client.createList(c.ctx, "bench-list", ids)
	if err != nil {
		return WorkloadResult{Name: "list_roundtrip", Error: "create: " + err.Error(), DurationMS: time.Since(start).Milliseconds()}
	}
	createDur := time.Since(start)

	getStart := time.Now()
	fileCount, err := c.client.getList(c.ctx, listID)
	if err != nil {
		return WorkloadResult{Name: "list_roundtrip", Error: "get: " + err.Error(), DurationMS: time.Since(start).Milliseconds()}
	}
	getDur := time.Since(getStart)
	dur := time.Since(start)

	return WorkloadResult{
		Name:       "list_roundtrip",
		Ops:        fileCount,
		DurationMS: dur.Milliseconds(),
		OpsPerSec:  float64(fileCount) / dur.Seconds(),
		Note:       fmt.Sprintf("create=%dms get=%dms list=%s %d files", createDur.Milliseconds(), getDur.Milliseconds(), listID, fileCount),
	}
}

func wlCacheBehavior(c *wlContext) WorkloadResult {
	const fileSize = 10 << 20
	data := randPayload(fileSize)
	fi, err := c.client.upload(c.ctx, "bench-cache-probe.bin", bytes.NewReader(data), fileSize)
	if err != nil {
		return WorkloadResult{Name: "cache_behavior", Error: err.Error()}
	}
	c.track(fi.ID)

	fetch := func(label string) (int64, error) {
		rc, _, err := c.client.download(c.ctx, fi.ID)
		if err != nil {
			return 0, err
		}
		_, _ = pooledDrain(rc)
		_ = rc.Close()
		return 0, nil
	}

	t0 := time.Now()
	if _, err := fetch("cold"); err != nil {
		return WorkloadResult{Name: "cache_behavior", Error: "cold: " + err.Error()}
	}
	coldMS := time.Since(t0).Milliseconds()

	t1 := time.Now()
	if _, err := fetch("warm1"); err != nil {
		return WorkloadResult{Name: "cache_behavior", Error: "warm1: " + err.Error()}
	}
	warm1MS := time.Since(t1).Milliseconds()

	t2 := time.Now()
	if _, err := fetch("warm2"); err != nil {
		return WorkloadResult{Name: "cache_behavior", Error: "warm2: " + err.Error()}
	}
	warm2MS := time.Since(t2).Milliseconds()

	dur := coldMS + warm1MS + warm2MS
	note := fmt.Sprintf("cold=%dms warm1=%dms warm2=%dms", coldMS, warm1MS, warm2MS)
	if warm2MS > 0 && warm2MS < coldMS*70/100 {
		note += " CACHE_HIT"
	}

	return WorkloadResult{
		Name:       "cache_behavior",
		Bytes:      3 * fileSize,
		Ops:        3,
		DurationMS: dur,
		MBps:       mbps(3*fileSize, time.Duration(dur)*time.Millisecond),
		Note:       note,
	}
}

func wlMixedReadWrite(c *wlContext) WorkloadResult {
	const (
		fileSize = 4 << 20
		duration = 20 * time.Second
	)
	fi, err := c.client.upload(c.ctx, "bench-mixed-src.bin", newCyclicReader(fileSize), fileSize)
	if err != nil {
		return WorkloadResult{Name: "mixed_readwrite_20s", Error: err.Error()}
	}
	c.track(fi.ID)

	deadline := time.Now().Add(duration)
	halfW := c.concurrency / 2
	if halfW < 1 {
		halfW = 1
	}

	var uploadBytes, downloadBytes int64
	var uploadOps, downloadOps, errs int64

	var wg sync.WaitGroup

	upSem := make(chan struct{}, halfW)
	wg.Add(1)
	go func() {
		defer wg.Done()
		var inner sync.WaitGroup
		for time.Now().Before(deadline) {
			inner.Add(1)
			upSem <- struct{}{}
			go func() {
				defer inner.Done()
				defer func() { <-upSem }()
				if time.Now().After(deadline) {
					return
				}
				fname := fmt.Sprintf("bench-mix-%d.bin", time.Now().UnixNano())
				ufi, uerr := c.client.upload(c.ctx, fname, newCyclicReader(fileSize), fileSize)
				if uerr != nil {
					atomic.AddInt64(&errs, 1)
					return
				}
				c.track(ufi.ID)
				atomic.AddInt64(&uploadOps, 1)
				atomic.AddInt64(&uploadBytes, fileSize)
			}()
		}
		inner.Wait()
	}()

	dnSem := make(chan struct{}, halfW)
	wg.Add(1)
	go func() {
		defer wg.Done()
		var inner sync.WaitGroup
		for time.Now().Before(deadline) {
			inner.Add(1)
			dnSem <- struct{}{}
			go func() {
				defer inner.Done()
				defer func() { <-dnSem }()
				if time.Now().After(deadline) {
					return
				}
				rc, _, derr := c.client.download(c.ctx, fi.ID)
				if derr != nil {
					atomic.AddInt64(&errs, 1)
					return
				}
				n, _ := pooledDrain(rc)
				_ = rc.Close()
				atomic.AddInt64(&downloadOps, 1)
				atomic.AddInt64(&downloadBytes, n)
			}()
		}
		inner.Wait()
	}()

	wg.Wait()
	dur := time.Since(deadline.Add(-duration))
	totalBytes := uploadBytes + downloadBytes

	return WorkloadResult{
		Name:        "mixed_readwrite_20s",
		Description: fmt.Sprintf("up=%d×4MB dn=%d×4MB, %d+%d workers", uploadOps, downloadOps, halfW, halfW),
		Bytes:       totalBytes,
		Ops:         int(uploadOps + downloadOps),
		Errors:      int(errs),
		DurationMS:  dur.Milliseconds(),
		MBps:        mbps(totalBytes, dur),
		OpsPerSec:   float64(uploadOps+downloadOps) / dur.Seconds(),
		Note:        fmt.Sprintf("up=%.1fMB/s(%d) dn=%.1fMB/s(%d)", mbps(uploadBytes, dur), uploadOps, mbps(downloadBytes, dur), downloadOps),
	}
}

func wlSoakUpload(c *wlContext) WorkloadResult {
	const (
		fileSize    = 64 << 20
		duration    = 2 * time.Minute
		windowDur   = 30 * time.Second
		windowCount = 4
	)

	deadline := time.Now().Add(duration)
	startTime := time.Now()
	windowBytes := make([]int64, windowCount)
	var totalOps, totalBytes, errs int64

	sem := make(chan struct{}, c.concurrency)
	var wg sync.WaitGroup

	for time.Now().Before(deadline) {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			if time.Now().After(deadline) {
				return
			}
			fname := fmt.Sprintf("bench-soak-up-%d.bin", time.Now().UnixNano())
			fi, uerr := c.client.upload(c.ctx, fname, newCyclicReader(fileSize), fileSize)
			if uerr != nil {
				atomic.AddInt64(&errs, 1)
				return
			}
			c.track(fi.ID)
			widx := int(time.Since(startTime) / windowDur)
			if widx >= windowCount {
				widx = windowCount - 1
			}
			atomic.AddInt64(&windowBytes[widx], fileSize)
			atomic.AddInt64(&totalOps, 1)
			atomic.AddInt64(&totalBytes, fileSize)
		}()
	}
	wg.Wait()
	dur := time.Since(startTime)

	var parts []string
	for i := 0; i < windowCount; i++ {
		w := float64(windowBytes[i]) / 1024.0 / 1024.0 / windowDur.Seconds()
		parts = append(parts, fmt.Sprintf("%.0f", w))
	}
	note := fmt.Sprintf("30s-windows: [%s] MB/s", strings.Join(parts, ", "))
	if windowBytes[0] > 0 && windowBytes[windowCount-1] > 0 {
		ratio := float64(windowBytes[windowCount-1]) / float64(windowBytes[0])
		if ratio < 0.7 {
			note += " THROTTLED"
		} else {
			note += " STABLE"
		}
	}

	return WorkloadResult{
		Name:        "soak_upload_2m",
		Description: fmt.Sprintf("%d × 64MB PUT, %d workers, 2 min", totalOps, c.concurrency),
		Bytes:       totalBytes,
		Ops:         int(totalOps),
		Errors:      int(errs),
		DurationMS:  dur.Milliseconds(),
		MBps:        mbps(totalBytes, dur),
		OpsPerSec:   float64(totalOps) / dur.Seconds(),
		Note:        note,
	}
}

func wlSoakDownload(c *wlContext) WorkloadResult {
	const (
		fileSize    = 64 << 20
		duration    = 2 * time.Minute
		windowDur   = 30 * time.Second
		windowCount = 4
	)

	fi, err := c.client.upload(c.ctx, "bench-soak-dn-src.bin", newCyclicReader(fileSize), fileSize)
	if err != nil {
		return WorkloadResult{Name: "soak_download_2m", Error: err.Error()}
	}
	c.track(fi.ID)

	deadline := time.Now().Add(duration)
	startTime := time.Now()
	windowBytes := make([]int64, windowCount)
	var totalOps, totalBytes, errs int64

	sem := make(chan struct{}, c.concurrency)
	var wg sync.WaitGroup

	for time.Now().Before(deadline) {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			if time.Now().After(deadline) {
				return
			}
			rc, _, derr := c.client.download(c.ctx, fi.ID)
			if derr != nil {
				atomic.AddInt64(&errs, 1)
				return
			}
			n, _ := pooledDrain(rc)
			_ = rc.Close()

			widx := int(time.Since(startTime) / windowDur)
			if widx >= windowCount {
				widx = windowCount - 1
			}
			atomic.AddInt64(&windowBytes[widx], n)
			atomic.AddInt64(&totalOps, 1)
			atomic.AddInt64(&totalBytes, n)
		}()
	}
	wg.Wait()
	dur := time.Since(startTime)

	var parts []string
	for i := 0; i < windowCount; i++ {
		w := float64(windowBytes[i]) / 1024.0 / 1024.0 / windowDur.Seconds()
		parts = append(parts, fmt.Sprintf("%.0f", w))
	}
	note := fmt.Sprintf("30s-windows: [%s] MB/s", strings.Join(parts, ", "))
	if windowBytes[0] > 0 && windowBytes[windowCount-1] > 0 {
		ratio := float64(windowBytes[windowCount-1]) / float64(windowBytes[0])
		if ratio < 0.7 {
			note += " THROTTLED"
		} else {
			note += " STABLE"
		}
	}

	return WorkloadResult{
		Name:        "soak_download_2m",
		Description: fmt.Sprintf("%d × 64MB GET, %d workers, 2 min", totalOps, c.concurrency),
		Bytes:       totalBytes,
		Ops:         int(totalOps),
		Errors:      int(errs),
		DurationMS:  dur.Milliseconds(),
		MBps:        mbps(totalBytes, dur),
		OpsPerSec:   float64(totalOps) / dur.Seconds(),
		Note:        note,
	}
}

// — Helpers for common workload patterns ----------------------------------------

func putN(c *wlContext, name string, size int64, n int) WorkloadResult {
	var latencies []int64
	var totalBytes int64
	start := time.Now()

	for i := 0; i < n; i++ {
		data := randPayload(size)
		fname := fmt.Sprintf("bench-%s-%d.bin", name, i)
		t := time.Now()
		fi, err := c.client.upload(c.ctx, fname, bytes.NewReader(data), size)
		lat := time.Since(t).Milliseconds()
		if err != nil {
			return WorkloadResult{Name: name, Error: err.Error(), DurationMS: time.Since(start).Milliseconds()}
		}
		c.track(fi.ID)
		latencies = append(latencies, lat)
		totalBytes += size
	}
	dur := time.Since(start)
	return WorkloadResult{
		Name:       name,
		Bytes:      totalBytes,
		Ops:        n,
		DurationMS: dur.Milliseconds(),
		P50MS:      percentile(latencies, 50),
		P95MS:      percentile(latencies, 95),
		MaxMS:      percentile(latencies, 100),
		MBps:       mbps(totalBytes, dur),
		OpsPerSec:  float64(n) / dur.Seconds(),
	}
}

func getLatest(c *wlContext, name string, expectedSize int64) WorkloadResult {
	c.mu.Lock()
	ids := append([]string{}, c.fileIDs...)
	c.mu.Unlock()
	if len(ids) == 0 {
		return WorkloadResult{Name: name, Skipped: true, Note: "no files to download"}
	}

	id := ids[len(ids)-1]
	const n = 5
	var latencies []int64
	var totalBytes int64
	start := time.Now()

	for i := 0; i < n; i++ {
		t := time.Now()
		rc, _, err := c.client.download(c.ctx, id)
		if err != nil {
			return WorkloadResult{Name: name, Error: err.Error(), DurationMS: time.Since(start).Milliseconds()}
		}
		nn, _ := pooledDrain(rc)
		_ = rc.Close()
		lat := time.Since(t).Milliseconds()
		latencies = append(latencies, lat)
		totalBytes += nn
	}
	dur := time.Since(start)
	return WorkloadResult{
		Name:       name,
		Bytes:      totalBytes,
		Ops:        n,
		DurationMS: dur.Milliseconds(),
		P50MS:      percentile(latencies, 50),
		P95MS:      percentile(latencies, 95),
		MaxMS:      percentile(latencies, 100),
		MBps:       mbps(totalBytes, dur),
		OpsPerSec:  float64(n) / dur.Seconds(),
	}
}

func concurrentWork(c *wlContext, name string, duration time.Duration, upload bool) WorkloadResult {
	const fileSize = 1 << 20 // 1MB per op
	deadline := time.Now().Add(duration)
	var totalOps int64
	var totalBytes int64
	var errs int64
	var latencies []int64
	var mu sync.Mutex

	sem := make(chan struct{}, c.concurrency)
	var wg sync.WaitGroup

	for time.Now().Before(deadline) {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()

			if time.Now().After(deadline) {
				return
			}

			data := randPayload(fileSize)
			fname := fmt.Sprintf("bench-conc-%d.bin", time.Now().UnixNano())
			t := time.Now()
			fi, err := c.client.upload(c.ctx, fname, bytes.NewReader(data), fileSize)
			lat := time.Since(t).Milliseconds()
			if err != nil {
				atomic.AddInt64(&errs, 1)
				return
			}
			c.track(fi.ID)
			atomic.AddInt64(&totalOps, 1)
			atomic.AddInt64(&totalBytes, fileSize)
			mu.Lock()
			latencies = append(latencies, lat)
			mu.Unlock()
		}()
	}
	wg.Wait()

	dur := time.Since(deadline.Add(-duration))
	return WorkloadResult{
		Name:       name,
		Bytes:      totalBytes,
		Ops:        int(totalOps),
		Errors:     int(errs),
		DurationMS: dur.Milliseconds(),
		P50MS:      percentile(latencies, 50),
		P95MS:      percentile(latencies, 95),
		P99MS:      percentile(latencies, 99),
		MaxMS:      percentile(latencies, 100),
		MBps:       mbps(totalBytes, dur),
		OpsPerSec:  float64(totalOps) / dur.Seconds(),
	}
}

func concurrentDownloadWork(c *wlContext, name string, duration time.Duration, fileID string) WorkloadResult {
	deadline := time.Now().Add(duration)
	var totalOps int64
	var totalBytes int64
	var errs int64
	var latencies []int64
	var mu sync.Mutex

	sem := make(chan struct{}, c.concurrency)
	var wg sync.WaitGroup

	for time.Now().Before(deadline) {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()

			if time.Now().After(deadline) {
				return
			}

			t := time.Now()
			rc, _, err := c.client.download(c.ctx, fileID)
			if err != nil {
				atomic.AddInt64(&errs, 1)
				return
			}
			n, _ := pooledDrain(rc)
			_ = rc.Close()
			lat := time.Since(t).Milliseconds()

			atomic.AddInt64(&totalOps, 1)
			atomic.AddInt64(&totalBytes, n)
			mu.Lock()
			latencies = append(latencies, lat)
			mu.Unlock()
		}()
	}
	wg.Wait()

	dur := time.Since(deadline.Add(-duration))
	return WorkloadResult{
		Name:       name,
		Bytes:      totalBytes,
		Ops:        int(totalOps),
		Errors:     int(errs),
		DurationMS: dur.Milliseconds(),
		P50MS:      percentile(latencies, 50),
		P95MS:      percentile(latencies, 95),
		P99MS:      percentile(latencies, 99),
		MaxMS:      percentile(latencies, 100),
		MBps:       mbps(totalBytes, dur),
		OpsPerSec:  float64(totalOps) / dur.Seconds(),
	}
}

// — Stats helpers ---------------------------------------------------------------

func percentile(sorted []int64, pct int) int64 {
	if len(sorted) == 0 {
		return 0
	}
	s := make([]int64, len(sorted))
	copy(s, sorted)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	idx := int(math.Ceil(float64(pct)/100.0*float64(len(s)))) - 1
	if idx < 0 {
		idx = 0
	}
	if idx >= len(s) {
		idx = len(s) - 1
	}
	return s[idx]
}

func mbps(bytes int64, d time.Duration) float64 {
	if d <= 0 {
		return 0
	}
	return float64(bytes) / 1024.0 / 1024.0 / d.Seconds()
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// — Output helpers --------------------------------------------------------------

func oneLine(w WorkloadResult) string {
	var b strings.Builder
	if w.Error != "" {
		fmt.Fprintf(&b, "❌ %-28s  %s", w.Name, truncate(w.Error, 60))
		return b.String()
	}
	if w.Skipped {
		fmt.Fprintf(&b, "⏭  %-28s  %s", w.Name, w.Note)
		return b.String()
	}
	fmt.Fprintf(&b, "✓  %-28s  %6dms", w.Name, w.DurationMS)
	if w.MBps > 0 {
		fmt.Fprintf(&b, "  %7.1f MB/s", w.MBps)
	}
	if w.OpsPerSec > 0 {
		fmt.Fprintf(&b, "  %6.1f ops/s", w.OpsPerSec)
	}
	if w.Errors > 0 {
		fmt.Fprintf(&b, "  (%d errs)", w.Errors)
	}
	if w.Note != "" {
		fmt.Fprintf(&b, "  [%s]", w.Note)
	}
	return b.String()
}

func writeJSON(path string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

func sanitize(s string) string {
	out := strings.Builder{}
	for _, r := range strings.ToLower(s) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' {
			out.WriteRune(r)
		}
	}
	return out.String()
}

// — Main ------------------------------------------------------------------------

func main() {
	var (
		outFile     = flag.String("out", "", "JSON output file")
		smoke       = flag.Bool("smoke", false, "Quick mode (small workloads only)")
		concurrency = flag.Int("concurrent", 16, "Max concurrent workers")
		host        = flag.String("host", "", "Host label")
	)
	flag.Parse()

	apiKey := os.Getenv("PIXELDRAIN_API_KEY")
	if apiKey == "" {
		fmt.Fprintln(os.Stderr, "PIXELDRAIN_API_KEY not set")
		os.Exit(1)
	}

	hostName := *host
	if hostName == "" {
		h, _ := os.Hostname()
		if h == "" {
			h = "unknown"
		}
		hostName = h
	}

	if *outFile == "" {
		ts := time.Now().Format("20060102-150405")
		*outFile = filepath.Join("bench-results", fmt.Sprintf("%s-pixeldrain-%s.json", sanitize(hostName), ts))
	}
	if err := os.MkdirAll(filepath.Dir(*outFile), 0o750); err != nil {
		fmt.Fprintf(os.Stderr, "mkdir: %v\n", err)
		os.Exit(1)
	}

	client := newClient(apiKey)

	fmt.Printf("Host:        %s (%s/%s)\n", hostName, runtime.GOOS, runtime.GOARCH)
	fmt.Printf("Provider:    pixeldrain\n")
	fmt.Printf("Output:      %s\n", *outFile)
	fmt.Printf("Concurrency: %d\n", *concurrency)
	fmt.Printf("Smoke:       %v\n", *smoke)
	fmt.Println("─────────────────────────────────────────────────────────────────")

	run := RunResult{
		Host:        hostName,
		OSArch:      runtime.GOOS + "/" + runtime.GOARCH,
		Provider:    "pixeldrain",
		BaseURL:     baseURL,
		StartedAt:   time.Now().UTC().Format(time.RFC3339),
		Smoke:       *smoke,
		Concurrency: *concurrency,
	}
	overall := time.Now()

	ctx := context.Background()
	wlc := &wlContext{ctx: ctx, client: client, concurrency: *concurrency}

	fmt.Printf("Warming up %d connections... ", *concurrency)
	var warmWG sync.WaitGroup
	for i := 0; i < *concurrency; i++ {
		warmWG.Add(1)
		go func() {
			defer warmWG.Done()
			_, _ = client.speedtest(ctx, 1)
		}()
	}
	warmWG.Wait()
	fmt.Println("ok")

	for _, w := range allWorkloads {
		if *smoke && !smokeWorkloads[w.name] {
			continue
		}
		wl := w.fn(wlc)
		fmt.Printf("  %s\n", oneLine(wl))
		run.Workloads = append(run.Workloads, wl)

		run.FinishedAt = time.Now().UTC().Format(time.RFC3339)
		run.DurationSec = time.Since(overall).Seconds()
		if err := writeJSON(*outFile, run); err != nil {
			fmt.Fprintf(os.Stderr, "  warn: progress save failed: %v\n", err)
		}
	}

	// Cleanup (concurrent, 32 workers)
	fmt.Printf("\nCleaning up %d files... ", len(wlc.fileIDs))
	cleanStart := time.Now()
	cleanSem := make(chan struct{}, 32)
	var cleanWG sync.WaitGroup
	for _, id := range wlc.fileIDs {
		cleanWG.Add(1)
		cleanSem <- struct{}{}
		go func(fid string) {
			defer cleanWG.Done()
			defer func() { <-cleanSem }()
			_ = client.delete(ctx, fid)
		}(id)
	}
	cleanWG.Wait()
	fmt.Printf("done (%s)\n", time.Since(cleanStart).Round(time.Millisecond))

	run.FinishedAt = time.Now().UTC().Format(time.RFC3339)
	run.DurationSec = time.Since(overall).Seconds()
	_ = writeJSON(*outFile, run)

	fmt.Printf("\n✅ Done in %s. Results: %s\n", time.Since(overall).Round(time.Second), *outFile)
}
