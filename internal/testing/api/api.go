// Package api provides utilities for API testing.
package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"
)

// Client is an API testing client.
type Client struct {
	BaseURL    string
	HTTPClient *http.Client
	Headers    map[string]string
	Timeout    time.Duration
}

// NewClient creates an API test client.
func NewClient(baseURL string) *Client {
	return &Client{
		BaseURL: strings.TrimSuffix(baseURL, "/"),
		HTTPClient: &http.Client{
			Timeout: 30 * time.Second,
		},
		Headers: make(map[string]string),
		Timeout: 30 * time.Second,
	}
}

// SetHeader sets a default header for all requests.
func (c *Client) SetHeader(key, value string) {
	c.Headers[key] = value
}

// SetAuth sets the Authorization header.
func (c *Client) SetAuth(token string) {
	c.Headers["Authorization"] = "Bearer " + token
}

// SetBasicAuth sets basic authentication.
func (c *Client) SetBasicAuth(username, password string) {
	c.Headers["Authorization"] = "Basic " + basicAuth(username, password)
}

// basicAuth encodes basic auth credentials.
func basicAuth(username, password string) string {
	auth := username + ":" + password
	return base64Encode([]byte(auth))
}

// base64Encode encodes bytes to base64.
func base64Encode(data []byte) string {
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"
	var result strings.Builder

	for i := 0; i < len(data); i += 3 {
		var n uint32
		remaining := len(data) - i

		n = uint32(data[i]) << 16
		if remaining > 1 {
			n |= uint32(data[i+1]) << 8
		}
		if remaining > 2 {
			n |= uint32(data[i+2])
		}

		result.WriteByte(alphabet[(n>>18)&0x3F])
		result.WriteByte(alphabet[(n>>12)&0x3F])

		if remaining > 1 {
			result.WriteByte(alphabet[(n>>6)&0x3F])
		} else {
			result.WriteByte('=')
		}

		if remaining > 2 {
			result.WriteByte(alphabet[n&0x3F])
		} else {
			result.WriteByte('=')
		}
	}

	return result.String()
}

// Request represents an API request.
type Request struct {
	Method  string
	Path    string
	Headers map[string]string
	Query   map[string]string
	Body    any
}

// Response represents an API response.
type Response struct {
	StatusCode int
	Headers    http.Header
	Body       []byte
	Duration   time.Duration
}

// JSON decodes the response body as JSON.
func (r *Response) JSON(v any) error {
	return json.Unmarshal(r.Body, v)
}

// String returns the response body as string.
func (r *Response) String() string {
	return string(r.Body)
}

// Do executes an API request.
func (c *Client) Do(ctx context.Context, req Request) (*Response, error) {
	url := c.BaseURL + req.Path

	// Add query parameters
	if len(req.Query) > 0 {
		params := make([]string, 0, len(req.Query))
		for k, v := range req.Query {
			params = append(params, fmt.Sprintf("%s=%s", k, v))
		}
		url += "?" + strings.Join(params, "&")
	}

	// Prepare body
	var bodyReader io.Reader
	if req.Body != nil {
		switch b := req.Body.(type) {
		case string:
			bodyReader = strings.NewReader(b)
		case []byte:
			bodyReader = bytes.NewReader(b)
		case io.Reader:
			bodyReader = b
		default:
			jsonBody, err := json.Marshal(b)
			if err != nil {
				return nil, fmt.Errorf("marshaling request body: %w", err)
			}
			bodyReader = bytes.NewReader(jsonBody)
		}
	}

	// Create request
	httpReq, err := http.NewRequestWithContext(ctx, req.Method, url, bodyReader)
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}

	// Set default headers
	for k, v := range c.Headers {
		httpReq.Header.Set(k, v)
	}

	// Set request-specific headers
	for k, v := range req.Headers {
		httpReq.Header.Set(k, v)
	}

	// Set Content-Type if body present and not already set
	if req.Body != nil && httpReq.Header.Get("Content-Type") == "" {
		httpReq.Header.Set("Content-Type", "application/json")
	}

	// Execute
	start := time.Now()
	httpResp, err := c.HTTPClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("executing request: %w", err)
	}
	defer func() { _ = httpResp.Body.Close() }()

	// Read body
	body, err := io.ReadAll(httpResp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading response body: %w", err)
	}

	return &Response{
		StatusCode: httpResp.StatusCode,
		Headers:    httpResp.Header,
		Body:       body,
		Duration:   time.Since(start),
	}, nil
}

// GET performs a GET request.
func (c *Client) GET(ctx context.Context, path string) (*Response, error) {
	return c.Do(ctx, Request{Method: http.MethodGet, Path: path})
}

