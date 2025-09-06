// internal/docs/openapi.go
package docs

import (
	"encoding/json"
	"fmt"
	"net/http"
)

// OpenAPISpec represents an OpenAPI 3.0 specification
type OpenAPISpec struct {
	OpenAPI    string                `json:"openapi"`
	Info       Info                  `json:"info"`
	Servers    []Server              `json:"servers,omitempty"`
	Paths      map[string]*PathItem  `json:"paths"`
	Components Components            `json:"components"`
	Security   []SecurityRequirement `json:"security,omitempty"`
	Tags       []Tag                 `json:"tags,omitempty"`
}

// Info contains API metadata
type Info struct {
	Title       string  `json:"title"`
	Description string  `json:"description,omitempty"`
	Version     string  `json:"version"`
	Contact     Contact `json:"contact,omitempty"`
	License     License `json:"license,omitempty"`
}

// Contact information
type Contact struct {
	Name  string `json:"name,omitempty"`
	URL   string `json:"url,omitempty"`
	Email string `json:"email,omitempty"`
}

// License information
type License struct {
	Name string `json:"name"`
	URL  string `json:"url,omitempty"`
}

// Server represents an API server
type Server struct {
	URL         string                    `json:"url"`
	Description string                    `json:"description,omitempty"`
	Variables   map[string]ServerVariable `json:"variables,omitempty"`
}

// ServerVariable for templating
type ServerVariable struct {
	Default     string   `json:"default"`
	Description string   `json:"description,omitempty"`
	Enum        []string `json:"enum,omitempty"`
}

// PathItem represents operations on a path
type PathItem struct {
	Get        *Operation  `json:"get,omitempty"`
	Put        *Operation  `json:"put,omitempty"`
	Post       *Operation  `json:"post,omitempty"`
	Delete     *Operation  `json:"delete,omitempty"`
	Head       *Operation  `json:"head,omitempty"`
	Options    *Operation  `json:"options,omitempty"`
	Parameters []Parameter `json:"parameters,omitempty"`
}

// Operation represents an API operation
type Operation struct {
	Tags        []string              `json:"tags,omitempty"`
	Summary     string                `json:"summary,omitempty"`
	Description string                `json:"description,omitempty"`
	OperationID string                `json:"operationId,omitempty"`
	Parameters  []Parameter           `json:"parameters,omitempty"`
	RequestBody *RequestBody          `json:"requestBody,omitempty"`
	Responses   map[string]Response   `json:"responses"`
	Security    []SecurityRequirement `json:"security,omitempty"`
}

// Parameter for operations
type Parameter struct {
	Name        string      `json:"name"`
	In          string      `json:"in"`
	Description string      `json:"description,omitempty"`
	Required    bool        `json:"required,omitempty"`
	Schema      *Schema     `json:"schema,omitempty"`
	Example     interface{} `json:"example,omitempty"`
}

// RequestBody for operations
type RequestBody struct {
	Description string               `json:"description,omitempty"`
	Content     map[string]MediaType `json:"content"`
	Required    bool                 `json:"required,omitempty"`
}

// Response from an operation
type Response struct {
	Description string               `json:"description"`
	Headers     map[string]Header    `json:"headers,omitempty"`
	Content     map[string]MediaType `json:"content,omitempty"`
}

// Header definition
type Header struct {
	Description string  `json:"description,omitempty"`
	Schema      *Schema `json:"schema,omitempty"`
}

// MediaType with schema
type MediaType struct {
	Schema   *Schema            `json:"schema,omitempty"`
	Example  interface{}        `json:"example,omitempty"`
	Examples map[string]Example `json:"examples,omitempty"`
}

// Example for documentation
type Example struct {
	Summary     string      `json:"summary,omitempty"`
	Description string      `json:"description,omitempty"`
	Value       interface{} `json:"value,omitempty"`
}

// Components container
type Components struct {
	Schemas         map[string]Schema         `json:"schemas,omitempty"`
	SecuritySchemes map[string]SecurityScheme `json:"securitySchemes,omitempty"`
	Parameters      map[string]Parameter      `json:"parameters,omitempty"`
	RequestBodies   map[string]RequestBody    `json:"requestBodies,omitempty"`
	Responses       map[string]Response       `json:"responses,omitempty"`
}

// Schema definition
type Schema struct {
	Type        string             `json:"type,omitempty"`
	Format      string             `json:"format,omitempty"`
	Description string             `json:"description,omitempty"`
	Properties  map[string]*Schema `json:"properties,omitempty"`
	Required    []string           `json:"required,omitempty"`
	Items       *Schema            `json:"items,omitempty"`
	Example     interface{}        `json:"example,omitempty"`
	Ref         string             `json:"$ref,omitempty"`
	Enum        []interface{}      `json:"enum,omitempty"`
	XML         *XMLObject         `json:"xml,omitempty"`
}

