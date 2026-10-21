// cmd/tools/blob-verify — read-verifies blobs on a backend, end to end.
//
// routing_truth asks a backend whether an object EXISTS (a HEAD). On
// 2026-10-21 Lyve us-east-1 answered HEAD 200 — right size, right ETag — for
// chunk blobs whose every GET was a 500 InternalError. This tool reads the
// bytes: for every key a HEAD, then a GET of the whole body (or a small
// range) hashed as it streams and compared with the ETag when the ETag is a
// plain MD5 (a multipart ETag carries a "-N" and is only size-checked). Each
// failed attempt is recorded with its status, error code and request id —
// the material of a vendor ticket. It writes nothing on the backend.
//
// Two ways to address a backend:
//
//   - an S3 endpoint directly (`-endpoint`, `-region`, `-bucket`, the pair in
//     `-access-key-env` / `-secret-key-env`; keys from `-keys FILE` or a
//     listing of `-prefix`): the SDK with ONE attempt per call, so every
//     status the vendor answers is seen, and this tool's own retry loop
//     (`-attempts`, default 3) on top;
//   - a registered driver built from the service's env the way main.go
//     builds it (`-driver sync|permafrost|blomp|geyser|lyve|idrive|wasabi`,
//     `-tenant`; keys from `-keys FILE` as `container<TAB>artifact[<TAB>size]`,
//     or `-walk` for everything the tenant holds, or `-list container[/prefix]`):
//     the bytes through the driver's Get, sha256'd, size-checked when known.
//
// One JSON line per key on stdout; progress and a summary on stderr.
//
//	source ~/vaultaire-bench/.env.bench
//	blob-verify -endpoint https://s3.us-east-1.global.lyve.seagate.com -region us-east-1 \
//	  -bucket stored-us-east-1 -access-key-env LYVE_ACCESS_KEY -secret-key-env LYVE_SECRET_KEY \
//	  -prefix t-_global/_global/_chunks/ -c 16 > chunks.jsonl
package main

import (
	"bufio"
	"context"
	"crypto/md5" // #nosec G501 — the S3 ETag is an MD5; this compares against it
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awshttp "github.com/aws/aws-sdk-go-v2/aws/transport/http"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go"

	"github.com/FairForge/vaultaire/internal/drivers"
)

type result struct {
	Key          string   `json:"key"`
	Size         int64    `json:"size"`
	ETag         string   `json:"etag,omitempty"`
	LastModified string   `json:"last_modified,omitempty"`
	SSE          string   `json:"sse,omitempty"`
	VersionID    string   `json:"version_id,omitempty"`
	Multipart    bool     `json:"multipart"`
	Outcome      string   `json:"outcome"` // ok | md5_mismatch | short_body | get_error | head_error | missing
	HeadStatus   int      `json:"head_status,omitempty"`
	GetStatus    int      `json:"get_status,omitempty"`
	Attempts     int      `json:"attempts"`
	Transient    int      `json:"transient"` // failed attempts before the one that succeeded
	BytesRead    int64    `json:"bytes_read"`
	MD5          string   `json:"md5,omitempty"`
	Code         string   `json:"code,omitempty"`
	Error        string   `json:"error,omitempty"`
	RequestIDs   []string `json:"request_ids,omitempty"`
	Statuses     []int    `json:"statuses,omitempty"` // every attempt's HTTP status (0 = no answer)
	MS           float64  `json:"ms"`
}

