// cmd/bench-compare/main.go
//
// Multi-endpoint benchmark for Lyve / R2 / Quotaless / Geyser / iDrive e2 comparison.
// Reads credentials from environment (e.g. via `source .env.bench`).
// Emits one structured JSON file per host run for offline analysis.
//
// Usage:
//
//	source .env.bench
//	go run ./cmd/bench-compare                   # full matrix
//	go run ./cmd/bench-compare -smoke            # quick check
//	go run ./cmd/bench-compare -only lyve-us     # filter by substring
//	go run ./cmd/bench-compare -skip ap          # skip Asia-Pacific
//
// Flags:
//
//	-out PATH    JSON output file (default bench-results/<host>-<ts>.json)
//	-only LIST   substring filter for endpoint names (comma-separated)
//	-skip LIST   substring filter to skip endpoints
//	-smoke       quick mode (cold-dial + warm small + list only)
//	-host NAME   override hostname label in output
//
// All measured times are wall-clock from the client.
// Results are written incrementally so a partial run is still useful.
package main

import (
	"bytes"
	"context"
	"crypto/md5"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/user"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	smithylog "github.com/aws/smithy-go/logging"
)

// — Endpoint catalog ---------------------------------------------------------

type Endpoint struct {
	Name         string // friendly key, e.g. "lyve-us-east-1"
	Provider     string // "lyve", "r2", "quotaless", "geyser", "idrive"
	URL          string // S3 endpoint URL (static, or empty if URLEnv is set)
	URLEnv       string // env var holding the S3 endpoint URL (overrides URL if set)
	Region       string // AWS SDK region (must match Lyve region; "auto" for R2)
	AccessKeyEnv string // env var holding access key
	SecretKeyEnv string // env var holding secret key
	PathStyle    bool   // path-style addressing (Lyve, Quotaless, Geyser)
	InsecureTLS  bool   // skip TLS verification (Geyser certs are untrusted)
	FixedBucket  string // use this bucket instead of auto-generating one
	BucketEnv    string // env var holding the bucket name (overrides FixedBucket if set)
	KeyPrefix    string // prepended to all object keys (e.g. "personal-files/" for Quotaless)
}

// Order matters — printed and tested in this order.
var endpoints = []Endpoint{
	{Name: "lyve-us-east-1", Provider: "lyve", URL: "https://s3.us-east-1.global.lyve.seagate.com", Region: "us-east-1", AccessKeyEnv: "LYVE_ACCESS_KEY", SecretKeyEnv: "LYVE_SECRET_KEY", PathStyle: true},
	{Name: "lyve-us-west-1", Provider: "lyve", URL: "https://s3.us-west-1.global.lyve.seagate.com", Region: "us-west-1", AccessKeyEnv: "LYVE_ACCESS_KEY", SecretKeyEnv: "LYVE_SECRET_KEY", PathStyle: true},
	{Name: "lyve-us-central-2", Provider: "lyve", URL: "https://s3.us-central-2.global.lyve.seagate.com", Region: "us-central-2", AccessKeyEnv: "LYVE_ACCESS_KEY", SecretKeyEnv: "LYVE_SECRET_KEY", PathStyle: true},
	{Name: "lyve-eu-west-1", Provider: "lyve", URL: "https://s3.eu-west-1.global.lyve.seagate.com", Region: "eu-west-1", AccessKeyEnv: "LYVE_ACCESS_KEY", SecretKeyEnv: "LYVE_SECRET_KEY", PathStyle: true},
	{Name: "lyve-eu-central-1", Provider: "lyve", URL: "https://s3.eu-central-1.global.lyve.seagate.com", Region: "eu-central-1", AccessKeyEnv: "LYVE_ACCESS_KEY", SecretKeyEnv: "LYVE_SECRET_KEY", PathStyle: true},
	{Name: "lyve-ap-southeast-1", Provider: "lyve", URL: "https://s3.ap-southeast-1.global.lyve.seagate.com", Region: "ap-southeast-1", AccessKeyEnv: "LYVE_ACCESS_KEY", SecretKeyEnv: "LYVE_SECRET_KEY", PathStyle: true},
	{Name: "lyve-ap-northeast-1", Provider: "lyve", URL: "https://s3.ap-northeast-1.global.lyve.seagate.com", Region: "ap-northeast-1", AccessKeyEnv: "LYVE_ACCESS_KEY", SecretKeyEnv: "LYVE_SECRET_KEY", PathStyle: true},
	{Name: "r2-default", Provider: "r2", URLEnv: "R2_ENDPOINT", Region: "auto", AccessKeyEnv: "R2_ACCESS_KEY", SecretKeyEnv: "R2_SECRET_KEY"},
	{Name: "r2-eu", Provider: "r2", URLEnv: "R2_EU_ENDPOINT", Region: "auto", AccessKeyEnv: "R2_ACCESS_KEY", SecretKeyEnv: "R2_SECRET_KEY"},
	// Quotaless: Minio gateway over Storj. Requires UNSIGNED-PAYLOAD (see newClient).
	// Fixed bucket `data` with key prefix `personal-files/` per quotaless_README.md.
	// srv1-srv8 = specific servers. io. = dynamic LB (slower). us./nl./sg. = regional.
	{Name: "quotaless-srv1", Provider: "quotaless", URL: "https://srv1.quotaless.cloud:8000", Region: "us-east-1", AccessKeyEnv: "QUOTALESS_ACCESS_KEY", SecretKeyEnv: "QUOTALESS_SECRET_KEY", PathStyle: true, FixedBucket: "data", KeyPrefix: "personal-files/"},
	{Name: "quotaless-srv2", Provider: "quotaless", URL: "https://srv2.quotaless.cloud:8000", Region: "us-east-1", AccessKeyEnv: "QUOTALESS_ACCESS_KEY", SecretKeyEnv: "QUOTALESS_SECRET_KEY", PathStyle: true, FixedBucket: "data", KeyPrefix: "personal-files/"},
	{Name: "quotaless-us", Provider: "quotaless", URL: "https://us.quotaless.cloud:8000", Region: "us-east-1", AccessKeyEnv: "QUOTALESS_ACCESS_KEY", SecretKeyEnv: "QUOTALESS_SECRET_KEY", PathStyle: true, FixedBucket: "data", KeyPrefix: "personal-files/"},
	{Name: "quotaless-io", Provider: "quotaless", URL: "https://io.quotaless.cloud:8000", Region: "us-east-1", AccessKeyEnv: "QUOTALESS_ACCESS_KEY", SecretKeyEnv: "QUOTALESS_SECRET_KEY", PathStyle: true, FixedBucket: "data", KeyPrefix: "personal-files/"},
	{Name: "geyser-la", Provider: "geyser", URL: "https://la1.geyserdata.com", Region: "us-west-1", AccessKeyEnv: "GEYSER_ACCESS_KEY", SecretKeyEnv: "GEYSER_SECRET_KEY", PathStyle: true, InsecureTLS: true, BucketEnv: "GEYSER_LA_BUCKET"},
	// geyser-london: bucket deleted 2026-04-20, re-enable when recreated
	// {Name: "geyser-london", Provider: "geyser", URL: "https://lon1.geyserdata.com", Region: "eu-west-1", AccessKeyEnv: "GEYSER_ACCESS_KEY", SecretKeyEnv: "GEYSER_SECRET_KEY", PathStyle: true, InsecureTLS: true, BucketEnv: "GEYSER_LONDON_BUCKET"},
	// iDrive e2 — per-region keypairs (auth DB isolated per region)
	{Name: "idrive-us-east-1", Provider: "idrive", URL: "https://s3.us-east-1.idrivee2.com", Region: "us-east-1", AccessKeyEnv: "IDRIVE_US_EAST_1_ACCESS_KEY", SecretKeyEnv: "IDRIVE_US_EAST_1_SECRET_KEY", PathStyle: true, FixedBucket: "vaultaire-bench"},
	{Name: "idrive-us-southeast-1", Provider: "idrive", URL: "https://s3.us-southeast-1.idrivee2.com", Region: "us-southeast-1", AccessKeyEnv: "IDRIVE_US_SOUTHEAST_1_ACCESS_KEY", SecretKeyEnv: "IDRIVE_US_SOUTHEAST_1_SECRET_KEY", PathStyle: true, FixedBucket: "vaultaire-bench"},
	{Name: "idrive-us-midwest-1", Provider: "idrive", URL: "https://s3.us-midwest-1.idrivee2.com", Region: "us-midwest-1", AccessKeyEnv: "IDRIVE_US_MIDWEST_1_ACCESS_KEY", SecretKeyEnv: "IDRIVE_US_MIDWEST_1_SECRET_KEY", PathStyle: true, FixedBucket: "vaultaire-bench"},
	{Name: "idrive-us-central-1", Provider: "idrive", URL: "https://s3.us-central-1.idrivee2.com", Region: "us-central-1", AccessKeyEnv: "IDRIVE_US_CENTRAL_1_ACCESS_KEY", SecretKeyEnv: "IDRIVE_US_CENTRAL_1_SECRET_KEY", PathStyle: true, FixedBucket: "vaultaire-bench"},
	{Name: "idrive-us-southwest-1", Provider: "idrive", URL: "https://s3.us-southwest-1.idrivee2.com", Region: "us-southwest-1", AccessKeyEnv: "IDRIVE_US_SOUTHWEST_1_ACCESS_KEY", SecretKeyEnv: "IDRIVE_US_SOUTHWEST_1_SECRET_KEY", PathStyle: true, FixedBucket: "vaultaire-bench"},
	{Name: "idrive-us-west-1", Provider: "idrive", URL: "https://s3.us-west-1.idrivee2.com", Region: "us-west-1", AccessKeyEnv: "IDRIVE_US_WEST_1_ACCESS_KEY", SecretKeyEnv: "IDRIVE_US_WEST_1_SECRET_KEY", PathStyle: true, FixedBucket: "vaultaire-bench"},
	{Name: "idrive-us-west-2", Provider: "idrive", URL: "https://s3.us-west-2.idrivee2.com", Region: "us-west-2", AccessKeyEnv: "IDRIVE_US_WEST_2_ACCESS_KEY", SecretKeyEnv: "IDRIVE_US_WEST_2_SECRET_KEY", PathStyle: true, FixedBucket: "vaultaire-bench"},
	{Name: "idrive-ca-east-1", Provider: "idrive", URL: "https://s3.ca-east-1.idrivee2.com", Region: "ca-east-1", AccessKeyEnv: "IDRIVE_CA_EAST_1_ACCESS_KEY", SecretKeyEnv: "IDRIVE_CA_EAST_1_SECRET_KEY", PathStyle: true, FixedBucket: "vaultaire-bench"},
	{Name: "idrive-eu-west-1", Provider: "idrive", URL: "https://s3.eu-west-1.idrivee2.com", Region: "eu-west-1", AccessKeyEnv: "IDRIVE_EU_WEST_1_ACCESS_KEY", SecretKeyEnv: "IDRIVE_EU_WEST_1_SECRET_KEY", PathStyle: true, FixedBucket: "vaultaire-bench"},
	{Name: "idrive-eu-west-3", Provider: "idrive", URL: "https://s3.eu-west-3.idrivee2.com", Region: "eu-west-3", AccessKeyEnv: "IDRIVE_EU_WEST_3_ACCESS_KEY", SecretKeyEnv: "IDRIVE_EU_WEST_3_SECRET_KEY", PathStyle: true, FixedBucket: "vaultaire-bench"},
	{Name: "idrive-eu-west-4", Provider: "idrive", URL: "https://s3.eu-west-4.idrivee2.com", Region: "eu-west-4", AccessKeyEnv: "IDRIVE_EU_WEST_4_ACCESS_KEY", SecretKeyEnv: "IDRIVE_EU_WEST_4_SECRET_KEY", PathStyle: true, FixedBucket: "vaultaire-bench"},
	{Name: "idrive-eu-central-2", Provider: "idrive", URL: "https://s3.eu-central-2.idrivee2.com", Region: "eu-central-2", AccessKeyEnv: "IDRIVE_EU_CENTRAL_2_ACCESS_KEY", SecretKeyEnv: "IDRIVE_EU_CENTRAL_2_SECRET_KEY", PathStyle: true, FixedBucket: "vaultaire-bench"},
	{Name: "idrive-ap-southeast-1", Provider: "idrive", URL: "https://s3.ap-southeast-1.idrivee2.com", Region: "ap-southeast-1", AccessKeyEnv: "IDRIVE_AP_SOUTHEAST_1_ACCESS_KEY", SecretKeyEnv: "IDRIVE_AP_SOUTHEAST_1_SECRET_KEY", PathStyle: true, FixedBucket: "vaultaire-bench"},
}

