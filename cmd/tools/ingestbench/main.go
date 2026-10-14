// ingestbench — concurrent small-object ingest against one S3 endpoint with a
// customer key: N workers PUT fixed-size objects for a duration (or a byte
// budget) and the tool reports throughput and the per-PUT latency tail.
//
// It exists because the 2026-10-12 bench found the concurrent-ingest tail
// (103 MB/s, p99 2.4 s through the origin vs 748 MB/s, p99 420 ms on raw
// Lyve) is Vaultaire's own, and bench-compare's workload cannot be pointed at
// a second tenant or a local instance. Two ingestbench processes with two
// keys = the multi-tenant contention test; one against a local build with
// pprof = the profile.
//
//	AWS_ACCESS_KEY_ID=… AWS_SECRET_ACCESS_KEY=… ingestbench \
//	  -endpoint https://s3.stored.ge -bucket ingest-x -workers 32 -size-mb 4 -duration 20s -out ingest.jsonl
//
// Credentials: VAULTAIRE_BENCH_ACCESS_KEY/SECRET_KEY, else AWS_ACCESS_KEY_ID/
// AWS_SECRET_ACCESS_KEY. The bucket is created if absent and the keys written
// are deleted at the end unless -keep. Exit 0 even with failed PUTs (they are
// counted and reported); exit 1 on a setup error.
package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"math/rand"
	"net"
	"net/http"
	"os"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
)

type result struct {
	Label      string         `json:"label"`
	Endpoint   string         `json:"endpoint"`
	Bucket     string         `json:"bucket"`
	Workers    int            `json:"workers"`
	SizeBytes  int64          `json:"size_bytes"`
	Ops        int64          `json:"ops"`
	Errors     int64          `json:"errors"`
	ErrorCodes map[string]int `json:"error_codes,omitempty"`
	Seconds    float64        `json:"seconds"`
	MBps       float64        `json:"mb_per_s"`
	OpsPerS    float64        `json:"ops_per_s"`
	P50ms      float64        `json:"p50_ms"`
	P90ms      float64        `json:"p90_ms"`
	P99ms      float64        `json:"p99_ms"`
	MaxMs      float64        `json:"max_ms"`
	StartedAt  time.Time      `json:"started_at"`
}

