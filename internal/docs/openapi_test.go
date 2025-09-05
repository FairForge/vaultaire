// internal/docs/openapi_test.go
package docs

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestOpenAPISpec_Generation(t *testing.T) {
	t.Run("generates valid OpenAPI 3.0 spec", func(t *testing.T) {
		spec := GenerateOpenAPISpec()

		assert.Equal(t, "3.0.3", spec.OpenAPI)
		assert.Equal(t, "Vaultaire Storage API", spec.Info.Title)
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

		assert.Contains(t, spec.Components.SecuritySchemes, "ApiKeyAuth")
		assert.Contains(t, spec.Components.SecuritySchemes, "S3Signature")
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
