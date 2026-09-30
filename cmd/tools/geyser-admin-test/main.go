package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/FairForge/vaultaire/internal/drivers"
	"go.uber.org/zap"
)

func main() {
	accessToken := os.Getenv("GEYSER_ACCESS_TOKEN")
	userID := os.Getenv("GEYSER_USER_ID")
	bucketID := "632df558-9627-427b-ab86-9f3ff1eaafe9"

	if accessToken == "" || userID == "" {
		log.Fatal("Set GEYSER_ACCESS_TOKEN and GEYSER_USER_ID")
	}

	logger, _ := zap.NewDevelopment()
	cfg := drivers.GeyserProvisioningConfig{
		DatacenterID:     os.Getenv("GEYSER_DATACENTER_ID"),
		CustomerID:       os.Getenv("GEYSER_CUSTOMER_ID"),
		TapeCollectionID: os.Getenv("GEYSER_TAPE_COLLECTION_ID"),
	}
	client := drivers.NewGeyserAdminClient(accessToken, userID, cfg, logger)

	ctx := context.Background()

	// Step 1: Start keepalive and wait for initial ping
	fmt.Println("→ Starting keepalive...")
	client.StartKeepalive(ctx)
	time.Sleep(500 * time.Millisecond)

	// Step 2: Get bucket status
	fmt.Println("→ Getting bucket status...")
	status, err := client.GetBucketStatus(ctx, bucketID)
	if err != nil {
		log.Fatalf("get status failed: %v", err)
	}
	fmt.Printf("✅ Bucket: %s | Status: %s | Size: %dGB | LogicalSize: %dMB\n",
		status.Name, status.Status, status.Size, status.LogicalSize/1024/1024)

	// Step 3: Check airgap state
	airgapped, err := client.IsAirgapped(ctx, bucketID)
	if err != nil {
		log.Fatalf("is airgapped check failed: %v", err)
	}
	fmt.Printf("✅ IsAirgapped: %v\n", airgapped)

	fmt.Println("\n✅ All checks passed — GeyserAdminClient is working")
	fmt.Println("   Token is valid and session is being maintained.")
}