func main() {
	endpoint := flag.String("endpoint", "", "S3 endpoint, e.g. https://s3.stored.ge")
	bucket := flag.String("bucket", "", "bucket (created if absent)")
	label := flag.String("label", "", "label in the output (default: bucket)")
	workers := flag.Int("workers", 32, "concurrent PUT workers")
	sizeMB := flag.Float64("size-mb", 4, "object size in MiB")
	duration := flag.Duration("duration", 20*time.Second, "run length; a PUT in flight at the deadline is finished")
	maxMB := flag.Float64("max-mb", 1024, "stop after this many MiB (0 = only the duration)")
	prefix := flag.String("prefix", "", "key prefix (default: ingest-<unix>/)")
	outPath := flag.String("out", "", "append one JSON line per run to this file")
	keep := flag.Bool("keep", false, "leave the objects in place")
	overwrite := flag.Int("overwrite", 0, "N > 0: each worker cycles N keys, overwriting them (the overwrite path: displaced bytes released in the commit, WP-R10-8)")
	h2 := flag.Bool("h2", false, "allow HTTP/2")
	insecure := flag.Bool("insecure", false, "skip TLS verification")
	flag.Parse()
	if *endpoint == "" || *bucket == "" {
		fmt.Fprintln(os.Stderr, "-endpoint and -bucket are required")
		os.Exit(1)
	}
	if *label == "" {
		*label = *bucket
	}
	ak, sk := os.Getenv("VAULTAIRE_BENCH_ACCESS_KEY"), os.Getenv("VAULTAIRE_BENCH_SECRET_KEY")
	if ak == "" {
		ak, sk = os.Getenv("AWS_ACCESS_KEY_ID"), os.Getenv("AWS_SECRET_ACCESS_KEY")
	}
	if ak == "" {
		fmt.Fprintln(os.Stderr, "no credentials (VAULTAIRE_BENCH_* or AWS_*)")
		os.Exit(1)
	}
	tr := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: 15 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		MaxIdleConns:          *workers * 2,
		MaxIdleConnsPerHost:   *workers * 2,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   15 * time.Second,
		ResponseHeaderTimeout: 10 * time.Minute,
		ForceAttemptHTTP2:     *h2,
		TLSClientConfig:       &tls.Config{InsecureSkipVerify: *insecure}, //nolint:gosec // operator flag
	}
	if !*h2 {
		tr.TLSNextProto = map[string]func(string, *tls.Conn) http.RoundTripper{}
	}
	client := s3.New(s3.Options{
		Region:           "us-east-1",
		BaseEndpoint:     aws.String(*endpoint),
		UsePathStyle:     true,
		Credentials:      credentials.NewStaticCredentialsProvider(ak, sk, ""),
		HTTPClient:       &http.Client{Transport: tr, Timeout: 30 * time.Minute},
		RetryMaxAttempts: 1,
	})
	ctx := context.Background()
	if _, err := client.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: bucket}); err != nil {
		if _, err := client.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: bucket}); err != nil {
			fmt.Fprintln(os.Stderr, "create bucket:", err)
			os.Exit(1)
		}
	}
	if *prefix == "" {
		*prefix = fmt.Sprintf("ingest-%d/", time.Now().Unix())
	}
	size := int64(*sizeMB * 1024 * 1024)
	budget := int64(*maxMB * 1024 * 1024)

	var (
		ops, errs, sent int64
		mu              sync.Mutex
		lat             []time.Duration
		codes           = map[string]int{}
		firstErr        = map[string]string{}
		keys            []string
	)
	deadline := time.Now().Add(*duration)
	started := time.Now()
	var wg sync.WaitGroup
	for w := 0; w < *workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			body := make([]byte, size)
			rnd := rand.New(rand.NewSource(int64(w) + started.UnixNano())) //nolint:gosec // bench data
			rnd.Read(body)
			for i := 0; ; i++ {
				if time.Now().After(deadline) {
					return
				}
				if budget > 0 && atomic.AddInt64(&sent, size) > budget {
					return
				}
				n := i
				if *overwrite > 0 {
					n = i % *overwrite
				}
				key := fmt.Sprintf("%sw%02d-%05d.bin", *prefix, w, n)
				t0 := time.Now()
				_, err := client.PutObject(ctx, &s3.PutObjectInput{Bucket: bucket, Key: &key, Body: bytes.NewReader(body), ContentLength: aws.Int64(size)})
				d := time.Since(t0)
				mu.Lock()
				if err != nil {
					errs++
					c := errCode(err)
					codes[c]++
					if _, seen := firstErr[c]; !seen {
						firstErr[c] = err.Error()
					}
				} else {
					ops++
					lat = append(lat, d)
					keys = append(keys, key)
				}
				mu.Unlock()
			}
		}(w)
	}
	wg.Wait()
	elapsed := time.Since(started)
	sort.Slice(lat, func(i, j int) bool { return lat[i] < lat[j] })
	pct := func(p float64) float64 {
		if len(lat) == 0 {
			return 0
		}
		i := int(float64(len(lat)-1) * p)
		return float64(lat[i].Microseconds()) / 1000
	}
	res := result{
		Label: *label, Endpoint: *endpoint, Bucket: *bucket, Workers: *workers, SizeBytes: size,
		Ops: ops, Errors: errs, Seconds: elapsed.Seconds(), StartedAt: started.UTC(),
		MBps:    float64(ops*size) / 1e6 / elapsed.Seconds(),
		OpsPerS: float64(ops) / elapsed.Seconds(),
		P50ms:   pct(0.50), P90ms: pct(0.90), P99ms: pct(0.99), MaxMs: pct(1),
	}
	if errs > 0 {
		res.ErrorCodes = codes
		for c, m := range firstErr {
			if len(m) > 300 {
				m = m[:300]
			}
			fmt.Fprintf(os.Stderr, "first %s error: %s\n", c, m)
		}
	}
	fmt.Printf("%s: %d workers × %.1f MiB: %d ok, %d failed in %.1fs = %.1f MB/s %.1f ops/s; p50 %.0f ms p90 %.0f p99 %.0f max %.0f %v\n",
		res.Label, res.Workers, *sizeMB, ops, errs, res.Seconds, res.MBps, res.OpsPerS, res.P50ms, res.P90ms, res.P99ms, res.MaxMs, codes)
	if *outPath != "" {
		f, err := os.OpenFile(*outPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err == nil {
			b, _ := json.Marshal(res)
			_, _ = f.Write(append(b, '\n'))
			_ = f.Close()
		}
	}
	if *keep {
		return
	}
	for len(keys) > 0 {
		n := len(keys)
		if n > 1000 {
			n = 1000
		}
		objs := make([]types.ObjectIdentifier, 0, n)
		for _, k := range keys[:n] {
			k := k
			objs = append(objs, types.ObjectIdentifier{Key: &k})
		}
		if _, err := client.DeleteObjects(ctx, &s3.DeleteObjectsInput{Bucket: bucket, Delete: &types.Delete{Objects: objs, Quiet: aws.Bool(true)}}); err != nil {
			fmt.Fprintln(os.Stderr, "cleanup:", err)
			return
		}
		keys = keys[n:]
	}
}

func errCode(err error) string {
	var ae smithy.APIError
	if errors.As(err, &ae) {
		return ae.ErrorCode()
	}
	return "transport"
}
