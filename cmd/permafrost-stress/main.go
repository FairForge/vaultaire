package main

import (
	"context"
	"crypto/rand"
	"fmt"
	"io"
	"log"
	"os"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	msgraphsdk "github.com/microsoftgraph/msgraph-sdk-go"
	"github.com/microsoftgraph/msgraph-sdk-go/models"
)

// tenant holds credentials for one OneDrive tenant.
type tenant struct {
	name     string
	tenantID string
	clientID string
	secret   string
	userUPN  string
}

// clientEntry holds a live Graph client and drive context for one tenant.
// Defined once at package level — used by all test functions.
type clientEntry struct {
	t       tenant
	client  *msgraphsdk.GraphServiceClient
	driveID string
	folder  string
}

// latencySample records a single operation's outcome.
type latencySample struct {
	duration time.Duration
	err      bool
}

func main() {
	tenants := loadTenants()
	if len(tenants) == 0 {
		log.Fatal("No tenants found. Set TENANT_1_ID, TENANT_1_CLIENT_ID, TENANT_1_SECRET, TENANT_1_USER")
	}
	log.Printf("Loaded %d tenant(s)\n\n", len(tenants))

	// Build one authenticated client per tenant, reused across all tests.
	var clients []clientEntry
	for _, t := range tenants {
		c, err := newGraphClient(t)
		if err != nil {
			log.Fatalf("[%s] auth failed: %v", t.name, err)
		}
		driveID, err := getDriveID(context.Background(), c, t)
		if err != nil {
			log.Fatalf("[%s] drive lookup failed: %v", t.name, err)
		}
		folderID, err := createFolder(context.Background(), c, driveID,
			fmt.Sprintf("stress-%d", time.Now().UnixNano()))
		if err != nil {
			log.Fatalf("[%s] folder creation failed: %v", t.name, err)
		}
		log.Printf("[%s] Ready (drive=%s..., folder=%s...)", t.name, driveID[:16], folderID[:16])
		clients = append(clients, clientEntry{t, c, driveID, folderID})
	}
	fmt.Println()

	runFileSizeScaling(clients)
	runWorkerScaling(clients)
	runDownloadTest(clients)
	runMixedWorkload(clients)
	runThrottleStress(clients)

	// Async cleanup — don't block on Microsoft's delete latency.
	fmt.Println("Cleaning up test folders...")
	for _, ce := range clients {
		go func(ce clientEntry) {
			_ = ce.client.Drives().ByDriveId(ce.driveID).Items().
				ByDriveItemId(ce.folder).Delete(context.Background(), nil)
		}(ce)
	}
	time.Sleep(2 * time.Second)
	fmt.Println("Done.")
}

// ── Test 1: File Size Scaling ─────────────────────────────────────────────────

func runFileSizeScaling(clients []clientEntry) {
	sizes := []int{1, 4, 10, 50, 100}
	filesPerSize := 5

	printHeader("TEST 1: File Size Scaling (throughput vs file size, 5 files each)")
	fmt.Printf("  %-10s  %-14s  %-14s  %-8s\n", "Size", "Avg MB/s", "Fleet MB/s", "Errors")
	fmt.Println("  ──────────────────────────────────────────────────")

	for _, sizeMB := range sizes {
		data := randomData(sizeMB * 1024 * 1024)
		var totalMBs float64
		var totalErrors int

		for _, ce := range clients {
			mbs, errors := uploadNFiles(context.Background(), ce.client, ce.driveID,
				ce.folder, fmt.Sprintf("size-%dmb", sizeMB), data, filesPerSize, 5)
			totalMBs += mbs
			totalErrors += errors
		}

		avgMBs := totalMBs / float64(len(clients))
		fmt.Printf("  %-10s  %-14.2f  %-14.2f  %-8d\n",
			fmt.Sprintf("%d MB", sizeMB), avgMBs, totalMBs, totalErrors)
	}
	fmt.Println()
}

// ── Test 2: Worker Count Scaling ──────────────────────────────────────────────

