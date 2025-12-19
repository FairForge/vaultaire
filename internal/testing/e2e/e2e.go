// Package e2e provides end-to-end testing utilities.
package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// Scenario represents an end-to-end test scenario.
type Scenario struct {
	Name        string
	Description string
	Steps       []Step
	Setup       func(ctx context.Context) error
	Teardown    func(ctx context.Context) error
	Timeout     time.Duration
}

// Step represents a single step in a scenario.
type Step struct {
	Name        string
	Action      func(ctx context.Context, state *State) error
	Validate    func(ctx context.Context, state *State) error
	OnError     func(ctx context.Context, state *State, err error)
	RetryCount  int
	RetryDelay  time.Duration
	SkipOnError bool
}

// State holds data between scenario steps.
type State struct {
	data map[string]any
}

// NewState creates a new state container.
func NewState() *State {
	return &State{data: make(map[string]any)}
}

// Set stores a value.
func (s *State) Set(key string, value any) {
	s.data[key] = value
}

// Get retrieves a value.
func (s *State) Get(key string) (any, bool) {
	v, ok := s.data[key]
	return v, ok
}

// GetString retrieves a string value.
func (s *State) GetString(key string) string {
	v, ok := s.data[key]
	if !ok {
		return ""
	}
	str, _ := v.(string)
	return str
}

// GetInt retrieves an int value.
func (s *State) GetInt(key string) int {
	v, ok := s.data[key]
	if !ok {
		return 0
	}
	i, _ := v.(int)
	return i
}

// MustGet retrieves a value or panics.
func (s *State) MustGet(key string) any {
	v, ok := s.data[key]
	if !ok {
		panic(fmt.Sprintf("state key %q not found", key))
	}
	return v
}

// Runner executes E2E scenarios.
type Runner struct {
	BaseURL    string
	HTTPClient *http.Client
	Verbose    bool
	state      *State
}

// NewRunner creates an E2E test runner.
func NewRunner(baseURL string) *Runner {
	return &Runner{
		BaseURL: strings.TrimSuffix(baseURL, "/"),
		HTTPClient: &http.Client{
			Timeout: 30 * time.Second,
		},
		state: NewState(),
	}
}

// Run executes a scenario.
func (r *Runner) Run(t *testing.T, scenario Scenario) {
	t.Helper()

	timeout := scenario.Timeout
	if timeout == 0 {
		timeout = 5 * time.Minute
	}

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	t.Logf("Running scenario: %s", scenario.Name)
	if scenario.Description != "" {
		t.Logf("Description: %s", scenario.Description)
	}

	// Setup
	if scenario.Setup != nil {
		if err := scenario.Setup(ctx); err != nil {
			t.Fatalf("Scenario setup failed: %v", err)
		}
	}

	// Teardown
	defer func() {
		if scenario.Teardown != nil {
			if err := scenario.Teardown(ctx); err != nil {
				t.Logf("Warning: scenario teardown failed: %v", err)
			}
		}
	}()

	// Execute steps
	for i, step := range scenario.Steps {
		stepNum := i + 1
		t.Logf("Step %d/%d: %s", stepNum, len(scenario.Steps), step.Name)

		err := r.executeStep(ctx, step)
		if err != nil {
			if step.OnError != nil {
				step.OnError(ctx, r.state, err)
			}

			if step.SkipOnError {
				t.Logf("Step %d skipped due to error: %v", stepNum, err)
				continue
			}

			t.Fatalf("Step %d failed: %v", stepNum, err)
		}
	}

	t.Logf("Scenario %q completed successfully", scenario.Name)
}

// executeStep runs a single step with retries.
func (r *Runner) executeStep(ctx context.Context, step Step) error {
	var lastErr error
	attempts := step.RetryCount + 1

	for i := 0; i < attempts; i++ {
		if i > 0 {
			if step.RetryDelay > 0 {
				time.Sleep(step.RetryDelay)
			}
		}

		// Execute action
		if step.Action != nil {
			if err := step.Action(ctx, r.state); err != nil {
				lastErr = fmt.Errorf("action failed: %w", err)
				continue
			}
		}

		// Validate
		if step.Validate != nil {
			if err := step.Validate(ctx, r.state); err != nil {
				lastErr = fmt.Errorf("validation failed: %w", err)
				continue
			}
		}

		return nil
	}

	return lastErr
}

// State returns the runner's state.
func (r *Runner) State() *State {
	return r.state
}

// Request represents an HTTP request for E2E testing.
type Request struct {
	Method  string
	Path    string
	Headers map[string]string
	Body    any
	Query   map[string]string
}

// Response represents an HTTP response.
type Response struct {
	StatusCode int
	Headers    http.Header
	Body       []byte
}

// JSON decodes the response body as JSON.
func (r *Response) JSON(v any) error {
	return json.Unmarshal(r.Body, v)
}

