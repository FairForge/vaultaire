// internal/devops/infrastructure.go
package devops

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"

	"gopkg.in/yaml.v3"
)

// Resource types
const (
	ResourceTypeCompute      = "compute"
	ResourceTypeStorage      = "storage"
	ResourceTypeDatabase     = "database"
	ResourceTypeNetwork      = "network"
	ResourceTypeLoadBalancer = "loadbalancer"
	ResourceTypeDNS          = "dns"
	ResourceTypeCache        = "cache"
)

// Actions
const (
	ActionCreate  = "create"
	ActionUpdate  = "update"
	ActionDelete  = "delete"
	ActionReplace = "replace"
	ActionNoop    = "noop"
)

// Export formats
const (
	FormatHCL  = "hcl"
	FormatJSON = "json"
	FormatYAML = "yaml"
)

// ResourceConfig configures a resource
type ResourceConfig struct {
	Name       string                 `json:"name"`
	Type       string                 `json:"type"`
	Provider   string                 `json:"provider"`
	Properties map[string]interface{} `json:"properties"`
}

// Validate checks configuration
func (c *ResourceConfig) Validate() error {
	if c.Name == "" {
		return errors.New("infrastructure: name is required")
	}
	validTypes := map[string]bool{
		ResourceTypeCompute:      true,
		ResourceTypeStorage:      true,
		ResourceTypeDatabase:     true,
		ResourceTypeNetwork:      true,
		ResourceTypeLoadBalancer: true,
		ResourceTypeDNS:          true,
		ResourceTypeCache:        true,
	}
	if !validTypes[c.Type] {
		return fmt.Errorf("infrastructure: invalid type: %s", c.Type)
	}
	return nil
}

// Resource represents an infrastructure resource
type Resource struct {
	config       *ResourceConfig
	dependencies []*Resource
	outputs      map[string]string
	manager      *InfraManager
	mu           sync.Mutex
}

// Name returns the resource name
func (r *Resource) Name() string {
	return r.config.Name
}

// Type returns the resource type
func (r *Resource) Type() string {
	return r.config.Type
}

// Provider returns the provider
func (r *Resource) Provider() string {
	return r.config.Provider
}

// DependsOn adds a dependency
func (r *Resource) DependsOn(dep *Resource) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.dependencies = append(r.dependencies, dep)
	return nil
}

// Dependencies returns all dependencies
func (r *Resource) Dependencies() []*Resource {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.dependencies
}

// ResolvedProperties returns properties with variables resolved
func (r *Resource) ResolvedProperties() map[string]interface{} {
	r.mu.Lock()
	defer r.mu.Unlock()

	resolved := make(map[string]interface{})
	for k, v := range r.config.Properties {
		if str, ok := v.(string); ok {
			resolved[k] = r.resolveVariable(str)
		} else {
			resolved[k] = v
		}
	}
	return resolved
}

func (r *Resource) resolveVariable(value string) string {
	if !strings.HasPrefix(value, "${var.") {
		return value
	}

	varName := strings.TrimPrefix(value, "${var.")
	varName = strings.TrimSuffix(varName, "}")

	if r.manager != nil {
		if resolved := r.manager.GetVariable(varName); resolved != "" {
			return resolved
		}
	}
	return value
}

// Provisioner provisions resources
type Provisioner interface {
	Create(resource *Resource) error
	Update(resource *Resource) error
	Delete(resource *Resource) error
	Read(resource *Resource) error
	Outputs(resource *Resource) map[string]string
}

// Change represents a planned change
type Change struct {
	Resource *Resource              `json:"resource"`
	Action   string                 `json:"action"`
	Before   map[string]interface{} `json:"before,omitempty"`
	After    map[string]interface{} `json:"after,omitempty"`
}

// Plan represents an execution plan
type Plan struct {
	Changes []*Change `json:"changes"`
}

// ApplyResult represents apply results
type ApplyResult struct {
	Success bool     `json:"success"`
	Created []string `json:"created"`
	Updated []string `json:"updated"`
	Deleted []string `json:"deleted"`
	Errors  []string `json:"errors"`
}

