// cmd/quotaless-bench-v2/main.go
//
// Quotaless benchmark using raw HTTP + SigV4 signing (UNSIGNED-PAYLOAD).
// Bypasses AWS SDK v2 middleware that's incompatible with Minio gateways.
// Zero external dependencies beyond what Vaultaire already imports.
//
// Usage:
//
//	source .env.bench
//	go run ./cmd/quotaless-bench-v2
//	go run ./cmd/quotaless-bench-v2 -endpoint https://srv1.quotaless.cloud:8000
//	go run ./cmd/quotaless-bench-v2 -smoke
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

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	"github.com/aws/aws-sdk-go-v2/credentials"
)

// --- Raw S3 client for Minio gateways ---

type rawS3Client struct {
	endpoint  string
	bucket    string
	keyPrefix string
	creds     aws.CredentialsProvider
	signer    *v4.Signer
	http      *http.Client
	region    string
}

func newRawS3Client(endpoint, accessKey, secretKey, bucket, keyPrefix string) *rawS3Client {
	transport := &http.Transport{
		MaxIdleConns:        200,
		MaxIdleConnsPerHost: 200,
		IdleConnTimeout:     90 * time.Second,
		TLSHandshakeTimeout: 10 * time.Second,
		ReadBufferSize:      1 << 20,
		WriteBufferSize:     1 << 20,
		DisableCompression:  true,
		ForceAttemptHTTP2:   true,
		DialContext: (&net.Dialer{
			Timeout:   10 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		TLSClientConfig: &tls.Config{
			MinVersion:         tls.VersionTLS12,
			ClientSessionCache: tls.NewLRUClientSessionCache(128),
		},
	}
	return &rawS3Client{
		endpoint:  strings.TrimRight(endpoint, "/"),
		bucket:    bucket,
		keyPrefix: keyPrefix,
		creds:     credentials.NewStaticCredentialsProvider(accessKey, secretKey, ""),
		signer:    v4.NewSigner(),
		http:      &http.Client{Transport: transport},
		region:    "us-east-1",
	}
}

func (c *rawS3Client) signAndDo(ctx context.Context, req *http.Request) (*http.Response, error) {
	creds, err := c.creds.Retrieve(ctx)
	if err != nil {
		return nil, fmt.Errorf("retrieve creds: %w", err)
	}
	// Must set x-amz-content-sha256 BEFORE signing — the v4 signer doesn't
	// add it automatically (the S3 SDK middleware normally does this).
	// UNSIGNED-PAYLOAD skips body hashing — required for Minio compatibility.
	req.Header.Set("x-amz-content-sha256", "UNSIGNED-PAYLOAD")
	err = c.signer.SignHTTP(ctx, creds, req, "UNSIGNED-PAYLOAD", "s3", c.region, time.Now())
	if err != nil {
		return nil, fmt.Errorf("sign: %w", err)
	}
	return c.http.Do(req)
}

func (c *rawS3Client) objectURL(key string) string {
	return fmt.Sprintf("%s/%s/%s%s", c.endpoint, c.bucket, c.keyPrefix, key)
}

func (c *rawS3Client) put(ctx context.Context, key string, body io.ReadSeeker, size int64) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, c.objectURL(key), body)
	if err != nil {
		return err
	}
	req.ContentLength = size
	req.Header.Set("Content-Type", "application/octet-stream")

	resp, err := c.signAndDo(ctx, req)
	if err != nil {
		return err
	}
	defer resp.Body.Close() //nolint:errcheck
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode >= 300 {
		return fmt.Errorf("PUT %d", resp.StatusCode)
	}
	return nil
}

func (c *rawS3Client) get(ctx context.Context, key string) (io.ReadCloser, int64, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.objectURL(key), nil)
	if err != nil {
		return nil, 0, err
	}
	resp, err := c.signAndDo(ctx, req)
	if err != nil {
		return nil, 0, err
	}
	if resp.StatusCode != 200 {
		_ = resp.Body.Close()
		return nil, 0, fmt.Errorf("GET %d", resp.StatusCode)
	}
	return resp.Body, resp.ContentLength, nil
}

func (c *rawS3Client) getRange(ctx context.Context, key string, start, end int64) (io.ReadCloser, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.objectURL(key), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", start, end))
	resp, err := c.signAndDo(ctx, req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != 206 && resp.StatusCode != 200 {
		_ = resp.Body.Close()
		return nil, fmt.Errorf("RANGE GET %d", resp.StatusCode)
	}
	return resp.Body, nil
}

func (c *rawS3Client) del(ctx context.Context, key string) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodDelete, c.objectURL(key), nil)
	if req == nil {
		return
	}
	resp, err := c.signAndDo(ctx, req)
	if err == nil {
		_ = resp.Body.Close()
	}
}

