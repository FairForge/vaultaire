package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestNewClient(t *testing.T) {
	client := NewClient("http://localhost:8080/")

	if client.BaseURL != "http://localhost:8080" {
		t.Errorf("expected trailing slash removed, got %s", client.BaseURL)
	}
	if client.HTTPClient == nil {
		t.Error("expected HTTP client")
	}
}

func TestClient_SetHeader(t *testing.T) {
	client := NewClient("http://test")
	client.SetHeader("X-Custom", "value")

	if client.Headers["X-Custom"] != "value" {
		t.Error("header not set")
	}
}

func TestClient_SetAuth(t *testing.T) {
	client := NewClient("http://test")
	client.SetAuth("token123")

	if client.Headers["Authorization"] != "Bearer token123" {
		t.Errorf("expected Bearer token, got %s", client.Headers["Authorization"])
	}
}

func TestClient_SetBasicAuth(t *testing.T) {
	client := NewClient("http://test")
	client.SetBasicAuth("user", "pass")

	expected := "Basic " + base64Encode([]byte("user:pass"))
	if client.Headers["Authorization"] != expected {
		t.Errorf("expected %s, got %s", expected, client.Headers["Authorization"])
	}
}

func TestBase64Encode(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"", ""},
		{"f", "Zg=="},
		{"fo", "Zm8="},
		{"foo", "Zm9v"},
		{"user:pass", "dXNlcjpwYXNz"},
	}

	for _, tt := range tests {
		result := base64Encode([]byte(tt.input))
		if result != tt.expected {
			t.Errorf("base64Encode(%q) = %q, expected %q", tt.input, result, tt.expected)
		}
	}
}

func TestClient_HTTP_Methods(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		response := map[string]string{
			"method": r.Method,
			"path":   r.URL.Path,
		}
		_ = json.NewEncoder(w).Encode(response)
	}))
	defer server.Close()

	client := NewClient(server.URL)
	ctx := context.Background()

	// Test GET
	resp, err := client.GET(ctx, "/test")
	if err != nil {
		t.Fatalf("GET failed: %v", err)
	}
	if resp.StatusCode != 200 {
		t.Errorf("expected 200, got %d", resp.StatusCode)
	}

	// Test POST
	resp, err = client.POST(ctx, "/test", map[string]string{"key": "value"})
	if err != nil {
		t.Fatalf("POST failed: %v", err)
	}

	var data map[string]string
	if err := resp.JSON(&data); err != nil {
		t.Fatalf("JSON parse failed: %v", err)
	}
	if data["method"] != "POST" {
		t.Errorf("expected POST, got %s", data["method"])
	}

	// Test PUT
	_, err = client.PUT(ctx, "/test", nil)
	if err != nil {
		t.Fatalf("PUT failed: %v", err)
	}

	// Test PATCH
	_, err = client.PATCH(ctx, "/test", nil)
	if err != nil {
		t.Fatalf("PATCH failed: %v", err)
	}

	// Test DELETE
	_, err = client.DELETE(ctx, "/test")
	if err != nil {
		t.Fatalf("DELETE failed: %v", err)
	}
}

func TestClient_Do_WithQuery(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("foo") != "bar" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	client := NewClient(server.URL)
	ctx := context.Background()

	resp, err := client.Do(ctx, Request{
		Method: http.MethodGet,
		Path:   "/test",
		Query:  map[string]string{"foo": "bar"},
	})

	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	if resp.StatusCode != 200 {
		t.Errorf("expected 200, got %d", resp.StatusCode)
	}
}

func TestResponse_JSON(t *testing.T) {
	resp := &Response{
		Body: []byte(`{"name":"test","count":42}`),
	}

	var data struct {
		Name  string `json:"name"`
		Count int    `json:"count"`
	}

	if err := resp.JSON(&data); err != nil {
		t.Fatalf("JSON decode failed: %v", err)
	}

	if data.Name != "test" || data.Count != 42 {
		t.Error("JSON data mismatch")
	}
}

func TestResponse_String(t *testing.T) {
	resp := &Response{Body: []byte("hello world")}
	if resp.String() != "hello world" {
		t.Error("String() failed")
	}
}

func TestValidator_Status(t *testing.T) {
	mockT := &testing.T{}
	v := NewValidator(mockT)

	resp := &Response{StatusCode: 200}
	v.Status(resp, 200)
	v.StatusOK(resp)

	resp.StatusCode = 201
	v.StatusCreated(resp)

	resp.StatusCode = 204
	v.StatusNoContent(resp)

	resp.StatusCode = 400
	v.StatusBadRequest(resp)

	resp.StatusCode = 401
	v.StatusUnauthorized(resp)

	resp.StatusCode = 403
	v.StatusForbidden(resp)

	resp.StatusCode = 404
	v.StatusNotFound(resp)
}

func TestValidator_Header(t *testing.T) {
	mockT := &testing.T{}
	v := NewValidator(mockT)

	resp := &Response{
		Headers: http.Header{
			"Content-Type": []string{"application/json; charset=utf-8"},
		},
	}

	v.Header(resp, "Content-Type", "application/json; charset=utf-8")
	v.HeaderContains(resp, "Content-Type", "application/json")
	v.ContentType(resp, "application/json")
	v.JSON(resp)
}

