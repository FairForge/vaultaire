package api

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	"github.com/prometheus/client_golang/prometheus"
	"go.uber.org/zap"
)

// Synthetic customer check (Review R13-13 — checklist item 4).
//
// The backend probes tell us whether iDrive/Geyser/Lyve answer a signed
// HeadBucket; nothing exercised the path a customer actually takes. The
// letshow demo's PUT failed every minute for 26 days (28 Aug → 22 Sep 2026)
// and no rule fired. This is that path: every interval a SigV4-signed
// PUT → HEAD → GET (bytes compared) → DELETE → GET (404) of a fresh key,
// through SYNTHETIC_CHECK_URL — the PUBLIC address (HAProxy, or Cloudflare
// when the URL is the public hostname), so TLS, the proxy chain, auth,
// quota, the head cache and the primary backend are all on the line.
//
// In-process rather than a cron on the box: it ships with the deploy, its
// series is scraped where every other rule lives, and the box has no other
// scheduler. It needs its own tenant (a dedicated key pair) and bucket —
// the bucket is never created here; a missing one is a loud error.
//
// Series (vaultaire_synthetic_check_*): success (1/0 for the last cycle),
// latency_seconds{op}, failures_total{op}, last_success_timestamp_seconds.
// Rules: deploy/monitoring/vaultaire-synthetic.yml.
//
// The edge leg (WP-LOG-2, D-LOG-6): the small cycle's 4 KiB is under
// EDGE_MIN_BYTES and never takes the edge path, so every
// SYNTHETIC_CHECK_EDGE_EVERY (≥ 5m; unset or `off` = no leg) — after a small
// cycle — the leg PUTs a random body of SYNTHETIC_CHECK_EDGE_BYTES (default
// 4 MiB, 1–64 MiB), GETs it whole comparing its sha256 as it streams (never
// a second copy in memory) and DELETEs it, 60 s an op. Its failure never
// changes `success`: it has its own edge_enabled, edge_success and
// edge_last_success_timestamp_seconds, and ops edge_put / edge_get /
// edge_delete in the latency and failure series. Whether the Worker took
// the leg is the admit counter's (DataPlaneEdgePathUnused), never a
// response header (the edge answers the origin's headers).

const (
	syntheticDefaultInterval = 2 * time.Minute
	syntheticMinInterval     = 30 * time.Second
	syntheticOpTimeout       = 20 * time.Second
	syntheticBodyBytes       = 4096
	syntheticDefaultBucket   = "synthetic-check"
	syntheticDefaultRegion   = "us-east-1"

	syntheticEdgeMinEvery     = 5 * time.Minute
	syntheticEdgeDefaultBytes = 4 << 20
	syntheticEdgeMinBytes     = 1 << 20
	syntheticEdgeMaxBytes     = 64 << 20
	syntheticEdgeOpTimeout    = 60 * time.Second
)

var syntheticOps = []string{"put", "head", "get", "delete", "get_after_delete"}

// syntheticEdgeOps are the edge leg's (WP-LOG-2), in the same series.
var syntheticEdgeOps = []string{"edge_put", "edge_get", "edge_delete"}

// syntheticChecker drives the cycle and holds the last outcome for /metrics.
type syntheticChecker struct {
	url       string
	bucket    string
	region    string
	interval  time.Duration
	creds     aws.Credentials
	signer    *v4.Signer
	client    *http.Client
	logger    *zap.Logger
	now       func() time.Time
	randomKey func() string

	// The edge leg (WP-LOG-2): edgeEvery 0 = no leg.
	edgeEvery  time.Duration
	edgeBytes  int64
	edgeClient *http.Client

	mu          sync.Mutex
	ran         bool
	ok          bool
	lastSuccess time.Time
	lastError   string
	latency     map[string]float64
	failures    map[string]float64

	edgeLast        time.Time // when the last leg started (zero = never)
	edgeRan         bool
	edgeOK          bool
	edgeLastSuccess time.Time
}

