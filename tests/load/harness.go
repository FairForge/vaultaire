package load

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/feature/s3/manager"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

func loadEndpoint() string {
	if v := os.Getenv("VAULTAIRE_LOAD_ENDPOINT"); v != "" {
		return v
	}
	return "http://localhost:8000"
}

func loadBucket() string {
	if v := os.Getenv("VAULTAIRE_LOAD_BUCKET"); v != "" {
		return v
	}
	return "load-test"
}

func loadAccessKey() string {
	if v := os.Getenv("VAULTAIRE_LOAD_ACCESS_KEY"); v != "" {
		return v
	}
	return os.Getenv("VAULTAIRE_BENCH_ACCESS_KEY")
}

func loadSecretKey() string {
	if v := os.Getenv("VAULTAIRE_LOAD_SECRET_KEY"); v != "" {
		return v
	}
	return os.Getenv("VAULTAIRE_BENCH_SECRET_KEY")
}

func skipIfNoServer(t *testing.T) {
	t.Helper()

	ak := loadAccessKey()
	sk := loadSecretKey()
	if ak == "" || sk == "" {
		t.Skip("VAULTAIRE_LOAD_ACCESS_KEY / SECRET_KEY (or BENCH_ fallback) not set")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, loadEndpoint()+"/health/live", nil)
	if err != nil {
		t.Skipf("cannot build health probe request: %v", err)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Skipf("server not reachable at %s: %v", loadEndpoint(), err)
	}
	_ = resp.Body.Close()
}

func newS3Client() *s3.Client {
	endpoint := loadEndpoint()
	return s3.New(s3.Options{
		BaseEndpoint: aws.String(endpoint),
		Region:       "us-east-1",
		Credentials: credentials.NewStaticCredentialsProvider(
			loadAccessKey(),
			loadSecretKey(),
			"",
		),
		UsePathStyle: true,
	})
}

func newUploader(client *s3.Client) *manager.Uploader { //nolint:staticcheck // manager.Uploader is deprecated in favor of transfermanager; migration is a post-launch WP
	return manager.NewUploader(client, func(u *manager.Uploader) { //nolint:staticcheck // manager.Uploader is deprecated in favor of transfermanager; migration is a post-launch WP
		u.PartSize = 16 << 20 // 16 MiB, matches production
		u.Concurrency = 8
	})
}

// patternReader produces size bytes from a repeating 4KB pattern. It implements
// io.ReadSeeker (zero-alloc) because the AWS SDK must seek the body to compute
// the SigV4 payload hash and to retry — a plain io.Reader fails PUT with
// "request stream is not seekable".
type patternReader struct {
	pattern []byte
	size    int64
	remain  int64
	pos     int
}

func newPatternReader(size int64) *patternReader {
	pat := make([]byte, 4096)
	for i := range pat {
		pat[i] = byte(i % 251) // prime avoids alignment artifacts
	}
	return &patternReader{pattern: pat, size: size, remain: size}
}

func (r *patternReader) Seek(offset int64, whence int) (int64, error) {
	var abs int64
	switch whence {
	case io.SeekStart:
		abs = offset
	case io.SeekCurrent:
		abs = (r.size - r.remain) + offset
	case io.SeekEnd:
		abs = r.size + offset
	default:
		return 0, fmt.Errorf("patternReader.Seek: invalid whence %d", whence)
	}
	if abs < 0 {
		return 0, fmt.Errorf("patternReader.Seek: negative position %d", abs)
	}
	if abs > r.size {
		abs = r.size
	}
	r.remain = r.size - abs
	r.pos = int(abs % int64(len(r.pattern)))
	return abs, nil
}

func (r *patternReader) Read(p []byte) (int, error) {
	if r.remain <= 0 {
		return 0, io.EOF
	}
	n := 0
	for n < len(p) && r.remain > 0 {
		chunk := len(r.pattern) - r.pos
		if int64(chunk) > r.remain {
			chunk = int(r.remain)
		}
		if chunk > len(p)-n {
			chunk = len(p) - n
		}
		copy(p[n:n+chunk], r.pattern[r.pos:r.pos+chunk])
		n += chunk
		r.remain -= int64(chunk)
		r.pos = (r.pos + chunk) % len(r.pattern)
	}
	return n, nil
}

