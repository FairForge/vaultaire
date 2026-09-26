// cmd/vaultaire/main.go
package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
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

// shutdownTimeout bounds the whole stop sequence: HTTP drain (in-flight
// uploads/downloads), the trackers' synchronous flush, then the engine.
// systemd's TimeoutStopSec must stay above this.
const shutdownTimeout = 30 * time.Second

// shutdowner is the slice of *api.Server and *engine.CoreEngine that the
// stop sequence needs; interfaces so the order is unit-testable.
type shutdowner interface {
	Shutdown(ctx context.Context) error
}

// gracefulShutdown stops the process in the only order that works: the HTTP
// server first — its Shutdown waits for in-flight requests and then flushes
// the buffered bandwidth/CDN/access-log trackers, both of which need the
// database — and the engine second, because engine.Shutdown closes the
// *sql.DB that both of them share (review R1-03: the previous order closed
// the pool under draining requests and the flush).
func gracefulShutdown(ctx context.Context, logger *zap.Logger, srv, eng shutdowner) {
	if err := srv.Shutdown(ctx); err != nil {
		logger.Warn("http server drain incomplete", zap.Error(err))
	}
	if err := eng.Shutdown(ctx); err != nil {
		logger.Warn("engine shutdown error", zap.Error(err))
	}
}

