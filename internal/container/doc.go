// internal/container/doc.go
// Package container provides containerization primitives for the Vaultaire platform.
//
// This package implements comprehensive container management capabilities including
// Dockerfile generation, multi-stage builds, security scanning, registry operations,
// orchestration, networking, storage, monitoring, and debugging.
//
// # Architecture Overview
//
// The container package is organized into several subsystems:
//
//   - Dockerfile Generation: Build optimized container images
//   - Multi-Stage Builds: Complex build pipelines with dependency resolution
//   - Security Scanning: Vulnerability detection and policy enforcement
//   - Registry Operations: Image management across multiple registries
//   - Orchestration: Kubernetes-style deployment management
//   - Networking: Container network configuration and policies
//   - Storage: Volume and persistent storage management
//   - Monitoring: Resource metrics and health checking
//   - Debugging: Exec, attach, logs, and inspection
//
// # Dockerfile Generation
//
// Use DockerfileBuilder for simple Dockerfiles:
//
//	config := &DockerfileConfig{
//	    BaseImage: "golang:1.21-alpine",
//	    AppName:   "vaultaire",
//	    Port:      8080,
//	}
//	builder := NewDockerfileBuilder(config)
//	dockerfile := builder.Build()
//
// For production images with security hardening:
//
//	dockerfile := GenerateProductionDockerfile(&DockerfileConfig{
//	    BaseImage: "golang:1.21-alpine",
//	    AppName:   "vaultaire",
//	    Port:      8080,
//	})
//
// # Multi-Stage Builds
//
// Use MultiStageBuilder for complex build pipelines:
//
//	builder := NewMultiStageBuilder()
//	builder.AddStage(&BuildStage{
//	    Name:      "builder",
//	    BaseImage: "golang:1.21",
//	    Commands:  []string{"go build -o /app ./cmd/server"},
//	})
//	builder.AddStage(&BuildStage{
//	    Name:       "runtime",
//	    BaseImage:  "gcr.io/distroless/static",
//	    CopyFrom:   []StageCopy{{From: "builder", Src: "/app", Dst: "/app"}},
//	    Entrypoint: []string{"/app"},
//	})
//	dockerfile, _ := builder.Build()
//
// # Security Scanning
//
// Scan images for vulnerabilities:
//
//	scanner := NewImageScanner(&ScannerConfig{Scanner: "trivy"})
//	result, _ := scanner.Scan(ctx, "myimage:latest")
//
//	policy := DefaultScanPolicy()
//	passed, violations := policy.Evaluate(result)
//
// # Registry Operations
//
// Interact with container registries:
//
//	client := NewRegistryClient(GHCRConfig("token"))
//	tags, _ := client.ListTags(ctx, "fairforge/vaultaire")
//	manifest, _ := client.GetManifest(ctx, "fairforge/vaultaire", "latest")
//
// # Orchestration
//
// Deploy and manage containerized applications:
//
//	orch := NewOrchestrator(&OrchestratorConfig{Provider: "kubernetes"})
//	orch.Deploy(ctx, &DeploymentSpec{
//	    Name:     "vaultaire",
//	    Replicas: 3,
//	    Template: &ContainerSpec{
//	        Name:  "vaultaire",
//	        Image: "ghcr.io/fairforge/vaultaire:1.0.0",
//	    },
//	})
//	orch.Scale(ctx, "default", "vaultaire", 5)
//
// # Networking
//
// Configure container networking:
//
//	mgr := NewNetworkManager(&NetworkManagerConfig{Provider: "docker"})
//	mgr.CreateNetwork(&NetworkConfig{
//	    Name:   "app-network",
//	    Driver: NetworkDriverBridge,
//	    Subnet: "172.20.0.0/16",
//	})
//
// # Storage
//
// Manage container volumes:
//
//	mgr := NewVolumeManager(&VolumeManagerConfig{Provider: "docker"})
//	mgr.CreateVolume(&VolumeConfig{Name: "data-volume"})
//
// Parse Kubernetes-style storage sizes:
//
//	bytes, _ := ParseStorageSize("10Gi")  // Returns 10737418240
//	human := FormatStorageSize(bytes)      // Returns "10Gi"
//
// # Monitoring
//
// Collect container metrics:
//
//	collector := NewMetricsCollector(&MetricsCollectorConfig{Provider: "docker"})
//	metrics, _ := collector.Collect(ctx, "container-123")
//	fmt.Printf("CPU: %.2f%%, Memory: %.2f%%\n", metrics.CPUPercent, metrics.MemoryPercent)
//
// Stream metrics in real-time:
//
//	ch, _ := collector.Stream(ctx, "container-123")
//	for metrics := range ch {
//	    // Process metrics
//	}
//
// # Debugging
//
// Execute commands in containers:
//
//	debugger := NewDebugger(&DebuggerConfig{Provider: "docker"})
//	result, _ := debugger.Exec(ctx, &ExecConfig{
//	    ContainerID: "abc123",
//	    Command:     []string{"ls", "-la"},
//	})
//
// Inspect container details:
//
//	info, _ := debugger.Inspect(ctx, "container-123")
//	fmt.Printf("State: %s, IP: %s\n", info.State.Status, info.NetworkSettings.IPAddress)
//
// # Thread Safety
//
// All manager types (NetworkManager, VolumeManager, MetricsCollector, Debugger)
// are safe for concurrent use from multiple goroutines.
//
// # Provider Abstraction
//
// Most components support a Provider configuration that determines the underlying
// implementation. Use "mock" for testing or the appropriate provider name
// ("docker", "kubernetes", etc.) for production use.
package container
