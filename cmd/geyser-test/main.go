// cmd/geyser-test/main.go
//
// Smoke-tests the Geyser LTO-9 tape backend end-to-end.
// Tape has a key constraint: PutObject without ContentLength causes
// the S3 gateway to buffer the entire object in memory. We test
// that our driver handles this correctly.
//
// Usage:
//
//	go run ./cmd/geyser-test \
//	  -access-key YOUR_KEY \
//	  -secret-key YOUR_SECRET \
//	  -bucket YOUR_UUID_BUCKET
package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"time"

	"github.com/FairForge/vaultaire/internal/drivers"
	"go.uber.org/zap"
)

func main() {
	accessKey := flag.String("access-key", "", "Geyser access key")
	secretKey := flag.String("secret-key", "", "Geyser secret key")
	bucket := flag.String("bucket", "", "Geyser UUID bucket name")
	tenantID := flag.String("tenant", "test-tenant", "Tenant ID for key namespacing")
	flag.Parse()

	if *accessKey == "" || *secretKey == "" || *bucket == "" {
		fmt.Fprintln(os.Stderr, "error: -access-key, -secret-key, and -bucket are required")
		flag.Usage()
		os.Exit(1)
	}

	logger, _ := zap.NewDevelopment()
	defer logger.Sync() //nolint:errcheck

	driver, err := drivers.NewGeyserDriver(*accessKey, *secretKey, *bucket, *tenantID, logger)
	if err != nil {
		log.Fatalf("failed to create Geyser driver: %v", err)
	}

	ctx := context.Background()
	failed := 0

	fmt.Println("\n🧊 Geyser Tape Backend — Smoke Test")
	fmt.Printf("   Bucket: %s\n\n", *bucket)

	// — Test 1: Health Check -------------------------------------------------
	failed += run("HealthCheck", func() error {
		return driver.HealthCheck(ctx)
	})

	// — Test 2: PUT small object ---------------------------------------------
	const container = "smoke-test"
	const artifact = "hello.txt"
	payload := []byte("hello from vaultaire smoke test")

	failed += run("PUT (small, 31 bytes)", func() error {
		return driver.Put(ctx, container, artifact, bytes.NewReader(payload))
	})

	// Tape has higher latency than disk — give it a moment before GET
	time.Sleep(2 * time.Second)

	// — Test 3: GET and verify integrity ------------------------------------
	failed += run("GET + verify integrity", func() error {
		rc, err := driver.Get(ctx, container, artifact)
		if err != nil {
			return err
		}
		defer rc.Close() //nolint:errcheck

		got, err := io.ReadAll(rc)
		if err != nil {
			return fmt.Errorf("read body: %w", err)
		}
		if !bytes.Equal(got, payload) {
			return fmt.Errorf("data mismatch: got %q, want %q", got, payload)
		}
		return nil
	})

	// — Test 4: PUT large object (1 MB) -------------------------------------
	largeBuf := make([]byte, 1*1024*1024)
	if _, err := rand.Read(largeBuf); err != nil {
		log.Fatalf("rand: %v", err)
	}

	failed += run("PUT (large, 1 MB)", func() error {
		return driver.Put(ctx, container, "large.bin", bytes.NewReader(largeBuf))
	})

	// — Test 5: LIST ---------------------------------------------------------
	failed += run("LIST container", func() error {
		keys, err := driver.List(ctx, container, "")
		if err != nil {
			return err
		}
		fmt.Printf("      found %d objects: %v\n", len(keys), keys)
		return nil
	})

	// — Test 6: DELETE -------------------------------------------------------
	failed += run("DELETE hello.txt", func() error {
		return driver.Delete(ctx, container, artifact)
	})

	failed += run("DELETE large.bin", func() error {
		return driver.Delete(ctx, container, "large.bin")
	})

	// — Summary --------------------------------------------------------------
	fmt.Println()
	if failed == 0 {
		fmt.Println("✅ All Geyser tests passed — tape backend is operational")
	} else {
		fmt.Fprintf(os.Stderr, "❌ %d test(s) failed — check credentials and bucket UUID\n", failed)
		os.Exit(1)
	}
}

// run executes a named test, prints pass/fail with timing, and returns 0 or 1.
func run(name string, fn func() error) int {
	fmt.Printf("  %-35s ", name+"...")
	start := time.Now()
	err := fn()
	elapsed := time.Since(start).Round(time.Millisecond)
	if err != nil {
		fmt.Printf("FAIL (%s)\n      └─ %v\n", elapsed, err)
		return 1
	}
	fmt.Printf("PASS (%s)\n", elapsed)
	return 0
}
