// Package ha provides high availability capabilities for the Vaultaire storage system.
//
// # Overview
//
// The ha package implements a complete high availability solution including:
//   - Health monitoring and state management
//   - Automatic failover with configurable policies
//   - Load balancing with health-aware routing
//   - Geographic redundancy and region management
//   - Backup strategies with retention policies
//   - Disaster recovery orchestration
//   - RTO/RPO target tracking and SLA compliance
//   - Comprehensive monitoring and alerting
//
// # Architecture
//
// The HA system follows a layered architecture:
//
//	┌─────────────────────────────────────────────────────────┐
//	│                     HAMonitor                           │
//	│  (Metrics collection, alerting, Prometheus export)     │
//	├─────────────────────────────────────────────────────────┤
//	│                   HAOrchestrator                        │
//	│  (Backend state management, failover coordination)      │
//	├──────────────────┬──────────────────┬───────────────────┤
//	│   LoadBalancer   │   GeoManager     │   DROrchestrator  │
//	│  (Traffic dist)  │ (Region routing) │  (DR automation)  │
//	├──────────────────┴──────────────────┴───────────────────┤
//	│                   BackupManager                          │
//	│  (Backup scheduling, retention, verification)           │
//	├─────────────────────────────────────────────────────────┤
//	│                   RTORPOTracker                          │
//	│  (Recovery objectives, SLA compliance, incidents)       │
//	└─────────────────────────────────────────────────────────┘
//
// # Quick Start
//
// Basic HA setup with health monitoring and automatic failover:
//
//	// Create the orchestrator
//	orchestrator := ha.NewHAOrchestrator()
//
//	// Register backends
//	orchestrator.RegisterBackend("primary", ha.BackendConfig{
//		Primary:          true,
//		FailureThreshold: 3,
//		RecoveryThreshold: 2,
//		CircuitBreaker:   true,
//	})
//
//	orchestrator.RegisterBackend("secondary", ha.BackendConfig{
//		Primary:          false,
//		FailureThreshold: 3,
//		RecoveryThreshold: 2,
//	})
//
//	// Configure automatic failover
//	orchestrator.ConfigureFailover("primary", ha.FailoverRule{
//		SecondaryBackend: "secondary",
//		AutoFailover:     true,
//		FailoverDelay:    5 * time.Second,
//	})
//
//	// Subscribe to events
//	orchestrator.Subscribe(func(event ha.HAEvent) {
//		log.Printf("HA Event: %s on %s", event.Type, event.Backend)
//	})
//
//	// Report health checks (typically from a health checker goroutine)
//	orchestrator.ReportHealthCheck("primary", true, 50*time.Millisecond, nil)
//
// # Components
//
// ## HAOrchestrator
//
// The central coordinator for backend health state management. It tracks:
//   - Backend health states (Healthy, Degraded, Failed, Recovering)
//   - Consecutive failure/success counts
//   - Circuit breaker states
//   - Failover rules and active backend mappings
//
// State transitions follow this pattern:
//
//	Healthy ──(failures)──► Degraded ──(more failures)──► Failed
//	   ▲                        │                           │
//	   └────────(recoveries)────┴───────(recoveries)────────┘
//	                            ▼
//	                       Recovering
//
// ## LoadBalancer
//
// Provides weighted round-robin load balancing with health awareness:
//
//	lb := ha.NewLoadBalancer()
//	lb.AddBackend("backend-1", 100)
//	lb.AddBackend("backend-2", 100)
//
//	// Get next backend for request
//	backend := lb.Next()
//
//	// Adjust weights based on health
//	lb.SetWeight("backend-1", 50)  // Reduce weight for degraded backend
//
// The HALoadBalancer integrates with HAOrchestrator to automatically
// adjust weights based on health state:
//   - Failed backends: excluded from rotation
//   - Degraded backends: weight reduced by 50%
//   - Recovering backends: weight reduced by 70%
//   - Healthy backends: full configured weight
//
// ## GeoManager
//
// Manages geographic regions and routing:
//
//	config := ha.DefaultGeoConfig()
//	geoManager, err := ha.NewGeoManager(config)
//
//	// Select optimal region for client
//	region, err := geoManager.SelectRegion(clientLat, clientLon)
//
//	// Handle region failure
//	fallback, err := geoManager.FailoverRegion(region)
//
// Default regions (NYC and LA) can be customized via GeoConfig.
//
// ## BackupManager
//
// Manages backup configurations and jobs:
//
//	geoManager, _ := ha.NewGeoManager(ha.DefaultGeoConfig())
//	backupMgr := ha.NewBackupManager(geoManager)
//
//	// Add backup configuration
//	config := &ha.BackupConfig{
//		Name:            "daily-backup",
//		SourceRegion:    ha.RegionNYC,
//		TargetRegion:    ha.RegionLA,
//		Schedule:        "0 2 * * *",  // 2 AM daily
//		RetentionDays:   30,
//		CompressionType: ha.CompressionGzip,
//	}
//	backupMgr.AddConfig("daily-backup", config)
//
//	// Start a backup
//	jobID, err := backupMgr.StartBackup(ctx, "daily-backup")
//
// ## DROrchestrator
//
// Coordinates disaster recovery operations:
//
//	geoManager, _ := ha.NewGeoManager(ha.DefaultGeoConfig())
//	drConfig := &ha.DRConfig{
//		AutoFailover:     true,
//		FailoverDelay:    30 * time.Second,
//		HealthCheckInterval: 10 * time.Second,
//	}
//	drOrch, err := ha.NewDROrchestrator(geoManager, drConfig)
//
//	// Start DR monitoring
//	ctx, cancel := context.WithCancel(context.Background())
//	go drOrch.Start(ctx)
//
//	// Force manual failover
//	err = drOrch.ForceFailover(ha.RegionLA)
//
//	// Initiate recovery
//	err = drOrch.InitiateRecovery(ha.RegionNYC)
//
// ## RTORPOTracker
//
// Tracks recovery time and point objectives:
//
//	// Use predefined service tiers
//	config := ha.GetTierDefaults(ha.TierCritical)
//	// Critical: 1 min RTO, 30 sec RPO
//	// Standard: 15 min RTO, 5 min RPO
//	// BestEffort: 4 hour RTO, 1 hour RPO
//
//	tracker, err := ha.NewRTORPOTracker(config)
//
//	// Start tracking an incident
//	incidentID, err := tracker.StartIncident("primary-backend", time.Now())
//
//	// Resolve incident
//	err = tracker.ResolveIncident(incidentID, time.Now(), time.Now().Add(-30*time.Second))
//
//	// Check compliance
//	status := tracker.CheckStatus("primary-backend")
//	if status.Level == ha.StatusCritical {
//		// RTO breached!
//	}
//
//	// Generate SLA report
//	report := ha.GenerateSLAReport(tracker, startDate, endDate)
//
// ## HAMonitor
//
// Provides comprehensive monitoring and alerting:
//
//	config := &ha.HAMonitorConfig{
//		CollectionInterval: 10 * time.Second,
//		RetentionPeriod:    7 * 24 * time.Hour,
//		AlertThresholds: ha.AlertThresholds{
//			FailedBackendPercent: 30.0,
//			LatencyP99Threshold:  2 * time.Second,
//		},
//	}
//	monitor := ha.NewHAMonitor(config)
//
//	// Register components
//	monitor.RegisterOrchestrator(orchestrator)
//	monitor.RegisterGeoManager(geoManager)
//
//	// Subscribe to alerts
//	monitor.SubscribeAlerts(func(alert ha.Alert) {
//		notifyOps(alert)
//	})
//
//	// Start monitoring loop
//	go monitor.Start(ctx)
//
//	// Get Prometheus metrics
//	metrics := monitor.GetPrometheusMetrics()
//
//	// Get dashboard data
//	dashboard := monitor.GetDashboardData()
//
// ## FailoverTestRunner
//
// Enables automated failover scenario testing:
//
//	runner := ha.NewFailoverTestRunner(orchestrator, geoManager, rtoTracker)
//
//	// Define test scenarios
//	scenario := ha.FailoverScenario{
//		Name:          "primary-failure",
//		FailureType:   ha.FailureTypeComplete,
//		TargetBackend: "primary",
//		ExpectedResult: ha.ExpectedResult{
//			FailoverTriggered: true,
//			ServiceAvailable:  true,
//			RTOMet:            true,
//		},
//		Timeout: 30 * time.Second,
//	}
//	runner.AddScenario(scenario)
//
//	// Execute tests
//	runner.RunAllScenarios(ctx)
//
//	// Generate report
//	report := runner.GenerateReport()
//
// # Failure Types
//
// The testing framework supports multiple failure types:
//   - FailureTypeComplete: Backend completely unavailable
//   - FailureTypePartial: Backend degraded (slow/errors)
//   - FailureTypeNetwork: Network partition simulation
//   - FailureTypeLatency: High latency injection
//   - FailureTypeCascading: Multiple backends fail
//   - FailureTypeIntermittent: Flapping backend behavior
//
// # Best Practices
//
// 1. Configure appropriate thresholds:
//   - FailureThreshold: 3 (avoid false positives)
//   - RecoveryThreshold: 2 (ensure stable recovery)
//   - FailoverDelay: 5-30s (balance speed vs stability)
//
// 2. Use circuit breakers for backends with known failure patterns
//
// 3. Set RTO/RPO targets based on business requirements:
//   - Critical services: Use TierCritical
//   - Standard services: Use TierStandard
//   - Non-critical: Use TierBestEffort
//
// 4. Monitor health scores and set alert thresholds appropriately
//
// 5. Regularly run failover tests to validate recovery procedures
//
// 6. Keep backup retention policies aligned with RPO requirements
//
// # Metrics
//
// The HAMonitor exports these Prometheus metrics:
//   - vaultaire_ha_backends_total: Total number of backends
//   - vaultaire_ha_backends_healthy: Number of healthy backends
//   - vaultaire_ha_backends_unhealthy: Number of unhealthy backends
//   - vaultaire_ha_latency_avg_ms: Average latency in milliseconds
//   - vaultaire_ha_health_score: System health score (0-100)
//   - vaultaire_ha_uptime_seconds: System uptime in seconds
//
// # Thread Safety
//
// All components in this package are safe for concurrent use.
// Internal state is protected by sync.RWMutex where appropriate.
package ha
