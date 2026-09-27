package drivers

import (
	"sort"
	"strings"
	"sync"
)

// iDrive e2 regions as the reseller account actually has them (Review R7-01,
// WP-R7-1). Region ids and hosts come from the per-region endpoints the
// reseller API returns (`s3.<region>.idrivee2.com`); the previous table named
// eight AWS-style ids on `e2-<region>.idrive.com`, three of which do not exist
// and none of which serve this account's regions (live: the old host answered
// 404 for a bucket the real host served).
//
// One key pair per region: the reseller account mints credentials per region,
// so an `idrive-<region>` driver is registered only when IDRIVE_<REGION>_* is
// set (IDriveRegionCredentials returns nothing otherwise — the primary pair
// answers 403 in every other region).
var IDriveRegions = map[string]string{
	"us-central-1":   "https://s3.us-central-1.idrivee2.com",
	"us-west-2":      "https://s3.us-west-2.idrivee2.com",
	"us-west-4":      "https://s3.us-west-4.idrivee2.com",
	"us-southwest-1": "https://s3.us-southwest-1.idrivee2.com",
	"us-southeast-1": "https://s3.us-southeast-1.idrivee2.com",
	"us-midwest-1":   "https://s3.us-midwest-1.idrivee2.com",
	"us-east-1":      "https://s3.us-east-1.idrivee2.com",
	"eu-west-1":      "https://s3.eu-west-1.idrivee2.com",
	"eu-west-3":      "https://s3.eu-west-3.idrivee2.com",
	"eu-west-4":      "https://s3.eu-west-4.idrivee2.com",
	"eu-central-1":   "https://s3.eu-central-1.idrivee2.com",
	"eu-south-1":     "https://s3.eu-south-1.idrivee2.com",
	"ap-northeast-1": "https://s3.ap-northeast-1.idrivee2.com",
}

// Display names follow the reseller console's city per region (matched
// through the per-region key prefixes minted 2026-09-20: DA, LA, OR2, PH, MI,
// CH, VA, IE, LDN2, PAR, FRA2, MIL, TYO).
var regionDisplayNames = map[string]string{
	"us-central-1":   "US Central (Dallas)",
	"us-west-2":      "US West (Los Angeles)",
	"us-west-4":      "US West (Oregon)",
	"us-southwest-1": "US Southwest (Phoenix)",
	"us-southeast-1": "US Southeast (Miami)",
	"us-midwest-1":   "US Midwest (Chicago)",
	"us-east-1":      "US East (Virginia)",
	"eu-west-1":      "EU West (Ireland)",
	"eu-west-3":      "EU West (London)",
	"eu-west-4":      "EU West (Paris)",
	"eu-central-1":   "EU Central (Frankfurt)",
	"eu-south-1":     "EU South (Milan)",
	"ap-northeast-1": "Asia Pacific (Tokyo)",
}

// IDriveFallbackRegion is the primary's region when IDRIVE_REGION is unset:
// prod's primary key and bucket live in us-central-1 (Dallas).
const IDriveFallbackRegion = "us-central-1"

// IDriveDefaultRegion is the region a bucket gets when the client names none,
// and the region whose buckets are served by the PRIMARY `idrive` driver
// rather than an `idrive-<region>` driver: IDRIVE_REGION, else the fallback.
func IDriveDefaultRegion(getenv func(string) string) string {
	if r := strings.TrimSpace(getenv("IDRIVE_REGION")); r != "" {
		return r
	}
	return IDriveFallbackRegion
}

func IsValidRegion(region string) bool {
	_, ok := IDriveRegions[region]
	return ok
}

func IsEURegion(region string) bool {
	return strings.HasPrefix(region, "eu-")
}

func RegionDisplayName(region string) string {
	if name, ok := regionDisplayNames[region]; ok {
		return name
	}
	return region
}

// IDriveRegionOption is one selectable region (dashboard picker, docs).
type IDriveRegionOption struct {
	ID   string
	Name string
}

// IDriveRegionGroup groups regions by residency area for display.
type IDriveRegionGroup struct {
	Label   string
	Regions []IDriveRegionOption
}