// — Result types -------------------------------------------------------------

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

type EndpointResult struct {
	Name        string           `json:"name"`
	Provider    string           `json:"provider"`
	Region      string           `json:"region"`
	URL         string           `json:"url"`
	Bucket      string           `json:"bucket"`
	StartedAt   string           `json:"started_at"`
	FinishedAt  string           `json:"finished_at"`
	DurationSec float64          `json:"duration_sec"`
	SetupError  string           `json:"setup_error,omitempty"`
	Workloads   []WorkloadResult `json:"workloads,omitempty"`
}

type RunResult struct {
	Host        string           `json:"host"`
	OSArch      string           `json:"os_arch"`
	StartedAt   string           `json:"started_at"`
	FinishedAt  string           `json:"finished_at"`
	DurationSec float64          `json:"duration_sec"`
	Smoke       bool             `json:"smoke"`
	Endpoints   []EndpointResult `json:"endpoints"`
}

// — Main ---------------------------------------------------------------------

func main() {
	var (
		outFile = flag.String("out", "", "JSON output file (default: bench-results/<host>-<ts>.json)")
		only    = flag.String("only", "", "Comma-separated substring filter for endpoint names")
		skip    = flag.String("skip", "", "Comma-separated substring filter to skip endpoints")
		smoke   = flag.Bool("smoke", false, "Quick mode (cold dial + warm small + list)")
		host    = flag.String("host", "", "Host label for output (default: hostname)")
		onlyWL  = flag.String("only-workload", "", "Run only these workloads (substring match)")
		skipWL  = flag.String("skip-workload", "", "Skip these workloads (substring match)")
	)
	flag.Parse()

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
		*outFile = filepath.Join("bench-results", fmt.Sprintf("%s-%s.json", sanitize(hostName), ts))
	}

	if err := os.MkdirAll(filepath.Dir(*outFile), 0o750); err != nil {
		fmt.Fprintf(os.Stderr, "mkdir: %v\n", err)
		os.Exit(1)
	}

	eps := filterEndpoints(endpoints, *only, *skip)
	if len(eps) == 0 {
		fmt.Fprintln(os.Stderr, "no endpoints to test (check filters and credentials)")
		os.Exit(1)
	}

	fmt.Printf("Host:      %s (%s/%s)\n", hostName, runtime.GOOS, runtime.GOARCH)
	fmt.Printf("Output:    %s\n", *outFile)
	fmt.Printf("Transport: tuned (MaxIdleConnsPerHost=200, compression=off)\n")
	fmt.Printf("Endpoints: %d (smoke=%v)\n", len(eps), *smoke)

	run := RunResult{
		Host:      hostName,
		OSArch:    runtime.GOOS + "/" + runtime.GOARCH,
		StartedAt: time.Now().UTC().Format(time.RFC3339),
		Smoke:     *smoke,
	}
	overall := time.Now()

	onlyWLList := splitNonEmpty(*onlyWL)
	skipWLList := splitNonEmpty(*skipWL)

	for i, ep := range eps {
		fmt.Printf("\n[%d/%d] %s  (%s)\n", i+1, len(eps), ep.Name, ep.URL)
		fmt.Printf("─────────────────────────────────────────────────────────────────\n")

		result := runEndpointFiltered(context.Background(), ep, *smoke, onlyWLList, skipWLList)
		run.Endpoints = append(run.Endpoints, result)

		// Save progress after each endpoint so a partial run is usable.
		run.FinishedAt = time.Now().UTC().Format(time.RFC3339)
		run.DurationSec = time.Since(overall).Seconds()
		if err := writeJSON(*outFile, run); err != nil {
			fmt.Fprintf(os.Stderr, "  warn: progress save failed: %v\n", err)
		}
	}

	fmt.Printf("\n\n✅ Done in %s. Results: %s\n", time.Since(overall).Round(time.Second), *outFile)
}

// — Filtering ----------------------------------------------------------------

func filterEndpoints(in []Endpoint, only, skip string) []Endpoint {
	out := make([]Endpoint, 0, len(in))
	onlyList := splitNonEmpty(only)
	skipList := splitNonEmpty(skip)
	for _, ep := range in {
		if len(onlyList) > 0 && !anyContains(ep.Name, onlyList) {
			continue
		}
		if len(skipList) > 0 && anyContains(ep.Name, skipList) {
			continue
		}
		if os.Getenv(ep.AccessKeyEnv) == "" || os.Getenv(ep.SecretKeyEnv) == "" {
			fmt.Printf("⚠️  skipping %s: %s/%s not set\n", ep.Name, ep.AccessKeyEnv, ep.SecretKeyEnv)
			continue
		}
		if ep.URLEnv != "" {
			if u := os.Getenv(ep.URLEnv); u != "" {
				ep.URL = u
			}
		}
		if ep.URL == "" {
			fmt.Printf("⚠️  skipping %s: no URL (set %s)\n", ep.Name, ep.URLEnv)
			continue
		}
		if ep.BucketEnv != "" {
			if b := os.Getenv(ep.BucketEnv); b != "" {
				ep.FixedBucket = b
			} else if ep.FixedBucket == "" {
				fmt.Printf("⚠️  skipping %s: no bucket (set %s)\n", ep.Name, ep.BucketEnv)
				continue
			}
		}
		out = append(out, ep)
	}
	return out
}