type requestResult struct {
	duration time.Duration
	status   int
	bytes    int64
}

type metrics struct {
	mu      sync.Mutex
	results []requestResult
	start   time.Time
	name    string
}

func newMetrics(name string) *metrics {
	return &metrics{
		name:  name,
		start: time.Now(),
	}
}

func (m *metrics) record(d time.Duration, status int, bytes int64) {
	m.mu.Lock()
	m.results = append(m.results, requestResult{duration: d, status: status, bytes: bytes})
	m.mu.Unlock()
}

func (m *metrics) report(t *testing.T, p99Gate time.Duration) {
	t.Helper()
	m.mu.Lock()
	defer m.mu.Unlock()

	if len(m.results) == 0 {
		t.Errorf("[%s] GATE FAIL: no requests completed", m.name)
		return
	}

	elapsed := time.Since(m.start)

	// Sort latencies
	latencies := make([]time.Duration, len(m.results))
	statusHist := make(map[int]int)
	var totalBytes int64
	var count5xx int

	for i, r := range m.results {
		latencies[i] = r.duration
		statusHist[r.status]++
		totalBytes += r.bytes
		if r.status >= 500 {
			count5xx++
		}
	}
	sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })

	total := len(latencies)
	p50 := latencies[total/2]
	p99 := latencies[int(float64(total)*0.99)]

	// Status histogram string
	var statusParts []string
	for code, count := range statusHist {
		statusParts = append(statusParts, fmt.Sprintf("%d:%d", code, count))
	}
	sort.Strings(statusParts)

	t.Logf("[%s] Results:", m.name)
	t.Logf("  Requests:   %d in %s", total, elapsed.Round(time.Millisecond))
	t.Logf("  Throughput: %.1f ops/s, %.2f MB/s", float64(total)/elapsed.Seconds(), float64(totalBytes)/elapsed.Seconds()/1e6)
	t.Logf("  Latency:    p50=%s  p99=%s  max=%s", p50.Round(time.Millisecond), p99.Round(time.Millisecond), latencies[total-1].Round(time.Millisecond))
	t.Logf("  Status:     %s", strings.Join(statusParts, " "))

	// Gate: zero 5xx
	if count5xx > 0 {
		t.Errorf("[%s] GATE FAIL: %d requests returned 5xx", m.name, count5xx)
	}

	// Gate: p99 latency
	if p99Gate > 0 && p99 > p99Gate {
		t.Errorf("[%s] GATE FAIL: p99 %s exceeds gate %s", m.name, p99.Round(time.Millisecond), p99Gate)
	}
}

func checkGoroutineGrowth(t *testing.T, name string, before, after int) {
	t.Helper()
	growth := after - before
	if growth > 50 {
		t.Errorf("[%s] goroutine leak: %d before, %d after (growth %d > 50 threshold)", name, before, after, growth)
	} else {
		t.Logf("[%s] goroutines: %d before, %d after (growth %d)", name, before, after, growth)
	}
}

func ensureBucket(ctx context.Context, client *s3.Client, bucket string) error {
	_, err := client.CreateBucket(ctx, &s3.CreateBucketInput{
		Bucket: aws.String(bucket),
	})
	if err != nil {
		// Bucket already exists is fine
		if strings.Contains(err.Error(), "BucketAlreadyOwnedByYou") ||
			strings.Contains(err.Error(), "BucketAlreadyExists") {
			return nil
		}
		return fmt.Errorf("create bucket %s: %w", bucket, err)
	}
	return nil
}

func deleteObjects(ctx context.Context, client *s3.Client, bucket string, keys []string) {
	for _, key := range keys {
		_, _ = client.DeleteObject(ctx, &s3.DeleteObjectInput{
			Bucket: aws.String(bucket),
			Key:    aws.String(key),
		})
	}
}

func getJWT(endpoint, email, password string) (string, error) {
	body, _ := json.Marshal(map[string]string{
		"email":    email,
		"password": password,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint+"/auth/login", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("login request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("login returned %d", resp.StatusCode)
	}

	var result struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", fmt.Errorf("decode login response: %w", err)
	}
	if result.Token == "" {
		return "", fmt.Errorf("login response contained no token")
	}
	return result.Token, nil
}
