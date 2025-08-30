package drivers

import (
	"testing"
)

func TestLyveDriver_TenantIsolation(t *testing.T) {
	driver := &LyveDriver{
		tenantID: "abc123",
		region:   "us-east-1",
	}

	testCases := []struct {
		name     string
		artifact string
		expected string
	}{
		{"simple file", "file.txt", "t-abc123/file.txt"},
		{"nested path", "photos/2024/img.jpg", "t-abc123/photos/2024/img.jpg"},
		{"root file", "data.json", "t-abc123/data.json"},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			key := driver.buildTenantKey(tc.artifact)
			if key != tc.expected {
				t.Errorf("Expected %s, got %s - TENANT ISOLATION BROKEN", tc.expected, key)
			}
		})
	}
}

func TestLyveDriver_RegionBucketMapping(t *testing.T) {
	testCases := []struct {
		region string
		bucket string
	}{
		{"us-east-1", "stored-us-east-1"},
		{"us-west-1", "stored-us-west-1"},
		{"ap-southeast-1", "stored-ap-southeast-1"},
	}

	for _, tc := range testCases {
		t.Run(tc.region, func(t *testing.T) {
			driver := &LyveDriver{region: tc.region}
			if driver.getBucket() != tc.bucket {
				t.Errorf("Region %s should use bucket %s", tc.region, tc.bucket)
			}
		})
	}
}
