// internal/docs/openapi_test.go
package docs

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestOpenAPISpec_Generation(t *testing.T) {
	t.Run("generates valid OpenAPI 3.0 spec", func(t *testing.T) {
		spec := GenerateOpenAPISpec()

		assert.Equal(t, "3.0.3", spec.OpenAPI)
		assert.Equal(t, "stored.ge Storage API", spec.Info.Title)
		assert.NotEmpty(t, spec.Info.Version)
		assert.NotEmpty(t, spec.Paths)
		assert.NotEmpty(t, spec.Components.Schemas)
	})

	t.Run("includes S3 operations", func(t *testing.T) {
		spec := GenerateOpenAPISpec()

		// Check for key S3 operations
		assert.Contains(t, spec.Paths, "/")
		assert.Contains(t, spec.Paths, "/{bucket}")
		assert.Contains(t, spec.Paths, "/{bucket}/{key}")
	})

	t.Run("includes authentication schemas", func(t *testing.T) {
		spec := GenerateOpenAPISpec()

		// Review R14: the JSON API is Bearer JWT; X-API-Key auth was
		// deleted in R11 and must not be advertised.
		assert.Contains(t, spec.Components.SecuritySchemes, "BearerAuth")
		assert.Equal(t, "http", spec.Components.SecuritySchemes["BearerAuth"].Type)
		assert.Equal(t, "bearer", spec.Components.SecuritySchemes["BearerAuth"].Scheme)
		assert.Contains(t, spec.Components.SecuritySchemes, "S3Signature")
		assert.NotContains(t, spec.Components.SecuritySchemes, "ApiKeyAuth")
		assert.Empty(t, spec.Security, "no top-level default security: public routes carry none, the rest declare their own")
		assert.Equal(t, "1.1.0", spec.Info.Version)
	})

	t.Run("generates JSON correctly", func(t *testing.T) {
		spec := GenerateOpenAPISpec()

		data, err := json.MarshalIndent(spec, "", "  ")
		require.NoError(t, err)
		assert.NotEmpty(t, data)

		// Verify it's valid JSON
		var parsed map[string]interface{}
		err = json.Unmarshal(data, &parsed)
		assert.NoError(t, err)
	})
}

func TestSwaggerUIHandler(t *testing.T) {
	t.Run("serves Swagger UI HTML", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/docs", nil)
		rec := httptest.NewRecorder()

		handler := SwaggerUIHandler()
		handler.ServeHTTP(rec, req)

		assert.Equal(t, http.StatusOK, rec.Code)
		assert.Contains(t, rec.Header().Get("Content-Type"), "text/html")
		assert.Contains(t, rec.Body.String(), "swagger-ui")
		assert.Contains(t, rec.Body.String(), "/openapi.json")
	})
}

func TestOpenAPIJSONHandler(t *testing.T) {
	t.Run("serves OpenAPI spec as JSON", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/openapi.json", nil)
		rec := httptest.NewRecorder()

		handler := OpenAPIJSONHandler()
		handler.ServeHTTP(rec, req)

		assert.Equal(t, http.StatusOK, rec.Code)
		assert.Equal(t, "application/json", rec.Header().Get("Content-Type"))

		var spec OpenAPISpec
		err := json.NewDecoder(rec.Body).Decode(&spec)
		require.NoError(t, err)
		assert.Equal(t, "3.0.3", spec.OpenAPI)
	})
}

func TestOpenAPISpec_Operations(t *testing.T) {
	spec := GenerateOpenAPISpec()

	t.Run("ListBuckets operation", func(t *testing.T) {
		path, exists := spec.Paths["/"]
		require.True(t, exists)
		require.NotNil(t, path.Get)

		assert.Equal(t, "ListBuckets", path.Get.OperationID)
		assert.Equal(t, "List all buckets", path.Get.Summary)
		assert.Contains(t, path.Get.Tags, "Buckets")
		assert.NotEmpty(t, path.Get.Responses["200"])
	})

	t.Run("GetObject operation", func(t *testing.T) {
		path, exists := spec.Paths["/{bucket}/{key}"]
		require.True(t, exists)
		require.NotNil(t, path.Get)

		assert.Equal(t, "GetObject", path.Get.OperationID)
		assert.Len(t, path.Get.Parameters, 2) // bucket and key
		assert.NotEmpty(t, path.Get.Responses["200"])
		assert.NotEmpty(t, path.Get.Responses["404"])
	})

	t.Run("PutObject operation", func(t *testing.T) {
		path, exists := spec.Paths["/{bucket}/{key}"]
		require.True(t, exists)
		require.NotNil(t, path.Put)

		assert.Equal(t, "PutObject", path.Put.OperationID)
		assert.NotNil(t, path.Put.RequestBody)
		assert.NotEmpty(t, path.Put.Responses["200"])
	})
}

