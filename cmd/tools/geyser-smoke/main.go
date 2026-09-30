package main

import (
	"context"
	"fmt"
	"os"

	"github.com/FairForge/vaultaire/internal/drivers"
	"go.uber.org/zap"
)

func main() {
	token := os.Getenv("GEYSER_ACCESS_TOKEN")
	userID := os.Getenv("GEYSER_USER_ID")
	if token == "" || userID == "" {
		fmt.Fprintln(os.Stderr, "set GEYSER_ACCESS_TOKEN and GEYSER_USER_ID")
		os.Exit(1)
	}

	logger, _ := zap.NewDevelopment()
	cfg := drivers.GeyserProvisioningConfig{
		DatacenterID:     os.Getenv("GEYSER_DATACENTER_ID"),
		CustomerID:       os.Getenv("GEYSER_CUSTOMER_ID"),
		TapeCollectionID: os.Getenv("GEYSER_TAPE_COLLECTION_ID"),
	}

	client := drivers.NewGeyserAdminClient(token, userID, cfg, logger)
	ctx := context.Background()

	client.StartKeepalive(ctx)
	defer client.StopKeepalive()

	fmt.Println("--- Getting status of Stored3Lib ---")
	status, err := client.GetBucketStatus(ctx, "632df558-9627-427b-ab86-9f3ff1eaafe9")
	if err != nil {
		fmt.Printf("FAIL GetBucketStatus: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("OK  bucket=%s status=%s logicalSize=%d\n",
		status.BucketName, status.Status, status.LogicalSize)

	fmt.Println("--- Getting invoices ---")
	invoices, err := client.GetInvoices(ctx)
	if err != nil {
		fmt.Printf("FAIL GetInvoices: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("OK  invoices=%d\n", len(invoices))
	for _, inv := range invoices {
		tbCount := 0.0
		if len(inv.TapeCollectionInvoices) > 0 {
			tbCount = inv.TapeCollectionInvoices[0].TBCount
		}
		fmt.Printf("    %d/%d isInvoice=%-5v tbUsed=%.1f total=$%.2f\n",
			inv.Month+1, inv.Year, inv.IsInvoice, tbCount, inv.Total)
	}

	fmt.Println("--- Smoke test passed ---")
}
