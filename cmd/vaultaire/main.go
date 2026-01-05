// cmd/vaultaire/main.go
package main

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/FairForge/vaultaire/internal/api"
	"github.com/FairForge/vaultaire/internal/config"
	"github.com/FairForge/vaultaire/internal/database"
	"github.com/FairForge/vaultaire/internal/drivers"
	"github.com/FairForge/vaultaire/internal/engine"
	"github.com/FairForge/vaultaire/internal/usage"
	"go.uber.org/zap"
)

type nilQuotaManager struct{}

func (n *nilQuotaManager) GetUsage(ctx context.Context, tenantID string) (used, limit int64, err error) {
	return 0, 1073741824, nil
}

func (n *nilQuotaManager) CheckAndReserve(ctx context.Context, tenantID string, bytes int64) (bool, error) {
	return true, nil
}

func (n *nilQuotaManager) ReleaseQuota(ctx context.Context, tenantID string, bytes int64) error {
	return nil
}

func (n *nilQuotaManager) CreateTenant(ctx context.Context, tenantID, plan string, storageLimit int64) error {
	return nil
}

func (n *nilQuotaManager) UpdateQuota(ctx context.Context, tenantID string, newLimit int64) error {
	return nil
}

func (n *nilQuotaManager) ListQuotas(ctx context.Context) ([]map[string]interface{}, error) {
	return []map[string]interface{}{}, nil
}

func (n *nilQuotaManager) DeleteQuota(ctx context.Context, tenantID string) error {
	return nil
}

func (n *nilQuotaManager) GetTier(ctx context.Context, tenantID string) (string, error) {
	return "starter", nil
}

func (n *nilQuotaManager) UpdateTier(ctx context.Context, tenantID, newTier string) error {
	return nil
}

func (n *nilQuotaManager) GetUsageHistory(ctx context.Context, tenantID string, days int) ([]map[string]interface{}, error) {
	return []map[string]interface{}{}, nil
}

