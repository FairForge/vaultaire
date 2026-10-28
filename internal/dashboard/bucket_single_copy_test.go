package dashboard

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// RP76-1: an EU bucket is never mirrored outside the EU, and the EU has no
// second fast leg yet — the bucket page says it is a single copy.
func TestBucketSettings_AnEUBucketSaysItIsASingleCopy(t *testing.T) {
	eu := renderCustomer(t, "templates/customer/bucket_settings.html",
		map[string]any{"Page": "buckets", "BucketName": "eu", "Region": "eu-west-1", "IsEURegion": true, "DataResidency": "eu"})
	assert.Contains(t, eu, "a single copy")
	us := renderCustomer(t, "templates/customer/bucket_settings.html",
		map[string]any{"Page": "buckets", "BucketName": "us", "Region": "us-central-1", "DataResidency": "us"})
	assert.NotContains(t, us, "a single copy")
}
