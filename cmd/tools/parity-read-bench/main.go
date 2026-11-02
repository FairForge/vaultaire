// parity-read-bench times the ways a Vault parity rebuild can read its m
// shards from a Lyve leg (WP-VAULT-3 PR 0): it writes m random objects of
// -shard-bytes under t-_bench-parity/<run>/p<j> on the Lyve bucket, reads
// them back in each shape below, prints MB/s per case and deletes them. An
// operator probe: never linked into the product.
//
//	(a) lockstep    the production driver; m whole GETs opened one after
//	                another, then 1 MiB from shard 0, 1, 2, 3 per stripe —
//	                exactly what parityRebuilder.decodeNext did before WP-VAULT-3
//	(b) lockstep-h1 the same over a bare SDK client on
//	                TunedHTTPClient(WithHTTP1Only()) — no op-split transport,
//	                no driver code in the body path (H3)
//	(c) concurrent  the production driver; m whole GETs opened at once, each
//	                read to EOF by its own goroutine (the wire's ceiling)
//	(d) readahead   m whole GETs opened at once, each read ahead by its own
//	                goroutine into a ring of 4 stripes, consumed by stripe
//	                (D-V3-2, a leg without ranges)
//	(e) ranged      parfetch over GetRangeInfo: parts of -part-bytes per
//	                shard, -parts / m in flight per shard, consumed by stripe
//	                (D-V3-1)
//
// Credentials: the env variables named by -access-key-env / -secret-key-env
// (default LYVE_ACCESS_KEY / LYVE_SECRET_KEY). Never printed.
//
//	parity-read-bench -runs 3
//	parity-read-bench -dry-run
package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"go.uber.org/zap"

	"github.com/FairForge/vaultaire/internal/common"
	"github.com/FairForge/vaultaire/internal/drivers"
	"github.com/FairForge/vaultaire/internal/parfetch"
)

const benchTenant = "_bench-parity"

type opts struct {
	runs       int
	m          int
	shardBytes int64
	stripe     int64
	partBytes  int64
	parts      int
	region     string
	cases      []string
	akEnv      string
	skEnv      string
	dryRun     bool
	fresh      bool
	coldConns  bool
}

func main() {
	var o opts
	var cases string
	flag.IntVar(&o.runs, "runs", 3, "runs per case")
	flag.IntVar(&o.m, "m", 4, "parity shards")
	flag.Int64Var(&o.shardBytes, "shard-bytes", 25<<20, "bytes per shard (25 MiB = a 100 MiB object's shard)")
	flag.Int64Var(&o.stripe, "stripe", 1<<20, "bytes per shard per stripe")
	flag.Int64Var(&o.partBytes, "part-bytes", 16<<20, "ranged case: bytes per range (LARGE_GET_PART_BYTES)")
	flag.IntVar(&o.parts, "parts", 8, "ranged case: ranges in flight over all shards (LARGE_GET_PARALLEL_PARTS)")
	flag.StringVar(&o.region, "region", "us-east-1", "Lyve region (bucket stored-<region>)")
	flag.StringVar(&cases, "cases", "lockstep,lockstep-h1,concurrent,readahead,ranged", "cases to run")
	flag.StringVar(&o.akEnv, "access-key-env", "LYVE_ACCESS_KEY", "env variable holding the access key")
	flag.StringVar(&o.skEnv, "secret-key-env", "LYVE_SECRET_KEY", "env variable holding the secret key")
	flag.BoolVar(&o.fresh, "fresh", false, "write a new object set before every case of every run (cold reads)")
	flag.BoolVar(&o.coldConns, "cold-conns", false, "a new driver (a new connection pool) for every case of every run")
	flag.BoolVar(&o.dryRun, "dry-run", false, "print the plan and exit (no network)")
	flag.Parse()
	o.cases = strings.Split(cases, ",")
	if err := o.validate(); err != nil {
		fmt.Fprintln(os.Stderr, "parity-read-bench:", err)
		os.Exit(2)
	}
	if o.dryRun {
		fmt.Print(o.plan())
		return
	}
	if err := run(o); err != nil {
		fmt.Fprintln(os.Stderr, "parity-read-bench:", err)
		os.Exit(1)
	}
}

var knownCases = map[string]bool{"lockstep": true, "lockstep-h1": true, "concurrent": true, "readahead": true, "ranged": true}

func (o opts) validate() error {
	if o.runs < 1 || o.m < 1 || o.stripe < 1 || o.shardBytes < o.stripe || o.shardBytes%o.stripe != 0 {
		return errors.New("need runs ≥ 1, m ≥ 1, and shard-bytes a positive multiple of stripe")
	}
	if o.partBytes < o.stripe || o.partBytes%o.stripe != 0 || o.parts < 1 {
		return errors.New("part-bytes must be a positive multiple of stripe, parts ≥ 1")
	}
	for _, c := range o.cases {
		if !knownCases[c] {
			return fmt.Errorf("unknown case %q", c)
		}
	}
	return nil
}