// --- Benchmark framework ---

type result struct {
	Name   string  `json:"name"`
	Bytes  int64   `json:"bytes,omitempty"`
	Ops    int     `json:"ops,omitempty"`
	Errors int     `json:"errors,omitempty"`
	DurMS  int64   `json:"duration_ms"`
	MBps   float64 `json:"mb_per_sec,omitempty"`
	OpsPS  float64 `json:"ops_per_sec,omitempty"`
	P50MS  int64   `json:"p50_ms,omitempty"`
	P95MS  int64   `json:"p95_ms,omitempty"`
	Note   string  `json:"note,omitempty"`
	Error  string  `json:"error,omitempty"`
}

func mbps(b int64, d time.Duration) float64 {
	if d <= 0 {
		return 0
	}
	return float64(b) / 1048576.0 / d.Seconds()
}

func pct(lats []int64, p int) int64 {
	if len(lats) == 0 {
		return 0
	}
	s := make([]int64, len(lats))
	copy(s, lats)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	idx := len(s)*p/100 - 1
	if idx < 0 {
		idx = 0
	}
	return s[idx]
}

func sha256hex(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func oneLine(r result) string {
	if r.Error != "" {
		return fmt.Sprintf("  %-30s ERROR — %s", r.Name, r.Error)
	}
	parts := []string{fmt.Sprintf("  %-30s %6dms", r.Name, r.DurMS)}
	if r.MBps > 0 {
		parts = append(parts, fmt.Sprintf("%7.1f MB/s", r.MBps))
	}
	if r.OpsPS > 0 {
		parts = append(parts, fmt.Sprintf("%5.1f ops/s", r.OpsPS))
	}
	if r.P50MS > 0 {
		parts = append(parts, fmt.Sprintf("p50=%dms p95=%dms", r.P50MS, r.P95MS))
	}
	if r.Errors > 0 {
		parts = append(parts, fmt.Sprintf("err=%d", r.Errors))
	}
	if r.Note != "" {
		parts = append(parts, r.Note)
	}
	return strings.Join(parts, "  ")
}

// --- Workloads ---

type benchCtx struct {
	ctx    context.Context
	client *rawS3Client
	mu     sync.Mutex
	keys   []string
}

func (b *benchCtx) track(key string) {
	b.mu.Lock()
	b.keys = append(b.keys, key)
	b.mu.Unlock()
}

func wlIntegrity(b *benchCtx, size int64, name string) result {
	data := make([]byte, size)
	_, _ = rand.Read(data)
	expected := sha256hex(data)
	key := fmt.Sprintf("bench/%s/%d", name, time.Now().UnixNano())

	start := time.Now()
	if err := b.client.put(b.ctx, key, bytes.NewReader(data), size); err != nil {
		return result{Name: name, Error: "put: " + err.Error(), DurMS: time.Since(start).Milliseconds()}
	}
	b.track(key)

	rc, _, err := b.client.get(b.ctx, key)
	if err != nil {
		return result{Name: name, Error: "get: " + err.Error(), DurMS: time.Since(start).Milliseconds()}
	}
	got, _ := io.ReadAll(rc)
	_ = rc.Close()
	dur := time.Since(start)

	actual := sha256hex(got)
	note := "✓ SHA256 match"
	if actual != expected {
		note = fmt.Sprintf("✗ MISMATCH want=%s got=%s (%d/%d bytes)", expected[:16], actual[:16], len(got), size)
	}
	return result{Name: name, Bytes: size, Ops: 1, DurMS: dur.Milliseconds(), MBps: mbps(size*2, dur), Note: note}
}

func wlPut(b *benchCtx, size int64, count int, name string) result {
	data := make([]byte, size)
	_, _ = rand.Read(data)
	var lats []int64
	var total int64
	start := time.Now()
	for i := 0; i < count; i++ {
		key := fmt.Sprintf("bench/%s/%d-%d", name, time.Now().UnixNano(), i)
		t := time.Now()
		if err := b.client.put(b.ctx, key, bytes.NewReader(data), size); err != nil {
			return result{Name: name, Error: err.Error(), DurMS: time.Since(start).Milliseconds()}
		}
		lats = append(lats, time.Since(t).Milliseconds())
		b.track(key)
		total += size
	}
	dur := time.Since(start)
	return result{Name: name, Bytes: total, Ops: count, DurMS: dur.Milliseconds(), MBps: mbps(total, dur), P50MS: pct(lats, 50), P95MS: pct(lats, 95)}
}

func wlGet(b *benchCtx, size int64, count int, name string) result {
	data := make([]byte, size)
	_, _ = rand.Read(data)
	key := fmt.Sprintf("bench/%s-seed/%d", name, time.Now().UnixNano())
	if err := b.client.put(b.ctx, key, bytes.NewReader(data), size); err != nil {
		return result{Name: name, Error: "seed: " + err.Error()}
	}
	b.track(key)

	var lats []int64
	var total int64
	start := time.Now()
	for i := 0; i < count; i++ {
		t := time.Now()
		rc, _, err := b.client.get(b.ctx, key)
		if err != nil {
			return result{Name: name, Error: err.Error(), DurMS: time.Since(start).Milliseconds()}
		}
		n, _ := io.Copy(io.Discard, rc)
		_ = rc.Close()
		lats = append(lats, time.Since(t).Milliseconds())
		total += n
	}
	dur := time.Since(start)
	return result{Name: name, Bytes: total, Ops: count, DurMS: dur.Milliseconds(), MBps: mbps(total, dur), P50MS: pct(lats, 50), P95MS: pct(lats, 95)}
}

func wlRangeParallel(b *benchCtx, name string, workers int) result {
	const sz = 64 << 20
	data := make([]byte, sz)
	_, _ = rand.Read(data)
	key := fmt.Sprintf("bench/%s-seed/%d", name, time.Now().UnixNano())
	if err := b.client.put(b.ctx, key, bytes.NewReader(data), sz); err != nil {
		return result{Name: name, Error: "seed: " + err.Error()}
	}
	b.track(key)

	chunkSize := int64(sz) / int64(workers)
	var totalBytes atomic.Int64
	var errs atomic.Int32
	var lats []int64
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
				rangeEnd = sz - 1
			}
			t := time.Now()
			rc, err := b.client.getRange(b.ctx, key, rangeStart, rangeEnd)
			if err != nil {
				errs.Add(1)
				return
			}
			n, _ := io.Copy(io.Discard, rc)
			_ = rc.Close()
			lat := time.Since(t).Milliseconds()
			totalBytes.Add(n)
			mu.Lock()
			lats = append(lats, lat)
			mu.Unlock()
		}(i)
	}
	wg.Wait()
	dur := time.Since(start)
	return result{
		Name: name, Bytes: totalBytes.Load(), Ops: workers, Errors: int(errs.Load()),
		DurMS: dur.Milliseconds(), MBps: mbps(totalBytes.Load(), dur),
		P50MS: pct(lats, 50), P95MS: pct(lats, 95),
		Note: fmt.Sprintf("%d parallel ranges of 64MB", workers),
	}
}

