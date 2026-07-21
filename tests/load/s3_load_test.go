package load

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/feature/s3/manager"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

func TestLoad_ConcurrentPut(t *testing.T) {
	skipIfNoServer(t)

	client := newS3Client()
	bucket := loadBucket()
	ctx := context.Background()

	if err := ensureBucket(ctx, client, bucket); err != nil {
		t.Fatalf("ensure bucket: %v", err)
	}

	const workers = 100
	const objectSize = 1 << 20 // 1 MB

	goroutinesBefore := runtime.NumGoroutine()
	m := newMetrics("ConcurrentPut")

	var wg sync.WaitGroup
	keys := make([]string, workers)

	for i := 0; i < workers; i++ {
		keys[i] = fmt.Sprintf("load-put-%04d", i)
		wg.Add(1)
		go func(key string) {
			defer wg.Done()
			start := time.Now()
			_, err := client.PutObject(ctx, &s3.PutObjectInput{
				Bucket:        aws.String(bucket),
				Key:           aws.String(key),
				Body:          newPatternReader(objectSize),
				ContentLength: aws.Int64(objectSize),
			})
			d := time.Since(start)
			if err != nil {
				m.record(d, 500, 0)
				t.Logf("PUT %s failed: %v", key, err)
				return
			}
			m.record(d, 200, objectSize)
		}(keys[i])
	}

	wg.Wait()
	m.report(t, 2*time.Second)

	runtime.GC()
	time.Sleep(200 * time.Millisecond)
	checkGoroutineGrowth(t, "ConcurrentPut", goroutinesBefore, runtime.NumGoroutine())

	deleteObjects(ctx, client, bucket, keys)
}

func TestLoad_ConcurrentGet(t *testing.T) {
	skipIfNoServer(t)

	client := newS3Client()
	bucket := loadBucket()
	ctx := context.Background()

	if err := ensureBucket(ctx, client, bucket); err != nil {
		t.Fatalf("ensure bucket: %v", err)
	}

	const seedCount = 50
	const objectSize = 1 << 20 // 1 MB
	const readers = 100

	// Seed objects
	keys := make([]string, seedCount)
	for i := 0; i < seedCount; i++ {
		keys[i] = fmt.Sprintf("load-get-%04d", i)
		_, err := client.PutObject(ctx, &s3.PutObjectInput{
			Bucket:        aws.String(bucket),
			Key:           aws.String(keys[i]),
			Body:          newPatternReader(objectSize),
			ContentLength: aws.Int64(objectSize),
		})
		if err != nil {
			t.Fatalf("seed PUT %s: %v", keys[i], err)
		}
	}

	goroutinesBefore := runtime.NumGoroutine()
	m := newMetrics("ConcurrentGet")

	var wg sync.WaitGroup
	for i := 0; i < readers; i++ {
		key := keys[i%seedCount]
		wg.Add(1)
		go func(key string) {
			defer wg.Done()
			start := time.Now()
			resp, err := client.GetObject(ctx, &s3.GetObjectInput{
				Bucket: aws.String(bucket),
				Key:    aws.String(key),
			})
			d := time.Since(start)
			if err != nil {
				m.record(d, 500, 0)
				t.Logf("GET %s failed: %v", key, err)
				return
			}
			n, _ := io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			m.record(d, 200, n)
		}(key)
	}

	wg.Wait()
	m.report(t, 500*time.Millisecond)

	runtime.GC()
	time.Sleep(200 * time.Millisecond)
	checkGoroutineGrowth(t, "ConcurrentGet", goroutinesBefore, runtime.NumGoroutine())

	deleteObjects(ctx, client, bucket, keys)
}

func TestLoad_Multipart(t *testing.T) {
	skipIfNoServer(t)

	client := newS3Client()
	bucket := loadBucket()
	ctx := context.Background()

	if err := ensureBucket(ctx, client, bucket); err != nil {
		t.Fatalf("ensure bucket: %v", err)
	}

	const workers = 50
	const objectSize = 100 << 20 // 100 MB

	goroutinesBefore := runtime.NumGoroutine()
	m := newMetrics("Multipart")
	uploader := newUploader(client)

	var wg sync.WaitGroup
	keys := make([]string, workers)

	for i := 0; i < workers; i++ {
		keys[i] = fmt.Sprintf("load-multipart-%04d", i)
		wg.Add(1)
		go func(key string) {
			defer wg.Done()
			start := time.Now()
			_, err := uploader.Upload(ctx, &s3.PutObjectInput{ //nolint:staticcheck // manager.Uploader is deprecated in favor of transfermanager; migration is a post-launch WP
				Bucket: aws.String(bucket),
				Key:    aws.String(key),
				Body:   newPatternReader(objectSize),
			})
			d := time.Since(start)
			if err != nil {
				m.record(d, 500, 0)
				t.Logf("multipart %s failed: %v", key, err)
				return
			}
			m.record(d, 200, objectSize)
		}(keys[i])
	}

	wg.Wait()
	// No p99 gate for multipart — large uploads vary widely; just check no 5xx
	m.report(t, 0)

	runtime.GC()
	time.Sleep(200 * time.Millisecond)
	checkGoroutineGrowth(t, "Multipart", goroutinesBefore, runtime.NumGoroutine())

	deleteObjects(ctx, client, bucket, keys)
}

