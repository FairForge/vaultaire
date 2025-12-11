// internal/container/storage_test.go
package container

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestVolumeConfig(t *testing.T) {
	t.Run("creates volume config", func(t *testing.T) {
		config := &VolumeConfig{
			Name:   "data-volume",
			Driver: "local",
			Labels: map[string]string{"app": "vaultaire"},
		}
		assert.Equal(t, "data-volume", config.Name)
	})
}

func TestVolumeConfig_Validate(t *testing.T) {
	t.Run("valid config passes", func(t *testing.T) {
		config := &VolumeConfig{Name: "test-vol"}
		err := config.Validate()
		assert.NoError(t, err)
	})

	t.Run("rejects empty name", func(t *testing.T) {
		config := &VolumeConfig{}
		err := config.Validate()
		assert.Error(t, err)
	})
}

func TestBindMount(t *testing.T) {
	t.Run("creates bind mount", func(t *testing.T) {
		mount := &BindMount{
			Source:   "/host/data",
			Target:   "/container/data",
			ReadOnly: false,
		}
		assert.Equal(t, "/host/data", mount.Source)
		assert.Equal(t, "/container/data", mount.Target)
	})

	t.Run("formats mount string", func(t *testing.T) {
		mount := &BindMount{
			Source: "/host/data",
			Target: "/data",
		}
		assert.Equal(t, "/host/data:/data", mount.String())
	})

	t.Run("formats readonly mount", func(t *testing.T) {
		mount := &BindMount{
			Source:   "/host/data",
			Target:   "/data",
			ReadOnly: true,
		}
		assert.Equal(t, "/host/data:/data:ro", mount.String())
	})
}

func TestTmpfsMount(t *testing.T) {
	t.Run("creates tmpfs mount", func(t *testing.T) {
		mount := &TmpfsMount{
			Target: "/tmp",
			Size:   "100m",
			Mode:   0755,
		}
		assert.Equal(t, "/tmp", mount.Target)
		assert.Equal(t, "100m", mount.Size)
	})
}

func TestStorageDriver(t *testing.T) {
	t.Run("storage drivers", func(t *testing.T) {
		assert.Equal(t, StorageDriver("overlay2"), StorageDriverOverlay2)
		assert.Equal(t, StorageDriver("devicemapper"), StorageDriverDeviceMapper)
		assert.Equal(t, StorageDriver("btrfs"), StorageDriverBtrfs)
		assert.Equal(t, StorageDriver("zfs"), StorageDriverZFS)
	})
}

func TestPersistentVolumeClaim(t *testing.T) {
	t.Run("creates PVC", func(t *testing.T) {
		pvc := &PersistentVolumeClaim{
			Name:             "data-pvc",
			Namespace:        "default",
			StorageClassName: "standard",
			AccessModes:      []AccessMode{AccessModeReadWriteOnce},
			Storage:          "10Gi",
		}
		assert.Equal(t, "data-pvc", pvc.Name)
		assert.Equal(t, "10Gi", pvc.Storage)
	})
}

func TestAccessMode(t *testing.T) {
	t.Run("access modes", func(t *testing.T) {
		assert.Equal(t, AccessMode("ReadWriteOnce"), AccessModeReadWriteOnce)
		assert.Equal(t, AccessMode("ReadOnlyMany"), AccessModeReadOnlyMany)
		assert.Equal(t, AccessMode("ReadWriteMany"), AccessModeReadWriteMany)
	})
}

func TestStorageClass(t *testing.T) {
	t.Run("creates storage class", func(t *testing.T) {
		sc := &StorageClass{
			Name:              "fast-storage",
			Provisioner:       "kubernetes.io/aws-ebs",
			ReclaimPolicy:     ReclaimPolicyRetain,
			VolumeBindingMode: VolumeBindingImmediate,
			Parameters: map[string]string{
				"type": "gp3",
				"iops": "3000",
			},
		}
		assert.Equal(t, "fast-storage", sc.Name)
		assert.Equal(t, ReclaimPolicyRetain, sc.ReclaimPolicy)
	})
}

