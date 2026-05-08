// cmd/uloz-bench/main.go
//
// Benchmark for Uloz.to cloud storage via their public REST + resumable upload API.
// Tests auth, chunked upload (4/32/128 MB), download, integrity, listing, and cleanup.
//
// Usage:
//
//	export ULOZ_LOGIN=your_username
//	export ULOZ_AUTH_TOKEN=your_application_token   # from Settings → Application tokens
//	go run ./cmd/uloz-bench                          # full suite
//	go run ./cmd/uloz-bench -smoke                   # auth + 4MB round-trip only
//	go run ./cmd/uloz-bench -only upload             # substring filter
//	go run ./cmd/uloz-bench -skip 128mb              # skip large workloads
//
// Flags:
//
//	-out PATH    JSON output file (default bench-results/uloz-<ts>.json)
//	-smoke       quick mode (auth + 4MB upload/download + cleanup)
//	-host NAME   override hostname label in output
//	-only LIST   comma-separated substring filter for workload names
//	-skip LIST   comma-separated substring filter to skip workloads
//
// Environment:
//
//	ULOZ_LOGIN        uloz.to username (required)
//	ULOZ_AUTH_TOKEN   application login token from Settings (required)
//	ULOZ_API_KEY      API key (optional, defaults to public test key)
//	ULOZ_API_HOST     API hostname (optional, default apis.uloz.to)
package main

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	chunk4MB   = 4 * 1024 * 1024   // 4194304
	chunk32MB  = 32 * 1024 * 1024  // 33554432
	chunk128MB = 128 * 1024 * 1024 // 134217728

	defaultAPIKey  = `;HG%7jW6@6/8vx">R;f(`
	defaultAPIHost = "apis.uloz.to"
)

// ---------------------------------------------------------------------------
// Result types — match bench-compare format for consistent analysis
// ---------------------------------------------------------------------------

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
	StartedAt   string           `json:"started_at"`
	FinishedAt  string           `json:"finished_at"`
	DurationSec float64          `json:"duration_sec"`
	Smoke       bool             `json:"smoke"`
	Workloads   []WorkloadResult `json:"workloads"`
}

// ---------------------------------------------------------------------------
// API response types
// ---------------------------------------------------------------------------

type authResp struct {
	TokenID string `json:"token_id"`
}

type meResp struct {
	User struct {
		Login          string `json:"login"`
		RootFolderSlug string `json:"root_folder_slug"`
		Credit         int64  `json:"credit"`
	} `json:"user"`
}

type uploadLinkResp struct {
	UploadSignature        string `json:"upload_signature"`
	PrivateSlug            string `json:"private_slug"`
	UploadResumableBaseURL string `json:"upload_resumable_base_url"`
}

type registerResp struct {
	UploadURL  string `json:"upload_url"`
	ValidUntil string `json:"valid_until"`
	ReturnCode int    `json:"return_code"`
}

type statusResp struct {
	Status     string `json:"status"`
	ReturnCode int    `json:"return_code"`
	IRPC       *struct {
		Slug     string `json:"slug"`
		Filename string `json:"filename"`
	} `json:"irpc"`
	MissingChunks []int `json:"missing_chunks"`
}

type dlLinkResp struct {
	Link string `json:"link"`
	Hash string `json:"hash"`
}

type fileListResp struct {
	Metadata struct {
		ItemsCount int `json:"items_count"`
	} `json:"metadata"`
	Items []struct {
		Slug     string `json:"slug"`
		Name     string `json:"name"`
		FileSize int64  `json:"filesize"`
	} `json:"items"`
}

// ---------------------------------------------------------------------------
// Benchmark context
// ---------------------------------------------------------------------------

type ulozCtx struct {
	login          string
	authToken      string
	apiKey         string
	apiURL         string
	userToken      string
	rootFolderSlug string
	deviceID       string
	httpClient     *http.Client

	mu    sync.Mutex
	files []trackedFile
}

type trackedFile struct {
	slug   string
	batch  string
	name   string
	size   int64
	sha256 string
}

func (c *ulozCtx) track(f trackedFile) {
	c.mu.Lock()
	c.files = append(c.files, f)
	c.mu.Unlock()
}