func main() {
	endpoint := flag.String("endpoint", "", "S3 endpoint URL (S3 mode)")
	region := flag.String("region", "us-east-1", "SigV4 region (S3 mode)")
	bucket := flag.String("bucket", "", "bucket (S3 mode)")
	akEnv := flag.String("access-key-env", "LYVE_ACCESS_KEY", "env var holding the access key (S3 mode)")
	skEnv := flag.String("secret-key-env", "LYVE_SECRET_KEY", "env var holding the secret key (S3 mode)")
	prefix := flag.String("prefix", "", "list every key under this prefix (S3 mode; '' with -keys)")
	keysFile := flag.String("keys", "", "file of keys, one per line ('-' = stdin); S3 mode: the key; driver mode: container<TAB>artifact[<TAB>size]")
	driver := flag.String("driver", "", "driver mode: sync | permafrost | blomp | geyser | lyve | idrive | wasabi (built from the service env)")
	tenant := flag.String("tenant", "", "driver mode: the tenant in the context")
	walk := flag.Bool("walk", false, "driver mode: verify everything the tenant holds (engine.TenantWalker)")
	list := flag.String("list", "", "driver mode: verify every key the driver lists under container[/prefix]")
	mode := flag.String("mode", "full", "full (HEAD + whole GET, MD5 vs ETag) | range (HEAD + GET of the first -range-bytes) | head (HEAD only)")
	rangeBytes := flag.Int64("range-bytes", 100, "bytes of the range in -mode range")
	conc := flag.Int("c", 16, "concurrent keys")
	attempts := flag.Int("attempts", 3, "attempts per call before the key is a failure (1 s, 3 s, 9 s between them)")
	timeout := flag.Duration("timeout", 3*time.Minute, "per-call deadline")
	limit := flag.Int("limit", 0, "stop after this many keys (0 = all)")
	sample := flag.Int("sample", 0, "a random sample of this many keys from the input (0 = all; seed -seed)")
	seed := flag.Int64("seed", 1, "sample seed")
	progressEvery := flag.Int("progress", 500, "progress line every N keys on stderr")
	flag.Parse()

	ctx := context.Background()
	if *driver != "" {
		runDriverMode(ctx, driverOpts{name: *driver, tenant: *tenant, walk: *walk, list: *list, keysFile: *keysFile,
			mode: *mode, rangeBytes: *rangeBytes, conc: *conc, attempts: *attempts, timeout: *timeout, limit: *limit, sample: *sample, seed: *seed, progress: *progressEvery})
		return
	}
	if *endpoint == "" || *bucket == "" {
		fmt.Fprintln(os.Stderr, "S3 mode needs -endpoint and -bucket (or -driver for driver mode)")
		os.Exit(2)
	}
	ak, sk := os.Getenv(*akEnv), os.Getenv(*skEnv)
	if ak == "" || sk == "" {
		fmt.Fprintf(os.Stderr, "%s / %s required\n", *akEnv, *skEnv)
		os.Exit(2)
	}
	hc := drivers.TunedHTTPClient(drivers.WithHTTP1Only(), drivers.WithResponseHeaderTimeout(60*time.Second))
	cfg, err := config.LoadDefaultConfig(ctx, config.WithRegion(*region),
		config.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(ak, sk, "")),
		config.WithHTTPClient(hc), config.WithRetryMaxAttempts(1))
	if err != nil {
		fmt.Fprintln(os.Stderr, "sdk config:", err)
		os.Exit(1)
	}
	client := s3.NewFromConfig(cfg, func(o *s3.Options) {
		o.BaseEndpoint = aws.String(*endpoint)
		o.UsePathStyle = true
		o.RequestChecksumCalculation = aws.RequestChecksumCalculationWhenRequired
		o.ResponseChecksumValidation = aws.ResponseChecksumValidationWhenRequired
	})

	var keys []string
	switch {
	case *keysFile != "":
		lines, err := readLines(*keysFile)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		for _, l := range lines {
			if k := strings.TrimSpace(strings.SplitN(l, "\t", 2)[0]); k != "" {
				keys = append(keys, k)
			}
		}
	default:
		t0 := time.Now()
		p := s3.NewListObjectsV2Paginator(client, &s3.ListObjectsV2Input{Bucket: aws.String(*bucket), Prefix: aws.String(*prefix)})
		for p.HasMorePages() {
			lctx, cancel := context.WithTimeout(ctx, *timeout)
			page, err := p.NextPage(lctx)
			cancel()
			if err != nil {
				fmt.Fprintln(os.Stderr, "list:", err)
				os.Exit(1)
			}
			for _, o := range page.Contents {
				keys = append(keys, aws.ToString(o.Key))
			}
		}
		fmt.Fprintf(os.Stderr, "listed %d keys under %s/%s in %s\n", len(keys), *bucket, *prefix, time.Since(t0).Round(time.Millisecond))
	}
	keys = pick(keys, *sample, *seed, *limit)
	fmt.Fprintf(os.Stderr, "verifying %d keys on %s/%s, mode %s, %d at a time, %d attempts\n", len(keys), *bucket, *bucket, *mode, *conc, *attempts)

	s := &s3Verifier{client: client, bucket: *bucket, mode: *mode, rangeBytes: *rangeBytes, attempts: *attempts, timeout: *timeout}
	runAll(ctx, keys, *conc, *progressEvery, func(ctx context.Context, k string) result { return s.verify(ctx, k) })
}

