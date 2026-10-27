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
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/FairForge/vaultaire/internal/api"
	"github.com/FairForge/vaultaire/internal/config"
	"github.com/FairForge/vaultaire/internal/database"
	"github.com/FairForge/vaultaire/internal/drivers"
	"github.com/FairForge/vaultaire/internal/engine"
	"github.com/FairForge/vaultaire/internal/node"
	"github.com/FairForge/vaultaire/internal/usage"
	"go.uber.org/zap"
)

// shutdownTimeout bounds the HTTP drain (in-flight uploads/downloads) and
// the engine's close. Between the two, api.Server.Shutdown waits for the long
// S3 operations the drain leaves running (CompleteMultipartUpload, CopyObject,
// DeleteObjects: detached from their clients once 200 + keep-alive is sent)
// up to 15 min from each one's start (api longOpDrainBound), with its own
// clock; systemd's TimeoutStopSec (deploy/systemd/vaultaire@.service, 1000 s)
// must stay above the sum.
const shutdownTimeout = 30 * time.Second

// shutdowner is the slice of *api.Server and *engine.CoreEngine that the
// stop sequence needs; interfaces so the order is unit-testable.
type shutdowner interface {
	Shutdown(ctx context.Context) error
}

// gracefulShutdown stops the process in the only order that works: the HTTP
// server first — its Shutdown waits for in-flight requests, then for the
// detached long operations, and then flushes the buffered
// bandwidth/CDN/access-log trackers, all of which need the database — and
// the engine second, because engine.Shutdown closes the
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

// isNodeRole says whether this process is a data node (WP-EDGE-3:
// VAULTAIRE_ROLE=node — no database, no engine, no jobs; internal/node).
func isNodeRole(getenv func(string) string) bool {
	return strings.TrimSpace(getenv(node.RoleEnv)) == node.RoleNode
}

