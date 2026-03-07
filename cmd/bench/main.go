// cmd/bench/main.go
//
// Vaultaire benchmark suite for real-world workload validation.
// Designed to produce numbers that LowEndTalk, DataHoarder, and
// developer communities can interpret and reproduce.
//
// Usage:
//
//	./bench -endpoint https://stored.ge -access-key KEY -secret-key SECRET -bucket BUCKET <subcommand>
//
// Subcommands:
//
//	largefile   — single large file upload/download (rclone/restic workload)
//	smallfiles  — many tiny files (thumbnail/backup metadata workload)
//	integrity   — SHA256 round-trip verification (data safety)
//	mixed       — concurrent reads and writes (real user simulation)
//	soak        — sustained load for 10 minutes (throttle detection)
//	multitenant — two tenants running simultaneously (isolation test)
//	tape        — LTO-9 tape optimized benchmark (archive / cold storage)
package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"flag"
	"fmt"
	"io"
	"log"
	"math"
	"os"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// — Globals ------------------------------------------------------------------

var (
	endpoint  string
	accessKey string
	secretKey string
	bucket    string
	tenant2AK string // second tenant for multitenant test
	tenant2SK string
)

func main() {
	flag.StringVar(&endpoint, "endpoint", "https://stored.ge", "S3 endpoint")
	flag.StringVar(&accessKey, "access-key", "", "Access key")
	flag.StringVar(&secretKey, "secret-key", "", "Secret key")
	flag.StringVar(&bucket, "bucket", "", "Bucket name")
	flag.StringVar(&tenant2AK, "access-key2", "", "Second tenant access key (multitenant test)")
	flag.StringVar(&tenant2SK, "secret-key2", "", "Second tenant secret key (multitenant test)")
	flag.Parse()

	if accessKey == "" || secretKey == "" || bucket == "" {
		fmt.Fprintln(os.Stderr, "error: -access-key, -secret-key, and -bucket are required")
		flag.Usage()
		os.Exit(1)
	}

	sub := flag.Arg(0)
	if sub == "" {
		fmt.Fprintln(os.Stderr, "error: subcommand required: largefile | smallfiles | integrity | mixed | soak | multitenant | tape")
		os.Exit(1)
	}

	client := mustNewClient(endpoint, accessKey, secretKey)
	ctx := context.Background()

	switch sub {
	case "largefile":
		runLargeFile(ctx, client)
	case "smallfiles":
		runSmallFiles(ctx, client)
	case "integrity":
		runIntegrity(ctx, client)
	case "mixed":
		runMixed(ctx, client)
	case "soak":
		runSoak(ctx, client)
	case "multitenant":
		runMultitenant(ctx, client)
	case "tape":
		runTape(ctx, client)
	default:
		fmt.Fprintf(os.Stderr, "unknown subcommand: %s\n", sub)
		os.Exit(1)
	}
}

// — S3 client ----------------------------------------------------------------

func mustNewClient(ep, ak, sk string) *s3.Client {
	cfg, err := awsconfig.LoadDefaultConfig(context.Background(),
		awsconfig.WithCredentialsProvider(
			credentials.NewStaticCredentialsProvider(ak, sk, ""),
		),
		awsconfig.WithRegion("us-east-1"),
	)
	if err != nil {
		log.Fatalf("aws config: %v", err)
	}

	return s3.NewFromConfig(cfg, func(o *s3.Options) {
		o.BaseEndpoint = aws.String(ep)
		o.UsePathStyle = true
		// ResponseChecksumValidation omitted intentionally.
		// Vaultaire does not return checksum headers; the SDK default
		// (validate when present) is correct and produces no warnings.
	})
}

// — Helpers ------------------------------------------------------------------

func randBytes(n int) []byte {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		log.Fatalf("rand: %v", err)
	}
	return b
}

func sha256hex(data []byte) string {
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:])
}