// pick applies -sample and -limit.
func pick(keys []string, sample int, seed int64, limit int) []string {
	if sample > 0 && sample < len(keys) {
		r := rand.New(rand.NewSource(seed)) // #nosec G404 — a sample, not a secret
		r.Shuffle(len(keys), func(i, j int) { keys[i], keys[j] = keys[j], keys[i] })
		keys = keys[:sample]
		sort.Strings(keys)
	}
	if limit > 0 && limit < len(keys) {
		keys = keys[:limit]
	}
	return keys
}

func readLines(path string) ([]string, error) {
	var r io.Reader = os.Stdin
	if path != "-" {
		f, err := os.Open(path) // #nosec G304 — the operator's own file
		if err != nil {
			return nil, err
		}
		defer func() { _ = f.Close() }()
		r = f
	}
	var out []string
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		if l := sc.Text(); strings.TrimSpace(l) != "" {
			out = append(out, l)
		}
	}
	return out, sc.Err()
}

// runAll runs verify over keys with conc workers, one JSON line per result on
// stdout, a summary on stderr.
func runAll(ctx context.Context, keys []string, conc, progress int, verify func(context.Context, string) result) {
	var mu sync.Mutex
	enc := json.NewEncoder(os.Stdout)
	counts := map[string]int{}
	codes := map[string]int{}
	var done, transient atomic.Int64
	var bytesRead atomic.Int64
	start := time.Now()
	in := make(chan string)
	var wg sync.WaitGroup
	for i := 0; i < conc; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for k := range in {
				r := verify(ctx, k)
				bytesRead.Add(r.BytesRead)
				transient.Add(int64(r.Transient))
				mu.Lock()
				_ = enc.Encode(r)
				counts[r.Outcome]++
				if r.Code != "" {
					codes[fmt.Sprintf("%s %d %s", r.Outcome, r.GetStatus, r.Code)]++
				}
				mu.Unlock()
				if n := done.Add(1); progress > 0 && n%int64(progress) == 0 {
					el := time.Since(start)
					fmt.Fprintf(os.Stderr, "  %d/%d keys, %.1f MB/s, %s elapsed\n", n, len(keys), float64(bytesRead.Load())/el.Seconds()/1e6, el.Round(time.Second))
				}
			}
		}()
	}
	for _, k := range keys {
		in <- k
	}
	close(in)
	wg.Wait()
	el := time.Since(start)
	fmt.Fprintf(os.Stderr, "\n== %d keys in %s, %.2f GB read (%.1f MB/s), %d transient failures that then succeeded\n",
		len(keys), el.Round(time.Second), float64(bytesRead.Load())/1e9, float64(bytesRead.Load())/el.Seconds()/1e6, transient.Load())
	for _, o := range sortedKeys(counts) {
		fmt.Fprintf(os.Stderr, "  %-14s %d\n", o, counts[o])
	}
	if len(codes) > 0 {
		fmt.Fprintln(os.Stderr, "  by error:")
		for _, c := range sortedKeys(codes) {
			fmt.Fprintf(os.Stderr, "    %-40s %d\n", c, codes[c])
		}
	}
}

func sortedKeys(m map[string]int) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

type s3Verifier struct {
	client     *s3.Client
	bucket     string
	mode       string
	rangeBytes int64
	attempts   int
	timeout    time.Duration
}

// classify pulls the status, the request id and the error code out of an
// SDK error.
func classify(err error) (status int, reqID, code string) {
	var re *awshttp.ResponseError
	if errors.As(err, &re) {
		status, reqID = re.HTTPStatusCode(), re.ServiceRequestID()
	}
	var ae smithy.APIError
	if errors.As(err, &ae) {
		code = ae.ErrorCode()
	}
	if code == "" && err != nil {
		code = "transport"
		if errors.Is(err, context.DeadlineExceeded) {
			code = "timeout"
		}
	}
	return status, reqID, code
}

