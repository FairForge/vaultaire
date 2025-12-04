// internal/gateway/features.go
package gateway

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

// Circuit breaker states
const (
	CircuitClosed   = "closed"
	CircuitOpen     = "open"
	CircuitHalfOpen = "half-open"
)

// TransformConfig configures request/response transformation
type TransformConfig struct {
	AddHeaders    map[string]string `json:"add_headers"`
	RemoveHeaders []string          `json:"remove_headers"`
	RewritePath   string            `json:"rewrite_path"`
}

// Validate checks configuration
func (c *TransformConfig) Validate() error {
	return nil
}

// RequestTransformer transforms requests
type RequestTransformer struct {
	config *TransformConfig
}

// NewRequestTransformer creates a request transformer
func NewRequestTransformer(config *TransformConfig) *RequestTransformer {
	return &RequestTransformer{config: config}
}

// TransformRequest transforms an HTTP request
func (t *RequestTransformer) TransformRequest(req *http.Request) *http.Request {
	// Add headers
	for key, value := range t.config.AddHeaders {
		if value == "{{uuid}}" {
			value = uuid.New().String()
		}
		req.Header.Set(key, value)
	}

	// Remove headers
	for _, key := range t.config.RemoveHeaders {
		req.Header.Del(key)
	}

	// Rewrite path
	if t.config.RewritePath != "" {
		newPath := strings.Replace(t.config.RewritePath, "{{path}}", req.URL.Path, 1)
		req.URL.Path = newPath
	}

	return req
}

// ResponseTransformer transforms responses
type ResponseTransformer struct {
	config *TransformConfig
}

// NewResponseTransformer creates a response transformer
func NewResponseTransformer(config *TransformConfig) *ResponseTransformer {
	return &ResponseTransformer{config: config}
}

// TransformResponse transforms an HTTP response
func (t *ResponseTransformer) TransformResponse(resp *http.Response) *http.Response {
	// Add headers
	for key, value := range t.config.AddHeaders {
		resp.Header.Set(key, value)
	}

	// Remove headers
	for _, key := range t.config.RemoveHeaders {
		resp.Header.Del(key)
	}

	return resp
}

// CircuitBreakerConfig configures a circuit breaker
type CircuitBreakerConfig struct {
	FailureThreshold int           `json:"failure_threshold"`
	SuccessThreshold int           `json:"success_threshold"`
	Timeout          time.Duration `json:"timeout"`
}

// CircuitBreaker implements the circuit breaker pattern
type CircuitBreaker struct {
	config      *CircuitBreakerConfig
	state       string
	failures    int
	successes   int
	lastFailure time.Time
	mu          sync.RWMutex
}

// NewCircuitBreaker creates a circuit breaker
func NewCircuitBreaker(config *CircuitBreakerConfig) *CircuitBreaker {
	return &CircuitBreaker{
		config: config,
		state:  CircuitClosed,
	}
}

// State returns the current circuit state
func (cb *CircuitBreaker) State() string {
	cb.mu.RLock()
	defer cb.mu.RUnlock()

	if cb.state == CircuitOpen {
		if time.Since(cb.lastFailure) > cb.config.Timeout {
			return CircuitHalfOpen
		}
	}
	return cb.state
}

// Allow checks if a request is allowed
func (cb *CircuitBreaker) Allow() error {
	state := cb.State()

	cb.mu.Lock()
	defer cb.mu.Unlock()

	switch state {
	case CircuitOpen:
		return errors.New("circuit open: request rejected")
	case CircuitHalfOpen:
		cb.state = CircuitHalfOpen
		return nil
	default:
		return nil
	}
}

// RecordSuccess records a successful request
func (cb *CircuitBreaker) RecordSuccess() {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	cb.failures = 0

	if cb.state == CircuitHalfOpen {
		cb.successes++
		if cb.successes >= cb.config.SuccessThreshold {
			cb.state = CircuitClosed
			cb.successes = 0
		}
	}
}

// RecordFailure records a failed request
func (cb *CircuitBreaker) RecordFailure() {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	cb.failures++
	cb.lastFailure = time.Now()
	cb.successes = 0

	if cb.failures >= cb.config.FailureThreshold {
		cb.state = CircuitOpen
	}
}

// Execute runs a function with circuit breaker protection
func (cb *CircuitBreaker) Execute(fn func() (interface{}, error)) (interface{}, error) {
	if err := cb.Allow(); err != nil {
		return nil, err
	}

	result, err := fn()
	if err != nil {
		cb.RecordFailure()
		return nil, err
	}

	cb.RecordSuccess()
	return result, nil
}

// RequestCoalescer deduplicates concurrent identical requests
type RequestCoalescer struct {
	inflight map[string]*inflightRequest
	mu       sync.Mutex
}

type inflightRequest struct {
	result interface{}
	err    error
	done   chan struct{}
}

// NewRequestCoalescer creates a request coalescer
func NewRequestCoalescer() *RequestCoalescer {
	return &RequestCoalescer{
		inflight: make(map[string]*inflightRequest),
	}
}

