package drivers

import (
	"os"

	"go.uber.org/zap"
)

// Sia is the Sia decentralized storage network reached through s3d
// (github.com/SiaFoundation/s3d), the Sia Foundation's S3 gateway: a local
// daemon that speaks SigV4 on localhost, encrypts client-side and erasure-
// codes every object 10-of-30 across Sia hosts. It is the same shape as
// iDrive/Wasabi — one fixed bucket, `t-<tenant>/<container>/<artifact>` keys —
// so it IS the fixed-bucket driver under the name `sia`.
//
// s3d facts that matter here (2026-10-21, v0.2.1): a PUT 200 means the object
// is buffered on s3d's local disk (10 GiB default), uploaded to Sia by a
// background loop or `s3d flush` — durability on Sia lags the 200 like the
// Sync bridge's cloud upload. The gateway is paid for by the sia.storage
// account s3d is registered to (Pro $14.99/5 TB ≈ $3/TB-mo, Unlimited
// $19.99/10 TB ≈ $2/TB-mo as of 2026-10). Measured from SLC (free plan):
// flush ~137 MB/s logical to Sia, reads ~85–107 MB/s back. Bench + setup:
// bench-results/SIA-S3D-2026-10.md (the lab lives in ~/s3d-lab on the box;
// ports 9000/9001 — 8000/8001 are the vaultaire slots).
const (
	// SiaDefaultEndpoint is where s3d serves its S3 API — localhost only;
	// the daemon runs on the same box.
	SiaDefaultEndpoint = "http://127.0.0.1:9000"
	// SiaDefaultBucket is the one fixed bucket every object lands in.
	SiaDefaultBucket = "vaultaire"
	// siaSigningRegion is the SigV4 region s3d accepts; it has no regions.
	siaSigningRegion = "us-east-1"
)

// SiaConfigFromEnv reads SIA_ENDPOINT / SIA_BUCKET with their defaults; the
// caller passes the key pair (SIA_ACCESS_KEY / SIA_SECRET_KEY, minted by
// `s3d keys create`).
func SiaConfigFromEnv(getenv func(string) string) (endpoint, bucket string) {
	endpoint = getenv("SIA_ENDPOINT")
	if endpoint == "" {
		endpoint = SiaDefaultEndpoint
	}
	bucket = getenv("SIA_BUCKET")
	if bucket == "" {
		bucket = SiaDefaultBucket
	}
	return endpoint, bucket
}

// NewSiaDriver creates the `sia` driver: the fixed-bucket S3 driver against a
// local s3d gateway. EnsureBucket creates `bucket` on first boot, like the
// iDrive region drivers.
func NewSiaDriver(accessKey, secretKey, endpoint, bucket string, logger *zap.Logger) (*IDriveDriver, error) {
	d, err := NewFixedBucketS3Driver("sia", accessKey, secretKey, endpoint, siaSigningRegion, bucket, logger)
	if err != nil {
		return nil, err
	}
	// A cut body resumed, reads never hedged, optional pacing (sia_read.go:
	// SIA_READ_RESUMES, SIA_READ_RESUME_MAX_WAIT, SIA_READ_BUDGET).
	d.setSiaRead(siaReadConfigFromEnv(os.Getenv, logger))
	return d, nil
}