func TestOpenAPISpec_Schemas(t *testing.T) {
	spec := GenerateOpenAPISpec()

	t.Run("Bucket schema", func(t *testing.T) {
		schema, exists := spec.Components.Schemas["Bucket"]
		require.True(t, exists)

		assert.Equal(t, "object", schema.Type)
		assert.Contains(t, schema.Properties, "Name")
		assert.Contains(t, schema.Properties, "CreationDate")
	})

	t.Run("Error schema", func(t *testing.T) {
		schema, exists := spec.Components.Schemas["Error"]
		require.True(t, exists)

		assert.Equal(t, "object", schema.Type)
		assert.Contains(t, schema.Properties, "Code")
		assert.Contains(t, schema.Properties, "Message")
	})

	t.Run("ListBucketsResponse schema", func(t *testing.T) {
		schema, exists := spec.Components.Schemas["ListBucketsResponse"]
		require.True(t, exists)

		assert.Contains(t, schema.Properties, "Buckets")
		assert.Contains(t, schema.Properties, "Owner")
	})
}

// operations flattens the spec into (method, path, op) triples.
func operations(spec *OpenAPISpec) []struct {
	Method, Path string
	Op           *Operation
} {
	var out []struct {
		Method, Path string
		Op           *Operation
	}
	for path, item := range spec.Paths {
		for method, op := range map[string]*Operation{
			"GET": item.Get, "PUT": item.Put, "POST": item.Post, "PATCH": item.Patch,
			"DELETE": item.Delete, "HEAD": item.Head, "OPTIONS": item.Options,
		} {
			if op != nil {
				out = append(out, struct {
					Method, Path string
					Op           *Operation
				}{method, path, op})
			}
		}
	}
	return out
}