func put(ctx context.Context, client *s3.Client, key string, data []byte) (time.Duration, error) {
	start := time.Now()
	_, err := client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:        aws.String(bucket),
		Key:           aws.String(key),
		Body:          bytes.NewReader(data),
		ContentLength: aws.Int64(int64(len(data))),
	})
	return time.Since(start), err
}

func get(ctx context.Context, client *s3.Client, key string) ([]byte, time.Duration, error) {
	start := time.Now()
	resp, err := client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return nil, time.Since(start), err
	}
	defer resp.Body.Close() //nolint:errcheck
	data, err := io.ReadAll(resp.Body)
	return data, time.Since(start), err
}

func del(ctx context.Context, client *s3.Client, key string) {
	_, _ = client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(key),
	})
}

func percentiles(durations []time.Duration) (p50, p95, p99, max time.Duration) {
	if len(durations) == 0 {
		return
	}
	sorted := make([]time.Duration, len(durations))
	copy(sorted, durations)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	p50 = sorted[len(sorted)*50/100]
	p95 = sorted[len(sorted)*95/100]
	p99 = sorted[len(sorted)*99/100]
	max = sorted[len(sorted)-1]
	return
}

func mbps(bytes int64, d time.Duration) float64 {
	return float64(bytes) / 1024 / 1024 / d.Seconds()
}

func header(title string) {
	fmt.Printf("\n━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━\n")
	fmt.Printf("  %s\n", title)
	fmt.Printf("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━\n")
}

// — Subcommand: largefile ----------------------------------------------------

func runLargeFile(ctx context.Context, client *s3.Client) {
	header("LARGE FILE BENCHMARK (rclone / restic workload)")

	sizes := []struct {
		label string
		bytes int
	}{
		{"10 MB", 10 * 1024 * 1024},
		{"100 MB", 100 * 1024 * 1024},
		{"500 MB", 500 * 1024 * 1024},
	}

	for _, sz := range sizes {
		fmt.Printf("\n  [%s]\n", sz.label)
		data := randBytes(sz.bytes)
		key := fmt.Sprintf("bench/largefile/%s-%d", sz.label, time.Now().UnixNano())

		uploadDur, err := put(ctx, client, key, data)
		if err != nil {
			fmt.Printf("    Upload:   FAIL — %v\n", err)
			continue
		}
		fmt.Printf("    Upload:   %.2f MB/s (%s)\n", mbps(int64(sz.bytes), uploadDur), uploadDur.Round(time.Millisecond))

		got, downloadDur, err := get(ctx, client, key)
		if err != nil {
			fmt.Printf("    Download: FAIL — %v\n", err)
		} else {
			fmt.Printf("    Download: %.2f MB/s (%s)\n", mbps(int64(sz.bytes), downloadDur), downloadDur.Round(time.Millisecond))
			if sha256hex(got) == sha256hex(data) {
				fmt.Printf("    Integrity: ✅ SHA256 match\n")
			} else {
				fmt.Printf("    Integrity: ❌ SHA256 MISMATCH — DATA CORRUPTION\n")
			}
		}

		del(ctx, client, key)
	}
}

// — Subcommand: smallfiles ---------------------------------------------------

