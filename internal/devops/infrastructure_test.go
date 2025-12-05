// internal/devops/infrastructure_test.go
package devops

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestResourceConfig_Validate(t *testing.T) {
	t.Run("valid config passes", func(t *testing.T) {
		config := &ResourceConfig{
			Name:     "web-server",
			Type:     ResourceTypeCompute,
			Provider: "aws",
			Properties: map[string]interface{}{
				"instance_type": "t3.medium",
				"ami":           "ami-12345",
			},
		}
		err := config.Validate()
		assert.NoError(t, err)
	})

	t.Run("rejects empty name", func(t *testing.T) {
		config := &ResourceConfig{Type: ResourceTypeCompute}
		err := config.Validate()
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "name")
	})

	t.Run("rejects invalid type", func(t *testing.T) {
		config := &ResourceConfig{Name: "test", Type: "invalid"}
		err := config.Validate()
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "type")
	})
}

func TestNewInfraManager(t *testing.T) {
	t.Run("creates manager", func(t *testing.T) {
		manager := NewInfraManager(nil)
		assert.NotNil(t, manager)
	})
}

func TestInfraManager_DefineResource(t *testing.T) {
	manager := NewInfraManager(nil)

	t.Run("defines resource", func(t *testing.T) {
		config := &ResourceConfig{
			Name:     "db-server",
			Type:     ResourceTypeDatabase,
			Provider: "aws",
			Properties: map[string]interface{}{
				"engine": "postgres",
				"size":   "db.t3.medium",
			},
		}

		resource, err := manager.DefineResource(config)
		require.NoError(t, err)
		assert.Equal(t, "db-server", resource.Name())
	})

	t.Run("rejects duplicate", func(t *testing.T) {
		config := &ResourceConfig{Name: "duplicate", Type: ResourceTypeCompute, Provider: "aws"}
		_, _ = manager.DefineResource(config)
		_, err := manager.DefineResource(config)
		assert.Error(t, err)
	})
}

func TestResource_Dependencies(t *testing.T) {
	manager := NewInfraManager(nil)

	vpc, _ := manager.DefineResource(&ResourceConfig{
		Name:     "main-vpc",
		Type:     ResourceTypeNetwork,
		Provider: "aws",
	})

	t.Run("adds dependency", func(t *testing.T) {
		subnet, _ := manager.DefineResource(&ResourceConfig{
			Name:     "subnet-1",
			Type:     ResourceTypeNetwork,
			Provider: "aws",
		})

		err := subnet.DependsOn(vpc)
		assert.NoError(t, err)

		deps := subnet.Dependencies()
		assert.Len(t, deps, 1)
		assert.Equal(t, "main-vpc", deps[0].Name())
	})
}

func TestInfraManager_Plan(t *testing.T) {
	manager := NewInfraManager(nil)

	_, _ = manager.DefineResource(&ResourceConfig{
		Name:     "server-1",
		Type:     ResourceTypeCompute,
		Provider: "aws",
		Properties: map[string]interface{}{
			"instance_type": "t3.small",
		},
	})

	t.Run("generates plan", func(t *testing.T) {
		plan, err := manager.Plan()
		require.NoError(t, err)
		assert.NotEmpty(t, plan.Changes)
	})

	t.Run("shows create actions", func(t *testing.T) {
		plan, _ := manager.Plan()
		assert.Equal(t, ActionCreate, plan.Changes[0].Action)
	})
}

func TestInfraManager_Apply(t *testing.T) {
	manager := NewInfraManager(nil)

	manager.RegisterProvisioner("mock", &MockProvisioner{})

	_, _ = manager.DefineResource(&ResourceConfig{
		Name:     "mock-server",
		Type:     ResourceTypeCompute,
		Provider: "mock",
	})

	t.Run("applies changes", func(t *testing.T) {
		result, err := manager.Apply()
		require.NoError(t, err)
		assert.True(t, result.Success)
	})
}

func TestInfraManager_Destroy(t *testing.T) {
	manager := NewInfraManager(nil)

	manager.RegisterProvisioner("mock", &MockProvisioner{})

	_, _ = manager.DefineResource(&ResourceConfig{
		Name:     "destroy-test",
		Type:     ResourceTypeCompute,
		Provider: "mock",
	})

	// Apply first
	_, _ = manager.Apply()

	t.Run("destroys resources", func(t *testing.T) {
		result, err := manager.Destroy()
		require.NoError(t, err)
		assert.True(t, result.Success)
	})
}

func TestInfraManager_State(t *testing.T) {
	manager := NewInfraManager(nil)

	manager.RegisterProvisioner("mock", &MockProvisioner{})

	_, _ = manager.DefineResource(&ResourceConfig{
		Name:     "state-test",
		Type:     ResourceTypeCompute,
		Provider: "mock",
	})

	_, _ = manager.Apply()

	t.Run("returns current state", func(t *testing.T) {
		state := manager.State()
		assert.NotNil(t, state)
		assert.NotEmpty(t, state.Resources)
	})
}

func TestInfraManager_Import(t *testing.T) {
	manager := NewInfraManager(nil)

	t.Run("imports existing resource", func(t *testing.T) {
		err := manager.Import("server-1", ResourceTypeCompute, "aws", "i-12345")
		assert.NoError(t, err)

		resource := manager.GetResource("server-1")
		assert.NotNil(t, resource)
	})
}

