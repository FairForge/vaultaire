// wasabi-stall replays one S3 call pattern against a fixed-bucket backend
// through the SAME tuned transport the drivers use, with a per-request
// net/http trace, and classifies every slow or failed call by the stage it
// stalled in (dial, TLS, request write, response headers). An operator
// probe: never linked into the product.
//
//	wasabi-stall -mode pooled -interval 30s -count 60     # prod's probe pattern
//	wasabi-stall -mode fresh  -interval 10s -count 180    # a new connection per call
//	wasabi-stall -mode idle   -idle 5s,30s,60s,95s,120s   # reuse after an idle gap
package main

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/http/httptrace"
	"os"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/FairForge/vaultaire/internal/common"
	"github.com/FairForge/vaultaire/internal/drivers"
	"github.com/FairForge/vaultaire/internal/engine"
	"github.com/prometheus/client_golang/prometheus"
	"go.uber.org/zap"
)

type stageTimes struct {
	GetConn, GotConn, ConnStart, ConnDone, TLSDone, WroteReq, FirstByte time.Time
	Reused, WasIdle                                                     bool
	IdleTime                                                            time.Duration
	Remote                                                              string
}

type record struct {
	N        int     `json:"n"`
	At       string  `json:"at"`
	Mode     string  `json:"mode"`
	Op       string  `json:"op"`
	OK       bool    `json:"ok"`
	Err      string  `json:"err,omitempty"`
	TotalMS  float64 `json:"total_ms"`
	Stage    string  `json:"stage"` // where the time went, or where it stalled
	Reused   bool    `json:"reused"`
	WasIdle  bool    `json:"was_idle"`
	IdleMS   float64 `json:"idle_ms"`
	Remote   string  `json:"remote,omitempty"`
	DialMS   float64 `json:"dial_ms"`
	TLSMS    float64 `json:"tls_ms"`
	WriteMS  float64 `json:"write_ms"`
	WaitMS   float64 `json:"wait_ms"`           // request written → first byte
	BodyMS   float64 `json:"body_ms,omitempty"` // get: first byte → body read to EOF
	Bytes    int64   `json:"bytes,omitempty"`
	IdleGapS float64 `json:"idle_gap_s,omitempty"`
	Won      string  `json:"won,omitempty"` // hedged: which attempt answered
}

func ms(a, b time.Time) float64 {
	if a.IsZero() || b.IsZero() || b.Before(a) {
		return 0
	}
	return float64(b.Sub(a)) / float64(time.Millisecond)
}