// perShard is the ranged case's ranges in flight per shard (D-V3-1).
func (o opts) perShard() int { return max(o.parts/o.m, 1) }

func (o opts) plan() string {
	var b strings.Builder
	fmt.Fprintf(&b, "bucket stored-%s, prefix t-%s/<run>/p0..p%d\n", o.region, benchTenant, o.m-1)
	fmt.Fprintf(&b, "%d shards × %d bytes = %d bytes of object (%d stripes of %d)\n",
		o.m, o.shardBytes, int64(o.m)*o.shardBytes, o.shardBytes/o.stripe, o.stripe)
	fmt.Fprintf(&b, "ranged: %d-byte ranges, %d in flight per shard, %d ranges per shard\n",
		o.partBytes, o.perShard(), (o.shardBytes+o.partBytes-1)/o.partBytes)
	fmt.Fprintf(&b, "cases %s × %d runs", strings.Join(o.cases, ","), o.runs)
	if o.fresh {
		b.WriteString(", a fresh object set per case")
	}
	if o.coldConns {
		b.WriteString(", a new connection pool per case")
	}
	b.WriteString("\n")
	return b.String()
}

type result struct {
	open, first, total time.Duration
	bytes              int64
}

func (r result) mbps() float64 { return float64(r.bytes) / r.total.Seconds() / 1e6 }

// shardReader yields one shard stripe by stripe.
type shardReader interface {
	next() ([]byte, error)
	close()
}

func run(o opts) error {
	ak, sk := os.Getenv(o.akEnv), os.Getenv(o.skEnv)
	if ak == "" || sk == "" {
		return fmt.Errorf("%s / %s are not set", o.akEnv, o.skEnv)
	}
	logger := zap.NewNop()
	drv, err := drivers.NewLyveDriver(ak, sk, benchTenant, o.region, logger)
	if err != nil {
		return fmt.Errorf("lyve driver: %w", err)
	}
	raw, err := rawClient(ak, sk, o.region)
	if err != nil {
		return err
	}
	ctx := common.WithTenantID(context.Background(), benchTenant)
	runID := fmt.Sprintf("%d", time.Now().UTC().Unix())
	names := make([]string, o.m)
	for j := range names {
		names[j] = fmt.Sprintf("p%d", j)
	}
	// Every container written is deleted at the end, whatever happened.
	var written []string
	defer func() {
		for _, c := range written {
			for _, n := range names {
				if err := drv.Delete(context.WithoutCancel(ctx), c, n); err != nil {
					fmt.Fprintf(os.Stderr, "delete %s/%s: %v\n", c, n, err)
				}
			}
			fmt.Fprintf(os.Stderr, "deleted t-%s/%s/p0..p%d\n", benchTenant, c, o.m-1)
		}
	}()
	writeSet := func(container string) error {
		written = append(written, container)
		for _, n := range names {
			body := make([]byte, o.shardBytes)
			if _, err := rand.Read(body); err != nil {
				return err
			}
			if err := drv.Put(ctx, container, n, bytes.NewReader(body)); err != nil {
				return fmt.Errorf("put %s/%s: %w", container, n, err)
			}
		}
		return nil
	}
	fmt.Fprint(os.Stderr, o.plan())
	container := runID
	if !o.fresh {
		if err := writeSet(container); err != nil {
			return err
		}
	}
	bucket := "stored-" + o.region

	results := map[string][]result{}
	for r := 0; r < o.runs; r++ {
		for ci, c := range o.cases {
			if o.fresh {
				// A new set per case and run: every case reads objects
				// nothing has read yet (the first read of a Lyve object is
				// the slow one — PR 0's finding).
				container = fmt.Sprintf("%s-r%d-c%d", runID, r, ci)
				if err := writeSet(container); err != nil {
					return err
				}
			}
			if o.coldConns {
				// A new transport: every connection of this case is dialled,
				// TLS-shaken and slow-started now, as a rare parity read's are.
				if drv, err = drivers.NewLyveDriver(ak, sk, benchTenant, o.region, logger); err != nil {
					return fmt.Errorf("lyve driver: %w", err)
				}
				if raw, err = rawClient(ak, sk, o.region); err != nil {
					return err
				}
			}
			key := func(n string) string { return fmt.Sprintf("t-%s/%s/%s", benchTenant, container, n) }
			var res result
			var err error
			switch c {
			case "lockstep":
				res, err = timeLockstep(o, func(n string) (io.ReadCloser, error) { return drv.Get(ctx, container, n) }, names, false)
			case "lockstep-h1":
				res, err = timeLockstep(o, func(n string) (io.ReadCloser, error) {
					out, gerr := raw.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(bucket), Key: aws.String(key(n))})
					if gerr != nil {
						return nil, gerr
					}
					return out.Body, nil
				}, names, false)
			case "readahead":
				res, err = timeLockstep(o, func(n string) (io.ReadCloser, error) { return drv.Get(ctx, container, n) }, names, true)
			case "concurrent":
				res, err = timeConcurrent(o, func(n string) (io.ReadCloser, error) { return drv.Get(ctx, container, n) }, names)
			case "ranged":
				res, err = timeRanged(ctx, o, drv, container, names)
			}
			if err != nil {
				return fmt.Errorf("run %d case %s: %w", r+1, c, err)
			}
			results[c] = append(results[c], res)
			fmt.Fprintf(os.Stderr, "run %d %-12s open %6.0f ms  first stripe %6.0f ms  total %6.2f s  %7.1f MB/s\n",
				r+1, c, ms(res.open), ms(res.first), res.total.Seconds(), res.mbps())
		}
	}
	fmt.Println("| case | runs | open (ms, median) | first stripe (ms, median) | MB/s (median) | MB/s (min–max) |")
	fmt.Println("|---|---|---|---|---|---|")
	for _, c := range o.cases {
		rs := results[c]
		fmt.Printf("| %s | %d | %.0f | %.0f | %.1f | %.1f–%.1f |\n", c, len(rs),
			median(rs, func(r result) float64 { return ms(r.open) }),
			median(rs, func(r result) float64 { return ms(r.first) }),
			median(rs, func(r result) float64 { return r.mbps() }),
			minOf(rs, func(r result) float64 { return r.mbps() }), maxOf(rs, func(r result) float64 { return r.mbps() }))
	}
	return nil
}

