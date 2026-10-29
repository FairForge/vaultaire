package dashboard

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// RP76-1: an EU bucket is never mirrored outside the EU, and the EU has no
// second fast leg yet — the bucket page says it is a single copy.
func TestBucketSettings_AnEUBucketSaysItIsASingleCopy(t *testing.T) {
	eu := renderCustomer(t, "templates/customer/bucket_settings.html",
		map[string]any{"Page": "buckets", "BucketName": "eu", "Region": "eu-west-1", "IsEURegion": true, "SingleCopyJurisdiction": "the EU", "DataResidency": "eu"})
	assert.Contains(t, eu, "a single copy")
	us := renderCustomer(t, "templates/customer/bucket_settings.html",
		map[string]any{"Page": "buckets", "BucketName": "us", "Region": "us-central-1", "DataResidency": "us"})
	assert.NotContains(t, us, "a single copy")
}

// RP78-4: a London bucket is the UK's: its residency label and its
// single-copy sentence name the UK, never the EU.
func TestBucketSettings_AUKBucketSaysTheUK(t *testing.T) {
	uk := renderCustomer(t, "templates/customer/bucket_settings.html",
		map[string]any{"Page": "buckets", "BucketName": "ldn", "Region": "eu-west-3", "SingleCopyJurisdiction": "the UK", "DataResidency": "uk"})
	assert.Contains(t, uk, "UK Data Residency")
	assert.Contains(t, uk, "never copied outside the UK")
	assert.Contains(t, uk, "a single copy")
	assert.NotContains(t, uk, "EU Data Residency")
	assert.NotContains(t, uk, "outside the EU")
	eu := renderCustomer(t, "templates/customer/bucket_settings.html",
		map[string]any{"Page": "buckets", "BucketName": "eu", "Region": "eu-west-1", "SingleCopyJurisdiction": "the EU", "DataResidency": "eu"})
	assert.Contains(t, eu, "never copied outside the EU")
}
