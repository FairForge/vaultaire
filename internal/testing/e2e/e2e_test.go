package e2e

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestNewState(t *testing.T) {
	state := NewState()
	if state.data == nil {
		t.Error("expected data map to be initialized")
	}
}

func TestState_SetGet(t *testing.T) {
	state := NewState()

	state.Set("key1", "value1")
	state.Set("key2", 42)

	v1, ok := state.Get("key1")
	if !ok || v1 != "value1" {
		t.Error("failed to get key1")
	}

	v2, ok := state.Get("key2")
	if !ok || v2 != 42 {
		t.Error("failed to get key2")
	}

	_, ok = state.Get("missing")
	if ok {
		t.Error("expected missing key to return false")
	}
}

func TestState_GetString(t *testing.T) {
	state := NewState()
	state.Set("str", "hello")
	state.Set("num", 123)

	if state.GetString("str") != "hello" {
		t.Error("GetString failed")
	}
	if state.GetString("num") != "" {
		t.Error("GetString should return empty for non-string")
	}
	if state.GetString("missing") != "" {
		t.Error("GetString should return empty for missing")
	}
}

func TestState_GetInt(t *testing.T) {
	state := NewState()
	state.Set("num", 42)
	state.Set("str", "hello")

	if state.GetInt("num") != 42 {
		t.Error("GetInt failed")
	}
	if state.GetInt("str") != 0 {
		t.Error("GetInt should return 0 for non-int")
	}
}

func TestState_MustGet(t *testing.T) {
	state := NewState()
	state.Set("key", "value")

	v := state.MustGet("key")
	if v != "value" {
		t.Error("MustGet failed")
	}

	defer func() {
		if r := recover(); r == nil {
			t.Error("expected panic for missing key")
		}
	}()
	state.MustGet("missing")
}

func TestNewRunner(t *testing.T) {
	runner := NewRunner("http://localhost:8080/")

	if runner.BaseURL != "http://localhost:8080" {
		t.Errorf("expected trailing slash removed, got %s", runner.BaseURL)
	}
	if runner.HTTPClient == nil {
		t.Error("expected HTTP client")
	}
	if runner.state == nil {
		t.Error("expected state")
	}
}

func TestRunner_Run_Success(t *testing.T) {
	runner := NewRunner("http://test")

	stepExecuted := false
	validated := false

	scenario := Scenario{
		Name:        "test-scenario",
		Description: "A test scenario",
		Steps: []Step{
			{
				Name: "step-1",
				Action: func(ctx context.Context, state *State) error {
					stepExecuted = true
					state.Set("result", "success")
					return nil
				},
				Validate: func(ctx context.Context, state *State) error {
					validated = true
					if state.GetString("result") != "success" {
						return errors.New("validation failed")
					}
					return nil
				},
			},
		},
		Timeout: 10 * time.Second,
	}

	runner.Run(t, scenario)

	if !stepExecuted {
		t.Error("step was not executed")
	}
	if !validated {
		t.Error("validation was not run")
	}
}

func TestRunner_Run_WithSetupTeardown(t *testing.T) {
	runner := NewRunner("http://test")

	setupCalled := false
	teardownCalled := false

	scenario := Scenario{
		Name: "setup-teardown",
		Setup: func(ctx context.Context) error {
			setupCalled = true
			return nil
		},
		Teardown: func(ctx context.Context) error {
			teardownCalled = true
			return nil
		},
		Steps: []Step{
			{
				Name:   "no-op",
				Action: func(ctx context.Context, state *State) error { return nil },
			},
		},
	}

	runner.Run(t, scenario)

	if !setupCalled {
		t.Error("setup not called")
	}
	if !teardownCalled {
		t.Error("teardown not called")
	}
}

func TestRunner_Run_WithRetry(t *testing.T) {
	runner := NewRunner("http://test")

	attempts := 0

	scenario := Scenario{
		Name: "retry-test",
		Steps: []Step{
			{
				Name: "flaky-step",
				Action: func(ctx context.Context, state *State) error {
					attempts++
					if attempts < 3 {
						return errors.New("temporary failure")
					}
					return nil
				},
				RetryCount: 3,
				RetryDelay: 10 * time.Millisecond,
			},
		},
	}

	runner.Run(t, scenario)

	if attempts != 3 {
		t.Errorf("expected 3 attempts, got %d", attempts)
	}
}

