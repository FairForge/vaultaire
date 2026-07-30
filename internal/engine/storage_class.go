package engine

// These keys are OUR S3 API's storage-class names — what a client sends us in
// x-amz-storage-class — and the values pick which backend driver handles the
// object. They are not the backend's own storage class: no driver sets
// StorageClass on its upstream request, so an object routed here is stored at
// whatever class that backend defaults to.
//
// STANDARD_IA is deliberately absent. We do not sell an infrequent-access tier
// (decision 2026-07-29) and IMPLEMENTATION_PLAN.md:871 forbids routing customer
// STANDARD_IA to Lyve. A client sending it gets the primary backend at
// STANDARD, the same as any unrecognized class.
//
// Note this is distinct from Seagate's own Lyve Infrequent Access service tier
// (180-day minimum retention, 128 KB minimum object size, retrieval caps — see
// internal/drivers/lyve_README.md). We have never used that: the old mapping
// sent objects to the Lyve *backend* at its default class, not to Lyve IA.
var storageClassToBackend = map[string]string{
	"STANDARD":           "idrive",
	"GLACIER":            "geyser",
	"DEEP_ARCHIVE":       "geyser",
	"REDUCED_REDUNDANCY": "local",
}

var backendToStorageClass = map[string]string{
	"idrive":     "STANDARD",
	"lyve":       "STANDARD",
	"geyser":     "GLACIER",
	"permafrost": "STANDARD",
	"local":      "REDUCED_REDUNDANCY",
	"s3":         "STANDARD",
}

func ResolveStorageClass(class string, primaryBackend string, availableDrivers map[string]Driver) (driverName, resolvedClass string) {
	if class == "" {
		return primaryBackend, "STANDARD"
	}

	canonical := class
	targetBackend, mapped := storageClassToBackend[class]
	if !mapped {
		return primaryBackend, "STANDARD"
	}

	if _, available := availableDrivers[targetBackend]; available {
		return targetBackend, canonical
	}

	return primaryBackend, canonical
}

func BackendToStorageClass(backendName string) string {
	if class, ok := backendToStorageClass[backendName]; ok {
		return class
	}
	return "STANDARD"
}

// BackendRegion returns "eu" or "us" for a given backend name.
// iDrive backends registered as "idrive-eu-*" are EU; everything else is US.
func BackendRegion(name string) string {
	if len(name) > 10 && name[:10] == "idrive-eu-" {
		return "eu"
	}
	return "us"
}
