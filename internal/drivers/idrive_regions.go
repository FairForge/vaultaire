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
