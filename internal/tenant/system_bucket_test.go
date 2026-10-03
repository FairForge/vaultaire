package tenant

import (
	"regexp"
	"testing"

	"github.com/stretchr/testify/assert"
)

// A system bucket (WP-R10-3b) lives in the tenant's own namespace but is the
// service's: its name starts with '_', which the S3 bucket-name grammar never
// allows, so no customer can create or collide with one.
func TestSystemBucket_NameIsNotAnS3BucketName(t *testing.T) {
	s3Name := regexp.MustCompile(`^[a-z0-9][a-z0-9.\-]{1,61}[a-z0-9]$`) // api.s3BucketNameRe
	assert.False(t, s3Name.MatchString(ExportsBucket), "a customer must never be able to create %q", ExportsBucket)
	assert.True(t, IsSystemBucket(ExportsBucket))
	assert.True(t, IsSystemBucket("_anything"))
	assert.False(t, IsSystemBucket("exports"))
	assert.False(t, IsSystemBucket("my-bucket"))
	assert.False(t, IsSystemBucket(""))
}
