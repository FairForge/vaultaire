package drivers

import (
	"go.uber.org/zap"
)

// S3Driver implements storage.Backend for S3-compatible storage
type S3Driver struct {
	endpoint  string
	accessKey string
	secretKey string
	region    string
	logger    *zap.Logger
}

// NewS3Driver creates a new S3 storage driver
func NewS3Driver(endpoint, accessKey, secretKey, region string, logger *zap.Logger) (*S3Driver, error) {
	return &S3Driver{
		endpoint:  endpoint,
		accessKey: accessKey,
		secretKey: secretKey,
		region:    region,
		logger:    logger,
	}, nil
}