// State represents infrastructure state
type State struct {
	Resources map[string]*ResourceState `json:"resources"`
}

// ResourceState represents a resource's state
type ResourceState struct {
	Name       string                 `json:"name"`
	Type       string                 `json:"type"`
	Provider   string                 `json:"provider"`
	Properties map[string]interface{} `json:"properties"`
	Outputs    map[string]string      `json:"outputs"`
}

// InfraManagerConfig configures the infra manager
type InfraManagerConfig struct {
	StateBackend string
}

// InfraManager manages infrastructure
type InfraManager struct {
	config       *InfraManagerConfig
	resources    map[string]*Resource
	provisioners map[string]Provisioner
	variables    map[string]string
	state        *State
	mu           sync.RWMutex
}

// NewInfraManager creates an infra manager
func NewInfraManager(config *InfraManagerConfig) *InfraManager {
	if config == nil {
		config = &InfraManagerConfig{}
	}

	return &InfraManager{
		config:       config,
		resources:    make(map[string]*Resource),
		provisioners: make(map[string]Provisioner),
		variables:    make(map[string]string),
		state: &State{
			Resources: make(map[string]*ResourceState),
		},
	}
}

// DefineResource defines a resource
func (m *InfraManager) DefineResource(config *ResourceConfig) (*Resource, error) {
	if err := config.Validate(); err != nil {
		return nil, err
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	if _, exists := m.resources[config.Name]; exists {
		return nil, fmt.Errorf("infrastructure: resource %s already exists", config.Name)
	}

	resource := &Resource{
		config:       config,
		dependencies: make([]*Resource, 0),
		outputs:      make(map[string]string),
		manager:      m,
	}

	m.resources[config.Name] = resource
	return resource, nil
}

// GetResource returns a resource by name
func (m *InfraManager) GetResource(name string) *Resource {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.resources[name]
}

// ListResources returns all resources
func (m *InfraManager) ListResources() []*Resource {
	m.mu.RLock()
	defer m.mu.RUnlock()

	resources := make([]*Resource, 0, len(m.resources))
	for _, r := range m.resources {
		resources = append(resources, r)
	}
	return resources
}

// RegisterProvisioner registers a provisioner
func (m *InfraManager) RegisterProvisioner(name string, provisioner Provisioner) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.provisioners[name] = provisioner
}

// SetVariable sets a variable
func (m *InfraManager) SetVariable(name, value string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.variables[name] = value
}

// GetVariable gets a variable
func (m *InfraManager) GetVariable(name string) string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.variables[name]
}

// Plan generates an execution plan
func (m *InfraManager) Plan() (*Plan, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	plan := &Plan{
		Changes: make([]*Change, 0),
	}

	for _, resource := range m.resources {
		action := ActionCreate
		if _, exists := m.state.Resources[resource.Name()]; exists {
			action = ActionUpdate
		}

		plan.Changes = append(plan.Changes, &Change{
			Resource: resource,
			Action:   action,
			After:    resource.ResolvedProperties(),
		})
	}

	return plan, nil
}

// Apply applies changes
func (m *InfraManager) Apply() (*ApplyResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	result := &ApplyResult{
		Success: true,
		Created: make([]string, 0),
		Updated: make([]string, 0),
		Deleted: make([]string, 0),
		Errors:  make([]string, 0),
	}

	// Sort by dependencies (simplified)
	for _, resource := range m.resources {
		provisioner := m.provisioners[resource.Provider()]

		_, exists := m.state.Resources[resource.Name()]

		var err error
		if provisioner != nil {
			if exists {
				err = provisioner.Update(resource)
				if err == nil {
					result.Updated = append(result.Updated, resource.Name())
				}
			} else {
				err = provisioner.Create(resource)
				if err == nil {
					result.Created = append(result.Created, resource.Name())
				}
			}

			if err == nil {
				resource.outputs = provisioner.Outputs(resource)
			}
		} else {
			// No provisioner, just track state
			if exists {
				result.Updated = append(result.Updated, resource.Name())
			} else {
				result.Created = append(result.Created, resource.Name())
			}
		}

		if err != nil {
			result.Success = false
			result.Errors = append(result.Errors, err.Error())
		} else {
			m.state.Resources[resource.Name()] = &ResourceState{
				Name:       resource.Name(),
				Type:       resource.Type(),
				Provider:   resource.Provider(),
				Properties: resource.ResolvedProperties(),
				Outputs:    resource.outputs,
			}
		}
	}

	return result, nil
}