// newSyntheticCheckerFromEnv returns nil (check off) when SYNTHETIC_CHECK_URL
// is unset. A URL with no key pair is logged and treated as off.
func newSyntheticCheckerFromEnv(getenv func(string) string, logger *zap.Logger) *syntheticChecker {
	if logger == nil {
		logger = zap.NewNop()
	}
	url := strings.TrimRight(strings.TrimSpace(getenv("SYNTHETIC_CHECK_URL")), "/")
	if url == "" {
		return nil
	}
	ak, sk := getenv("SYNTHETIC_CHECK_ACCESS_KEY"), getenv("SYNTHETIC_CHECK_SECRET_KEY")
	if ak == "" || sk == "" {
		logger.Error("SYNTHETIC_CHECK_URL is set but SYNTHETIC_CHECK_ACCESS_KEY / SYNTHETIC_CHECK_SECRET_KEY are not — synthetic check OFF")
		return nil
	}
	bucket := getenv("SYNTHETIC_CHECK_BUCKET")
	if bucket == "" {
		bucket = syntheticDefaultBucket
	}
	region := getenv("SYNTHETIC_CHECK_REGION")
	if region == "" {
		region = syntheticDefaultRegion
	}
	interval := syntheticDefaultInterval
	if v := getenv("SYNTHETIC_CHECK_INTERVAL"); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d >= syntheticMinInterval {
			interval = d
		} else {
			logger.Warn("invalid SYNTHETIC_CHECK_INTERVAL (need a duration >= 30s), keeping default", zap.String("value", v))
		}
	}
	c := newSyntheticChecker(url, ak, sk, bucket, region, interval, logger)
	if v := strings.TrimSpace(getenv("SYNTHETIC_CHECK_EDGE_EVERY")); v != "" && v != "off" {
		if d, err := time.ParseDuration(v); err == nil && d >= syntheticEdgeMinEvery {
			c.edgeEvery = d
		} else {
			logger.Warn("invalid SYNTHETIC_CHECK_EDGE_EVERY (need a duration >= 5m, or off), keeping the default: no edge leg", zap.String("value", v))
		}
	}
	if v := strings.TrimSpace(getenv("SYNTHETIC_CHECK_EDGE_BYTES")); v != "" {
		if n, err := parseByteSize(v); err == nil && n >= syntheticEdgeMinBytes && n <= syntheticEdgeMaxBytes {
			c.edgeBytes = n
		} else {
			logger.Warn("invalid SYNTHETIC_CHECK_EDGE_BYTES (need 1MiB-64MiB), keeping the default", zap.String("value", v))
		}
	}
	return c
}

func newSyntheticChecker(url, accessKey, secretKey, bucket, region string, interval time.Duration, logger *zap.Logger) *syntheticChecker {
	if logger == nil {
		logger = zap.NewNop()
	}
	return &syntheticChecker{
		url:      strings.TrimRight(url, "/"),
		bucket:   bucket,
		region:   region,
		interval: interval,
		creds:    aws.Credentials{AccessKeyID: accessKey, SecretAccessKey: secretKey},
		signer:   v4.NewSigner(),
		client:   &http.Client{Timeout: syntheticOpTimeout},
		logger:   logger,
		now:      time.Now,
		randomKey: func() string {
			b := make([]byte, 6)
			_, _ = rand.Read(b)
			return hex.EncodeToString(b)
		},
		latency:    map[string]float64{},
		failures:   map[string]float64{},
		edgeBytes:  syntheticEdgeDefaultBytes,
		edgeClient: &http.Client{Timeout: syntheticEdgeOpTimeout},
	}
}

// Start runs the cycle immediately and then every interval until ctx is
// done. Nil-safe (check off).
func (c *syntheticChecker) Start(ctx context.Context) {
	if c == nil {
		return
	}
	c.logger.Info("synthetic customer check ON", zap.String("url", c.url), zap.String("bucket", c.bucket), zap.Duration("interval", c.interval),
		zap.Duration("edge_every", c.edgeEvery), zap.Int64("edge_bytes", c.edgeBytes))
	go func() {
		c.runOnce(ctx)
		ticker := time.NewTicker(c.interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				c.runOnce(ctx)
			}
		}
	}()
}

// runOnce executes one cycle and records it, then the edge leg when due.
func (c *syntheticChecker) runOnce(ctx context.Context) {
	op, err := c.cycle(ctx)
	c.mu.Lock()
	c.ran = true
	if err != nil {
		c.ok = false
		c.lastError = op + ": " + err.Error()
		c.failures[op]++
		c.logger.Warn("synthetic customer check FAILED", zap.String("op", op), zap.Error(err))
	} else {
		c.ok = true
		c.lastError = ""
		c.lastSuccess = c.now()
		c.logger.Debug("synthetic customer check ok", zap.Any("latency_seconds", c.latency))
	}
	due := c.edgeEvery > 0 && (c.edgeLast.IsZero() || c.now().Sub(c.edgeLast) >= c.edgeEvery)
	if due {
		c.edgeLast = c.now()
	}
	c.mu.Unlock()
	if due {
		c.runEdgeLeg(ctx)
	}
}