// serveUntilShutdown runs the blocking serve function and tells the two ways
// it can return apart. http.Server.ListenAndServe returns ErrServerClosed
// the instant Shutdown closes the listener — BEFORE in-flight requests have
// drained and before Shutdown itself returns — so that value means "the stop
// sequence has started", not "failed": wait for it to finish (review R1-02:
// treating it as fatal exited with status 1 mid-drain on every deploy). Any
// other error is a real failure and is returned as-is.
func serveUntilShutdown(serve func() error, shutdownDone <-chan struct{}) error {
	err := serve()
	if errors.Is(err, http.ErrServerClosed) {
		<-shutdownDone
		return nil
	}
	return err
}

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
		// Caching is OFF: cache.TieredCache is an unbounded map whose Config
		// is ignored (WP-2 / CR-2, R0-01). R6 decided the code path should
		// be deleted rather than fixed (docs/reviews/R6-engine.md, WP-R6-6);
		// any future read cache must be bytes-capped, evicting, per-object
		// capped and filled outside the request goroutine.
		EnableCaching:  false,
		EnableML:       db != nil, // Only enable ML if we have DB
		DefaultBackend: "local",
	})

	// Configure backend costs
	eng.SetCostConfiguration(map[string]float64{
		"idrive": 0.0033, // $3.30/TB
		// $7.99/TB. Lyve invoices us $0 under a 1-year SaaS promo; we track a
		// conservative rate so the promo cannot mask the post-promo liability.
		"lyve":       0.00799,
		"quotaless":  0.001,
		"s3":         0.023,
		"permafrost": 0.0,
		"local":      0.0,
		"geyser":     0.00155, // $1.55/TB
		"r2":         0.01536, // $15.36/TB — public buckets only, paid for by $0 egress
	})

	eng.SetEgressCosts(map[string]float64{
		"idrive":     0.0,
		"lyve":       0.0,
		"s3":         0.09,
		"quotaless":  0.01,
		"permafrost": 0.0,
		"local":      0.0,
		"geyser":     0.0,
		"r2":         0.0,
	})

	// Initialize storage drivers
	// 1. Always add local driver
	dataPath := os.Getenv("DATA_PATH")
	if dataPath == "" {
		dataPath = "/tmp/vaultaire-data"
	}
	if err := os.MkdirAll(dataPath, 0750); err != nil { // #nosec G703 — TODO: sanitize DATA_PATH to prevent traversal
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
			// Closest Lyve region to the SLC prod box. Buckets are homed per
			// region on Lyve Cloud 2 — see internal/drivers/lyve.go.
			region = "us-west-1"
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

	// 5. Add Geyser if credentials available
	if accessKey := os.Getenv("GEYSER_ACCESS_KEY"); accessKey != "" {
		secretKey := os.Getenv("GEYSER_SECRET_KEY")
		bucket := os.Getenv("GEYSER_BUCKET")
		if bucket == "" {
			bucket = "stored3lib-632df558-9627-427b-ab86-9f3ff1eaafe9"
		}
		var geyserOpts []drivers.GeyserOption
		if ep := os.Getenv("GEYSER_ENDPOINT"); ep != "" {
			geyserOpts = append(geyserOpts, drivers.WithGeyserEndpoint(ep))
		}
		geyserDriver, err := drivers.NewGeyserDriver(accessKey, secretKey, bucket, "vaultaire", logger, geyserOpts...)
		if err != nil {
			logger.Warn("failed to create Geyser driver", zap.Error(err))
		} else {
			eng.AddDriver("geyser", geyserDriver)
			logger.Info("geyser driver added (LTO-9 tape)", zap.String("bucket", bucket))
		}
	}

	// 6. Add iDrive if credentials available — register one driver per region
	if accessKey := os.Getenv("IDRIVE_ACCESS_KEY"); accessKey != "" {
		secretKey := os.Getenv("IDRIVE_SECRET_KEY")
		defaultEndpoint := os.Getenv("IDRIVE_ENDPOINT")
		if defaultEndpoint == "" {
			defaultEndpoint = "https://e2-us-west-1.idrive.com"
		}
		defaultRegion := os.Getenv("IDRIVE_REGION")
		if defaultRegion == "" {
			defaultRegion = "us-west-1"
		}

		// Register default driver (backward compat alias).
		idriveDriver, err := drivers.NewIDriveDriver(accessKey, secretKey, defaultEndpoint, defaultRegion, logger)
		if err != nil {
			logger.Warn("failed to add iDrive driver", zap.Error(err))
		} else {
			eng.AddDriver("idrive", idriveDriver)
			logger.Info("iDrive driver added", zap.String("endpoint", defaultEndpoint), zap.String("region", defaultRegion))
		}

		// Register per-region drivers for bucket-level region routing. The
		// reseller account mints one key pair per region, so each region
		// takes IDRIVE_<REGION>_ACCESS_KEY/_SECRET_KEY (+ optional _ENDPOINT)
		// when set and falls back to the primary pair otherwise.
		for region := range drivers.IDriveRegions {
			driverName := "idrive-" + region
			regionAK, regionSK := drivers.IDriveRegionCredentials(os.Getenv, region)
			endpoint := drivers.IDriveRegionEndpoint(os.Getenv, region)
			drv, drvErr := drivers.NewIDriveDriver(regionAK, regionSK, endpoint, region, logger)
			if drvErr != nil {
				logger.Warn("failed to add region iDrive driver",
					zap.String("region", region), zap.Error(drvErr))
				continue
			}
			eng.AddDriver(driverName, drv)
		}
		logger.Info("iDrive per-region drivers registered", zap.Int("regions", len(drivers.IDriveRegions)))
	}

	// 6b. Cloudflare R2 — PUBLIC BUCKETS / CDN ORIGIN ONLY, never a tier
	// (SMART_TIER_DESIGN.md, revised 2026-09-19). Public-read buckets resolve
	// to the PUBLIC storage class → this driver; everything else ignores it.
	if r2Account := os.Getenv("R2_ACCOUNT_ID"); r2Account != "" {
		r2Driver, err := drivers.NewR2Driver(r2Account,
			os.Getenv("R2_ACCESS_KEY"), os.Getenv("R2_SECRET_KEY"),
			os.Getenv("R2_JURISDICTION"), os.Getenv("R2_BUCKET"), logger)
		if err != nil {
			logger.Warn("failed to add R2 driver", zap.Error(err))
		} else {
			eng.AddDriver("r2", r2Driver)
			logger.Info("R2 driver added (public buckets / CDN origin)",
				zap.String("jurisdiction", os.Getenv("R2_JURISDICTION")),
				zap.String("bucket", r2Driver.Bucket()))
		}
	}

	// 7. Add Permafrost (OneDrive fleet) — internal parity tier, not customer-facing
	if os.Getenv("TENANT_1_ID") != "" {
		onedriveDriver, err := drivers.NewOneDriveFleetDriver(logger)
		if err != nil {
			logger.Warn("failed to add Permafrost driver", zap.Error(err))
		} else {
			eng.AddDriver("permafrost", onedriveDriver)
			logger.Info("Permafrost fleet driver added", zap.Int("tenants", onedriveDriver.TenantCount()))
		}
	}

	// 8. Set primary backend (auto-detect best available)
	storageMode := os.Getenv("STORAGE_MODE")
	if storageMode == "" {
		// Auto-detect: prefer iDrive > Quotaless > S3 > Geyser > local
		if os.Getenv("IDRIVE_ACCESS_KEY") != "" {
			storageMode = "idrive"
		} else if os.Getenv("QUOTALESS_ACCESS_KEY") != "" {
			storageMode = "quotaless"
		} else if os.Getenv("S3_ACCESS_KEY") != "" {
			storageMode = "s3"
		} else if os.Getenv("GEYSER_ACCESS_KEY") != "" {
			storageMode = "geyser"
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
		server = api.NewServer(cfg, logger, eng, quotaManager, db)
	} else {
		server = api.NewServer(cfg, logger, eng, &nilQuotaManager{}, nil)
	}

	// Graceful shutdown: SIGTERM/SIGINT → drain HTTP + flush trackers →
	// engine (closes the DB). main waits on shutdownDone so the process
	// exits only after the sequence completes, and exits 0.
	shutdownDone := make(chan struct{})
	go func() {
		defer close(shutdownDone)
		sigChan := make(chan os.Signal, 1)
		signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
		sig := <-sigChan

		logger.Info("shutting down...", zap.String("signal", sig.String()))
		ctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()

		gracefulShutdown(ctx, logger, server, eng)
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

	if err := serveUntilShutdown(server.Start, shutdownDone); err != nil {
		logger.Fatal("server failed", zap.Error(err))
	}
	logger.Info("shutdown complete")
}
