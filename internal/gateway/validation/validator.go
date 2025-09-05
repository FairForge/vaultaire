// internal/gateway/validation/validator.go
package validation

import (
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"github.com/xeipuuv/gojsonschema"
)

// ParamType represents the type of a query parameter
type ParamType string

const (
	ParamTypeString ParamType = "string"
	ParamTypeInt    ParamType = "int"
	ParamTypeBool   ParamType = "bool"
	ParamTypeFloat  ParamType = "float"
)

// Range defines min and max values for numeric parameters
type Range struct {
	Min float64
	Max float64
}

// HeaderRules defines validation rules for headers
type HeaderRules struct {
	Required []string
	Patterns map[string]string
}

// QueryRules defines validation rules for query parameters
type QueryRules struct {
	Required []string
	Types    map[string]ParamType
	Patterns map[string]string
	Ranges   map[string]Range
}

// ValidationRules defines all validation rules for a request
type ValidationRules struct {
	ContentTypes []string
	MaxBodySize  int64
	Headers      HeaderRules
	Query        QueryRules
	JSONSchema   string
}

// RequestValidator handles request validation
type RequestValidator struct {
	patternCache map[string]*regexp.Regexp
}

// NewRequestValidator creates a new request validator
func NewRequestValidator() *RequestValidator {
	return &RequestValidator{
		patternCache: make(map[string]*regexp.Regexp),
	}
}

// ValidateContentType validates the request content type
func (v *RequestValidator) ValidateContentType(r *http.Request, allowed []string) error {
	if len(allowed) == 0 {
		return nil
	}

	contentType := r.Header.Get("Content-Type")
	if contentType == "" {
		return fmt.Errorf("content-type header is required")
	}

	// Handle content type with charset or other parameters
	parts := strings.Split(contentType, ";")
	contentType = strings.TrimSpace(parts[0])

	for _, ct := range allowed {
		if strings.EqualFold(contentType, ct) {
			return nil
		}
	}

	return fmt.Errorf("invalid content-type: %s, allowed: %v", contentType, allowed)
}

// ValidateHeaders validates request headers against rules
func (v *RequestValidator) ValidateHeaders(r *http.Request, rules HeaderRules) error {
	// Check required headers
	for _, required := range rules.Required {
		if r.Header.Get(required) == "" {
			return fmt.Errorf("missing required header: %s", required)
		}
	}

	// Validate header patterns
	for header, pattern := range rules.Patterns {
		value := r.Header.Get(header)
		if value == "" {
			continue
		}

		re, err := v.getPattern(pattern)
		if err != nil {
			return fmt.Errorf("invalid pattern for header %s: %w", header, err)
		}

		if !re.MatchString(value) {
			return fmt.Errorf("header %s does not match pattern", header)
		}
	}

	return nil
}

// ValidateQueryParams validates query parameters against rules
func (v *RequestValidator) ValidateQueryParams(r *http.Request, rules QueryRules) error {
	params := r.URL.Query()

	// Check required parameters
	for _, required := range rules.Required {
		if params.Get(required) == "" {
			return fmt.Errorf("missing required parameter: %s", required)
		}
	}

	// Validate parameter types
	for param, paramType := range rules.Types {
		value := params.Get(param)
		if value == "" {
			continue
		}

		switch paramType {
		case ParamTypeInt:
			if _, err := strconv.Atoi(value); err != nil {
				return fmt.Errorf("parameter %s must be an integer", param)
			}
		case ParamTypeBool:
			if _, err := strconv.ParseBool(value); err != nil {
				return fmt.Errorf("parameter %s must be a boolean", param)
			}
		case ParamTypeFloat:
			if _, err := strconv.ParseFloat(value, 64); err != nil {
				return fmt.Errorf("parameter %s must be a number", param)
			}
		}
	}

	// Validate parameter patterns
	for param, pattern := range rules.Patterns {
		value := params.Get(param)
		if value == "" {
			continue
		}

		re, err := v.getPattern(pattern)
		if err != nil {
			return fmt.Errorf("invalid pattern for parameter %s: %w", param, err)
		}

		if !re.MatchString(value) {
			return fmt.Errorf("parameter %s does not match pattern", param)
		}
	}

	// Validate numeric ranges
	for param, r := range rules.Ranges {
		value := params.Get(param)
		if value == "" {
			continue
		}

		num, err := strconv.ParseFloat(value, 64)
		if err != nil {
			return fmt.Errorf("parameter %s must be numeric for range validation", param)
		}

		if num < r.Min || num > r.Max {
			return fmt.Errorf("parameter %s must be between %.0f and %.0f", param, r.Min, r.Max)
		}
	}

	return nil
}