// XMLObject for XML marshaling hints
type XMLObject struct {
	Name      string `json:"name,omitempty"`
	Namespace string `json:"namespace,omitempty"`
	Wrapped   bool   `json:"wrapped,omitempty"`
}

// SecurityScheme definition
type SecurityScheme struct {
	Type        string `json:"type"`
	Description string `json:"description,omitempty"`
	Name        string `json:"name,omitempty"`
	In          string `json:"in,omitempty"`
	Scheme      string `json:"scheme,omitempty"`
}

// SecurityRequirement mapping
type SecurityRequirement map[string][]string

// Tag for grouping operations
type Tag struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
}

// GenerateOpenAPISpec creates the complete OpenAPI specification
func GenerateOpenAPISpec() *OpenAPISpec {
	return &OpenAPISpec{
		OpenAPI: "3.0.3",
		Info: Info{
			Title:       "Vaultaire Storage API",
			Description: "S3-compatible distributed storage API",
			Version:     "1.0.0",
			Contact: Contact{
				Name:  "Vaultaire Team",
				Email: "support@vaultaire.io",
			},
			License: License{
				Name: "MIT",
				URL:  "https://opensource.org/licenses/MIT",
			},
		},
		Servers: []Server{
			{
				URL:         "https://api.stored.ge",
				Description: "Production server",
			},
			{
				URL:         "http://localhost:8080",
				Description: "Development server",
			},
		},
		Tags: []Tag{
			{Name: "Buckets", Description: "Bucket operations"},
			{Name: "Objects", Description: "Object operations"},
			{Name: "Auth", Description: "Authentication"},
			{Name: "Health", Description: "Health checks"},
		},
		Paths: generatePaths(),
		Components: Components{
			Schemas:         generateSchemas(),
			SecuritySchemes: generateSecuritySchemes(),
		},
		Security: []SecurityRequirement{
			{"ApiKeyAuth": {}},
		},
	}
}