// IDriveRegionGroups returns every region grouped US / EU / Asia Pacific,
// each group sorted by id — the single source for region pickers so the HTML
// cannot drift from the table again.
func IDriveRegionGroups() []IDriveRegionGroup {
	groups := map[string]*IDriveRegionGroup{
		"us": {Label: "United States"},
		"eu": {Label: "European Union"},
		"ap": {Label: "Asia Pacific"},
	}
	for id := range IDriveRegions {
		key := id[:2]
		g, ok := groups[key]
		if !ok {
			g = &IDriveRegionGroup{Label: strings.ToUpper(key)}
			groups[key] = g
		}
		g.Regions = append(g.Regions, IDriveRegionOption{ID: id, Name: RegionDisplayName(id)})
	}
	order := []string{"us", "eu", "ap"}
	var out []IDriveRegionGroup
	for _, key := range order {
		if g := groups[key]; g != nil && len(g.Regions) > 0 {
			sort.Slice(g.Regions, func(i, j int) bool { return g.Regions[i].ID < g.Regions[j].ID })
			out = append(out, *g)
		}
	}
	return out
}

// IDriveRegionEnvKey returns the env var name for a per-region override, e.g.
// IDriveRegionEnvKey("us-west-2", "ACCESS_KEY") = "IDRIVE_US_WEST_2_ACCESS_KEY".
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

// IDriveRegionCredentials returns the region's own key pair
// (IDRIVE_<REGION>_ACCESS_KEY / _SECRET_KEY, both required) or empty strings.
// There is deliberately NO fallback to the primary pair: the reseller account
// mints one pair per region and the primary pair answers 403 everywhere else
// (verified live, Review R7-01), so a region without its own pair must not be
// registered at all.
func IDriveRegionCredentials(getenv func(string) string, region string) (accessKey, secretKey string) {
	ak := getenv(IDriveRegionEnvKey(region, "ACCESS_KEY"))
	sk := getenv(IDriveRegionEnvKey(region, "SECRET_KEY"))
	if ak == "" || sk == "" {
		return "", ""
	}
	return ak, sk
}

// IDriveRegionEndpoint returns IDRIVE_<REGION>_ENDPOINT when set, else the
// default endpoint from IDriveRegions ("" for an unknown region).
func IDriveRegionEndpoint(getenv func(string) string, region string) string {
	if ep := getenv(IDriveRegionEnvKey(region, "ENDPOINT")); ep != "" {
		return ep
	}
	return IDriveRegions[region]
}

// Region availability: which regions this deployment can actually store in.
// main.go records the registered `idrive-<region>` drivers plus the default
// region after driver construction; CreateBucket (S3 and dashboard) refuses
// any other region instead of accepting a bucket whose objects would silently
// land on the primary (a data-residency breach). Until Set is called
// (tests, deployments without iDrive) every table region is allowed.
var (
	availableRegionsMu  sync.RWMutex
	availableRegions    map[string]bool
	availableRegionsSet bool
)

// SetAvailableIDriveRegions records the regions with a registered driver. The
// default region is always available (served by the primary).
func SetAvailableIDriveRegions(defaultRegion string, registered []string) {
	m := map[string]bool{defaultRegion: true}
	for _, r := range registered {
		m[r] = true
	}
	availableRegionsMu.Lock()
	availableRegions, availableRegionsSet = m, true
	availableRegionsMu.Unlock()
}

// ResetAvailableIDriveRegions returns to the unenforced state (tests).
func ResetAvailableIDriveRegions() {
	availableRegionsMu.Lock()
	availableRegions, availableRegionsSet = nil, false
	availableRegionsMu.Unlock()
}

// IDriveRegionAvailable reports whether a bucket may be created in region on
// this deployment.
func IDriveRegionAvailable(region string) bool {
	availableRegionsMu.RLock()
	defer availableRegionsMu.RUnlock()
	if !availableRegionsSet {
		return IsValidRegion(region)
	}
	return availableRegions[region]
}
