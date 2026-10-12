// tierbench drives ONE bucket on ONE S3 endpoint through the workloads a
// customer of a given tier actually runs, and prints what each took.
//
// bench-compare measures providers against each other with a bucket of its
// own; tierbench measures a bucket whose tier_preference / visibility the
// operator has already set (standard, archive, sync, public-read …), so the
// numbers are the tier's, through whichever endpoint is given (the origin
// s3.stored.ge, the Cloudflare-proxied stored.ge, or a loopback port).
//
// Every GET is sha256-verified against the bytes that were PUT; a mismatch is
// printed as !!! MISMATCH and the exit status is 2. Nothing is left behind
// unless -keep is given.
//
//	tierbench -endpoint https://s3.stored.ge -bucket b-std -label std-origin \
//	  -small 30 -single 8,64,256 -mpu 256,1024 -range 20 \
//	  [-cdn https://stored.ge/cdn/<slug>/<bucket>] [-restore] [-max-single-mb 100]
//
// Credentials: VAULTAIRE_BENCH_ACCESS_KEY / VAULTAIRE_BENCH_SECRET_KEY (else
// AWS_ACCESS_KEY_ID / AWS_SECRET_ACCESS_KEY). Output: one human line per
// workload on stdout and one JSON object per workload appended to -out.
package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

type result struct {
	Label    string  `json:"label"`
	Endpoint string  `json:"endpoint"`
	Bucket   string  `json:"bucket"`
	Workload string  `json:"workload"`
	Bytes    int64   `json:"bytes,omitempty"`
	N        int     `json:"n,omitempty"`
	Seconds  float64 `json:"seconds"`
	MBps     float64 `json:"mbps,omitempty"`
	TTFBms   float64 `json:"ttfb_ms,omitempty"`
	P50ms    float64 `json:"p50_ms,omitempty"`
	P95ms    float64 `json:"p95_ms,omitempty"`
	Maxms    float64 `json:"max_ms,omitempty"`
	Verified bool    `json:"verified,omitempty"`
	Note     string  `json:"note,omitempty"`
	Error    string  `json:"error,omitempty"`
	At       string  `json:"at"`
}

var (
	out      *os.File
	label    string
	endpoint string
	bucket   string
	mismatch bool
)

func emit(r result) {
	r.Label, r.Endpoint, r.Bucket, r.At = label, endpoint, bucket, time.Now().UTC().Format(time.RFC3339)
	line := fmt.Sprintf("%-14s %-28s", label, r.Workload)
	switch {
	case r.Error != "":
		line += " ERROR " + r.Error
	case r.P50ms > 0:
		line += fmt.Sprintf(" n=%d p50 %.0f ms  p95 %.0f ms  max %.0f ms", r.N, r.P50ms, r.P95ms, r.Maxms)
	default:
		line += fmt.Sprintf(" %.2f s", r.Seconds)
		if r.MBps > 0 {
			line += fmt.Sprintf("  %.1f MB/s", r.MBps)
		}
		if r.TTFBms > 0 {
			line += fmt.Sprintf("  ttfb %.0f ms", r.TTFBms)
		}
	}
	if r.Verified {
		line += "  ok"
	}
	if r.Note != "" {
		line += "  [" + r.Note + "]"
	}
	fmt.Println(line)
	if out != nil {
		b, _ := json.Marshal(r)
		_, _ = out.Write(append(b, '\n'))
	}
}

// seeded returns size deterministic pseudo-random bytes and their sha256.
func seeded(seed uint64, size int64) ([]byte, string) {
	buf := make([]byte, size)
	r := rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15))
	for i := 0; i+8 <= len(buf); i += 8 {
		v := r.Uint64()
		buf[i] = byte(v)
		buf[i+1] = byte(v >> 8)
		buf[i+2] = byte(v >> 16)
		buf[i+3] = byte(v >> 24)
		buf[i+4] = byte(v >> 32)
		buf[i+5] = byte(v >> 40)
		buf[i+6] = byte(v >> 48)
		buf[i+7] = byte(v >> 56)
	}
	sum := sha256.Sum256(buf)
	return buf, hex.EncodeToString(sum[:])
}

func pct(d []time.Duration, p float64) float64 {
	if len(d) == 0 {
		return 0
	}
	s := append([]time.Duration(nil), d...)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	i := int(float64(len(s)-1) * p)
	return float64(s[i]) / 1e6
}