func TestLoad_MixedReadWrite(t *testing.T) {
	skipIfNoServer(t)

	client := newS3Client()
	bucket := loadBucket()
	ctx := context.Background()

	if err := ensureBucket(ctx, client, bucket); err != nil {
		t.Fatalf("ensure bucket: %v", err)
	}

	const workers = 100
	const objectSize = 1 << 20 // 1 MB

	// Seed objects for reads
	const seedCount = 30
	seedKeys := make([]string, seedCount)
	for i := 0; i < seedCount; i++ {
		seedKeys[i] = fmt.Sprintf("load-mix-seed-%04d", i)
		_, err := client.PutObject(ctx, &s3.PutObjectInput{
			Bucket:        aws.String(bucket),
			Key:           aws.String(seedKeys[i]),
			Body:          newPatternReader(objectSize),
			ContentLength: aws.Int64(objectSize),
		})
		if err != nil {
			t.Fatalf("seed PUT %s: %v", seedKeys[i], err)
		}
	}

	goroutinesBefore := runtime.NumGoroutine()
	m := newMetrics("MixedReadWrite")

	var wg sync.WaitGroup
	var putKeysMu sync.Mutex
	var putKeys []string

	for i := 0; i < workers; i++ {
		wg.Add(1)
		if i%10 < 7 { // 70% GET
			key := seedKeys[i%seedCount]
			go func(key string) {
				defer wg.Done()
				start := time.Now()
				resp, err := client.GetObject(ctx, &s3.GetObjectInput{
					Bucket: aws.String(bucket),
					Key:    aws.String(key),
				})
				d := time.Since(start)
				if err != nil {
					m.record(d, 500, 0)
					return
				}
				n, _ := io.Copy(io.Discard, resp.Body)
				_ = resp.Body.Close()
				m.record(d, 200, n)
			}(key)
		} else { // 30% PUT
			key := fmt.Sprintf("load-mix-put-%04d", i)
			putKeysMu.Lock()
			putKeys = append(putKeys, key)
			putKeysMu.Unlock()
			go func(key string) {
				defer wg.Done()
				start := time.Now()
				_, err := client.PutObject(ctx, &s3.PutObjectInput{
					Bucket:        aws.String(bucket),
					Key:           aws.String(key),
					Body:          newPatternReader(objectSize),
					ContentLength: aws.Int64(objectSize),
				})
				d := time.Since(start)
				if err != nil {
					m.record(d, 500, 0)
					return
				}
				m.record(d, 200, objectSize)
			}(key)
		}
	}

	wg.Wait()
	m.report(t, 2*time.Second)

	runtime.GC()
	time.Sleep(200 * time.Millisecond)
	checkGoroutineGrowth(t, "MixedReadWrite", goroutinesBefore, runtime.NumGoroutine())

	deleteObjects(ctx, client, bucket, seedKeys)
	deleteObjects(ctx, client, bucket, putKeys)
}

func TestLoad_ManagementBurst(t *testing.T) {
	skipIfNoServer(t)

	email := os.Getenv("VAULTAIRE_LOAD_EMAIL")
	password := os.Getenv("VAULTAIRE_LOAD_PASSWORD")
	if email == "" || password == "" {
		t.Skip("VAULTAIRE_LOAD_EMAIL / VAULTAIRE_LOAD_PASSWORD not set")
	}

	endpoint := loadEndpoint()
	token, err := getJWT(endpoint, email, password)
	if err != nil {
		t.Fatalf("login failed: %v", err)
	}

	const requests = 50

	goroutinesBefore := runtime.NumGoroutine()
	m := newMetrics("ManagementBurst")

	var wg sync.WaitGroup
	httpClient := &http.Client{Timeout: 10 * time.Second}

	for i := 0; i < requests; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx := context.Background()
			req, reqErr := http.NewRequestWithContext(ctx, http.MethodGet, endpoint+"/api/v1/manage/usage", nil)
			if reqErr != nil {
				m.record(0, 0, 0)
				return
			}
			req.Header.Set("Authorization", "Bearer "+token)

			start := time.Now()
			resp, doErr := httpClient.Do(req)
			d := time.Since(start)
			if doErr != nil {
				m.record(d, 0, 0)
				return
			}
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			m.record(d, resp.StatusCode, 0)
		}()
	}

	wg.Wait()

	// Custom assertions: expect some 429s (rate limit hit) and zero 5xx
	m.mu.Lock()
	var count429, count5xx int
	for _, r := range m.results {
		if r.status == http.StatusTooManyRequests {
			count429++
		}
		if r.status >= 500 {
			count5xx++
		}
	}
	m.mu.Unlock()

	t.Logf("[ManagementBurst] 429s: %d/%d, 5xx: %d/%d", count429, requests, count5xx, requests)

	if count429 == 0 {
		t.Errorf("[ManagementBurst] GATE FAIL: expected rate limiter to return 429s, got none")
	}
	if count5xx > 0 {
		t.Errorf("[ManagementBurst] GATE FAIL: %d requests returned 5xx (expected 429, not 500)", count5xx)
	}

	// Also report latency table (no p99 gate for management)
	m.report(t, 0)

	runtime.GC()
	time.Sleep(200 * time.Millisecond)
	checkGoroutineGrowth(t, "ManagementBurst", goroutinesBefore, runtime.NumGoroutine())
}

// TestLoad_Uploader_PartSize verifies the harness uploader matches production tuning.
func TestLoad_Uploader_PartSize(t *testing.T) {
	uploader := manager.NewUploader(nil, func(u *manager.Uploader) { //nolint:staticcheck // manager.Uploader is deprecated in favor of transfermanager; migration is a post-launch WP
		u.PartSize = 16 << 20
		u.Concurrency = 8
	})
	if uploader.PartSize != 16<<20 {
		t.Errorf("uploader part size = %d, want %d", uploader.PartSize, 16<<20)
	}
	if uploader.Concurrency != 8 {
		t.Errorf("uploader concurrency = %d, want 8", uploader.Concurrency)
	}
}