// String returns the response body as string.
func (r *Response) String() string {
	return string(r.Body)
}

// Do executes an HTTP request.
func (r *Runner) Do(ctx context.Context, req Request) (*Response, error) {
	url := r.BaseURL + req.Path

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

	// Set headers
	for k, v := range req.Headers {
		httpReq.Header.Set(k, v)
	}
	if req.Body != nil && httpReq.Header.Get("Content-Type") == "" {
		httpReq.Header.Set("Content-Type", "application/json")
	}

	// Execute
	httpResp, err := r.HTTPClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("executing request: %w", err)
	}
	defer func() {
		_ = httpResp.Body.Close()
	}()

	// Read body
	body, err := io.ReadAll(httpResp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading response body: %w", err)
	}

	return &Response{
		StatusCode: httpResp.StatusCode,
		Headers:    httpResp.Header,
		Body:       body,
	}, nil
}

// GET performs a GET request.
func (r *Runner) GET(ctx context.Context, path string) (*Response, error) {
	return r.Do(ctx, Request{Method: http.MethodGet, Path: path})
}

// POST performs a POST request.
func (r *Runner) POST(ctx context.Context, path string, body any) (*Response, error) {
	return r.Do(ctx, Request{Method: http.MethodPost, Path: path, Body: body})
}

// PUT performs a PUT request.
func (r *Runner) PUT(ctx context.Context, path string, body any) (*Response, error) {
	return r.Do(ctx, Request{Method: http.MethodPut, Path: path, Body: body})
}

// DELETE performs a DELETE request.
func (r *Runner) DELETE(ctx context.Context, path string) (*Response, error) {
	return r.Do(ctx, Request{Method: http.MethodDelete, Path: path})
}

// Assertions provides common E2E assertions.
type Assertions struct {
	t *testing.T
}

// NewAssertions creates an assertions helper.
func NewAssertions(t *testing.T) *Assertions {
	return &Assertions{t: t}
}

// StatusCode asserts the response status code.
func (a *Assertions) StatusCode(resp *Response, expected int) {
	a.t.Helper()
	if resp.StatusCode != expected {
		a.t.Errorf("expected status %d, got %d. Body: %s", expected, resp.StatusCode, resp.String())
	}
}

// StatusOK asserts status 200.
func (a *Assertions) StatusOK(resp *Response) {
	a.StatusCode(resp, http.StatusOK)
}

// StatusCreated asserts status 201.
func (a *Assertions) StatusCreated(resp *Response) {
	a.StatusCode(resp, http.StatusCreated)
}

// StatusNoContent asserts status 204.
func (a *Assertions) StatusNoContent(resp *Response) {
	a.StatusCode(resp, http.StatusNoContent)
}

// BodyContains asserts the body contains a substring.
func (a *Assertions) BodyContains(resp *Response, substr string) {
	a.t.Helper()
	if !strings.Contains(resp.String(), substr) {
		a.t.Errorf("expected body to contain %q, got: %s", substr, resp.String())
	}
}

// HeaderEquals asserts a header value.
func (a *Assertions) HeaderEquals(resp *Response, header, expected string) {
	a.t.Helper()
	actual := resp.Headers.Get(header)
	if actual != expected {
		a.t.Errorf("expected header %s=%q, got %q", header, expected, actual)
	}
}

// JSONPath asserts a JSON path value (simple dot notation).
func (a *Assertions) JSONPath(resp *Response, path string, expected any) {
	a.t.Helper()

	var data map[string]any
	if err := resp.JSON(&data); err != nil {
		a.t.Errorf("failed to parse JSON: %v", err)
		return
	}

	parts := strings.Split(path, ".")
	var current any = data

	for _, part := range parts {
		m, ok := current.(map[string]any)
		if !ok {
			a.t.Errorf("path %q: expected object at %s", path, part)
			return
		}
		current, ok = m[part]
		if !ok {
			a.t.Errorf("path %q: key %s not found", path, part)
			return
		}
	}

	if fmt.Sprintf("%v", current) != fmt.Sprintf("%v", expected) {
		a.t.Errorf("path %q: expected %v, got %v", path, expected, current)
	}
}

// UserJourney represents a complete user workflow.
type UserJourney struct {
	Name      string
	Actor     string
	Scenarios []Scenario
}

// RunJourney executes a complete user journey.
func (r *Runner) RunJourney(t *testing.T, journey UserJourney) {
	t.Helper()

	t.Logf("Starting user journey: %s (Actor: %s)", journey.Name, journey.Actor)

	for i, scenario := range journey.Scenarios {
		t.Logf("Journey scenario %d/%d: %s", i+1, len(journey.Scenarios), scenario.Name)
		r.Run(t, scenario)
	}

	t.Logf("User journey %q completed", journey.Name)
}
