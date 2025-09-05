// internal/gateway/validation_example.go
package gateway

import (
	"github.com/FairForge/vaultaire/internal/gateway/validation"
)

// Example of how to use the validation layer
func ExampleValidationSetup() *validation.RequestValidator {
	return validation.NewRequestValidator()
}

// Example validation rules for common operations
var CommonValidationRules = map[string]*validation.ValidationRules{
	"artifact-upload": {
		ContentTypes: []string{"application/octet-stream"},
		MaxBodySize:  5 * 1024 * 1024 * 1024, // 5GB
		Headers: validation.HeaderRules{
			Required: []string{"X-Tenant-ID"},
		},
	},
	"list-artifacts": {
		Query: validation.QueryRules{
			Types: map[string]validation.ParamType{
				"limit": validation.ParamTypeInt,
			},
			Ranges: map[string]validation.Range{
				"limit": {Min: 1, Max: 1000},
			},
		},
	},
}
