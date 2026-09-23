package drivers

// IDriveRegions maps iDrive e2 region identifiers to their S3-compatible endpoints.
var IDriveRegions = map[string]string{
	"us-west-1":    "https://e2-us-west-1.idrive.com",
	"us-west-2":    "https://e2-us-west-2.idrive.com",
	"us-central-1": "https://e2-us-central-1.idrive.com",
	"us-east-1":    "https://e2-us-east-1.idrive.com",
	"eu-west-1":    "https://e2-eu-west-1.idrive.com",
	"eu-central-2": "https://e2-eu-central-2.idrive.com",
	"eu-west-2":    "https://e2-eu-west-2.idrive.com",
	"eu-south-1":   "https://e2-eu-south-1.idrive.com",
}

var regionDisplayNames = map[string]string{
	"us-west-1":    "US West (San Jose)",
	"us-west-2":    "US West (Dallas)",
	"us-central-1": "US Central (Chicago)",
	"us-east-1":    "US East (Virginia)",
	"eu-west-1":    "EU West (Ireland)",
	"eu-central-2": "EU Central (Frankfurt)",
	"eu-west-2":    "EU West (London)",
	"eu-south-1":   "EU South (Milan)",
}

func IsValidRegion(region string) bool {
	_, ok := IDriveRegions[region]
	return ok
}

func IsEURegion(region string) bool {
	if len(region) < 3 {
		return false
	}
	return region[:3] == "eu-"
}

func RegionDisplayName(region string) string {
	if name, ok := regionDisplayNames[region]; ok {
		return name
	}
	return region
}

// IDriveRegionEnvKey returns the env var name for a per-region override, e.g.
// IDriveRegionEnvKey("us-central-1", "ACCESS_KEY") = "IDRIVE_US_CENTRAL_1_ACCESS_KEY".
func IDriveRegionEnvKey(region, suffix string) string {
	upper := make([]byte, 0, len(region))
	for i := 0; i < len(region); i++ {
		c := region[i]
		switch {
		case c == '-':
			upper = append(upper, '_')
		case c >= 'a' && c <= 'z':
			upper = append(upper, c-'a'+'A')
		default:
			upper = append(upper, c)
		}
	}
	return "IDRIVE_" + string(upper) + "_" + suffix
}

// IDriveRegionCredentials returns the key pair for a region: the per-region
// override (IDRIVE_<REGION>_ACCESS_KEY / _SECRET_KEY, both required) when set,
// else the primary IDRIVE_ACCESS_KEY / IDRIVE_SECRET_KEY pair.
//
// The reseller account mints one key pair per region, so without overrides
// every idrive-<region> driver runs on the primary region's key and 403s.
func IDriveRegionCredentials(getenv func(string) string, region string) (accessKey, secretKey string) {
	ak := getenv(IDriveRegionEnvKey(region, "ACCESS_KEY"))
	sk := getenv(IDriveRegionEnvKey(region, "SECRET_KEY"))
	if ak != "" && sk != "" {
		return ak, sk
	}
	return getenv("IDRIVE_ACCESS_KEY"), getenv("IDRIVE_SECRET_KEY")
}

// IDriveRegionEndpoint returns IDRIVE_<REGION>_ENDPOINT when set, else the
// default endpoint from IDriveRegions.
func IDriveRegionEndpoint(getenv func(string) string, region string) string {
	if ep := getenv(IDriveRegionEnvKey(region, "ENDPOINT")); ep != "" {
		return ep
	}
	return IDriveRegions[region]
}
