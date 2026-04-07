package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/FairForge/vaultaire/internal/drivers"
	"go.uber.org/zap"
)

func main() {
	logger, _ := zap.NewProduction()

	ak := os.Getenv("QUOTALESS_ACCESS_KEY")
	sk := os.Getenv("QUOTALESS_SECRET_KEY")
	ep := os.Getenv("QUOTALESS_ENDPOINT")
	if ak == "" {
		fmt.Println("Set QUOTALESS_ACCESS_KEY, QUOTALESS_SECRET_KEY, QUOTALESS_ENDPOINT")
		os.Exit(1)
	}

	d, err := drivers.NewQuotalessDriver(ak, sk, ep, logger)
	if err != nil {
		fmt.Printf("driver error: %v\n", err)
		os.Exit(1)
	}

	ctx := context.Background()

	fmt.Println()
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println("  QUOTALESS BENCHMARK")
	fmt.Printf("  Endpoint: %s\n", ep)
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")

	fmt.Print("\n  Health check... ")
	start := time.Now()
	if err := d.HealthCheck(ctx); err != nil {
		fmt.Printf("FAIL: %v\n", err)
	} else {
		fmt.Printf("OK (%s)\n", time.Since(start).Round(time.Millisecond))
	}

	// --- Sequential throughput ---
	fmt.Println("\n  Sequential Throughput")
	fmt.Println("  ─────────────────────")
	sizes := []struct {
		name string
		size int
	}{
		{"1 KB", 1024},
		{"64 KB", 64 * 1024},
		{"1 MB", 1024 * 1024},
		{"10 MB", 10 * 1024 * 1024},
	}

	for _, s := range sizes {
		data := make([]byte, s.size)
		_, _ = rand.Read(data)
		hash := sha256.Sum256(data)
		key := fmt.Sprintf("bench/seq-%s-%d.bin", s.name, time.Now().UnixNano())

		start := time.Now()
		err := d.Put(ctx, "bench", key, bytes.NewReader(data))
		putDur := time.Since(start)
		if err != nil {
			fmt.Printf("  [%s] PUT FAIL: %v\n", s.name, err)
			continue
		}
		putMBs := float64(s.size) / putDur.Seconds() / 1024 / 1024

		start = time.Now()
		reader, err := d.Get(ctx, "bench", key)
		if err != nil {
			fmt.Printf("  [%s] GET FAIL: %v\n", s.name, err)
			_ = d.Delete(ctx, "bench", key)
			continue
		}
		got, _ := io.ReadAll(reader)
		_ = reader.Close()
		getDur := time.Since(start)
		getMBs := float64(s.size) / getDur.Seconds() / 1024 / 1024

		gotHash := sha256.Sum256(got)
		integrity := "PASS"
		if hash != gotHash {
			integrity = "FAIL"
		}

		fmt.Printf("  %-7s  up %7.2f MB/s (%s)  down %7.2f MB/s (%s)  SHA256 %s\n",
			s.name, putMBs, putDur.Round(time.Millisecond), getMBs, getDur.Round(time.Millisecond), integrity)

		_ = d.Delete(ctx, "bench", key)
	}

	// --- Small file concurrency ---
	fmt.Println("\n  Small File Concurrency (1KB × 200, 10 workers)")
	fmt.Println("  ───────────────────────────────────────────────")
	var ops int64
	var errors int64
	var totalLatency int64
	var wg sync.WaitGroup
	concStart := time.Now()

	for w := 0; w < 10; w++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			for i := 0; i < 20; i++ {
				data := make([]byte, 1024)
				_, _ = rand.Read(data)
				key := fmt.Sprintf("bench/conc-%d-%d-%d.bin", workerID, i, time.Now().UnixNano())

				opStart := time.Now()
				err := d.Put(ctx, "bench", key, bytes.NewReader(data))
				lat := time.Since(opStart)
				atomic.AddInt64(&totalLatency, int64(lat))

				if err != nil {
					atomic.AddInt64(&errors, 1)
				} else {
					atomic.AddInt64(&ops, 1)
					_ = d.Delete(ctx, "bench", key)
				}
			}
		}(w)
	}
	wg.Wait()
	concDur := time.Since(concStart)
	opsCount := atomic.LoadInt64(&ops)
	errCount := atomic.LoadInt64(&errors)
	avgLat := time.Duration(0)
	if opsCount > 0 {
		avgLat = time.Duration(atomic.LoadInt64(&totalLatency) / opsCount)
	}

	fmt.Printf("  Completed: %d ops, %d errors in %s\n", opsCount, errCount, concDur.Round(time.Millisecond))
	fmt.Printf("  Throughput: %.1f ops/s | avg latency: %s\n", float64(opsCount)/concDur.Seconds(), avgLat.Round(time.Millisecond))

	// --- List test ---
	fmt.Println("\n  List Operations")
	fmt.Println("  ───────────────")
	start = time.Now()
	items, err := d.List(ctx, "bench", "")
	listDur := time.Since(start)
	if err != nil {
		fmt.Printf("  LIST FAIL: %v\n", err)
	} else {
		fmt.Printf("  Listed %d objects in %s\n", len(items), listDur.Round(time.Millisecond))
	}

	fmt.Println()
}