// Do executes a function, coalescing identical concurrent requests
func (c *RequestCoalescer) Do(key string, fn func() (interface{}, error)) (interface{}, error) {
	c.mu.Lock()
	if req, ok := c.inflight[key]; ok {
		c.mu.Unlock()
		<-req.done
		return req.result, req.err
	}

	req := &inflightRequest{done: make(chan struct{})}
	c.inflight[key] = req
	c.mu.Unlock()

	req.result, req.err = fn()
	close(req.done)

	c.mu.Lock()
	delete(c.inflight, key)
	c.mu.Unlock()

	return req.result, req.err
}

// CompositionConfig configures API composition
type CompositionConfig struct {
	Endpoints []EndpointConfig `json:"endpoints"`
	Parallel  bool             `json:"parallel"`
}

// EndpointConfig configures an endpoint in composition
type EndpointConfig struct {
	Name   string `json:"name"`
	URL    string `json:"url"`
	Method string `json:"method"`
}

// APIComposer composes multiple API calls
type APIComposer struct {
	client *http.Client
}

// NewAPIComposer creates an API composer
func NewAPIComposer() *APIComposer {
	return &APIComposer{
		client: &http.Client{Timeout: 30 * time.Second},
	}
}

// Compose executes multiple API calls and combines results
func (c *APIComposer) Compose(ctx context.Context, config *CompositionConfig) (interface{}, error) {
	results := make(map[string]interface{})
	var mu sync.Mutex

	if config.Parallel {
		var wg sync.WaitGroup
		errChan := make(chan error, len(config.Endpoints))

		for _, ep := range config.Endpoints {
			wg.Add(1)
			go func(ep EndpointConfig) {
				defer wg.Done()
				result, err := c.callEndpoint(ctx, ep)
				if err != nil {
					errChan <- err
					return
				}
				mu.Lock()
				results[ep.Name] = result
				mu.Unlock()
			}(ep)
		}

		wg.Wait()
		close(errChan)

		if err := <-errChan; err != nil {
			return nil, err
		}
	} else {
		for _, ep := range config.Endpoints {
			result, err := c.callEndpoint(ctx, ep)
			if err != nil {
				return nil, err
			}
			results[ep.Name] = result
		}
	}

	return results, nil
}

func (c *APIComposer) callEndpoint(ctx context.Context, ep EndpointConfig) (interface{}, error) {
	req, err := http.NewRequestWithContext(ctx, ep.Method, ep.URL, nil)
	if err != nil {
		return nil, err
	}

	resp, err := c.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	var result interface{}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}

	return result, nil
}

// RateLimitPolicyConfig configures rate limiting
type RateLimitPolicyConfig struct {
	DefaultLimit int            `json:"default_limit"`
	BurstLimit   int            `json:"burst_limit"`
	Window       time.Duration  `json:"window"`
	ByEndpoint   map[string]int `json:"by_endpoint"`
	ByTier       map[string]int `json:"by_tier"`
}

// RateLimitPolicy determines rate limits
type RateLimitPolicy struct {
	config *RateLimitPolicyConfig
}

// NewRateLimitPolicy creates a rate limit policy
func NewRateLimitPolicy(config *RateLimitPolicyConfig) *RateLimitPolicy {
	return &RateLimitPolicy{config: config}
}

// GetLimit returns the rate limit for a path and tier
func (p *RateLimitPolicy) GetLimit(path, tier string) int {
	// Check endpoint-specific limits first
	if limit, ok := p.config.ByEndpoint[path]; ok {
		return limit
	}

	// Check tier-specific limits
	if limit, ok := p.config.ByTier[tier]; ok {
		return limit
	}

	return p.config.DefaultLimit
}

// ValidationRule defines request validation rules
type ValidationRule struct {
	RequiredHeaders     []string `json:"required_headers"`
	AllowedContentTypes []string `json:"allowed_content_types"`
	MaxBodySize         int64    `json:"max_body_size"`
}

// RequestValidator validates requests
type RequestValidator struct {
	rules map[string]*ValidationRule
	mu    sync.RWMutex
}

// NewRequestValidator creates a request validator
func NewRequestValidator() *RequestValidator {
	return &RequestValidator{
		rules: make(map[string]*ValidationRule),
	}
}

// AddRule adds a validation rule for a path pattern
func (v *RequestValidator) AddRule(pattern string, rule *ValidationRule) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.rules[pattern] = rule
}

// Validate validates a request
func (v *RequestValidator) Validate(req *http.Request) []string {
	v.mu.RLock()
	defer v.mu.RUnlock()

	var errors []string

	for pattern, rule := range v.rules {
		if !matchPath(pattern, req.URL.Path) {
			continue
		}

		// Check required headers
		for _, header := range rule.RequiredHeaders {
			if req.Header.Get(header) == "" {
				errors = append(errors, fmt.Sprintf("missing required header: %s", header))
			}
		}

		// Check content type
		if len(rule.AllowedContentTypes) > 0 && req.Method != "GET" {
			contentType := req.Header.Get("Content-Type")
			allowed := false
			for _, ct := range rule.AllowedContentTypes {
				if strings.HasPrefix(contentType, ct) {
					allowed = true
					break
				}
			}
			if !allowed {
				errors = append(errors, fmt.Sprintf("content type not allowed: %s", contentType))
			}
		}

		// Check body size
		if rule.MaxBodySize > 0 && req.ContentLength > rule.MaxBodySize {
			errors = append(errors, fmt.Sprintf("body size exceeds limit: %d > %d", req.ContentLength, rule.MaxBodySize))
		}
	}

	return errors
}