// ValidateJSONSchema validates request body against a JSON schema
func (v *RequestValidator) ValidateJSONSchema(r *http.Request, schemaStr string) error {
	if schemaStr == "" {
		return nil
	}

	// Read body
	bodyBytes, err := io.ReadAll(r.Body)
	if err != nil {
		return fmt.Errorf("failed to read request body: %w", err)
	}
	// Reset body for downstream handlers
	r.Body = io.NopCloser(strings.NewReader(string(bodyBytes)))

	// Parse schema
	schemaLoader := gojsonschema.NewStringLoader(schemaStr)

	// Parse document
	documentLoader := gojsonschema.NewBytesLoader(bodyBytes)

	// Validate
	result, err := gojsonschema.Validate(schemaLoader, documentLoader)
	if err != nil {
		return fmt.Errorf("schema validation error: %w", err)
	}

	if !result.Valid() {
		errors := make([]string, 0, len(result.Errors()))
		for _, err := range result.Errors() {
			errors = append(errors, err.String())
		}
		return fmt.Errorf("validation failed: %s", strings.Join(errors, "; "))
	}

	return nil
}

// ValidateContentLength validates that the request body doesn't exceed max size
func (v *RequestValidator) ValidateContentLength(r *http.Request, maxSize int64) error {
	if maxSize <= 0 {
		return nil
	}

	if r.ContentLength > maxSize {
		return fmt.Errorf("request body too large: %d bytes (max: %d)", r.ContentLength, maxSize)
	}

	return nil
}

// Validate performs all configured validations
func (v *RequestValidator) Validate(r *http.Request, rules *ValidationRules) error {
	if rules == nil {
		return nil
	}

	// Validate content type
	if len(rules.ContentTypes) > 0 {
		if err := v.ValidateContentType(r, rules.ContentTypes); err != nil {
			return err
		}
	}

	// Validate content length
	if rules.MaxBodySize > 0 {
		if err := v.ValidateContentLength(r, rules.MaxBodySize); err != nil {
			return err
		}
	}

	// Validate headers
	if err := v.ValidateHeaders(r, rules.Headers); err != nil {
		return err
	}

	// Validate query parameters
	if err := v.ValidateQueryParams(r, rules.Query); err != nil {
		return err
	}

	// Validate JSON schema
	if rules.JSONSchema != "" && r.Method != http.MethodGet {
		if err := v.ValidateJSONSchema(r, rules.JSONSchema); err != nil {
			return err
		}
	}

	return nil
}

// getPattern retrieves a compiled regex pattern from cache or compiles it
func (v *RequestValidator) getPattern(pattern string) (*regexp.Regexp, error) {
	if re, ok := v.patternCache[pattern]; ok {
		return re, nil
	}

	re, err := regexp.Compile(pattern)
	if err != nil {
		return nil, err
	}

	v.patternCache[pattern] = re
	return re, nil
}

// ValidationMiddleware creates an HTTP middleware for request validation
func ValidationMiddleware(validator *RequestValidator, rules *ValidationRules) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if err := validator.Validate(r, rules); err != nil {
				http.Error(w, fmt.Sprintf("validation error: %s", err), http.StatusBadRequest)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