func wlRangeParallel256(b *benchCtx, name string, workers int) result {
	const sz = 256 << 20
	data := make([]byte, sz)
	_, _ = rand.Read(data)
	key := fmt.Sprintf("bench/%s-seed/%d", name, time.Now().UnixNano())
	if err := b.client.put(b.ctx, key, bytes.NewReader(data), sz); err != nil {
		return result{Name: name, Error: "seed: " + err.Error()}
	}
	b.track(key)

	chunkSize := int64(sz) / int64(workers)
	var totalBytes atomic.Int64
	var errs atomic.Int32
	var lats []int64
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
				rangeEnd = sz - 1
			}
			t := time.Now()
			rc, err := b.client.getRange(b.ctx, key, rangeStart, rangeEnd)
			if err != nil {
				errs.Add(1)
				return
			}
			n, _ := io.Copy(io.Discard, rc)
			_ = rc.Close()
			mu.Lock()
			lats = append(lats, time.Since(t).Milliseconds())
			mu.Unlock()
			totalBytes.Add(n)
		}(i)
	}
	wg.Wait()
	dur := time.Since(start)
	return result{
		Name: name, Bytes: totalBytes.Load(), Ops: workers, Errors: int(errs.Load()),
		DurMS: dur.Milliseconds(), MBps: mbps(totalBytes.Load(), dur),
		P50MS: pct(lats, 50), P95MS: pct(lats, 95),
		Note: fmt.Sprintf("%d parallel ranges of 256MB", workers),
	}
}

