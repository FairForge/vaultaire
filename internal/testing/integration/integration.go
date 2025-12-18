// Package integration provides utilities for integration testing.
package integration

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"sync"
	"testing"
	"time"
)

// Suite manages integration test setup and teardown.
type Suite struct {
	Name       string
	Components []Component
	Config     Config
	ctx        context.Context
	cancel     context.CancelFunc
	mu         sync.Mutex
	started    bool
	results    []ComponentStatus
}

// Config configures the integration test suite.
type Config struct {
	Timeout        time.Duration
	SetupTimeout   time.Duration
	CleanupTimeout time.Duration
	Parallel       bool
	FailFast       bool
	RetryCount     int
	RetryDelay     time.Duration
}

// DefaultConfig returns sensible defaults for integration testing.
func DefaultConfig() Config {
	return Config{
		Timeout:        5 * time.Minute,
		SetupTimeout:   30 * time.Second,
		CleanupTimeout: 30 * time.Second,
		Parallel:       false,
		FailFast:       true,
		RetryCount:     3,
		RetryDelay:     time.Second,
	}
}

// Component represents a testable system component.
type Component interface {
	Name() string
	Setup(ctx context.Context) error
	Teardown(ctx context.Context) error
	HealthCheck(ctx context.Context) error
}

// ComponentStatus tracks a component's state.
type ComponentStatus struct {
	Name      string
	Status    Status
	StartTime time.Time
	Duration  time.Duration
	Error     error
}

// Status represents component state.
type Status string

const (
	StatusPending  Status = "pending"
	StatusStarting Status = "starting"
	StatusReady    Status = "ready"
	StatusFailed   Status = "failed"
	StatusStopped  Status = "stopped"
)

// NewSuite creates a new integration test suite.
func NewSuite(name string, config Config) *Suite {
	ctx, cancel := context.WithTimeout(context.Background(), config.Timeout)
	return &Suite{
		Name:       name,
		Config:     config,
		Components: make([]Component, 0),
		ctx:        ctx,
		cancel:     cancel,
		results:    make([]ComponentStatus, 0),
	}
}

// AddComponent adds a component to the suite.
func (s *Suite) AddComponent(c Component) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Components = append(s.Components, c)
	s.results = append(s.results, ComponentStatus{
		Name:   c.Name(),
		Status: StatusPending,
	})
}

// Setup starts all components in order.
func (s *Suite) Setup(t *testing.T) error {
	s.mu.Lock()
	if s.started {
		s.mu.Unlock()
		return fmt.Errorf("suite already started")
	}
	s.started = true
	s.mu.Unlock()

	t.Logf("Starting integration suite: %s", s.Name)

	for i, component := range s.Components {
		s.results[i].Status = StatusStarting
		s.results[i].StartTime = time.Now()

		ctx, cancel := context.WithTimeout(s.ctx, s.Config.SetupTimeout)

		err := s.setupWithRetry(ctx, component)
		cancel()

		s.results[i].Duration = time.Since(s.results[i].StartTime)

		if err != nil {
			s.results[i].Status = StatusFailed
			s.results[i].Error = err
			t.Logf("Failed to start component %s: %v", component.Name(), err)

			if s.Config.FailFast {
				return fmt.Errorf("component %s setup failed: %w", component.Name(), err)
			}
			continue
		}

		s.results[i].Status = StatusReady
		t.Logf("Component %s ready (took %v)", component.Name(), s.results[i].Duration)
	}

	return nil
}

// setupWithRetry attempts to setup a component with retries.
func (s *Suite) setupWithRetry(ctx context.Context, c Component) error {
	var lastErr error

	for i := 0; i <= s.Config.RetryCount; i++ {
		if i > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(s.Config.RetryDelay):
			}
		}

		if err := c.Setup(ctx); err != nil {
			lastErr = err
			continue
		}

		// Verify health
		if err := c.HealthCheck(ctx); err != nil {
			lastErr = err
			continue
		}

		return nil
	}

	return fmt.Errorf("after %d retries: %w", s.Config.RetryCount, lastErr)
}

// Teardown stops all components in reverse order.
func (s *Suite) Teardown(t *testing.T) {
	s.mu.Lock()
	defer s.mu.Unlock()

	t.Logf("Stopping integration suite: %s", s.Name)

	// Stop in reverse order
	for i := len(s.Components) - 1; i >= 0; i-- {
		component := s.Components[i]

		ctx, cancel := context.WithTimeout(context.Background(), s.Config.CleanupTimeout)
		err := component.Teardown(ctx)
		cancel()

		if err != nil {
			t.Logf("Warning: failed to stop %s: %v", component.Name(), err)
		} else {
			t.Logf("Component %s stopped", component.Name())
		}

		s.results[i].Status = StatusStopped
	}

	s.cancel()
}

// Context returns the suite's context.
func (s *Suite) Context() context.Context {
	return s.ctx
}