func runWorkerScaling(clients []clientEntry) {
	workerCounts := []int{10, 25, 50, 75, 100}
	fileCount := 50
	data := randomData(1 * 1024 * 1024)

	printHeader("TEST 2: Worker Count Scaling (1MB files, 50 uploads per tenant)")
	fmt.Printf("  %-10s  %-14s  %-14s  %-8s\n", "Workers", "Avg MB/s", "Fleet MB/s", "Errors")
	fmt.Println("  ──────────────────────────────────────────────────")

	for _, w := range workerCounts {
		var totalMBs float64
		var totalErrors int
		var mu sync.Mutex
		var wg sync.WaitGroup

		for _, ce := range clients {
			wg.Add(1)
			go func(ce clientEntry) {
				defer wg.Done()
				mbs, errors := uploadNFiles(context.Background(), ce.client, ce.driveID,
					ce.folder, fmt.Sprintf("workers-%d", w), data, fileCount, w)
				mu.Lock()
				totalMBs += mbs
				totalErrors += errors
				mu.Unlock()
			}(ce)
		}
		wg.Wait()

		avgMBs := totalMBs / float64(len(clients))
		fmt.Printf("  %-10d  %-14.2f  %-14.2f  %-8d\n", w, avgMBs, totalMBs, totalErrors)
	}
	fmt.Println()
}

// ── Test 3: Download Speed ────────────────────────────────────────────────────

func runDownloadTest(clients []clientEntry) {
	printHeader("TEST 3: Download Speed (restore performance)")
	fmt.Printf("  %-20s  %-10s  %-12s  %-8s\n", "Tenant", "Size", "MB/s", "Status")
	fmt.Println("  ──────────────────────────────────────────────────")

	sizes := []int{1, 10, 50}

	for _, ce := range clients {
		for _, sizeMB := range sizes {
			data := randomData(sizeMB * 1024 * 1024)
			fileName := fmt.Sprintf("dl-test-%dmb.bin", sizeMB)

			if err := uploadSingleCall(context.Background(), ce.client, ce.driveID,
				ce.folder, fileName, data); err != nil {
				fmt.Printf("  %-20s  %-10s  %-12s  ❌ upload failed\n",
					ce.t.name, fmt.Sprintf("%d MB", sizeMB), "-")
				continue
			}

			start := time.Now()
			n, err := downloadFile(context.Background(), ce.client, ce.driveID,
				ce.folder, fileName)
			duration := time.Since(start)

			if err != nil {
				fmt.Printf("  %-20s  %-10s  %-12s  ❌ %v\n",
					ce.t.name, fmt.Sprintf("%d MB", sizeMB), "-", err)
				continue
			}

			mbs := float64(n) / (1024 * 1024) / duration.Seconds()
			fmt.Printf("  %-20s  %-10s  %-12.2f  ✅\n",
				ce.t.name, fmt.Sprintf("%d MB", sizeMB), mbs)
		}
	}
	fmt.Println()
}

// ── Test 4: Mixed Workload ────────────────────────────────────────────────────

func runMixedWorkload(clients []clientEntry) {
	printHeader("TEST 4: Mixed Workload (simultaneous upload + download, all tenants)")

	data := randomData(5 * 1024 * 1024)

	// Pre-upload files on tenant-1 for download during the mixed test.
	ce := clients[0]
	var seedFiles []string
	for i := 0; i < 10; i++ {
		name := fmt.Sprintf("mixed-seed-%04d.bin", i)
		if err := uploadSingleCall(context.Background(), ce.client, ce.driveID,
			ce.folder, name, data); err == nil {
			seedFiles = append(seedFiles, name)
		}
	}
	fmt.Printf("  Seeded %d files for download. Starting mixed workload...\n", len(seedFiles))

	var uploadMBs float64
	var downloadBytes int64
	var uploadErrors, downloadErrors atomic.Int64
	var wg sync.WaitGroup

	start := time.Now()

	// Uploads across all tenants concurrently.
	var uploadMu sync.Mutex
	for _, ce := range clients {
		wg.Add(1)
		go func(ce clientEntry) {
			defer wg.Done()
			mbs, errs := uploadNFiles(context.Background(), ce.client, ce.driveID,
				ce.folder, "mixed-up", data, 20, 10)
			uploadMu.Lock()
			uploadMBs += mbs
			uploadMu.Unlock()
			uploadErrors.Add(int64(errs))
		}(ce)
	}

	// Downloads from tenant-1 simultaneously.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for _, name := range seedFiles {
			n, err := downloadFile(context.Background(), ce.client, ce.driveID, ce.folder, name)
			if err != nil {
				downloadErrors.Add(1)
			} else {
				downloadBytes += n
			}
		}
	}()

	wg.Wait()
	duration := time.Since(start)
	downloadMBs := float64(downloadBytes) / (1024 * 1024) / duration.Seconds()

	fmt.Printf("  Duration:       %s\n", duration.Round(time.Millisecond))
	fmt.Printf("  Upload:         %.2f MB/s (%d errors)\n", uploadMBs, uploadErrors.Load())
	fmt.Printf("  Download:       %.2f MB/s (%d errors)\n", downloadMBs, downloadErrors.Load())
	fmt.Printf("  Aggregate:      %.2f MB/s\n", uploadMBs+downloadMBs)
	fmt.Println()
}