// Destroy destroys all resources
func (m *InfraManager) Destroy() (*ApplyResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	result := &ApplyResult{
		Success: true,
		Deleted: make([]string, 0),
		Errors:  make([]string, 0),
	}

	for name, resource := range m.resources {
		provisioner := m.provisioners[resource.Provider()]

		var err error
		if provisioner != nil {
			err = provisioner.Delete(resource)
		}

		if err != nil {
			result.Success = false
			result.Errors = append(result.Errors, err.Error())
		} else {
			result.Deleted = append(result.Deleted, name)
			delete(m.state.Resources, name)
		}
	}

	return result, nil
}

// State returns current state
func (m *InfraManager) State() *State {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.state
}

// Import imports an existing resource
func (m *InfraManager) Import(name, resourceType, provider, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	resource := &Resource{
		config: &ResourceConfig{
			Name:       name,
			Type:       resourceType,
			Provider:   provider,
			Properties: map[string]interface{}{"id": id},
		},
		dependencies: make([]*Resource, 0),
		outputs:      make(map[string]string),
		manager:      m,
	}

	m.resources[name] = resource
	m.state.Resources[name] = &ResourceState{
		Name:       name,
		Type:       resourceType,
		Provider:   provider,
		Properties: map[string]interface{}{"id": id},
	}

	return nil
}

// Export exports configuration
func (m *InfraManager) Export(format string) (string, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	switch format {
	case FormatHCL:
		return m.exportHCL()
	case FormatJSON:
		return m.exportJSON()
	case FormatYAML:
		return m.exportYAML()
	default:
		return "", fmt.Errorf("infrastructure: unknown format: %s", format)
	}
}

func (m *InfraManager) exportHCL() (string, error) {
	var sb strings.Builder

	for _, resource := range m.resources {
		sb.WriteString(fmt.Sprintf("resource \"%s\" \"%s\" {\n", resource.Type(), resource.Name()))
		sb.WriteString(fmt.Sprintf("  provider = \"%s\"\n", resource.Provider()))
		for k, v := range resource.config.Properties {
			sb.WriteString(fmt.Sprintf("  %s = \"%v\"\n", k, v))
		}
		sb.WriteString("}\n\n")
	}

	return sb.String(), nil
}

func (m *InfraManager) exportJSON() (string, error) {
	data := make(map[string]interface{})
	resources := make([]map[string]interface{}, 0)

	for _, resource := range m.resources {
		resources = append(resources, map[string]interface{}{
			"name":       resource.Name(),
			"type":       resource.Type(),
			"provider":   resource.Provider(),
			"properties": resource.config.Properties,
		})
	}

	data["resources"] = resources

	bytes, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		return "", err
	}
	return string(bytes), nil
}

func (m *InfraManager) exportYAML() (string, error) {
	data := make(map[string]interface{})
	resources := make([]map[string]interface{}, 0)

	for _, resource := range m.resources {
		resources = append(resources, map[string]interface{}{
			"name":       resource.Name(),
			"type":       resource.Type(),
			"provider":   resource.Provider(),
			"properties": resource.config.Properties,
		})
	}

	data["resources"] = resources

	bytes, err := yaml.Marshal(data)
	if err != nil {
		return "", err
	}
	return string(bytes), nil
}

// Outputs returns all outputs
func (m *InfraManager) Outputs() map[string]string {
	m.mu.RLock()
	defer m.mu.RUnlock()

	outputs := make(map[string]string)
	for name, state := range m.state.Resources {
		for k, v := range state.Outputs {
			outputs[name+"."+k] = v
		}
	}
	return outputs
}
