package drivers

import (
	"sort"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// WP-R7-1: the region table must describe the reseller account (Review R7-01):
// 13 regions on s3.<region>.idrivee2.com, one key pair per region, the primary
// serving the default region, and no region accepted that no driver serves.

func TestIDriveRegions_MatchTheAccount(t *testing.T) {
	want := []string{
		"ap-northeast-1", "eu-central-1", "eu-south-1", "eu-west-1", "eu-west-3", "eu-west-4",
		"us-central-1", "us-east-1", "us-midwest-1", "us-southeast-1", "us-southwest-1", "us-west-2", "us-west-4",
	}
	var got []string
	for id, ep := range IDriveRegions {
		got = append(got, id)
		assert.Equal(t, "https://s3."+id+".idrivee2.com", ep, "every host is the account's regional S3 endpoint")
		assert.NotEqual(t, id, RegionDisplayName(id), "every region has a display name")
	}
	sort.Strings(got)
	assert.Equal(t, want, got)

	for _, gone := range []string{"us-west-1", "eu-central-2", "eu-west-2"} {
		assert.False(t, IsValidRegion(gone), "%s does not exist in the account", gone)
	}
	assert.True(t, IsEURegion("eu-west-1"))
	assert.False(t, IsEURegion("eu-west-3"), "London is the UK (RP78-4)")
	assert.False(t, IsEURegion("us-central-1"))
	assert.False(t, IsEURegion("ap-northeast-1"))
}

func TestIDriveDefaultRegion(t *testing.T) {
	assert.Equal(t, "us-central-1", IDriveDefaultRegion(func(string) string { return "" }), "prod's primary region")
	assert.Equal(t, "eu-west-1", IDriveDefaultRegion(func(k string) string {
		if k == "IDRIVE_REGION" {
			return " eu-west-1 "
		}
		return ""
	}))
	assert.True(t, IsValidRegion(IDriveFallbackRegion))
}

func TestIDriveRegionCredentials_DedicatedPairOnly(t *testing.T) {
	env := map[string]string{
		"IDRIVE_ACCESS_KEY":                "primary-ak",
		"IDRIVE_SECRET_KEY":                "primary-sk",
		"IDRIVE_US_WEST_2_ACCESS_KEY":      "la-ak",
		"IDRIVE_US_WEST_2_SECRET_KEY":      "la-sk",
		"IDRIVE_EU_WEST_1_ACCESS_KEY":      "ie-ak", // half a pair
		"IDRIVE_AP_NORTHEAST_1_SECRET_KEY": "tyo-sk",
	}
	getenv := func(k string) string { return env[k] }

	ak, sk := IDriveRegionCredentials(getenv, "us-west-2")
	assert.Equal(t, "la-ak", ak)
	assert.Equal(t, "la-sk", sk)

	// The primary pair answers 403 in every other region (live, R7-01): no fallback.
	for _, r := range []string{"eu-west-1", "ap-northeast-1", "us-east-1"} {
		ak, sk = IDriveRegionCredentials(getenv, r)
		assert.Empty(t, ak, r)
		assert.Empty(t, sk, r)
	}
}

func TestIDriveRegionEnvKey(t *testing.T) {
	assert.Equal(t, "IDRIVE_US_WEST_2_ACCESS_KEY", IDriveRegionEnvKey("us-west-2", "ACCESS_KEY"))
	assert.Equal(t, "IDRIVE_AP_NORTHEAST_1_SECRET_KEY", IDriveRegionEnvKey("ap-northeast-1", "SECRET_KEY"))
}

func TestIDriveRegionEndpoint_OverrideElseDefault(t *testing.T) {
	env := map[string]string{"IDRIVE_US_WEST_2_ENDPOINT": "https://example.test"}
	getenv := func(k string) string { return env[k] }
	assert.Equal(t, "https://example.test", IDriveRegionEndpoint(getenv, "us-west-2"))
	assert.Equal(t, "https://s3.eu-west-3.idrivee2.com", IDriveRegionEndpoint(getenv, "eu-west-3"))
	assert.Empty(t, IDriveRegionEndpoint(getenv, "us-west-1"), "unknown region has no endpoint")
}

func TestIDriveRegionGroups_CoverEveryRegionOnce(t *testing.T) {
	groups := IDriveRegionGroups()
	require.Len(t, groups, 4)
	assert.Equal(t, []string{"United States", "European Union", "United Kingdom", "Asia Pacific"},
		[]string{groups[0].Label, groups[1].Label, groups[2].Label, groups[3].Label}, "London is the UK's group (RP78-4)")
	seen := map[string]int{}
	for _, g := range groups {
		for i, r := range g.Regions {
			seen[r.ID]++
			assert.Equal(t, RegionDisplayName(r.ID), r.Name)
			if i > 0 {
				assert.Less(t, g.Regions[i-1].ID, r.ID, "sorted within the group")
			}
		}
	}
	assert.Len(t, seen, len(IDriveRegions))
	for id, n := range seen {
		assert.Equal(t, 1, n, id)
	}
}

func TestIDriveRegionAvailable_EnforcedOnlyOnceSet(t *testing.T) {
	ResetAvailableIDriveRegions()
	t.Cleanup(ResetAvailableIDriveRegions)

	assert.True(t, IDriveRegionAvailable("eu-west-3"), "unset = every table region (dev, tests)")
	assert.False(t, IDriveRegionAvailable("us-west-1"), "never a region the account lacks")

	SetAvailableIDriveRegions("us-central-1", []string{"us-west-2"})
	assert.True(t, IDriveRegionAvailable("us-central-1"), "the default region is served by the primary")
	assert.True(t, IDriveRegionAvailable("us-west-2"))
	assert.False(t, IDriveRegionAvailable("eu-west-3"), "no driver → a bucket here would silently land on the primary")

	SetAvailableIDriveRegions("us-central-1", nil)
	assert.False(t, IDriveRegionAvailable("us-west-2"))
}

// RP78-4: iDrive's eu-west-3 is London — the UK, not the EU. Each region of
// the table names its jurisdiction; a region outside it (Lyve's us-west-1)
// is its first segment's.
func TestIsEURegion_London(t *testing.T) {
	assert.False(t, IsEURegion("eu-west-3"), "London is not the EU")
	assert.Equal(t, "uk", RegionJurisdiction("eu-west-3"))
	for _, r := range []string{"eu-west-1", "eu-west-4", "eu-central-1", "eu-south-1"} {
		assert.True(t, IsEURegion(r), r)
		assert.Equal(t, "eu", RegionJurisdiction(r), r)
	}
	assert.Equal(t, "us", RegionJurisdiction("us-central-1"))
	assert.Equal(t, "jp", RegionJurisdiction("ap-northeast-1"))
	assert.Equal(t, "us", RegionJurisdiction("us-west-1"), "a region outside the table (lyve): its first segment")
	assert.Empty(t, RegionJurisdiction(""))
	for r := range IDriveRegions {
		assert.NotEmpty(t, regionJurisdictions[r], "every region of the table names its jurisdiction: %s", r)
	}
	assert.Equal(t, "uk", DataResidency("eu-west-3"))
	assert.Equal(t, "eu", DataResidency("eu-west-1"))
	assert.Equal(t, "us", DataResidency("us-central-1"))
}