func backoff(i int) time.Duration { return time.Duration(1<<(2*uint(i))) * time.Second } // 1 s, 4 s, 16 s

func (s *s3Verifier) verify(ctx context.Context, key string) result {
	r := result{Key: key}
	start := time.Now()
	defer func() { r.MS = float64(time.Since(start)) / float64(time.Millisecond) }()

	// HEAD: the vendor's view of the object.
	var head *s3.HeadObjectOutput
	for i := 0; i < s.attempts; i++ {
		r.Attempts++
		hctx, cancel := context.WithTimeout(ctx, s.timeout)
		out, err := s.client.HeadObject(hctx, &s3.HeadObjectInput{Bucket: aws.String(s.bucket), Key: aws.String(key)})
		cancel()
		if err == nil {
			head = out
			r.HeadStatus = http.StatusOK
			break
		}
		st, rid, code := classify(err)
		r.Statuses = append(r.Statuses, st)
		if rid != "" {
			r.RequestIDs = append(r.RequestIDs, rid)
		}
		if st == http.StatusNotFound {
			r.Outcome, r.HeadStatus, r.Code = "missing", st, code
			return r
		}
		r.HeadStatus, r.Code, r.Error = st, code, err.Error()
		if i+1 < s.attempts {
			time.Sleep(backoff(i))
		}
	}
	if head == nil {
		r.Outcome = "head_error"
		return r
	}
	r.Transient = r.Attempts - 1
	r.Size = aws.ToInt64(head.ContentLength)
	r.ETag = strings.Trim(aws.ToString(head.ETag), `"`)
	r.Multipart = strings.Contains(r.ETag, "-")
	if head.LastModified != nil {
		r.LastModified = head.LastModified.UTC().Format(time.RFC3339)
	}
	r.SSE = string(head.ServerSideEncryption)
	r.VersionID = aws.ToString(head.VersionId)
	if s.mode == "head" {
		r.Outcome = "ok"
		return r
	}

	// GET: the bytes.
	in := &s3.GetObjectInput{Bucket: aws.String(s.bucket), Key: aws.String(key)}
	want := r.Size
	if s.mode == "range" {
		n := s.rangeBytes
		if r.Size < n {
			n = r.Size
		}
		if n > 0 {
			in.Range = aws.String(fmt.Sprintf("bytes=0-%d", n-1))
		}
		want = n
	}
	getAttempts := 0
	for i := 0; i < s.attempts; i++ {
		getAttempts++
		r.Attempts++
		gctx, cancel := context.WithTimeout(ctx, s.timeout)
		out, err := s.client.GetObject(gctx, in)
		if err == nil {
			h := md5.New() // #nosec G401 — comparing with the S3 ETag
			n, cerr := io.Copy(h, out.Body)
			_ = out.Body.Close()
			cancel()
			r.BytesRead += n
			if cerr != nil {
				r.Statuses = append(r.Statuses, http.StatusOK)
				r.Code, r.Error = "body", cerr.Error()
				if i+1 < s.attempts {
					time.Sleep(backoff(i))
				}
				continue
			}
			r.GetStatus = http.StatusOK
			r.Transient += getAttempts - 1
			if n != want {
				r.Outcome = "short_body"
				r.Error = fmt.Sprintf("read %d of %d bytes", n, want)
				return r
			}
			if s.mode == "full" {
				r.MD5 = hex.EncodeToString(h.Sum(nil))
				if !r.Multipart && r.MD5 != r.ETag {
					r.Outcome = "md5_mismatch"
					return r
				}
			}
			r.Outcome = "ok"
			r.Code, r.Error = "", ""
			return r
		}
		cancel()
		st, rid, code := classify(err)
		r.Statuses = append(r.Statuses, st)
		if rid != "" {
			r.RequestIDs = append(r.RequestIDs, rid)
		}
		r.GetStatus, r.Code, r.Error = st, code, err.Error()
		if st == http.StatusNotFound {
			r.Outcome = "missing"
			return r
		}
		if i+1 < s.attempts {
			time.Sleep(backoff(i))
		}
	}
	r.Outcome = "get_error"
	return r
}