// TestOpenAPISpec_JSONAPICoverage — Review R14: the spec documents the whole
// JSON API (the drift guard in internal/api ties it to the router; this
// checks the shape of what is documented).
func TestOpenAPISpec_JSONAPICoverage(t *testing.T) {
	spec := GenerateOpenAPISpec()
	ops := operations(spec)
	require.GreaterOrEqual(t, len(ops), 59, "7 S3 + 52 JSON operations")

	t.Run("every operation has a unique operationId, a tag and a success response", func(t *testing.T) {
		seen := map[string]string{}
		known := map[string]bool{}
		for _, tag := range spec.Tags {
			known[tag.Name] = true
		}
		for _, o := range ops {
			key := o.Method + " " + o.Path
			require.NotEmpty(t, o.Op.OperationID, "%s has no operationId", key)
			if prev, dup := seen[o.Op.OperationID]; dup {
				t.Errorf("operationId %q used by both %s and %s", o.Op.OperationID, prev, key)
			}
			seen[o.Op.OperationID] = key
			require.NotEmpty(t, o.Op.Tags, "%s has no tag", key)
			for _, tag := range o.Op.Tags {
				assert.True(t, known[tag], "%s uses undeclared tag %q", key, tag)
			}
			success := false
			for code := range o.Op.Responses {
				if strings.HasPrefix(code, "2") {
					success = true
				}
			}
			assert.True(t, success, "%s documents no 2xx response", key)
		}
	})

	t.Run("every secured operation documents 401; public ones are the known set", func(t *testing.T) {
		public := map[string]bool{
			"POST /auth/register": true, "POST /auth/login": true,
			"POST /auth/password-reset": true, "POST /auth/password-reset/complete": true,
			"POST /api/waitlist": true,
		}
		for _, o := range ops {
			key := o.Method + " " + o.Path
			if len(o.Op.Security) == 0 {
				assert.True(t, public[key], "%s has no security requirement but is not a known public route", key)
				continue
			}
			if _, s3 := o.Op.Security[0]["S3Signature"]; s3 {
				continue // SigV4 failures are 403 AccessDenied on the S3 wire
			}
			_, has401 := o.Op.Responses["401"]
			assert.True(t, has401, "%s requires Bearer auth but documents no 401", key)
		}
	})

	t.Run("admin operations require the admin role and document 403", func(t *testing.T) {
		for _, o := range ops {
			if !strings.HasPrefix(o.Path, "/api/v1/admin/") {
				continue
			}
			key := o.Method + " " + o.Path
			assert.Contains(t, o.Op.Description, "role `admin`", "%s must say it is admin-only", key)
			_, has403 := o.Op.Responses["403"]
			assert.True(t, has403, "%s documents no 403", key)
			assert.Contains(t, o.Op.Tags, "Admin", key)
		}
	})

	t.Run("rate-limited groups document 429 and the X-RateLimit headers", func(t *testing.T) {
		for _, o := range ops {
			if !strings.HasPrefix(o.Path, "/api/v1/manage") && !strings.HasPrefix(o.Path, "/api/v1/webhooks") && o.Path != "/api/v1/events" {
				continue
			}
			key := o.Method + " " + o.Path
			assert.Equal(t, "#/components/responses/RateLimited", o.Op.Responses["429"].Ref, "%s", key)
			for code, r := range o.Op.Responses {
				if strings.HasPrefix(code, "2") {
					assert.Contains(t, r.Headers, "X-RateLimit-Remaining", "%s %s", key, code)
				}
			}
			if o.Method != "GET" && o.Path != "/api/v1/events" {
				require.NotEmpty(t, o.Op.Parameters, "%s mutation must take Idempotency-Key", key)
				assert.Equal(t, "#/components/parameters/IdempotencyKey", o.Op.Parameters[0].Ref, "%s", key)
			}
		}
	})

	t.Run("every $ref resolves to a component", func(t *testing.T) {
		data, err := json.Marshal(spec)
		require.NoError(t, err)
		var raw map[string]any
		require.NoError(t, json.Unmarshal(data, &raw))
		components := raw["components"].(map[string]any)
		var walk func(v any)
		walk = func(v any) {
			switch x := v.(type) {
			case map[string]any:
				if r, ok := x["$ref"].(string); ok {
					parts := strings.Split(strings.TrimPrefix(r, "#/components/"), "/")
					require.Len(t, parts, 2, "bad ref %q", r)
					group, _ := components[parts[0]].(map[string]any)
					_, found := group[parts[1]]
					assert.True(t, found, "$ref %q does not resolve", r)
				}
				for _, child := range x {
					walk(child)
				}
			case []any:
				for _, child := range x {
					walk(child)
				}
			}
		}
		walk(raw)
	})

	t.Run("documents the R11 facts", func(t *testing.T) {
		sts := spec.Paths["/api/v1/sts/token"].Post
		require.NotNil(t, sts)
		assert.Contains(t, spec.Components.Schemas["STSRequest"].Properties, "parent_key_id")

		audit := spec.Paths["/api/v1/admin/audit"].Get
		require.NotNil(t, audit)
		var names []string
		for _, p := range audit.Parameters {
			names = append(names, p.Name+p.Ref)
		}
		for _, want := range []string{"tenant_id", "user_id", "action", "event_type", "limit", "#/components/parameters/Cursor"} {
			assert.Contains(t, names, want)
		}

		create := spec.Paths["/api/v1/manage/buckets"].Post
		require.NotNil(t, create)
		assert.Contains(t, spec.Components.Schemas["ManagedBucketCreateRequest"].Properties, "region")
		assert.Contains(t, create.Responses, "201")
		assert.Contains(t, create.Responses, "403")

		wh := spec.Paths["/api/v1/webhooks"].Post
		require.NotNil(t, wh)
		assert.Contains(t, wh.Description, "loopback")

		assert.NotNil(t, spec.Paths["/api/v1/manage/buckets/{name}"].Patch, "PATCH is documented now that PathItem has the field")
	})
}