func rawClient(ak, sk, region string) (*s3.Client, error) {
	cfg, err := config.LoadDefaultConfig(context.Background(),
		config.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(ak, sk, "")),
		config.WithRegion(region),
		config.WithHTTPClient(drivers.TunedHTTPClient(drivers.WithHTTP1Only())))
	if err != nil {
		return nil, fmt.Errorf("sdk config: %w", err)
	}
	ep := fmt.Sprintf("https://s3.%s.global.lyve.seagate.com", region)
	return s3.NewFromConfig(cfg, func(o *s3.Options) { o.BaseEndpoint = aws.String(ep); o.UsePathStyle = true }), nil
}

// timeLockstep opens the shards one after another (readahead=false, as the
// pre-WP-VAULT-3 Open did) or concurrently with a read-ahead goroutine per
// shard (readahead=true), then consumes stripe by stripe.
func timeLockstep(o opts, open func(string) (io.ReadCloser, error), names []string, readahead bool) (result, error) {
	start := time.Now()
	readers := make([]shardReader, len(names))
	if readahead {
		var wg sync.WaitGroup
		errs := make([]error, len(names))
		for j, n := range names {
			wg.Add(1)
			go func() {
				defer wg.Done()
				rc, err := open(n)
				if err != nil {
					errs[j] = err
					return
				}
				readers[j] = newAhead(rc, int(o.stripe), 4)
			}()
		}
		wg.Wait()
		if err := errors.Join(errs...); err != nil {
			closeReaders(readers)
			return result{}, err
		}
	} else {
		for j, n := range names {
			rc, err := open(n)
			if err != nil {
				closeReaders(readers)
				return result{}, err
			}
			readers[j] = &lockstep{rc: rc, buf: make([]byte, o.stripe)}
		}
	}
	return consume(o, readers, start, time.Since(start))
}

func consume(o opts, readers []shardReader, start time.Time, open time.Duration) (result, error) {
	defer closeReaders(readers)
	res := result{open: open}
	stripes := o.shardBytes / o.stripe
	for s := int64(0); s < stripes; s++ {
		for _, r := range readers {
			b, err := r.next()
			if err != nil {
				return res, fmt.Errorf("stripe %d: %w", s, err)
			}
			res.bytes += int64(len(b))
		}
		if s == 0 {
			res.first = time.Since(start)
		}
	}
	res.total = time.Since(start)
	return res, nil
}

func closeReaders(rs []shardReader) {
	for _, r := range rs {
		if r != nil {
			r.close()
		}
	}
}

type lockstep struct {
	rc  io.ReadCloser
	buf []byte
}

func (l *lockstep) next() ([]byte, error) {
	_, err := io.ReadFull(l.rc, l.buf)
	return l.buf, err
}
func (l *lockstep) close() { _ = l.rc.Close() }