func TestValidator_Body(t *testing.T) {
	mockT := &testing.T{}
	v := NewValidator(mockT)

	resp := &Response{Body: []byte("hello world")}

	v.BodyContains(resp, "world")
	v.BodyEquals(resp, "hello world")
}

func TestValidator_JSONPath(t *testing.T) {
	mockT := &testing.T{}
	v := NewValidator(mockT)

	data := map[string]any{
		"user": map[string]any{
			"name": "test",
			"age":  float64(30),
		},
		"active": true,
	}
	body, _ := json.Marshal(data)
	resp := &Response{Body: body}

	v.JSONPath(resp, "user.name", "test")
	v.JSONPath(resp, "user.age", float64(30))
	v.JSONPath(resp, "active", true)
	v.JSONPathExists(resp, "user.name")
}

func TestValidator_ValidateSchema(t *testing.T) {
	mockT := &testing.T{}
	v := NewValidator(mockT)

	data := map[string]any{
		"id":    float64(123),
		"name":  "test",
		"email": "test@example.com",
	}
	body, _ := json.Marshal(data)
	resp := &Response{Body: body}

	schema := Schema{
		Required: []string{"id", "name"},
		Properties: map[string]PropertySchema{
			"id":   {Type: "number", Required: true},
			"name": {Type: "string", Required: true, MinLen: 1, MaxLen: 100},
		},
	}

	v.ValidateSchema(resp, schema)
}

func TestGetType(t *testing.T) {
	tests := []struct {
		value    any
		expected string
	}{
		{"string", "string"},
		{float64(42), "number"},
		{true, "boolean"},
		{[]any{1, 2, 3}, "array"},
		{map[string]any{}, "object"},
		{nil, "null"},
	}

	for _, tt := range tests {
		result := getType(tt.value)
		if result != tt.expected {
			t.Errorf("getType(%v) = %s, expected %s", tt.value, result, tt.expected)
		}
	}
}

func TestClient_RunTestCases(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ok":
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("OK"))
		case "/json":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"status":"success"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	client := NewClient(server.URL)

	cases := []TestCase{
		{
			Name:           "GET /ok",
			Method:         http.MethodGet,
			Path:           "/ok",
			ExpectedStatus: 200,
			ExpectedBody:   "OK",
		},
		{
			Name:           "GET /json",
			Method:         http.MethodGet,
			Path:           "/json",
			ExpectedStatus: 200,
			Validate: func(t *testing.T, resp *Response) {
				NewValidator(t).JSON(resp).JSONPath(resp, "status", "success")
			},
		},
		{
			Name:           "GET /notfound",
			Method:         http.MethodGet,
			Path:           "/notfound",
			ExpectedStatus: 404,
		},
	}

	client.RunTestCases(t, cases)
}

func TestClient_HealthCheck(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			w.WriteHeader(http.StatusOK)
		} else {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
	}))
	defer server.Close()

	client := NewClient(server.URL)
	ctx := context.Background()

	// Should succeed
	if err := client.HealthCheck(ctx, "/health"); err != nil {
		t.Errorf("health check failed: %v", err)
	}

	// Should fail
	if err := client.HealthCheck(ctx, "/unhealthy"); err == nil {
		t.Error("expected health check to fail")
	}
}

func TestClient_WaitForReady(t *testing.T) {
	ready := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if ready {
			w.WriteHeader(http.StatusOK)
		} else {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
	}))
	defer server.Close()

	client := NewClient(server.URL)
	ctx := context.Background()

	// Make ready after short delay
	go func() {
		time.Sleep(200 * time.Millisecond)
		ready = true
	}()

	err := client.WaitForReady(ctx, "/health", time.Second)
	if err != nil {
		t.Errorf("WaitForReady failed: %v", err)
	}
}

func TestClient_WaitForReady_Timeout(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server.Close()

	client := NewClient(server.URL)
	ctx := context.Background()

	err := client.WaitForReady(ctx, "/health", 200*time.Millisecond)
	if err == nil {
		t.Error("expected timeout error")
	}
}

func TestTestCase(t *testing.T) {
	tc := TestCase{
		Name:           "test",
		Method:         http.MethodPost,
		Path:           "/api",
		Headers:        map[string]string{"X-Test": "value"},
		Query:          map[string]string{"q": "search"},
		Body:           map[string]string{"key": "value"},
		ExpectedStatus: 201,
	}

	if tc.Name != "test" {
		t.Error("Name not set")
	}
	if tc.Method != http.MethodPost {
		t.Error("Method not set")
	}
}

func TestSchema(t *testing.T) {
	schema := Schema{
		Required: []string{"id", "name"},
		Properties: map[string]PropertySchema{
			"id": {Type: "number", Required: true},
		},
	}

	if len(schema.Required) != 2 {
		t.Error("Required not set")
	}
	if schema.Properties["id"].Type != "number" {
		t.Error("Properties not set")
	}
}

func TestPropertySchema(t *testing.T) {
	prop := PropertySchema{
		Type:     "string",
		Required: true,
		MinLen:   1,
		MaxLen:   100,
		Pattern:  "^[a-z]+$",
	}

	if prop.Type != "string" {
		t.Error("Type not set")
	}
	if prop.MinLen != 1 {
		t.Error("MinLen not set")
	}
}
