// cmd/lighthouse-bench/main.go
//
// Benchmark for Lighthouse (Filecoin + IPFS) upload/download/deal-status.
// Tests upload speed (data→IPFS), retrieval speed (IPFS gateway), and
// deal lifecycle status polling.
//
// Usage:
//
//	export LIGHTHOUSE_API_KEY="your-api-key"
//	go run ./cmd/lighthouse-bench                   # full matrix
//	go run ./cmd/lighthouse-bench -smoke            # quick check
//	go run ./cmd/lighthouse-bench -gateway https://gateway.lighthouse.storage/ipfs
//
// Get an API key: https://files.lighthouse.storage/
//
// Flags:
//
//	-out PATH       JSON output file (default bench-results/<host>-lighthouse-<ts>.json)
//	-smoke          quick mode (1KB + 1MB only)
//	-gateway URL    IPFS gateway for retrieval (default: https://gateway.lighthouse.storage/ipfs)
//	-host NAME      override hostname label
package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math"
	"mime/multipart"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// — Result types ----------------------------------------------------------------

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
	Gateway     string           `json:"gateway"`
	StartedAt   string           `json:"started_at"`
	FinishedAt  string           `json:"finished_at"`
	DurationSec float64          `json:"duration_sec"`
	Smoke       bool             `json:"smoke"`
	Workloads   []WorkloadResult `json:"workloads"`
}

// — Lighthouse client -----------------------------------------------------------

const lighthouseAPI = "https://upload.lighthouse.storage/api/v0"

type lhClient struct {
	apiKey  string
	gateway string
	http    *http.Client
}

type lhUploadResponse struct {
	Name string `json:"Name"`
	Hash string `json:"Hash"` // CID
	Size string `json:"Size"`
}

type lhDealInfo struct {
	CID            string `json:"cid"`
	DealStatus     string `json:"dealStatus"`
	MinerID        string `json:"miner"`
	LastUpdate     int64  `json:"lastUpdate"`
	ChainDealID    int64  `json:"chainDealID"`
	Content        any    `json:"content"`
	AggregateIn    string `json:"aggregateIn,omitempty"`
	ProofSetID     int64  `json:"proofSetID,omitempty"`
	ProofSetStatus string `json:"proofSetStatus,omitempty"`
}

func newLHClient(apiKey, gateway string) *lhClient {
	transport := &http.Transport{
		MaxIdleConns:        100,
		MaxIdleConnsPerHost: 100,
		IdleConnTimeout:     90 * time.Second,
		DialContext: (&net.Dialer{
			Timeout:   10 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
	}
	return &lhClient{
		apiKey:  apiKey,
		gateway: gateway,
		http: &http.Client{
			Transport: transport,
			Timeout:   10 * time.Minute,
		},
	}
}

func (c *lhClient) upload(ctx context.Context, filename string, data []byte) (*lhUploadResponse, error) {
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	part, err := w.CreateFormFile("file", filename)
	if err != nil {
		return nil, fmt.Errorf("create form file: %w", err)
	}
	if _, err := io.Copy(part, bytes.NewReader(data)); err != nil {
		return nil, fmt.Errorf("copy data: %w", err)
	}
	_ = w.Close()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, lighthouseAPI+"/add", &buf)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", w.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+c.apiKey)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("upload: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("upload status %d: %s", resp.StatusCode, truncate(string(body), 200))
	}

	var result lhUploadResponse
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	return &result, nil
}

func (c *lhClient) downloadViaCID(ctx context.Context, cid string) (io.ReadCloser, error) {
	url := c.gateway + "/" + cid
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("download: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close()
		return nil, fmt.Errorf("download status %d", resp.StatusCode)
	}
	return resp.Body, nil
}

func (c *lhClient) getDealStatus(ctx context.Context, cid string) ([]lhDealInfo, error) {
	url := fmt.Sprintf("https://api.lighthouse.storage/api/lighthouse/deal_status?cid=%s", cid)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("deal status: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("deal status %d: %s", resp.StatusCode, truncate(string(body), 200))
	}

	var deals []lhDealInfo
	if err := json.NewDecoder(resp.Body).Decode(&deals); err != nil {
		return nil, fmt.Errorf("decode: %w", err)
	}
	return deals, nil
}

func (c *lhClient) remove(ctx context.Context, cid string) error {
	url := fmt.Sprintf("https://api.lighthouse.storage/api/lighthouse/remove?cid=%s", cid)
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, url, nil)
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("remove: %w", err)
	}
	_ = resp.Body.Close()
	return nil
}

// — Workload context ------------------------------------------------------------

