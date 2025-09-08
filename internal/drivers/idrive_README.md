# iDrive E2 Driver

## Configuration
- Endpoint: Region-specific (e.g., https://e2-us-west-1.idrive.com)
- Authentication: Access Key + Secret Key
- Path Style: Required (unlike AWS S3)

## Key Differences from AWS S3
1. Must use path-style URLs (UsePathStyle = true)
2. Different endpoint per region
3. No egress fees under 1GB/month per TB stored
4. Pricing: $0.004/GB/month

## Usage Example
```go
driver, err := NewIDriveDriver(
    os.Getenv("IDRIVE_ACCESS_KEY"),
    os.Getenv("IDRIVE_SECRET_KEY"),
    "https://e2-us-west-1.idrive.com",
    "us-west-1",
    logger,
)
Testing

Unit tests: go test ./internal/drivers -run IDrive
Integration: Requires real iDrive credentials