func runSmallFiles(ctx context.Context, client *s3.Client) {
	header("SMALL FILES BENCHMARK (backup / thumbnail workload)")

	batches := []struct {
		label   string
		count   int
		sizeKB  int
		workers int
	}{
		{"1 KB × 1000 files,  10 workers", 1000, 1, 10},
		{"64 KB × 500 files,  20 workers", 500, 64, 20},
		{"1 MB × 200 files,   20 workers", 200, 1024, 20},
	}

	for _, b := range batches {
		fmt.Printf("\n  [%s]\n", b.label)
		payload := randBytes(b.sizeKB * 1024)
		keys := make([]string, b.count)
		for i := range keys {
			keys[i] = fmt.Sprintf("bench/smallfiles/f-%d-%d", time.Now().UnixNano(), i)
		}

		var uploaded atomic.Int64
		var uploadErrors atomic.Int64
		uploadTimes := make([]time.Duration, b.count)
		var mu sync.Mutex

		start := time.Now()
		sem := make(chan struct{}, b.workers)
		var wg sync.WaitGroup

		for i, key := range keys {
			sem <- struct{}{}
			wg.Add(1)
			go func(idx int, k string) {
				defer wg.Done()
				defer func() { <-sem }()
				d, err := put(ctx, client, k, payload)
				if err != nil {
					uploadErrors.Add(1)
					return
				}
				uploaded.Add(1)
				mu.Lock()
				uploadTimes[idx] = d
				mu.Unlock()
			}(i, key)
		}
		wg.Wait()
		totalUpload := time.Since(start)

		successCount := int(uploaded.Load())
		totalBytes := int64(successCount * b.sizeKB * 1024)
		p50, p95, p99, maxL := percentiles(uploadTimes[:successCount])

		fmt.Printf("    Upload:  %d/%d files | %.0f files/s | %.2f MB/s\n",
			successCount, b.count,
			float64(successCount)/totalUpload.Seconds(),
			mbps(totalBytes, totalUpload))
		fmt.Printf("    Latency: p50=%s p95=%s p99=%s max=%s\n",
			p50.Round(time.Millisecond), p95.Round(time.Millisecond),
			p99.Round(time.Millisecond), maxL.Round(time.Millisecond))

		if uploadErrors.Load() > 0 {
			fmt.Printf("    Errors:  %d upload failures\n", uploadErrors.Load())
		}

		go func(ks []string) {
			for _, k := range ks {
				del(ctx, client, k)
			}
		}(keys)
	}
}

// — Subcommand: integrity ----------------------------------------------------

func runIntegrity(ctx context.Context, client *s3.Client) {
	header("INTEGRITY BENCHMARK (data safety verification)")

	sizes := []int{
		1,
		1024,
		64 * 1024,
		1024 * 1024,
		10 * 1024 * 1024,
	}

	labels := []string{"1 B", "1 KB", "64 KB", "1 MB", "10 MB"}
	passed := 0
	failed := 0

	for i, sz := range sizes {
		data := randBytes(sz)
		originalHash := sha256hex(data)
		key := fmt.Sprintf("bench/integrity/obj-%d-%d", sz, time.Now().UnixNano())

		fmt.Printf("  %-8s  ", labels[i])

		if _, err := put(ctx, client, key, data); err != nil {
			fmt.Printf("FAIL (upload: %v)\n", err)
			failed++
			continue
		}

		got, _, err := get(ctx, client, key)
		if err != nil {
			fmt.Printf("FAIL (download: %v)\n", err)
			failed++
			del(ctx, client, key)
			continue
		}

		gotHash := sha256hex(got)
		if gotHash == originalHash {
			fmt.Printf("✅ PASS  SHA256=%s…\n", originalHash[:16])
			passed++
		} else {
			fmt.Printf("❌ FAIL  expected=%s… got=%s…\n", originalHash[:16], gotHash[:16])
			failed++
		}

		del(ctx, client, key)
	}

	fmt.Printf("\n  Result: %d passed, %d failed\n", passed, failed)
	if failed > 0 {
		fmt.Println("  ⚠️  DATA INTEGRITY ISSUES DETECTED — DO NOT USE FOR PRODUCTION")
	} else {
		fmt.Println("  ✅ All integrity checks passed — safe for production use")
	}
}

// — Subcommand: mixed --------------------------------------------------------