func main() {
	// Create logger
	logger, _ := zap.NewProduction()
	defer func() { _ = logger.Sync() }()

	// Parse config
	port := 8000
	if p := os.Getenv("PORT"); p != "" {
		if _, err := fmt.Sscanf(p, "%d", &port); err != nil {
			logger.Error("invalid port number", zap.String("port", p), zap.Error(err))
			port = 8000
		}
	}

	cfg := &config.Config{
		Server: config.ServerConfig{
			Port: port,
		},
	}

	// Database configuration
	dbHost := os.Getenv("DB_HOST")
	if dbHost == "" {
		dbHost = "localhost"
	}
	dbPort := 5432
	if p := os.Getenv("DB_PORT"); p != "" {
		_, _ = fmt.Sscanf(p, "%d", &dbPort)
	}
	dbName := os.Getenv("DB_NAME")
	if dbName == "" {
		dbName = "vaultaire"
	}
	dbUser := os.Getenv("DB_USER")
	if dbUser == "" {
		dbUser = "viera"
	}
	dbPassword := os.Getenv("DB_PASSWORD")
	if dbPassword == "" {
		dbPassword = ""
	}

	// Try to connect to database
	var db *sql.DB
	dbConfig := database.Config{
		Host:     dbHost,
		Port:     dbPort,
		Database: dbName,
		User:     dbUser,
		Password: dbPassword,
		SSLMode:  "disable",
	}

	dbConn, err := database.NewPostgres(dbConfig, logger)
	if err != nil {
		logger.Warn("failed to connect to database, running without intelligence",
			zap.Error(err))
		db = nil
	} else {
		db = dbConn.DB()
		defer func() { _ = db.Close() }()
		logger.Info("connected to database",
			zap.String("host", dbHost),
			zap.String("database", dbName))
	}

	// Create engine with or without DB
	eng := engine.NewEngine(db, logger, &engine.Config{
		EnableCaching:  true,
		EnableML:       db != nil, // Only enable ML if we have DB
		DefaultBackend: "local",
	})

	// Configure backend costs
	eng.SetCostConfiguration(map[string]float64{
		"lyve":      0.00637,
		"quotaless": 0.001,
		"s3":        0.023,
		"onedrive":  0.0,
		"local":     0.0,
	})

	eng.SetEgressCosts(map[string]float64{
		"lyve":      0.0,
		"s3":        0.09,
		"quotaless": 0.01,
		"onedrive":  0.02,
		"local":     0.0,
	})

	// Initialize storage drivers
	// 1. Always add local driver
	dataPath := os.Getenv("DATA_PATH")
	if dataPath == "" {
		dataPath = "/tmp/vaultaire-data"
	}
	if err := os.MkdirAll(dataPath, 0750); err != nil {
		logger.Fatal("failed to create storage directory", zap.Error(err))
	}
	localDriver := drivers.NewLocalDriver(dataPath, logger)
	eng.AddDriver("local", localDriver)
	logger.Info("local driver added", zap.String("path", dataPath))

	// 2. Add S3 if credentials available
	if accessKey := os.Getenv("S3_ACCESS_KEY"); accessKey != "" {
		secretKey := os.Getenv("S3_SECRET_KEY")
		if s3Driver, err := drivers.NewS3CompatDriver(accessKey, secretKey, logger); err == nil {
			eng.AddDriver("s3", s3Driver)
			logger.Info("S3 driver added")
		} else {
			logger.Warn("failed to add S3 driver", zap.Error(err))
		}
	}

	// 3. Add Lyve if credentials available
	if accessKey := os.Getenv("LYVE_ACCESS_KEY"); accessKey != "" {
		secretKey := os.Getenv("LYVE_SECRET_KEY")
		region := os.Getenv("LYVE_REGION")
		if region == "" {
			region = "us-east-1"
		}
		if lyveDriver, err := drivers.NewLyveDriver(accessKey, secretKey, "", region, logger); err == nil {
			eng.AddDriver("lyve", lyveDriver)
			logger.Info("Lyve driver added")
		} else {
			logger.Warn("failed to add Lyve driver", zap.Error(err))
		}
	}

	// 4. Add Quotaless if credentials available
	if accessKey := os.Getenv("QUOTALESS_ACCESS_KEY"); accessKey != "" {
		secretKey := os.Getenv("QUOTALESS_SECRET_KEY")
		endpoint := os.Getenv("QUOTALESS_ENDPOINT")
		if endpoint == "" {
			endpoint = "https://us.quotaless.cloud:8000"
		}

		quotalessDriver, err := drivers.NewQuotalessDriver(accessKey, secretKey, endpoint, logger)
		if err != nil {
			logger.Warn("failed to create Quotaless driver", zap.Error(err))
		} else {
			eng.AddDriver("quotaless", quotalessDriver)
			logger.Info("quotaless driver added", zap.String("endpoint", endpoint))
		}
	}

	// 5. Set primary backend (auto-detect best available)
	storageMode := os.Getenv("STORAGE_MODE")
	if storageMode == "" {
		// Auto-detect: prefer Quotaless > Lyve > S3 > local
		if os.Getenv("QUOTALESS_ACCESS_KEY") != "" {
			storageMode = "quotaless"
		} else if os.Getenv("LYVE_ACCESS_KEY") != "" {
			storageMode = "lyve"
		} else if os.Getenv("S3_ACCESS_KEY") != "" {
			storageMode = "s3"
		} else {
			storageMode = "local"
		}
	}
	eng.SetPrimary(storageMode)
	logger.Info("primary backend set", zap.String("mode", storageMode))

	// Create server
	var server *api.Server
	if db != nil {
		quotaManager := usage.NewQuotaManager(db)
		eng.SetQuotaManager(quotaManager)
		server = api.NewServer(cfg, logger, eng, quotaManager, db)
	} else {
		server = api.NewServer(cfg, logger, eng, &nilQuotaManager{}, nil)
	}

	// Graceful shutdown
	go func() {
		sigChan := make(chan os.Signal, 1)
		signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
		<-sigChan

		logger.Info("shutting down...")
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		_ = eng.Shutdown(ctx)
		_ = server.Shutdown(ctx)
		os.Exit(0)
	}()

	// Start server
	fmt.Printf("\n")
	fmt.Printf("╔══════════════════════════════════════╗\n")
	fmt.Printf("║       Vaultaire Server Started       ║\n")
	fmt.Printf("╠══════════════════════════════════════╣\n")
	fmt.Printf("║  S3 API: http://localhost:%-10d ║\n", port)
	fmt.Printf("║  Storage: %-26s ║\n", storageMode)
	if db != nil {
		fmt.Printf("║  Intelligence: ENABLED               ║\n")
	} else {
		fmt.Printf("║  Intelligence: DISABLED (no DB)      ║\n")
	}
	fmt.Printf("╚══════════════════════════════════════╝\n")
	fmt.Printf("\n")

	if err := server.Start(); err != nil {
		logger.Fatal("server failed", zap.Error(err))
	}
}
