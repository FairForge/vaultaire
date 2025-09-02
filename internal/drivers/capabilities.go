package drivers

// Capability represents what a driver can do
type Capability string

const (
	CapabilityStreaming   Capability = "streaming"
	CapabilityRangeRead   Capability = "range_read"
	CapabilityMultipart   Capability = "multipart"
	CapabilityVersioning  Capability = "versioning"
	CapabilityEncryption  Capability = "encryption"
	CapabilityReplication Capability = "replication"
	CapabilityWatch       Capability = "watch"
	CapabilityAtomic      Capability = "atomic"
)

// CapabilityChecker interface for drivers that report capabilities
type CapabilityChecker interface {
	Capabilities() []Capability
	HasCapability(cap Capability) bool
}

// Capabilities returns the capabilities of the LocalDriver
func (d *LocalDriver) Capabilities() []Capability {
	return []Capability{
		CapabilityStreaming, // Can stream data without buffering
		CapabilityRangeRead, // Can read partial files
		CapabilityMultipart, // Supports multipart uploads
		CapabilityWatch,     // Can watch for file changes
		CapabilityAtomic,    // Supports atomic operations
	}
}

// HasCapability checks if the driver has a specific capability
func (d *LocalDriver) HasCapability(cap Capability) bool {
	capabilities := d.Capabilities()
	for _, c := range capabilities {
		if c == cap {
			return true
		}
	}
	return false
}