func TestReclaimPolicy(t *testing.T) {
	t.Run("reclaim policies", func(t *testing.T) {
		assert.Equal(t, ReclaimPolicy("Retain"), ReclaimPolicyRetain)
		assert.Equal(t, ReclaimPolicy("Delete"), ReclaimPolicyDelete)
		assert.Equal(t, ReclaimPolicy("Recycle"), ReclaimPolicyRecycle)
	})
}

func TestNewVolumeManager(t *testing.T) {
	t.Run("creates volume manager", func(t *testing.T) {
		mgr := NewVolumeManager(&VolumeManagerConfig{
			Provider: "mock",
		})
		assert.NotNil(t, mgr)
	})
}

func TestVolumeManager_CreateVolume(t *testing.T) {
	mgr := NewVolumeManager(&VolumeManagerConfig{Provider: "mock"})

	t.Run("creates volume", func(t *testing.T) {
		config := &VolumeConfig{Name: "test-volume"}
		id, err := mgr.CreateVolume(config)
		require.NoError(t, err)
		assert.NotEmpty(t, id)
	})
}

func TestVolumeManager_DeleteVolume(t *testing.T) {
	mgr := NewVolumeManager(&VolumeManagerConfig{Provider: "mock"})

	t.Run("deletes volume", func(t *testing.T) {
		err := mgr.DeleteVolume("test-volume")
		assert.NoError(t, err)
	})
}

func TestVolumeManager_ListVolumes(t *testing.T) {
	mgr := NewVolumeManager(&VolumeManagerConfig{Provider: "mock"})

	t.Run("lists volumes", func(t *testing.T) {
		volumes, err := mgr.ListVolumes()
		require.NoError(t, err)
		assert.NotNil(t, volumes)
	})
}

func TestVolumeManager_InspectVolume(t *testing.T) {
	mgr := NewVolumeManager(&VolumeManagerConfig{Provider: "mock"})

	t.Run("inspects volume", func(t *testing.T) {
		info, err := mgr.InspectVolume("test-volume")
		require.NoError(t, err)
		assert.NotNil(t, info)
	})
}

func TestVolumeInfo(t *testing.T) {
	t.Run("creates volume info", func(t *testing.T) {
		info := &VolumeInfo{
			Name:       "data-vol",
			Driver:     "local",
			Mountpoint: "/var/lib/docker/volumes/data-vol/_data",
			Labels:     map[string]string{"app": "test"},
			Scope:      "local",
		}
		assert.Equal(t, "data-vol", info.Name)
		assert.Equal(t, "local", info.Scope)
	})
}

func TestCSIDriver(t *testing.T) {
	t.Run("creates CSI driver", func(t *testing.T) {
		driver := &CSIDriver{
			Name:                 "ebs.csi.aws.com",
			AttachRequired:       true,
			PodInfoOnMount:       false,
			VolumeLifecycleModes: []string{"Persistent"},
		}
		assert.Equal(t, "ebs.csi.aws.com", driver.Name)
		assert.True(t, driver.AttachRequired)
	})
}

func TestParseStorageSize(t *testing.T) {
	t.Run("parses gigabytes", func(t *testing.T) {
		bytes, err := ParseStorageSize("10Gi")
		require.NoError(t, err)
		assert.Equal(t, int64(10*1024*1024*1024), bytes)
	})

	t.Run("parses megabytes", func(t *testing.T) {
		bytes, err := ParseStorageSize("512Mi")
		require.NoError(t, err)
		assert.Equal(t, int64(512*1024*1024), bytes)
	})

	t.Run("parses decimal gigabytes", func(t *testing.T) {
		bytes, err := ParseStorageSize("1G")
		require.NoError(t, err)
		assert.Equal(t, int64(1000*1000*1000), bytes)
	})

	t.Run("rejects invalid size", func(t *testing.T) {
		_, err := ParseStorageSize("invalid")
		assert.Error(t, err)
	})
}

func TestFormatStorageSize(t *testing.T) {
	t.Run("formats bytes to human readable", func(t *testing.T) {
		assert.Equal(t, "1Gi", FormatStorageSize(1024*1024*1024))
		assert.Equal(t, "512Mi", FormatStorageSize(512*1024*1024))
		assert.Equal(t, "1Ki", FormatStorageSize(1024))
	})
}