type wlContext struct {
	ctx    context.Context
	client *lhClient
	mu     sync.Mutex
	cids   []string
}

func (c *wlContext) track(cid string) {
	c.mu.Lock()
	c.cids = append(c.cids, cid)
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
	{"cold_upload_1kb", wlColdUpload},
	{"warm_upload_1kb", wlWarmUpload1KB},
	{"ipfs_retrieve_1kb", wlRetrieve1KB},
	{"deal_status_check", wlDealStatus},
	{"upload_1mb", wlUpload1MB},
	{"ipfs_retrieve_1mb", wlRetrieve1MB},
	{"upload_10mb", wlUpload10MB},
	{"ipfs_retrieve_10mb", wlRetrieve10MB},
	{"upload_50mb", wlUpload50MB},
	{"ipfs_retrieve_50mb", wlRetrieve50MB},
	{"integrity_10mb", wlIntegrity},
	{"concurrent_upload_20s", wlConcurrentUpload},
	{"concurrent_retrieve_20s", wlConcurrentRetrieve},
	{"ttfb_latency_10x", wlTTFBLatency},
}

var smokeNames = map[string]bool{
	"cold_upload_1kb":   true,
	"warm_upload_1kb":   true,
	"ipfs_retrieve_1kb": true,
	"deal_status_check": true,
	"upload_1mb":        true,
	"ipfs_retrieve_1mb": true,
}

func wlColdUpload(c *wlContext) WorkloadResult {
	data := randPayload(1024)
	start := time.Now()
	resp, err := c.client.upload(c.ctx, "bench-cold-1kb.bin", data)
	dur := time.Since(start)
	if err != nil {
		return WorkloadResult{Name: "cold_upload_1kb", Error: err.Error(), DurationMS: dur.Milliseconds()}
	}
	c.track(resp.Hash)
	return WorkloadResult{
		Name:       "cold_upload_1kb",
		Bytes:      1024,
		Ops:        1,
		DurationMS: dur.Milliseconds(),
		MBps:       mbps(1024, dur),
		Note:       fmt.Sprintf("cid=%s", resp.Hash),
	}
}

func wlWarmUpload1KB(c *wlContext) WorkloadResult {
	return uploadN(c, "warm_upload_1kb", 1024, 10)
}

func wlRetrieve1KB(c *wlContext) WorkloadResult {
	return retrieveLatest(c, "ipfs_retrieve_1kb")
}

func wlDealStatus(c *wlContext) WorkloadResult {
	c.mu.Lock()
	cids := append([]string{}, c.cids...)
	c.mu.Unlock()
	if len(cids) == 0 {
		return WorkloadResult{Name: "deal_status_check", Skipped: true, Note: "no uploads yet"}
	}

	cid := cids[0]
	start := time.Now()
	deals, err := c.client.getDealStatus(c.ctx, cid)
	dur := time.Since(start)
	if err != nil {
		return WorkloadResult{Name: "deal_status_check", DurationMS: dur.Milliseconds(), Note: fmt.Sprintf("cid=%s err=%s", cid, err.Error())}
	}

	note := fmt.Sprintf("cid=%s deals=%d", cid, len(deals))
	if len(deals) > 0 {
		note += fmt.Sprintf(" status=%s", deals[0].DealStatus)
		if deals[0].MinerID != "" {
			note += fmt.Sprintf(" miner=%s", deals[0].MinerID)
		}
		if deals[0].ProofSetStatus != "" {
			note += fmt.Sprintf(" proofSet=%s", deals[0].ProofSetStatus)
		}
	}

	return WorkloadResult{
		Name:       "deal_status_check",
		Ops:        1,
		DurationMS: dur.Milliseconds(),
		Note:       note,
	}
}

func wlUpload1MB(c *wlContext) WorkloadResult    { return uploadN(c, "upload_1mb", 1<<20, 3) }
func wlRetrieve1MB(c *wlContext) WorkloadResult  { return retrieveLatest(c, "ipfs_retrieve_1mb") }
func wlUpload10MB(c *wlContext) WorkloadResult   { return uploadN(c, "upload_10mb", 10<<20, 2) }
func wlRetrieve10MB(c *wlContext) WorkloadResult { return retrieveLatest(c, "ipfs_retrieve_10mb") }

func wlUpload50MB(c *wlContext) WorkloadResult {
	data := randPayload(50 << 20)
	start := time.Now()
	resp, err := c.client.upload(c.ctx, "bench-50mb.bin", data)
	dur := time.Since(start)
	if err != nil {
		return WorkloadResult{Name: "upload_50mb", Error: err.Error(), DurationMS: dur.Milliseconds()}
	}
	c.track(resp.Hash)
	return WorkloadResult{
		Name:       "upload_50mb",
		Bytes:      50 << 20,
		Ops:        1,
		DurationMS: dur.Milliseconds(),
		MBps:       mbps(50<<20, dur),
		Note:       fmt.Sprintf("cid=%s", resp.Hash),
	}
}

