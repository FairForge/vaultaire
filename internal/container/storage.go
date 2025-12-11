// internal/container/storage.go
package container

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// StorageDriver represents container storage driver
type StorageDriver string

const (
	StorageDriverOverlay2     StorageDriver = "overlay2"
	StorageDriverDeviceMapper StorageDriver = "devicemapper"
	StorageDriverBtrfs        StorageDriver = "btrfs"
	StorageDriverZFS          StorageDriver = "zfs"
)

// AccessMode represents volume access mode
type AccessMode string

const (
	AccessModeReadWriteOnce AccessMode = "ReadWriteOnce"
	AccessModeReadOnlyMany  AccessMode = "ReadOnlyMany"
	AccessModeReadWriteMany AccessMode = "ReadWriteMany"
)

// ReclaimPolicy represents volume reclaim policy
type ReclaimPolicy string

const (
	ReclaimPolicyRetain  ReclaimPolicy = "Retain"
	ReclaimPolicyDelete  ReclaimPolicy = "Delete"
	ReclaimPolicyRecycle ReclaimPolicy = "Recycle"
)

// VolumeBindingMode represents when volume binding occurs
type VolumeBindingMode string

const (
	VolumeBindingImmediate            VolumeBindingMode = "Immediate"
	VolumeBindingWaitForFirstConsumer VolumeBindingMode = "WaitForFirstConsumer"
)

// VolumeConfig represents a volume configuration
type VolumeConfig struct {
	Name       string            `json:"name"`
	Driver     string            `json:"driver"`
	DriverOpts map[string]string `json:"driver_opts"`
	Labels     map[string]string `json:"labels"`
}

// Validate checks the volume configuration
func (c *VolumeConfig) Validate() error {
	if c.Name == "" {
		return errors.New("storage: volume name is required")
	}
	return nil
}

// BindMount represents a bind mount
type BindMount struct {
	Source      string `json:"source"`
	Target      string `json:"target"`
	ReadOnly    bool   `json:"read_only"`
	Propagation string `json:"propagation"`
}

// String returns the bind mount as a string
func (b *BindMount) String() string {
	s := fmt.Sprintf("%s:%s", b.Source, b.Target)
	if b.ReadOnly {
		s += ":ro"
	}
	return s
}

// TmpfsMount represents a tmpfs mount
type TmpfsMount struct {
	Target  string   `json:"target"`
	Size    string   `json:"size"`
	Mode    uint32   `json:"mode"`
	Options []string `json:"options"`
}

// PersistentVolumeClaim represents a Kubernetes PVC
type PersistentVolumeClaim struct {
	Name             string            `json:"name"`
	Namespace        string            `json:"namespace"`
	StorageClassName string            `json:"storage_class_name"`
	AccessModes      []AccessMode      `json:"access_modes"`
	Storage          string            `json:"storage"`
	Selector         map[string]string `json:"selector"`
}

// StorageClass represents a Kubernetes storage class
type StorageClass struct {
	Name              string            `json:"name"`
	Provisioner       string            `json:"provisioner"`
	ReclaimPolicy     ReclaimPolicy     `json:"reclaim_policy"`
	VolumeBindingMode VolumeBindingMode `json:"volume_binding_mode"`
	Parameters        map[string]string `json:"parameters"`
	AllowExpansion    bool              `json:"allow_expansion"`
}

// VolumeInfo represents volume information
type VolumeInfo struct {
	Name       string            `json:"name"`
	Driver     string            `json:"driver"`
	Mountpoint string            `json:"mountpoint"`
	Labels     map[string]string `json:"labels"`
	Scope      string            `json:"scope"`
	CreatedAt  string            `json:"created_at"`
	Status     map[string]string `json:"status"`
}

// CSIDriver represents a CSI driver
type CSIDriver struct {
	Name                 string   `json:"name"`
	AttachRequired       bool     `json:"attach_required"`
	PodInfoOnMount       bool     `json:"pod_info_on_mount"`
	VolumeLifecycleModes []string `json:"volume_lifecycle_modes"`
}