func TestRunner_HTTP(t *testing.T) {
	// Create test server
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/health":
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"status":"ok"}`))
		case "/users":
			if r.Method == http.MethodPost {
				w.WriteHeader(http.StatusCreated)
				_, _ = w.Write([]byte(`{"id":123,"name":"test"}`))
			}
		case "/items/1":
			if r.Method == http.MethodDelete {
				w.WriteHeader(http.StatusNoContent)
			}
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	runner := NewRunner(server.URL)
	ctx := context.Background()

	// Test GET
	resp, err := runner.GET(ctx, "/health")
	if err != nil {
		t.Fatalf("GET failed: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected 200, got %d", resp.StatusCode)
	}

	// Test POST
	resp, err = runner.POST(ctx, "/users", map[string]string{"name": "test"})
	if err != nil {
		t.Fatalf("POST failed: %v", err)
	}
	if resp.StatusCode != http.StatusCreated {
		t.Errorf("expected 201, got %d", resp.StatusCode)
	}

	// Test DELETE
	resp, err = runner.DELETE(ctx, "/items/1")
	if err != nil {
		t.Fatalf("DELETE failed: %v", err)
	}
	if resp.StatusCode != http.StatusNoContent {
		t.Errorf("expected 204, got %d", resp.StatusCode)
	}
}

func TestRunner_Do_WithQuery(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("foo") == "bar" {
			w.WriteHeader(http.StatusOK)
		} else {
			w.WriteHeader(http.StatusBadRequest)
		}
	}))
	defer server.Close()

	runner := NewRunner(server.URL)
	ctx := context.Background()

	resp, err := runner.Do(ctx, Request{
		Method: http.MethodGet,
		Path:   "/test",
		Query:  map[string]string{"foo": "bar"},
	})
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
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

func TestAssertions_StatusCode(t *testing.T) {
	mockT := &testing.T{}
	a := NewAssertions(mockT)

	resp := &Response{StatusCode: 200}
	a.StatusCode(resp, 200)
	a.StatusOK(resp)

	resp201 := &Response{StatusCode: 201}
	a.StatusCreated(resp201)

	resp204 := &Response{StatusCode: 204}
	a.StatusNoContent(resp204)
}

func TestAssertions_BodyContains(t *testing.T) {
	mockT := &testing.T{}
	a := NewAssertions(mockT)

	resp := &Response{Body: []byte("hello world")}
	a.BodyContains(resp, "world")
}

func TestAssertions_HeaderEquals(t *testing.T) {
	mockT := &testing.T{}
	a := NewAssertions(mockT)

	resp := &Response{
		Headers: http.Header{
			"Content-Type": []string{"application/json"},
		},
	}
	a.HeaderEquals(resp, "Content-Type", "application/json")
}

func TestAssertions_JSONPath(t *testing.T) {
	mockT := &testing.T{}
	a := NewAssertions(mockT)

	data := map[string]any{
		"user": map[string]any{
			"name": "test",
			"age":  float64(30),
		},
	}
	body, _ := json.Marshal(data)
	resp := &Response{Body: body}

	a.JSONPath(resp, "user.name", "test")
	a.JSONPath(resp, "user.age", float64(30))
}

func TestRunner_RunJourney(t *testing.T) {
	runner := NewRunner("http://test")

	scenariosRun := 0

	journey := UserJourney{
		Name:  "user-signup",
		Actor: "new-user",
		Scenarios: []Scenario{
			{
				Name: "visit-homepage",
				Steps: []Step{
					{
						Name:   "load-page",
						Action: func(ctx context.Context, state *State) error { return nil },
					},
				},
			},
			{
				Name: "create-account",
				Steps: []Step{
					{
						Name: "fill-form",
						Action: func(ctx context.Context, state *State) error {
							scenariosRun++
							return nil
						},
					},
				},
			},
		},
	}

	runner.RunJourney(t, journey)

	if scenariosRun != 1 {
		t.Errorf("expected 1 scenario action, got %d", scenariosRun)
	}
}

func TestScenario(t *testing.T) {
	scenario := Scenario{
		Name:        "test",
		Description: "A test scenario",
		Timeout:     time.Minute,
		Steps:       []Step{},
	}

	if scenario.Name != "test" {
		t.Error("Name not set")
	}
	if scenario.Timeout != time.Minute {
		t.Error("Timeout not set")
	}
}

func TestStep(t *testing.T) {
	step := Step{
		Name:        "test-step",
		RetryCount:  3,
		RetryDelay:  time.Second,
		SkipOnError: true,
	}

	if step.RetryCount != 3 {
		t.Error("RetryCount not set")
	}
	if !step.SkipOnError {
		t.Error("SkipOnError not set")
	}
}