// POST performs a POST request.
func (c *Client) POST(ctx context.Context, path string, body any) (*Response, error) {
	return c.Do(ctx, Request{Method: http.MethodPost, Path: path, Body: body})
}

// PUT performs a PUT request.
func (c *Client) PUT(ctx context.Context, path string, body any) (*Response, error) {
	return c.Do(ctx, Request{Method: http.MethodPut, Path: path, Body: body})
}

// PATCH performs a PATCH request.
func (c *Client) PATCH(ctx context.Context, path string, body any) (*Response, error) {
	return c.Do(ctx, Request{Method: http.MethodPatch, Path: path, Body: body})
}

// DELETE performs a DELETE request.
func (c *Client) DELETE(ctx context.Context, path string) (*Response, error) {
	return c.Do(ctx, Request{Method: http.MethodDelete, Path: path})
}

// Validator validates API responses.
type Validator struct {
	t *testing.T
}

// NewValidator creates a response validator.
func NewValidator(t *testing.T) *Validator {
	return &Validator{t: t}
}

// Status asserts the response status code.
func (v *Validator) Status(resp *Response, expected int) *Validator {
	v.t.Helper()
	if resp.StatusCode != expected {
		v.t.Errorf("expected status %d, got %d. Body: %s", expected, resp.StatusCode, resp.String())
	}
	return v
}

// StatusOK asserts status 200.
func (v *Validator) StatusOK(resp *Response) *Validator {
	return v.Status(resp, http.StatusOK)
}

// StatusCreated asserts status 201.
func (v *Validator) StatusCreated(resp *Response) *Validator {
	return v.Status(resp, http.StatusCreated)
}

// StatusNoContent asserts status 204.
func (v *Validator) StatusNoContent(resp *Response) *Validator {
	return v.Status(resp, http.StatusNoContent)
}

// StatusBadRequest asserts status 400.
func (v *Validator) StatusBadRequest(resp *Response) *Validator {
	return v.Status(resp, http.StatusBadRequest)
}

// StatusUnauthorized asserts status 401.
func (v *Validator) StatusUnauthorized(resp *Response) *Validator {
	return v.Status(resp, http.StatusUnauthorized)
}

// StatusForbidden asserts status 403.
func (v *Validator) StatusForbidden(resp *Response) *Validator {
	return v.Status(resp, http.StatusForbidden)
}

// StatusNotFound asserts status 404.
func (v *Validator) StatusNotFound(resp *Response) *Validator {
	return v.Status(resp, http.StatusNotFound)
}

// Header asserts a header value.
func (v *Validator) Header(resp *Response, key, expected string) *Validator {
	v.t.Helper()
	actual := resp.Headers.Get(key)
	if actual != expected {
		v.t.Errorf("expected header %s=%q, got %q", key, expected, actual)
	}
	return v
}

// HeaderContains asserts a header contains a value.
func (v *Validator) HeaderContains(resp *Response, key, substring string) *Validator {
	v.t.Helper()
	actual := resp.Headers.Get(key)
	if !strings.Contains(actual, substring) {
		v.t.Errorf("expected header %s to contain %q, got %q", key, substring, actual)
	}
	return v
}

// ContentType asserts the Content-Type header.
func (v *Validator) ContentType(resp *Response, expected string) *Validator {
	return v.HeaderContains(resp, "Content-Type", expected)
}

// JSON asserts the Content-Type is JSON.
func (v *Validator) JSON(resp *Response) *Validator {
	return v.ContentType(resp, "application/json")
}

// BodyContains asserts the body contains a substring.
func (v *Validator) BodyContains(resp *Response, substring string) *Validator {
	v.t.Helper()
	if !strings.Contains(resp.String(), substring) {
		v.t.Errorf("expected body to contain %q, got: %s", substring, resp.String())
	}
	return v
}

// BodyEquals asserts the body equals a string.
func (v *Validator) BodyEquals(resp *Response, expected string) *Validator {
	v.t.Helper()
	if resp.String() != expected {
		v.t.Errorf("expected body %q, got %q", expected, resp.String())
	}
	return v
}

// JSONPath asserts a JSON path value.
func (v *Validator) JSONPath(resp *Response, path string, expected any) *Validator {
	v.t.Helper()

	var data map[string]any
	if err := resp.JSON(&data); err != nil {
		v.t.Errorf("failed to parse JSON: %v", err)
		return v
	}

	value := getJSONPath(data, path)
	if !reflect.DeepEqual(value, expected) {
		v.t.Errorf("path %q: expected %v (%T), got %v (%T)",
			path, expected, expected, value, value)
	}
	return v
}

