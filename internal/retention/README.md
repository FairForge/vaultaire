Retention Package
Backend-aware data retention policies for GDPR Article 5(1)(e) compliance.
Features

Backend-Specific Policies: Target specific backends (Lyve, S3, OneDrive)
Container-Specific Policies: Apply to specific buckets/containers
Native Feature Toggle: Use backend's object lock when available
Legal Holds: Prevent deletion during investigations
Cleanup Jobs: Automated enforcement with dry-run mode

Usage
Create Backend-Specific Policy
gopolicy := &RetentionPolicy{
    Name:                 "Lyve Compliance Storage",
    BackendID:            "lyve-us-east-1",
    ContainerName:        "compliance-data",
    RetentionPeriod:      7 * 365 * 24 * time.Hour,
    UseBackendObjectLock: true, // Use Lyve's native object lock
}

service.CreatePolicy(ctx, policy)
Create Global Policy
gopolicy := &RetentionPolicy{
    Name:            "Temp File Cleanup",
    DataCategory:    CategoryTempFiles,
    RetentionPeriod: 7 * 24 * time.Hour,
    // BackendID empty = applies to all backends
}
Legal Hold
gohold := &LegalHold{
    UserID:          userID,
    Reason:          "Litigation pending",
    ApplyObjectLock: true, // Use S3 Object Lock if available
}

holdService.CreateHold(ctx, hold)
How It Works

Capability Detection: System checks if backend supports object lock
Native First: Uses backend's native feature if available
Fallback: Falls back to Vaultaire enforcement if not
Transparency: Logs which method is used

Database Schema
See internal/database/migrations/010_retention_policies.sql