func splitNonEmpty(s string) []string {
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

func anyContains(name string, list []string) bool {
	for _, s := range list {
		if strings.Contains(name, s) {
			return true
		}
	}
	return false
}

// — Per-endpoint runner ------------------------------------------------------

type wlContext struct {
	ctx        context.Context
	ep         Endpoint
	client     *s3.Client
	httpClient *http.Client
	bucket     string
	keyPrefix  string // prepended to all keys (e.g. "personal-files/" for Quotaless)

	keysMu sync.Mutex
	keys   []string
}

// key prepends the endpoint's required prefix to all object keys.
// Quotaless requires "personal-files/" prefix for all objects.
func (c *wlContext) key(path string) string {
	return c.keyPrefix + path
}

func (c *wlContext) track(k string) {
	c.keysMu.Lock()
	c.keys = append(c.keys, k)
	c.keysMu.Unlock()
}

type workload struct {
	name string
	fn   func(*wlContext) WorkloadResult
}

var allWorkloads = []workload{
	{"cold_dial_put_1kb", wlColdDial},
	{"warm_put_4kb", wlWarmPut},
	{"warm_get_4kb", wlWarmGet},
	{"warm_head_4kb", wlWarmHead},
	{"warm_copy_4kb", wlWarmCopy},
	{"warm_delete_single", wlDeleteSingle},
	{"delete_batch_100", wlDeleteBatch},
	{"list_100", wlList},
	{"list_prefix", wlListPrefix},
	{"medium_put_1mb", wlMediumPut1MB},
	{"medium_get_1mb", wlMediumGet1MB},
	{"medium_put_16mb", wlMediumPut16MB},
	{"medium_get_16mb", wlMediumGet16MB},
	{"large_put_64mb", wlLargePut64MB},
	{"large_get_64mb", wlLargeGet64MB},
	{"multipart_put_256mb", wlMultipart256MB},
	{"mpu_abort", wlMpuAbort},
	{"concurrent_ingest_20s", wlConcurrentIngest},
	{"concurrent_download_20s", wlConcurrentDownload},
	{"range_get_1mb_chunks", wlRangeGet1MBChunks},
	{"burst_small_files_500", wlBurstSmallFiles},
	{"integrity_16mb", wlIntegrity},
	{"integrity_robust_16mb", wlIntegrityRobust},
	{"integrity_chunked_16mb", wlIntegrityChunked},
}

// Smoke mode: only the small workloads.
var smokeWorkloads = []string{"cold_dial_put_1kb", "warm_put_4kb", "warm_get_4kb", "warm_head_4kb", "list_100"}

func runEndpointFiltered(ctx context.Context, ep Endpoint, smoke bool, onlyWL, skipWL []string) EndpointResult {
	res := EndpointResult{
		Name:      ep.Name,
		Provider:  ep.Provider,
		Region:    ep.Region,
		URL:       ep.URL,
		StartedAt: time.Now().UTC().Format(time.RFC3339),
	}
	start := time.Now()

	client, hc, err := newClient(ep)
	if err != nil {
		res.SetupError = fmt.Sprintf("client: %v", err)
		res.FinishedAt = time.Now().UTC().Format(time.RFC3339)
		res.DurationSec = time.Since(start).Seconds()
		fmt.Printf("  ❌ %s\n", res.SetupError)
		return res
	}

	bucket := bucketName(ep)
	res.Bucket = bucket

	if err := ensureBucket(ctx, client, ep, bucket); err != nil {
		res.SetupError = fmt.Sprintf("bucket: %v", err)
		res.FinishedAt = time.Now().UTC().Format(time.RFC3339)
		res.DurationSec = time.Since(start).Seconds()
		fmt.Printf("  ❌ %s\n", res.SetupError)
		return res
	}
	fmt.Printf("  ✓ bucket: %s\n", bucket)

	wlc := &wlContext{ctx: ctx, ep: ep, client: client, httpClient: hc, bucket: bucket, keyPrefix: ep.KeyPrefix}

	for _, w := range allWorkloads {
		if smoke && !contains(smokeWorkloads, w.name) {
			continue
		}
		if len(onlyWL) > 0 && !anyContains(w.name, onlyWL) {
			continue
		}
		if len(skipWL) > 0 && anyContains(w.name, skipWL) {
			continue
		}
		if skipWorkloadForProvider(ep.Provider, w.name) {
			fmt.Printf("  %-32s SKIPPED (incompatible with %s)\n", w.name, ep.Provider)
			continue
		}
		wl := w.fn(wlc)
		fmt.Printf("  %s\n", oneLine(wl))
		res.Workloads = append(res.Workloads, wl)
		hc.CloseIdleConnections()
	}

	cleanup(ctx, client, bucket, wlc.keys)

	res.FinishedAt = time.Now().UTC().Format(time.RFC3339)
	res.DurationSec = time.Since(start).Seconds()
	return res
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// skipWorkloadForProvider returns true if a workload is known-incompatible
// with a provider and should be skipped to avoid polluting results with
// XML deserialization or checksum errors that no transport tuning can fix.
func skipWorkloadForProvider(provider, workload string) bool {
	// Quotaless (Minio gateway over Storj) has broken response XML for these
	// operations per internal/drivers/quotaless_README.md (rule #10, lines 78-84).
	// UNSIGNED-PAYLOAD signing fixes put/get/head/delete but not list/batch/multipart/copy.
	if provider == "quotaless" {
		switch workload {
		case "list_100", "list_prefix", "delete_batch_100",
			"multipart_put_256mb", "mpu_abort", "warm_copy_4kb",
			"integrity_chunked_16mb":
			return true
		}
	}
	return false
}

func bucketName(ep Endpoint) string {
	if ep.FixedBucket != "" {
		return ep.FixedBucket
	}
	u, _ := user.Current()
	name := "vbench"
	if u != nil && u.Username != "" {
		name = "vbench-" + sanitize(u.Username)
	}
	// Lyve buckets are scoped per region, so the same name is OK across regions.
	// R2 buckets are account-global; same name re-used across runs.
	return name + "-" + sanitize(ep.Provider)
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

// — S3 client ----------------------------------------------------------------

// dnsCache caches resolved IP addresses to avoid repeated DNS lookups across
// connections within the same benchmark run. CDN endpoints (Pixeldrain, OneDrive)
// have 10ms RTT — eliminating 5-20ms DNS per connection adds up fast.
var dnsCache sync.Map // host → []string (resolved addrs)

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

// tunedHTTPClient returns an HTTP client with connection pooling, keep-alive,
// DNS caching, and TLS session resumption tuned for high-concurrency benchmarks.
func tunedHTTPClient(insecureTLS bool) *http.Client {
	dialer := &net.Dialer{
		Timeout:   10 * time.Second,
		KeepAlive: 30 * time.Second,
	}
	transport := &http.Transport{
		MaxIdleConns:        200,
		MaxIdleConnsPerHost: 200,
		IdleConnTimeout:     90 * time.Second,
		TLSHandshakeTimeout: 10 * time.Second,
		ReadBufferSize:      1 << 20,
		WriteBufferSize:     1 << 20,
		DisableCompression:  true,
		ForceAttemptHTTP2:   true,
		DialContext:         cachedDialContext(dialer),
		TLSClientConfig: &tls.Config{
			MinVersion:         tls.VersionTLS12,
			ClientSessionCache: tls.NewLRUClientSessionCache(128),
		},
	}
	if insecureTLS {
		transport.TLSClientConfig.InsecureSkipVerify = true //nolint:gosec // benchmark tool, Geyser certs untrusted
	}
	return &http.Client{Transport: transport}
}

func newClient(ep Endpoint) (*s3.Client, *http.Client, error) {
	ak := os.Getenv(ep.AccessKeyEnv)
	sk := os.Getenv(ep.SecretKeyEnv)
	if ak == "" || sk == "" {
		return nil, nil, fmt.Errorf("missing %s or %s", ep.AccessKeyEnv, ep.SecretKeyEnv)
	}
	hc := tunedHTTPClient(ep.InsecureTLS)
	cfg, err := awsconfig.LoadDefaultConfig(context.Background(),
		awsconfig.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(ak, sk, "")),
		awsconfig.WithRegion(ep.Region),
		awsconfig.WithLogger(smithylog.Nop{}),
		awsconfig.WithHTTPClient(hc),
	)
	if err != nil {
		return nil, nil, err
	}
	return s3.NewFromConfig(cfg, func(o *s3.Options) {
		o.BaseEndpoint = aws.String(ep.URL)
		o.UsePathStyle = ep.PathStyle
		o.Logger = smithylog.Nop{}
		o.ClientLogMode = 0
		// Non-AWS S3-compatible providers often don't implement the flexible checksum
		// algorithms introduced in SDK v2 — they return "checksum not available yet"
		// or return 200 with malformed XML. Only compute/validate when explicitly required.
		o.RequestChecksumCalculation = aws.RequestChecksumCalculationWhenRequired
		o.ResponseChecksumValidation = aws.ResponseChecksumValidationWhenRequired
		// Quotaless (Minio gateway over Storj) requires x-amz-content-sha256=UNSIGNED-PAYLOAD
		// for signing. The SDK default (streaming SHA256) corrupts data on upload per
		// internal/drivers/quotaless_README.md. Swap it for all quotaless endpoints.
		if ep.Provider == "quotaless" {
			o.APIOptions = append(o.APIOptions, v4.SwapComputePayloadSHA256ForUnsignedPayloadMiddleware)
		}
	}), hc, nil
}

func ensureBucket(ctx context.Context, client *s3.Client, ep Endpoint, bucket string) error {
	// FixedBucket means the user pre-provisioned it. Trust it and don't attempt create.
	// Geyser can't CreateBucket via S3 (AccessDenied); quotaless 'data' exists; iDrive
	// 'vaultaire-bench' is pre-created. HeadBucket can also timeout on cold Geyser TLS.
	if ep.FixedBucket != "" {
		return nil
	}
	hctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	_, err := client.HeadBucket(hctx, &s3.HeadBucketInput{Bucket: aws.String(bucket)})
	if err == nil {
		return nil
	}
	cctx, cancel2 := context.WithTimeout(ctx, 30*time.Second)
	defer cancel2()
	_, cerr := client.CreateBucket(cctx, &s3.CreateBucketInput{Bucket: aws.String(bucket)})
	if cerr == nil {
		return nil
	}
	var owned *s3types.BucketAlreadyOwnedByYou
	if errors.As(cerr, &owned) {
		return nil
	}
	var exists *s3types.BucketAlreadyExists
	if errors.As(cerr, &exists) {
		return nil
	}
	return fmt.Errorf("create: %w (head: %v)", cerr, err)
}

func cleanup(ctx context.Context, client *s3.Client, bucket string, keys []string) {
	if len(keys) == 0 {
		return
	}
	dctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	sem := make(chan struct{}, 16)
	var wg sync.WaitGroup
	for _, k := range keys {
		sem <- struct{}{}
		wg.Add(1)
		go func(key string) {
			defer wg.Done()
			defer func() { <-sem }()
			_, _ = client.DeleteObject(dctx, &s3.DeleteObjectInput{
				Bucket: aws.String(bucket),
				Key:    aws.String(key),
			})
		}(k)
	}
	wg.Wait()
}

// — PUT/GET helpers ----------------------------------------------------------

func putObject(ctx context.Context, c *s3.Client, bucket, key string, data []byte) (time.Duration, error) {
	start := time.Now()
	_, err := c.PutObject(ctx, &s3.PutObjectInput{
		Bucket:        aws.String(bucket),
		Key:           aws.String(key),
		Body:          bytes.NewReader(data),
		ContentLength: aws.Int64(int64(len(data))),
	})
	return time.Since(start), err
}

func getObject(ctx context.Context, c *s3.Client, bucket, key string) ([]byte, time.Duration, error) {
	start := time.Now()
	resp, err := c.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return nil, time.Since(start), err
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(resp.Body)
	return data, time.Since(start), err
}

// getObjectStream discards the body via io.Copy, avoiding large heap allocations
// for throughput-only tests (64MB GET would otherwise allocate a 64MB buffer).
func getObjectStream(ctx context.Context, c *s3.Client, bucket, key string) (int64, time.Duration, error) {
	start := time.Now()
	resp, err := c.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return 0, time.Since(start), err
	}
	defer func() { _ = resp.Body.Close() }()
	n, err := io.Copy(io.Discard, resp.Body)
	return n, time.Since(start), err
}

// getObjectRobust downloads an object defensively:
//   - HEAD to learn expected size
//   - Range-fetch in 8MB chunks (smaller chunks = less truncation risk)
//   - Retry each short chunk up to 3 times
//   - Return assembled bytes only when total == expected size
//
// This is Vaultaire's thesis: client-side smarts turn unreliable
// backends (that truncate large GETs or flake mid-stream) into reliable ones.
func getObjectRobust(ctx context.Context, c *s3.Client, bucket, key string) ([]byte, time.Duration, int, error) {
	const chunkSize int64 = 8 << 20 // 8MB
	const maxRetries = 3
	start := time.Now()

	head, err := c.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return nil, time.Since(start), 0, fmt.Errorf("head: %w", err)
	}
	size := aws.ToInt64(head.ContentLength)
	if size == 0 {
		return nil, time.Since(start), 0, fmt.Errorf("HEAD reported zero size")
	}

	out := make([]byte, size)
	retries := 0
	for offset := int64(0); offset < size; {
		end := offset + chunkSize - 1
		if end >= size {
			end = size - 1
		}
		chunkLen := end - offset + 1

		var got int64
		var lastErr error
		for attempt := 0; attempt < maxRetries; attempt++ {
			resp, err := c.GetObject(ctx, &s3.GetObjectInput{
				Bucket: aws.String(bucket),
				Key:    aws.String(key),
				Range:  aws.String(fmt.Sprintf("bytes=%d-%d", offset, end)),
			})
			if err != nil {
				lastErr = err
				retries++
				continue
			}
			n, err := io.ReadFull(resp.Body, out[offset:offset+chunkLen])
			_ = resp.Body.Close()
			got = int64(n)
			if err == nil && got == chunkLen {
				break
			}
			lastErr = err
			retries++
		}
		if got != chunkLen {
			return nil, time.Since(start), retries, fmt.Errorf("range [%d-%d] short after %d retries: got %d/%d: %v", offset, end, maxRetries, got, chunkLen, lastErr)
		}
		offset = end + 1
	}
	return out, time.Since(start), retries, nil
}