func main() {
	mode := flag.String("mode", "pooled", "pooled | fresh | idle | hedged | driver (the real LyveDriver.Put, FIXED_BUCKET_PUT_TIMEOUT honoured, the retry counter printed)")
	hedgeAfter := flag.Duration("hedge-after", 2*time.Second, "hedged: a second attempt on a fresh connection after this; the first answer wins")
	op := flag.String("op", "headbucket", "headbucket | headobject | put | get")
	size := flag.Int64("size", 1<<20, "put: body bytes (random); each object is deleted after the call")
	key := flag.String("key", "t-_probe/_probe/slc-vaultaire-01", "object key for headobject")
	interval := flag.Duration("interval", 30*time.Second, "pause between calls (pooled/fresh)")
	count := flag.Int("count", 60, "calls (pooled/fresh)")
	idle := flag.String("idle", "5s,30s,60s,95s,120s,180s", "idle gaps for -mode idle, each repeated -idle-reps times")
	idleReps := flag.Int("idle-reps", 3, "")
	timeout := flag.Duration("timeout", 15*time.Second, "per-call deadline (the probe's)")
	endpoint := flag.String("endpoint", env("WASABI_ENDPOINT", "https://s3.us-west-1.wasabisys.com"), "")
	region := flag.String("region", env("WASABI_REGION", "us-west-1"), "")
	bucket := flag.String("bucket", env("WASABI_BUCKET", "vaultaire"), "")
	akEnv := flag.String("access-key-env", "WASABI_ACCESS_KEY", "env var holding the access key (any S3-class backend)")
	skEnv := flag.String("secret-key-env", "WASABI_SECRET_KEY", "env var holding the secret key")
	prefix := flag.String("prefix", "t-_probe/_probe/", "key prefix for the put mode's objects")
	keep := flag.Bool("keep", false, "put: leave the objects in place (no delete after each call); clean the prefix up afterwards")
	checksum := flag.String("checksum", "default", "SDK request checksum calculation: default (the SDK's: a trailing CRC on every PUT) | required (only where the API requires one)")
	flag.Parse()

	ak, sk := os.Getenv(*akEnv), os.Getenv(*skEnv)
	if ak == "" || sk == "" {
		fmt.Fprintf(os.Stderr, "%s / %s required\n", *akEnv, *skEnv)
		os.Exit(2)
	}
	opts := []drivers.TransportOption{drivers.WithHTTP1Only(), drivers.WithResponseHeaderTimeout(60 * time.Second)}
	if *mode == "fresh" {
		opts = append(opts, drivers.WithFreshConnections())
	}
	hc := drivers.TunedHTTPClient(opts...)
	mk := func(h *http.Client) *s3.Client {
		cfg, err := config.LoadDefaultConfig(context.Background(), config.WithRegion(*region),
			config.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(ak, sk, "")),
			config.WithHTTPClient(h), config.WithRetryMaxAttempts(1))
		if err != nil {
			panic(err)
		}
		return s3.NewFromConfig(cfg, func(o *s3.Options) {
			o.BaseEndpoint = aws.String(*endpoint)
			o.UsePathStyle = true
			if *checksum == "required" {
				o.RequestChecksumCalculation = aws.RequestChecksumCalculationWhenRequired
				o.ResponseChecksumValidation = aws.ResponseChecksumValidationWhenRequired
			}
		})
	}
	client := mk(hc)
	freshClient := mk(drivers.TunedHTTPClient(drivers.WithHTTP1Only(), drivers.WithFreshConnections()))

	enc := json.NewEncoder(os.Stdout)
	var okN, failN, slowN int
	one := func(n int, gap float64) {
		st := &stageTimes{}
		tr := &httptrace.ClientTrace{
			GetConn: func(string) { st.GetConn = time.Now() },
			GotConn: func(i httptrace.GotConnInfo) {
				st.GotConn = time.Now()
				st.Reused, st.WasIdle, st.IdleTime = i.Reused, i.WasIdle, i.IdleTime
				if i.Conn != nil {
					st.Remote = i.Conn.RemoteAddr().String()
				}
			},
			ConnectStart: func(string, string) {
				if st.ConnStart.IsZero() {
					st.ConnStart = time.Now()
				}
			},
			ConnectDone:          func(string, string, error) { st.ConnDone = time.Now() },
			TLSHandshakeDone:     func(tls.ConnectionState, error) { st.TLSDone = time.Now() },
			WroteRequest:         func(httptrace.WroteRequestInfo) { st.WroteReq = time.Now() },
			GotFirstResponseByte: func() { st.FirstByte = time.Now() },
		}
		ctx, cancel := context.WithTimeout(httptrace.WithClientTrace(context.Background(), tr), *timeout)
		defer cancel()
		start := time.Now()
		var bodyMS float64
		var nbytes int64
		call := func(c *s3.Client, cctx context.Context) error {
			var e error
			switch *op {
			case "headobject":
				_, e = c.HeadObject(cctx, &s3.HeadObjectInput{Bucket: aws.String(*bucket), Key: aws.String(*key)})
			case "put":
				k := fmt.Sprintf("%sstalltest-%d-%d", *prefix, os.Getpid(), n)
				buf := make([]byte, *size)
				_, _ = rand.Read(buf)
				_, e = c.PutObject(cctx, &s3.PutObjectInput{Bucket: aws.String(*bucket), Key: aws.String(k), Body: bytesReader(buf), ContentLength: aws.Int64(*size)})
				nbytes = *size
				if !*keep {
					dctx, dc := context.WithTimeout(context.Background(), 30*time.Second)
					_, _ = c.DeleteObject(dctx, &s3.DeleteObjectInput{Bucket: aws.String(*bucket), Key: aws.String(k)})
					dc()
				}
			case "get":
				out, ge := c.GetObject(cctx, &s3.GetObjectInput{Bucket: aws.String(*bucket), Key: aws.String(*key)})
				e = ge
				if ge == nil {
					t0 := time.Now()
					nbytes, e = io.Copy(io.Discard, out.Body)
					_ = out.Body.Close()
					bodyMS = ms(t0, time.Now())
				}
			default:
				_, e = c.HeadBucket(cctx, &s3.HeadBucketInput{Bucket: aws.String(*bucket)})
			}
			return e
		}
		var callErr error
		hedged := ""
		if *mode == "hedged" {
			type res struct {
				who string
				err error
			}
			ch := make(chan res, 2)
			c1, cancel1 := context.WithCancel(ctx)
			go func() { ch <- res{"first", call(client, c1)} }()
			var cancel2 context.CancelFunc = func() {}
			select {
			case r := <-ch:
				callErr, hedged = r.err, r.who
			case <-time.After(*hedgeAfter):
				var c2 context.Context
				c2, cancel2 = context.WithCancel(ctx)
				go func() { ch <- res{"hedge", call(freshClient, c2)} }()
				r := <-ch
				callErr, hedged = r.err, r.who
				if r.err != nil { // the other may still succeed
					r2 := <-ch
					callErr, hedged = r2.err, r2.who
				}
			}
			cancel1()
			cancel2()
		} else {
			callErr = call(client, ctx)
		}
		end := time.Now()
		r := record{N: n, At: start.UTC().Format(time.RFC3339Nano), Mode: *mode, Op: *op, OK: callErr == nil,
			TotalMS: ms(start, end), Reused: st.Reused, WasIdle: st.WasIdle, IdleMS: float64(st.IdleTime) / 1e6, Remote: st.Remote,
			DialMS: ms(st.ConnStart, st.ConnDone), TLSMS: ms(st.ConnDone, st.TLSDone), WriteMS: ms(st.GotConn, st.WroteReq), WaitMS: ms(st.WroteReq, st.FirstByte), IdleGapS: gap, Won: hedged, BodyMS: bodyMS, Bytes: nbytes}
		if callErr != nil {
			r.Err = callErr.Error()
			if len(r.Err) > 200 {
				r.Err = r.Err[:200]
			}
		}
		switch {
		case st.GotConn.IsZero() && !st.ConnStart.IsZero() && st.ConnDone.IsZero():
			r.Stage = "dial"
		case st.GotConn.IsZero() && !st.ConnDone.IsZero() && st.TLSDone.IsZero():
			r.Stage = "tls"
		case st.GotConn.IsZero():
			r.Stage = "getconn"
		case st.WroteReq.IsZero():
			r.Stage = "write"
		case st.FirstByte.IsZero():
			r.Stage = "response"
		default:
			r.Stage = "done"
		}
		_ = enc.Encode(r)
		switch {
		case callErr != nil:
			failN++
		case r.TotalMS > 5000:
			slowN++
		default:
			okN++
		}
	}
	switch *mode {
	case "driver":
		runDriverMode(*region, *interval, *count, *size, *timeout, enc)
		return
	case "idle":
		n := 0
		one(n, 0) // warm the pool
		for _, g := range strings.Split(*idle, ",") {
			d, err := time.ParseDuration(strings.TrimSpace(g))
			if err != nil {
				panic(err)
			}
			for i := 0; i < *idleReps; i++ {
				time.Sleep(d)
				n++
				one(n, d.Seconds())
			}
		}
	default:
		for n := 1; n <= *count; n++ {
			one(n, 0)
			if n < *count {
				time.Sleep(*interval)
			}
		}
	}
	fmt.Fprintf(os.Stderr, "summary mode=%s op=%s ok=%d slow(>5s)=%d failed=%d\n", *mode, *op, okN, slowN, failN)
}

