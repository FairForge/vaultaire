// internal/gateway/validation/routes.go
package validation

// Remove any self-import - we don't need to import the package we're in

// RouteValidationRules defines validation rules for different routes
var RouteValidationRules = map[string]*ValidationRules{
	"PUT /v1/containers/{container}/artifacts/{artifact}": {
		ContentTypes: []string{"application/octet-stream", "multipart/form-data"},
		MaxBodySize:  5 * 1024 * 1024 * 1024, // 5GB
		Headers: HeaderRules{
			Required: []string{"X-Tenant-ID"},
		},
	},
	"GET /v1/containers/{container}/artifacts": {
		Query: QueryRules{
			Types: map[string]ParamType{
				"limit":  ParamTypeInt,
				"offset": ParamTypeInt,
			},
			Ranges: map[string]Range{
				"limit": {Min: 1, Max: 1000},
			},
		},
	},
	"POST /v1/users": {
		ContentTypes: []string{"application/json"},
		MaxBodySize:  1024 * 1024, // 1MB
		JSONSchema: `{
			"type": "object",
			"required": ["email", "password"],
			"properties": {
				"email": {"type": "string", "format": "email"},
				"password": {"type": "string", "minLength": 8},
				"name": {"type": "string", "maxLength": 100}
			}
		}`,
	},
}

// GetValidationRules returns validation rules for a given route
func GetValidationRules(method, path string) *ValidationRules {
	key := method + " " + path
	return RouteValidationRules[key]
}