// putObjectVerified uploads then HEADs to verify the ETag matches the local MD5.
// Retries on mismatch — this defends against backends that corrupt/truncate uploads.
func putObjectVerified(ctx context.Context, c *s3.Client, bucket, key string, data []byte) (time.Duration, int, error) {
	const maxRetries = 3
	start := time.Now()
	wantMD5 := fmt.Sprintf("%x", md5Sum(data))

	var lastErr error
	for attempt := 0; attempt < maxRetries; attempt++ {
		_, err := c.PutObject(ctx, &s3.PutObjectInput{
			Bucket:        aws.String(bucket),
			Key:           aws.String(key),
			Body:          bytes.NewReader(data),
			ContentLength: aws.Int64(int64(len(data))),
		})
		if err != nil {
			lastErr = err
			continue
		}
		head, err := c.HeadObject(ctx, &s3.HeadObjectInput{
			Bucket: aws.String(bucket),
			Key:    aws.String(key),
		})
		if err != nil {
			lastErr = fmt.Errorf("verify HEAD: %w", err)
			continue
		}
		// ETag may be quoted or include a -N suffix for multipart; compare prefix.
		etag := strings.Trim(aws.ToString(head.ETag), `"`)
		etag = strings.SplitN(etag, "-", 2)[0]
		if etag == wantMD5 {
			return time.Since(start), attempt, nil
		}
		lastErr = fmt.Errorf("etag mismatch: want=%s got=%s", wantMD5, etag)
	}
	return time.Since(start), maxRetries, lastErr
}

func headObject(ctx context.Context, c *s3.Client, bucket, key string) (time.Duration, error) {
	start := time.Now()
	_, err := c.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(key),
	})
	return time.Since(start), err
}

// seedObjects creates `count` objects of `sizeBytes` in parallel and returns
// the keys that succeeded. Used for setup phases (not measured as workload).
func seedObjects(c *wlContext, prefix string, count int, sizeBytes int) []string {
	payload := randBytes(sizeBytes)
	keys := make([]string, count)
	for i := range keys {
		keys[i] = c.key(fmt.Sprintf("%s/%d-%d", prefix, time.Now().UnixNano(), i))
	}
	sem := make(chan struct{}, 16)
	var wg sync.WaitGroup
	var failed atomic.Int32
	for i, k := range keys {
		sem <- struct{}{}
		wg.Add(1)
		go func(idx int, key string) {
			defer wg.Done()
			defer func() { <-sem }()
			if _, err := putObject(c.ctx, c.client, c.bucket, key, payload); err != nil {
				failed.Add(1)
				keys[idx] = ""
				return
			}
			c.track(key)
		}(i, k)
	}
	wg.Wait()
	out := make([]string, 0, count)
	for _, k := range keys {
		if k != "" {
			out = append(out, k)
		}
	}
	return out
}

// — Helpers ------------------------------------------------------------------

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

func md5Sum(b []byte) []byte {
	h := md5.Sum(b) //nolint:gosec // MD5 used for S3 ETag comparison, not security
	return h[:]
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
	p50 = s[idx(50)]
	p95 = s[idx(95)]
	p99 = s[idx(99)]
	mx = s[len(s)-1]
	return
}

func mbps(b int64, d time.Duration) float64 {
	if d <= 0 {
		return 0
	}
	return float64(b) / 1024 / 1024 / d.Seconds()
}

func msInt(d time.Duration) int64 { return d.Milliseconds() }