// ── Test 5: Throttle Stress ───────────────────────────────────────────────────

func runThrottleStress(clients []clientEntry) {
	printHeader("TEST 5: Throttle Stress (100 workers, 200 files, 512KB each)")
	fmt.Println("  Watching for 429 throttle errors on tenant-1...")
	fmt.Println()

	data := randomData(512 * 1024)
	var samples []latencySample
	var mu sync.Mutex
	var throttled atomic.Int64

	ce := clients[0]
	sem := make(chan struct{}, 100)
	var wg sync.WaitGroup

	start := time.Now()
	for i := 0; i < 200; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			opStart := time.Now()
			err := uploadSingleCall(context.Background(), ce.client, ce.driveID,
				ce.folder, fmt.Sprintf("stress-%04d.bin", n), data)
			d := time.Since(opStart)

			s := latencySample{duration: d}
			if err != nil {
				s.err = true
				throttled.Add(1)
			}
			mu.Lock()
			samples = append(samples, s)
			mu.Unlock()
		}(i)
	}
	wg.Wait()
	totalDuration := time.Since(start)

	var durations []time.Duration
	for _, s := range samples {
		if !s.err {
			durations = append(durations, s.duration)
		}
	}
	sort.Slice(durations, func(i, j int) bool { return durations[i] < durations[j] })

	successCount := len(durations)
	fmt.Printf("  Duration:    %s\n", totalDuration.Round(time.Millisecond))
	fmt.Printf("  Successful:  %d / 200\n", successCount)
	fmt.Printf("  Throttled:   %d (429 errors)\n", throttled.Load())
	if successCount > 0 {
		fmt.Printf("  p50 latency: %s\n", durations[successCount*50/100].Round(time.Millisecond))
		fmt.Printf("  p95 latency: %s\n", durations[successCount*95/100].Round(time.Millisecond))
		fmt.Printf("  p99 latency: %s\n", durations[successCount*99/100].Round(time.Millisecond))
	}
	fmt.Printf("  Throughput:  %.2f MB/s\n", float64(successCount)*0.5/totalDuration.Seconds())
	fmt.Printf("  Est. RU/min: ~%d / 1250\n", successCount*2)
	if throttled.Load() == 0 {
		fmt.Println("  ✅ No throttling detected")
	} else {
		fmt.Println("  ⚠️  Throttling detected — add exponential backoff before production")
	}
	fmt.Println()
}

// ── Helpers ───────────────────────────────────────────────────────────────────

func uploadNFiles(ctx context.Context, client *msgraphsdk.GraphServiceClient,
	driveID, folderID, prefix string, data []byte, count, workers int) (float64, int) {

	var errCount atomic.Int64
	sem := make(chan struct{}, workers)
	var wg sync.WaitGroup

	start := time.Now()
	for i := 0; i < count; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			if err := uploadSingleCall(ctx, client, driveID, folderID,
				fmt.Sprintf("%s-%04d.bin", prefix, n), data); err != nil {
				errCount.Add(1)
			}
		}(i)
	}
	wg.Wait()
	duration := time.Since(start)

	errs := int(errCount.Load())
	successMB := float64((count-errs)*len(data)) / (1024 * 1024)
	return successMB / duration.Seconds(), errs
}