func matchPath(pattern, path string) bool {
	if strings.HasSuffix(pattern, "/*") {
		prefix := strings.TrimSuffix(pattern, "/*")
		return strings.HasPrefix(path, prefix)
	}
	return pattern == path
}

// CachePolicyConfig configures caching
type CachePolicyConfig struct {
	DefaultTTL  time.Duration            `json:"default_ttl"`
	ByStatus    map[int]time.Duration    `json:"by_status"`
	ByPath      map[string]time.Duration `json:"by_path"`
	VaryHeaders []string                 `json:"vary_headers"`
}

// CachePolicy determines cache behavior
type CachePolicy struct {
	config *CachePolicyConfig
}

// NewCachePolicy creates a cache policy
func NewCachePolicy(config *CachePolicyConfig) *CachePolicy {
	return &CachePolicy{config: config}
}

// GetTTL returns the TTL for a path and status code
func (p *CachePolicy) GetTTL(path string, status int) time.Duration {
	// Check path-specific TTL
	for pattern, ttl := range p.config.ByPath {
		if matchPath(pattern, path) {
			return ttl
		}
	}

	// Check status-specific TTL
	if ttl, ok := p.config.ByStatus[status]; ok {
		return ttl
	}

	return p.config.DefaultTTL
}

// GenerateCacheKey generates a cache key for a request
func (p *CachePolicy) GenerateCacheKey(req *http.Request) string {
	h := sha256.New()
	h.Write([]byte(req.Method))
	h.Write([]byte(req.URL.String()))

	for _, header := range p.config.VaryHeaders {
		h.Write([]byte(req.Header.Get(header)))
	}

	return hex.EncodeToString(h.Sum(nil))
}

// IPFilterConfig configures IP filtering
type IPFilterConfig struct {
	Allowlist []string `json:"allowlist"`
	Blocklist []string `json:"blocklist"`
}

// IPFilter filters requests by IP
type IPFilter struct {
	allowlist []*net.IPNet
	blocklist []*net.IPNet
}

// NewIPFilter creates an IP filter
func NewIPFilter(config *IPFilterConfig) *IPFilter {
	f := &IPFilter{}

	for _, cidr := range config.Allowlist {
		_, network, err := net.ParseCIDR(cidr)
		if err == nil {
			f.allowlist = append(f.allowlist, network)
		}
	}

	for _, cidr := range config.Blocklist {
		_, network, err := net.ParseCIDR(cidr)
		if err == nil {
			f.blocklist = append(f.blocklist, network)
		}
	}

	return f
}

// Allow checks if an IP is allowed
func (f *IPFilter) Allow(ipStr string) bool {
	ip := net.ParseIP(ipStr)
	if ip == nil {
		return false
	}

	// Check blocklist first
	for _, network := range f.blocklist {
		if network.Contains(ip) {
			return false
		}
	}

	// If allowlist is empty, allow all (except blocklisted)
	if len(f.allowlist) == 0 {
		return true
	}

	// Check allowlist
	for _, network := range f.allowlist {
		if network.Contains(ip) {
			return true
		}
	}

	return false
}

// RetryPolicyConfig configures retry behavior
type RetryPolicyConfig struct {
	MaxRetries      int           `json:"max_retries"`
	InitialBackoff  time.Duration `json:"initial_backoff"`
	MaxBackoff      time.Duration `json:"max_backoff"`
	BackoffFactor   float64       `json:"backoff_factor"`
	RetryableStatus []int         `json:"retryable_status"`
}

// RetryPolicy determines retry behavior
type RetryPolicy struct {
	config *RetryPolicyConfig
}

// NewRetryPolicy creates a retry policy
func NewRetryPolicy(config *RetryPolicyConfig) *RetryPolicy {
	return &RetryPolicy{config: config}
}

// GetBackoff returns the backoff duration for an attempt
func (p *RetryPolicy) GetBackoff(attempt int) time.Duration {
	backoff := float64(p.config.InitialBackoff)
	for i := 0; i < attempt; i++ {
		backoff *= p.config.BackoffFactor
	}

	if time.Duration(backoff) > p.config.MaxBackoff {
		return p.config.MaxBackoff
	}

	return time.Duration(backoff)
}

// IsRetryable checks if a status code is retryable
func (p *RetryPolicy) IsRetryable(status int) bool {
	for _, s := range p.config.RetryableStatus {
		if s == status {
			return true
		}
	}
	return false
}

// Ensure io is used
var _ = io.Discard