// VolumeManagerConfig configures the volume manager
type VolumeManagerConfig struct {
	Provider string `json:"provider"`
}

// VolumeManager manages container volumes
type VolumeManager struct {
	config *VolumeManagerConfig
}

// NewVolumeManager creates a new volume manager
func NewVolumeManager(config *VolumeManagerConfig) *VolumeManager {
	return &VolumeManager{config: config}
}

// CreateVolume creates a new volume
func (m *VolumeManager) CreateVolume(config *VolumeConfig) (string, error) {
	if err := config.Validate(); err != nil {
		return "", err
	}
	if m.config.Provider == "mock" {
		return "vol-" + config.Name, nil
	}
	return "", errors.New("storage: not implemented")
}

// DeleteVolume deletes a volume
func (m *VolumeManager) DeleteVolume(name string) error {
	if m.config.Provider == "mock" {
		return nil
	}
	return errors.New("storage: not implemented")
}

// ListVolumes lists all volumes
func (m *VolumeManager) ListVolumes() ([]*VolumeInfo, error) {
	if m.config.Provider == "mock" {
		return []*VolumeInfo{
			{Name: "default-volume", Driver: "local", Scope: "local"},
		}, nil
	}
	return nil, errors.New("storage: not implemented")
}

// InspectVolume gets volume information
func (m *VolumeManager) InspectVolume(name string) (*VolumeInfo, error) {
	if m.config.Provider == "mock" {
		return &VolumeInfo{
			Name:       name,
			Driver:     "local",
			Mountpoint: "/var/lib/docker/volumes/" + name + "/_data",
			Scope:      "local",
		}, nil
	}
	return nil, errors.New("storage: not implemented")
}

// ParseStorageSize parses a storage size string to bytes
func ParseStorageSize(size string) (int64, error) {
	size = strings.TrimSpace(size)
	if size == "" {
		return 0, errors.New("storage: empty size string")
	}

	// Binary suffixes (Ki, Mi, Gi, Ti)
	binaryPattern := regexp.MustCompile(`^(\d+)(Ki|Mi|Gi|Ti)$`)
	if matches := binaryPattern.FindStringSubmatch(size); matches != nil {
		value, _ := strconv.ParseInt(matches[1], 10, 64)
		switch matches[2] {
		case "Ki":
			return value * 1024, nil
		case "Mi":
			return value * 1024 * 1024, nil
		case "Gi":
			return value * 1024 * 1024 * 1024, nil
		case "Ti":
			return value * 1024 * 1024 * 1024 * 1024, nil
		}
	}

	// Decimal suffixes (K, M, G, T)
	decimalPattern := regexp.MustCompile(`^(\d+)(K|M|G|T)$`)
	if matches := decimalPattern.FindStringSubmatch(size); matches != nil {
		value, _ := strconv.ParseInt(matches[1], 10, 64)
		switch matches[2] {
		case "K":
			return value * 1000, nil
		case "M":
			return value * 1000 * 1000, nil
		case "G":
			return value * 1000 * 1000 * 1000, nil
		case "T":
			return value * 1000 * 1000 * 1000 * 1000, nil
		}
	}

	// Plain bytes
	if value, err := strconv.ParseInt(size, 10, 64); err == nil {
		return value, nil
	}

	return 0, fmt.Errorf("storage: invalid size format %q", size)
}

// FormatStorageSize formats bytes to human readable string
func FormatStorageSize(bytes int64) string {
	const (
		Ki = 1024
		Mi = Ki * 1024
		Gi = Mi * 1024
		Ti = Gi * 1024
	)

	switch {
	case bytes >= Ti:
		return fmt.Sprintf("%dTi", bytes/Ti)
	case bytes >= Gi:
		return fmt.Sprintf("%dGi", bytes/Gi)
	case bytes >= Mi:
		return fmt.Sprintf("%dMi", bytes/Mi)
	case bytes >= Ki:
		return fmt.Sprintf("%dKi", bytes/Ki)
	default:
		return fmt.Sprintf("%d", bytes)
	}
}
