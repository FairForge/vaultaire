package usage

// FreeTierLimits defines the resource caps for the free tier.
var FreeTierLimits = struct {
	StorageBytes int64
	MaxBuckets   int
	MaxAPIKeys   int
}{
	StorageBytes: 5368709120, // 5 GB
	MaxBuckets:   1,
	MaxAPIKeys:   1,
}

func IsFreeTier(tier string) bool {
	return tier == "free"
}