func (c *ulozCtx) findBySize(size int64) (trackedFile, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, f := range c.files {
		if f.size == size {
			return f, true
		}
	}
	return trackedFile{}, false
}

// ---------------------------------------------------------------------------
// HTTP helpers
// ---------------------------------------------------------------------------

func tunedHTTPClient() *http.Client {
	dialer := &net.Dialer{
		Timeout:   15 * time.Second,
		KeepAlive: 30 * time.Second,
	}
	return &http.Client{
		Timeout: 10 * time.Minute,
		Transport: &http.Transport{
			MaxIdleConns:        100,
			MaxIdleConnsPerHost: 100,
			IdleConnTimeout:     90 * time.Second,
			TLSHandshakeTimeout: 10 * time.Second,
			ReadBufferSize:      1 << 20,
			WriteBufferSize:     1 << 20,
			DisableCompression:  true,
			ForceAttemptHTTP2:   true,
			DialContext:         dialer.DialContext,
			TLSClientConfig: &tls.Config{
				MinVersion:         tls.VersionTLS12,
				ClientSessionCache: tls.NewLRUClientSessionCache(64),
			},
		},
	}
}

// apiJSON calls an uloz.to API endpoint with auth headers.
func (c *ulozCtx) apiJSON(method, url string, body, result any) (int, error) {
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return 0, fmt.Errorf("marshal: %w", err)
		}
		r = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, url, r)
	if err != nil {
		return 0, err
	}
	req.Header.Set("X-Auth-Token", c.apiKey)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.userToken != "" {
		req.Header.Set("X-User-Token", c.userToken)
	}
	return c.doJSON(req, result)
}

// cdnJSON calls the upload CDN (no auth headers).
func (c *ulozCtx) cdnJSON(method, url string, body, result any) (int, error) {
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return 0, fmt.Errorf("marshal: %w", err)
		}
		r = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, url, r)
	if err != nil {
		return 0, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return c.doJSON(req, result)
}

func (c *ulozCtx) doJSON(req *http.Request, result any) (int, error) {
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return resp.StatusCode, fmt.Errorf("read body: %w", err)
	}
	if resp.StatusCode >= 400 {
		return resp.StatusCode, fmt.Errorf("HTTP %d: %s", resp.StatusCode, truncStr(string(raw), 500))
	}
	if result != nil && len(raw) > 0 {
		if err := json.Unmarshal(raw, result); err != nil {
			return resp.StatusCode, fmt.Errorf("unmarshal: %w (body: %s)", err, truncStr(string(raw), 200))
		}
	}
	return resp.StatusCode, nil
}

// ---------------------------------------------------------------------------
// API methods
// ---------------------------------------------------------------------------

func (c *ulozCtx) authenticate() error {
	body := map[string]string{"login": c.login, "token": c.authToken}
	var resp authResp
	_, err := c.apiJSON("POST", c.apiURL+"/v5/auth/token", body, &resp)
	if err != nil {
		return fmt.Errorf("auth: %w", err)
	}
	if resp.TokenID == "" {
		return fmt.Errorf("auth: empty token_id")
	}
	c.userToken = resp.TokenID
	return nil
}

func (c *ulozCtx) fetchMe() error {
	var resp meResp
	_, err := c.apiJSON("GET", c.apiURL+"/v5/me", nil, &resp)
	if err != nil {
		return fmt.Errorf("me: %w", err)
	}
	if resp.User.RootFolderSlug == "" {
		return fmt.Errorf("me: no root_folder_slug in response")
	}
	c.rootFolderSlug = resp.User.RootFolderSlug
	return nil
}

func (c *ulozCtx) getUploadSignature() (*uploadLinkResp, error) {
	body := map[string]string{"user_login": c.login}
	var resp uploadLinkResp
	_, err := c.apiJSON("POST", c.apiURL+"/v6/upload/link", body, &resp)
	if err != nil {
		return nil, fmt.Errorf("upload link: %w", err)
	}
	if resp.UploadSignature == "" || resp.PrivateSlug == "" {
		return nil, fmt.Errorf("upload link: empty signature or private_slug")
	}
	return &resp, nil
}

