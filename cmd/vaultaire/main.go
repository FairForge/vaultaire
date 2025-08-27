// cmd/vaultaire/main.go
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/FairForge/vaultaire/internal/api"
	"github.com/FairForge/vaultaire/internal/config"
	"github.com/FairForge/vaultaire/internal/drivers"
	"github.com/FairForge/vaultaire/internal/engine"
	"go.uber.org/zap"
)

func main() {
	// Create logger
	logger, _ := zap.NewProduction()
	defer func() { _ = logger.Sync() }()

	// Create config
	port := 8000
	if p := os.Getenv("PORT"); p != "" {
		if _, err := fmt.Sscanf(p, "%d", &port); err != nil {
			logger.Error("invalid port number", zap.String("port", p), zap.Error(err))
			port = 8000 // default fallback
		}
	}

	cfg := &config.Config{
		Server: config.ServerConfig{
			Port: port,
		},
	}

	// Create engine
	eng := engine.NewEngine(logger)

	// Add storage driver based on environment
	storageMode := os.Getenv("STORAGE_MODE")
	if storageMode == "" {
		storageMode = "local"
	}

	switch storageMode {
	case "local":
		// Local driver for development
		dataPath := os.Getenv("LOCAL_STORAGE_PATH")
		if dataPath == "" {
			dataPath = "/tmp/vaultaire-data"
		}
		if err := os.MkdirAll(dataPath, 0750); err != nil {
			logger.Fatal("failed to create storage directory", zap.Error(err))
		}
		localDriver := drivers.NewLocalDriver(dataPath, logger)
		eng.AddDriver("local", localDriver)
		eng.SetPrimary("local")
		logger.Info("using local storage", zap.String("path", dataPath))

	case "s3":
		// S3-compatible driver for production
		accessKey := os.Getenv("S3_ACCESS_KEY")
		secretKey := os.Getenv("S3_SECRET_KEY")
		if accessKey == "" || secretKey == "" {
			logger.Fatal("S3_ACCESS_KEY and S3_SECRET_KEY required for s3 mode")
		}

		s3Driver, err := drivers.NewS3CompatDriver(accessKey, secretKey, logger)
		if err != nil {
			logger.Fatal("failed to create S3 driver", zap.Error(err))
		}
		eng.AddDriver("s3", s3Driver)
		eng.SetPrimary("s3")
		logger.Info("using S3-compatible storage")

	default:
		logger.Fatal("invalid STORAGE_MODE", zap.String("mode", storageMode))
	}

	// Create server - pass nil for db since it's not used yet
	server := api.NewServer(cfg, logger, nil)

	// Handle shutdown gracefully
	go func() {
		sigChan := make(chan os.Signal, 1)
		signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
		<-sigChan

		logger.Info("shutting down server...")
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		if err := server.Shutdown(ctx); err != nil {
			logger.Error("shutdown error", zap.Error(err))
		}
		os.Exit(0)
	}()

	// Start server
	fmt.Printf("\n")
	fmt.Printf("╔══════════════════════════════════════╗\n")
	fmt.Printf("║       Vaultaire Server Started       ║\n")
	fmt.Printf("╠══════════════════════════════════════╣\n")
	fmt.Printf("║  S3 API: http://localhost:%-10d ║\n", cfg.Server.Port)
	fmt.Printf("║  Storage: %-26s ║\n", storageMode)
	fmt.Printf("╚══════════════════════════════════════╝\n")
	fmt.Printf("\n")

	if err := server.Start(); err != nil {
		logger.Fatal("server failed", zap.Error(err))
	}
}