func wlConcurrentPut(b *benchCtx, name string, fileSize int64, workers int, dur time.Duration) result {
	data := make([]byte, fileSize)
	_, _ = rand.Read(data)
	deadline := time.Now().Add(dur)
	var ops, errs atomic.Int64
	var totalBytes atomic.Int64
	var lats []int64
	var mu sync.Mutex

	start := time.Now()
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(wid int) {
			defer wg.Done()
			for i := 0; time.Now().Before(deadline); i++ {
				key := fmt.Sprintf("bench/%s/w%d-%d", name, wid, i)
				t := time.Now()
				if err := b.client.put(b.ctx, key, bytes.NewReader(data), fileSize); err != nil {
					errs.Add(1)
					continue
				}
				lat := time.Since(t).Milliseconds()
				ops.Add(1)
				totalBytes.Add(fileSize)
				mu.Lock()
				lats = append(lats, lat)
				mu.Unlock()
				b.track(key)
			}
		}(w)
	}
	wg.Wait()
	elapsed := time.Since(start)
	o := int(ops.Load())
	tb := totalBytes.Load()
	return result{
		Name: name, Bytes: tb, Ops: o, Errors: int(errs.Load()),
		DurMS: elapsed.Milliseconds(), MBps: mbps(tb, elapsed),
		OpsPS: float64(o) / elapsed.Seconds(),
		P50MS: pct(lats, 50), P95MS: pct(lats, 95),
		Note: fmt.Sprintf("%d workers × %dMB for %s", workers, fileSize>>20, dur),
	}
}