func generatePaths() map[string]*PathItem {
	return map[string]*PathItem{
		"/": {
			Get: &Operation{
				Tags:        []string{"Buckets"},
				Summary:     "List all buckets",
				Description: "Returns a list of all buckets owned by the authenticated user",
				OperationID: "ListBuckets",
				Responses: map[string]Response{
					"200": {
						Description: "Successful response",
						Content: map[string]MediaType{
							"application/xml": {
								Schema: &Schema{
									Ref: "#/components/schemas/ListBucketsResponse",
								},
							},
						},
					},
					"403": {
						Description: "Access denied",
						Content: map[string]MediaType{
							"application/xml": {
								Schema: &Schema{
									Ref: "#/components/schemas/Error",
								},
							},
						},
					},
				},
			},
		},
		"/{bucket}": {
			Parameters: []Parameter{
				{
					Name:        "bucket",
					In:          "path",
					Description: "Bucket name",
					Required:    true,
					Schema:      &Schema{Type: "string"},
				},
			},
			Get: &Operation{
				Tags:        []string{"Objects"},
				Summary:     "List objects in bucket",
				Description: "Returns a list of objects in the specified bucket",
				OperationID: "ListObjects",
				Parameters: []Parameter{
					{
						Name:        "prefix",
						In:          "query",
						Description: "Limits results to objects beginning with prefix",
						Schema:      &Schema{Type: "string"},
					},
					{
						Name:        "delimiter",
						In:          "query",
						Description: "Delimiter for grouping objects",
						Schema:      &Schema{Type: "string"},
					},
					{
						Name:        "max-keys",
						In:          "query",
						Description: "Maximum number of objects to return",
						Schema:      &Schema{Type: "integer", Format: "int32"},
					},
				},
				Responses: map[string]Response{
					"200": {
						Description: "Successful response",
						Content: map[string]MediaType{
							"application/xml": {
								Schema: &Schema{
									Ref: "#/components/schemas/ListObjectsResponse",
								},
							},
						},
					},
					"404": {
						Description: "Bucket not found",
						Content: map[string]MediaType{
							"application/xml": {
								Schema: &Schema{
									Ref: "#/components/schemas/Error",
								},
							},
						},
					},
				},
			},
			Put: &Operation{
				Tags:        []string{"Buckets"},
				Summary:     "Create bucket",
				Description: "Creates a new bucket",
				OperationID: "CreateBucket",
				Responses: map[string]Response{
					"200": {
						Description: "Bucket created successfully",
					},
					"409": {
						Description: "Bucket already exists",
						Content: map[string]MediaType{
							"application/xml": {
								Schema: &Schema{
									Ref: "#/components/schemas/Error",
								},
							},
						},
					},
				},
			},
			Delete: &Operation{
				Tags:        []string{"Buckets"},
				Summary:     "Delete bucket",
				Description: "Deletes an empty bucket",
				OperationID: "DeleteBucket",
				Responses: map[string]Response{
					"204": {
						Description: "Bucket deleted successfully",
					},
					"404": {
						Description: "Bucket not found",
					},
					"409": {
						Description: "Bucket not empty",
					},
				},
			},
		},

		"/{bucket}/{key}": {
			Get: &Operation{
				Tags:        []string{"Objects"},
				Summary:     "Get object",
				Description: "Retrieves an object from a bucket",
				OperationID: "GetObject",
				Parameters: []Parameter{
					{
						Name:        "bucket",
						In:          "path",
						Description: "Bucket name",
						Required:    true,
						Schema:      &Schema{Type: "string"},
					},
					{
						Name:        "key",
						In:          "path",
						Description: "Object key",
						Required:    true,
						Schema:      &Schema{Type: "string"},
					},
				},
				Responses: map[string]Response{
					"200": {
						Description: "Object retrieved successfully",
						Headers: map[string]Header{
							"Content-Type": {
								Schema: &Schema{Type: "string"},
							},
							"Content-Length": {
								Schema: &Schema{Type: "integer"},
							},
							"ETag": {
								Schema: &Schema{Type: "string"},
							},
						},
						Content: map[string]MediaType{
							"application/octet-stream": {
								Schema: &Schema{
									Type:   "string",
									Format: "binary",
								},
							},
						},
					},
					"404": {
						Description: "Object not found",
						Content: map[string]MediaType{
							"application/xml": {
								Schema: &Schema{
									Ref: "#/components/schemas/Error",
								},
							},
						},
					},
				},
			},
			Put: &Operation{
				Tags:        []string{"Objects"},
				Summary:     "Upload object",
				Description: "Uploads an object to a bucket",
				OperationID: "PutObject",
				Parameters: []Parameter{
					{
						Name:        "bucket",
						In:          "path",
						Description: "Bucket name",
						Required:    true,
						Schema:      &Schema{Type: "string"},
					},
					{
						Name:        "key",
						In:          "path",
						Description: "Object key",
						Required:    true,
						Schema:      &Schema{Type: "string"},
					},
				},
				RequestBody: &RequestBody{
					Description: "Object data",
					Required:    true,
					Content: map[string]MediaType{
						"application/octet-stream": {
							Schema: &Schema{
								Type:   "string",
								Format: "binary",
							},
						},
					},
				},
				Responses: map[string]Response{
					"200": {
						Description: "Object uploaded successfully",
						Headers: map[string]Header{
							"ETag": {
								Schema: &Schema{Type: "string"},
							},
						},
					},
				},
			},
			Delete: &Operation{
				Tags:        []string{"Objects"},
				Summary:     "Delete object",
				Description: "Deletes an object from a bucket",
				OperationID: "DeleteObject",
				Parameters: []Parameter{
					{
						Name:        "bucket",
						In:          "path",
						Description: "Bucket name",
						Required:    true,
						Schema:      &Schema{Type: "string"},
					},
					{
						Name:        "key",
						In:          "path",
						Description: "Object key",
						Required:    true,
						Schema:      &Schema{Type: "string"},
					},
				},
				Responses: map[string]Response{
					"204": {
						Description: "Object deleted successfully",
					},
					"404": {
						Description: "Object not found",
					},
				},
			},
		},
	}
}

