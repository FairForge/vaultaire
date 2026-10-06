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
//
// REDUCED_REDUNDANCY is absent too (WP-R7-5, decision in
// docs/reviews/WP-R7-5.md): it used to route to `local`, the hub's own disk
// — a single copy on a DATA_PATH that is not backed up (R9-12); 449 of
// prod's 451 `local` head rows pointed at bytes that were gone. `local` is
// the development primary and the engine's last resort, never a class a
// customer can choose; the class degrades to the primary at STANDARD like
// any other unsold class (the request header never reached here anyway —
// api.clientStorageClass drops it, R2-07).
// STANDARD is deliberately unmapped: it is the primary backend's class, and
// the primary is chosen at boot (STORAGE_MODE / config.DetectStorageMode) —
// iDrive in the long run, Wasabi while the iDrive account is repaired
// (2026-10-03). A fixed "STANDARD → idrive" entry sent every Standard PUT to
// the registered iDrive driver whatever the primary was, so the interim
// switch would have changed nothing. ResolveStorageClass returns the primary
// for any unmapped class.
var storageClassToBackend = map[string]string{
	"GLACIER":      "geyser",
	"DEEP_ARCHIVE": "geyser",

	// RESILIENT is OUR tier name, not an AWS storage class — it is what the
	// `resilient` bucket tier_preference resolves to (the $7.99 Lyve-backed
	// tier: WORM, write-only creds, no egress fees, 3-AZ-free concurrency).
	// This is NOT the removed STANDARD_IA mapping: objects land on the Lyve
	// backend at its default (STANDARD) class — Lyve's IA service tier with
	// its retention/size penalties is never used.
	"RESILIENT": "lyve",

	// PUBLIC is OUR internal class for public-read buckets (never sent by
	// clients): it routes to the Cloudflare R2 backend, whose only role is
	// public buckets / CDN origin ($0 egress, next to the proxied
	// cdn.stored.ge host). Not a tier — nothing else resolves here. Set by
	// api.resolvePutStorageClass when the bucket is public-read and an r2
	// driver is registered; falls back to the primary like every mapping.
	"PUBLIC": "r2",

	// SYNC is OUR internal class for the Sync.com WebDAV bridge (never sent
	// by clients): set by api.resolvePutStorageClass only for a bucket whose
	// tier_preference is `sync` AND whose tenant has the `sync_backend` flag
	// (Sync's terms: no customer data without its written consent). Falls
	// back to the primary like every mapping when no `sync` driver exists.
	"SYNC": "sync",
}

var backendToStorageClass = map[string]string{
	"idrive":     "STANDARD",
	"wasabi":     "STANDARD",
	"lyve":       "STANDARD",
	"geyser":     "GLACIER",
	"permafrost": "STANDARD",
	// local reports STANDARD: on a box where it is the primary (development)
	// that is the class the object has; nowhere else is it a tier (WP-R7-5).
	"local": "STANDARD",
	"s3":    "STANDARD",
	"r2":    "STANDARD",
	"sync":  "STANDARD",
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

// archiveFloor is the object_head_cache.floor id of the attic
// (usage.FloorVault — the engine cannot import internal/usage; the api
// package's TestCustomerStorageClass_AgreesWithTheFloorIDs pins the two).
const archiveFloor = "vault"

// IsArchiveClass reports whether a storage class needs a restore before the
// object can be read (what `aws s3 sync` / `cp` skip without
// --force-glacier-transfer).
func IsArchiveClass(class string) bool {
	return class == "GLACIER" || class == "DEEP_ARCHIVE"
}

// CustomerStorageClass is the storage class a customer sees for an object:
// the ONE place listings, HEAD, GET, inventory reports and the dashboard
// derive it (WP-R13-1, Review R13-04).
//
// The class follows the floor the object is billed on (object_head_cache.floor),
// not the backend that holds its bytes today. A downstairs object
// (floor = standard) is never reported in an archive class: where the Smart
// tier parks it is ours, the customer bought STANDARD and reads it with a
// plain GET (the read-time promotion brings it back). It used to list as
// GLACIER once demoted to tape, and `aws s3 sync` skipped it. An attic
// object (floor = vault) reports the class of the backend it is on, as
// before. Any floor value that is not the attic is treated as downstairs
// (the column is NOT NULL DEFAULT 'standard' since migration 066).
func CustomerStorageClass(floor, backendName string) string {
	class := BackendToStorageClass(backendName)
	if floor != archiveFloor && IsArchiveClass(class) {
		return "STANDARD"
	}
	return class
}

// BackendRegion returns "eu" or "us" for a given backend name.
// iDrive backends registered as "idrive-eu-*" are EU; everything else is US.
func BackendRegion(name string) string {
	if len(name) > 10 && name[:10] == "idrive-eu-" {
		return "eu"
	}
	return "us"
}