func wlRetrieve50MB(c *wlContext) WorkloadResult {
	return retrieveLatest(c, "ipfs_retrieve_50mb")
}

func wlIntegrity(c *wlContext) WorkloadResult {
	data := randPayload(10 << 20)
	expected := hashBytes(data)

	start := time.Now()
	resp, err := c.client.upload(c.ctx, "bench-integrity.bin", data)
	if err != nil {
		return WorkloadResult{Name: "integrity_10mb", Error: err.Error(), DurationMS: time.Since(start).Milliseconds()}
	}
	c.track(resp.Hash)

	rc, err := c.client.downloadViaCID(c.ctx, resp.Hash)
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
		Bytes:      20 << 20,
		Ops:        2,
		Errors:     errs,
		DurationMS: dur.Milliseconds(),
		MBps:       mbps(20<<20, dur),
		Note:       note,
	}
}

func wlConcurrentUpload(c *wlContext) WorkloadResult {
	const fileSize = 1 << 20
	const workers = 8
	duration := 20 * time.Second
	deadline := time.Now().Add(duration)
	var totalOps int64
	var totalBytes int64
	var errs int64
	var latencies []int64
	var mu sync.Mutex

	sem := make(chan struct{}, workers)
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
			resp, err := c.client.upload(c.ctx, fname, data)
			lat := time.Since(t).Milliseconds()
			if err != nil {
				atomic.AddInt64(&errs, 1)
				return
			}
			c.track(resp.Hash)
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
		Name:       "concurrent_upload_20s",
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
		Note:       fmt.Sprintf("workers=%d size=1MB", workers),
	}
}

func wlConcurrentRetrieve(c *wlContext) WorkloadResult {
	c.mu.Lock()
	cids := append([]string{}, c.cids...)
	c.mu.Unlock()
	if len(cids) == 0 {
		return WorkloadResult{Name: "concurrent_retrieve_20s", Skipped: true, Note: "no uploads"}
	}

	cid := cids[len(cids)-1]
	const workers = 8
	duration := 20 * time.Second
	deadline := time.Now().Add(duration)
	var totalOps int64
	var totalBytes int64
	var errs int64
	var latencies []int64
	var mu sync.Mutex

	sem := make(chan struct{}, workers)
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
			rc, err := c.client.downloadViaCID(c.ctx, cid)
			if err != nil {
				atomic.AddInt64(&errs, 1)
				return
			}
			n, _ := io.Copy(io.Discard, rc)
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
		Name:       "concurrent_retrieve_20s",
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
		Note:       fmt.Sprintf("workers=%d cid=%s", workers, cid[:12]),
	}
}

