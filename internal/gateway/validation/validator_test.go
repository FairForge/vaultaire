// internal/gateway/validation/validator_test.go
package validation

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestRequestValidator_ValidateContentType(t *testing.T) {
	tests := []struct {
		name        string
		contentType string
		allowed     []string
		wantErr     bool
	}{
		{
			name:        "valid json content type",
			contentType: "application/json",
			allowed:     []string{"application/json", "application/xml"},
			wantErr:     false,
		},
		{
			name:        "valid json with charset",
			contentType: "application/json; charset=utf-8",
			allowed:     []string{"application/json"},
			wantErr:     false,
		},
		{
			name:        "invalid content type",
			contentType: "text/plain",
			allowed:     []string{"application/json"},
			wantErr:     true,
		},
		{
			name:        "empty content type when required",
			contentType: "",
			allowed:     []string{"application/json"},
			wantErr:     true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			validator := NewRequestValidator()
			req := httptest.NewRequest(http.MethodPost, "/test", nil)
			if tt.contentType != "" {
				req.Header.Set("Content-Type", tt.contentType)
			}

			err := validator.ValidateContentType(req, tt.allowed)
			if tt.wantErr {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

func TestRequestValidator_ValidateHeaders(t *testing.T) {
	tests := []struct {
		name     string
		headers  map[string]string
		rules    HeaderRules
		wantErr  bool
		errorMsg string
	}{
		{
			name: "all required headers present",
			headers: map[string]string{
				"X-Tenant-ID": "tenant-123",
				"X-API-Key":   "key-123",
			},
			rules: HeaderRules{
				Required: []string{"X-Tenant-ID", "X-API-Key"},
			},
			wantErr: false,
		},
		{
			name: "missing required header",
			headers: map[string]string{
				"X-API-Key": "key-123",
			},
			rules: HeaderRules{
				Required: []string{"X-Tenant-ID", "X-API-Key"},
			},
			wantErr:  true,
			errorMsg: "missing required header: X-Tenant-ID",
		},
		{
			name: "header pattern validation",
			headers: map[string]string{
				"X-Request-ID": "req-123-456",
			},
			rules: HeaderRules{
				Patterns: map[string]string{
					"X-Request-ID": `^req-[\d-]+$`,
				},
			},
			wantErr: false,
		},
		{
			name: "invalid header pattern",
			headers: map[string]string{
				"X-Request-ID": "invalid",
			},
			rules: HeaderRules{
				Patterns: map[string]string{
					"X-Request-ID": `^req-[\d-]+$`,
				},
			},
			wantErr:  true,
			errorMsg: "header X-Request-ID does not match pattern",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			validator := NewRequestValidator()
			req := httptest.NewRequest(http.MethodPost, "/test", nil)

			for k, v := range tt.headers {
				req.Header.Set(k, v)
			}

			err := validator.ValidateHeaders(req, tt.rules)
			if tt.wantErr {
				assert.Error(t, err)
				if tt.errorMsg != "" {
					assert.Contains(t, err.Error(), tt.errorMsg)
				}
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

func TestRequestValidator_ValidateQueryParams(t *testing.T) {
	tests := []struct {
		name     string
		query    string
		rules    QueryRules
		wantErr  bool
		errorMsg string
	}{
		{
			name:  "valid required parameters",
			query: "page=1&limit=10",
			rules: QueryRules{
				Required: []string{"page", "limit"},
			},
			wantErr: false,
		},
		{
			name:  "missing required parameter",
			query: "page=1",
			rules: QueryRules{
				Required: []string{"page", "limit"},
			},
			wantErr:  true,
			errorMsg: "missing required parameter: limit",
		},
		{
			name:  "valid parameter types",
			query: "page=1&limit=10&active=true",
			rules: QueryRules{
				Types: map[string]ParamType{
					"page":   ParamTypeInt,
					"limit":  ParamTypeInt,
					"active": ParamTypeBool,
				},
			},
			wantErr: false,
		},
		{
			name:  "invalid integer parameter",
			query: "page=abc",
			rules: QueryRules{
				Types: map[string]ParamType{
					"page": ParamTypeInt,
				},
			},
			wantErr:  true,
			errorMsg: "parameter page must be an integer",
		},
		{
			name:  "parameter range validation",
			query: "limit=100",
			rules: QueryRules{
				Ranges: map[string]Range{
					"limit": {Min: 1, Max: 50},
				},
			},
			wantErr:  true,
			errorMsg: "parameter limit must be between 1 and 50",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			validator := NewRequestValidator()
			req := httptest.NewRequest(http.MethodGet, "/test?"+tt.query, nil)

			err := validator.ValidateQueryParams(req, tt.rules)
			if tt.wantErr {
				assert.Error(t, err)
				if tt.errorMsg != "" {
					assert.Contains(t, err.Error(), tt.errorMsg)
				}
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

func TestRequestValidator_ValidateJSONSchema(t *testing.T) {
	schema := `{
		"type": "object",
		"required": ["name", "age"],
		"properties": {
			"name": {"type": "string", "minLength": 1, "maxLength": 100},
			"age": {"type": "number", "minimum": 0, "maximum": 150},
			"email": {"type": "string", "format": "email"}
		}
	}`

	tests := []struct {
		name    string
		body    interface{}
		wantErr bool
	}{
		{
			name: "valid request body",
			body: map[string]interface{}{
				"name":  "John Doe",
				"age":   30,
				"email": "john@example.com",
			},
			wantErr: false,
		},
		{
			name: "missing required field",
			body: map[string]interface{}{
				"name": "John Doe",
			},
			wantErr: true,
		},
		{
			name: "invalid email format",
			body: map[string]interface{}{
				"name":  "John Doe",
				"age":   30,
				"email": "not-an-email",
			},
			wantErr: true,
		},
		{
			name: "age out of range",
			body: map[string]interface{}{
				"name": "John Doe",
				"age":  200,
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			validator := NewRequestValidator()

			bodyBytes, _ := json.Marshal(tt.body)
			req := httptest.NewRequest(http.MethodPost, "/test", bytes.NewReader(bodyBytes))
			req.Header.Set("Content-Type", "application/json")

			err := validator.ValidateJSONSchema(req, schema)
			if tt.wantErr {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

func TestRequestValidator_ValidateContentLength(t *testing.T) {
	tests := []struct {
		name     string
		body     string
		maxSize  int64
		wantErr  bool
		errorMsg string
	}{
		{
			name:    "body within limit",
			body:    "small body",
			maxSize: 100,
			wantErr: false,
		},
		{
			name:     "body exceeds limit",
			body:     strings.Repeat("x", 101),
			maxSize:  100,
			wantErr:  true,
			errorMsg: "request body too large",
		},
		{
			name:    "empty body allowed",
			body:    "",
			maxSize: 100,
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			validator := NewRequestValidator()
			req := httptest.NewRequest(http.MethodPost, "/test", strings.NewReader(tt.body))
			req.ContentLength = int64(len(tt.body))

			err := validator.ValidateContentLength(req, tt.maxSize)
			if tt.wantErr {
				assert.Error(t, err)
				if tt.errorMsg != "" {
					assert.Contains(t, err.Error(), tt.errorMsg)
				}
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

func TestValidationMiddleware(t *testing.T) {
	t.Run("successful validation", func(t *testing.T) {
		validator := NewRequestValidator()
		rules := &ValidationRules{
			ContentTypes: []string{"application/json"},
			MaxBodySize:  1024,
			Headers: HeaderRules{
				Required: []string{"X-API-Key"},
			},
		}

		handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("success"))
		})

		middleware := ValidationMiddleware(validator, rules)
		testHandler := middleware(handler)

		req := httptest.NewRequest(http.MethodPost, "/test", strings.NewReader("{}"))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-API-Key", "test-key")
		req.ContentLength = 2

		rec := httptest.NewRecorder()
		testHandler.ServeHTTP(rec, req)

		assert.Equal(t, http.StatusOK, rec.Code)
		assert.Equal(t, "success", rec.Body.String())
	})

	t.Run("validation failure returns 400", func(t *testing.T) {
		validator := NewRequestValidator()
		rules := &ValidationRules{
			Headers: HeaderRules{
				Required: []string{"X-API-Key"},
			},
		}

		handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			t.Fatal("handler should not be called")
		})

		middleware := ValidationMiddleware(validator, rules)
		testHandler := middleware(handler)

		req := httptest.NewRequest(http.MethodPost, "/test", nil)
		rec := httptest.NewRecorder()
		testHandler.ServeHTTP(rec, req)

		assert.Equal(t, http.StatusBadRequest, rec.Code)
		assert.Contains(t, rec.Body.String(), "validation error")
	})
}

// Benchmark test
func BenchmarkRequestValidator_ValidateHeaders(b *testing.B) {
	validator := NewRequestValidator()
	req := httptest.NewRequest(http.MethodPost, "/test", nil)
	req.Header.Set("X-Tenant-ID", "tenant-123")
	req.Header.Set("X-API-Key", "key-123")

	rules := HeaderRules{
		Required: []string{"X-Tenant-ID", "X-API-Key"},
		Patterns: map[string]string{
			"X-Tenant-ID": `^tenant-\d+$`,
			"X-API-Key":   `^key-\d+$`,
		},
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = validator.ValidateHeaders(req, rules)
	}
}