// Results returns component statuses.
func (s *Suite) Results() []ComponentStatus {
	s.mu.Lock()
	defer s.mu.Unlock()

	results := make([]ComponentStatus, len(s.results))
	copy(results, s.results)
	return results
}

// AllReady returns true if all components are ready.
func (s *Suite) AllReady() bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	for _, r := range s.results {
		if r.Status != StatusReady {
			return false
		}
	}
	return len(s.results) > 0
}

// HTTPComponent represents an HTTP service component.
type HTTPComponent struct {
	name       string
	addr       string
	handler    http.Handler
	server     *http.Server
	healthPath string
}

// NewHTTPComponent creates an HTTP service component.
func NewHTTPComponent(name, addr string, handler http.Handler) *HTTPComponent {
	return &HTTPComponent{
		name:       name,
		addr:       addr,
		handler:    handler,
		healthPath: "/health",
	}
}

// Name returns the component name.
func (h *HTTPComponent) Name() string {
	return h.name
}

// Setup starts the HTTP server.
func (h *HTTPComponent) Setup(ctx context.Context) error {
	h.server = &http.Server{
		Addr:    h.addr,
		Handler: h.handler,
	}

	listener, err := net.Listen("tcp", h.addr)
	if err != nil {
		return fmt.Errorf("failed to listen on %s: %w", h.addr, err)
	}

	go func() {
		_ = h.server.Serve(listener) // Ignore error - server stops on Shutdown
	}()

	return nil
}

// Teardown stops the HTTP server.
func (h *HTTPComponent) Teardown(ctx context.Context) error {
	if h.server == nil {
		return nil
	}
	return h.server.Shutdown(ctx)
}

// HealthCheck verifies the HTTP server is responding.
func (h *HTTPComponent) HealthCheck(ctx context.Context) error {
	url := fmt.Sprintf("http://%s%s", h.addr, h.healthPath)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}

	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("health check failed: %w", err)
	}
	defer func() {
		_ = resp.Body.Close()
	}()

	if resp.StatusCode >= 400 {
		return fmt.Errorf("health check returned status %d", resp.StatusCode)
	}

	return nil
}

// SetHealthPath sets the health check endpoint.
func (h *HTTPComponent) SetHealthPath(path string) {
	h.healthPath = path
}

// Address returns the server address.
func (h *HTTPComponent) Address() string {
	return h.addr
}

// MockComponent is a simple component for testing.
type MockComponent struct {
	name         string
	setupFunc    func(context.Context) error
	teardownFunc func(context.Context) error
	healthFunc   func(context.Context) error
}

// NewMockComponent creates a mock component.
func NewMockComponent(name string) *MockComponent {
	return &MockComponent{
		name:         name,
		setupFunc:    func(context.Context) error { return nil },
		teardownFunc: func(context.Context) error { return nil },
		healthFunc:   func(context.Context) error { return nil },
	}
}

// Name returns the component name.
func (m *MockComponent) Name() string {
	return m.name
}

// Setup runs the setup function.
func (m *MockComponent) Setup(ctx context.Context) error {
	return m.setupFunc(ctx)
}

// Teardown runs the teardown function.
func (m *MockComponent) Teardown(ctx context.Context) error {
	return m.teardownFunc(ctx)
}

// HealthCheck runs the health check function.
func (m *MockComponent) HealthCheck(ctx context.Context) error {
	return m.healthFunc(ctx)
}

// OnSetup sets the setup function.
func (m *MockComponent) OnSetup(f func(context.Context) error) {
	m.setupFunc = f
}

// OnTeardown sets the teardown function.
func (m *MockComponent) OnTeardown(f func(context.Context) error) {
	m.teardownFunc = f
}

// OnHealthCheck sets the health check function.
func (m *MockComponent) OnHealthCheck(f func(context.Context) error) {
	m.healthFunc = f
}

// RequireEnv skips the test if required environment variables are not set.
func RequireEnv(t *testing.T, vars ...string) {
	t.Helper()
	for _, v := range vars {
		if os.Getenv(v) == "" {
			t.Skipf("Skipping: required environment variable %s not set", v)
		}
	}
}

// RequireDocker skips the test if Docker is not available.
func RequireDocker(t *testing.T) {
	t.Helper()
	// Simple check - try to connect to Docker socket
	if _, err := os.Stat("/var/run/docker.sock"); os.IsNotExist(err) {
		t.Skip("Skipping: Docker not available")
	}
}

// RequireDatabase skips the test if no database connection is available.
func RequireDatabase(t *testing.T) {
	t.Helper()
	RequireEnv(t, "DATABASE_URL")
}

// WaitForReady polls until a component is ready or timeout.
func WaitForReady(ctx context.Context, c Component, interval time.Duration) error {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if err := c.HealthCheck(ctx); err == nil {
				return nil
			}
		}
	}
}

// FreePort returns an available TCP port.
func FreePort() (int, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer func() {
		_ = listener.Close()
	}()

	addr := listener.Addr().(*net.TCPAddr)
	return addr.Port, nil
}