// uploadSingleCall uploads a file in one Graph API request using
// path-based addressing, halving latency vs the two-call create+put pattern.
func uploadSingleCall(ctx context.Context, client *msgraphsdk.GraphServiceClient,
	driveID, folderID, fileName string, data []byte) error {

	itemPath := fmt.Sprintf("%s:/%s:", folderID, fileName)
	_, err := client.Drives().ByDriveId(driveID).Items().
		ByDriveItemId(itemPath).Content().Put(ctx, data, nil)
	if err != nil {
		return fmt.Errorf("upload: %w", err)
	}
	return nil
}

// downloadFile downloads a file and returns bytes read.
// The Graph SDK returns []byte directly — no Close() needed.
func downloadFile(ctx context.Context, client *msgraphsdk.GraphServiceClient,
	driveID, folderID, fileName string) (int64, error) {

	itemPath := fmt.Sprintf("%s:/%s:", folderID, fileName)
	data, err := client.Drives().ByDriveId(driveID).Items().
		ByDriveItemId(itemPath).Content().Get(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("download: %w", err)
	}
	// Drain into discard to measure throughput accurately.
	n, err := io.Copy(io.Discard, bytesReader(data))
	return n, err
}

// bytesReader wraps []byte as an io.Reader for io.Copy.
func bytesReader(b []byte) io.Reader {
	return &byteSliceReader{data: b}
}

type byteSliceReader struct {
	data   []byte
	offset int
}

func (r *byteSliceReader) Read(p []byte) (int, error) {
	if r.offset >= len(r.data) {
		return 0, io.EOF
	}
	n := copy(p, r.data[r.offset:])
	r.offset += n
	return n, nil
}

func createFolder(ctx context.Context, client *msgraphsdk.GraphServiceClient,
	driveID, name string) (string, error) {

	folder := models.NewDriveItem()
	folder.SetName(&name)
	folderFacet := models.NewFolder()
	folder.SetFolder(folderFacet)

	item, err := client.Drives().ByDriveId(driveID).Items().
		ByDriveItemId("root").Children().Post(ctx, folder, nil)
	if err != nil {
		return "", fmt.Errorf("create folder: %w", err)
	}
	return *item.GetId(), nil
}

func getDriveID(ctx context.Context, client *msgraphsdk.GraphServiceClient,
	t tenant) (string, error) {

	drives, err := client.Users().ByUserId(t.userUPN).Drives().Get(ctx, nil)
	if err != nil {
		return "", err
	}
	if len(drives.GetValue()) == 0 {
		return "", fmt.Errorf("no drives for %s", t.userUPN)
	}
	return *drives.GetValue()[0].GetId(), nil
}

func newGraphClient(t tenant) (*msgraphsdk.GraphServiceClient, error) {
	cred, err := azidentity.NewClientSecretCredential(t.tenantID, t.clientID, t.secret, nil)
	if err != nil {
		return nil, err
	}
	return msgraphsdk.NewGraphServiceClientWithCredentials(
		cred, []string{"https://graph.microsoft.com/.default"})
}

func loadTenants() []tenant {
	var tenants []tenant
	for i := 1; i <= 15; i++ {
		p := fmt.Sprintf("TENANT_%d_", i)
		if os.Getenv(p+"ID") == "" {
			continue
		}
		tenants = append(tenants, tenant{
			name:     fmt.Sprintf("tenant-%d", i),
			tenantID: os.Getenv(p + "ID"),
			clientID: os.Getenv(p + "CLIENT_ID"),
			secret:   os.Getenv(p + "SECRET"),
			userUPN:  os.Getenv(p + "USER"),
		})
	}
	return tenants
}

func randomData(size int) []byte {
	data := make([]byte, size)
	if _, err := rand.Read(data); err != nil {
		log.Fatalf("rand.Read: %v", err)
	}
	return data
}

func printHeader(title string) {
	fmt.Println("═══════════════════════════════════════════════════════════════")
	fmt.Printf("  %s\n", title)
	fmt.Println("═══════════════════════════════════════════════════════════════")
}