func main() {
	// A data node opens nothing of the origin's: it is decided before any
	// driver, database or job exists.
	if isNodeRole(os.Getenv) {
		node.Main(api.BuildSHA)
		return
	}

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
	eng := engine.NewEngine(db, logger, &engine.Config{DefaultBackend: "local"})
	// Every driver is registered before eng.Seal() below: a refusal here is
	// a boot-order bug (WP-R6-2).
	addDriver := func(name string, d engine.Driver) {
		if err := eng.AddDriver(name, d); err != nil {
			logger.Fatal("driver registration refused", zap.String("name", name), zap.Error(err))
		}
	}

	// The drivers' shared DNS cache: an answer kept DRIVER_DNS_TTL (WP-R7-6),
	// set before the first driver dials.
	drivers.ConfigureDriverDNS(logger)

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
	addDriver("local", localDriver)
	logger.Info("local driver added", zap.String("path", dataPath))

	// 2. Add S3 if credentials available
	if accessKey := os.Getenv("S3_ACCESS_KEY"); accessKey != "" {
		secretKey := os.Getenv("S3_SECRET_KEY")
		if s3Driver, err := drivers.NewS3CompatDriver(accessKey, secretKey, logger); err == nil {
			addDriver("s3", s3Driver)
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
			checkLyveBucketLock(lyveDriver, logger)
			addDriver("lyve", lyveDriver)
			logger.Info("Lyve driver added", zap.Bool("target_only", lyveDriver.TargetOnly()))
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
			addDriver("quotaless", quotalessDriver)
			logger.Info("quotaless driver added", zap.String("endpoint", endpoint))
		}
	}

	// 4b. Add Sia (the s3d gateway) if credentials available. Registered,
	// never auto-selected as primary; a lab selects it with STORAGE_MODE=sia.
	if accessKey := os.Getenv("SIA_ACCESS_KEY"); accessKey != "" {
		secretKey := os.Getenv("SIA_SECRET_KEY")
		endpoint, bucket := drivers.SiaConfigFromEnv(os.Getenv)
		siaDriver, err := drivers.NewSiaDriver(accessKey, secretKey, endpoint, bucket, logger)
		if err != nil {
			logger.Error("failed to add Sia driver", zap.Error(err))
		} else {
			bootCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			created, ensureErr := siaDriver.EnsureBucket(bootCtx)
			cancel()
			if ensureErr != nil {
				// Registered anyway: s3d may still be starting; the probe
				// keeps reporting it.
				logger.Error("Sia bucket not confirmed at boot", zap.String("bucket", bucket), zap.Error(ensureErr))
			}
			addDriver("sia", siaDriver)
			logger.Info("Sia driver added",
				zap.String("endpoint", endpoint), zap.String("bucket", bucket),
				zap.Bool("bucket_created", created))
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
		// No driver-level default tenant (it was "vaultaire"): a call whose
		// context names no tenant is refused, not filed under a made-up one
		// (WP-R8-7, internal/drivers/tenant_ctx.go).
		geyserDriver, err := drivers.NewGeyserDriver(accessKey, secretKey, bucket, "", logger, geyserOpts...)
		if err != nil {
			logger.Warn("failed to create Geyser driver", zap.Error(err))
		} else {
			addDriver("geyser", geyserDriver)
			logger.Info("geyser driver added (LTO-9 tape)", zap.String("bucket", bucket))
		}
	}

	// 6. Add iDrive if credentials available — the primary plus one driver per
	// region that has its own key pair (WP-R7-1).
	if accessKey := os.Getenv("IDRIVE_ACCESS_KEY"); accessKey != "" {
		secretKey := os.Getenv("IDRIVE_SECRET_KEY")
		defaultRegion := drivers.IDriveDefaultRegion(os.Getenv)
		defaultEndpoint := os.Getenv("IDRIVE_ENDPOINT")
		if defaultEndpoint == "" {
			defaultEndpoint = drivers.IDriveRegionEndpoint(os.Getenv, defaultRegion)
		}

		// The primary serves the default region.
		idriveDriver, err := drivers.NewIDriveDriver(accessKey, secretKey, defaultEndpoint, defaultRegion, logger)
		if err != nil {
			logger.Warn("failed to add iDrive driver", zap.Error(err))
		} else {
			addDriver("idrive", idriveDriver)
			logger.Info("iDrive driver added", zap.String("endpoint", defaultEndpoint), zap.String("region", defaultRegion))
		}

		// Per-region drivers for bucket-level data residency. The reseller
		// account mints one key pair per region and the primary pair answers
		// 403 everywhere else (Review R7-01), so a region is registered ONLY
		// with IDRIVE_<REGION>_ACCESS_KEY/_SECRET_KEY set; its fixed bucket is
		// created on first boot if absent. Regions without a pair are refused
		// at CreateBucket rather than silently stored on the primary.
		regions := make([]string, 0, len(drivers.IDriveRegions))
		for region := range drivers.IDriveRegions {
			regions = append(regions, region)
		}
		sort.Strings(regions)
		var registered []string
		for _, region := range regions {
			if region == defaultRegion {
				continue
			}
			regionAK, regionSK := drivers.IDriveRegionCredentials(os.Getenv, region)
			if regionAK == "" {
				logger.Info("iDrive region not enabled (no dedicated key pair)",
					zap.String("region", region),
					zap.String("env", drivers.IDriveRegionEnvKey(region, "ACCESS_KEY")))
				continue
			}
			endpoint := drivers.IDriveRegionEndpoint(os.Getenv, region)
			drv, drvErr := drivers.NewIDriveDriver(regionAK, regionSK, endpoint, region, logger)
			if drvErr != nil {
				logger.Error("failed to add region iDrive driver",
					zap.String("region", region), zap.Error(drvErr))
				continue
			}
			ectx, ecancel := context.WithTimeout(context.Background(), 20*time.Second)
			if created, eerr := drv.EnsureBucket(ectx); eerr != nil {
				logger.Error("iDrive region bucket not verified — region registered, probe will alert",
					zap.String("region", region), zap.Error(eerr))
			} else if created {
				logger.Info("iDrive region bucket provisioned", zap.String("region", region))
			}
			ecancel()
			// The driver calls itself "idrive"; its put-retry series must say
			// which region stalled (WP-VAULT-1).
			drv.SetBackendName("idrive-" + region)
			addDriver("idrive-"+region, drv)
			registered = append(registered, region)
		}
		drivers.SetAvailableIDriveRegions(defaultRegion, registered)
		logger.Info("iDrive regions enabled",
			zap.String("default", defaultRegion),
			zap.Strings("regions", registered),
			zap.Int("available_in_account", len(drivers.IDriveRegions)))
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
			addDriver("r2", r2Driver)
			logger.Info("R2 driver added (public buckets / CDN origin)",
				zap.String("jurisdiction", os.Getenv("R2_JURISDICTION")),
				zap.String("bucket", r2Driver.Bucket()))
		}
	}

	// 6b. Add Wasabi if credentials available — the interim Standard-tier
	// primary (owner decision 2026-10-03: the iDrive prod key answers 403 on
	// object calls while the account is repaired; the partner account is
	// free). Same fixed-bucket shape as iDrive under its own name; the bucket
	// is created in the region on first boot. Becomes the primary only via
	// STORAGE_MODE=wasabi (or when no iDrive pair is set).
	if accessKey := os.Getenv("WASABI_ACCESS_KEY"); accessKey != "" {
		secretKey := os.Getenv("WASABI_SECRET_KEY")
		endpoint, region, bucket := drivers.WasabiConfigFromEnv(os.Getenv)
		wasabiDriver, err := drivers.NewWasabiDriver(accessKey, secretKey, endpoint, region, bucket, logger)
		if err != nil {
			logger.Error("failed to add Wasabi driver", zap.Error(err))
		} else {
			bootCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			created, ensureErr := wasabiDriver.EnsureBucket(bootCtx)
			cancel()
			if ensureErr != nil {
				// Registered anyway: a transient HeadBucket failure must not
				// unregister the primary; the probe keeps reporting it.
				logger.Error("Wasabi bucket not confirmed at boot", zap.String("bucket", bucket), zap.Error(ensureErr))
			}
			addDriver("wasabi", wasabiDriver)
			logger.Info("Wasabi driver added",
				zap.String("endpoint", endpoint), zap.String("region", region),
				zap.String("bucket", bucket), zap.Bool("bucket_created", created))
		}
	}

	// 7. Add Permafrost (OneDrive fleet) — internal parity tier, not customer-facing
	if os.Getenv("TENANT_1_ID") != "" {
		onedriveDriver, err := drivers.NewOneDriveFleetDriver(logger)
		if err != nil {
			logger.Warn("failed to add Permafrost driver", zap.Error(err))
		} else {
			addDriver("permafrost", onedriveDriver)
			logger.Info("Permafrost fleet driver added", zap.Int("tenants", onedriveDriver.TenantCount()))
			checkPermafrostFleet(onedriveDriver)
		}
	}

	// 7b. Sync.com's encrypted WebDAV bridge (`sync-webdav`, on this box,
	// localhost only) — a TARGET-ONLY backend like permafrost: never the
	// primary, never a failover destination (engine targetOnlyBackends), and
	// placed on only for a bucket with tier_preference 'sync' of a tenant
	// with the sync_backend flag. Sync's terms forbid reselling the service
	// without its written consent. See internal/drivers/webdav_README.md.
	// One or several bridges (SYNC_WEBDAV_URLS + SYNC_WEBDAV_PASSWORDS): each
	// key lives on one bridge (HRW), reads fall back, writes do not
	// (internal/drivers/webdav_multi.go). Passwords are never logged.
	syncCfg, syncOK, syncErr := drivers.SyncWebDAVConfigFromEnv(os.Getenv)
	if syncOK {
		for _, w := range syncCfg.Warnings {
			logger.Warn(w)
		}
	}
	switch {
	case syncErr != nil:
		logger.Error("failed to add Sync WebDAV driver: bad bridge configuration", zap.Error(syncErr))
	case syncOK:
		syncDriver, err := drivers.NewMultiWebDAVDriver("sync", syncCfg, logger)
		if err != nil {
			logger.Error("failed to add Sync WebDAV driver", zap.Error(err))
		} else {
			addDriver("sync", syncDriver)
			logger.Info("Sync WebDAV driver added (target-only; sync_backend flag)",
				zap.Int("bridges", syncDriver.Bridges()), zap.String("root", syncCfg.Root))
		}
	}

	// 7c. Blomp's OpenStack Swift storage (internal/drivers/swift_README.md):
	// target-only and strict like sync — placed on only for a bucket with
	// tier_preference 'blomp' of a tenant with the blomp_backend flag. Its
	// storage URL is plain HTTP: writes there are sealed unless
	// BLOMP_ALLOW_UNSEALED (testing). The password is never logged.
	blompCfg, blompOK, blompErr := drivers.BlompConfigFromEnv(os.Getenv)
	switch {
	case blompErr != nil:
		logger.Error("failed to add Blomp driver: bad configuration", zap.Error(blompErr))
	case blompOK:
		blompDriver, err := drivers.NewSwiftDriver("blomp", blompCfg, logger)
		if err != nil {
			logger.Error("failed to add Blomp driver", zap.Error(err))
		} else {
			addDriver("blomp", blompDriver)
			logger.Info("Blomp (Swift) driver added (target-only; blomp_backend flag)",
				zap.String("user", blompCfg.User), zap.String("container", blompCfg.Container),
				zap.Int64("segment_size", blompCfg.SegmentSize), zap.Int("put_concurrency", blompCfg.PutConcurrency),
				zap.Int64("put_budget", blompCfg.PutBudget), zap.Duration("listing_settle", blompDriver.ListingLag()))
		}
	}

	registerLabDrivers(eng, logger) // a -tags lab build only (lab_on.go)

	// 8. Set primary backend: STORAGE_MODE, else the first configured of
	// iDrive > Wasabi > Quotaless > S3 > Geyser, else local
	// (config.StorageModeOrder; the dashboard reads the same value).
	storageMode := config.DetectStorageMode(os.Getenv)
	if _, ok := eng.GetDriver(storageMode); !ok && storageMode != "local" {
		// A STORAGE_MODE naming a driver that did not register (bad pair,
		// typo) would make every PUT fail with "backend not found" and the
		// boot log say nothing about why. Fail loudly here (WP-F).
		logger.Fatal("STORAGE_MODE names a backend that is not registered",
			zap.String("mode", storageMode), zap.Strings("registered", eng.GetDriverNames()))
	}
	primaryDriver, _ := eng.GetDriver(storageMode)
	if err := refusedPrimary(storageMode, primaryDriver); err != nil {
		logger.Fatal("STORAGE_MODE names a backend that may not be the primary", zap.Error(err))
	}
	eng.SetPrimary(storageMode)
	logger.Info("primary backend set", zap.String("mode", storageMode))
	eng.Seal() // every driver is registered: AddDriver is refused from here (WP-R6-2)

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

// refusedPrimary refuses a STORAGE_MODE that names a backend which must never
// take every tenant's objects: `sync` (Sync.com's bridge — its terms forbid
// reselling the service without written consent; config.DetectStorageMode
// never picks it, this stops an operator's STORAGE_MODE=sync), and a driver
// that declares itself target-only (engine.TargetOnlyDeclarer: a Lyve bucket
// with Object Lock or a default retention, WP-R7-7).
func refusedPrimary(mode string, drv engine.Driver) error {
	if mode == "sync" {
		return fmt.Errorf("%q is a target-only backend (Sync.com: customer data only for flagged tenants)", mode)
	}
	if mode == "blomp" {
		return fmt.Errorf("%q is a target-only backend (Blomp: one container, ~3 writes/s, plain HTTP — flagged tenants' blomp buckets only)", mode)
	}
	if drv != nil && engine.IsDeclaredTargetOnly(drv) {
		return fmt.Errorf("%q declares itself target-only: its bucket has Object Lock enabled or a default retention, so every object written there would be undeletable until the retention lapsed (WP-R7-7; R7-16) — remove the lock or choose another primary", mode)
	}
	return nil
}

// checkPermafrostFleet compares the fleet's order with the one its accounts'
// markers recorded (WP-R7-4, R7-09; drivers/onedrive_fleet.go): a mismatch
// is the driver's Error line and vaultaire_permafrost_fleet_mismatch 1,
// PERMAFROST_FLEET_ACCEPT=<fingerprint> accepts a new order. Never a boot
// failure: reads find a remapped object by the fleet walk. A verdict the
// boot budget could not reach (unknown) is re-checked by the driver's probe
// at its next run, then hourly (RP18-1); the accept value given here is
// the one those re-checks use.
func checkPermafrostFleet(d *drivers.OneDriveDriver) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	_, _ = d.CheckFleetFingerprint(ctx, os.Getenv("PERMAFROST_FLEET_ACCEPT")) // logged by the driver
}

