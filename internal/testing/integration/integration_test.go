package integration

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"
)

func TestDefaultConfig(t *testing.T) {
	config := DefaultConfig()

	if config.Timeout != 5*time.Minute {
		t.Errorf("expected timeout 5m, got %v", config.Timeout)
	}
	if config.SetupTimeout != 30*time.Second {
		t.Errorf("expected setup timeout 30s, got %v", config.SetupTimeout)
	}
	if config.RetryCount != 3 {
		t.Errorf("expected retry count 3, got %d", config.RetryCount)
	}
}

func TestNewSuite(t *testing.T) {
	suite := NewSuite("test-suite", DefaultConfig())

	if suite.Name != "test-suite" {
		t.Errorf("expected name 'test-suite', got %q", suite.Name)
	}
	if len(suite.Components) != 0 {
		t.Error("expected empty components")
	}
	if suite.ctx == nil {
		t.Error("expected context to be set")
	}
}

func TestSuite_AddComponent(t *testing.T) {
	suite := NewSuite("test", DefaultConfig())

	mock := NewMockComponent("mock-1")
	suite.AddComponent(mock)

	if len(suite.Components) != 1 {
		t.Errorf("expected 1 component, got %d", len(suite.Components))
	}
	if suite.results[0].Status != StatusPending {
		t.Errorf("expected pending status, got %s", suite.results[0].Status)
	}
}

func TestSuite_Setup_Success(t *testing.T) {
	config := DefaultConfig()
	config.Timeout = 10 * time.Second
	suite := NewSuite("test", config)

	setupCalled := false
	mock := NewMockComponent("mock")
	mock.OnSetup(func(ctx context.Context) error {
		setupCalled = true
		return nil
	})

	suite.AddComponent(mock)

	err := suite.Setup(t)
	if err != nil {
		t.Fatalf("setup failed: %v", err)
	}

	if !setupCalled {
		t.Error("setup was not called")
	}
	if !suite.AllReady() {
		t.Error("expected all components ready")
	}

	suite.Teardown(t)
}

func TestSuite_Setup_Failure(t *testing.T) {
	config := DefaultConfig()
	config.Timeout = 5 * time.Second
	config.RetryCount = 1
	config.RetryDelay = 10 * time.Millisecond
	suite := NewSuite("test", config)

	mock := NewMockComponent("failing")
	mock.OnSetup(func(ctx context.Context) error {
		return errors.New("setup error")
	})

	suite.AddComponent(mock)

	err := suite.Setup(t)
	if err == nil {
		t.Error("expected setup to fail")
	}

	if suite.AllReady() {
		t.Error("expected components not ready")
	}

	results := suite.Results()
	if results[0].Status != StatusFailed {
		t.Errorf("expected failed status, got %s", results[0].Status)
	}

	suite.Teardown(t)
}

func TestSuite_Setup_HealthCheckFailure(t *testing.T) {
	config := DefaultConfig()
	config.Timeout = 5 * time.Second
	config.RetryCount = 1
	config.RetryDelay = 10 * time.Millisecond
	suite := NewSuite("test", config)

	mock := NewMockComponent("unhealthy")
	mock.OnHealthCheck(func(ctx context.Context) error {
		return errors.New("health check failed")
	})

	suite.AddComponent(mock)

	err := suite.Setup(t)
	if err == nil {
		t.Error("expected setup to fail due to health check")
	}

	suite.Teardown(t)
}

func TestSuite_Teardown(t *testing.T) {
	suite := NewSuite("test", DefaultConfig())

	teardownCalled := false
	mock := NewMockComponent("mock")
	mock.OnTeardown(func(ctx context.Context) error {
		teardownCalled = true
		return nil
	})

	suite.AddComponent(mock)
	_ = suite.Setup(t)

	suite.Teardown(t)

	if !teardownCalled {
		t.Error("teardown was not called")
	}

	results := suite.Results()
	if results[0].Status != StatusStopped {
		t.Errorf("expected stopped status, got %s", results[0].Status)
	}
}

func TestSuite_MultipleComponents(t *testing.T) {
	config := DefaultConfig()
	config.Timeout = 10 * time.Second
	suite := NewSuite("multi", config)

	order := make([]string, 0)

	for i := 1; i <= 3; i++ {
		name := fmt.Sprintf("component-%d", i)
		mock := NewMockComponent(name)
		n := name // capture
		mock.OnSetup(func(ctx context.Context) error {
			order = append(order, "setup-"+n)
			return nil
		})
		mock.OnTeardown(func(ctx context.Context) error {
			order = append(order, "teardown-"+n)
			return nil
		})
		suite.AddComponent(mock)
	}

	if err := suite.Setup(t); err != nil {
		t.Fatalf("setup failed: %v", err)
	}

	suite.Teardown(t)

	// Verify setup order (1, 2, 3) and teardown reverse order (3, 2, 1)
	expected := []string{
		"setup-component-1", "setup-component-2", "setup-component-3",
		"teardown-component-3", "teardown-component-2", "teardown-component-1",
	}

	if len(order) != len(expected) {
		t.Fatalf("expected %d operations, got %d: %v", len(expected), len(order), order)
	}

	for i, op := range expected {
		if order[i] != op {
			t.Errorf("position %d: expected %s, got %s", i, op, order[i])
		}
	}

	t.Logf("Operation order: %v", order)
}