func maxd(d []time.Duration) float64 {
	var m time.Duration
	for _, x := range d {
		if x > m {
			m = x
		}
	}
	return float64(m) / 1e6
}

type ttfbReader struct {
	io.ReadCloser
	start time.Time
	first time.Duration
	once  sync.Once
}

func (t *ttfbReader) Read(p []byte) (int, error) {
	n, err := t.ReadCloser.Read(p)
	if n > 0 {
		t.once.Do(func() { t.first = time.Since(t.start) })
	}
	return n, err
}

func mbps(b int64, d time.Duration) float64 {
	if d <= 0 {
		return 0
	}
	return float64(b) / 1e6 / d.Seconds()
}

func main() {
	flag.StringVar(&endpoint, "endpoint", "https://s3.stored.ge", "S3 endpoint")
	flag.StringVar(&bucket, "bucket", "", "bucket (must exist)")
	flag.StringVar(&label, "label", "", "label for the output lines")
	small := flag.Int("small", 30, "4 KiB objects: sequential PUT / GET / HEAD / DELETE, each timed")
	single := flag.String("single", "8,64,256", "single PutObject sizes in MiB (> 64 MiB = the chunked path when chunking is on)")
	mpu := flag.String("mpu", "256,1024", "multipart upload sizes in MiB")
	partMB := flag.Int("part-mb", 16, "multipart part size in MiB")
	conc := flag.Int("conc", 8, "concurrent parts per multipart upload")
	ranges := flag.Int("range", 20, "random 1 MiB range reads of the largest object")
	maxSingle := flag.Int("max-single-mb", 0, "skip single PUTs above this many MiB (Cloudflare Free refuses bodies > 100 MB); 0 = no limit")
	cdn := flag.String("cdn", "", "public CDN base URL of this bucket (https://stored.ge/cdn/<slug>/<bucket>); GETs each object twice unauthenticated")
	restore := flag.Bool("restore", false, "archive bucket: HEAD x-amz-restore + RestoreObject on the first object")
	keep := flag.Bool("keep", false, "leave the objects in the bucket")
	outPath := flag.String("out", "", "append one JSON line per workload here")
	h2 := flag.Bool("h2", false, "allow HTTP/2 to the endpoint (default HTTP/1.1, what aws-cli and rclone use)")
	insecure := flag.Bool("insecure", false, "skip TLS verification")
	flag.Parse()
	if bucket == "" {
		fmt.Fprintln(os.Stderr, "-bucket is required")
		os.Exit(1)
	}
	if label == "" {
		label = bucket
	}
	if *outPath != "" {
		f, err := os.OpenFile(*outPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		out = f
		defer f.Close()
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
		MaxIdleConns:          64,
		MaxIdleConnsPerHost:   64,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   15 * time.Second,
		ResponseHeaderTimeout: 10 * time.Minute,
		ForceAttemptHTTP2:     *h2,
		TLSClientConfig:       &tls.Config{InsecureSkipVerify: *insecure}, //nolint:gosec // operator flag
	}
	if !*h2 {
		tr.TLSNextProto = map[string]func(string, *tls.Conn) http.RoundTripper{}
	}
	httpc := &http.Client{Transport: tr, Timeout: 30 * time.Minute}
	client := s3.New(s3.Options{
		Region:           "us-east-1",
		BaseEndpoint:     aws.String(endpoint),
		UsePathStyle:     true,
		Credentials:      credentials.NewStaticCredentialsProvider(ak, sk, ""),
		HTTPClient:       httpc,
		RetryMaxAttempts: 1,
	})
	ctx := context.Background()
	run := fmt.Sprintf("tb-%d", time.Now().Unix())
	var cleanup []string
	defer func() {
		if *keep {
			return
		}
		for _, k := range cleanup {
			_, _ = client.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: &bucket, Key: &k})
		}
	}()
	plain := &http.Client{Transport: tr, Timeout: 30 * time.Minute}

	// --- small objects ---
	if *small > 0 {
		body, _ := seeded(1, 4096)
		var put, get, head, del []time.Duration
		keys := make([]string, 0, *small)
		for i := 0; i < *small; i++ {
			k := fmt.Sprintf("%s/small/%03d", run, i)
			t0 := time.Now()
			_, err := client.PutObject(ctx, &s3.PutObjectInput{Bucket: &bucket, Key: &k, Body: bytes.NewReader(body), ContentLength: aws.Int64(4096)})
			if err != nil {
				emit(result{Workload: "small_put_4k", Error: err.Error()})
				break
			}
			put = append(put, time.Since(t0))
			keys = append(keys, k)
		}
		if len(put) > 0 {
			emit(result{Workload: "small_put_4k", N: len(put), P50ms: pct(put, 0.5), P95ms: pct(put, 0.95), Maxms: maxd(put)})
		}
		okAll := true
		for _, k := range keys {
			t0 := time.Now()
			o, err := client.GetObject(ctx, &s3.GetObjectInput{Bucket: &bucket, Key: &k})
			if err != nil {
				emit(result{Workload: "small_get_4k", Error: err.Error()})
				okAll = false
				break
			}
			b, _ := io.ReadAll(o.Body)
			o.Body.Close()
			get = append(get, time.Since(t0))
			if !bytes.Equal(b, body) {
				okAll = false
				mismatch = true
				fmt.Println("!!! MISMATCH small", k)
			}
		}
		if len(get) > 0 {
			emit(result{Workload: "small_get_4k", N: len(get), P50ms: pct(get, 0.5), P95ms: pct(get, 0.95), Maxms: maxd(get), Verified: okAll})
		}
		for _, k := range keys {
			t0 := time.Now()
			if _, err := client.HeadObject(ctx, &s3.HeadObjectInput{Bucket: &bucket, Key: &k}); err != nil {
				emit(result{Workload: "small_head_4k", Error: err.Error()})
				break
			}
			head = append(head, time.Since(t0))
		}
		if len(head) > 0 {
			emit(result{Workload: "small_head_4k", N: len(head), P50ms: pct(head, 0.5), P95ms: pct(head, 0.95), Maxms: maxd(head)})
		}
		t0 := time.Now()
		lo, err := client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{Bucket: &bucket, Prefix: aws.String(run + "/small/")})
		if err != nil {
			emit(result{Workload: "list_prefix", Error: err.Error()})
		} else {
			emit(result{Workload: "list_prefix", N: len(lo.Contents), Seconds: time.Since(t0).Seconds(), Note: fmt.Sprintf("%d keys listed", len(lo.Contents))})
		}
		if *cdn != "" && len(keys) > 0 {
			cdnGet(plain, *cdn, keys[0], body, "cdn_small_4k")
		}
		for _, k := range keys {
			t0 := time.Now()
			if _, err := client.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: &bucket, Key: &k}); err != nil {
				emit(result{Workload: "small_delete", Error: err.Error()})
				break
			}
			del = append(del, time.Since(t0))
		}
		if len(del) > 0 {
			emit(result{Workload: "small_delete", N: len(del), P50ms: pct(del, 0.5), P95ms: pct(del, 0.95), Maxms: maxd(del)})
		}
	}

	var largest string
	var largestSize int64

	// --- single PUT ---
	for _, s := range splitInts(*single) {
		if *maxSingle > 0 && s > *maxSingle {
			emit(result{Workload: fmt.Sprintf("single_put_%dmb", s), Note: "skipped: above -max-single-mb"})
			continue
		}
		size := int64(s) << 20
		body, sum := seeded(uint64(s), size)
		k := fmt.Sprintf("%s/single-%dmb.bin", run, s)
		t0 := time.Now()
		_, err := client.PutObject(ctx, &s3.PutObjectInput{Bucket: &bucket, Key: &k, Body: bytes.NewReader(body), ContentLength: aws.Int64(size)})
		d := time.Since(t0)
		if err != nil {
			emit(result{Workload: fmt.Sprintf("single_put_%dmb", s), Bytes: size, Seconds: d.Seconds(), Error: trim(err)})
			continue
		}
		cleanup = append(cleanup, k)
		emit(result{Workload: fmt.Sprintf("single_put_%dmb", s), Bytes: size, Seconds: d.Seconds(), MBps: mbps(size, d)})
		getVerify(ctx, client, k, size, sum, fmt.Sprintf("get_single_%dmb", s))
		if *cdn != "" {
			cdnGet(plain, *cdn, k, body, fmt.Sprintf("cdn_get_%dmb", s))
		}
		if size > largestSize {
			largest, largestSize = k, size
		}
		if *restore && largest == k {
			restoreProbe(ctx, client, k)
		}
	}

	// --- multipart ---
	for _, s := range splitInts(*mpu) {
		size := int64(s) << 20
		part := int64(*partMB) << 20
		k := fmt.Sprintf("%s/mpu-%dmb.bin", run, s)
		h := sha256.New()
		nparts := int((size + part - 1) / part)
		t0 := time.Now()
		cr, err := client.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{Bucket: &bucket, Key: &k})
		if err != nil {
			emit(result{Workload: fmt.Sprintf("mpu_put_%dmb", s), Error: trim(err)})
			continue
		}
		type done struct {
			n    int
			etag string
			err  error
		}
		sem := make(chan struct{}, *conc)
		res := make([]done, nparts)
		var wg sync.WaitGroup
		// hash in order first (cheap), then upload concurrently
		bodies := make([][]byte, nparts)
		for i := 0; i < nparts; i++ {
			sz := part
			if int64(i+1)*part > size {
				sz = size - int64(i)*part
			}
			bodies[i], _ = seeded(uint64(s)*1000+uint64(i), sz)
			h.Write(bodies[i])
		}
		for i := 0; i < nparts; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				sem <- struct{}{}
				defer func() { <-sem }()
				up, err := client.UploadPart(ctx, &s3.UploadPartInput{Bucket: &bucket, Key: &k, UploadId: cr.UploadId, PartNumber: aws.Int32(int32(i + 1)), Body: bytes.NewReader(bodies[i]), ContentLength: aws.Int64(int64(len(bodies[i])))})
				if err != nil {
					res[i] = done{n: i + 1, err: err}
					return
				}
				res[i] = done{n: i + 1, etag: aws.ToString(up.ETag)}
			}(i)
		}
		wg.Wait()
		var parts []types.CompletedPart
		var perr error
		for _, d := range res {
			if d.err != nil {
				perr = d.err
				break
			}
			parts = append(parts, types.CompletedPart{PartNumber: aws.Int32(int32(d.n)), ETag: aws.String(d.etag)})
		}
		if perr != nil {
			_, _ = client.AbortMultipartUpload(ctx, &s3.AbortMultipartUploadInput{Bucket: &bucket, Key: &k, UploadId: cr.UploadId})
			emit(result{Workload: fmt.Sprintf("mpu_put_%dmb", s), Bytes: size, Seconds: time.Since(t0).Seconds(), Error: trim(perr)})
			continue
		}
		tc := time.Now()
		_, err = client.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{Bucket: &bucket, Key: &k, UploadId: cr.UploadId, MultipartUpload: &types.CompletedMultipartUpload{Parts: parts}})
		d := time.Since(t0)
		if err != nil {
			emit(result{Workload: fmt.Sprintf("mpu_put_%dmb", s), Bytes: size, Seconds: d.Seconds(), Error: "complete: " + trim(err)})
			continue
		}
		cleanup = append(cleanup, k)
		emit(result{Workload: fmt.Sprintf("mpu_put_%dmb", s), Bytes: size, Seconds: d.Seconds(), MBps: mbps(size, d), Note: fmt.Sprintf("%d parts x %d MiB, %d wide, complete %.1f s", nparts, *partMB, *conc, time.Since(tc).Seconds())})
		bodies = nil
		getVerify(ctx, client, k, size, hex.EncodeToString(h.Sum(nil)), fmt.Sprintf("get_mpu_%dmb", s))
		if *cdn != "" && s <= 256 {
			cdnGetStream(plain, *cdn, k, hex.EncodeToString(h.Sum(nil)), size, fmt.Sprintf("cdn_get_mpu_%dmb", s))
		}
		if size > largestSize {
			largest, largestSize = k, size
		}
	}

	// --- range reads ---
	if *ranges > 0 && largest != "" {
		var ds []time.Duration
		r := rand.New(rand.NewPCG(7, 7))
		for i := 0; i < *ranges; i++ {
			off := r.Int64N(largestSize - (1 << 20))
			rng := fmt.Sprintf("bytes=%d-%d", off, off+(1<<20)-1)
			t0 := time.Now()
			o, err := client.GetObject(ctx, &s3.GetObjectInput{Bucket: &bucket, Key: &largest, Range: &rng})
			if err != nil {
				emit(result{Workload: "range_get_1mb", Error: trim(err)})
				break
			}
			n, _ := io.Copy(io.Discard, o.Body)
			o.Body.Close()
			if n != 1<<20 {
				fmt.Printf("!!! range returned %d bytes\n", n)
				mismatch = true
			}
			ds = append(ds, time.Since(t0))
		}
		if len(ds) > 0 {
			emit(result{Workload: "range_get_1mb", N: len(ds), P50ms: pct(ds, 0.5), P95ms: pct(ds, 0.95), Maxms: maxd(ds), Note: "of " + largest[strings.LastIndex(largest, "/")+1:]})
		}
	}
	if mismatch {
		os.Exit(2)
	}
}