func oneLine(w WorkloadResult) string {
	if w.Skipped {
		return fmt.Sprintf("%-22s SKIP — %s", w.Name, w.Note)
	}
	if w.Error != "" {
		return fmt.Sprintf("%-22s ERROR — %s", w.Name, w.Error)
	}
	parts := []string{fmt.Sprintf("%-22s %6dms", w.Name, w.DurationMS)}
	if w.MBps > 0 {
		parts = append(parts, fmt.Sprintf("%6.1f MB/s", w.MBps))
	}
	if w.OpsPerSec > 0 {
		parts = append(parts, fmt.Sprintf("%5.0f ops/s", w.OpsPerSec))
	}
	if w.P95MS > 0 {
		parts = append(parts, fmt.Sprintf("p50=%dms p95=%dms p99=%dms", w.P50MS, w.P95MS, w.P99MS))
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
	f, err := os.Create(tmp) // #nosec G304 — path is controlled by caller
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

// — Workloads ----------------------------------------------------------------

// 1. cold_dial_put_1kb — fresh client per iteration. Captures TLS+TCP+TTFB.
func wlColdDial(c *wlContext) WorkloadResult {
	const N = 10
	payload := randBytes(1024)
	var times []time.Duration
	var errs int
	overall := time.Now()
	for i := 0; i < N; i++ {
		client, _, err := newClient(c.ep)
		if err != nil {
			errs++
			continue
		}
		key := c.key(fmt.Sprintf("bench/cold/%d-%d", time.Now().UnixNano(), i))
		d, err := putObject(c.ctx, client, c.bucket, key, payload)
		if err != nil {
			errs++
			continue
		}
		times = append(times, d)
		c.track(key)
	}
	p50, p95, p99, mx := percentiles(times)
	return WorkloadResult{
		Name:        "cold_dial_put_1kb",
		Description: "10× fresh-client 1KB PUT (TLS+TCP+TTFB)",
		Ops:         len(times),
		Errors:      errs,
		DurationMS:  msInt(time.Since(overall)),
		P50MS:       msInt(p50),
		P95MS:       msInt(p95),
		P99MS:       msInt(p99),
		MaxMS:       msInt(mx),
	}
}

// 2. warm_put_4kb — 100× warm 4KB serial PUT
func wlWarmPut(c *wlContext) WorkloadResult {
	const N = 100
	payload := randBytes(4 * 1024)
	var times []time.Duration
	var errs int
	overall := time.Now()
	for i := 0; i < N; i++ {
		key := c.key(fmt.Sprintf("bench/warm/%d-%d", time.Now().UnixNano(), i))
		d, err := putObject(c.ctx, c.client, c.bucket, key, payload)
		if err != nil {
			errs++
			continue
		}
		times = append(times, d)
		c.track(key)
	}
	elapsed := time.Since(overall)
	p50, p95, p99, mx := percentiles(times)
	ops := len(times)
	return WorkloadResult{
		Name:        "warm_put_4kb",
		Description: "100× warm 4KB PUT serial",
		Ops:         ops,
		Errors:      errs,
		Bytes:       int64(ops) * 4 * 1024,
		DurationMS:  msInt(elapsed),
		P50MS:       msInt(p50),
		P95MS:       msInt(p95),
		P99MS:       msInt(p99),
		MaxMS:       msInt(mx),
		OpsPerSec:   float64(ops) / elapsed.Seconds(),
	}
}

// 3. warm_get_4kb — pre-seed 100 4KB objects, then GET them serially.
func wlWarmGet(c *wlContext) WorkloadResult {
	const N = 100
	payload := randBytes(4 * 1024)
	keys := make([]string, 0, N)
	for i := 0; i < N; i++ {
		key := c.key(fmt.Sprintf("bench/warm-get/%d-%d", time.Now().UnixNano(), i))
		if _, err := putObject(c.ctx, c.client, c.bucket, key, payload); err == nil {
			keys = append(keys, key)
			c.track(key)
		}
	}
	var times []time.Duration
	var errs int
	overall := time.Now()
	for _, k := range keys {
		_, d, err := getObject(c.ctx, c.client, c.bucket, k)
		if err != nil {
			errs++
			continue
		}
		times = append(times, d)
	}
	elapsed := time.Since(overall)
	p50, p95, p99, mx := percentiles(times)
	ops := len(times)
	return WorkloadResult{
		Name:        "warm_get_4kb",
		Description: "100× warm 4KB GET serial",
		Ops:         ops,
		Errors:      errs,
		Bytes:       int64(ops) * 4 * 1024,
		DurationMS:  msInt(elapsed),
		P50MS:       msInt(p50),
		P95MS:       msInt(p95),
		P99MS:       msInt(p99),
		MaxMS:       msInt(mx),
		OpsPerSec:   float64(ops) / elapsed.Seconds(),
	}
}

func mediumPut(c *wlContext, name string, sizeBytes int, count int) WorkloadResult {
	payload := randBytes(sizeBytes)
	var times []time.Duration
	var errs int
	var totalBytes int64
	overall := time.Now()
	for i := 0; i < count; i++ {
		key := c.key(fmt.Sprintf("bench/%s/%d-%d", name, time.Now().UnixNano(), i))
		d, err := putObject(c.ctx, c.client, c.bucket, key, payload)
		if err != nil {
			errs++
			continue
		}
		times = append(times, d)
		c.track(key)
		totalBytes += int64(sizeBytes)
	}
	elapsed := time.Since(overall)
	p50, p95, p99, mx := percentiles(times)
	return WorkloadResult{
		Name:        name,
		Description: fmt.Sprintf("%d × %dMB PUT", count, sizeBytes/(1024*1024)),
		Ops:         len(times),
		Errors:      errs,
		Bytes:       totalBytes,
		DurationMS:  msInt(elapsed),
		P50MS:       msInt(p50),
		P95MS:       msInt(p95),
		P99MS:       msInt(p99),
		MaxMS:       msInt(mx),
		MBps:        mbps(totalBytes, elapsed),
	}
}

func mediumGet(c *wlContext, name string, sizeBytes int, count int) WorkloadResult {
	// Seed objects, then GET each.
	payload := randBytes(sizeBytes)
	keys := make([]string, 0, count)
	for i := 0; i < count; i++ {
		key := c.key(fmt.Sprintf("bench/%s-seed/%d-%d", name, time.Now().UnixNano(), i))
		if _, err := putObject(c.ctx, c.client, c.bucket, key, payload); err == nil {
			keys = append(keys, key)
			c.track(key)
		}
	}
	var times []time.Duration
	var errs int
	var totalBytes int64
	overall := time.Now()
	for _, k := range keys {
		n, d, err := getObjectStream(c.ctx, c.client, c.bucket, k)
		if err != nil {
			errs++
			continue
		}
		times = append(times, d)
		totalBytes += n
	}
	elapsed := time.Since(overall)
	p50, p95, p99, mx := percentiles(times)
	return WorkloadResult{
		Name:        name,
		Description: fmt.Sprintf("%d × %dMB GET", count, sizeBytes/(1024*1024)),
		Ops:         len(times),
		Errors:      errs,
		Bytes:       totalBytes,
		DurationMS:  msInt(elapsed),
		P50MS:       msInt(p50),
		P95MS:       msInt(p95),
		P99MS:       msInt(p99),
		MaxMS:       msInt(mx),
		MBps:        mbps(totalBytes, elapsed),
	}
}

// 4-7. medium PUT/GET at 1MB and 16MB
func wlMediumPut1MB(c *wlContext) WorkloadResult {
	return mediumPut(c, "medium_put_1mb", 1*1024*1024, 5)
}
func wlMediumGet1MB(c *wlContext) WorkloadResult {
	return mediumGet(c, "medium_get_1mb", 1*1024*1024, 5)
}
func wlMediumPut16MB(c *wlContext) WorkloadResult {
	return mediumPut(c, "medium_put_16mb", 16*1024*1024, 5)
}
func wlMediumGet16MB(c *wlContext) WorkloadResult {
	return mediumGet(c, "medium_get_16mb", 16*1024*1024, 5)
}

// 8. large_put_64mb — single large PutObject
func wlLargePut64MB(c *wlContext) WorkloadResult {
	const sz = 64 * 1024 * 1024
	payload := randBytes(sz)
	key := c.key(fmt.Sprintf("bench/large/%d", time.Now().UnixNano()))
	overall := time.Now()
	d, err := putObject(c.ctx, c.client, c.bucket, key, payload)
	elapsed := time.Since(overall)
	if err != nil {
		return WorkloadResult{Name: "large_put_64mb", Error: err.Error(), DurationMS: msInt(elapsed)}
	}
	c.track(key)
	return WorkloadResult{
		Name:        "large_put_64mb",
		Description: "1× 64MB PUT",
		Ops:         1,
		Bytes:       sz,
		DurationMS:  msInt(d),
		MBps:        mbps(sz, d),
	}
}

// 9. large_get_64mb — seed and GET a 64MB object
func wlLargeGet64MB(c *wlContext) WorkloadResult {
	const sz = 64 * 1024 * 1024
	payload := randBytes(sz)
	key := c.key(fmt.Sprintf("bench/large-get/%d", time.Now().UnixNano()))
	if _, err := putObject(c.ctx, c.client, c.bucket, key, payload); err != nil {
		return WorkloadResult{Name: "large_get_64mb", Error: "seed failed: " + err.Error()}
	}
	c.track(key)
	n, d, err := getObjectStream(c.ctx, c.client, c.bucket, key)
	if err != nil {
		return WorkloadResult{Name: "large_get_64mb", Error: err.Error(), DurationMS: msInt(d)}
	}
	return WorkloadResult{
		Name:        "large_get_64mb",
		Description: "1× 64MB GET",
		Ops:         1,
		Bytes:       n,
		DurationMS:  msInt(d),
		MBps:        mbps(n, d),
	}
}

// 10. multipart_put_256mb — explicit multipart upload, 16MB parts, parallel 4.
func wlMultipart256MB(c *wlContext) WorkloadResult {
	const (
		total    = 256 * 1024 * 1024
		partSize = 16 * 1024 * 1024
		parallel = 4
	)
	parts := total / partSize
	payload := randBytes(partSize)
	key := c.key(fmt.Sprintf("bench/multipart/%d", time.Now().UnixNano()))

	mctx, cancel := context.WithTimeout(c.ctx, 10*time.Minute)
	defer cancel()

	overall := time.Now()
	cmu, err := c.client.CreateMultipartUpload(mctx, &s3.CreateMultipartUploadInput{
		Bucket: aws.String(c.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return WorkloadResult{Name: "multipart_put_256mb", Error: "create: " + err.Error()}
	}
	uploadID := cmu.UploadId

	completed := make([]s3types.CompletedPart, parts)
	var failed atomic.Int32
	sem := make(chan struct{}, parallel)
	var wg sync.WaitGroup
	for i := 0; i < parts; i++ {
		sem <- struct{}{}
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			defer func() { <-sem }()
			partNum := int32(idx + 1)
			out, err := c.client.UploadPart(mctx, &s3.UploadPartInput{
				Bucket:        aws.String(c.bucket),
				Key:           aws.String(key),
				UploadId:      uploadID,
				PartNumber:    aws.Int32(partNum),
				Body:          bytes.NewReader(payload),
				ContentLength: aws.Int64(int64(partSize)),
			})
			if err != nil {
				failed.Add(1)
				return
			}
			completed[idx] = s3types.CompletedPart{
				ETag:       out.ETag,
				PartNumber: aws.Int32(partNum),
			}
		}(i)
	}
	wg.Wait()
	if failed.Load() > 0 {
		_, _ = c.client.AbortMultipartUpload(mctx, &s3.AbortMultipartUploadInput{
			Bucket:   aws.String(c.bucket),
			Key:      aws.String(key),
			UploadId: uploadID,
		})
		return WorkloadResult{
			Name:       "multipart_put_256mb",
			Error:      fmt.Sprintf("%d parts failed", failed.Load()),
			DurationMS: msInt(time.Since(overall)),
		}
	}

	_, err = c.client.CompleteMultipartUpload(mctx, &s3.CompleteMultipartUploadInput{
		Bucket:          aws.String(c.bucket),
		Key:             aws.String(key),
		UploadId:        uploadID,
		MultipartUpload: &s3types.CompletedMultipartUpload{Parts: completed},
	})
	elapsed := time.Since(overall)
	if err != nil {
		return WorkloadResult{Name: "multipart_put_256mb", Error: "complete: " + err.Error(), DurationMS: msInt(elapsed)}
	}
	c.track(key)
	return WorkloadResult{
		Name:        "multipart_put_256mb",
		Description: fmt.Sprintf("256MB MPU, %d×%dMB parts, parallel=%d", parts, partSize/(1024*1024), parallel),
		Ops:         parts,
		Bytes:       total,
		DurationMS:  msInt(elapsed),
		MBps:        mbps(total, elapsed),
	}
}

// concurrent_ingest_20s — 32 parallel × 4MB PUTs, capped at 20s OR 1.0 GB.
func wlConcurrentIngest(c *wlContext) WorkloadResult {
	const (
		workers   = 32
		objSize   = 4 * 1024 * 1024
		maxDur    = 20 * time.Second
		maxBytes  = int64(1024) * 1024 * 1024 // 1 GB cap
		batchName = "concurrent_ingest_20s"
	)
	payload := randBytes(objSize)
	ctx, cancel := context.WithTimeout(c.ctx, maxDur)
	defer cancel()

	var (
		bytesUp  atomic.Int64
		ops      atomic.Int64
		errs     atomic.Int64
		latencyM sync.Mutex
		latency  []time.Duration
	)

	overall := time.Now()
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			i := 0
			for ctx.Err() == nil {
				if bytesUp.Load() >= maxBytes {
					return
				}
				key := c.key(fmt.Sprintf("bench/concurrent/w%d-%d", workerID, i))
				d, err := putObject(ctx, c.client, c.bucket, key, payload)
				if ctx.Err() != nil {
					return
				}
				if err != nil {
					errs.Add(1)
					i++
					continue
				}
				ops.Add(1)
				bytesUp.Add(int64(objSize))
				latencyM.Lock()
				latency = append(latency, d)
				latencyM.Unlock()
				c.track(key)
				i++
			}
		}(w)
	}
	wg.Wait()

	elapsed := time.Since(overall)
	p50, p95, p99, mx := percentiles(latency)
	totalOps := int(ops.Load())
	totalBytes := bytesUp.Load()
	return WorkloadResult{
		Name:        batchName,
		Description: fmt.Sprintf("%d parallel × 4MB PUT for %s (cap %dMB)", workers, maxDur, maxBytes/1024/1024),
		Ops:         totalOps,
		Errors:      int(errs.Load()),
		Bytes:       totalBytes,
		DurationMS:  msInt(elapsed),
		P50MS:       msInt(p50),
		P95MS:       msInt(p95),
		P99MS:       msInt(p99),
		MaxMS:       msInt(mx),
		MBps:        mbps(totalBytes, elapsed),
		OpsPerSec:   float64(totalOps) / elapsed.Seconds(),
	}
}

// concurrent_download_20s — 32 parallel × 4MB GETs, capped at 20s OR 1.0 GB.
// Mirrors multi-user download or CDN pull pattern.
func wlConcurrentDownload(c *wlContext) WorkloadResult {
	const (
		workers  = 32
		objSize  = 4 * 1024 * 1024
		maxDur   = 20 * time.Second
		maxBytes = int64(1024) * 1024 * 1024
	)

	payload := randBytes(objSize)
	seedKey := c.key(fmt.Sprintf("bench/concurrent-dl/%d", time.Now().UnixNano()))
	if _, err := putObject(c.ctx, c.client, c.bucket, seedKey, payload); err != nil {
		return WorkloadResult{Name: "concurrent_download_20s", Error: "seed: " + err.Error()}
	}
	c.track(seedKey)

	ctx, cancel := context.WithTimeout(c.ctx, maxDur)
	defer cancel()

	var (
		bytesDown atomic.Int64
		ops       atomic.Int64
		errs      atomic.Int64
		latencyM  sync.Mutex
		latency   []time.Duration
	)

	overall := time.Now()
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for ctx.Err() == nil {
				if bytesDown.Load() >= maxBytes {
					return
				}
				start := time.Now()
				resp, err := c.client.GetObject(ctx, &s3.GetObjectInput{
					Bucket: aws.String(c.bucket),
					Key:    aws.String(seedKey),
				})
				if ctx.Err() != nil {
					return
				}
				if err != nil {
					errs.Add(1)
					continue
				}
				n, _ := io.Copy(io.Discard, resp.Body)
				_ = resp.Body.Close()
				d := time.Since(start)

				ops.Add(1)
				bytesDown.Add(n)
				latencyM.Lock()
				latency = append(latency, d)
				latencyM.Unlock()
			}
		}()
	}
	wg.Wait()

	elapsed := time.Since(overall)
	p50, p95, p99, mx := percentiles(latency)
	totalOps := int(ops.Load())
	totalBytes := bytesDown.Load()
	return WorkloadResult{
		Name:        "concurrent_download_20s",
		Description: fmt.Sprintf("%d parallel × 4MB GET for %s (cap %dMB)", workers, maxDur, maxBytes/1024/1024),
		Ops:         totalOps,
		Errors:      int(errs.Load()),
		Bytes:       totalBytes,
		DurationMS:  msInt(elapsed),
		P50MS:       msInt(p50),
		P95MS:       msInt(p95),
		P99MS:       msInt(p99),
		MaxMS:       msInt(mx),
		MBps:        mbps(totalBytes, elapsed),
		OpsPerSec:   float64(totalOps) / elapsed.Seconds(),
	}
}