func (c *ulozCtx) registerFile(baseURL, signature, slug string, batchID int, name string, fileSize int64, chunkSize int) error {
	body := map[string]any{
		"batch_file_id":    batchID,
		"upload_signature": signature,
		"name":             name,
		"filesize":         fileSize,
		"chunksize":        chunkSize,
		"crc32":            nil,
	}
	var resp registerResp
	_, err := c.cdnJSON("POST", baseURL+"/v1/chunked-file-upload", body, &resp)
	if err != nil {
		return fmt.Errorf("register file: %w", err)
	}
	return nil
}

func (c *ulozCtx) uploadChunk(baseURL, slug string, batchID, chunkID int, data []byte) error {
	url := fmt.Sprintf("%s/v1/chunked-file-upload/%s/%d/%d", baseURL, slug, batchID, chunkID)
	req, err := http.NewRequest("POST", url, bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode != 201 && resp.StatusCode != 200 {
		return fmt.Errorf("chunk %d: HTTP %d", chunkID, resp.StatusCode)
	}
	return nil
}

func (c *ulozCtx) waitForProcessing(baseURL, slug string, batchID int) (string, error) {
	url := fmt.Sprintf("%s/v1/chunked-file-upload/%s/%d/status", baseURL, slug, batchID)
	deadline := time.Now().Add(5 * time.Minute)
	for time.Now().Before(deadline) {
		var resp statusResp
		_, err := c.cdnJSON("GET", url, nil, &resp)
		if err != nil {
			return "", fmt.Errorf("status poll: %w", err)
		}
		switch resp.Status {
		case "finished":
			if resp.IRPC != nil && resp.IRPC.Slug != "" {
				return resp.IRPC.Slug, nil
			}
			return "", fmt.Errorf("finished but no file slug in irpc")
		case "error":
			return "", fmt.Errorf("server-side processing error")
		}
		time.Sleep(2 * time.Second)
	}
	return "", fmt.Errorf("processing timeout (5m)")
}

func (c *ulozCtx) setProperties(fileSlugs []string, uploadTokens map[string]string, folderSlug string) error {
	body := map[string]any{
		"slugs":         fileSlugs,
		"upload_tokens": uploadTokens,
		"folder_slug":   folderSlug,
	}
	_, err := c.apiJSON("PATCH", c.apiURL+"/v8/file-list/private", body, nil)
	return err
}

func (c *ulozCtx) commitBatch(slug string) error {
	body := map[string]string{"status": "confirmed", "owner_login": c.login}
	_, err := c.apiJSON("PATCH", c.apiURL+"/v8/upload-batch/private/"+slug, body, nil)
	return err
}

func (c *ulozCtx) getDownloadLink(fileSlug string) (link, method string, err error) {
	body := map[string]any{
		"file_slug":  fileSlug,
		"device_id":  c.deviceID,
		"user_login": c.login,
	}
	for _, m := range []string{"vipdata", "fast", "free"} {
		var resp dlLinkResp
		url := fmt.Sprintf("%s/v5/file/download-link/%s", c.apiURL, m)
		code, e := c.apiJSON("POST", url, body, &resp)
		if e != nil {
			if code == 401 || code == 403 || code == 404 || code == 406 {
				continue
			}
			return "", m, fmt.Errorf("download-link/%s: %w", m, e)
		}
		if resp.Link != "" {
			return resp.Link, m, nil
		}
	}
	return "", "", fmt.Errorf("no download method available (tried vipdata/fast/free)")
}

func (c *ulozCtx) deleteFile(fileSlug string) error {
	_, err := c.apiJSON("DELETE", c.apiURL+"/v6/file/"+fileSlug+"/private", nil, nil)
	return err
}

func (c *ulozCtx) listFiles() (int, error) {
	url := fmt.Sprintf("%s/v8/user/%s/folder/%s/file-list?limit=100",
		c.apiURL, c.login, c.rootFolderSlug)
	var resp fileListResp
	_, err := c.apiJSON("GET", url, nil, &resp)
	if err != nil {
		return 0, err
	}
	return resp.Metadata.ItemsCount, nil
}

// ---------------------------------------------------------------------------
// Upload pipeline — ties together signature → register → chunks → wait → commit
// ---------------------------------------------------------------------------

type uploadTiming struct {
	fileSlug     string
	batchSlug    string
	signatureMS  int64
	registerMS   int64
	transferMS   int64
	processingMS int64
	commitMS     int64
	totalMS      int64
}

func (c *ulozCtx) uploadFile(name string, data []byte, chunkSize int) (*uploadTiming, error) {
	t := &uploadTiming{}
	overall := time.Now()

	// 1. Upload signature (creates a batch)
	mark := time.Now()
	sig, err := c.getUploadSignature()
	if err != nil {
		return nil, err
	}
	t.signatureMS = time.Since(mark).Milliseconds()
	t.batchSlug = sig.PrivateSlug
	baseURL := sig.UploadResumableBaseURL
	if baseURL == "" {
		baseURL = "https://upload-resumable.greencdn.io"
	}

	// 2. Register file
	mark = time.Now()
	if err := c.registerFile(baseURL, sig.UploadSignature, sig.PrivateSlug, 1, name, int64(len(data)), chunkSize); err != nil {
		return nil, err
	}
	t.registerMS = time.Since(mark).Milliseconds()

	// 3. Upload chunks sequentially
	mark = time.Now()
	numChunks := (len(data) + chunkSize - 1) / chunkSize
	for i := 0; i < numChunks; i++ {
		start := i * chunkSize
		end := start + chunkSize
		if end > len(data) {
			end = len(data)
		}
		if err := c.uploadChunk(baseURL, sig.PrivateSlug, 1, i+1, data[start:end]); err != nil {
			return nil, fmt.Errorf("chunk %d/%d: %w", i+1, numChunks, err)
		}
	}
	t.transferMS = time.Since(mark).Milliseconds()

	// 4. Wait for server-side processing
	mark = time.Now()
	fileSlug, err := c.waitForProcessing(baseURL, sig.PrivateSlug, 1)
	if err != nil {
		return nil, err
	}
	t.processingMS = time.Since(mark).Milliseconds()
	t.fileSlug = fileSlug

	// 5. Set properties (assign to root folder) + commit batch
	mark = time.Now()
	uploadTokens := map[string]string{
		fileSlug: fmt.Sprintf("%s:1", sig.PrivateSlug),
	}
	if err := c.setProperties([]string{fileSlug}, uploadTokens, c.rootFolderSlug); err != nil {
		return nil, fmt.Errorf("set properties: %w", err)
	}
	if err := c.commitBatch(sig.PrivateSlug); err != nil {
		return nil, fmt.Errorf("commit: %w", err)
	}
	t.commitMS = time.Since(mark).Milliseconds()

	t.totalMS = time.Since(overall).Milliseconds()

	h := sha256.Sum256(data)
	c.track(trackedFile{
		slug:   fileSlug,
		batch:  sig.PrivateSlug,
		name:   name,
		size:   int64(len(data)),
		sha256: hex.EncodeToString(h[:]),
	})

	return t, nil
}

// ---------------------------------------------------------------------------
// Download helpers
// ---------------------------------------------------------------------------

// downloadStream gets a download link, then streams the file to /dev/null.
// Returns bytes downloaded, download method, link-acquisition time, transfer time.
func (c *ulozCtx) downloadStream(fileSlug string) (int64, string, time.Duration, time.Duration, error) {
	t0 := time.Now()
	link, method, err := c.getDownloadLink(fileSlug)
	linkDur := time.Since(t0)
	if err != nil {
		return 0, "", linkDur, 0, err
	}

	t1 := time.Now()
	resp, err := c.httpClient.Get(link) //nolint:gosec // URL from trusted API response
	if err != nil {
		return 0, method, linkDur, time.Since(t1), err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != 200 {
		_, _ = io.Copy(io.Discard, resp.Body)
		return 0, method, linkDur, time.Since(t1), fmt.Errorf("download HTTP %d", resp.StatusCode)
	}
	n, err := io.Copy(io.Discard, resp.Body)
	return n, method, linkDur, time.Since(t1), err
}

// downloadFull downloads and returns the complete file bytes (for integrity checks).
func (c *ulozCtx) downloadFull(fileSlug string) ([]byte, string, error) {
	link, method, err := c.getDownloadLink(fileSlug)
	if err != nil {
		return nil, "", err
	}
	resp, err := c.httpClient.Get(link) //nolint:gosec // URL from trusted API response
	if err != nil {
		return nil, method, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != 200 {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil, method, fmt.Errorf("download HTTP %d", resp.StatusCode)
	}
	data, err := io.ReadAll(resp.Body)
	return data, method, err
}

// ---------------------------------------------------------------------------
// Workloads
// ---------------------------------------------------------------------------

type workload struct {
	name string
	fn   func(*ulozCtx) WorkloadResult
}

var allWorkloads = []workload{
	{"auth_latency", wlAuth},
	{"upload_4mb_chunk4", wlUpload4MB},
	{"upload_32mb_chunk4", wlUpload32MBc4},
	{"upload_32mb_chunk32", wlUpload32MBc32},
	{"upload_128mb_chunk128", wlUpload128MB},
	{"download_4mb", wlDownload4MB},
	{"download_128mb", wlDownload128MB},
	{"integrity_4mb", wlIntegrity},
	{"list_folder", wlListFolder},
	{"cleanup", wlCleanup},
}

var smokeWorkloads = []string{
	"auth_latency", "upload_4mb_chunk4", "download_4mb", "list_folder", "cleanup",
}

// --- auth ---

func wlAuth(c *ulozCtx) WorkloadResult {
	const N = 5
	var times []time.Duration
	var errs int
	overall := time.Now()

	for i := 0; i < N; i++ {
		tmp := &ulozCtx{
			login: c.login, authToken: c.authToken,
			apiKey: c.apiKey, apiURL: c.apiURL,
			httpClient: c.httpClient,
		}
		t := time.Now()
		if err := tmp.authenticate(); err != nil {
			errs++
		}
		times = append(times, time.Since(t))
	}

	p50, p95, p99, mx := percentiles(times)
	return WorkloadResult{
		Name:        "auth_latency",
		Description: fmt.Sprintf("%dx POST /v5/auth/token", N),
		Ops:         N,
		Errors:      errs,
		DurationMS:  msInt(time.Since(overall)),
		P50MS:       msInt(p50),
		P95MS:       msInt(p95),
		P99MS:       msInt(p99),
		MaxMS:       msInt(mx),
		OpsPerSec:   opsPerSec(N, time.Since(overall)),
	}
}

// --- uploads ---

func wlUpload4MB(c *ulozCtx) WorkloadResult {
	return doUploadWorkload(c, "upload_4mb_chunk4", 4*1024*1024, chunk4MB)
}
func wlUpload32MBc4(c *ulozCtx) WorkloadResult {
	return doUploadWorkload(c, "upload_32mb_chunk4", 32*1024*1024, chunk4MB)
}
func wlUpload32MBc32(c *ulozCtx) WorkloadResult {
	return doUploadWorkload(c, "upload_32mb_chunk32", 32*1024*1024, chunk32MB)
}
func wlUpload128MB(c *ulozCtx) WorkloadResult {
	return doUploadWorkload(c, "upload_128mb_chunk128", 128*1024*1024, chunk128MB)
}

func doUploadWorkload(c *ulozCtx, name string, fileSize, chunkSize int) WorkloadResult {
	data := randBytes(fileSize)
	numChunks := (fileSize + chunkSize - 1) / chunkSize

	t := time.Now()
	res, err := c.uploadFile(
		fmt.Sprintf("bench-%s-%d.bin", name, time.Now().UnixNano()),
		data, chunkSize,
	)
	dur := time.Since(t)

	if err != nil {
		return WorkloadResult{
			Name: name, DurationMS: msInt(dur),
			Error: err.Error(), Errors: 1,
		}
	}

	chunkLabel := fmtSize(chunkSize)
	note := fmt.Sprintf("%d chunks x %s | sig=%dms reg=%dms xfer=%dms proc=%dms commit=%dms",
		numChunks, chunkLabel,
		res.signatureMS, res.registerMS, res.transferMS,
		res.processingMS, res.commitMS)

	return WorkloadResult{
		Name:        name,
		Description: fmt.Sprintf("%s via %s chunks", fmtSize(fileSize), chunkLabel),
		Bytes:       int64(fileSize),
		Ops:         numChunks,
		DurationMS:  msInt(dur),
		MBps:        mbps(int64(fileSize), time.Duration(res.transferMS)*time.Millisecond),
		Note:        note,
	}
}

// --- downloads ---

func wlDownload4MB(c *ulozCtx) WorkloadResult {
	return doDownloadWorkload(c, "download_4mb", 4*1024*1024)
}
func wlDownload128MB(c *ulozCtx) WorkloadResult {
	return doDownloadWorkload(c, "download_128mb", 128*1024*1024)
}

func doDownloadWorkload(c *ulozCtx, name string, targetSize int64) WorkloadResult {
	f, ok := c.findBySize(targetSize)
	if !ok {
		return WorkloadResult{
			Name: name, Skipped: true,
			Note: fmt.Sprintf("no %s file uploaded", fmtSize(int(targetSize))),
		}
	}

	t := time.Now()
	n, method, linkDur, xferDur, err := c.downloadStream(f.slug)
	dur := time.Since(t)

	if err != nil {
		return WorkloadResult{
			Name: name, DurationMS: msInt(dur),
			Error: err.Error(), Errors: 1,
		}
	}

	return WorkloadResult{
		Name:        name,
		Description: fmt.Sprintf("%s download via %s", fmtSize(int(targetSize)), method),
		Bytes:       n,
		Ops:         1,
		DurationMS:  msInt(dur),
		MBps:        mbps(n, xferDur),
		Note:        fmt.Sprintf("method=%s link=%dms transfer=%dms", method, msInt(linkDur), msInt(xferDur)),
	}
}

// --- integrity ---

func wlIntegrity(c *ulozCtx) WorkloadResult {
	const size = 4 * 1024 * 1024
	data := randBytes(size)
	wantHash := sha256hex(data)
	name := fmt.Sprintf("bench-integrity-%d.bin", time.Now().UnixNano())

	t := time.Now()
	res, err := c.uploadFile(name, data, chunk4MB)
	if err != nil {
		return WorkloadResult{
			Name: "integrity_4mb", DurationMS: msInt(time.Since(t)),
			Error: fmt.Sprintf("upload: %v", err), Errors: 1,
		}
	}

	dlData, method, dlErr := c.downloadFull(res.fileSlug)
	dur := time.Since(t)
	if dlErr != nil {
		return WorkloadResult{
			Name: "integrity_4mb", DurationMS: msInt(dur),
			Error: fmt.Sprintf("download: %v", dlErr), Errors: 1,
		}
	}

	gotHash := sha256hex(dlData)
	match := gotHash == wantHash
	errs := 0
	note := fmt.Sprintf("sha256_match=%v method=%s upload=%dms", match, method, res.totalMS)
	if !match {
		errs = 1
		note += fmt.Sprintf(" want=%s got=%s", wantHash[:16], gotHash[:16])
	}

	return WorkloadResult{
		Name:        "integrity_4mb",
		Description: "upload 4MB → download → SHA256 verify",
		Bytes:       size,
		Ops:         2,
		Errors:      errs,
		DurationMS:  msInt(dur),
		Note:        note,
	}
}

// --- list ---

func wlListFolder(c *ulozCtx) WorkloadResult {
	const N = 5
	var times []time.Duration
	var errs int
	var lastCount int
	overall := time.Now()

	for i := 0; i < N; i++ {
		t := time.Now()
		count, err := c.listFiles()
		times = append(times, time.Since(t))
		if err != nil {
			errs++
		} else {
			lastCount = count
		}
	}

	p50, p95, p99, mx := percentiles(times)
	return WorkloadResult{
		Name:        "list_folder",
		Description: fmt.Sprintf("%dx GET file-list", N),
		Ops:         N,
		Errors:      errs,
		DurationMS:  msInt(time.Since(overall)),
		P50MS:       msInt(p50),
		P95MS:       msInt(p95),
		P99MS:       msInt(p99),
		MaxMS:       msInt(mx),
		OpsPerSec:   opsPerSec(N, time.Since(overall)),
		Note:        fmt.Sprintf("items=%d", lastCount),
	}
}

// --- cleanup ---

func wlCleanup(c *ulozCtx) WorkloadResult {
	c.mu.Lock()
	files := append([]trackedFile{}, c.files...)
	c.mu.Unlock()

	if len(files) == 0 {
		return WorkloadResult{Name: "cleanup", Skipped: true, Note: "nothing to delete"}
	}

	overall := time.Now()
	var errs int
	for _, f := range files {
		if err := c.deleteFile(f.slug); err != nil {
			fmt.Printf("  ⚠️  delete %s: %v\n", f.slug, err)
			errs++
		}
	}

	return WorkloadResult{
		Name:        "cleanup",
		Description: fmt.Sprintf("delete %d bench files", len(files)),
		Ops:         len(files),
		Errors:      errs,
		DurationMS:  msInt(time.Since(overall)),
		OpsPerSec:   opsPerSec(len(files), time.Since(overall)),
	}
}

// ---------------------------------------------------------------------------
// Main
// ---------------------------------------------------------------------------

func main() {
	var (
		outFile = flag.String("out", "", "JSON output file")
		smoke   = flag.Bool("smoke", false, "Quick mode: auth + 4MB round-trip only")
		host    = flag.String("host", "", "Hostname label")
		only    = flag.String("only", "", "Comma-separated workload name filter")
		skip    = flag.String("skip", "", "Comma-separated workload skip filter")
	)
	flag.Parse()

	login := os.Getenv("ULOZ_LOGIN")
	authToken := os.Getenv("ULOZ_AUTH_TOKEN")
	if login == "" || authToken == "" {
		fmt.Fprintln(os.Stderr, "ULOZ_LOGIN and ULOZ_AUTH_TOKEN are required")
		fmt.Fprintln(os.Stderr, "  ULOZ_LOGIN=username ULOZ_AUTH_TOKEN=app_token go run ./cmd/uloz-bench")
		os.Exit(1)
	}

	apiKey := os.Getenv("ULOZ_API_KEY")
	if apiKey == "" {
		apiKey = defaultAPIKey
	}
	apiHost := os.Getenv("ULOZ_API_HOST")
	if apiHost == "" {
		apiHost = defaultAPIHost
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
		*outFile = filepath.Join("bench-results", fmt.Sprintf("uloz-%s-%s.json", sanitize(hostName), ts))
	}
	if err := os.MkdirAll(filepath.Dir(*outFile), 0o750); err != nil {
		fmt.Fprintf(os.Stderr, "mkdir: %v\n", err)
		os.Exit(1)
	}

	ctx := &ulozCtx{
		login:      login,
		authToken:  authToken,
		apiKey:     apiKey,
		apiURL:     "https://" + apiHost,
		deviceID:   fmt.Sprintf("vaultaire-bench-%x", randBytes(8)),
		httpClient: tunedHTTPClient(),
	}

	// --- Setup: authenticate and get root folder ---
	fmt.Println("Authenticating...")
	if err := ctx.authenticate(); err != nil {
		fmt.Fprintf(os.Stderr, "❌ %v\n", err)
		os.Exit(1)
	}
	if err := ctx.fetchMe(); err != nil {
		fmt.Fprintf(os.Stderr, "❌ %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("✓ Authenticated as %s (root folder: %s)\n", login, ctx.rootFolderSlug)

	fmt.Printf("Host:       %s (%s/%s)\n", hostName, runtime.GOOS, runtime.GOARCH)
	fmt.Printf("Output:     %s\n", *outFile)
	fmt.Printf("Provider:   uloz.to (api: %s)\n", apiHost)
	fmt.Printf("Smoke:      %v\n", *smoke)
	fmt.Println("─────────────────────────────────────────────────────────────────")

	run := RunResult{
		Host:      hostName,
		OSArch:    runtime.GOOS + "/" + runtime.GOARCH,
		Provider:  "uloz.to",
		StartedAt: time.Now().UTC().Format(time.RFC3339),
		Smoke:     *smoke,
	}
	overall := time.Now()

	onlyList := splitCSV(*only)
	skipList := splitCSV(*skip)

	for _, w := range allWorkloads {
		if *smoke && !contains(smokeWorkloads, w.name) {
			continue
		}
		if len(onlyList) > 0 && !anyContains(w.name, onlyList) {
			continue
		}
		if len(skipList) > 0 && anyContains(w.name, skipList) {
			continue
		}

		fmt.Printf("\n  %-30s ", w.name)
		result := w.fn(ctx)
		fmt.Printf("%s\n", oneLine(result))
		run.Workloads = append(run.Workloads, result)

		run.FinishedAt = time.Now().UTC().Format(time.RFC3339)
		run.DurationSec = time.Since(overall).Seconds()
		if err := writeJSON(*outFile, run); err != nil {
			fmt.Fprintf(os.Stderr, "  warn: save failed: %v\n", err)
		}
	}

	fmt.Printf("\n✅ Done in %s. Results: %s\n", time.Since(overall).Round(time.Second), *outFile)
}

// ---------------------------------------------------------------------------
// Utilities
// ---------------------------------------------------------------------------

func randBytes(n int) []byte {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return b
}

func sha256hex(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func msInt(d time.Duration) int64 { return d.Milliseconds() }

func mbps(b int64, d time.Duration) float64 {
	if d <= 0 {
		return 0
	}
	return float64(b) / 1024 / 1024 / d.Seconds()
}

func opsPerSec(ops int, d time.Duration) float64 {
	if d <= 0 {
		return 0
	}
	return float64(ops) / d.Seconds()
}

func percentiles(durs []time.Duration) (p50, p95, p99, mx time.Duration) {
	if len(durs) == 0 {
		return
	}
	s := append([]time.Duration{}, durs...)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	idx := func(p int) int {
		i := len(s) * p / 100
		if i >= len(s) {
			i = len(s) - 1
		}
		return i
	}
	return s[idx(50)], s[idx(95)], s[idx(99)], s[len(s)-1]
}

func fmtSize(b int) string {
	switch {
	case b >= 1024*1024*1024:
		return fmt.Sprintf("%dGB", b/(1024*1024*1024))
	case b >= 1024*1024:
		return fmt.Sprintf("%dMB", b/(1024*1024))
	case b >= 1024:
		return fmt.Sprintf("%dKB", b/1024)
	default:
		return fmt.Sprintf("%dB", b)
	}
}

func sanitize(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func truncStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

func splitCSV(s string) []string {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if t := strings.TrimSpace(p); t != "" {
			out = append(out, t)
		}
	}
	return out
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func anyContains(name string, list []string) bool {
	for _, s := range list {
		if strings.Contains(name, s) {
			return true
		}
	}
	return false
}

func oneLine(w WorkloadResult) string {
	if w.Skipped {
		return fmt.Sprintf("SKIP — %s", w.Note)
	}
	if w.Error != "" {
		return fmt.Sprintf("ERROR — %s", w.Error)
	}
	parts := []string{fmt.Sprintf("%6dms", w.DurationMS)}
	if w.MBps > 0 {
		parts = append(parts, fmt.Sprintf("%6.1f MB/s", w.MBps))
	}
	if w.OpsPerSec > 0 {
		parts = append(parts, fmt.Sprintf("%5.1f ops/s", w.OpsPerSec))
	}
	if w.P50MS > 0 {
		parts = append(parts, fmt.Sprintf("p50=%dms p95=%dms", w.P50MS, w.P95MS))
	}
	if w.Errors > 0 {
		parts = append(parts, fmt.Sprintf("err=%d", w.Errors))
	}
	if w.Note != "" {
		parts = append(parts, w.Note)
	}
	return strings.Join(parts, "  ")
}

func writeJSON(path string, v any) error {
	tmp := path + ".tmp"
	f, err := os.Create(tmp) // #nosec G304 — benchmark output path
	if err != nil {
		return err
	}
	enc := json.NewEncoder(f)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