// JSONPathExists asserts a JSON path exists.
func (v *Validator) JSONPathExists(resp *Response, path string) *Validator {
	v.t.Helper()

	var data map[string]any
	if err := resp.JSON(&data); err != nil {
		v.t.Errorf("failed to parse JSON: %v", err)
		return v
	}

	if getJSONPath(data, path) == nil {
		v.t.Errorf("path %q does not exist", path)
	}
	return v
}

// getJSONPath retrieves a value by dot-notation path.
func getJSONPath(data map[string]any, path string) any {
	parts := strings.Split(path, ".")
	var current any = data

	for _, part := range parts {
		switch c := current.(type) {
		case map[string]any:
			var ok bool
			current, ok = c[part]
			if !ok {
				return nil
			}
		default:
			return nil
		}
	}

	return current
}

// Schema validates JSON against a schema definition.
type Schema struct {
	Required   []string
	Properties map[string]PropertySchema
}

// PropertySchema defines a property's schema.
type PropertySchema struct {
	Type     string // "string", "number", "boolean", "array", "object"
	Required bool
	MinLen   int
	MaxLen   int
	Pattern  string
}

// ValidateSchema validates response against a schema.
func (v *Validator) ValidateSchema(resp *Response, schema Schema) *Validator {
	v.t.Helper()

	var data map[string]any
	if err := resp.JSON(&data); err != nil {
		v.t.Errorf("failed to parse JSON: %v", err)
		return v
	}

	// Check required fields
	for _, field := range schema.Required {
		if _, ok := data[field]; !ok {
			v.t.Errorf("missing required field: %s", field)
		}
	}

	// Validate properties
	for name, prop := range schema.Properties {
		value, ok := data[name]
		if !ok {
			if prop.Required {
				v.t.Errorf("missing required property: %s", name)
			}
			continue
		}

		v.validateProperty(name, value, prop)
	}

	return v
}

// validateProperty validates a single property.
func (v *Validator) validateProperty(name string, value any, schema PropertySchema) {
	v.t.Helper()

	actualType := getType(value)
	if schema.Type != "" && actualType != schema.Type {
		v.t.Errorf("property %s: expected type %s, got %s", name, schema.Type, actualType)
	}

	if schema.Type == "string" {
		str, ok := value.(string)
		if ok {
			if schema.MinLen > 0 && len(str) < schema.MinLen {
				v.t.Errorf("property %s: length %d below minimum %d", name, len(str), schema.MinLen)
			}
			if schema.MaxLen > 0 && len(str) > schema.MaxLen {
				v.t.Errorf("property %s: length %d above maximum %d", name, len(str), schema.MaxLen)
			}
		}
	}
}

// getType returns the JSON type of a value.
func getType(v any) string {
	switch v.(type) {
	case string:
		return "string"
	case float64, int, int64:
		return "number"
	case bool:
		return "boolean"
	case []any:
		return "array"
	case map[string]any:
		return "object"
	case nil:
		return "null"
	default:
		return "unknown"
	}
}

// TestCase represents an API test case.
type TestCase struct {
	Name           string
	Method         string
	Path           string
	Headers        map[string]string
	Query          map[string]string
	Body           any
	ExpectedStatus int
	ExpectedBody   string
	Validate       func(*testing.T, *Response)
}

// RunTestCases executes a slice of test cases.
func (c *Client) RunTestCases(t *testing.T, cases []TestCase) {
	t.Helper()

	for _, tc := range cases {
		t.Run(tc.Name, func(t *testing.T) {
			ctx := context.Background()

			resp, err := c.Do(ctx, Request{
				Method:  tc.Method,
				Path:    tc.Path,
				Headers: tc.Headers,
				Query:   tc.Query,
				Body:    tc.Body,
			})

			if err != nil {
				t.Fatalf("request failed: %v", err)
			}

			if tc.ExpectedStatus != 0 && resp.StatusCode != tc.ExpectedStatus {
				t.Errorf("expected status %d, got %d", tc.ExpectedStatus, resp.StatusCode)
			}

			if tc.ExpectedBody != "" && resp.String() != tc.ExpectedBody {
				t.Errorf("expected body %q, got %q", tc.ExpectedBody, resp.String())
			}

			if tc.Validate != nil {
				tc.Validate(t, resp)
			}
		})
	}
}

// HealthCheck performs a health check on the API.
func (c *Client) HealthCheck(ctx context.Context, path string) error {
	resp, err := c.GET(ctx, path)
	if err != nil {
		return fmt.Errorf("health check failed: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("health check returned status %d", resp.StatusCode)
	}

	return nil
}

// WaitForReady waits for the API to become ready.
func (c *Client) WaitForReady(ctx context.Context, path string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)

	for time.Now().Before(deadline) {
		if err := c.HealthCheck(ctx, path); err == nil {
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}

	return fmt.Errorf("API not ready after %v", timeout)
}