// range_get_1mb_chunks — GET 1MB ranges from a 16MB object, 16 times.
// Mirrors JuiceFS block-read pattern (default 4MB blocks, but 1MB is a
// stricter test for small-range efficiency).
func wlRangeGet1MBChunks(c *wlContext) WorkloadResult {
	const (
		totalSize = 16 * 1024 * 1024
		chunkSize = 1 * 1024 * 1024
		chunks    = totalSize / chunkSize
	)
	seedKey := c.key(fmt.Sprintf("bench/range-seed/%d", time.Now().UnixNano()))
	payload := randBytes(totalSize)
	if _, err := putObject(c.ctx, c.client, c.bucket, seedKey, payload); err != nil {
		return WorkloadResult{Name: "range_get_1mb_chunks", Error: "seed: " + err.Error()}
	}
	c.track(seedKey)

	var times []time.Duration
	var errs int
	var totalBytes int64
	overall := time.Now()
	for i := 0; i < chunks; i++ {
		start := i * chunkSize
		end := start + chunkSize - 1
		rng := fmt.Sprintf("bytes=%d-%d", start, end)
		t0 := time.Now()
		resp, err := c.client.GetObject(c.ctx, &s3.GetObjectInput{
			Bucket: aws.String(c.bucket),
			Key:    aws.String(seedKey),
			Range:  aws.String(rng),
		})
		if err != nil {
			errs++
			continue
		}
		n, _ := io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		times = append(times, time.Since(t0))
		totalBytes += n
	}
	elapsed := time.Since(overall)
	p50, p95, p99, mx := percentiles(times)
	return WorkloadResult{
		Name:        "range_get_1mb_chunks",
		Description: fmt.Sprintf("%d × 1MB Range GET on 16MB object", chunks),
		Ops:         len(times),
		Errors:      errs,
		Bytes:       totalBytes,
		DurationMS:  msInt(elapsed),
		P50MS:       msInt(p50),
		P95MS:       msInt(p95),
		P99MS:       msInt(p99),
		MaxMS:       msInt(mx),
		MBps:        mbps(totalBytes, elapsed),
		OpsPerSec:   float64(len(times)) / elapsed.Seconds(),
	}
}

// burst_small_files_500 — 500 × 4KB PUTs with 10 concurrent workers.
// Mirrors Immich/Nextcloud/photo-library ingest where thousands of tiny
// files land in rapid bursts.
func wlBurstSmallFiles(c *wlContext) WorkloadResult {
	const (
		totalFiles = 500
		workers    = 10
		objSize    = 4 * 1024
	)
	payload := randBytes(objSize)
	jobs := make(chan int, totalFiles)
	for i := 0; i < totalFiles; i++ {
		jobs <- i
	}
	close(jobs)

	var (
		ops      atomic.Int64
		errs     atomic.Int64
		latencyM sync.Mutex
		latency  []time.Duration
	)
	prefix := c.key(fmt.Sprintf("bench/burst/%d", time.Now().UnixNano()))
	overall := time.Now()
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			for i := range jobs {
				key := fmt.Sprintf("%s/w%d-%d", prefix, workerID, i)
				d, err := putObject(c.ctx, c.client, c.bucket, key, payload)
				if err != nil {
					errs.Add(1)
					continue
				}
				ops.Add(1)
				latencyM.Lock()
				latency = append(latency, d)
				latencyM.Unlock()
				c.track(key)
			}
		}(w)
	}
	wg.Wait()
	elapsed := time.Since(overall)
	p50, p95, p99, mx := percentiles(latency)
	totalOps := int(ops.Load())
	totalBytes := int64(totalOps) * int64(objSize)
	return WorkloadResult{
		Name:        "burst_small_files_500",
		Description: fmt.Sprintf("%d × 4KB PUT, %d workers", totalFiles, workers),
		Ops:         totalOps,
		Errors:      int(errs.Load()),
		Bytes:       totalBytes,
		DurationMS:  msInt(elapsed),
		P50MS:       msInt(p50),
		P95MS:       msInt(p95),
		P99MS:       msInt(p99),
		MaxMS:       msInt(mx),
		MBps:        mbps(totalBytes, elapsed),
		OpsPerSec:   float64(totalOps) / elapsed.Seconds(),
	}
}