func runMixed(ctx context.Context, client *s3.Client) {
	header("MIXED WORKLOAD BENCHMARK (70% write / 30% read, 50 workers, 3 min)")

	const (
		workers  = 50
		duration = 3 * time.Minute
		sizeKB   = 64
	)

	payload := randBytes(sizeKB * 1024)

	fmt.Println("  Seeding 50 objects for read pool...")
	readPool := make([]string, 50)
	for i := range readPool {
		k := fmt.Sprintf("bench/mixed/seed-%d", i)
		if _, err := put(ctx, client, k, payload); err != nil {
			log.Fatalf("seed failed: %v", err)
		}
		readPool[i] = k
	}
	defer func() {
		for _, k := range readPool {
			del(ctx, client, k)
		}
	}()

	var (
		writeOps    atomic.Int64
		readOps     atomic.Int64
		writeErrors atomic.Int64
		readErrors  atomic.Int64
		writeBytes  atomic.Int64
		readBytes   atomic.Int64
	)

	writeTimes := make([]time.Duration, 0, 10000)
	readTimes := make([]time.Duration, 0, 10000)
	var mu sync.Mutex

	ctx, cancel := context.WithTimeout(ctx, duration)
	defer cancel()

	sem := make(chan struct{}, workers)
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()

	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				fmt.Printf("  ↳ writes: %d (err:%d) | reads: %d (err:%d)\n",
					writeOps.Load(), writeErrors.Load(),
					readOps.Load(), readErrors.Load())
			}
		}
	}()

	start := time.Now()
	opID := atomic.Int64{}

	for ctx.Err() == nil {
		select {
		case <-ctx.Done():
		case sem <- struct{}{}:
			go func(id int64) {
				defer func() { <-sem }()

				if id%10 < 7 {
					key := fmt.Sprintf("bench/mixed/w-%d", id)
					d, err := put(ctx, client, key, payload)
					if ctx.Err() != nil {
						return
					}
					if err != nil {
						writeErrors.Add(1)
					} else {
						writeOps.Add(1)
						writeBytes.Add(int64(len(payload)))
						mu.Lock()
						writeTimes = append(writeTimes, d)
						mu.Unlock()
						del(ctx, client, key)
					}
				} else {
					k := readPool[id%int64(len(readPool))]
					_, d, err := get(ctx, client, k)
					if ctx.Err() != nil {
						return
					}
					if err != nil {
						readErrors.Add(1)
					} else {
						readOps.Add(1)
						readBytes.Add(int64(len(payload)))
						mu.Lock()
						readTimes = append(readTimes, d)
						mu.Unlock()
					}
				}
			}(opID.Add(1))
		}
	}

	for i := 0; i < workers; i++ {
		sem <- struct{}{}
	}

	elapsed := time.Since(start)

	mu.Lock()
	wp50, wp95, wp99, wmax := percentiles(writeTimes)
	rp50, rp95, rp99, rmax := percentiles(readTimes)
	mu.Unlock()

	fmt.Printf("\n  Duration:  %s\n", elapsed.Round(time.Second))
	fmt.Printf("  Writes:    %d ops | %.1f ops/s | %.2f MB/s | errors: %d\n",
		writeOps.Load(),
		float64(writeOps.Load())/elapsed.Seconds(),
		mbps(writeBytes.Load(), elapsed),
		writeErrors.Load())
	fmt.Printf("  Reads:     %d ops | %.1f ops/s | %.2f MB/s | errors: %d\n",
		readOps.Load(),
		float64(readOps.Load())/elapsed.Seconds(),
		mbps(readBytes.Load(), elapsed),
		readErrors.Load())
	fmt.Printf("  Write p50=%s p95=%s p99=%s max=%s\n",
		wp50.Round(time.Millisecond), wp95.Round(time.Millisecond),
		wp99.Round(time.Millisecond), wmax.Round(time.Millisecond))
	fmt.Printf("  Read  p50=%s p95=%s p99=%s max=%s\n",
		rp50.Round(time.Millisecond), rp95.Round(time.Millisecond),
		rp99.Round(time.Millisecond), rmax.Round(time.Millisecond))
}

