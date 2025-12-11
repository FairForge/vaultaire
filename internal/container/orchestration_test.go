// internal/container/orchestration_test.go
package container

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestContainerSpec(t *testing.T) {
	t.Run("creates container spec", func(t *testing.T) {
		spec := &ContainerSpec{
			Name:    "vaultaire",
			Image:   "ghcr.io/fairforge/vaultaire:1.0.0",
			Command: []string{"/app/vaultaire"},
			Args:    []string{"serve", "--config", "/etc/config.yaml"},
			Env: map[string]string{
				"LOG_LEVEL": "info",
			},
			Ports: []PortMapping{
				{ContainerPort: 8080, HostPort: 8080, Protocol: "tcp"},
			},
		}
		assert.Equal(t, "vaultaire", spec.Name)
		assert.Len(t, spec.Ports, 1)
	})
}

func TestContainerSpec_Validate(t *testing.T) {
	t.Run("valid spec passes", func(t *testing.T) {
		spec := &ContainerSpec{
			Name:  "app",
			Image: "nginx:latest",
		}
		err := spec.Validate()
		assert.NoError(t, err)
	})

	t.Run("rejects empty name", func(t *testing.T) {
		spec := &ContainerSpec{Image: "nginx"}
		err := spec.Validate()
		assert.Error(t, err)
	})

	t.Run("rejects empty image", func(t *testing.T) {
		spec := &ContainerSpec{Name: "app"}
		err := spec.Validate()
		assert.Error(t, err)
	})
}

func TestResourceRequirements(t *testing.T) {
	t.Run("creates resource requirements", func(t *testing.T) {
		resources := &ResourceRequirements{
			Requests: ResourceList{
				CPU:    "100m",
				Memory: "128Mi",
			},
			Limits: ResourceList{
				CPU:    "500m",
				Memory: "512Mi",
			},
		}
		assert.Equal(t, "100m", resources.Requests.CPU)
		assert.Equal(t, "512Mi", resources.Limits.Memory)
	})
}

func TestVolumeMount(t *testing.T) {
	t.Run("creates volume mount", func(t *testing.T) {
		mount := &VolumeMount{
			Name:      "config",
			MountPath: "/etc/config",
			ReadOnly:  true,
		}
		assert.Equal(t, "/etc/config", mount.MountPath)
		assert.True(t, mount.ReadOnly)
	})
}

func TestProbe(t *testing.T) {
	t.Run("creates http probe", func(t *testing.T) {
		probe := &Probe{
			HTTPGet: &HTTPGetAction{
				Path: "/health",
				Port: 8080,
			},
			InitialDelaySeconds: 10,
			PeriodSeconds:       30,
			TimeoutSeconds:      5,
			FailureThreshold:    3,
		}
		assert.Equal(t, "/health", probe.HTTPGet.Path)
	})

	t.Run("creates exec probe", func(t *testing.T) {
		probe := &Probe{
			Exec: &ExecAction{
				Command: []string{"cat", "/tmp/healthy"},
			},
		}
		assert.NotNil(t, probe.Exec)
	})

	t.Run("creates tcp probe", func(t *testing.T) {
		probe := &Probe{
			TCPSocket: &TCPSocketAction{
				Port: 8080,
			},
		}
		assert.Equal(t, 8080, probe.TCPSocket.Port)
	})
}

func TestDeploymentSpec(t *testing.T) {
	t.Run("creates deployment spec", func(t *testing.T) {
		spec := &DeploymentSpec{
			Name:      "vaultaire",
			Namespace: "default",
			Replicas:  3,
			Selector: map[string]string{
				"app": "vaultaire",
			},
			Template: &ContainerSpec{
				Name:  "vaultaire",
				Image: "ghcr.io/fairforge/vaultaire:1.0.0",
			},
		}
		assert.Equal(t, 3, spec.Replicas)
	})
}

func TestDeploymentSpec_Validate(t *testing.T) {
	t.Run("valid spec passes", func(t *testing.T) {
		spec := &DeploymentSpec{
			Name:     "app",
			Replicas: 1,
			Template: &ContainerSpec{Name: "app", Image: "nginx"},
		}
		err := spec.Validate()
		assert.NoError(t, err)
	})

	t.Run("rejects zero replicas", func(t *testing.T) {
		spec := &DeploymentSpec{
			Name:     "app",
			Replicas: 0,
			Template: &ContainerSpec{Name: "app", Image: "nginx"},
		}
		err := spec.Validate()
		assert.Error(t, err)
	})
}

func TestServiceSpec(t *testing.T) {
	t.Run("creates service spec", func(t *testing.T) {
		spec := &ServiceSpec{
			Name:      "vaultaire",
			Namespace: "default",
			Type:      ServiceTypeClusterIP,
			Selector: map[string]string{
				"app": "vaultaire",
			},
			Ports: []ServicePort{
				{Name: "http", Port: 80, TargetPort: 8080},
			},
		}
		assert.Equal(t, ServiceTypeClusterIP, spec.Type)
	})
}

