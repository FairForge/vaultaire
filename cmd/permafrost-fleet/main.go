package main

import (
	"context"
	"crypto/rand"
	"fmt"
	"log"
	"os"
	"sync"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	msgraphsdk "github.com/microsoftgraph/msgraph-sdk-go"
	"github.com/microsoftgraph/msgraph-sdk-go/models"
)

// tenant holds credentials and identity for a single OneDrive tenant.
type tenant struct {
	name     string
	tenantID string
	clientID string
	secret   string
	userUPN  string
}

// result holds benchmark output for one tenant.
type result struct {
	tenantName string
	workers    int
	fileCount  int
	fileSizeMB int
	duration   time.Duration
	errors     int
}

func (r result) throughputMBs() float64 {
	total := float64(r.fileCount-r.errors) * float64(r.fileSizeMB)
	return total / r.duration.Seconds()
}

func main() {
	// Load all tenants from environment.
	// Add more by following the TENANT_N_* pattern.
	tenants := loadTenants()
	if len(tenants) == 0 {
		log.Fatal("No tenants found. Set TENANT_1_ID, TENANT_1_CLIENT_ID, TENANT_1_SECRET, TENANT_1_USER (and TENANT_2_*, TENANT_3_* etc.)")
	}
	log.Printf("Loaded %d tenant(s): running fleet benchmark\n", len(tenants))

	// Test parameters
	const (
		workers    = 25  // concurrent uploads per tenant
		fileCount  = 100 // files per tenant
		fileSizeMB = 1   // MB per file
	)

	// Pre-generate random data once — reused across all uploads.
	// OneDrive doesn't deduplicate on our end so this is fine for benchmarking.
	data := make([]byte, fileSizeMB*1024*1024)
	if _, err := rand.Read(data); err != nil {
		log.Fatalf("Failed to generate random data: %v", err)
	}

	// Run all tenants concurrently and collect results.
	results := make([]result, len(tenants))
	var wg sync.WaitGroup

	fleetStart := time.Now()
	for i, t := range tenants {
		wg.Add(1)
		go func(idx int, tn tenant) {
			defer wg.Done()
			results[idx] = benchmarkTenant(tn, data, workers, fileCount, fileSizeMB)
		}(i, t)
	}
	wg.Wait()
	fleetDuration := time.Since(fleetStart)

	// Print results table.
	fmt.Println()
	fmt.Println("═══════════════════════════════════════════════════════════════")
	fmt.Println("  PERMAFROST FLEET BENCHMARK RESULTS")
	fmt.Println("═══════════════════════════════════════════════════════════════")
	fmt.Printf("  %-20s  %8s  %6s  %10s  %8s\n",
		"Tenant", "Workers", "Errors", "Duration", "MB/s")
	fmt.Println("  ─────────────────────────────────────────────────────────────")

	var totalThroughput float64
	for _, r := range results {
		fmt.Printf("  %-20s  %8d  %6d  %10s  %8.2f\n",
			r.tenantName,
			r.workers,
			r.errors,
			r.duration.Round(time.Millisecond),
			r.throughputMBs(),
		)
		totalThroughput += r.throughputMBs()
	}

	fmt.Println("  ─────────────────────────────────────────────────────────────")
	fmt.Printf("  %-20s  %8s  %6s  %10s  %8.2f\n",
		"FLEET AGGREGATE", "", "", fleetDuration.Round(time.Millisecond), totalThroughput)
	fmt.Println("═══════════════════════════════════════════════════════════════")
	fmt.Printf("\n  Fleet uploaded %d MB across %d tenants in %s\n",
		fileCount*fileSizeMB*len(tenants),
		len(tenants),
		fleetDuration.Round(time.Millisecond))
	fmt.Printf("  Resource units used per tenant: ~%d / 1250 per minute\n",
		fileCount*2) // ~2 RU per file
	fmt.Println()
}