// ahead reads its stream into a ring of depth stripes from a goroutine.
type ahead struct {
	rc   io.ReadCloser
	full chan []byte
	free chan []byte
	err  error
	done chan struct{}
	cur  []byte
}

func newAhead(rc io.ReadCloser, stripe, depth int) *ahead {
	a := &ahead{rc: rc, full: make(chan []byte, depth), free: make(chan []byte, depth+1), done: make(chan struct{})}
	for i := 0; i < depth+1; i++ {
		a.free <- make([]byte, stripe)
	}
	go func() {
		defer close(a.full)
		for {
			var b []byte
			select {
			case b = <-a.free:
			case <-a.done:
				return
			}
			if _, err := io.ReadFull(rc, b); err != nil {
				a.err = err
				return
			}
			a.full <- b
		}
	}()
	return a
}

func (a *ahead) next() ([]byte, error) {
	if a.cur != nil {
		a.free <- a.cur
		a.cur = nil
	}
	b, ok := <-a.full
	if !ok {
		if a.err == nil {
			return nil, io.EOF
		}
		return nil, a.err
	}
	a.cur = b
	return b, nil
}

func (a *ahead) close() {
	select {
	case <-a.done:
	default:
		close(a.done)
	}
	_ = a.rc.Close()
}

func timeConcurrent(o opts, open func(string) (io.ReadCloser, error), names []string) (result, error) {
	start := time.Now()
	var wg sync.WaitGroup
	var mu sync.Mutex
	res := result{}
	errs := make([]error, len(names))
	for j, n := range names {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rc, err := open(n)
			if err != nil {
				errs[j] = err
				return
			}
			defer func() { _ = rc.Close() }()
			mu.Lock()
			res.open = max(res.open, time.Since(start))
			mu.Unlock()
			buf := make([]byte, o.stripe)
			var got int64
			for {
				k, err := io.ReadFull(rc, buf)
				got += int64(k)
				if got == o.stripe {
					mu.Lock()
					if res.first == 0 || time.Since(start) > res.first {
						res.first = max(res.first, time.Since(start))
					}
					mu.Unlock()
				}
				if err != nil {
					if !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
						errs[j] = err
					}
					break
				}
			}
			mu.Lock()
			res.bytes += got
			mu.Unlock()
		}()
	}
	wg.Wait()
	res.total = time.Since(start)
	return res, errors.Join(errs...)
}

type rangedShard struct {
	stream *parfetch.Stream
	part   []byte
	stripe int
}

func (r *rangedShard) next() ([]byte, error) {
	if len(r.part) == 0 {
		p, err := r.stream.Next()
		if err != nil {
			return nil, err
		}
		r.part = p
	}
	n := min(r.stripe, len(r.part))
	b := r.part[:n]
	r.part = r.part[n:]
	return b, nil
}
func (r *rangedShard) close() { r.stream.Close() }

func timeRanged(ctx context.Context, o opts, drv *drivers.LyveDriver, container string, names []string) (result, error) {
	start := time.Now()
	nParts := int((o.shardBytes + o.partBytes - 1) / o.partBytes)
	sizes := make([]int64, nParts)
	for i := range sizes {
		sizes[i] = min(o.partBytes, o.shardBytes-int64(i)*o.partBytes)
	}
	per := o.perShard()
	readers := make([]shardReader, len(names))
	for j, n := range names {
		fetch := func(fctx context.Context, i int) ([]byte, error) {
			off := int64(i) * o.partBytes
			rc, _, err := drv.GetRangeInfo(fctx, container, n, off, sizes[i])
			if err != nil {
				return nil, err
			}
			defer func() { _ = rc.Close() }()
			buf := make([]byte, sizes[i])
			if _, err := io.ReadFull(rc, buf); err != nil {
				return nil, err
			}
			return buf, nil
		}
		st := parfetch.Start(ctx, parfetch.Config{Window: int64(per) * o.partBytes, MaxParts: per,
			HedgeAfter: time.Second, FreeParts: 1}, sizes, fetch)
		readers[j] = &rangedShard{stream: st, stripe: int(o.stripe)}
	}
	return consume(o, readers, start, 0)
}

func ms(d time.Duration) float64 { return float64(d) / float64(time.Millisecond) }

func median(rs []result, f func(result) float64) float64 {
	v := make([]float64, len(rs))
	for i, r := range rs {
		v[i] = f(r)
	}
	sort.Float64s(v)
	return v[len(v)/2]
}

func minOf(rs []result, f func(result) float64) float64 {
	m := f(rs[0])
	for _, r := range rs[1:] {
		m = min(m, f(r))
	}
	return m
}

func maxOf(rs []result, f func(result) float64) float64 {
	m := f(rs[0])
	for _, r := range rs[1:] {
		m = max(m, f(r))
	}
	return m
}
