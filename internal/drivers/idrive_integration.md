# iDrive E2 Storage Driver

## Overview

The iDrive E2 driver provides S3-compatible storage with advanced features for cost optimization and high availability.

## Features

- ✅ S3-compatible API (Step 161)
- ✅ Authentication validation (Step 162)
- ✅ Multipart uploads for large files (Step 163)
- ✅ Egress cost tracking per tenant (Step 164)
- ✅ Bandwidth quotas with monthly reset (Step 165)
- ✅ Usage prediction and alerts (Step 166)
- ✅ Smart caching for egress reduction (Step 167)
- ✅ Cost optimization advisor (Step 168)
- ✅ Regional failover for high availability (Step 169)

## Configuration

### Environment Variables

```bash
export IDRIVE_ACCESS_KEY="your-access-key"
export IDRIVE_SECRET_KEY="your-secret-key"
export IDRIVE_ENDPOINT="https://e2-us-west-1.idrive.com"
export IDRIVE_REGION="us-west-1"
Basic Usage
go// Create driver from environment
driver, err := NewIDriveDriverFromConfig(logger)

// Or with explicit config
driver, err := NewIDriveDriver(
    accessKey,
    secretKey,
    endpoint,
    region,
    logger,
)

// Validate authentication
err = driver.ValidateAuth(ctx)

// Basic operations
reader, err := driver.Get(ctx, "bucket", "file.txt")
err = driver.Put(ctx, "bucket", "file.txt", data)
err = driver.Delete(ctx, "bucket", "file.txt")
exists, err := driver.Exists(ctx, "bucket", "file.txt")
files, err := driver.List(ctx, "bucket", "prefix/")
Advanced Features
Egress Tracking
gotracker := NewEgressTracker()
driver.SetEgressTracker(tracker)

// Get usage and cost
usage := tracker.GetTenantEgress("tenant-1")
cost := tracker.GetTenantCost("tenant-1")
Bandwidth Quotas
goquota := NewBandwidthQuota(10 * 1024 * 1024 * 1024) // 10GB
allowed := quota.AllowEgress("tenant-1", bytes)
remaining := quota.GetRemaining("tenant-1")
Usage Prediction
gopredictor := NewEgressPredictor()
predictor.SetQuota("tenant-1", quotaBytes)
predicted := predictor.PredictMonthlyUsage("tenant-1", time.Now())
alert := predictor.CheckAlert("tenant-1", currentUsage)
Smart Caching
gocache := NewSmartCache(100 * 1024 * 1024) // 100MB cache
data := cache.Get("tenant-1", "file.txt")
cache.Put("tenant-1", "file.txt", data)
stats := cache.GetStats()
Cost Advisor
goadvisor := NewCostAdvisor()
advisor.RecordUpload("tenant-1", "file.txt", size, contentType)
recommendations := advisor.GetRecommendations("tenant-1")
Regional Failover
goprimary := NewIDriveDriver(...)   // US West
secondary := NewIDriveDriver(...) // US East
failover := NewRegionalFailover(primary, secondary, logger)

// Use failover wrapper for automatic HA
reader, err := failover.Get(ctx, "bucket", "file.txt")
Cost Optimization
Storage Costs

Standard: $0.009/GB/month
Egress: $0.009/GB

Optimization Strategies

Enable Compression: 60% reduction for text/JSON
Use Caching: Reduce egress by 40-60%
Archive Old Data: Move to cheaper tiers
Set Quotas: Prevent unexpected costs
Monitor Usage: Track patterns and optimize

Testing
bash# Unit tests
go test ./internal/drivers -run IDrive -v

# Integration tests (requires credentials)
export IDRIVE_ACCESS_KEY="test-key"
export IDRIVE_SECRET_KEY="test-secret"
go test ./internal/drivers -run IDrive.*Integration -v
Performance

Multipart threshold: 5MB (configurable)
Part size: 5MB (optimal for iDrive)
Cache size: Based on available memory
Failover recovery: 30 seconds

Monitoring

Health checks every 30 seconds
Egress tracking in real-time
Alert thresholds: 50%, 75%, 90%
Cache hit ratio tracking

```