func TestSuite_Context(t *testing.T) {
	suite := NewSuite("test", DefaultConfig())

	ctx := suite.Context()
	if ctx == nil {
		t.Error("expected non-nil context")
	}

	// Context should have deadline
	deadline, ok := ctx.Deadline()
	if !ok {
		t.Error("expected context to have deadline")
	}
	if deadline.Before(time.Now()) {
		t.Error("deadline should be in the future")
	}
}

func TestHTTPComponent(t *testing.T) {
	port, err := FreePort()
	if err != nil {
		t.Fatalf("failed to get free port: %v", err)
	}

	addr := fmt.Sprintf("127.0.0.1:%d", port)

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	component := NewHTTPComponent("test-http", addr, handler)
	component.SetHealthPath("/")

	ctx := context.Background()

	if err := component.Setup(ctx); err != nil {
		t.Fatalf("setup failed: %v", err)
	}

	// Give server time to start
	time.Sleep(50 * time.Millisecond)

	if err := component.HealthCheck(ctx); err != nil {
		t.Errorf("health check failed: %v", err)
	}

	if err := component.Teardown(ctx); err != nil {
		t.Errorf("teardown failed: %v", err)
	}

	t.Logf("HTTP component %s at %s", component.Name(), component.Address())
}

func TestMockComponent(t *testing.T) {
	mock := NewMockComponent("test-mock")

	if mock.Name() != "test-mock" {
		t.Errorf("expected name 'test-mock', got %q", mock.Name())
	}

	// Default functions should not error
	ctx := context.Background()
	if err := mock.Setup(ctx); err != nil {
		t.Errorf("default setup should not error: %v", err)
	}
	if err := mock.HealthCheck(ctx); err != nil {
		t.Errorf("default health check should not error: %v", err)
	}
	if err := mock.Teardown(ctx); err != nil {
		t.Errorf("default teardown should not error: %v", err)
	}
}

func TestRequireEnv(t *testing.T) {
	// Set a test variable
	testVar := "TEST_INTEGRATION_VAR_12345"
	t.Setenv(testVar, "value")

	// This should not skip
	RequireEnv(t, testVar)

	// Test with missing var would skip, so we just verify the function exists
}

func TestFreePort(t *testing.T) {
	port, err := FreePort()
	if err != nil {
		t.Fatalf("failed to get free port: %v", err)
	}

	if port <= 0 || port > 65535 {
		t.Errorf("invalid port: %d", port)
	}

	t.Logf("Got free port: %d", port)
}

func TestWaitForReady(t *testing.T) {
	mock := NewMockComponent("wait-test")

	attempts := 0
	mock.OnHealthCheck(func(ctx context.Context) error {
		attempts++
		if attempts < 3 {
			return errors.New("not ready")
		}
		return nil
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err := WaitForReady(ctx, mock, 50*time.Millisecond)
	if err != nil {
		t.Errorf("WaitForReady failed: %v", err)
	}

	if attempts < 3 {
		t.Errorf("expected at least 3 attempts, got %d", attempts)
	}

	t.Logf("Component ready after %d attempts", attempts)
}

func TestWaitForReady_Timeout(t *testing.T) {
	mock := NewMockComponent("timeout-test")
	mock.OnHealthCheck(func(ctx context.Context) error {
		return errors.New("never ready")
	})

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	err := WaitForReady(ctx, mock, 50*time.Millisecond)
	if err == nil {
		t.Error("expected timeout error")
	}
}

func TestComponentStatus(t *testing.T) {
	status := ComponentStatus{
		Name:      "test",
		Status:    StatusReady,
		StartTime: time.Now(),
		Duration:  100 * time.Millisecond,
		Error:     nil,
	}

	if status.Status != StatusReady {
		t.Error("status not set correctly")
	}
	if status.Duration != 100*time.Millisecond {
		t.Error("duration not set correctly")
	}
}

func TestStatus_Constants(t *testing.T) {
	statuses := []Status{
		StatusPending,
		StatusStarting,
		StatusReady,
		StatusFailed,
		StatusStopped,
	}

	for _, s := range statuses {
		if s == "" {
			t.Error("status constant should not be empty")
		}
	}
}