// runEdgeLeg runs the edge leg once and records it on its own series.
func (c *syntheticChecker) runEdgeLeg(ctx context.Context) {
	op, err := c.edgeLeg(ctx)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.edgeRan = true
	if err != nil {
		c.edgeOK = false
		c.failures[op]++
		c.logger.Warn("synthetic customer check: edge leg FAILED", zap.String("op", op), zap.Error(err))
		return
	}
	c.edgeOK = true
	c.edgeLastSuccess = c.now()
}

// edgeLeg is PUT → GET (sha256 as it streams) → DELETE of a body large
// enough for the edge path. The key is deleted whatever the GET found.
func (c *syntheticChecker) edgeLeg(ctx context.Context) (string, error) {
	key := fmt.Sprintf("synthetic/edge-%s-%s", c.now().UTC().Format("20060102T150405Z"), c.randomKey())
	body := make([]byte, c.edgeBytes)
	if _, err := rand.Read(body); err != nil {
		return "edge_put", fmt.Errorf("random body: %w", err)
	}
	want := sha256.Sum256(body)

	pctx, cancel := context.WithTimeout(ctx, syntheticEdgeOpTimeout)
	// The GET compares by hash: the body is not used after the PUT.
	resp, err := c.sendWith(pctx, c.edgeClient, "edge_put", http.MethodPut, key, body)
	if err != nil {
		cancel()
		return "edge_put", err
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	_ = resp.Body.Close()
	cancel()
	if resp.StatusCode != http.StatusOK {
		return "edge_put", fmt.Errorf("status %d", resp.StatusCode)
	}

	getOp, getErr := c.edgeGet(ctx, key, want)

	dctx, dcancel := context.WithTimeout(ctx, syntheticEdgeOpTimeout)
	defer dcancel()
	dresp, err := c.sendWith(dctx, c.edgeClient, "edge_delete", http.MethodDelete, key, nil)
	if getErr != nil {
		if err == nil {
			_ = dresp.Body.Close()
		}
		return getOp, getErr
	}
	if err != nil {
		return "edge_delete", err
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(dresp.Body, 64<<10))
	_ = dresp.Body.Close()
	if dresp.StatusCode != http.StatusNoContent && dresp.StatusCode != http.StatusOK {
		return "edge_delete", fmt.Errorf("status %d", dresp.StatusCode)
	}
	return "", nil
}

// edgeGet reads key whole, hashing it as it streams.
func (c *syntheticChecker) edgeGet(ctx context.Context, key string, want [sha256.Size]byte) (string, error) {
	gctx, cancel := context.WithTimeout(ctx, syntheticEdgeOpTimeout)
	defer cancel()
	resp, err := c.sendWith(gctx, c.edgeClient, "edge_get", http.MethodGet, key, nil)
	if err != nil {
		return "edge_get", err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "edge_get", fmt.Errorf("status %d", resp.StatusCode)
	}
	h := sha256.New()
	n, err := io.Copy(h, io.LimitReader(resp.Body, c.edgeBytes+1))
	if err != nil {
		return "edge_get", fmt.Errorf("read body: %w", err)
	}
	if n != c.edgeBytes || !bytes.Equal(h.Sum(nil), want[:]) {
		return "edge_get", fmt.Errorf("body mismatch: %d bytes back, %d sent, sha256 %s", n, c.edgeBytes, hex.EncodeToString(h.Sum(nil))[:16])
	}
	return "", nil
}

// cycle is PUT → HEAD → GET → DELETE → GET(404). It returns the op that
// failed and why; latencies of the ops that ran are recorded either way.
func (c *syntheticChecker) cycle(ctx context.Context) (string, error) {
	key := fmt.Sprintf("synthetic/%s-%s", c.now().UTC().Format("20060102T150405Z"), c.randomKey())
	body := make([]byte, syntheticBodyBytes)
	if _, err := rand.Read(body); err != nil {
		return "put", fmt.Errorf("random body: %w", err)
	}

	status, _, err := c.do(ctx, "put", http.MethodPut, key, body)
	if err != nil {
		return "put", err
	}
	if status != http.StatusOK {
		return "put", fmt.Errorf("status %d (does bucket %q exist for the synthetic tenant?)", status, c.bucket)
	}

	status, hdr, err := c.doHead(ctx, key)
	if err != nil {
		return "head", err
	}
	if status != http.StatusOK {
		return "head", fmt.Errorf("status %d", status)
	}
	if cl := hdr.Get("Content-Length"); cl != strconv.Itoa(len(body)) {
		return "head", fmt.Errorf("Content-Length %q, want %d", cl, len(body))
	}

	status, got, err := c.do(ctx, "get", http.MethodGet, key, nil)
	if err != nil {
		return "get", err
	}
	if status != http.StatusOK {
		return "get", fmt.Errorf("status %d", status)
	}
	if !bytes.Equal(got, body) {
		return "get", fmt.Errorf("body mismatch: %d bytes back, %d sent", len(got), len(body))
	}

	status, _, err = c.do(ctx, "delete", http.MethodDelete, key, nil)
	if err != nil {
		return "delete", err
	}
	if status != http.StatusNoContent && status != http.StatusOK {
		return "delete", fmt.Errorf("status %d", status)
	}

	status, _, err = c.do(ctx, "get_after_delete", http.MethodGet, key, nil)
	if err != nil {
		return "get_after_delete", err
	}
	if status != http.StatusNotFound {
		return "get_after_delete", fmt.Errorf("status %d, want 404 after delete", status)
	}
	return "", nil
}

// do signs and sends one request, returning status, body (GETs) and
// recording the op's latency.
func (c *syntheticChecker) do(ctx context.Context, op, method, key string, body []byte) (int, []byte, error) {
	// The per-op timeout must outlive the body read: cancelling it inside
	// send() made the read fail with "context canceled" under load.
	octx, cancel := context.WithTimeout(ctx, syntheticOpTimeout)
	defer cancel()
	resp, err := c.send(octx, op, method, key, body)
	if err != nil {
		return 0, nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	var got []byte
	if method == http.MethodGet {
		got, err = io.ReadAll(io.LimitReader(resp.Body, syntheticBodyBytes*4))
		if err != nil {
			return resp.StatusCode, nil, fmt.Errorf("read body: %w", err)
		}
	} else {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	}
	return resp.StatusCode, got, nil
}

func (c *syntheticChecker) doHead(ctx context.Context, key string) (int, http.Header, error) {
	octx, cancel := context.WithTimeout(ctx, syntheticOpTimeout)
	defer cancel()
	resp, err := c.send(octx, "head", http.MethodHead, key, nil)
	if err != nil {
		return 0, nil, err
	}
	_ = resp.Body.Close()
	return resp.StatusCode, resp.Header, nil
}

// send signs and sends one request on ctx (the caller owns the deadline).
func (c *syntheticChecker) send(ctx context.Context, op, method, key string, body []byte) (*http.Response, error) {
	return c.sendWith(ctx, c.client, op, method, key, body)
}

// sendWith is send on client (the edge leg's has the longer timeout).
func (c *syntheticChecker) sendWith(ctx context.Context, client *http.Client, op, method, key string, body []byte) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.url+"/"+c.bucket+"/"+key, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.ContentLength = int64(len(body))
	sum := sha256.Sum256(body)
	payloadHash := hex.EncodeToString(sum[:])
	req.Header.Set("x-amz-content-sha256", payloadHash)
	req.Header.Set("User-Agent", "vaultaire-synthetic-check/1.0")
	if err := c.signer.SignHTTP(ctx, c.creds, req, payloadHash, "s3", c.region, c.now().UTC()); err != nil {
		return nil, fmt.Errorf("sign: %w", err)
	}
	start := time.Now()
	resp, err := client.Do(req)
	c.mu.Lock()
	c.latency[op] = time.Since(start).Seconds()
	c.mu.Unlock()
	if err != nil {
		return nil, fmt.Errorf("%s %s: %w", method, key, err)
	}
	return resp, nil
}

// edgeSnapshot is the edge leg's state for the collector.
func (c *syntheticChecker) edgeSnapshot() (enabled, ran, ok bool, lastSuccess time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.edgeEvery > 0, c.edgeRan, c.edgeOK, c.edgeLastSuccess
}

// snapshot copies the state for the collector.
func (c *syntheticChecker) snapshot() (ran, ok bool, lastSuccess time.Time, latency, failures map[string]float64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	latency = make(map[string]float64, len(c.latency))
	for k, v := range c.latency {
		latency[k] = v
	}
	failures = make(map[string]float64, len(c.failures))
	for k, v := range c.failures {
		failures[k] = v
	}
	return c.ran, c.ok, c.lastSuccess, latency, failures
}

type syntheticCollector struct {
	c           *syntheticChecker
	success     *prometheus.Desc
	latency     *prometheus.Desc
	failures    *prometheus.Desc
	lastSuccess *prometheus.Desc

	edgeEnabled     *prometheus.Desc
	edgeSuccess     *prometheus.Desc
	edgeLastSuccess *prometheus.Desc
}

func newSyntheticCollector(c *syntheticChecker) *syntheticCollector {
	return &syntheticCollector{
		c: c,
		success: prometheus.NewDesc("vaultaire_synthetic_check_success",
			"1 if the last synthetic PUT/HEAD/GET/DELETE cycle through the public S3 endpoint succeeded, else 0.", nil, nil),
		latency: prometheus.NewDesc("vaultaire_synthetic_check_latency_seconds",
			"Latency of the last synthetic request, by op.", []string{"op"}, nil),
		failures: prometheus.NewDesc("vaultaire_synthetic_check_failures_total",
			"Synthetic cycles that failed, by the op that failed.", []string{"op"}, nil),
		lastSuccess: prometheus.NewDesc("vaultaire_synthetic_check_last_success_timestamp_seconds",
			"Unix time of the last successful synthetic cycle (0 = never).", nil, nil),
		edgeEnabled: prometheus.NewDesc("vaultaire_synthetic_check_edge_enabled",
			"1 while the synthetic check's edge leg is configured (SYNTHETIC_CHECK_EDGE_EVERY), else 0.", nil, nil),
		edgeSuccess: prometheus.NewDesc("vaultaire_synthetic_check_edge_success",
			"1 if the last edge leg (a body large enough for the edge path: PUT, GET with its sha256 compared, DELETE) succeeded, else 0. No series before the first leg.", nil, nil),
		edgeLastSuccess: prometheus.NewDesc("vaultaire_synthetic_check_edge_last_success_timestamp_seconds",
			"Unix time of the last successful edge leg (0 = never).", nil, nil),
	}
}

func (s *syntheticCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- s.success
	ch <- s.latency
	ch <- s.failures
	ch <- s.lastSuccess
	ch <- s.edgeEnabled
	ch <- s.edgeSuccess
	ch <- s.edgeLastSuccess
}

func (s *syntheticCollector) Collect(ch chan<- prometheus.Metric) {
	ran, ok, last, latency, failures := s.c.snapshot()
	if ran {
		v := 0.0
		if ok {
			v = 1
		}
		ch <- prometheus.MustNewConstMetric(s.success, prometheus.GaugeValue, v)
	}
	ts := 0.0
	if !last.IsZero() {
		ts = float64(last.Unix())
	}
	ch <- prometheus.MustNewConstMetric(s.lastSuccess, prometheus.GaugeValue, ts)
	for _, op := range append(append([]string{}, syntheticOps...), syntheticEdgeOps...) {
		if l, present := latency[op]; present {
			ch <- prometheus.MustNewConstMetric(s.latency, prometheus.GaugeValue, l, op)
		}
		ch <- prometheus.MustNewConstMetric(s.failures, prometheus.CounterValue, failures[op], op)
	}
	enabled, edgeRan, edgeOK, edgeLast := s.c.edgeSnapshot()
	ch <- prometheus.MustNewConstMetric(s.edgeEnabled, prometheus.GaugeValue, b2f(enabled))
	if edgeRan {
		ch <- prometheus.MustNewConstMetric(s.edgeSuccess, prometheus.GaugeValue, b2f(edgeOK))
	}
	ets := 0.0
	if !edgeLast.IsZero() {
		ets = float64(edgeLast.Unix())
	}
	ch <- prometheus.MustNewConstMetric(s.edgeLastSuccess, prometheus.GaugeValue, ets)
}

func b2f(b bool) float64 {
	if b {
		return 1
	}
	return 0
}
