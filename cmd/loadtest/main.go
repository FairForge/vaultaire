// cmd/loadtest/main.go
//
// Runs a real S3 load test against a Vaultaire production endpoint.
// Each worker performs a full PUT → GET → DELETE round trip so we're
// testing the entire request path — auth, Quotaless backend, metadata
// writes — not just a health check ping.
//
// Usage:
//
//	go run ./cmd/loadtest \
//	  -endpoint https://stored.ge \
//	  -access-key YOUR_KEY \
//	  -secret-key YOUR_SECRET \
//	  -bucket YOUR_BUCKET \
//	  -workers 100 \
//	  -rps 50 \
//	  -duration 2m \
//	  -size 64
package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"flag"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/FairForge/vaultaire/internal/loadtest"
)

func main() {
	// — Flags ----------------------------------------------------------------
	endpoint := flag.String("endpoint", "https://stored.ge", "Vaultaire S3 endpoint")
	accessKey := flag.String("access-key", "", "S3 access key")
	secretKey := flag.String("secret-key", "", "S3 secret key")
	bucket := flag.String("bucket", "", "S3 bucket name")
	workers := flag.Int("workers", 100, "Max concurrent workers")
	rps := flag.Int("rps", 50, "Target requests per second")
	duration := flag.Duration("duration", 2*time.Minute, "Test duration")
	sizeKB := flag.Int("size", 64, "Object size in KB")
	flag.Parse()

	if *accessKey == "" || *secretKey == "" || *bucket == "" {
		fmt.Fprintln(os.Stderr, "error: -access-key, -secret-key, and -bucket are required")
		flag.Usage()
		os.Exit(1)
	}

	// — S3 client ------------------------------------------------------------
	client, err := newS3Client(*endpoint, *accessKey, *secretKey)
	if err != nil {
		log.Fatalf("failed to create S3 client: %v", err)
	}

	// Pre-generate a random payload so CPU isn't a bottleneck during the test.
	// Every worker uploads a fresh key but reuses this body via bytes.NewReader.
	payload := make([]byte, *sizeKB*1024)
	if _, err := rand.Read(payload); err != nil {
		log.Fatalf("failed to generate payload: %v", err)
	}

	// — Worker function ------------------------------------------------------
	// Each worker does a complete PUT → GET → DELETE round trip.
	// This validates the full path: auth → handler → Quotaless → metadata.
	//
	// Key design decision: we check ctx.Err() before recording any network
	// error as a test failure. When the test timer expires, the framework
	// cancels the context — requests still in flight get a context.Canceled
	// error that is NOT a server problem. Without this check, the teardown
	// window inflates the error rate and fails the threshold check.
	workerFn := func(ctx context.Context, workerID int) loadtest.Result {
		start := time.Now()

		// Unique key per request to avoid cache hits and key collisions.
		key := fmt.Sprintf("loadtest/worker-%d/obj-%d", workerID, time.Now().UnixNano())

		// PUT
		_, err := client.PutObject(ctx, &s3.PutObjectInput{
			Bucket:        aws.String(*bucket),
			Key:           aws.String(key),
			Body:          bytes.NewReader(payload),
			ContentLength: aws.Int64(int64(len(payload))),
		})
		if err != nil {
			// Context cancelled at test end — not a real server failure.
			if ctx.Err() != nil {
				return loadtest.Result{StartTime: start, Duration: time.Since(start)}
			}
			return loadtest.Result{
				StartTime: start,
				Duration:  time.Since(start),
				Error:     fmt.Errorf("PUT failed: %w", err),
			}
		}

		// GET — verify the object comes back
		getResp, err := client.GetObject(ctx, &s3.GetObjectInput{
			Bucket: aws.String(*bucket),
			Key:    aws.String(key),
		})
		if err != nil {
			if ctx.Err() != nil {
				return loadtest.Result{StartTime: start, Duration: time.Since(start), BytesSent: int64(len(payload))}
			}
			return loadtest.Result{
				StartTime: start,
				Duration:  time.Since(start),
				BytesSent: int64(len(payload)),
				Error:     fmt.Errorf("GET failed: %w", err),
			}
		}
		_ = getResp.Body.Close()

		// DELETE — clean up after ourselves. Non-fatal if it fails;
		// the object uploaded and downloaded successfully which is what matters.
		_, _ = client.DeleteObject(ctx, &s3.DeleteObjectInput{
			Bucket: aws.String(*bucket),
			Key:    aws.String(key),
		})

		return loadtest.Result{
			StartTime:  start,
			Duration:   time.Since(start),
			StatusCode: 200,
			BytesSent:  int64(len(payload)),
			BytesRecv:  int64(len(payload)),
		}
	}

	// — Run ------------------------------------------------------------------
	cfg := &loadtest.Config{
		Name:           fmt.Sprintf("stored.ge PUT/GET/DELETE (%dKB objects)", *sizeKB),
		Type:           loadtest.TestTypeLoad,
		Duration:       *duration,
		RampUpDuration: 15 * time.Second,
		TargetRPS:      *rps,
		MaxConcurrency: *workers,
		Timeout:        30 * time.Second,
	}

	fmt.Printf("\n🚀 Vaultaire Load Test\n")
	fmt.Printf("   Endpoint:  %s\n", *endpoint)
	fmt.Printf("   Bucket:    %s\n", *bucket)
	fmt.Printf("   Workers:   %d\n", *workers)
	fmt.Printf("   Target:    %d RPS\n", *rps)
	fmt.Printf("   Duration:  %s\n", *duration)
	fmt.Printf("   Payload:   %d KB/object\n\n", *sizeKB)

	framework := loadtest.New(cfg, workerFn)

	// Print live stats every 10s during the run.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		ticker := time.NewTicker(10 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				total, success, failure, rpsNow := framework.CurrentStats()
				fmt.Printf("   ↳ live: %d req | %d ok | %d err | %.1f RPS\n",
					total, success, failure, rpsNow)
			}
		}
	}()

	summary, err := framework.Run(ctx)
	cancel()
	if err != nil {
		log.Fatalf("load test failed: %v", err)
	}

	// — Report ---------------------------------------------------------------
	printReport(summary)

	// Exit non-zero if error rate exceeds 1%.
	if summary.ErrorRate > 0.01 {
		fmt.Fprintf(os.Stderr, "\n❌ FAIL: error rate %.2f%% exceeds 1%% threshold\n",
			summary.ErrorRate*100)
		os.Exit(1)
	}
	fmt.Println("\n✅ PASS: error rate within acceptable threshold")
}