func getVerify(ctx context.Context, client *s3.Client, k string, size int64, want, name string) {
	t0 := time.Now()
	o, err := client.GetObject(ctx, &s3.GetObjectInput{Bucket: &bucket, Key: &k})
	if err != nil {
		emit(result{Workload: name, Bytes: size, Seconds: time.Since(t0).Seconds(), Error: trim(err)})
		return
	}
	tr := &ttfbReader{ReadCloser: o.Body, start: t0}
	h := sha256.New()
	n, err := io.Copy(h, tr)
	o.Body.Close()
	d := time.Since(t0)
	if err != nil {
		emit(result{Workload: name, Bytes: n, Seconds: d.Seconds(), Error: trim(err)})
		return
	}
	got := hex.EncodeToString(h.Sum(nil))
	ok := got == want && n == size
	if !ok {
		mismatch = true
		fmt.Printf("!!! MISMATCH %s: %d bytes sha %s want %s\n", k, n, got[:12], want[:12])
	}
	emit(result{Workload: name, Bytes: n, Seconds: d.Seconds(), MBps: mbps(n, d), TTFBms: float64(tr.first) / 1e6, Verified: ok})
}

func cdnGet(plain *http.Client, base, k string, want []byte, name string) {
	sum := sha256.Sum256(want)
	cdnGetStream(plain, base, k, hex.EncodeToString(sum[:]), int64(len(want)), name)
}

