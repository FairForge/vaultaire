package drivers

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestIsValidRegion(t *testing.T) {
	valid := []string{"us-west-1", "us-east-1", "eu-west-1", "eu-central-2", "eu-south-1"}
	for _, r := range valid {
		assert.True(t, IsValidRegion(r), "expected %q to be valid", r)
	}

	invalid := []string{"", "us-north-1", "ap-southeast-1", "eu", "US-WEST-1"}
	for _, r := range invalid {
		assert.False(t, IsValidRegion(r), "expected %q to be invalid", r)
	}
}

func TestIsEURegion(t *testing.T) {
	eu := []string{"eu-west-1", "eu-central-2", "eu-west-2", "eu-south-1"}
	for _, r := range eu {
		assert.True(t, IsEURegion(r), "expected %q to be EU", r)
	}

	notEU := []string{"us-west-1", "us-east-1", "us-central-1", "", "e"}
	for _, r := range notEU {
		assert.False(t, IsEURegion(r), "expected %q to not be EU", r)
	}
}

func TestRegionDisplayName(t *testing.T) {
	assert.Equal(t, "US West (San Jose)", RegionDisplayName("us-west-1"))
	assert.Equal(t, "EU Central (Frankfurt)", RegionDisplayName("eu-central-2"))
	assert.Equal(t, "unknown-region", RegionDisplayName("unknown-region"))
}

func TestIDriveRegions_AllHaveEndpoints(t *testing.T) {
	for region, endpoint := range IDriveRegions {
		assert.NotEmpty(t, endpoint, "region %q has empty endpoint", region)
		assert.Contains(t, endpoint, "idrive.com", "region %q endpoint should be idrive.com", region)
	}
	assert.True(t, len(IDriveRegions) >= 8, "expected at least 8 regions")
}