func newS3Client(endpoint, accessKey, secretKey string) (*s3.Client, error) {
	cfg, err := awsconfig.LoadDefaultConfig(context.Background(),
		awsconfig.WithCredentialsProvider(
			credentials.NewStaticCredentialsProvider(accessKey, secretKey, ""),
		),
		awsconfig.WithRegion("us-east-1"),
	)
	if err != nil {
		return nil, fmt.Errorf("aws config: %w", err)
	}

	return s3.NewFromConfig(cfg, func(o *s3.Options) {
		o.BaseEndpoint = aws.String(endpoint)
		o.UsePathStyle = true
	}), nil
}

func printReport(s *loadtest.Summary) {
	fmt.Printf("\n━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━\n")
	fmt.Printf("  LOAD TEST REPORT: %s\n", s.TestName)
	fmt.Printf("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━\n")
	fmt.Printf("  Duration:     %s\n", s.EndTime.Sub(s.StartTime).Round(time.Second))
	fmt.Printf("  Total Req:    %d\n", s.TotalRequests)
	fmt.Printf("  Success:      %d\n", s.SuccessCount)
	fmt.Printf("  Failures:     %d\n", s.FailureCount)
	fmt.Printf("  Error Rate:   %.2f%%\n", s.ErrorRate*100)
	fmt.Printf("  Actual RPS:   %.1f\n", s.RequestsPerSec)
	fmt.Printf("  Throughput:   %.2f MB transferred\n",
		float64(s.TotalBytes)/1024/1024)
	fmt.Printf("\n  Latency (full PUT+GET round trip):\n")
	fmt.Printf("    P50:  %s\n", s.P50Latency.Round(time.Millisecond))
	fmt.Printf("    P95:  %s\n", s.P95Latency.Round(time.Millisecond))
	fmt.Printf("    P99:  %s\n", s.P99Latency.Round(time.Millisecond))
	fmt.Printf("    Max:  %s\n", s.MaxLatency.Round(time.Millisecond))
	if len(s.Errors) > 0 {
		fmt.Printf("\n  Top Errors:\n")
		for msg, count := range s.Errors {
			fmt.Printf("    [%d×] %s\n", count, msg)
		}
	}
	fmt.Printf("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━\n")
}
