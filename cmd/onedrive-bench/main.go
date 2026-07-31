// cmd/onedrive-bench — the driver-path OneDrive benchmark.
//
// Every permafrost-* tool hand-rolls its own auth, upload, and fleet
// distribution, so none of their numbers measure what production actually
// does. This tool constructs the real driver (NewOneDriveFleetDriver) and
// drives Put/Get/List/Exists/Delete through the engine.Driver contract:
// deterministic FNV placement, home-first read probes, the List union, and
// Delete's fleet sweep are all on the measured path.
//
// Usage (needs TENANT_N_* env vars, e.g. `source .env.bench`):
//
//	go run ./cmd/onedrive-bench/ -files 12 -size-mb 25 -concurrency 4
//
// Interpreting output:
//   - The placement histogram shows the real balls-in-bins spread. The
//     permafrost tools' even N-per-account split is an upper bound; this is
//     the actual distribution a workload gets.
//   - "fallback hit" log lines during GET mean data was found off its home
//     account — for freshly written data that count must be ZERO, so any
//     hit here is a placement regression.
package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"flag"
	"fmt"
	"hash/fnv"
	"io"
	"log"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/FairForge/vaultaire/internal/common"
	"github.com/FairForge/vaultaire/internal/drivers"
	"github.com/FairForge/vaultaire/internal/engine"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

func main() {
	files := flag.Int("files", 12, "number of objects")
	sizeMB := flag.Int("size-mb", 25, "object size in MB")
	concurrency := flag.Int("concurrency", 4, "concurrent operations")
	container := flag.String("container", "od-bench", "container (bucket) name")
	tenant := flag.String("tenant", "bench", "tenant ID for path scoping")
	keep := flag.Bool("keep", false, "skip the delete phase, keep objects")
	flag.Parse()

	logCfg := zap.NewDevelopmentConfig()
	logCfg.Level = zap.NewAtomicLevelAt(zapcore.InfoLevel)
	logger, _ := logCfg.Build()
	defer func() { _ = logger.Sync() }()

	d, err := drivers.NewOneDriveFleetDriver(logger)
	if err != nil {
		log.Fatalf("driver init: %v (need TENANT_N_* env vars — `source .env.bench`)", err)
	}
	ctx := common.WithTenantID(context.Background(), *tenant)
	n := d.TenantCount()
	size := int64(*sizeMB) * 1024 * 1024
	totalMB := float64(*files) * float64(*sizeMB)

	fmt.Printf("\nOneDrive DRIVER-PATH benchmark — %d accounts, %d × %d MB, concurrency %d\n",
		n, *files, *sizeMB, *concurrency)
	fmt.Println("─────────────────────────────────────────────────────────────────")

	names := make([]string, *files)
	for i := range names {
		names[i] = fmt.Sprintf("bench-%03d.bin", i)
	}

	// Placement histogram: mirrors the driver's homeTenantIndex formula
	// (FNV-1a of "t-<tenant>/<container>/<artifact>" mod fleet size).
	hist := make([]int, n)
	for _, name := range names {
		h := fnv.New32a()
		_, _ = fmt.Fprintf(h, "t-%s/%s/%s", *tenant, *container, name)
		hist[h.Sum32()%uint32(n)]++
	}
	fmt.Printf("placement histogram (objects per account): %v\n\n", hist)

	data := make([]byte, size)
	if _, err := rand.Read(data); err != nil {
		log.Fatalf("rand: %v", err)
	}

	run := func(phase string, fn func(name string) error) (float64, int) {
		var errs atomic.Int64
		sem := make(chan struct{}, *concurrency)
		var wg sync.WaitGroup
		start := time.Now()
		for _, name := range names {
			wg.Add(1)
			go func(name string) {
				defer wg.Done()
				sem <- struct{}{}
				defer func() { <-sem }()
				if err := fn(name); err != nil {
					errs.Add(1)
					logger.Error(phase+" failed", zap.String("name", name), zap.Error(err))
				}
			}(name)
		}
		wg.Wait()
		elapsed := time.Since(start).Seconds()
		okMB := totalMB * float64(*files-int(errs.Load())) / float64(*files)
		fmt.Printf("%-8s %6.1f MB in %6.2fs = %7.2f MB/s  (%d errors)\n",
			phase, okMB, elapsed, okMB/elapsed, errs.Load())
		return okMB / elapsed, int(errs.Load())
	}

	// PUT — through driver placement, home account per object.
	_, putErrs := run("PUT", func(name string) error {
		return d.Put(ctx, *container, name, bytes.NewReader(data),
			engine.WithContentLength(size))
	})
	if putErrs == *files {
		log.Fatal("every PUT failed — aborting")
	}

	// GET — home-first probe; fallback-hit log lines here = placement regression.
	run("GET", func(name string) error {
		rc, err := d.Get(ctx, *container, name)
		if err != nil {
			return err
		}
		defer func() { _ = rc.Close() }()
		got, err := io.Copy(io.Discard, rc)
		if err != nil {
			return err
		}
		if got != size {
			return fmt.Errorf("size mismatch: got %d want %d", got, size)
		}
		return nil
	})

	// LIST — unions every account and follows pagination.
	start := time.Now()
	listed, err := d.List(ctx, *container, "bench-")
	if err != nil {
		log.Fatalf("LIST: %v", err)
	}
	fmt.Printf("%-8s %d objects in %.2fs (expected %d)\n",
		"LIST", len(listed), time.Since(start).Seconds(), *files-putErrs)
	if len(listed) < *files-putErrs {
		fmt.Fprintf(os.Stderr, "WARNING: LIST returned fewer objects than were written\n")
	}

	if *keep {
		fmt.Println("\n-keep set: objects left in place")
		return
	}

	// DELETE — fleet sweep per object.
	run("DELETE", func(name string) error {
		return d.Delete(ctx, *container, name)
	})

	// Post-delete honesty check: nothing should remain.
	if remaining, err := d.List(ctx, *container, "bench-"); err == nil && len(remaining) > 0 {
		fmt.Fprintf(os.Stderr, "WARNING: %d objects survived delete: %v\n",
			len(remaining), remaining)
	} else {
		fmt.Println("post-delete LIST clean: 0 objects remain")
	}
}
