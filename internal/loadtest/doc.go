// Package loadtest provides a comprehensive infrastructure for performance
// and reliability testing of the Vaultaire storage platform.
//
// # Overview
//
// This package implements multiple testing strategies for validating system
// behavior under various conditions:
//
//   - Load Testing: Baseline performance measurement at expected traffic levels
//   - Stress Testing: Finding system breaking points with progressive load increase
//   - Spike Testing: Simulating sudden traffic bursts and recovery behavior
//   - Soak Testing: Extended duration testing for resource leaks and degradation
//   - Chaos Testing: Intentional failure injection to validate resilience
//
// # Quick Start
//
// Basic load test:
//
//	config := loadtest.DefaultConfig("api-test")
//	config.Duration = 5 * time.Minute
//	config.TargetRPS = 100
//
//	framework := loadtest.NewFramework(config, func(ctx context.Context, id int) loadtest.Result {
//	    // Your test logic here
//	    resp, err := http.Get("http://localhost:8080/api/health")
//	    return loadtest.Result{
//	        StartTime:  time.Now(),
//	        Duration:   time.Since(start),
//	        StatusCode: resp.StatusCode,
//	        Error:      err,
//	    }
//	})
//
//	summary, err := framework.Run(ctx)
//
// # Test Types
//
// ## Load Testing (Framework)
//
// The Framework type provides basic load testing with configurable RPS,
// concurrency, and duration. Use this for establishing performance baselines.
//
//	config := loadtest.DefaultConfig("baseline")
//	config.TargetRPS = 50
//	config.Duration = 10 * time.Minute
//
// ## Stress Testing (StressTester)
//
// StressTester progressively increases load to find breaking points:
//
//	config := loadtest.DefaultStressConfig("stress")
//	config.StartRPS = 10
//	config.MaxRPS = 500
//	config.RampUpRate = 10  // Increase by 10 RPS per interval
//	config.FailureThreshold = 0.05  // Stop at 5% error rate
//
//	tester := loadtest.NewStressTester(config, workerFunc)
//	result, _ := tester.Run(ctx)
//	fmt.Printf("Breaking point: %d RPS\n", result.BreakingPointRPS)
//
// ## Spike Testing (SpikeTester)
//
// SpikeTester simulates sudden traffic bursts:
//
//	config := loadtest.DefaultSpikeConfig("spike")
//	config.BaselineRPS = 50
//	config.SpikeRPS = 500
//	config.NumSpikes = 3
//
//	tester := loadtest.NewSpikeTester(config, workerFunc)
//	result, _ := tester.Run(ctx)
//	fmt.Printf("System recovered: %v\n", result.SystemRecovered)
//
// ## Soak Testing (SoakTester)
//
// SoakTester runs extended tests to detect resource leaks:
//
//	config := loadtest.DefaultSoakConfig("soak")
//	config.Duration = 4 * time.Hour
//	config.TargetRPS = 50
//	config.MemoryThreshold = 2 * 1024 * 1024 * 1024  // 2GB
//
//	tester := loadtest.NewSoakTester(config, workerFunc)
//	result, _ := tester.Run(ctx)
//	fmt.Printf("Memory growth: %.2f%%\n", result.MemoryGrowth)
//
// ## Chaos Testing (ChaosTester)
//
// ChaosTester injects failures to validate resilience:
//
//	config := loadtest.DefaultChaosConfig("chaos")
//	config.ChaosTypes = []loadtest.ChaosType{
//	    loadtest.ChaosLatency,
//	    loadtest.ChaosError,
//	}
//	config.ChaosProbability = 0.1  // 10% of requests affected
//
//	tester := loadtest.NewChaosTester(config, workerFunc)
//	result, _ := tester.Run(ctx)
//	fmt.Printf("Resilience score: %.0f/100\n", result.ResilienceScore)
//
// # Analysis Tools
//
// ## Performance Baselines
//
// Track and compare performance across releases:
//
//	manager := loadtest.NewBaselineManager("/path/to/baselines")
//	baseline := manager.CreateBaseline("v1.0", "Initial release", "prod", "1.0.0", summary)
//	manager.SaveToFile("v1.0")
//
//	// Later, compare new results
//	comparison, _ := manager.Compare("v1.0", newSummary)
//	if comparison.OverallStatus == loadtest.StatusRegression {
//	    fmt.Println("Performance regression detected!")
//	}
//
// ## Bottleneck Analysis
//
// Identify performance issues automatically:
//
//	analyzer := loadtest.NewBottleneckAnalyzer(nil)  // Uses defaults
//	analysis := analyzer.AnalyzeLoadTest(summary)
//
//	fmt.Printf("Health score: %.0f/100\n", analysis.HealthScore)
//	for _, b := range analysis.Bottlenecks {
//	    fmt.Printf("[%s] %s: %s\n", b.Severity, b.Type, b.Description)
//	}
//
// ## Capacity Planning
//
// Estimate resource requirements:
//
//	model := loadtest.BuildModelFromStress("api", stressResult)
//	planner := loadtest.NewCapacityPlanner(model)
//
//	estimate := planner.EstimateCapacity(1000)  // Target 1000 RPS
//	fmt.Printf("Required instances: %d\n", estimate.RequiredInstances)
//	fmt.Printf("Estimated latency: %v\n", estimate.EstimatedLatency)
//
//	// Plan for growth
//	plans := planner.PlanForGrowth(100, 20, 12)  // 20% monthly growth, 12 months
//
// # Best Practices
//
// 1. Always run load tests in an environment similar to production
// 2. Establish baselines before making changes
// 3. Use stress tests to find breaking points, not as regular tests
// 4. Run soak tests for at least 4 hours to detect slow leaks
// 5. Compare results against baselines after each release
// 6. Set realistic SLOs and use them as test thresholds
//
// # Metrics
//
// All test types produce a Summary with standard metrics:
//
//   - RequestsPerSec: Achieved throughput
//   - ErrorRate: Percentage of failed requests
//   - AvgLatency, P50, P95, P99, MaxLatency: Latency distribution
//   - TotalRequests, SuccessCount, FailureCount: Request counts
//
// Additional test-specific metrics are available in each result type.
package loadtest