func wlTTFBLatency(c *wlContext) WorkloadResult {
	c.mu.Lock()
	cids := append([]string{}, c.cids...)
	c.mu.Unlock()
	if len(cids) == 0 {
		return WorkloadResult{Name: "ttfb_latency_10x", Skipped: true, Note: "no uploads"}
	}

	cid := cids[0]
	var latencies []int64
	start := time.Now()

	for i := 0; i < 10; i++ {
		url := c.client.gateway + "/" + cid
		req, err := http.NewRequestWithContext(c.ctx, http.MethodGet, url, nil)
		if err != nil {
			continue
		}
		req.Header.Set("Authorization", "Bearer "+c.client.apiKey)

		t := time.Now()
		resp, err := c.client.http.Do(req)
		ttfb := time.Since(t).Milliseconds()
		if err != nil {
			continue
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close() //nolint:errcheck
		latencies = append(latencies, ttfb)
	}
	dur := time.Since(start)

	return WorkloadResult{
		Name:       "ttfb_latency_10x",
		Ops:        len(latencies),
		DurationMS: dur.Milliseconds(),
		P50MS:      percentile(latencies, 50),
		P95MS:      percentile(latencies, 95),
		P99MS:      percentile(latencies, 99),
		MaxMS:      percentile(latencies, 100),
		OpsPerSec:  float64(len(latencies)) / dur.Seconds(),
		Note:       fmt.Sprintf("cid=%s", cid[:12]),
	}
}

// — Helpers ---------------------------------------------------------------------

func uploadN(c *wlContext, name string, size int64, n int) WorkloadResult {
	var latencies []int64
	var totalBytes int64
	start := time.Now()

	for i := 0; i < n; i++ {
		data := randPayload(size)
		fname := fmt.Sprintf("bench-%s-%d.bin", name, i)
		t := time.Now()
		resp, err := c.client.upload(c.ctx, fname, data)
		lat := time.Since(t).Milliseconds()
		if err != nil {
			return WorkloadResult{Name: name, Error: err.Error(), DurationMS: time.Since(start).Milliseconds()}
		}
		c.track(resp.Hash)
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

func retrieveLatest(c *wlContext, name string) WorkloadResult {
	c.mu.Lock()
	cids := append([]string{}, c.cids...)
	c.mu.Unlock()
	if len(cids) == 0 {
		return WorkloadResult{Name: name, Skipped: true, Note: "no uploads"}
	}

	cid := cids[len(cids)-1]
	const n = 3
	var latencies []int64
	var totalBytes int64
	start := time.Now()

	for i := 0; i < n; i++ {
		t := time.Now()
		rc, err := c.client.downloadViaCID(c.ctx, cid)
		if err != nil {
			return WorkloadResult{Name: name, Error: err.Error(), DurationMS: time.Since(start).Milliseconds()}
		}
		nn, _ := io.Copy(io.Discard, rc)
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
		Note:       fmt.Sprintf("cid=%s", cid[:min(12, len(cid))]),
	}
}

// — Stats helpers ---------------------------------------------------------------

func percentile(data []int64, pct int) int64 {
	if len(data) == 0 {
		return 0
	}
	s := make([]int64, len(data))
	copy(s, data)
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

func mbps(b int64, d time.Duration) float64 {
	if d <= 0 {
		return 0
	}
	return float64(b) / 1024.0 / 1024.0 / d.Seconds()
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
		fmt.Fprintf(&b, "❌ %-30s  %s", w.Name, truncate(w.Error, 60))
		return b.String()
	}
	if w.Skipped {
		fmt.Fprintf(&b, "⏭  %-30s  %s", w.Name, w.Note)
		return b.String()
	}
	fmt.Fprintf(&b, "✓  %-30s  %6dms", w.Name, w.DurationMS)
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
		outFile = flag.String("out", "", "JSON output file")
		smoke   = flag.Bool("smoke", false, "Quick mode")
		gateway = flag.String("gateway", "https://gateway.lighthouse.storage/ipfs", "IPFS gateway URL")
		host    = flag.String("host", "", "Host label")
	)
	flag.Parse()

	apiKey := os.Getenv("LIGHTHOUSE_API_KEY")
	if apiKey == "" {
		fmt.Fprintln(os.Stderr, "LIGHTHOUSE_API_KEY not set")
		fmt.Fprintln(os.Stderr, "Get one at: https://files.lighthouse.storage/")
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
		*outFile = filepath.Join("bench-results", fmt.Sprintf("%s-lighthouse-%s.json", sanitize(hostName), ts))
	}
	if err := os.MkdirAll(filepath.Dir(*outFile), 0o750); err != nil {
		fmt.Fprintf(os.Stderr, "mkdir: %v\n", err)
		os.Exit(1)
	}

	client := newLHClient(apiKey, *gateway)

	fmt.Printf("Host:     %s (%s/%s)\n", hostName, runtime.GOOS, runtime.GOARCH)
	fmt.Printf("Provider: Lighthouse (Filecoin + IPFS)\n")
	fmt.Printf("Gateway:  %s\n", *gateway)
	fmt.Printf("Output:   %s\n", *outFile)
	fmt.Printf("Smoke:    %v\n", *smoke)
	fmt.Println("─────────────────────────────────────────────────────────────────")

	run := RunResult{
		Host:      hostName,
		OSArch:    runtime.GOOS + "/" + runtime.GOARCH,
		Provider:  "lighthouse",
		Gateway:   *gateway,
		StartedAt: time.Now().UTC().Format(time.RFC3339),
		Smoke:     *smoke,
	}
	overall := time.Now()

	ctx := context.Background()
	wlc := &wlContext{ctx: ctx, client: client}

	for _, w := range allWorkloads {
		if *smoke && !smokeNames[w.name] {
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

	// Cleanup uploaded files
	fmt.Printf("\nCleaning up %d CIDs...\n", len(wlc.cids))
	for _, cid := range wlc.cids {
		_ = client.remove(ctx, cid)
	}

	run.FinishedAt = time.Now().UTC().Format(time.RFC3339)
	run.DurationSec = time.Since(overall).Seconds()
	_ = writeJSON(*outFile, run)

	fmt.Printf("\n✅ Done in %s. Results: %s\n", time.Since(overall).Round(time.Second), *outFile)
}
