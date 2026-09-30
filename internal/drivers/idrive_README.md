# iDrive E2 Driver

## Configuration
- Endpoint: region-specific, from the `IDriveRegions` table in `idrive_regions.go` (`https://s3.<region>.idrivee2.com`, e.g. `https://s3.us-central-1.idrivee2.com`). Prod primary = `us-central-1` (Dallas); `IDRIVE_ENDPOINT` / `IDRIVE_REGION` override the primary, `IDRIVE_<REGION>_*` pairs enable additional regions (see root `CLAUDE.md`)
- Authentication: Access Key + Secret Key (one pair per region — the primary pair answers 403 elsewhere)
- Path Style: Required (unlike AWS S3)
- Bucket: one fixed bucket per region (`IDRIVE_BUCKET`, default `vaultaire`), tenant-prefixed keys

## Key Differences from AWS S3
1. Must use path-style URLs (UsePathStyle = true)
2. Different endpoint per region
3. No egress fees under 1GB/month per TB stored
4. Pricing: see `.private/IDRIVE_RESELLER_API.md` (the cost model in `dashboard/handlers/admin_costs.go` uses the annual-plan rate)

## Usage Example
```go
driver, err := NewIDriveDriver(
    os.Getenv("IDRIVE_ACCESS_KEY"),
    os.Getenv("IDRIVE_SECRET_KEY"),
    drivers.IDriveRegions["us-central-1"], // https://s3.us-central-1.idrivee2.com
    "us-central-1",
    logger,
)
```

## Testing

- Unit tests: `go test ./internal/drivers -run IDrive`
- Integration: requires real iDrive credentials
