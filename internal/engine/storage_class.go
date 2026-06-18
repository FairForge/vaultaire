package engine

var storageClassToBackend = map[string]string{
	"STANDARD":           "idrive",
	"STANDARD_IA":        "lyve",
	"GLACIER":            "geyser",
	"DEEP_ARCHIVE":       "geyser",
	"REDUCED_REDUNDANCY": "local",
}

var backendToStorageClass = map[string]string{
	"idrive":     "STANDARD",
	"lyve":       "STANDARD_IA",
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
