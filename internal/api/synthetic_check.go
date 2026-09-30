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

const (
	syntheticDefaultInterval = 2 * time.Minute
	syntheticMinInterval     = 30 * time.Second
	syntheticOpTimeout       = 20 * time.Second
	syntheticBodyBytes       = 4096
	syntheticDefaultBucket   = "synthetic-check"
	syntheticDefaultRegion   = "us-east-1"
)

var syntheticOps = []string{"put", "head", "get", "delete", "get_after_delete"}

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

	mu          sync.Mutex
	ran         bool
	ok          bool
	lastSuccess time.Time
	lastError   string
	latency     map[string]float64
	failures    map[string]float64
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
	return newSyntheticChecker(url, ak, sk, bucket, region, interval, logger)
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
		latency:  map[string]float64{},
		failures: map[string]float64{},
	}
}

// Start runs the cycle immediately and then every interval until ctx is
// done. Nil-safe (check off).
func (c *syntheticChecker) Start(ctx context.Context) {
	if c == nil {
		return
	}
	c.logger.Info("synthetic customer check ON", zap.String("url", c.url), zap.String("bucket", c.bucket), zap.Duration("interval", c.interval))
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

// runOnce executes one cycle and records it.
func (c *syntheticChecker) runOnce(ctx context.Context) {
	op, err := c.cycle(ctx)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ran = true
	if err != nil {
		c.ok = false
		c.lastError = op + ": " + err.Error()
		c.failures[op]++
		c.logger.Warn("synthetic customer check FAILED", zap.String("op", op), zap.Error(err))
		return
	}
	c.ok = true
	c.lastError = ""
	c.lastSuccess = c.now()
	c.logger.Debug("synthetic customer check ok", zap.Any("latency_seconds", c.latency))
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
	start := c.now()
	resp, err := c.client.Do(req)
	c.mu.Lock()
	c.latency[op] = c.now().Sub(start).Seconds()
	c.mu.Unlock()
	if err != nil {
		return nil, fmt.Errorf("%s %s: %w", method, key, err)
	}
	return resp, nil
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
	}
}

func (s *syntheticCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- s.success
	ch <- s.latency
	ch <- s.failures
	ch <- s.lastSuccess
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
	for _, op := range syntheticOps {
		if l, present := latency[op]; present {
			ch <- prometheus.MustNewConstMetric(s.latency, prometheus.GaugeValue, l, op)
		}
		ch <- prometheus.MustNewConstMetric(s.failures, prometheus.CounterValue, failures[op], op)
	}
}