// 12. list_100 — list at most 100 keys.
func wlList(c *wlContext) WorkloadResult {
	overall := time.Now()
	out, err := c.client.ListObjectsV2(c.ctx, &s3.ListObjectsV2Input{
		Bucket:  aws.String(c.bucket),
		MaxKeys: aws.Int32(100),
	})
	elapsed := time.Since(overall)
	if err != nil {
		return WorkloadResult{Name: "list_100", Error: err.Error(), DurationMS: msInt(elapsed)}
	}
	count := len(out.Contents)
	return WorkloadResult{
		Name:        "list_100",
		Description: "ListObjectsV2 MaxKeys=100",
		Ops:         count,
		DurationMS:  msInt(elapsed),
		Note:        fmt.Sprintf("returned=%d", count),
	}
}

// warm_head_4kb — 50× HEAD on pre-seeded 4KB objects.
func wlWarmHead(c *wlContext) WorkloadResult {
	const N = 50
	keys := seedObjects(c, "bench/head-seed", N, 4*1024)
	var times []time.Duration
	var errs int
	overall := time.Now()
	for _, k := range keys {
		d, err := headObject(c.ctx, c.client, c.bucket, k)
		if err != nil {
			errs++
			continue
		}
		times = append(times, d)
	}
	elapsed := time.Since(overall)
	p50, p95, p99, mx := percentiles(times)
	ops := len(times)
	return WorkloadResult{
		Name:        "warm_head_4kb",
		Description: fmt.Sprintf("%d× HEAD on 4KB objects", N),
		Ops:         ops,
		Errors:      errs,
		DurationMS:  msInt(elapsed),
		P50MS:       msInt(p50),
		P95MS:       msInt(p95),
		P99MS:       msInt(p99),
		MaxMS:       msInt(mx),
		OpsPerSec:   float64(ops) / elapsed.Seconds(),
	}
}

// warm_copy_4kb — 10× CopyObject (server-side copy).
func wlWarmCopy(c *wlContext) WorkloadResult {
	const N = 10
	sources := seedObjects(c, "bench/copy-src", N, 4*1024)
	var times []time.Duration
	var errs int
	overall := time.Now()
	for i, src := range sources {
		dst := c.key(fmt.Sprintf("bench/copy-dst/%d-%d", time.Now().UnixNano(), i))
		copySource := fmt.Sprintf("%s/%s", c.bucket, src)
		start := time.Now()
		_, err := c.client.CopyObject(c.ctx, &s3.CopyObjectInput{
			Bucket:     aws.String(c.bucket),
			Key:        aws.String(dst),
			CopySource: aws.String(copySource),
		})
		d := time.Since(start)
		if err != nil {
			errs++
			continue
		}
		times = append(times, d)
		c.track(dst)
	}
	elapsed := time.Since(overall)
	p50, p95, p99, mx := percentiles(times)
	ops := len(times)
	return WorkloadResult{
		Name:        "warm_copy_4kb",
		Description: fmt.Sprintf("%d× CopyObject (server-side)", N),
		Ops:         ops,
		Errors:      errs,
		DurationMS:  msInt(elapsed),
		P50MS:       msInt(p50),
		P95MS:       msInt(p95),
		P99MS:       msInt(p99),
		MaxMS:       msInt(mx),
		OpsPerSec:   float64(ops) / elapsed.Seconds(),
	}
}

// warm_delete_single — seed N then DeleteObject one at a time, measuring each.
func wlDeleteSingle(c *wlContext) WorkloadResult {
	const N = 30
	keys := seedObjects(c, "bench/del-single-seed", N, 4*1024)
	var times []time.Duration
	var errs int
	overall := time.Now()
	for _, k := range keys {
		start := time.Now()
		_, err := c.client.DeleteObject(c.ctx, &s3.DeleteObjectInput{
			Bucket: aws.String(c.bucket),
			Key:    aws.String(k),
		})
		d := time.Since(start)
		if err != nil {
			errs++
			continue
		}
		times = append(times, d)
	}
	elapsed := time.Since(overall)
	p50, p95, p99, mx := percentiles(times)
	ops := len(times)
	return WorkloadResult{
		Name:        "warm_delete_single",
		Description: fmt.Sprintf("%d× single DeleteObject", N),
		Ops:         ops,
		Errors:      errs,
		DurationMS:  msInt(elapsed),
		P50MS:       msInt(p50),
		P95MS:       msInt(p95),
		P99MS:       msInt(p99),
		MaxMS:       msInt(mx),
		OpsPerSec:   float64(ops) / elapsed.Seconds(),
	}
}

// delete_batch_100 — seed 100, single DeleteObjects call.
func wlDeleteBatch(c *wlContext) WorkloadResult {
	const N = 100
	keys := seedObjects(c, "bench/del-batch-seed", N, 4*1024)
	if len(keys) == 0 {
		return WorkloadResult{Name: "delete_batch_100", Error: "seed failed"}
	}
	objs := make([]s3types.ObjectIdentifier, len(keys))
	for i, k := range keys {
		objs[i] = s3types.ObjectIdentifier{Key: aws.String(k)}
	}
	overall := time.Now()
	out, err := c.client.DeleteObjects(c.ctx, &s3.DeleteObjectsInput{
		Bucket: aws.String(c.bucket),
		Delete: &s3types.Delete{Objects: objs, Quiet: aws.Bool(true)},
	})
	elapsed := time.Since(overall)
	if err != nil {
		return WorkloadResult{Name: "delete_batch_100", Error: err.Error(), DurationMS: msInt(elapsed)}
	}
	deleted := len(keys) - len(out.Errors)
	return WorkloadResult{
		Name:        "delete_batch_100",
		Description: "1× DeleteObjects with 100 keys",
		Ops:         deleted,
		Errors:      len(out.Errors),
		DurationMS:  msInt(elapsed),
		OpsPerSec:   float64(deleted) / elapsed.Seconds(),
	}
}

// list_prefix — seed 30 with a unique prefix, then ListObjectsV2 with that prefix.
func wlListPrefix(c *wlContext) WorkloadResult {
	const N = 30
	rawPrefix := fmt.Sprintf("bench/list-prefix-%d", time.Now().UnixNano())
	keys := seedObjects(c, rawPrefix, N, 1024)
	if len(keys) == 0 {
		return WorkloadResult{Name: "list_prefix", Error: "seed failed"}
	}
	overall := time.Now()
	out, err := c.client.ListObjectsV2(c.ctx, &s3.ListObjectsV2Input{
		Bucket: aws.String(c.bucket),
		Prefix: aws.String(c.key(rawPrefix)),
	})
	elapsed := time.Since(overall)
	if err != nil {
		return WorkloadResult{Name: "list_prefix", Error: err.Error(), DurationMS: msInt(elapsed)}
	}
	return WorkloadResult{
		Name:        "list_prefix",
		Description: fmt.Sprintf("ListObjectsV2 Prefix= (seeded %d)", N),
		Ops:         len(out.Contents),
		DurationMS:  msInt(elapsed),
		Note:        fmt.Sprintf("returned=%d", len(out.Contents)),
	}
}

// mpu_abort — measure CreateMultipartUpload + AbortMultipartUpload latency.
func wlMpuAbort(c *wlContext) WorkloadResult {
	const N = 5
	var times []time.Duration
	var errs int
	overall := time.Now()
	for i := 0; i < N; i++ {
		key := c.key(fmt.Sprintf("bench/mpu-abort/%d-%d", time.Now().UnixNano(), i))
		start := time.Now()
		cmu, err := c.client.CreateMultipartUpload(c.ctx, &s3.CreateMultipartUploadInput{
			Bucket: aws.String(c.bucket),
			Key:    aws.String(key),
		})
		if err != nil {
			errs++
			continue
		}
		_, err = c.client.AbortMultipartUpload(c.ctx, &s3.AbortMultipartUploadInput{
			Bucket:   aws.String(c.bucket),
			Key:      aws.String(key),
			UploadId: cmu.UploadId,
		})
		d := time.Since(start)
		if err != nil {
			errs++
			continue
		}
		times = append(times, d)
	}
	elapsed := time.Since(overall)
	p50, p95, p99, mx := percentiles(times)
	ops := len(times)
	return WorkloadResult{
		Name:        "mpu_abort",
		Description: fmt.Sprintf("%d× CreateMPU + AbortMPU round-trip", N),
		Ops:         ops,
		Errors:      errs,
		DurationMS:  msInt(elapsed),
		P50MS:       msInt(p50),
		P95MS:       msInt(p95),
		P99MS:       msInt(p99),
		MaxMS:       msInt(mx),
		OpsPerSec:   float64(ops) / elapsed.Seconds(),
	}
}