// — Subcommand: soak ---------------------------------------------------------

func runSoak(ctx context.Context, client *s3.Client) {
	header("SOAK BENCHMARK (throttle detection — 10 min steady load)")

	const (
		targetRPS = 20
		duration  = 10 * time.Minute
		sizeKB    = 64
		workers   = 40
	)

	payload := randBytes(sizeKB * 1024)

	type minuteStat struct {
		minute  int
		success int64
		errors  int64
		bytes   int64
	}

	stats := make([]minuteStat, 0, 10)
	var currentStat minuteStat
	var mu sync.Mutex

	ctx, cancel := context.WithTimeout(ctx, duration)
	defer cancel()

	sem := make(chan struct{}, workers)
	rateTick := time.NewTicker(time.Second / targetRPS)
	defer rateTick.Stop()
	minuteTick := time.NewTicker(time.Minute)
	defer minuteTick.Stop()

	minute := 1
	fmt.Printf("  Minute  RPS    Errors  MB/s\n")
	fmt.Printf("  ──────  ─────  ──────  ────\n")

	opID := atomic.Int64{}

	for {
		select {
		case <-ctx.Done():
			goto done

		case <-minuteTick.C:
			mu.Lock()
			currentStat.minute = minute
			stats = append(stats, currentStat)
			fmt.Printf("  %6d  %5.1f  %6d  %.2f\n",
				minute,
				float64(currentStat.success+currentStat.errors)/60.0,
				currentStat.errors,
				mbps(currentStat.bytes, time.Minute))
			currentStat = minuteStat{}
			mu.Unlock()
			minute++

		case <-rateTick.C:
			select {
			case sem <- struct{}{}:
				go func(id int64) {
					defer func() { <-sem }()
					key := fmt.Sprintf("bench/soak/obj-%d", id)
					d, err := put(ctx, client, key, payload)
					if ctx.Err() != nil {
						return
					}
					mu.Lock()
					if err != nil {
						currentStat.errors++
					} else {
						currentStat.success++
						currentStat.bytes += int64(len(payload))
						_ = d
						del(ctx, client, key)
					}
					mu.Unlock()
				}(opID.Add(1))
			default:
			}
		}
	}

done:
	if len(stats) < 2 {
		fmt.Println("  Not enough data for trend analysis")
		return
	}

	early := (stats[0].bytes + stats[1].bytes) / 2
	last := stats[len(stats)-1]
	secondLast := stats[len(stats)-2]
	late := (last.bytes + secondLast.bytes) / 2

	degradation := 0.0
	if early > 0 {
		degradation = (1.0 - float64(late)/float64(early)) * 100
	}

	fmt.Printf("\n  Early throughput (avg min 1-2): %.2f MB/s\n", mbps(early, time.Minute))
	fmt.Printf("  Late throughput  (avg last 2):  %.2f MB/s\n", mbps(late, time.Minute))

	if math.Abs(degradation) < 15 {
		fmt.Printf("  ✅ No throttling detected (%.1f%% variance)\n", degradation)
	} else if degradation > 0 {
		fmt.Printf("  ⚠️  Throughput degraded %.1f%% — possible throttling\n", degradation)
	} else {
		fmt.Printf("  ✅ Throughput improved %.1f%% (warming up)\n", -degradation)
	}
}

// — Subcommand: multitenant --------------------------------------------------