// checkLyveBucketLock reads the Lyve bucket's object-lock configuration at
// boot (WP-R7-7, R7-16): locked = an Error line and the driver registered
// target-only (never the primary, never a fail-over destination); a key that
// may not read it = a Warn and no guard; a failed read = a Warn, the driver
// as it was. Never a boot failure.
func checkLyveBucketLock(d *drivers.LyveDriver, logger *zap.Logger) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	locked, err := d.CheckBucketLock(ctx)
	switch {
	case errors.Is(err, drivers.ErrLyveLockUnreadable):
		logger.Warn("lyve: this key cannot read the bucket's object-lock configuration — no lock guard (never enable a default retention on stored-*)",
			zap.String("region", d.Region()), zap.Error(err))
	case err != nil:
		logger.Warn("lyve: the bucket's object-lock configuration could not be read at boot — no lock guard this boot",
			zap.String("region", d.Region()), zap.Error(err))
	case locked:
		logger.Error("lyve: the bucket has Object Lock enabled or a default retention — registered TARGET-ONLY (never the primary, never a fail-over destination): a write there would be undeletable until the retention lapsed",
			zap.String("region", d.Region()), zap.String("bucket", "stored-"+d.Region()))
	default:
		logger.Info("lyve: bucket has no object lock", zap.String("region", d.Region()))
	}
}