// benchmarkTenant runs a parallel upload benchmark against a single tenant.
func benchmarkTenant(t tenant, data []byte, workers, fileCount, fileSizeMB int) result {
	ctx := context.Background()

	client, err := newGraphClient(t)
	if err != nil {
		log.Printf("[%s] Failed to create client: %v", t.name, err)
		return result{tenantName: t.name, errors: fileCount}
	}

	driveID, err := getDriveID(ctx, client, t)
	if err != nil {
		log.Printf("[%s] Failed to get drive: %v", t.name, err)
		return result{tenantName: t.name, errors: fileCount}
	}

	// Create an isolated test folder for this run.
	folderID, err := createFolder(ctx, client, driveID,
		fmt.Sprintf("permafrost-fleet-%d", time.Now().UnixNano()))
	if err != nil {
		log.Printf("[%s] Failed to create folder: %v", t.name, err)
		return result{tenantName: t.name, errors: fileCount}
	}

	// Upload files with bounded concurrency.
	var (
		mu       sync.Mutex
		errCount int
	)
	sem := make(chan struct{}, workers)
	var uploadWg sync.WaitGroup

	start := time.Now()
	for i := 0; i < fileCount; i++ {
		uploadWg.Add(1)
		go func(n int) {
			defer uploadWg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			fileName := fmt.Sprintf("file-%04d.bin", n)
			// Optimized: single API call using path-based addressing.
			// Standard approach does create-item + put-content (2 calls).
			// Path upload does both in one request, halving per-file latency.
			if err := uploadSingleCall(ctx, client, driveID, folderID, fileName, data); err != nil {
				mu.Lock()
				errCount++
				mu.Unlock()
				log.Printf("[%s] Upload failed for %s: %v", t.name, fileName, err)
			}
		}(i)
	}
	uploadWg.Wait()
	duration := time.Since(start)

	log.Printf("[%s] Done — %d files, %d errors, %.2f MB/s",
		t.name, fileCount, errCount, float64((fileCount-errCount)*fileSizeMB)/duration.Seconds())

	// Clean up asynchronously — don't block results.
	go func() {
		if err := client.Drives().ByDriveId(driveID).Items().
			ByDriveItemId(folderID).Delete(ctx, nil); err != nil {
			log.Printf("[%s] Warning: cleanup failed: %v", t.name, err)
		}
	}()

	return result{
		tenantName: t.name,
		workers:    workers,
		fileCount:  fileCount,
		fileSizeMB: fileSizeMB,
		duration:   duration,
		errors:     errCount,
	}
}

// uploadSingleCall uploads a file in a single Graph API request using
// path-based addressing: PUT /drives/{id}/items/{parentId}:/{fileName}:/content
// This replaces the two-call pattern (create item + put content) used in the
// original benchmarks, reducing per-file latency by ~50%.
func uploadSingleCall(ctx context.Context, client *msgraphsdk.GraphServiceClient,
	driveID, folderID, fileName string, data []byte) error {

	// Path-based item ID: "{parentId}:/{fileName}:"
	// Graph API interprets this as "the item named fileName inside parentId".
	itemPath := fmt.Sprintf("%s:/%s:", folderID, fileName)

	_, err := client.Drives().ByDriveId(driveID).Items().
		ByDriveItemId(itemPath).Content().Put(ctx, data, nil)
	if err != nil {
		return fmt.Errorf("path upload: %w", err)
	}
	return nil
}

// createFolder creates a new folder inside the drive root and returns its ID.
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

// getDriveID returns the primary drive ID for the tenant user.
func getDriveID(ctx context.Context, client *msgraphsdk.GraphServiceClient,
	t tenant) (string, error) {

	drives, err := client.Users().ByUserId(t.userUPN).Drives().Get(ctx, nil)
	if err != nil {
		return "", fmt.Errorf("list drives: %w", err)
	}
	if len(drives.GetValue()) == 0 {
		return "", fmt.Errorf("no drives found for %s", t.userUPN)
	}
	return *drives.GetValue()[0].GetId(), nil
}

// newGraphClient creates an authenticated Graph client for a tenant.
func newGraphClient(t tenant) (*msgraphsdk.GraphServiceClient, error) {
	cred, err := azidentity.NewClientSecretCredential(
		t.tenantID, t.clientID, t.secret, nil)
	if err != nil {
		return nil, fmt.Errorf("create credential: %w", err)
	}
	return msgraphsdk.NewGraphServiceClientWithCredentials(
		cred, []string{"https://graph.microsoft.com/.default"})
}

// loadTenants reads tenant credentials from environment variables.
// Supports up to 15 tenants following the TENANT_N_* naming convention.
func loadTenants() []tenant {
	var tenants []tenant
	for i := 1; i <= 15; i++ {
		prefix := fmt.Sprintf("TENANT_%d_", i)
		id := os.Getenv(prefix + "ID")
		if id == "" {
			continue // No more tenants defined
		}
		tenants = append(tenants, tenant{
			name:     fmt.Sprintf("tenant-%d", i),
			tenantID: id,
			clientID: os.Getenv(prefix + "CLIENT_ID"),
			secret:   os.Getenv(prefix + "SECRET"),
			userUPN:  os.Getenv(prefix + "USER"),
		})
	}
	return tenants
}