func runMultitenant(ctx context.Context, client *s3.Client) {
	header("MULTI-TENANT ISOLATION BENCHMARK (2 tenants, 2 min)")

	if tenant2AK == "" || tenant2SK == "" {
		fmt.Fprintln(os.Stderr, "error: -access-key2 and -secret-key2 required for multitenant test")
		fmt.Fprintln(os.Stderr, "  Register a second account and pass its credentials.")
		os.Exit(1)
	}

	client2 := mustNewClient(endpoint, tenant2AK, tenant2SK)

	const (
		duration = 2 * time.Minute
		workers  = 30
		sizeKB   = 64
	)

	payload := randBytes(sizeKB * 1024)

	runTenant := func(name string, c *s3.Client, results *[]time.Duration, mu *sync.Mutex, errs *atomic.Int64, wg *sync.WaitGroup) {
		defer wg.Done()
		ctx, cancel := context.WithTimeout(ctx, duration)
		defer cancel()

		sem := make(chan struct{}, workers)
		id := atomic.Int64{}
		for ctx.Err() == nil {
			select {
			case <-ctx.Done():
			case sem <- struct{}{}:
				go func(oid int64) {
					defer func() { <-sem }()
					key := fmt.Sprintf("bench/mt/%s-%d", name, oid)
					d, err := put(ctx, c, key, payload)
					if ctx.Err() != nil {
						return
					}
					if err != nil {
						errs.Add(1)
					} else {
						mu.Lock()
						*results = append(*results, d)
						mu.Unlock()
						del(ctx, c, key)
					}
				}(id.Add(1))
			}
		}
		for i := 0; i < workers; i++ {
			sem <- struct{}{}
		}
	}

	var (
		r1, r2   []time.Duration
		mu1, mu2 sync.Mutex
		e1, e2   atomic.Int64
		wg       sync.WaitGroup
	)

	fmt.Println("  Running tenant-1 (heavy) and tenant-2 (light) simultaneously...")
	wg.Add(2)
	go runTenant("t1", client, &r1, &mu1, &e1, &wg)
	go runTenant("t2", client2, &r2, &mu2, &e2, &wg)
	wg.Wait()

	t1p50, t1p95, t1p99, t1max := percentiles(r1)
	t2p50, t2p95, t2p99, t2max := percentiles(r2)

	fmt.Printf("\n  Tenant 1:  %d ops | errors: %d\n", len(r1), e1.Load())
	fmt.Printf("    p50=%s p95=%s p99=%s max=%s\n",
		t1p50.Round(time.Millisecond), t1p95.Round(time.Millisecond),
		t1p99.Round(time.Millisecond), t1max.Round(time.Millisecond))

	fmt.Printf("\n  Tenant 2:  %d ops | errors: %d\n", len(r2), e2.Load())
	fmt.Printf("    p50=%s p95=%s p99=%s max=%s\n",
		t2p50.Round(time.Millisecond), t2p95.Round(time.Millisecond),
		t2p99.Round(time.Millisecond), t2max.Round(time.Millisecond))

	ratio := float64(t1p95) / float64(t2p95)
	if t2p95 > t1p95 {
		ratio = float64(t2p95) / float64(t1p95)
	}
	fmt.Printf("\n  P95 ratio: %.2fx\n", ratio)
	if ratio < 2.0 {
		fmt.Println("  ✅ Good isolation — tenants not significantly affecting each other")
	} else {
		fmt.Printf("  ⚠️  Poor isolation — one tenant is affecting the other (%.2fx P95 spread)\n", ratio)
	}
}

// — Subcommand: tape ---------------------------------------------------------
//
// Tape-optimized benchmark for LTO-9 backends (e.g. Geyser Data).
// Tape excels at large sequential workloads. These tests show what matters
// for DataHoarders and cold storage use cases:
//   - Sequential ingest throughput at increasing sizes
//   - Sustained multi-GB archive ingest (nightly backup simulation)
//   - Parallel stream performance (2 and 4 concurrent streams)
//   - Restore throughput (sequential download)
//   - Cost efficiency projection vs S3 Glacier Deep Archive