func TestServiceType(t *testing.T) {
	t.Run("service types", func(t *testing.T) {
		assert.Equal(t, ServiceType("ClusterIP"), ServiceTypeClusterIP)
		assert.Equal(t, ServiceType("NodePort"), ServiceTypeNodePort)
		assert.Equal(t, ServiceType("LoadBalancer"), ServiceTypeLoadBalancer)
	})
}

func TestNewOrchestrator(t *testing.T) {
	t.Run("creates orchestrator", func(t *testing.T) {
		orch := NewOrchestrator(&OrchestratorConfig{
			Provider: "mock",
		})
		assert.NotNil(t, orch)
	})
}

func TestOrchestrator_Deploy(t *testing.T) {
	orch := NewOrchestrator(&OrchestratorConfig{
		Provider: "mock",
	})

	t.Run("deploys application", func(t *testing.T) {
		ctx := context.Background()
		spec := &DeploymentSpec{
			Name:     "test-app",
			Replicas: 1,
			Template: &ContainerSpec{Name: "app", Image: "nginx"},
		}
		err := orch.Deploy(ctx, spec)
		assert.NoError(t, err)
	})
}

func TestOrchestrator_Scale(t *testing.T) {
	orch := NewOrchestrator(&OrchestratorConfig{
		Provider: "mock",
	})

	t.Run("scales deployment", func(t *testing.T) {
		ctx := context.Background()
		err := orch.Scale(ctx, "default", "test-app", 5)
		assert.NoError(t, err)
	})
}

func TestOrchestrator_Rollback(t *testing.T) {
	orch := NewOrchestrator(&OrchestratorConfig{
		Provider: "mock",
	})

	t.Run("rolls back deployment", func(t *testing.T) {
		ctx := context.Background()
		err := orch.Rollback(ctx, "default", "test-app", 1)
		assert.NoError(t, err)
	})
}

func TestOrchestrator_GetStatus(t *testing.T) {
	orch := NewOrchestrator(&OrchestratorConfig{
		Provider: "mock",
	})

	t.Run("gets deployment status", func(t *testing.T) {
		ctx := context.Background()
		status, err := orch.GetStatus(ctx, "default", "test-app")
		require.NoError(t, err)
		assert.NotNil(t, status)
	})
}

func TestDeploymentStatus(t *testing.T) {
	t.Run("creates status", func(t *testing.T) {
		status := &DeploymentStatus{
			Name:              "test-app",
			Namespace:         "default",
			Replicas:          3,
			ReadyReplicas:     3,
			UpdatedReplicas:   3,
			AvailableReplicas: 3,
			Conditions: []DeploymentCondition{
				{
					Type:    "Available",
					Status:  "True",
					Reason:  "MinimumReplicasAvailable",
					Message: "Deployment has minimum availability",
				},
			},
		}
		assert.Equal(t, 3, status.ReadyReplicas)
		assert.True(t, status.IsReady())
	})

	t.Run("not ready when replicas mismatch", func(t *testing.T) {
		status := &DeploymentStatus{
			Replicas:      3,
			ReadyReplicas: 2,
		}
		assert.False(t, status.IsReady())
	})
}

func TestOrchestrator_CreateService(t *testing.T) {
	orch := NewOrchestrator(&OrchestratorConfig{
		Provider: "mock",
	})

	t.Run("creates service", func(t *testing.T) {
		ctx := context.Background()
		spec := &ServiceSpec{
			Name: "test-svc",
			Type: ServiceTypeClusterIP,
			Ports: []ServicePort{
				{Port: 80, TargetPort: 8080},
			},
		}
		err := orch.CreateService(ctx, spec)
		assert.NoError(t, err)
	})
}

func TestOrchestrator_DeleteDeployment(t *testing.T) {
	orch := NewOrchestrator(&OrchestratorConfig{
		Provider: "mock",
	})

	t.Run("deletes deployment", func(t *testing.T) {
		ctx := context.Background()
		err := orch.DeleteDeployment(ctx, "default", "test-app")
		assert.NoError(t, err)
	})
}

func TestRollingUpdateStrategy(t *testing.T) {
	t.Run("creates rolling update strategy", func(t *testing.T) {
		strategy := &RollingUpdateStrategy{
			MaxUnavailable: "25%",
			MaxSurge:       "25%",
		}
		assert.Equal(t, "25%", strategy.MaxUnavailable)
	})
}

func TestOrchestratorConfig(t *testing.T) {
	t.Run("creates config", func(t *testing.T) {
		config := &OrchestratorConfig{
			Provider:   "kubernetes",
			Kubeconfig: "/path/to/kubeconfig",
			Namespace:  "production",
			Timeout:    5 * time.Minute,
		}
		assert.Equal(t, "kubernetes", config.Provider)
	})
}
