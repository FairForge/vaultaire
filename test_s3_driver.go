package main

import (
	"fmt"
	"github.com/FairForge/vaultaire/internal/drivers"
	"go.uber.org/zap"
)

func main() {
	logger, _ := zap.NewDevelopment()

	accessKey := "test"
	secretKey := "test"

	driver, err := drivers.NewS3CompatDriver(accessKey, secretKey, logger)
	if err != nil {
		fmt.Printf("S3 driver failed: %v\n", err)
	} else {
		fmt.Printf("S3 driver created successfully: %+v\n", driver)
	}
}