func TestInfraManager_Export(t *testing.T) {
	manager := NewInfraManager(nil)

	_, _ = manager.DefineResource(&ResourceConfig{
		Name:     "export-test",
		Type:     ResourceTypeCompute,
		Provider: "aws",
	})

	t.Run("exports as HCL", func(t *testing.T) {
		hcl, err := manager.Export(FormatHCL)
		require.NoError(t, err)
		assert.Contains(t, hcl, "resource")
	})

	t.Run("exports as JSON", func(t *testing.T) {
		json, err := manager.Export(FormatJSON)
		require.NoError(t, err)
		assert.Contains(t, json, "export-test")
	})

	t.Run("exports as YAML", func(t *testing.T) {
		yaml, err := manager.Export(FormatYAML)
		require.NoError(t, err)
		assert.Contains(t, yaml, "export-test")
	})
}

func TestInfraManager_Variables(t *testing.T) {
	manager := NewInfraManager(nil)

	t.Run("sets variable", func(t *testing.T) {
		manager.SetVariable("region", "us-east-1")
		value := manager.GetVariable("region")
		assert.Equal(t, "us-east-1", value)
	})

	t.Run("uses variable in resource", func(t *testing.T) {
		manager.SetVariable("instance_type", "t3.large")

		_, _ = manager.DefineResource(&ResourceConfig{
			Name:     "var-test",
			Type:     ResourceTypeCompute,
			Provider: "aws",
			Properties: map[string]interface{}{
				"instance_type": "${var.instance_type}",
			},
		})

		resource := manager.GetResource("var-test")
		resolved := resource.ResolvedProperties()
		assert.Equal(t, "t3.large", resolved["instance_type"])
	})
}

func TestInfraManager_Outputs(t *testing.T) {
	manager := NewInfraManager(nil)

	manager.RegisterProvisioner("mock", &MockProvisioner{
		outputs: map[string]string{"ip": "10.0.0.1"},
	})

	_, _ = manager.DefineResource(&ResourceConfig{
		Name:     "output-test",
		Type:     ResourceTypeCompute,
		Provider: "mock",
	})

	_, _ = manager.Apply()

	t.Run("returns outputs", func(t *testing.T) {
		outputs := manager.Outputs()
		assert.Contains(t, outputs, "output-test.ip")
	})
}

func TestResourceTypes(t *testing.T) {
	t.Run("defines types", func(t *testing.T) {
		assert.Equal(t, "compute", ResourceTypeCompute)
		assert.Equal(t, "storage", ResourceTypeStorage)
		assert.Equal(t, "database", ResourceTypeDatabase)
		assert.Equal(t, "network", ResourceTypeNetwork)
		assert.Equal(t, "loadbalancer", ResourceTypeLoadBalancer)
		assert.Equal(t, "dns", ResourceTypeDNS)
		assert.Equal(t, "cache", ResourceTypeCache)
	})
}

func TestActions(t *testing.T) {
	t.Run("defines actions", func(t *testing.T) {
		assert.Equal(t, "create", ActionCreate)
		assert.Equal(t, "update", ActionUpdate)
		assert.Equal(t, "delete", ActionDelete)
		assert.Equal(t, "replace", ActionReplace)
		assert.Equal(t, "noop", ActionNoop)
	})
}

func TestFormats(t *testing.T) {
	t.Run("defines formats", func(t *testing.T) {
		assert.Equal(t, "hcl", FormatHCL)
		assert.Equal(t, "json", FormatJSON)
		assert.Equal(t, "yaml", FormatYAML)
	})
}

func TestInfraManager_GetResource(t *testing.T) {
	manager := NewInfraManager(nil)
	_, _ = manager.DefineResource(&ResourceConfig{Name: "get-test", Type: ResourceTypeCompute, Provider: "aws"})

	t.Run("gets resource", func(t *testing.T) {
		r := manager.GetResource("get-test")
		assert.NotNil(t, r)
	})

	t.Run("returns nil for unknown", func(t *testing.T) {
		r := manager.GetResource("unknown")
		assert.Nil(t, r)
	})
}

func TestInfraManager_ListResources(t *testing.T) {
	manager := NewInfraManager(nil)
	_, _ = manager.DefineResource(&ResourceConfig{Name: "r1", Type: ResourceTypeCompute, Provider: "aws"})
	_, _ = manager.DefineResource(&ResourceConfig{Name: "r2", Type: ResourceTypeStorage, Provider: "aws"})

	t.Run("lists resources", func(t *testing.T) {
		resources := manager.ListResources()
		assert.Len(t, resources, 2)
	})
}

// MockProvisioner for testing
type MockProvisioner struct {
	outputs map[string]string
}

func (m *MockProvisioner) Create(resource *Resource) error {
	return nil
}

func (m *MockProvisioner) Update(resource *Resource) error {
	return nil
}

func (m *MockProvisioner) Delete(resource *Resource) error {
	return nil
}

func (m *MockProvisioner) Read(resource *Resource) error {
	return nil
}

func (m *MockProvisioner) Outputs(resource *Resource) map[string]string {
	if m.outputs != nil {
		return m.outputs
	}
	return map[string]string{}
}