func generateSchemas() map[string]Schema {
	return map[string]Schema{
		"Bucket": {
			Type: "object",
			Properties: map[string]*Schema{
				"Name": {
					Type:        "string",
					Description: "Bucket name",
				},
				"CreationDate": {
					Type:        "string",
					Format:      "date-time",
					Description: "Bucket creation timestamp",
				},
			},
			Required: []string{"Name", "CreationDate"},
		},
		"ListBucketsResponse": {
			Type: "object",
			XML:  &XMLObject{Name: "ListAllMyBucketsResult"},
			Properties: map[string]*Schema{
				"Buckets": {
					Type: "array",
					XML:  &XMLObject{Name: "Buckets", Wrapped: true},
					Items: &Schema{
						Ref: "#/components/schemas/Bucket",
					},
				},
				"Owner": {
					Type: "object",
					Properties: map[string]*Schema{
						"ID": {
							Type: "string",
						},
						"DisplayName": {
							Type: "string",
						},
					},
				},
			},
		},
		"Object": {
			Type: "object",
			Properties: map[string]*Schema{
				"Key": {
					Type:        "string",
					Description: "Object key",
				},
				"LastModified": {
					Type:        "string",
					Format:      "date-time",
					Description: "Last modification timestamp",
				},
				"ETag": {
					Type:        "string",
					Description: "Entity tag",
				},
				"Size": {
					Type:        "integer",
					Format:      "int64",
					Description: "Object size in bytes",
				},
				"StorageClass": {
					Type:        "string",
					Description: "Storage class",
					Enum:        []interface{}{"STANDARD", "REDUCED_REDUNDANCY", "GLACIER"},
				},
			},
			Required: []string{"Key", "LastModified", "ETag", "Size"},
		},
		"ListObjectsResponse": {
			Type: "object",
			XML:  &XMLObject{Name: "ListBucketResult"},
			Properties: map[string]*Schema{
				"Name": {
					Type:        "string",
					Description: "Bucket name",
				},
				"Prefix": {
					Type:        "string",
					Description: "Object prefix",
				},
				"MaxKeys": {
					Type:        "integer",
					Description: "Maximum keys returned",
				},
				"IsTruncated": {
					Type:        "boolean",
					Description: "Whether the results were truncated",
				},
				"Contents": {
					Type: "array",
					XML:  &XMLObject{Name: "Contents"},
					Items: &Schema{
						Ref: "#/components/schemas/Object",
					},
				},
			},
		},
		"Error": {
			Type: "object",
			XML:  &XMLObject{Name: "Error"},
			Properties: map[string]*Schema{
				"Code": {
					Type:        "string",
					Description: "Error code",
					Example:     "NoSuchBucket",
				},
				"Message": {
					Type:        "string",
					Description: "Error message",
					Example:     "The specified bucket does not exist",
				},
				"Resource": {
					Type:        "string",
					Description: "Resource associated with error",
				},
				"RequestId": {
					Type:        "string",
					Description: "Request ID for debugging",
				},
			},
			Required: []string{"Code", "Message"},
		},
	}
}

func generateSecuritySchemes() map[string]SecurityScheme {
	return map[string]SecurityScheme{
		"ApiKeyAuth": {
			Type:        "apiKey",
			Description: "API key authentication",
			Name:        "X-API-Key",
			In:          "header",
		},
		"S3Signature": {
			Type:        "apiKey",
			Description: "AWS Signature Version 4",
			Name:        "Authorization",
			In:          "header",
		},
	}
}

// OpenAPIJSONHandler returns an HTTP handler that serves the OpenAPI spec as JSON
func OpenAPIJSONHandler() http.HandlerFunc {
	spec := GenerateOpenAPISpec()
	specJSON, _ := json.MarshalIndent(spec, "", "  ")

	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(specJSON)
	}
}

// SwaggerUIHandler returns an HTTP handler that serves Swagger UI
func SwaggerUIHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = fmt.Fprint(w, swaggerUIHTML)
	}
}

// swaggerUIHTML is the HTML for Swagger UI
const swaggerUIHTML = `<!DOCTYPE html>
<html lang="en">
<head>
    <meta charset="UTF-8">
    <title>Vaultaire API Documentation</title>
    <link rel="stylesheet" href="https://unpkg.com/swagger-ui-dist@5.10.3/swagger-ui.css">
    <style>
        html { box-sizing: border-box; overflow: -moz-scrollbars-vertical; overflow-y: scroll; }
        *, *:before, *:after { box-sizing: inherit; }
        body { margin: 0; background: #fafafa; }
    </style>
</head>
<body>
    <div id="swagger-ui"></div>
    <script src="https://unpkg.com/swagger-ui-dist@5.10.3/swagger-ui-bundle.js"></script>
    <script src="https://unpkg.com/swagger-ui-dist@5.10.3/swagger-ui-standalone-preset.js"></script>
    <script>
    window.onload = function() {
        window.ui = SwaggerUIBundle({
            url: "/openapi.json",
            dom_id: '#swagger-ui',
            deepLinking: true,
            presets: [
                SwaggerUIBundle.presets.apis,
                SwaggerUIStandalonePreset
            ],
            plugins: [
                SwaggerUIBundle.plugins.DownloadUrl
            ],
            layout: "StandaloneLayout"
        });
    };
    </script>
</body>
</html>`