// bytesReader is a seekable body (the SDK signs and may rewind it).
func bytesReader(b []byte) io.ReadSeeker { return &seekBuf{b: b} }

type seekBuf struct {
	b   []byte
	off int64
}

func (s *seekBuf) Read(p []byte) (int, error) {
	if s.off >= int64(len(s.b)) {
		return 0, io.EOF
	}
	n := copy(p, s.b[s.off:])
	s.off += int64(n)
	return n, nil
}

func (s *seekBuf) Seek(offset int64, whence int) (int64, error) {
	switch whence {
	case io.SeekStart:
		s.off = offset
	case io.SeekCurrent:
		s.off += offset
	case io.SeekEnd:
		s.off = int64(len(s.b)) + offset
	}
	return s.off, nil
}

// runDriverMode drives the real LyveDriver (the product's code path: the
// deadline client, FIXED_BUCKET_PUT_TIMEOUT, the fresh-connection retry)
// with 1 MiB PUTs under t-_probe/_probe/, and prints the
// vaultaire_driver_put_retries_total{driver="lyve"} counter after each call.
func runDriverMode(region string, interval time.Duration, count int, size int64, timeout time.Duration, enc *json.Encoder) {
	ak, sk := os.Getenv("LYVE_ACCESS_KEY"), os.Getenv("LYVE_SECRET_KEY")
	logger, _ := zap.NewProduction()
	d, err := drivers.NewLyveDriver(ak, sk, "", region, logger)
	if err != nil {
		panic(err)
	}
	retries := func() float64 {
		mfs, _ := prometheus.DefaultGatherer.Gather()
		for _, mf := range mfs {
			if mf.GetName() != "vaultaire_driver_put_retries_total" {
				continue
			}
			for _, m := range mf.GetMetric() {
				for _, l := range m.GetLabel() {
					if l.GetName() == "driver" && l.GetValue() == "lyve" {
						return m.GetCounter().GetValue()
					}
				}
			}
		}
		return -1
	}
	ctx := common.WithTenantID(context.Background(), "_probe")
	var okN, failN int
	for n := 1; n <= count; n++ {
		buf := make([]byte, size)
		_, _ = rand.Read(buf)
		key := fmt.Sprintf("driverprobe-%d-%d", os.Getpid(), n)
		cctx, cancel := context.WithTimeout(ctx, timeout)
		start := time.Now()
		perr := d.Put(cctx, "_probe", key, bytesReader(buf), engine.WithContentLength(size))
		cancel()
		r := map[string]any{"n": n, "at": start.UTC().Format(time.RFC3339Nano), "mode": "driver", "op": "put", "ok": perr == nil,
			"total_ms": float64(time.Since(start)) / 1e6, "retries_total": retries(), "bytes": size}
		if perr != nil {
			r["err"] = perr.Error()
			failN++
		} else {
			okN++
		}
		_ = enc.Encode(r)
		if n < count {
			time.Sleep(interval)
		}
	}
	fmt.Fprintf(os.Stderr, "summary mode=driver ok=%d failed=%d retries_total=%.0f\n", okN, failN, retries())
}

func env(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}
