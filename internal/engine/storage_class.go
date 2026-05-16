package engine

var storageClassToBackend = map[string]string{
	"STANDARD":           "idrive",
	"STANDARD_IA":        "lyve",
	"GLACIER":            "geyser",
	"DEEP_ARCHIVE":       "geyser",
	"ONEZONE_IA":         "onedrive",
	"REDUCED_REDUNDANCY": "local",
}

var backendToStorageClass = map[string]string{
	"idrive":   "STANDARD",
	"lyve":     "STANDARD_IA",
	"geyser":   "GLACIER",
	"onedrive": "ONEZONE_IA",
	"local":    "REDUCED_REDUNDANCY",
	"s3":       "STANDARD",
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