// integrity_robust_16mb — Vaultaire-style hardened round-trip:
// verified PUT (upload + HEAD ETag check + retry) + robust GET (HEAD + Range
// chunks + retry-on-short). Proves we can patch around broken S3-compatibles.
func wlIntegrityRobust(c *wlContext) WorkloadResult {
	const sz = 16 * 1024 * 1024
	payload := randBytes(sz)
	want := sha256hex(payload)
	key := c.key(fmt.Sprintf("bench/integrity-robust/%d", time.Now().UnixNano()))

	overall := time.Now()
	_, putRetries, err := putObjectVerified(c.ctx, c.client, c.bucket, key, payload)
	if err != nil {
		return WorkloadResult{Name: "integrity_robust_16mb", Error: "verified put: " + err.Error(), DurationMS: msInt(time.Since(overall))}
	}
	c.track(key)

	got, _, getRetries, err := getObjectRobust(c.ctx, c.client, c.bucket, key)
	elapsed := time.Since(overall)
	if err != nil {
		return WorkloadResult{Name: "integrity_robust_16mb", Error: "robust get: " + err.Error(), DurationMS: msInt(elapsed)}
	}
	if sha256hex(got) != want {
		return WorkloadResult{Name: "integrity_robust_16mb", Error: "SHA256 MISMATCH after robust get", DurationMS: msInt(elapsed)}
	}
	return WorkloadResult{
		Name:        "integrity_robust_16mb",
		Description: "16MB verified PUT + Range-GET round-trip (Vaultaire resilience demo)",
		Ops:         1,
		Bytes:       sz,
		DurationMS:  msInt(elapsed),
		MBps:        mbps(sz, elapsed),
		Note:        fmt.Sprintf("✓ verified (put_retries=%d, get_retries=%d)", putRetries, getRetries),
	}
}

// integrity_chunked_16mb — BitTorrent-style: split into 2MB chunks,
// upload each with retry, store a manifest (SHA256 of each chunk).
// On read: fetch manifest, parallel-fetch each chunk, verify against manifest,
// retry any chunk that fails verification.
//
// This is how you make corrupt/flaky backends viable: never trust a single
// large transfer, always chunk + verify. Overhead is N extra objects and
// one manifest object, but individual chunk failures are recoverable.
func wlIntegrityChunked(c *wlContext) WorkloadResult {
	const (
		totalSize = 16 * 1024 * 1024
		chunkSize = 2 * 1024 * 1024 // 2MB — small enough to avoid Quotaless's large-object issues
		numChunks = totalSize / chunkSize
	)
	payload := randBytes(totalSize)
	expectedSHA := sha256hex(payload)
	baseKey := c.key(fmt.Sprintf("bench/chunked/%d", time.Now().UnixNano()))

	// Build manifest: per-chunk SHA256s
	manifest := make([]string, numChunks)
	for i := 0; i < numChunks; i++ {
		manifest[i] = sha256hex(payload[i*chunkSize : (i+1)*chunkSize])
	}
	manifestBytes := []byte(strings.Join(manifest, "\n"))

	overall := time.Now()
	var putRetries atomic.Int32

	// Parallel upload of chunks
	sem := make(chan struct{}, 4)
	var wg sync.WaitGroup
	var uploadErr error
	var errMu sync.Mutex
	for i := 0; i < numChunks; i++ {
		wg.Add(1)
		sem <- struct{}{}
		go func(idx int) {
			defer wg.Done()
			defer func() { <-sem }()
			chunkKey := fmt.Sprintf("%s/chunk-%d", baseKey, idx)
			data := payload[idx*chunkSize : (idx+1)*chunkSize]
			// Retry up to 3 times per chunk
			for attempt := 0; attempt < 3; attempt++ {
				_, err := c.client.PutObject(c.ctx, &s3.PutObjectInput{
					Bucket:        aws.String(c.bucket),
					Key:           aws.String(chunkKey),
					Body:          bytes.NewReader(data),
					ContentLength: aws.Int64(int64(len(data))),
				})
				if err == nil {
					c.track(chunkKey)
					return
				}
				putRetries.Add(1)
				time.Sleep(time.Duration(attempt+1) * 200 * time.Millisecond)
			}
			errMu.Lock()
			if uploadErr == nil {
				uploadErr = fmt.Errorf("chunk %d failed after 3 retries", idx)
			}
			errMu.Unlock()
		}(i)
	}
	wg.Wait()
	if uploadErr != nil {
		return WorkloadResult{Name: "integrity_chunked_16mb", Error: uploadErr.Error(), DurationMS: msInt(time.Since(overall))}
	}

	// Upload manifest
	manifestKey := baseKey + "/manifest"
	_, err := c.client.PutObject(c.ctx, &s3.PutObjectInput{
		Bucket:        aws.String(c.bucket),
		Key:           aws.String(manifestKey),
		Body:          bytes.NewReader(manifestBytes),
		ContentLength: aws.Int64(int64(len(manifestBytes))),
	})
	if err != nil {
		return WorkloadResult{Name: "integrity_chunked_16mb", Error: "manifest put: " + err.Error(), DurationMS: msInt(time.Since(overall))}
	}
	c.track(manifestKey)

	// Now read back: fetch manifest, then each chunk, verify SHA256 per chunk
	mresp, err := c.client.GetObject(c.ctx, &s3.GetObjectInput{Bucket: aws.String(c.bucket), Key: aws.String(manifestKey)})
	if err != nil {
		return WorkloadResult{Name: "integrity_chunked_16mb", Error: "manifest get: " + err.Error(), DurationMS: msInt(time.Since(overall))}
	}
	gotManifest, _ := io.ReadAll(mresp.Body)
	_ = mresp.Body.Close()
	gotChunks := strings.Split(string(gotManifest), "\n")
	if len(gotChunks) != numChunks {
		return WorkloadResult{Name: "integrity_chunked_16mb", Error: fmt.Sprintf("manifest corrupt: expected %d chunks, got %d", numChunks, len(gotChunks)), DurationMS: msInt(time.Since(overall))}
	}

	assembled := make([]byte, totalSize)
	var getRetries atomic.Int32
	wg = sync.WaitGroup{}
	sem = make(chan struct{}, 4)
	var getErr error
	var getErrMu sync.Mutex
	for i := 0; i < numChunks; i++ {
		wg.Add(1)
		sem <- struct{}{}
		go func(idx int) {
			defer wg.Done()
			defer func() { <-sem }()
			chunkKey := fmt.Sprintf("%s/chunk-%d", baseKey, idx)
			for attempt := 0; attempt < 3; attempt++ {
				resp, err := c.client.GetObject(c.ctx, &s3.GetObjectInput{Bucket: aws.String(c.bucket), Key: aws.String(chunkKey)})
				if err != nil {
					getRetries.Add(1)
					continue
				}
				data, _ := io.ReadAll(resp.Body)
				_ = resp.Body.Close()
				if sha256hex(data) == gotChunks[idx] && len(data) == chunkSize {
					copy(assembled[idx*chunkSize:(idx+1)*chunkSize], data)
					return
				}
				getRetries.Add(1)
			}
			getErrMu.Lock()
			if getErr == nil {
				getErr = fmt.Errorf("chunk %d verification failed after 3 retries", idx)
			}
			getErrMu.Unlock()
		}(i)
	}
	wg.Wait()
	elapsed := time.Since(overall)
	if getErr != nil {
		return WorkloadResult{Name: "integrity_chunked_16mb", Error: getErr.Error(), DurationMS: msInt(elapsed)}
	}
	if sha256hex(assembled) != expectedSHA {
		return WorkloadResult{Name: "integrity_chunked_16mb", Error: "final SHA256 mismatch after assembly", DurationMS: msInt(elapsed)}
	}
	return WorkloadResult{
		Name:        "integrity_chunked_16mb",
		Description: "16MB via 8x2MB chunks + manifest (BitTorrent-style)",
		Ops:         numChunks + 1,
		Bytes:       totalSize,
		DurationMS:  msInt(elapsed),
		MBps:        mbps(totalSize, elapsed),
		Note:        fmt.Sprintf("✓ verified chunks (put_retries=%d, get_retries=%d)", putRetries.Load(), getRetries.Load()),
	}
}

// integrity_16mb — sha256 round-trip.
func wlIntegrity(c *wlContext) WorkloadResult {
	const sz = 16 * 1024 * 1024
	payload := randBytes(sz)
	want := sha256hex(payload)
	key := c.key(fmt.Sprintf("bench/integrity/%d", time.Now().UnixNano()))
	overall := time.Now()
	if _, err := putObject(c.ctx, c.client, c.bucket, key, payload); err != nil {
		return WorkloadResult{Name: "integrity_16mb", Error: "put: " + err.Error(), DurationMS: msInt(time.Since(overall))}
	}
	c.track(key)
	got, _, err := getObject(c.ctx, c.client, c.bucket, key)
	elapsed := time.Since(overall)
	if err != nil {
		return WorkloadResult{Name: "integrity_16mb", Error: "get: " + err.Error(), DurationMS: msInt(elapsed)}
	}
	if sha256hex(got) != want {
		return WorkloadResult{Name: "integrity_16mb", Error: "SHA256 MISMATCH", DurationMS: msInt(elapsed)}
	}
	return WorkloadResult{
		Name:        "integrity_16mb",
		Description: "16MB SHA256 round-trip",
		Ops:         1,
		Bytes:       sz,
		DurationMS:  msInt(elapsed),
		Note:        "✓ checksum match",
	}
}