func wlConcurrentGet(b *benchCtx, name string, fileSize int64, workers int, dur time.Duration) result {
	data := make([]byte, fileSize)
	_, _ = rand.Read(data)
	key := fmt.Sprintf("bench/%s-seed/%d", name, time.Now().UnixNano())
	if err := b.client.put(b.ctx, key, bytes.NewReader(data), fileSize); err != nil {
		return result{Name: name, Error: "seed: " + err.Error()}
	}
	b.track(key)

	deadline := time.Now().Add(dur)
	var ops, errs atomic.Int64
	var totalBytes atomic.Int64
	var lats []int64
	var mu sync.Mutex

	start := time.Now()
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for time.Now().Before(deadline) {
				t := time.Now()
				rc, _, err := b.client.get(b.ctx, key)
				if err != nil {
					errs.Add(1)
					continue
				}
				n, _ := io.Copy(io.Discard, rc)
				_ = rc.Close()
				lat := time.Since(t).Milliseconds()
				ops.Add(1)
				totalBytes.Add(n)
				mu.Lock()
				lats = append(lats, lat)
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	elapsed := time.Since(start)
	o := int(ops.Load())
	tb := totalBytes.Load()
	return result{
		Name: name, Bytes: tb, Ops: o, Errors: int(errs.Load()),
		DurMS: elapsed.Milliseconds(), MBps: mbps(tb, elapsed),
		OpsPS: float64(o) / elapsed.Seconds(),
		P50MS: pct(lats, 50), P95MS: pct(lats, 95),
		Note: fmt.Sprintf("%d workers × %dMB for %s", workers, fileSize>>20, dur),
	}
}

// --- Main ---

type runResult struct {
	Host      string   `json:"host"`
	OSArch    string   `json:"os_arch"`
	Endpoint  string   `json:"endpoint"`
	Bucket    string   `json:"bucket"`
	StartedAt string   `json:"started_at"`
	DurSec    float64  `json:"duration_sec"`
	Smoke     bool     `json:"smoke"`
	Results   []result `json:"results"`
}

func main() {
	var (
		endpoint = flag.String("endpoint", "https://srv1.quotaless.cloud:8000", "S3 endpoint")
		bucket   = flag.String("bucket", "data", "S3 bucket")
		prefix   = flag.String("prefix", "personal-files/", "Key prefix")
		smoke    = flag.Bool("smoke", false, "Quick mode")
		outFile  = flag.String("out", "", "JSON output file")
		host     = flag.String("host", "", "Host label")
	)
	flag.Parse()

	ak := os.Getenv("QUOTALESS_ACCESS_KEY")
	sk := os.Getenv("QUOTALESS_SECRET_KEY")
	if ak == "" {
		fmt.Fprintln(os.Stderr, "Set QUOTALESS_ACCESS_KEY (and optionally QUOTALESS_SECRET_KEY, default=gatewaysecret)")
		os.Exit(1)
	}
	if sk == "" {
		sk = "gatewaysecret"
	}

	hostName := *host
	if hostName == "" {
		h, _ := os.Hostname()
		hostName = h
	}
	if *outFile == "" {
		ts := time.Now().Format("20060102-150405")
		*outFile = filepath.Join("bench-results", fmt.Sprintf("quotaless-%s-%s.json", hostName, ts))
	}
	_ = os.MkdirAll(filepath.Dir(*outFile), 0o750)

	client := newRawS3Client(*endpoint, ak, sk, *bucket, *prefix)
	ctx := context.Background()
	bc := &benchCtx{ctx: ctx, client: client}

	fmt.Printf("Host:     %s (%s/%s)\n", hostName, runtime.GOOS, runtime.GOARCH)
	fmt.Printf("Endpoint: %s\n", *endpoint)
	fmt.Printf("Bucket:   %s/%s\n", *bucket, *prefix)
	fmt.Printf("Mode:     smoke=%v\n", *smoke)
	fmt.Printf("Client:   raw HTTP + SigV4 (UNSIGNED-PAYLOAD)\n")
	fmt.Println("─────────────────────────────────────────────────────────────")

	run := runResult{
		Host: hostName, OSArch: runtime.GOOS + "/" + runtime.GOARCH,
		Endpoint: *endpoint, Bucket: *bucket, StartedAt: time.Now().UTC().Format(time.RFC3339), Smoke: *smoke,
	}
	overall := time.Now()

	emit := func(r result) {
		fmt.Println(oneLine(r))
		run.Results = append(run.Results, r)
	}

	// Integrity first
	emit(wlIntegrity(bc, 1024, "integrity_1kb"))
	emit(wlIntegrity(bc, 1<<20, "integrity_1mb"))
	emit(wlIntegrity(bc, 16<<20, "integrity_16mb"))

	// Throughput
	emit(wlPut(bc, 1<<20, 5, "put_1mb_x5"))
	emit(wlGet(bc, 1<<20, 5, "get_1mb_x5"))
	emit(wlPut(bc, 16<<20, 3, "put_16mb_x3"))
	emit(wlGet(bc, 16<<20, 3, "get_16mb_x3"))
	if !*smoke {
		emit(wlPut(bc, 64<<20, 2, "put_64mb_x2"))
		emit(wlGet(bc, 64<<20, 2, "get_64mb_x2"))
	}

	// Parallel range download
	emit(wlRangeParallel(bc, "range_parallel_4x_64mb", 4))
	emit(wlRangeParallel(bc, "range_parallel_8x_64mb", 8))
	if !*smoke {
		emit(wlRangeParallel(bc, "range_parallel_16x_64mb", 16))
	}

	// Concurrent upload
	emit(wlConcurrentPut(bc, "concurrent_put_4w_1mb", 1<<20, 4, 20*time.Second))
	emit(wlConcurrentPut(bc, "concurrent_put_8w_1mb", 1<<20, 8, 20*time.Second))
	if !*smoke {
		emit(wlConcurrentPut(bc, "concurrent_put_4w_16mb", 16<<20, 4, 20*time.Second))
	}

	// Concurrent download — find the ceiling
	emit(wlConcurrentGet(bc, "concurrent_get_8w_1mb", 1<<20, 8, 20*time.Second))
	if !*smoke {
		emit(wlConcurrentGet(bc, "concurrent_get_8w_16mb", 16<<20, 8, 20*time.Second))
		emit(wlConcurrentGet(bc, "concurrent_get_16w_16mb", 16<<20, 16, 20*time.Second))
		emit(wlConcurrentGet(bc, "concurrent_get_32w_16mb", 16<<20, 32, 20*time.Second))
		emit(wlConcurrentGet(bc, "concurrent_get_64w_4mb", 4<<20, 64, 20*time.Second))

		// High concurrency upload
		emit(wlConcurrentPut(bc, "concurrent_put_16w_16mb", 16<<20, 16, 20*time.Second))
		emit(wlConcurrentPut(bc, "concurrent_put_32w_4mb", 4<<20, 32, 20*time.Second))

		// Large single files
		emit(wlPut(bc, 256<<20, 1, "put_256mb"))
		emit(wlGet(bc, 256<<20, 1, "get_256mb"))

		// Sustained 60s download (check for throttling)
		emit(wlConcurrentGet(bc, "sustained_get_8w_60s", 16<<20, 8, 60*time.Second))

		// Parallel range on 256MB (download accelerator pattern)
		emit(wlRangeParallel256(bc, "range_parallel_16x_256mb", 16))
	}

	// Cleanup
	fmt.Printf("\nCleaning up %d objects...\n", len(bc.keys))
	for _, k := range bc.keys {
		client.del(ctx, k)
	}

	run.DurSec = time.Since(overall).Seconds()
	data, _ := json.MarshalIndent(run, "", "  ")
	if err := os.WriteFile(*outFile, data, 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "write: %v\n", err)
	}
	fmt.Printf("\n✅ Done in %s. Results: %s\n", time.Since(overall).Round(time.Second), *outFile)
}