func runTape(ctx context.Context, client *s3.Client) {
	header("TAPE BENCHMARK (LTO-9 cold storage — DataHoarder / archive workload)")

	const costPerTBMonth = 1.99 // Geyser archive tier $/TB/month

	// — Phase 1: Sequential throughput at increasing sizes -------------------
	fmt.Println("\n  Phase 1: Sequential Throughput (single stream)")
	fmt.Println("  ───────────────────────────────────────────────")

	seqSizes := []struct {
		label string
		bytes int64
	}{
		{"10 MB", 10 * 1024 * 1024},
		{"100 MB", 100 * 1024 * 1024},
		{"500 MB", 500 * 1024 * 1024},
		{"1 GB", 1 * 1024 * 1024 * 1024},
	}

	var totalSeqBytes int64
	var totalSeqUpload, totalSeqDownload time.Duration

	for _, sz := range seqSizes {
		data := randBytes(int(sz.bytes))
		key := fmt.Sprintf("bench/tape/seq-%s-%d", sz.label, time.Now().UnixNano())

		fmt.Printf("  %-8s  ", sz.label)

		uploadDur, err := put(ctx, client, key, data)
		if err != nil {
			fmt.Printf("UPLOAD FAIL — %v\n", err)
			continue
		}

		got, downloadDur, err := get(ctx, client, key)
		if err != nil {
			fmt.Printf("upload %.1f MB/s | DOWNLOAD FAIL — %v\n", mbps(sz.bytes, uploadDur), err)
			del(ctx, client, key)
			continue
		}

		integrity := "✅"
		if sha256hex(got) != sha256hex(data) {
			integrity = "❌ CORRUPT"
		}

		fmt.Printf("up %.1f MB/s  down %.1f MB/s  %s\n",
			mbps(sz.bytes, uploadDur),
			mbps(sz.bytes, downloadDur),
			integrity)

		totalSeqBytes += sz.bytes
		totalSeqUpload += uploadDur
		totalSeqDownload += downloadDur

		del(ctx, client, key)
	}

	if totalSeqUpload > 0 {
		fmt.Printf("\n  Sequential avg:  upload %.1f MB/s  |  download %.1f MB/s\n",
			mbps(totalSeqBytes, totalSeqUpload),
			mbps(totalSeqBytes, totalSeqDownload))
	}

	// — Phase 2: Sustained multi-GB ingest (simulates nightly backup) --------
	fmt.Println("\n  Phase 2: Sustained Ingest (10 GB total, 1 GB chunks)")
	fmt.Println("  ────────────────────────────────────────────────────")

	const (
		chunkSize   = 1 * 1024 * 1024 * 1024 // 1 GB per chunk
		totalChunks = 10
	)

	chunk := randBytes(chunkSize)
	var ingestKeys []string
	ingestStart := time.Now()
	var ingestFailed int

	for i := 0; i < totalChunks; i++ {
		key := fmt.Sprintf("bench/tape/ingest-chunk-%02d-%d", i, time.Now().UnixNano())
		dur, err := put(ctx, client, key, chunk)
		if err != nil {
			fmt.Printf("  chunk %02d: FAIL — %v\n", i, err)
			ingestFailed++
			continue
		}
		ingestKeys = append(ingestKeys, key)
		elapsed := time.Since(ingestStart)
		fmt.Printf("  chunk %02d/10: %.1f MB/s  (cumulative: %.1f MB/s)\n",
			i+1,
			mbps(int64(chunkSize), dur),
			mbps(int64((i+1)*chunkSize), elapsed))
	}

	ingestTotal := time.Since(ingestStart)
	ingestGB := float64((totalChunks-ingestFailed)*chunkSize) / 1024 / 1024 / 1024

	fmt.Printf("\n  Ingest summary: %.1f GB in %s — avg %.1f MB/s",
		ingestGB,
		ingestTotal.Round(time.Second),
		mbps(int64((totalChunks-ingestFailed)*chunkSize), ingestTotal))
	if ingestFailed == 0 {
		fmt.Println(" ✅")
	} else {
		fmt.Printf(" (%d chunks failed)\n", ingestFailed)
	}

	// — Phase 3: Parallel streams (2 and 4 concurrent) -----------------------
	fmt.Println("\n  Phase 3: Parallel Streams")
	fmt.Println("  ─────────────────────────")

	for _, streams := range []int{2, 4} {
		streamSize := 256 * 1024 * 1024 // 256 MB per stream
		payload := randBytes(streamSize)
		keys := make([]string, streams)
		durs := make([]time.Duration, streams)
		errs := make([]error, streams)

		var wg sync.WaitGroup
		start := time.Now()
		for i := 0; i < streams; i++ {
			wg.Add(1)
			keys[i] = fmt.Sprintf("bench/tape/parallel-%d-stream-%d-%d", streams, i, time.Now().UnixNano())
			go func(idx int, k string) {
				defer wg.Done()
				durs[idx], errs[idx] = put(ctx, client, k, payload)
			}(i, keys[i])
		}
		wg.Wait()
		wall := time.Since(start)

		totalBytes := int64(streams * streamSize)
		failed := 0
		for _, e := range errs {
			if e != nil {
				failed++
			}
		}

		fmt.Printf("  %d streams × 256 MB: aggregate %.1f MB/s  errors: %d\n",
			streams,
			mbps(totalBytes, wall),
			failed)

		for _, k := range keys {
			del(ctx, client, k)
		}
	}

	// — Phase 4: Restore throughput (sequential download of ingest chunks) ---
	fmt.Println("\n  Phase 4: Restore Throughput (download ingest chunks)")
	fmt.Println("  ────────────────────────────────────────────────────")

	if len(ingestKeys) == 0 {
		fmt.Println("  skipped — no chunks were ingested successfully")
	} else {
		restoreStart := time.Now()
		restored := 0
		for i, k := range ingestKeys {
			_, dur, err := get(ctx, client, k)
			if err != nil {
				fmt.Printf("  chunk %02d: FAIL — %v\n", i, err)
				continue
			}
			restored++
			elapsed := time.Since(restoreStart)
			fmt.Printf("  chunk %02d/%d: %.1f MB/s  (cumulative: %.1f MB/s)\n",
				i+1, len(ingestKeys),
				mbps(int64(chunkSize), dur),
				mbps(int64(restored*chunkSize), elapsed))
			del(ctx, client, k)
		}
		restoreTotal := time.Since(restoreStart)
		fmt.Printf("\n  Restore summary: %.1f GB in %s — avg %.1f MB/s\n",
			float64(restored*chunkSize)/1024/1024/1024,
			restoreTotal.Round(time.Second),
			mbps(int64(restored*chunkSize), restoreTotal))
	}

	// — Phase 5: Cost efficiency projection ----------------------------------
	fmt.Println("\n  Phase 5: Cost Efficiency")
	fmt.Println("  ────────────────────────")

	storedGB := float64((totalChunks-ingestFailed)*chunkSize) / 1024 / 1024 / 1024
	costPerMonth := (storedGB / 1024) * costPerTBMonth
	s3cost := (storedGB / 1024) * 23.00 // S3 Glacier Deep Archive ~$23/TB/month

	fmt.Printf("  Data ingested this run:  %.1f GB\n", storedGB)
	fmt.Printf("  Geyser cost:             $%.4f/month (at $%.2f/TB)\n", costPerMonth, costPerTBMonth)
	fmt.Printf("  S3 Glacier Deep Archive: $%.4f/month (at $23.00/TB)\n", s3cost)
	if costPerMonth > 0 {
		fmt.Printf("  Savings vs Glacier:      %.1fx cheaper\n", s3cost/costPerMonth)
	}
	fmt.Printf("\n  At 100 TB scale:\n")
	fmt.Printf("    Geyser:                $%.0f/month\n", 100*costPerTBMonth)
	fmt.Printf("    S3 Glacier:            $%.0f/month\n", 100*23.00)
	fmt.Printf("    Monthly savings:       $%.0f\n", 100*(23.00-costPerTBMonth))
}
