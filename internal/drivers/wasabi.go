package drivers

import (
	"fmt"

	"go.uber.org/zap"
)

// Wasabi is the interim Standard-tier primary (2026-10-03, owner decision:
// the iDrive prod key answers 403 on every object call while the account is
// repaired, and the partner account is free). It is the same shape as iDrive
// — one fixed bucket, `t-<tenant>/<container>/<artifact>` keys, SigV4 over
// HTTP/1.1 — so it IS the fixed-bucket driver under another name: rows it
// writes say `backend_name = 'wasabi'`, its errors and metrics say "wasabi".
//
// Wasabi facts that matter here (2026-10-03): $7.99/TB-month list since
// 2026-07-01 with a 90-day minimum storage charge per object and no egress
// or API fees. Deleting an object does not stop its bill for 90 days — the
// partner account is not invoiced, so the erasure sweep and dedup GC are
// unaffected, but a paid account would want the minimum charge in COGS.

const (
	// WasabiDefaultRegion is Wasabi's Oregon region, the closest to the SLC hub.
	WasabiDefaultRegion = "us-west-1"
	// WasabiDefaultBucket is the one fixed bucket every object lands in.
	WasabiDefaultBucket = "vaultaire"
)

// WasabiEndpoint is the service endpoint of a Wasabi region.
func WasabiEndpoint(region string) string {
	return fmt.Sprintf("https://s3.%s.wasabisys.com", region)
}

// WasabiConfigFromEnv reads WASABI_REGION / WASABI_ENDPOINT / WASABI_BUCKET
// with their defaults; the caller passes the key pair.
func WasabiConfigFromEnv(getenv func(string) string) (endpoint, region, bucket string) {
	region = getenv("WASABI_REGION")
	if region == "" {
		region = WasabiDefaultRegion
	}
	endpoint = getenv("WASABI_ENDPOINT")
	if endpoint == "" {
		endpoint = WasabiEndpoint(region)
	}
	bucket = getenv("WASABI_BUCKET")
	if bucket == "" {
		bucket = WasabiDefaultBucket
	}
	return endpoint, region, bucket
}

// NewWasabiDriver creates the `wasabi` driver: the fixed-bucket S3 driver
// against a Wasabi regional endpoint. EnsureBucket creates `bucket` in the
// endpoint's region on first boot, like the iDrive region drivers.
func NewWasabiDriver(accessKey, secretKey, endpoint, region, bucket string, logger *zap.Logger) (*IDriveDriver, error) {
	return NewFixedBucketS3Driver("wasabi", accessKey, secretKey, endpoint, region, bucket, logger)
}