func cdnGetStream(plain *http.Client, base, k, want string, size int64, name string) {
	url := strings.TrimRight(base, "/") + "/" + k
	for pass := 1; pass <= 2; pass++ {
		t0 := time.Now()
		resp, err := plain.Get(url)
		if err != nil {
			emit(result{Workload: name, Error: err.Error()})
			return
		}
		tr := &ttfbReader{ReadCloser: resp.Body, start: t0}
		h := sha256.New()
		n, _ := io.Copy(h, tr)
		resp.Body.Close()
		d := time.Since(t0)
		got := hex.EncodeToString(h.Sum(nil))
		ok := resp.StatusCode == 200 && got == want && n == size
		if !ok {
			mismatch = true
		}
		note := fmt.Sprintf("pass %d status %d cf-cache-status=%s", pass, resp.StatusCode, resp.Header.Get("cf-cache-status"))
		if sf := resp.Header.Get("x-vaultaire-served-from"); sf != "" {
			note += " served-from=" + sf
		}
		emit(result{Workload: name, Bytes: n, Seconds: d.Seconds(), MBps: mbps(n, d), TTFBms: float64(tr.first) / 1e6, Verified: ok, Note: note})
	}
}

func restoreProbe(ctx context.Context, client *s3.Client, k string) {
	ho, err := client.HeadObject(ctx, &s3.HeadObjectInput{Bucket: &bucket, Key: &k})
	if err != nil {
		emit(result{Workload: "archive_head", Error: trim(err)})
		return
	}
	emit(result{Workload: "archive_head", Note: fmt.Sprintf("class=%s restore=%q", ho.StorageClass, aws.ToString(ho.Restore))})
	t0 := time.Now()
	_, err = client.RestoreObject(ctx, &s3.RestoreObjectInput{Bucket: &bucket, Key: &k, RestoreRequest: &types.RestoreRequest{Days: aws.Int32(1)}})
	if err != nil {
		emit(result{Workload: "archive_restore", Seconds: time.Since(t0).Seconds(), Note: "answer: " + trim(err)})
		return
	}
	emit(result{Workload: "archive_restore", Seconds: time.Since(t0).Seconds(), Note: "accepted"})
}

func splitInts(s string) []int {
	var out []int
	for _, p := range strings.Split(s, ",") {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		n, err := strconv.Atoi(p)
		if err == nil && n > 0 {
			out = append(out, n)
		}
	}
	return out
}

func trim(err error) string {
	s := err.Error()
	if i := strings.Index(s, ", RequestID"); i > 0 {
		s = s[:i]
	}
	if len(s) > 220 {
		s = s[:220]
	}
	return s
}
